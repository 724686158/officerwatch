// Package web 是 HTTP 层：JSON API + 内嵌单页前端。
//
// 前端不引框架、不设构建步骤：go:embed 把 index.html / app.js / style.css 打进二进制，
// 部署时只需要一个文件。前身 1700 行的 gui_http.go 把 HTML 写在字符串里、
// 每个 handler 手写一遍 json.NewEncoder —— 这里统一走 writeJSON/readJSON。
package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"officerwatch/internal/catalog"
	"officerwatch/internal/collect"
	"officerwatch/internal/config"
	"officerwatch/internal/esi"
	"officerwatch/internal/locs"
	"officerwatch/internal/model"
	"officerwatch/internal/notify"
	"officerwatch/internal/source"
	"officerwatch/internal/store"
)

//go:embed assets
var assetsFS embed.FS

// Server 组装所有依赖。
type Server struct {
	st        *store.Store
	cfg       *config.Config
	col       *collect.Collector
	nt        *notify.Notifier
	esi       *esi.Client
	res       *esi.Resolver
	cat       *catalog.Syncer
	zkb       *source.ZKB
	log       *slog.Logger
	ver       string
	start     time.Time
	imgs      *imageCache
	auth      *authState
	loginTmpl *template.Template

	settingsMu sync.Mutex
}

// New 构造。
func New(st *store.Store, cfg *config.Config, col *collect.Collector, nt *notify.Notifier,
	ec *esi.Client, res *esi.Resolver, cat *catalog.Syncer, log *slog.Logger, ver string) (*Server, error) {
	s := &Server{st: st, cfg: cfg, col: col, nt: nt, esi: ec, res: res, cat: cat,
		zkb: source.NewZKB(cfg.UserAgent), log: log, ver: ver, start: time.Now(),
		imgs: newImageCache(cfg.DataDir, cfg.UserAgent)}

	tmpl, err := template.ParseFS(assetsFS, "assets/login.html")
	if err != nil {
		return nil, fmt.Errorf("解析登录页模板失败: %w", err)
	}
	s.loginTmpl = tmpl
	if err := s.initAuth(); err != nil {
		return nil, err
	}
	return s, nil
}

// Handler 返回带中间件的路由。
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 登录页与退出不经过鉴权中间件
	root := http.NewServeMux()
	// 健康检查不鉴权：部署脚本和外部监控要能在未登录时探活
	root.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]any{"ok": true, "uptime": int(time.Since(s.start).Seconds())})
	})
	root.HandleFunc("GET /login", s.handleLoginPage)
	root.HandleFunc("POST /login", s.handleLoginSubmit)
	root.HandleFunc("GET /logout", s.handleLogout)

	mux.HandleFunc("GET /api/ship/{id}", s.handleShip)
	mux.HandleFunc("GET /api/overview", s.handleOverview)
	mux.HandleFunc("GET /api/kills", s.handleKills)
	mux.HandleFunc("POST /api/kills/import", s.handleImport)
	mux.HandleFunc("GET /api/kills/{id}", s.handleKillDetail)
	mux.HandleFunc("POST /api/kills/{id}/renotify", s.handleRenotify)

	mux.HandleFunc("GET /api/officers", s.handleOfficers)
	mux.HandleFunc("POST /api/officers/{id}/toggle", s.handleToggleOfficer)

	mux.HandleFunc("GET /api/stats", s.handleStats)
	mux.HandleFunc("GET /api/settings", s.handleGetSettings)
	mux.HandleFunc("PUT /api/settings", s.handlePutSettings)

	mux.HandleFunc("GET /api/status", s.handleStatus)
	mux.HandleFunc("POST /api/test-notify", s.handleTestNotify)
	mux.HandleFunc("POST /api/catalog/sync", s.handleCatalogSync)

	mux.HandleFunc("GET /", s.handleStatic)

	root.Handle("/", s.requireAuth(mux))
	return s.withMiddleware(root)
}

func (s *Server) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.log.Error("panic", "path", r.URL.Path, "rec", rec)
				writeErr(w, 500, "内部错误")
			}
		}()
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

