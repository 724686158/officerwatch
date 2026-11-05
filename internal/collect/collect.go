// Package collect 采集 + 匹配 + 入库 + 触发通知的编排层。
//
// 一条 killmail 要穿过四道关：
//  1. 匹配：这条里有官员吗？（攻击方是 NPC 官员 / 受害方是官员 NPC）
//  2. 开关：这个官员我们开着吗？
//  3. 口径：价值、角色、阵营、静默时段、限流（store.Settings）
//  4. 去重：killmail_id 是主键，重复推送直接忽略
package collect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"officerwatch/internal/esi"
	"officerwatch/internal/locs"
	"officerwatch/internal/model"
	"officerwatch/internal/notify"
	"officerwatch/internal/source"
	"officerwatch/internal/store"
)

// Stats 运行期计数器（状态页用）。
type Stats struct {
	Fetched     atomic.Int64
	Matched     atomic.Int64
	Stored      atomic.Int64
	Notified    atomic.Int64
	Skipped     atomic.Int64
	Errors      atomic.Int64
	ConsecFails atomic.Int64 // 连续失败次数（成功即归零）
	LastSeq     atomic.Int64
	LastFetch   atomic.Value // time.Time 最近一次成功取到文件
	LastAdvance atomic.Value // time.Time 游标上次真正前进（确实收到了新击杀）
	LastErr     atomic.Value // string
	AtFrontier  atomic.Bool  // 已追平上游（404 是"还没新数据"，不是故障）
	Waits       atomic.Int64 // 连续 404 次数
	Started     time.Time
}

// Collector 采集器。
type Collector struct {
	st  *store.Store
	res *esi.Resolver
	ec  *esi.Client
	nt  *notify.Notifier
	log *slog.Logger
	stt *Stats

	r2z2 *source.R2Z2
	zkb  *source.ZKB

	mu     sync.Mutex
	runCfg RunConfig
}

// RunConfig 采集模式与节奏。
type RunConfig struct {
	Mode          string // r2z2 | zkb | both
	ZKBInterval   time.Duration
	BackfillHours int
}

// New 构造采集器。
func New(st *store.Store, res *esi.Resolver, ec *esi.Client, nt *notify.Notifier, log *slog.Logger) *Collector {
	return &Collector{
		st:   st,
		res:  res,
		ec:   ec,
		nt:   nt,
		log:  log,
		stt:  &Stats{Started: time.Now()},
		r2z2: source.NewR2Z2(""),
		zkb:  source.NewZKB(""),
	}
}

// Stats 返回计数器指针（只读用）。
func (c *Collector) Stats() *Stats { return c.stt }

// MatchOfficer 在一条 killmail 里找官员。纯函数，方便单测。
//
// 判定依据：官员是 NPC，NPC 在 killmail 里**没有 character_id**；
// 所以"攻击方没有 character_id 且船型在名录里"才是官员打人，
// 而不能只看船型（玩家开不了官员船，但未来 PT 船/特殊活动船可能撞 ID）。
func MatchOfficer(km *model.Killmail, officers map[int64]bool) *model.Match {
	// 先看受害方：玩家打死了官员 —— 这条更值钱（官员掉官员装备），优先。
	if km.Victim.CharacterID == 0 && officers[km.Victim.ShipTypeID] {
		return &model.Match{
			OfficerTypeID:      km.Victim.ShipTypeID,
			Role:               model.RoleOfficerDown,
			AttackerShipTypeID: km.Victim.ShipTypeID,
		}
	}
	for _, a := range km.Attackers {
		if a.CharacterID != 0 {
			continue // 玩家，不是官员
		}
		if officers[a.ShipTypeID] {
			return &model.Match{
				OfficerTypeID:      a.ShipTypeID,
				Role:               model.RoleOfficerKilledPlayer,
				AttackerShipTypeID: a.ShipTypeID,
				FinalBlowByOfficer: a.FinalBlow,
			}
		}
	}
	return nil
}

// Handle 处理一条来源包（后台采集走这里，只认"开着"的官员）。
func (c *Collector) Handle(ctx context.Context, p *model.Payload) bool {
	officers, err := c.st.EnabledOfficerTypes()
	if err != nil {
		c.fail(err)
		return false
	}
	matched, _, err := c.process(ctx, p, officers)
	if err != nil {
		c.fail(err)
		return false
	}
	return matched
}

