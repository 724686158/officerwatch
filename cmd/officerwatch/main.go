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