// ─────────────────────────────── 静态资源

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/")
	if path == "" {
		path = "index.html"
	}
	sub, err := fs.Sub(assetsFS, "assets")
	if err != nil {
		http.Error(w, "assets broken", 500)
		return
	}
	b, err := fs.ReadFile(sub, path)
	if err != nil {
		// SPA：未知路径回首页（前端用 hash 路由，正常不会走到这）
		b, err = fs.ReadFile(sub, "index.html")
		if err != nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(b)
		return
	}
	switch {
	case strings.HasSuffix(path, ".js"):
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
	case strings.HasSuffix(path, ".css"):
		w.Header().Set("Content-Type", "text/css; charset=utf-8")
	case strings.HasSuffix(path, ".html"):
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
	default:
		w.Header().Set("Content-Type", "application/octet-stream")
	}
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(b)
}

// ─────────────────────────────── 概览

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ov, err := s.st.GetOverview()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	daily, err := s.st.Daily(14)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	factions, err := s.st.FactionStats()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	recent, _, err := s.st.ListKills(store.KillFilter{Limit: 8})
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{
		"overview": ov,
		"daily":    daily,
		"factions": factions,
		"recent":   recent,
		"source":   s.cfg.Source,
	})
}

// ─────────────────────────────── 击杀列表

func (s *Server) handleKills(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.KillFilter{
		Limit:    atoi(q.Get("limit"), 50),
		Offset:   atoi(q.Get("offset"), 0),
		Officer:  int64(atoi(q.Get("officer"), 0)),
		Faction:  q.Get("faction"),
		Role:     q.Get("role"),
		Status:   q.Get("status"),
		SortBy:   q.Get("sort"),
		Query:    strings.TrimSpace(q.Get("q")),
		MinValue: atof(q.Get("minValue")),
	}
	if since := q.Get("since"); since != "" {
		f.Since = since
	}
	items, total, err := s.st.ListKills(f)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, 200, map[string]any{"items": items, "total": total, "limit": f.Limit, "offset": f.Offset})
}

func (s *Server) handleKillDetail(w http.ResponseWriter, r *http.Request) {
	id := int64(atoi(r.PathValue("id"), 0))
	if id <= 0 {
		writeErr(w, 400, "非法 killmail id")
		return
	}
	k, raw, err := s.st.GetKill(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, 404, "没有这条击杀")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	var pretty any
	if raw != "" {
		_ = json.Unmarshal([]byte(raw), &pretty)
	}
	writeJSON(w, 200, map[string]any{"kill": k, "raw": pretty})
}