// Import 手工导入一条 killmail（页面上的"KillID 导入"）。
//
// 与后台采集的区别：这里用**全量名录**匹配，所以即使那个官员被关掉了也能导入；
// 但通知仍然走同一套口径（价值/静默/限流），不会被当成绕过开关的后门。
func (c *Collector) Import(ctx context.Context, p *model.Payload) (bool, error) {
	list, err := c.st.ListOfficers()
	if err != nil {
		return false, err
	}
	all := map[int64]bool{}
	for _, o := range list {
		all[o.TypeID] = true
	}
	if MatchOfficer(&p.ESI, all) == nil {
		return false, nil
	}
	matched, _, err := c.process(ctx, p, all)
	return matched, err
}

// process 是采集与导入的公共路径：匹配 → 入库 → 按口径通知。
func (c *Collector) process(ctx context.Context, p *model.Payload, officers map[int64]bool) (bool, bool, error) {
	c.stt.Fetched.Add(1)
	c.stt.LastFetch.Store(time.Now())

	km := &p.ESI
	if km.KillmailID == 0 {
		km.KillmailID = p.KillmailID
	}

	m := MatchOfficer(km, officers)
	if m == nil {
		return false, false, nil
	}
	c.stt.Matched.Add(1)

	// 补全名字（星系/星域/安全等级/受害者座舰/受害者身份）——
	// 任一步失败都不阻塞入库，只是邮件里少一行。
	sysName, regionName, sysSec := c.res.SystemInfo(ctx, km.SolarSystemID)
	victimShip := c.res.TypeName(ctx, km.Victim.ShipTypeID)
	victimName := c.res.CharacterName(ctx, km.Victim.CharacterID)
	victimCorp := c.res.CorporationName(ctx, km.Victim.CorporationID)
	victimAlliance := c.res.AllianceName(ctx, km.Victim.AllianceID)

	// 死亡星体（抢官员的核心情报）+ 距"家"多少跳
	var locID int64
	var locName string
	if p.ZKB.LocationID != 0 {
		locID = p.ZKB.LocationID
		if l, ok := locs.Lookup(c.st.DB(), locID); ok {
			locName = fmt.Sprintf("%s（%s）", l.Short(sysName), l.Kind())
		}
	}
	jumps := c.res.JumpsFromHome(ctx, km.SolarSystemID, c.st.LoadSettings().HomeSystem)

	k := &store.Kill{
		KillmailID:         km.KillmailID,
		OfficerTypeID:      m.OfficerTypeID,
		Role:               m.Role,
		KillmailTime:       km.ParsedTime(),
		TotalValue:         p.ZKB.TotalValue,
		FittedValue:        p.ZKB.FittedValue,
		DroppedValue:       p.ZKB.DroppedValue,
		DestroyedValue:     p.ZKB.DestroyedValue,
		Points:             p.ZKB.Points,
		Npc:                p.ZKB.Npc,
		Solo:               p.ZKB.Solo,
		Awox:               p.ZKB.Awox,
		Labels:             p.ZKB.Labels,
		SystemID:           km.SolarSystemID,
		SystemName:         sysName,
		RegionName:         regionName,
		SystemSec:          sysSec,
		LocationID:         locID,
		LocationName:       locName,
		Jumps:              jumps,
		VictimCharID:       km.Victim.CharacterID,
		VictimName:         victimName,
		VictimCorpName:     victimCorp,
		VictimAllianceName: victimAlliance,
		VictimCorpID:       km.Victim.CorporationID,
		VictimAlliance:     km.Victim.AllianceID,
		VictimShipID:       km.Victim.ShipTypeID,
		VictimShipName:     victimShip,
		AttackerCount:      len(km.Attackers),
		FinalBlowShip:      m.AttackerShipTypeID,
		Hash:               p.Hash,
		NotifyStatus:       "pending",
	}
	if k.KillmailTime.IsZero() {
		k.KillmailTime = time.Now().UTC()
	}
	raw, _ := json.Marshal(p)

	inserted, err := c.st.InsertKill(k, string(raw))
	if err != nil {
		return true, false, fmt.Errorf("入库失败 kill %d: %w", k.KillmailID, err)
	}
	if !inserted {
		return true, false, nil
	}
	c.stt.Stored.Add(1)

	// 通知口径
	st := c.st.LoadSettings()
	send, reason := Decide(c.st, st, *k, time.Now())
	if !send {
		c.stt.Skipped.Add(1)
		_ = c.st.MarkNotify(k.KillmailID, "filtered", reason)
		c.log.Info("跳过通知", "kill", k.KillmailID, "reason", reason)
		return true, true, nil
	}

	if c.nt == nil || len(c.nt.Channels()) == 0 {
		_ = c.st.MarkNotify(k.KillmailID, "no_channel", "未配置通知渠道")
		return true, true, nil
	}

	// 重新取一次（带上官员名字/阵营），通知模板要用
	full, _, err := c.st.GetKill(k.KillmailID)
	if err != nil {
		full = k
	}
	if err := c.nt.Notify(ctx, notify.NewMessage(*full, st.AtAll, st.HomeSystem, st.Recipients)); err != nil {
		c.stt.Errors.Add(1)
		_ = c.st.MarkNotify(k.KillmailID, "failed", err.Error())
		c.st.AddEvent("error", "notify", fmt.Sprintf("kill %d 通知失败: %v", k.KillmailID, err))
		c.log.Error("通知失败", "kill", k.KillmailID, "err", err)
		return true, true, nil
	}
	c.stt.Notified.Add(1)
	_ = c.st.MarkNotify(k.KillmailID, "sent", "")
	c.st.AddEvent("info", "notify", fmt.Sprintf("已推送 kill %d（%s）", k.KillmailID, full.OfficerName))
	return true, true, nil
}

