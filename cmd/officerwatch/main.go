// officerwatch —— EVE Online 官员击杀监控（EVE-RedisQ 的现代化重写）
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"officerwatch/internal/catalog"
	"officerwatch/internal/collect"
	"officerwatch/internal/config"
	"officerwatch/internal/esi"
	"officerwatch/internal/legacy"
	"officerwatch/internal/locs"
	"officerwatch/internal/notify"
	"officerwatch/internal/patrol"
	"officerwatch/internal/source"
	"officerwatch/internal/store"
	"officerwatch/internal/web"
)

// version 由构建时 -ldflags "-X main.version=..." 注入。
var version = "dev"

func main() {
	var (
		cfgPath   = flag.String("config", "config.json", "配置文件路径")
		listen    = flag.String("listen", "", "覆盖监听地址（默认取配置）")
		showVer   = flag.Bool("version", false, "打印版本后退出")
		importOld = flag.String("import-legacy", "", "从旧 EVE-RedisQ 的 killmails.db 导入历史记录后退出")
		syncOnly  = flag.Bool("sync-catalog", false, "只同步官员名录后退出（运维用）")
		enrich    = flag.Bool("enrich", false, "补全历史记录缺的星系/安全等级/受害者名后退出")
	)
	flag.Parse()

	if *showVer {
		fmt.Println("officerwatch", version)
		return
	}

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintln(os.Stderr, "加载配置失败:", err)
		os.Exit(1)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	logger := newLogger(cfg.LogLevel)

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		logger.Error("打开数据库失败", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	// 星体名索引：首次启动把打包的 45 万行星体表灌进 SQLite（几秒），之后是纯本地查询
	if n, err := locs.Ensure(st.DB()); err != nil {
		logger.Warn("星体索引初始化失败（死亡位置将只显示星系）", "err", err)
	} else {
		logger.Info("星体索引就绪", "rows", n)
	}

	ec := esi.New(cfg.ESIBase, cfg.UserAgent)
	res := esi.NewResolver(ec, st)
	cat := catalog.New(ec, res, st)

	// 一次性运维命令
	if *importOld != "" {
		rows, err := legacy.Read(*importOld)
		if err != nil {
			logger.Error("读旧库失败", "err", err)
			os.Exit(1)
		}
		if _, err := cat.Sync(context.Background()); err != nil {
			logger.Warn("名录同步失败（继续导入）", "err", err)
		}
		added, err := legacy.Import(st, rows)
		if err != nil {
			logger.Error("导入失败", "err", err)
			os.Exit(1)
		}
		fmt.Printf("旧库 %d 条，新增 %d 条\n", len(rows), added)
		return
	}
	if *enrich {
		n, err := enrichHistory(context.Background(), st, res, source.NewZKB(cfg.UserAgent), logger)
		if err != nil {
			logger.Error("补全失败", "err", err)
			os.Exit(1)
		}
		fmt.Printf("补全完成，处理 %d 条\n", n)
		return
	}
	if *syncOnly {
		added, err := cat.Sync(context.Background())
		if err != nil {
			logger.Error("名录同步失败", "err", err)
			os.Exit(1)
		}
		fmt.Println("名录同步完成，新增", added)
		return
	}

	// 通知通道：配置为空的直接不注册。
	// 邮件是主通道；QQ 群（OneBot）依赖自建机器人，默认留空即可。
	nt := notify.New(logger)
	nt.Add(notify.NewMail(cfg.SMTP.Host, cfg.SMTP.Port, cfg.SMTP.User, cfg.SMTP.Pass, cfg.SMTP.To))
	nt.Add(notify.NewWebhook(cfg.Webhook.URL, cfg.Webhook.Secret))
	nt.Add(notify.NewOneBot(cfg.OneBot.URL, cfg.OneBot.Token, cfg.OneBot.GroupID))
	chNames := []string{}
	for _, c := range nt.Channels() {
		chNames = append(chNames, c.Name())
	}
	logger.Info("启动", "version", version, "listen", cfg.Listen, "db", cfg.DBPath,
		"source", cfg.Source, "channels", chNames)

	col := collect.New(st, res, ec, nt, logger)

	// 启动计数：巡检靠它识别"中途被重启过"
	var boots int64
	fmt.Sscanf(st.GetMeta("boot_count"), "%d", &boots)
	_ = st.SetMeta("boot_count", fmt.Sprint(boots+1))

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 名录：空库就灌一次，之后每 24 小时同步一遍（CCP 加新官员时能自动跟上）
	go func() {
		c, cancel := context.WithTimeout(ctx, 3*time.Minute)
		defer cancel()
		if err := cat.Ensure(c); err != nil {
			logger.Warn("名录初始化失败", "err", err)
			st.AddEvent("warn", "catalog", "名录初始化失败: "+err.Error())
		} else {
			n, _ := st.ListOfficers()
			logger.Info("名录就绪", "officers", len(n))
			st.AddEvent("info", "catalog", fmt.Sprintf("官员名录就绪（%d 条）", len(n)))
		}
		t := time.NewTicker(24 * time.Hour)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				c2, cancel2 := context.WithTimeout(ctx, 5*time.Minute)
				if added, err := cat.Sync(c2); err != nil {
					logger.Warn("名录同步失败", "err", err)
				} else if added > 0 {
					st.AddEvent("info", "catalog", fmt.Sprintf("名录同步新增 %d 条", added))
				}
				cancel2()
			}
		}
	}()

	// 采集循环
	go func() {
		for {
			var err error
			switch cfg.Source {
			case "zkb":
				err = col.RunZKB(ctx, time.Duration(cfg.ZKBIntervalSeconds)*time.Second)
			case "both":
				go func() { _ = col.RunZKB(ctx, time.Duration(cfg.ZKBIntervalSeconds)*time.Second) }()
				err = col.RunR2Z2(ctx)
			default:
				err = col.RunR2Z2(ctx)
			}
			if ctx.Err() != nil {
				return
			}
			logger.Error("采集循环退出，5 秒后重启", "err", err)
			st.AddEvent("error", "collect", "采集循环退出: "+fmt.Sprint(err))
			select {
			case <-ctx.Done():
				return
			case <-time.After(5 * time.Second):
			}
		}
	}()

	handler, err := web.New(st, cfg, col, nt, ec, res, cat, logger, version)
	if err != nil {
		logger.Error("初始化 HTTP 层失败", "err", err)
		os.Exit(1)
	}
	// 巡检：每 5 分钟体检一次（落后、写库、通道、名录、重启），连续 2 轮失败才告警
	pr := patrol.New(st, nt, logger,
		func(c context.Context) (int64, error) { return source.NewR2Z2(cfg.UserAgent).Sequence(c) },
		func() int64 {
			if v, ok := col.Stats().LastFetch.Load().(time.Time); ok && !v.IsZero() {
				return int64(time.Since(v).Seconds())
			}
			return -1
		},
		func() int64 { return col.Stats().LastSeq.Load() },
		func() bool { return col.Stats().AtFrontier.Load() },
	)
	go pr.Loop(ctx)

	// 看门狗：区分三种"没动静"，别把上游停摆说成你网络坏了。
	//   1) 连续失败（连接重置/超时）→ 是我们取不到
	//   2) 已追平、上游长时间没有新击杀 → 不是我们的问题
	//   3) 落后上游却取不到 → 本地/网络问题
	// 另外：R2Z2 停摆超过 10 分钟时，自动启用 zkb 轮询兜底，恢复后自动关掉。
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		var lastAlert time.Time
		var fallbackCancel context.CancelFunc
		const (
			failAlertAfter   = 10 * time.Minute
			behindAlertAfter = 15 * time.Minute
			upstreamIdle     = 45 * time.Minute
			alertCooldown    = 30 * time.Minute
			fallbackAfter    = 10 * time.Minute
		)
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}

			stt := col.Stats()
			idle := idleSince(stt)
			if idle < 0 {
				continue // 刚启动，还没拉过
			}
			fails := stt.ConsecFails.Load()
			atFrontier := stt.AtFrontier.Load()

			var (
				title string
				body  string
				alert bool
			)
			switch {
			case fails >= 3 && idle >= failAlertAfter:
				title = "连不上 zKillboard"
				body = fmt.Sprintf("连续 %d 次拉取失败，已 %d 分钟没有成功。\n最近错误：%s\n\n"+
					"服务进程正常，是我们到 r2z2.zkillboard.com 的网络不通（连接重置/超时）。\n"+
					"服务器本身可能没问题，先看看出网和 DNS。", fails, int(idle.Minutes()), errText(stt))
				alert = true
			case !atFrontier && idle >= behindAlertAfter:
				title = "采集落后于上游"
				body = fmt.Sprintf("游标 %d，已 %d 分钟没有取到新数据，但上游在推进。\n最近错误：%s\n\n"+
					"这是本地侧问题（网络抖动/请求被限），检查出网。", stt.LastSeq.Load(), int(idle.Minutes()), errText(stt))
				alert = true
			case atFrontier && idle >= upstreamIdle:
				title = "上游停摆（不是你这边的问题）"
				body = fmt.Sprintf("本机已追平上游（游标 %d），但 zKillboard 已经 %d 分钟没有产生新击杀。\n\n"+
					"R2Z2 的机制是「每解析一条击杀生成一个序列文件」，全服平均每 6 秒一条；"+
					"连续 %d 分钟没有新文件，基本可以判定是上游管线卡住，不是采集器故障。\n"+
					"采集器会在恢复后自动追上，不用处理。", stt.LastSeq.Load(), int(idle.Minutes()), int(idle.Minutes()))
				alert = true
			}

			if alert && time.Since(lastAlert) >= alertCooldown {
				msg := "officerwatch 巡检告警\n\n" + title + "\n\n" + body +
					"\n\n时间：" + time.Now().In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04")
				if err := nt.NotifyRaw(ctx, msg); err != nil {
					logger.Error("看门狗告警发送失败", "err", err)
				} else {
					st.AddEvent("error", "collect", title)
					lastAlert = time.Now()
				}
			}

			// 兜底：R2Z2 静默 → 用 zkb 按船型轮询，至少不漏掉那 8 位官员
			if cfg.Source == "r2z2" || cfg.Source == "" {
				switch {
				case idle >= fallbackAfter && fallbackCancel == nil:
					fctx, cancel := context.WithCancel(ctx)
					fallbackCancel = cancel
					go func() { _ = col.RunZKB(fctx, 5*time.Minute) }()
					logger.Warn("R2Z2 无进展，启动 zkb 轮询兜底")
					st.AddEvent("warn", "collect", "R2Z2 无进展，启用 zkb 轮询兜底")
				case idle < 2*time.Minute && fallbackCancel != nil:
					fallbackCancel()
					fallbackCancel = nil
					logger.Info("R2Z2 恢复，停止 zkb 兜底")
					st.AddEvent("info", "collect", "R2Z2 恢复，停止 zkb 兜底")
				}
			}
		}
	}()

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		logger.Info("HTTP 监听", "addr", cfg.Listen)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			// 端口被占是部署期最常见的失败，直接退出让 systemd 记一笔并重启，
			// 而不是"优雅退出"——那会掩盖真实原因（前身用 log.Fatalf，方向是对的）。
			logger.Error("HTTP 服务失败", "addr", cfg.Listen, "err", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("收到退出信号，收尾中")
	shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(shutCtx)
}