// handleImport 手工导入一个 KillID：zkb 拿 hash → ESI 拿完整 killmail → 走同一套入库/通知。
func (s *Server) handleImport(w http.ResponseWriter, r *http.Request) {
	var req struct {
		KillID int64 `json:"killId"`
	}
	if err := readJSON(r, &req); err != nil || req.KillID <= 0 {
		writeErr(w, 400, "请提供合法的 killId")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	// 先看本地有没有：有就顺手补全缺失字段（老库迁移进来的行没有星系/座舰），
	// 都齐了才直接返回，避免重复打 zkb。
	if existing, _, err := s.st.GetKill(req.KillID); err == nil &&
		existing.SystemName != "" && (existing.VictimName != "" || existing.VictimCharID == 0) {
		writeJSON(w, 200, map[string]any{"ok": true, "message": "这条已经在库里了", "alreadyExists": true})
		return
	}

	p, err := s.zkb.ByKillID(ctx, req.KillID)
	if err != nil {
		writeErr(w, 502, "zkb 取 hash 失败: "+err.Error())
		return
	}
	// zkb 给出的 hash 可直接喂 ESI；zkb 的 killmail 本体也够用，但 ESI 是权威源
	if km, err := s.esi.Killmail(ctx, req.KillID, p.Hash); err == nil {
		p.ESI = *km
	}
	if p.ESI.KillmailID == 0 {
		writeErr(w, 502, "ESI 没有返回这条 killmail")
		return
	}

	matched, err := s.col.Import(ctx, p)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if !matched {
		writeJSON(w, 200, map[string]any{"ok": false, "message": "这条不是官员击杀，未入库"})
		return
	}

	// 已存在但字段不全（典型：从旧库迁进来的 15 条）→ 补全。
	// 补全范围：星系/星域/安全等级/座舰/受害者身份（老库只有 ID）。
	if k, _, err := s.st.GetKill(req.KillID); err == nil && (k.SystemName == "" || k.VictimName == "") {
		sys, region, sec := s.res.SystemInfo(ctx, p.ESI.SolarSystemID)
		ship := s.res.TypeName(ctx, p.ESI.Victim.ShipTypeID)
		vname := s.res.CharacterName(ctx, p.ESI.Victim.CharacterID)
		vcorp := s.res.CorporationName(ctx, p.ESI.Victim.CorporationID)
		valli := s.res.AllianceName(ctx, p.ESI.Victim.AllianceID)
		locID := p.ZKB.LocationID
		locName := ""
		if l, ok := locs.Lookup(s.st.DB(), locID); ok {
			locName = fmt.Sprintf("%s（%s）", l.Short(sys), l.Kind())
		}
		jumps := s.res.JumpsFromHome(ctx, p.ESI.SolarSystemID, s.st.LoadSettings().HomeSystem)
		if sys != "" || ship != "" || vname != "" {
			_ = s.st.EnrichKill(req.KillID, p.ESI.SolarSystemID, sys, region, sec, locID, locName, jumps, ship, vname, vcorp, valli)
			s.st.AddEvent("info", "import", fmt.Sprintf("补全历史记录 kill %d（%s · %s）", req.KillID, sys, vname))
		}
	}
	k, _, _ := s.st.GetKill(req.KillID)
	writeJSON(w, 200, map[string]any{"ok": true, "message": "已导入", "kill": k})
}

// handleRenotify 重发通知（幂等：只影响通知状态，不影响击杀数据）。
func (s *Server) handleRenotify(w http.ResponseWriter, r *http.Request) {
	id := int64(atoi(r.PathValue("id"), 0))
	k, _, err := s.st.GetKill(id)
	if err != nil {
		writeErr(w, 404, "没有这条击杀")
		return
	}
	st := s.st.LoadSettings()
	if err := s.nt.Notify(r.Context(), notify.NewMessage(*k, st.AtAll, st.HomeSystem, st.Recipients)); err != nil {
		_ = s.st.MarkNotify(id, "failed", err.Error())
		writeErr(w, 502, "发送失败: "+err.Error())
		return
	}
	_ = s.st.MarkNotify(id, "sent", "")
	s.st.AddEvent("info", "notify", fmt.Sprintf("手工重发 kill %d", id))
	writeJSON(w, 200, map[string]any{"ok": true, "message": "已重发"})
}

// ─────────────────────────────── 官员

func (s *Server) handleOfficers(w http.ResponseWriter, r *http.Request) {
	list, err := s.st.ListOfficers()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	decorateOfficers(list)
	writeJSON(w, 200, map[string]any{
		"items":    list,
		"syncedAt": s.st.GetMeta("catalog_synced_at"),
	})
}

func (s *Server) handleToggleOfficer(w http.ResponseWriter, r *http.Request) {
	id := int64(atoi(r.PathValue("id"), 0))
	var req struct {
		Enabled bool `json:"enabled"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, "请求体不对")
		return
	}
	if err := s.st.SetOfficerEnabled(id, req.Enabled); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, 404, "名录里没有这个官员")
			return
		}
		writeErr(w, 500, err.Error())
		return
	}
	o, _, _ := s.st.OfficerByType(id)
	decorateOfficers([]model.Officer{o})
	s.st.AddEvent("info", "catalog", fmt.Sprintf("官员开关 %s → %v", o.Name, req.Enabled))
	writeJSON(w, 200, map[string]any{"ok": true, "officer": o})
}

// decorateOfficers 把"阵营/吨位"的中文名补上。
//
// 这些是展示用的派生字段：数据库只存 ship_class（battleship…）和 faction（Serpentis），
// 中文映射来自 internal/catalog 的元数据。放在 web 层做，是因为 store 不能反向依赖 catalog。
func decorateOfficers(list []model.Officer) {
	for i := range list {
		list[i].FactionZh = catalog.FactionZh(list[i].Faction)
		list[i].ShipClassZh = catalog.ClassZh(list[i].ShipClass)
	}
}

// ─────────────────────────────── 统计

func (s *Server) handleStats(w http.ResponseWriter, r *http.Request) {
	days := atoi(r.URL.Query().Get("days"), 30)
	daily, err := s.st.Daily(days)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	factions, err := s.st.FactionStats()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	officers, err := s.st.ListOfficers()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// 按击杀数排序取前 10
	top := make([]model.Officer, 0, len(officers))
	for _, o := range officers {
		if o.KillCount > 0 {
			top = append(top, o)
		}
	}
	for i := 0; i < len(top); i++ {
		for j := i + 1; j < len(top); j++ {
			if top[j].KillCount > top[i].KillCount {
				top[i], top[j] = top[j], top[i]
			}
		}
	}
	if len(top) > 10 {
		top = top[:10]
	}
	writeJSON(w, 200, map[string]any{"daily": daily, "factions": factions, "topOfficers": top})
}

// ─────────────────────────────── 设置

func (s *Server) handleGetSettings(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]any{
		"settings": s.st.LoadSettings(),
		"channels": s.cfg.Mask(),
	})
}

func (s *Server) handlePutSettings(w http.ResponseWriter, r *http.Request) {
	var st store.Settings
	if err := readJSON(r, &st); err != nil {
		writeErr(w, 400, "请求体不是合法 JSON")
		return
	}
	if st.MinValue < 0 {
		writeErr(w, 400, "价值阈值不能为负")
		return
	}
	switch st.Role {
	case model.RoleOfficerKilledPlayer, model.RoleOfficerDown, "both", "":
	default:
		writeErr(w, 400, "role 只能是 npc_kill / officer_down / both")
		return
	}
	if st.Role == "" {
		st.Role = "both"
	}
	if st.QuietStart < 0 || st.QuietStart > 23 || st.QuietEnd < 0 || st.QuietEnd > 23 {
		writeErr(w, 400, "静默时段必须是 0-23")
		return
	}
	if st.MaxPerHour < 0 {
		st.MaxPerHour = 0
	}
	// 收件人：去重、校验、封顶 20 个（防止误粘贴一整列地址把 SMTP 打爆）
	valid, invalid := normalizeEmails(st.Recipients)
	if len(invalid) > 0 {
		writeErr(w, 400, "这些地址格式不对: "+strings.Join(invalid, ", "))
		return
	}
	if len(valid) > 20 {
		writeErr(w, 400, "收件人最多 20 个")
		return
	}
	st.Recipients = valid
	if st.BackfillHours < 0 || st.BackfillHours > 24 {
		st.BackfillHours = 0
	}

	s.settingsMu.Lock()
	defer s.settingsMu.Unlock()
	if err := s.st.SaveSettings(st); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	s.st.AddEvent("info", "settings", "设置已更新")
	writeJSON(w, 200, map[string]any{"ok": true, "settings": st})
}

// normalizeEmails 清洗收件人列表：去空白、去重（忽略大小写）、丢掉不合法的。
func normalizeEmails(in []string) (valid, invalid []string) {
	seen := map[string]bool{}
	for _, raw := range in {
		// 允许用户一次粘贴多个（逗号/分号/空格分隔）
		for _, part := range strings.FieldsFunc(raw, func(r rune) bool {
			return r == ',' || r == ';' || r == ' ' || r == '\n' || r == '\t'
		}) {
			e := strings.TrimSpace(part)
			if e == "" {
				continue
			}
			if !isEmail(e) {
				invalid = append(invalid, e)
				continue
			}
			key := strings.ToLower(e)
			if seen[key] {
				continue
			}
			seen[key] = true
			valid = append(valid, e)
		}
	}
	return valid, invalid
}

// isEmail 只做"够用"的校验：一个 @、本地部分和域名都非空、无空格。
// 不用正则抠到 RFC 级别 —— 真正的合法性由 SMTP 服务器裁决。
func isEmail(s string) bool {
	if len(s) > 254 || strings.ContainsAny(s, " \t\r\n<>") {
		return false
	}
	at := strings.IndexByte(s, '@')
	if at <= 0 || at == len(s)-1 || strings.Count(s, "@") != 1 {
		return false
	}
	host := s[at+1:]
	return strings.Contains(host, ".") && !strings.HasPrefix(host, ".") && !strings.HasSuffix(host, ".")
}

// ─────────────────────────────── 状态

// Status 是运行状态的快照。
type Status struct {
	Version   string         `json:"version"`
	StartedAt time.Time      `json:"startedAt"`
	UptimeSec int64          `json:"uptimeSec"`
	Source    string         `json:"source"`
	Stats     map[string]any `json:"stats"`
	DB        map[string]any `json:"db"`
	Catalog   map[string]any `json:"catalog"`
	Channels  []string       `json:"channels"`
	Config    map[string]any `json:"config"`
	Events    []store.Event  `json:"events"`
	Now       time.Time      `json:"now"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	stt := s.col.Stats()
	lastFetch := ""
	if v, ok := stt.LastFetch.Load().(time.Time); ok && !v.IsZero() {
		lastFetch = v.UTC().Format(time.RFC3339)
	}
	lastErr := ""
	if v, ok := stt.LastErr.Load().(string); ok {
		lastErr = v
	}
	events, _ := s.st.ListEvents(40)
	chans := []string{}
	for _, c := range s.nt.Channels() {
		chans = append(chans, c.Name())
	}
	writeJSON(w, 200, Status{
		Version:   s.ver,
		StartedAt: s.start,
		UptimeSec: int64(time.Since(s.start).Seconds()),
		Source:    s.cfg.Source,
		Stats: map[string]any{
			"fetched": stt.Fetched.Load(), "matched": stt.Matched.Load(),
			"stored": stt.Stored.Load(), "notified": stt.Notified.Load(),
			"skipped": stt.Skipped.Load(), "errors": stt.Errors.Load(),
			"lastSequence": stt.LastSeq.Load(), "lastFetch": lastFetch, "lastError": lastErr,
		},
		DB: map[string]any{"sizeBytes": s.st.Size(),
			"killmailRows": countOrZero(s.st.GetOverview)},
		Catalog: map[string]any{
			"syncedAt": s.st.GetMeta("catalog_synced_at"),
			"count":    s.st.GetMeta("catalog_count"),
		},
		Channels: chans,
		Config:   s.cfg.Mask(),
		Events:   events,
		Now:      time.Now().UTC(),
	})
}

func countOrZero(f func() (store.Overview, error)) int64 {
	ov, err := f()
	if err != nil {
		return 0
	}
	return int64(ov.Total)
}

func (s *Server) handleTestNotify(w http.ResponseWriter, r *http.Request) {
	res := s.nt.SendTest(r.Context(), s.st.LoadSettings().Recipients)
	writeJSON(w, 200, map[string]any{"ok": true, "result": res})
}

func (s *Server) handleCatalogSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Deep bool `json:"deep"`
	}
	_ = readJSON(r, &req)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	var added int
	var err error
	if req.Deep {
		added, err = s.cat.DeepScan(ctx)
	} else {
		added, err = s.cat.Sync(ctx)
	}
	if err != nil {
		writeErr(w, 502, "同步失败: "+err.Error())
		return
	}
	_ = s.st.SetMeta("catalog_count", s.st.GetMeta("catalog_count"))
	s.st.AddEvent("info", "catalog", fmt.Sprintf("名录同步完成，新增 %d", added))
	writeJSON(w, 200, map[string]any{"ok": true, "added": added, "deep": req.Deep})
}

// ─────────────────────────────── 小工具

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"error": msg})
}

func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(v)
}

func atoi(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return n
}

func atof(s string) float64 {
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0
	}
	return v
}