// Decide 判断一条命中的击杀要不要推送。返回 (是否推送, 原因)。
// 抽成独立函数是为了能单测 —— 前身把这些规则埋在 1700 行的 handler 里，
// 改一个阈值要读半天。
func Decide(st *store.Store, set store.Settings, k store.Kill, now time.Time) (bool, string) {
	if !set.NotifyEnabled {
		return false, "通知总开关关闭"
	}
	switch set.Role {
	case model.RoleOfficerKilledPlayer:
		if k.Role != model.RoleOfficerKilledPlayer {
			return false, "口径=只报被官员击杀"
		}
	case model.RoleOfficerDown:
		if k.Role != model.RoleOfficerDown {
			return false, "口径=只报官员被击杀"
		}
	}
	if len(set.Factions) > 0 {
		ok := false
		for _, f := range set.Factions {
			if f == k.Faction {
				ok = true
				break
			}
		}
		if !ok {
			return false, "阵营不在白名单"
		}
	}
	if set.MinValue > 0 && k.TotalValue < set.MinValue {
		return false, fmt.Sprintf("价值 %.2f 低于阈值 %.2f", k.TotalValue, set.MinValue)
	}
	if set.QuietEnabled && inQuietHours(set, now) {
		return false, "静默时段"
	}
	if set.MaxPerHour > 0 && st != nil {
		if n, err := st.CountNotifiedSince(now.Add(-time.Hour)); err == nil && n >= set.MaxPerHour {
			return false, fmt.Sprintf("限流：过去一小时已发 %d 条", n)
		}
	}
	return true, ""
}

// inQuietHours 支持跨零点的窗口（如 23 → 7）。
func inQuietHours(set store.Settings, now time.Time) bool {
	h := now.Local().Hour()
	s, e := set.QuietStart, set.QuietEnd
	if s == e {
		return false
	}
	if s < e {
		return h >= s && h < e
	}
	return h >= s || h < e
}

// fail 记录一次采集失败。
//
// 单次网络抖动（Cloudflare 偶发 connection reset）不值得往事件表里塞一条，
// 连续 3 次才记一次事件；恢复时也记一条，方便回看"断了多久"。
func (c *Collector) fail(err error) {
	c.stt.Errors.Add(1)
	n := c.stt.ConsecFails.Add(1)
	c.stt.LastErr.Store(err.Error())
	if n >= 3 && n%3 == 0 {
		c.log.Error("采集连续失败", "consecutive", n, "err", err)
		c.st.AddEvent("error", "collect", fmt.Sprintf("连续 %d 次采集失败: %v", n, err))
	}
}

// ok 记录一次成功（复位连续失败计数）。
func (c *Collector) ok() {
	if n := c.stt.ConsecFails.Swap(0); n >= 3 {
		c.log.Info("采集恢复", "after", n)
		c.st.AddEvent("info", "collect", fmt.Sprintf("采集已恢复（此前连续失败 %d 次）", n))
	}
}

// ─────────────────────────────── 运行循环