// enrichHistory 给历史记录补上星系/星域/安全等级/受害者名。
// 只补空字段（store.EnrichKill 内部判断），重复跑安全。
func enrichHistory(ctx context.Context, st *store.Store, res *esi.Resolver, zkb *source.ZKB, log *slog.Logger) (int, error) {
	ids, err := st.KillsNeedingEnrich()
	if err != nil {
		return 0, err
	}
	n := 0
	for _, id := range ids {
		k, raw, err := st.GetKill(id)
		if err != nil {
			continue
		}
		// 从原始 JSON 里取受害者/星系 ID（不额外打 zkb）
		var p struct {
			ESI struct {
				SolarSystemID int64 `json:"solar_system_id"`
				Victim        struct {
					CharacterID   int64 `json:"character_id"`
					CorporationID int64 `json:"corporation_id"`
					AllianceID    int64 `json:"alliance_id"`
					ShipTypeID    int64 `json:"ship_type_id"`
				} `json:"victim"`
			} `json:"esi"`
		}
		_ = json.Unmarshal([]byte(raw), &p)
		sysID := p.ESI.SolarSystemID
		if sysID == 0 {
			sysID = k.SystemID
		}
		// 老库只存了 total_value，掉落/损毁是 0 → 用 zkb 补一次（只有十几条历史，代价可接受）
		locID := k.LocationID
		if pay, err := zkb.ByKillID(ctx, id); err == nil {
			if k.DroppedValue == 0 && k.DestroyedValue == 0 && pay.ZKB.TotalValue > 0 {
				_ = st.UpdateKillEconomy(id, pay.ZKB.FittedValue, pay.ZKB.DroppedValue,
					pay.ZKB.DestroyedValue, pay.ZKB.Points, pay.ZKB.Solo)
			}
			if locID == 0 {
				locID = pay.ZKB.LocationID
			}
		}
		locName := ""
		if l, ok := locs.Lookup(st.DB(), locID); ok {
			locName = fmt.Sprintf("%s（%s）", l.Short(k.SystemName), l.Kind())
		}
		sys, region, sec := res.SystemInfo(ctx, sysID)
		jumps := res.JumpsFromHome(ctx, sysID, st.LoadSettings().HomeSystem)
		ship := res.TypeName(ctx, p.ESI.Victim.ShipTypeID)
		if ship == "" {
			ship = k.VictimShipName
		}
		vname := res.CharacterName(ctx, p.ESI.Victim.CharacterID)
		vcorp := res.CorporationName(ctx, p.ESI.Victim.CorporationID)
		valli := res.AllianceName(ctx, p.ESI.Victim.AllianceID)
		if err := st.EnrichKill(id, sysID, sys, region, sec, locID, locName, jumps, ship, vname, vcorp, valli); err != nil {
			log.Warn("补全写入失败", "kill", id, "err", err)
			continue
		}
		n++
	}
	return n, nil
}

// idleSince 距上次成功拉取的秒数；-1 表示还没开始。
func idleSince(stt *collect.Stats) time.Duration {
	if v, ok := stt.LastFetch.Load().(time.Time); ok && !v.IsZero() {
		return time.Since(v)
	}
	return -1
}

func errText(stt *collect.Stats) string {
	if v, ok := stt.LastErr.Load().(string); ok && v != "" {
		return v
	}
	return "（无）"
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	switch level {
	case "debug":
		lv = slog.LevelDebug
	case "warn":
		lv = slog.LevelWarn
	case "error":
		lv = slog.LevelError
	default:
		lv = slog.LevelInfo
	}
	h := slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv})
	return slog.New(h)
}
