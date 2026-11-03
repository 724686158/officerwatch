// Package source 是两个 killmail 数据源。
//
//	r2z2 —— zKillboard 的序列流：每 5.5 秒一条全服击杀，序列号单调递增，
//	        文件保留 24 小时，可断点续传。**这是主源**：无损、能补、便宜。
//	zkb  —— zKillboard 的按船型查询接口：/api/kills/shipTypeID/<id>/。
//	        只用来兜底（R2Z2 被墙/被封时），或只关心少数几个官员时省流量。
//
// 前身用的是 zkillredisq.stream 长轮询，但那台机器上它 TLS 直接被重置（实测
// wrong version number），而且长轮询只能抽样、会漏——所以这次换成序列流。
package source

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"officerwatch/internal/model"
)

// ErrNotYet 表示"这个序列还没有生成"（R2Z2 的 404），调用方应等待后重试。
var ErrNotYet = fmt.Errorf("not yet")

// ErrBanned 表示被 Cloudflare 拦了（403），要长退避。
var ErrBanned = fmt.Errorf("forbidden")

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        8,
			MaxIdleConnsPerHost: 8,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// R2Z2 序列流客户端。
type R2Z2 struct {
	Base string
	UA   string
	hc   *http.Client
}

// NewR2Z2 构造。
func NewR2Z2(ua string) *R2Z2 {
	if ua == "" {
		ua = "officerwatch/1.0 (+https://github.com/724686158/officerwatch)"
	}
	return &R2Z2{Base: "https://r2z2.zkillboard.com", UA: ua, hc: newHTTPClient(30 * time.Second)}
}

func (r *R2Z2) get(ctx context.Context, path string) ([]byte, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.Base+path, nil)
	if err != nil {
		return nil, 0, err
	}
	// Cloudflare 会因为空 UA 直接 403 —— 这行不能省。
	req.Header.Set("User-Agent", r.UA)
	req.Header.Set("Accept", "application/json")
	resp, err := r.hc.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, resp.StatusCode, err
	}
	return body, resp.StatusCode, nil
}

// Sequence 取当前最新序列号（用于冷启动定位）。
func (r *R2Z2) Sequence(ctx context.Context) (int64, error) {
	body, code, err := r.get(ctx, "/ephemeral/sequence.json")
	if err != nil {
		return 0, err
	}
	if code == http.StatusForbidden {
		return 0, ErrBanned
	}
	if code != http.StatusOK {
		return 0, fmt.Errorf("sequence.json HTTP %d", code)
	}
	var v struct {
		Sequence int64 `json:"sequence"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return 0, fmt.Errorf("解析 sequence.json 失败: %w", err)
	}
	return v.Sequence, nil
}

// Fetch 取某个序列号的 killmail；404 返回 ErrNotYet。
func (r *R2Z2) Fetch(ctx context.Context, seq int64) (*model.Payload, error) {
	body, code, err := r.get(ctx, fmt.Sprintf("/ephemeral/%d.json", seq))
	if err != nil {
		return nil, err
	}
	switch code {
	case http.StatusOK:
	case http.StatusNotFound:
		return nil, ErrNotYet
	case http.StatusForbidden:
		return nil, ErrBanned
	default:
		return nil, fmt.Errorf("sequence %d HTTP %d", seq, code)
	}
	var p model.Payload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("解析 sequence %d 失败: %w", seq, err)
	}
	if p.KillmailID == 0 {
		p.KillmailID = p.ESI.KillmailID
	}
	if p.KillmailID == 0 {
		return nil, fmt.Errorf("sequence %d 缺 killmail_id", seq)
	}
	if p.SequenceID == 0 {
		p.SequenceID = seq
	}
	return &p, nil
}

// ZKB 按船型查询的兜底源。
type ZKB struct {
	Base      string
	UA        string
	hc        *http.Client
	LastFetch time.Time
}

// NewZKB 构造。
func NewZKB(ua string) *ZKB {
	if ua == "" {
		ua = "officerwatch/1.0 (+https://github.com/724686158/officerwatch)"
	}
	return &ZKB{Base: "https://zkillboard.com/api", UA: ua, hc: newHTTPClient(45 * time.Second)}
}

// Recent 取某船型最近参与的击杀（zkb 的 shipTypeID 过滤不分攻守，
// 所以打人的官员和被打的官员都能捞到）。
func (z *ZKB) Recent(ctx context.Context, shipTypeID int64) ([]model.Payload, error) {
	return z.fetchList(ctx, fmt.Sprintf("%s/kills/shipTypeID/%d/", z.Base, shipTypeID))
}

// ByKillID 按 KillID 取一条（手工导入用：ESI 需要 hash，而只有 zkb 知道 hash）。
func (z *ZKB) ByKillID(ctx context.Context, killID int64) (*model.Payload, error) {
	list, err := z.fetchList(ctx, fmt.Sprintf("%s/killID/%d/", z.Base, killID))
	if err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, fmt.Errorf("zkb 没有这条 killmail")
	}
	return &list[0], nil
}

func (z *ZKB) fetchList(ctx context.Context, url string) ([]model.Payload, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", z.UA)
	req.Header.Set("Accept", "application/json")
	resp, err := z.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden {
		return nil, ErrBanned
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("zkb HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, err
	}
	// zkb 返回的是一个数组，每项 = ESI killmail 字段 + zkb 块（平铺）。
	var raw []json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("解析 zkb 响应失败: %w", err)
	}
	out := make([]model.Payload, 0, len(raw))
	for _, r := range raw {
		var item struct {
			model.Killmail
			ZKB model.ZKB `json:"zkb"`
		}
		if err := json.Unmarshal(r, &item); err != nil {
			continue
		}
		km := item.Killmail
		out = append(out, model.Payload{
			KillmailID: km.KillmailID,
			Hash:       item.ZKB.Hash,
			ESI:        km,
			ZKB:        item.ZKB,
		})
	}
	z.LastFetch = time.Now()
	return out, nil
}

// OfficerShipTypes 解析 "13654,13659" 这种配置写法。
func OfficerShipTypes(s string) []int64 {
	out := []int64{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var id int64
		if _, err := fmt.Sscanf(part, "%d", &id); err == nil && id > 0 {
			out = append(out, id)
		}
	}
	return out
}
