// Package esi 是 EVE ESI 的极简客户端 + 名字解析缓存。
//
// 只实现本项目用到的读接口：universe（组/类型/星系/星座/星域）与 killmail（手工导入用）。
// ESI 在中国大陆可达（实测 200），而 zkillredisq.stream 在这台机器上是 TLS 被重置的，
// 所以"补全 ESI 细节"这条路必须稳。
package esi

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"officerwatch/internal/model"
	"officerwatch/internal/store"
)

// Client 带 UA、重试、超时的 ESI 客户端。
type Client struct {
	Base string
	UA   string
	hc   *http.Client
}

// New 构造客户端。
func New(base, ua string) *Client {
	if base == "" {
		base = "https://esi.evetech.net/latest"
	}
	if ua == "" {
		ua = "officerwatch/1.0 (+https://github.com/724686158/officerwatch)"
	}
	return &Client{Base: strings.TrimSuffix(base, "/"), UA: ua, hc: &http.Client{Timeout: 30 * time.Second}}
}

// Group / Type / Category 是 ESI universe 的子集。
type Group struct {
	ID         int64   `json:"group_id"`
	Name       string  `json:"name"`
	CategoryID int64   `json:"category_id"`
	Types      []int64 `json:"types"`
}

// Type 船型。
type Type struct {
	ID       int64   `json:"type_id"`
	Name     string  `json:"name"`
	GroupID  int64   `json:"group_id"`
	Mass     float64 `json:"mass"`
	Volume   float64 `json:"volume"`
	Capacity float64 `json:"capacity"`
	Desc     string  `json:"description"`
}

// Category 分类。
type Category struct {
	ID     int64   `json:"category_id"`
	Name   string  `json:"name"`
	Groups []int64 `json:"groups"`
}

// System 星系（名字 + 星座 + 安全等级）。
type System struct {
	ID             int64   `json:"system_id"`
	Name           string  `json:"name"`
	Constellation  int64   `json:"constellation_id"`
	SecurityStatus float64 `json:"security_status"`
}

// Entity 角色/公司/联盟的公共名字接口都长这样。
type Entity struct {
	Name string `json:"name"`
}

// Constellation 星座。
type Constellation struct {
	ID       int64  `json:"constellation_id"`
	Name     string `json:"name"`
	RegionID int64  `json:"region_id"`
}

// Region 星域。
type Region struct {
	ID   int64  `json:"region_id"`
	Name string `json:"name"`
}

func (c *Client) get(ctx context.Context, path string, out any) error {
	url := c.Base + path
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(time.Duration(attempt) * 700 * time.Millisecond):
			}
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", c.UA)
		req.Header.Set("Accept", "application/json")
		resp, err := c.hc.Do(req)
		if err != nil {
			lastErr = err
			continue
		}
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
		resp.Body.Close()
		if rerr != nil {
			lastErr = rerr
			continue
		}
		switch {
		case resp.StatusCode == http.StatusOK:
			if err := json.Unmarshal(body, out); err != nil {
				return fmt.Errorf("解析 ESI 响应失败 %s: %w", path, err)
			}
			return nil
		case resp.StatusCode == http.StatusNotFound:
			return errNotFound
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = fmt.Errorf("ESI %d: %s", resp.StatusCode, truncate(string(body), 200))
			// 429 时按 Retry-After 走
			if resp.StatusCode == http.StatusTooManyRequests {
				time.Sleep(time.Second)
			}
		default:
			return fmt.Errorf("ESI %d: %s", resp.StatusCode, truncate(string(body), 200))
		}
	}
	return fmt.Errorf("ESI 请求失败 %s: %w", path, lastErr)
}

// ErrNotFound 表示资源不存在（船型被删、星系 ID 错等）。
var ErrNotFound = errNotFound

type esiErr string

func (e esiErr) Error() string { return string(e) }

const errNotFound = esiErr("esi: not found")

// Group 查船组。
func (c *Client) Group(ctx context.Context, id int64) (*Group, error) {
	var g Group
	if err := c.get(ctx, fmt.Sprintf("/universe/groups/%d/?datasource=tranquility", id), &g); err != nil {
		return nil, err
	}
	return &g, nil
}

// Type 查船型。
func (c *Client) Type(ctx context.Context, id int64) (*Type, error) {
	var t Type
	if err := c.get(ctx, fmt.Sprintf("/universe/types/%d/?datasource=tranquility", id), &t); err != nil {
		return nil, err
	}
	t.ID = id
	return &t, nil
}

// Category 查分类（"Entity" 分类的 ID 是 11，官员名录靠它全量重扫）。
func (c *Client) Category(ctx context.Context, id int64) (*Category, error) {
	var cat Category
	if err := c.get(ctx, fmt.Sprintf("/universe/categories/%d/?datasource=tranquility", id), &cat); err != nil {
		return nil, err
	}
	return &cat, nil
}

