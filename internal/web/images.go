package web

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"
)

// 舰船缩略图代理。
//
// 为什么不在页面里直接 <img src="https://images.evetech.net/...">：
//   - 访客的 IP 会暴露给第三方 CDN；
//   - 国内直连 images.evetech.net 不稳定（实测首次抓 icon 要 6.8 秒）；
//   - 同一个船型在列表里会出现几十次，热链等于把请求放大几十倍。
//
// 所以：浏览器只请求我们自己的 /api/ship/{typeID}，服务端去 CCP 的图片服务取一次，
// 落盘缓存，之后本地直出（带 30 天强缓存）。
//
// 图片版权归 CCP 所有，这里只做运行时转发与缓存，仓库里不存任何图。

const (
	imgUpstream   = "https://images.evetech.net/types/%d/%s?size=%d"
	imgCacheLimit = 64 << 20 // 缓存目录上限 64MB，超了就不再写入（只读取已有）
)

var allowedSizes = map[int]bool{24: true, 32: true, 48: true, 64: true, 128: true, 256: true, 512: true}

type imageCache struct {
	dir      string
	ua       string
	hc       *http.Client
	mu       sync.Mutex
	inflight map[string]*sync.Mutex
}

func newImageCache(dataDir, ua string) *imageCache {
	dir := filepath.Join(dataDir, "cache", "ship")
	_ = os.MkdirAll(dir, 0o755)
	return &imageCache{
		dir: dir, ua: ua,
		hc:       &http.Client{Timeout: 20 * time.Second},
		inflight: make(map[string]*sync.Mutex),
	}
}

// lock 同一张图并发只回源一次（列表里同一船型会出现很多次）。
func (c *imageCache) lock(key string) func() {
	c.mu.Lock()
	l, ok := c.inflight[key]
	if !ok {
		l = &sync.Mutex{}
		c.inflight[key] = l
	}
	c.mu.Unlock()
	l.Lock()
	return func() {
		l.Unlock()
		c.mu.Lock()
		delete(c.inflight, key)
		c.mu.Unlock()
	}
}

// handleShip 处理 /api/ship/{id}?size=64&kind=render
func (s *Server) handleShip(w http.ResponseWriter, r *http.Request) {
	id := int64(atoi(r.PathValue("id"), 0))
	if id <= 0 || id > 1e9 {
		http.Error(w, "bad id", http.StatusBadRequest)
		return
	}
	size := atoi(r.URL.Query().Get("size"), 64)
	if !allowedSizes[size] {
		size = 64
	}
	kind := r.URL.Query().Get("kind")
	if kind != "icon" {
		kind = "render"
	}

	unlock := s.imgs.lock(fmt.Sprintf("%d-%s-%d", id, kind, size))
	defer unlock()

	path := filepath.Join(s.imgs.dir, fmt.Sprintf("%d_%s_%d.img", id, kind, size))
	if b, err := os.ReadFile(path); err == nil {
		serveImage(w, b, true)
		return
	}

	b, ctype, err := s.imgs.fetch(id, kind, upstreamSize(size))
	if err != nil {
		// 取不到就让前端显示占位（不缓存失败结果，下次还会重试）
		http.Error(w, "image unavailable", http.StatusNotFound)
		return
	}
	_ = ctype
	if dirSize(s.imgs.dir) < imgCacheLimit {
		_ = os.WriteFile(path, b, 0o644)
	}
	serveImage(w, b, false)
}

// upstreamSize 把请求尺寸对齐到 CCP 图片服务支持的档位（32/64/128/256/512/1024）。
// 传 24 会被上游 404，本地缓存又按 24 存——那种"空格子"就是踩过这个坑。
func upstreamSize(size int) int {
	switch {
	case size <= 32:
		return 32
	case size <= 64:
		return 64
	case size <= 128:
		return 128
	case size <= 256:
		return 256
	default:
		return 512
	}
}

func (c *imageCache) fetch(id int64, kind string, size int) ([]byte, string, error) {
	// 上游偶尔会瞬时 404（CCP 的图在生成/被限流），重试一次明显降低"图裂"概率
	b, ct, err := c.fetchOnce(id, kind, size)
	if err == nil {
		return b, ct, nil
	}
	time.Sleep(300 * time.Millisecond)
	return c.fetchOnce(id, kind, size)
}

func (c *imageCache) fetchOnce(id int64, kind string, size int) ([]byte, string, error) {
	url := fmt.Sprintf(imgUpstream, id, kind, size)
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", c.ua)
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("upstream %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", err
	}
	if len(b) == 0 {
		return nil, "", fmt.Errorf("upstream 空响应")
	}
	return b, resp.Header.Get("Content-Type"), nil
}

// serveImage 按文件头判断类型（上游 render 是 JPEG、icon 是 PNG），给足缓存时间。
func serveImage(w http.ResponseWriter, b []byte, cached bool) {
	ct := "image/jpeg"
	if bytes.HasPrefix(b, []byte("\x89PNG")) {
		ct = "image/png"
	} else if bytes.HasPrefix(b, []byte("<svg")) || bytes.HasPrefix(b, []byte("<?xml")) {
		ct = "image/svg+xml"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
	if cached {
		w.Header().Set("X-Cache", "hit")
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(b)))
	_, _ = w.Write(b)
}

// dirSize 粗略统计缓存目录大小（够用即可，不追求准确）。
func dirSize(dir string) int64 {
	var total int64
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	for _, e := range ents {
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total
}