// RunR2Z2 主循环：按序列号推进，404 就睡 6 秒（zkb 官方建议的最小值）。
func (c *Collector) RunR2Z2(ctx context.Context) error {
	seq, err := c.resumeSequence(ctx)
	if err != nil {
		return err
	}
	c.log.Info("R2Z2 开始", "from", seq)
	c.st.AddEvent("info", "collect", fmt.Sprintf("R2Z2 采集启动，起始序列 %d", seq))

	var lastFrontierCheck time.Time
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p, err := c.r2z2.Fetch(ctx, seq)
		switch {
		case errors.Is(err, source.ErrNotYet):
			// 404 = 这个序列还没生成。**这不一定是故障**：
			//   - 追平上游时，每个新击杀到达前都会 404（正常空闲）
			//   - 追平但上游也没新数据时，会一直 404（上游停摆，不是我们的问题）
			//   - 我们落后于 head 却一直 404，才是本地/网络问题
			// 所以每 3 分钟对照一次 head 判个方向，别把"等数据"当成"采集停了"。
			c.stt.AtFrontier.Store(true)
			c.stt.Waits.Add(1)
			if time.Since(lastFrontierCheck) > 3*time.Minute {
				lastFrontierCheck = time.Now()
				if head, herr := c.r2z2.Sequence(ctx); herr == nil {
					if head > seq {
						c.stt.AtFrontier.Store(false)
						c.fail(fmt.Errorf("落后于上游：游标 %d，head %d（本地取不到）", seq, head))
					} else {
						c.ok() // 追平等待，算健康
					}
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(6 * time.Second):
			}
			continue
		case errors.Is(err, source.ErrBanned):
			c.fail(fmt.Errorf("R2Z2 返回 403（UA 被拦或 IP 被限）"))
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(5 * time.Minute):
			}
			continue
		case err != nil:
			c.fail(err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(15 * time.Second):
			}
			continue
		}

		c.Handle(ctx, p)
		c.ok()
		c.stt.AtFrontier.Store(false)
		c.stt.Waits.Store(0)
		seq++
		c.stt.LastSeq.Store(seq)
		c.stt.LastAdvance.Store(time.Now())
		_ = c.st.SetMeta("r2z2_seq", fmt.Sprint(seq))

		// 官方限流 15/s，这里 10/s，留余量。
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// resumeSequence 断点续传；没有游标时从"现在"开始（或按配置回填几小时）。
func (c *Collector) resumeSequence(ctx context.Context) (int64, error) {
	if v := c.st.GetMeta("r2z2_seq"); v != "" {
		var seq int64
		if _, err := fmt.Sscanf(v, "%d", &seq); err == nil && seq > 0 {
			return seq, nil
		}
	}
	head, err := c.r2z2.Sequence(ctx)
	if err != nil {
		return 0, fmt.Errorf("取初始序列失败: %w", err)
	}
	// 回填：全服平均 5.5 秒一条击杀，按小时折算成序列号回退。
	st := c.st.LoadSettings()
	hours := st.BackfillHours
	if hours > 24 {
		hours = 24 // R2Z2 只保留 24 小时
	}
	if hours > 0 {
		back := int64(hours) * 655 // 3600/5.5 ≈ 655
		if head > back {
			head -= back
		}
	}
	return head, nil
}

// RunZKB 兜底循环：按官员船型轮询 zkb（接口有 1 小时缓存，10 分钟轮一次足够）。
func (c *Collector) RunZKB(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = 10 * time.Minute
	}
	c.st.AddEvent("info", "collect", "zKillboard 轮询模式启动")
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		types, err := c.enabledTypes()
		if err != nil {
			c.fail(err)
		}
		for _, tid := range types {
			list, err := c.zkb.Recent(ctx, tid)
			if err != nil {
				c.fail(fmt.Errorf("zkb 轮询船型 %d 失败: %w", tid, err))
				if errors.Is(err, source.ErrBanned) {
					break
				}
				continue
			}
			for i := range list {
				c.Handle(ctx, &list[i])
			}
			// 别把 zkb 打毛：每个船型之间喘口气
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}
	}
}

func (c *Collector) enabledTypes() ([]int64, error) {
	officers, err := c.st.ListOfficers()
	if err != nil {
		return nil, err
	}
	out := []int64{}
	for _, o := range officers {
		if o.Enabled {
			out = append(out, o.TypeID)
		}
	}
	return out, nil
}