// System 查星系。
func (c *Client) System(ctx context.Context, id int64) (*System, error) {
	var s System
	if err := c.get(ctx, fmt.Sprintf("/universe/systems/%d/?datasource=tranquility", id), &s); err != nil {
		return nil, err
	}
	s.ID = id
	return &s, nil
}

// Constellation 查星座。
func (c *Client) Constellation(ctx context.Context, id int64) (*Constellation, error) {
	var s Constellation
	if err := c.get(ctx, fmt.Sprintf("/universe/constellations/%d/?datasource=tranquility", id), &s); err != nil {
		return nil, err
	}
	s.ID = id
	return &s, nil
}

// Region 查星域。
func (c *Client) Region(ctx context.Context, id int64) (*Region, error) {
	var r Region
	if err := c.get(ctx, fmt.Sprintf("/universe/regions/%d/?datasource=tranquility", id), &r); err != nil {
		return nil, err
	}
	r.ID = id
	return &r, nil
}

// CharacterName / CorporationName / AllianceName 名字（ESI 的公共只读接口）。
func (c *Client) CharacterName(ctx context.Context, id int64) (string, error) {
	var e Entity
	if err := c.get(ctx, fmt.Sprintf("/characters/%d/", id), &e); err != nil {
		return "", err
	}
	return e.Name, nil
}

// CorporationName 公司名。
func (c *Client) CorporationName(ctx context.Context, id int64) (string, error) {
	var e Entity
	if err := c.get(ctx, fmt.Sprintf("/corporations/%d/", id), &e); err != nil {
		return "", err
	}
	return e.Name, nil
}

// AllianceName 联盟名。
func (c *Client) AllianceName(ctx context.Context, id int64) (string, error) {
	var e Entity
	if err := c.get(ctx, fmt.Sprintf("/alliances/%d/", id), &e); err != nil {
		return "", err
	}
	return e.Name, nil
}

// SystemIDs 把名字解析成星系 ID（POST /universe/ids/，一次可传多个名字）。
func (c *Client) SystemIDs(ctx context.Context, names []string) (map[string]int64, error) {
	body, err := json.Marshal(names)
	if err != nil || len(names) == 0 {
		return nil, err
	}
	url := c.Base + "/universe/ids/?datasource=tranquility"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.UA)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("universe/ids HTTP %d", resp.StatusCode)
	}
	var out struct {
		Systems []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
		} `json:"systems"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	m := map[string]int64{}
	for _, s := range out.Systems {
		m[strings.ToLower(s.Name)] = s.ID
	}
	return m, nil
}

// RouteJumps 返回两星系之间的最短跳数（ESI 的 /route/ 返回完整路径）。
func (c *Client) RouteJumps(ctx context.Context, from, to int64) (int, error) {
	if from == to {
		return 0, nil
	}
	var path []int64
	if err := c.get(ctx, fmt.Sprintf("/route/%d/%d/?datasource=tranquility&flag=shortest", from, to), &path); err != nil {
		return 0, err
	}
	if len(path) == 0 {
		return 0, fmt.Errorf("route 返回空路径")
	}
	return len(path) - 1, nil
}

// Killmail 取一条完整 killmail（手工导入 KillID 时需要）。
func (c *Client) Killmail(ctx context.Context, id int64, hash string) (*model.Killmail, error) {
	var k model.Killmail
	if err := c.get(ctx, fmt.Sprintf("/killmails/%d/%s/?datasource=tranquility", id, hash), &k); err != nil {
		return nil, err
	}
	k.KillmailID = id
	return &k, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// ─────────────────────────────── 名字解析

// Resolver 把 ID 变成人话，并把结果缓存在 SQLite 里（星系/星域基本不变，缓存永久有效）。
type Resolver struct {
	esi          *Client
	st           *store.Store
	mu           sync.Mutex
	negTS        map[int64]time.Time  // 解析失败的星系，5 分钟内不再打
	entityFailed map[string]time.Time // 解析失败的实体名，10 分钟内不再打
}

// NewResolver 构造解析器。
func NewResolver(c *Client, st *store.Store) *Resolver {
	return &Resolver{esi: c, st: st, negTS: map[int64]time.Time{}, entityFailed: map[string]time.Time{}}
}

// TypeName 船型名字。
func (r *Resolver) TypeName(ctx context.Context, typeID int64) string {
	if typeID == 0 {
		return ""
	}
	if name, _, ok := r.st.CachedName("type", typeID); ok {
		return name
	}
	t, err := r.esi.Type(ctx, typeID)
	if err != nil {
		return ""
	}
	r.st.CacheName("type", typeID, t.Name, "")
	return t.Name
}

// SystemInfo 星系的三件套：名字、星域名、安全等级。
// 任何一步失败都不影响主流程（只是邮件里少一个字段）。
//
// 缓存分两类（system / syssec）：老版本只缓存了名字，遇到"有名字没安全等级"
// 的记录要继续去 ESI 补，不能直接返回 0 —— 否则安全等级永远是 0.00。
func (r *Resolver) SystemInfo(ctx context.Context, systemID int64) (name, region string, sec float64) {
	if systemID == 0 {
		return "", "", 0
	}
	have := false
	if n, extra, ok := r.st.CachedName("system", systemID); ok {
		name, region, have = n, extra, true
		if v, _, ok2 := r.st.CachedName("syssec", systemID); ok2 {
			fmt.Sscanf(v, "%f", &sec)
			return name, region, sec
		}
	}

	r.mu.Lock()
	if ts, ok := r.negTS[systemID]; ok && time.Since(ts) < 5*time.Minute {
		r.mu.Unlock()
		return name, region, sec
	}
	r.mu.Unlock()

	sys, err := r.esi.System(ctx, systemID)
	if err != nil {
		r.mu.Lock()
		r.negTS[systemID] = time.Now()
		r.mu.Unlock()
		if have {
			return name, region, sec
		}
		return "", "", 0
	}
	region = ""
	if cons, err := r.esi.Constellation(ctx, sys.Constellation); err == nil {
		if reg, err := r.esi.Region(ctx, cons.RegionID); err == nil {
			region = reg.Name
		}
	}
	r.st.CacheName("system", systemID, sys.Name, region)
	r.st.CacheName("syssec", systemID, fmt.Sprintf("%.2f", sys.SecurityStatus), "")
	return sys.Name, region, sys.SecurityStatus
}

// SystemRegion 兼容旧调用：只取名字。
func (r *Resolver) SystemRegion(ctx context.Context, systemID int64) (string, string) {
	n, reg, _ := r.SystemInfo(ctx, systemID)
	return n, reg
}

// JumpsFromHome 从"家"到目标星系的跳数。
//
// 家按名字配（设置里的 HomeSystem，如 "Jita"），解析出的 ID 缓存在 meta；
// 改配置会自动重解析。这是"抢官员"的第二个关键数字：值不值得跑一趟，先看几跳。
func (r *Resolver) JumpsFromHome(ctx context.Context, destSystemID int64, homeName string) int {
	homeName = strings.TrimSpace(homeName)
	if homeName == "" || destSystemID == 0 {
		return 0
	}
	var homeID int64
	fmt.Sscanf(r.st.GetMeta("home_system_id"), "%d", &homeID)
	if homeID == 0 || r.st.GetMeta("home_system_name") != homeName {
		ids, err := r.esi.SystemIDs(ctx, []string{homeName})
		if err != nil {
			return 0
		}
		homeID = ids[strings.ToLower(homeName)]
		if homeID == 0 {
			return 0
		}
		_ = r.st.SetMeta("home_system_id", fmt.Sprint(homeID))
		_ = r.st.SetMeta("home_system_name", homeName)
	}
	if destSystemID == homeID {
		return 0
	}
	n, err := r.esi.RouteJumps(ctx, homeID, destSystemID)
	if err != nil {
		return 0
	}
	return n
}

// 实体名（角色/公司/联盟）：缓存 + 负缓存，避免每次通知都打三次 ESI。
func (r *Resolver) entityName(ctx context.Context, kind string, id int64, fetch func(context.Context, int64) (string, error)) string {
	if id == 0 {
		return ""
	}
	if name, _, ok := r.st.CachedName(kind, id); ok {
		return name
	}
	key := kind + ":" + fmt.Sprint(id)
	r.mu.Lock()
	if ts, ok := r.entityFailed[key]; ok && time.Since(ts) < 10*time.Minute {
		r.mu.Unlock()
		return ""
	}
	r.mu.Unlock()

	name, err := fetch(ctx, id)
	if err != nil {
		r.mu.Lock()
		r.entityFailed[key] = time.Now()
		r.mu.Unlock()
		return ""
	}
	r.st.CacheName(kind, id, name, "")
	return name
}

// CharacterName 角色名（受害者）。
func (r *Resolver) CharacterName(ctx context.Context, id int64) string {
	return r.entityName(ctx, "character", id, r.esi.CharacterName)
}

// CorporationName 公司名。
func (r *Resolver) CorporationName(ctx context.Context, id int64) string {
	return r.entityName(ctx, "corporation", id, r.esi.CorporationName)
}

// AllianceName 联盟名。
func (r *Resolver) AllianceName(ctx context.Context, id int64) string {
	return r.entityName(ctx, "alliance", id, r.esi.AllianceName)
}
