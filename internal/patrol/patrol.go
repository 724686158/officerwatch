// Package patrol 是巡检：定期体检"看门狗之外"的那些静默故障。
//
// 为什么还要巡检（已经有采集侧看门狗了）：
//   - 看门狗只看"多久没成功拉取"，管不了"进程活着但落后两小时""邮件通道断了""库写不进去"
//   - 这些故障的共同点是**服务看起来正常**：systemd active、页面能开、但没有产出
//
// 规则（沿用作者另一套系统的口径）：
//   - 每 5 分钟一轮（可配），结果落库，保留最近 200 轮
//   - **连续 2 轮失败**才发告警，避免单次抖动半夜吵人；恢复时补一封"已恢复"
//   - 告警走通知通道（邮件），所以巡检本身也依赖通道可用——通道不可用会体现在 channels 检查项上
package patrol

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"officerwatch/internal/notify"
	"officerwatch/internal/store"
)

// Status 单项检查的结果。
type Status string

const (
	OK   Status = "ok"
	Warn Status = "warn"
	Fail Status = "fail"
)

// Check 一项检查。
type Check struct {
	Name   string `json:"name"`
	Label  string `json:"label"`
	Status Status `json:"status"`
	Detail string `json:"detail"`
}

// Inputs 一轮巡检需要的原料（由调用方采集，便于单测）。
type Inputs struct {
	IdleSec        int64     // 距上次成功拉取的秒数（-1 = 尚未开始）
	Lag            int64     // 游标落后 head 多少条
	Channels       int       // 已配置的通知通道数
	CatalogSynced  time.Time // 名录上次同步时间
	DBWriteErr     error     // 探测写库的结果
	BootCount      int64     // 本机启动次数（递增说明服务重启过）
	BootCountAdded bool      // 相比上一轮是否增加
	Now            time.Time
}

// 阈值集中放这里，便于调。
const (
	idleWarnSec = 10 * 60
	idleFailSec = 30 * 60
	lagWarn     = 600  // ≈30~60 分钟的击杀量
	lagFail     = 2000 // ≈2 小时以上
	catalogWarn = 7 * 24 * time.Hour
	catalogFail = 30 * 24 * time.Hour
)

// Evaluate 纯函数：把原料变成检查结果。方便单测，也方便以后加项。
func Evaluate(in Inputs) []Check {
	now := in.Now
	if now.IsZero() {
		now = time.Now()
	}
	checks := []Check{}

	// 1) 采集是否新鲜
	switch {
	case in.IdleSec < 0:
		checks = append(checks, Check{"collector_fresh", "采集新鲜度", Warn, "尚未开始拉取"})
	case in.IdleSec > idleFailSec:
		checks = append(checks, Check{"collector_fresh", "采集新鲜度", Fail,
			fmt.Sprintf("%d 分钟没有成功拉取", in.IdleSec/60)})
	case in.IdleSec > idleWarnSec:
		checks = append(checks, Check{"collector_fresh", "采集新鲜度", Warn,
			fmt.Sprintf("%d 分钟没有成功拉取", in.IdleSec/60)})
	default:
		checks = append(checks, Check{"collector_fresh", "采集新鲜度", OK,
			fmt.Sprintf("%d 秒前", in.IdleSec)})
	}

	// 2) 是否落后（活着但在补课）
	switch {
	case in.Lag >= lagFail:
		checks = append(checks, Check{"collector_lag", "序列进度", Fail,
			fmt.Sprintf("落后 %d 条", in.Lag)})
	case in.Lag >= lagWarn:
		checks = append(checks, Check{"collector_lag", "序列进度", Warn,
			fmt.Sprintf("落后 %d 条", in.Lag)})
	default:
		checks = append(checks, Check{"collector_lag", "序列进度", OK, "实时"})
	}

	// 3) 数据库可写
	if in.DBWriteErr != nil {
		checks = append(checks, Check{"db_write", "数据库可写", Fail, in.DBWriteErr.Error()})
	} else {
		checks = append(checks, Check{"db_write", "数据库可写", OK, ""})
	}

	// 4) 通知通道
	if in.Channels == 0 {
		checks = append(checks, Check{"channels", "通知通道", Fail, "没有配置任何通道，告警发不出去"})
	} else {
		checks = append(checks, Check{"channels", "通知通道", OK, fmt.Sprintf("%d 个", in.Channels)})
	}

	// 5) 名录新鲜度（CCP 加新官员后要能跟上）
	if in.CatalogSynced.IsZero() {
		checks = append(checks, Check{"catalog_age", "官员名录", Warn, "从未同步"})
	} else {
		age := now.Sub(in.CatalogSynced)
		switch {
		case age > catalogFail:
			checks = append(checks, Check{"catalog_age", "官员名录", Fail,
				fmt.Sprintf("%d 天未同步", int(age.Hours()/24))})
		case age > catalogWarn:
			checks = append(checks, Check{"catalog_age", "官员名录", Warn,
				fmt.Sprintf("%d 天未同步", int(age.Hours()/24))})
		default:
			checks = append(checks, Check{"catalog_age", "官员名录", OK,
				fmt.Sprintf("%d 小时前同步", int(age.Hours()))})
		}
	}

	// 6) 是否重启过（重启本身不是错，但要知道"刚才为什么断了"）
	if in.BootCountAdded {
		checks = append(checks, Check{"restart", "进程重启", Warn,
			fmt.Sprintf("本轮之间服务重启过（累计 %d 次）", in.BootCount)})
	} else {
		checks = append(checks, Check{"restart", "进程重启", OK, ""})
	}

	return checks
}

// HasFailure 有没有 fail 级问题（warn 不触发告警）。
func HasFailure(checks []Check) bool {
	for _, c := range checks {
		if c.Status == Fail {
			return true
		}
	}
	return false
}

// Summary 一行摘要，用于事件与告警标题。
func Summary(checks []Check) string {
	fails, warns := []string{}, []string{}
	for _, c := range checks {
		if c.Status == Fail {
			fails = append(fails, c.Label+": "+c.Detail)
		} else if c.Status == Warn {
			warns = append(warns, c.Label+": "+c.Detail)
		}
	}
	switch {
	case len(fails) > 0:
		return "失败 " + strings.Join(fails, "；")
	case len(warns) > 0:
		return "注意 " + strings.Join(warns, "；")
	default:
		return "全部正常"
	}
}

// Runner 周期巡检。
type Runner struct {
	st   *store.Store
	nt   *notify.Notifier
	log  *slog.Logger
	head func(ctx context.Context) (int64, error) // 取 R2Z2 当前 head
	idle func() int64                             // 距上次成功拉取的秒数
	cur  func() int64                             // 当前序列游标

	Interval time.Duration

	// 内部状态
	failStreak int
	alerted    bool
	lastBoot   int64
}

// New 构造。
func New(st *store.Store, nt *notify.Notifier, log *slog.Logger,
	head func(context.Context) (int64, error), idle, cursor func() int64) *Runner {
	return &Runner{st: st, nt: nt, log: log, head: head, idle: idle, cur: cursor, Interval: 5 * time.Minute}
}

// RunOnce 执行一轮：采集指标 → 评估 → 落库 → 必要时告警。
func (r *Runner) RunOnce(ctx context.Context) ([]Check, error) {
	now := time.Now()

	// 写库探测：把时间戳写进 meta，成本极低但能发现"库变只读/磁盘满"
	dbErr := r.st.SetMeta("patrol_probe", fmt.Sprint(now.Unix()))

	lag := int64(0)
	if r.head != nil {
		if h, err := r.head(ctx); err == nil {
			if c := r.cur(); c > 0 && h > c {
				lag = h - c
			}
		}
	}

	catalogSynced := time.Time{}
	if v := r.st.GetMeta("catalog_synced_at"); v != "" {
		catalogSynced, _ = time.Parse(time.RFC3339, v)
	}

	boot := r.bootCount()
	added := r.lastBoot != 0 && boot > r.lastBoot
	r.lastBoot = boot

	in := Inputs{
		IdleSec: r.idle(), Lag: lag, Channels: len(r.nt.Channels()),
		CatalogSynced: catalogSynced, DBWriteErr: dbErr,
		BootCount: boot, BootCountAdded: added, Now: now,
	}
	checks := Evaluate(in)
	ok := !HasFailure(checks)

	// 落库（保留最近 200 轮）
	b, _ := json.Marshal(checks)
	if _, err := r.st.SavePatrolRun(now, ok, Summary(checks), string(b)); err != nil {
		r.log.Warn("巡检结果落库失败", "err", err)
	}

	// 告警状态机：连续 2 轮失败才告警；恢复时补一封
	if !ok {
		r.failStreak++
		if r.failStreak >= 2 && !r.alerted {
			r.alert(ctx, checks, now)
			r.alerted = true
		}
	} else {
		if r.alerted {
			r.recover(ctx, now)
			r.alerted = false
		}
		r.failStreak = 0
	}
	return checks, nil
}

func (r *Runner) alert(ctx context.Context, checks []Check, now time.Time) {
	var b strings.Builder
	b.WriteString("officerwatch 巡检告警\n\n")
	b.WriteString("时间：" + now.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04") + "\n\n")
	for _, c := range checks {
		if c.Status == OK {
			continue
		}
		mark := "⚠️"
		if c.Status == Fail {
			mark = "❌"
		}
		fmt.Fprintf(&b, "%s %s：%s\n", mark, c.Label, c.Detail)
	}
	b.WriteString("\n详细见 状态页 → 巡检。")
	if err := r.nt.NotifyRaw(ctx, b.String()); err != nil {
		r.log.Error("巡检告警发送失败", "err", err)
	}
	r.st.AddEvent("error", "patrol", "巡检异常: "+Summary(checks))
}

func (r *Runner) recover(ctx context.Context, now time.Time) {
	msg := "officerwatch 巡检恢复\n\n时间：" +
		now.In(time.FixedZone("CST", 8*3600)).Format("2006-01-02 15:04") + "\n各项检查已恢复正常。"
	if err := r.nt.NotifyRaw(ctx, msg); err != nil {
		r.log.Error("巡检恢复通知发送失败", "err", err)
	}
	r.st.AddEvent("info", "patrol", "巡检恢复正常")
}

// bootCount 启动次数：每次进程启动 +1，用于识别"中途被重启"。
func (r *Runner) bootCount() int64 {
	var n int64
	fmt.Sscanf(r.st.GetMeta("boot_count"), "%d", &n)
	return n
}

// Loop 常驻。
func (r *Runner) Loop(ctx context.Context) {
	if r.Interval <= 0 {
		r.Interval = 5 * time.Minute
	}
	// 启动后等第一轮采集落地再体检，否则第一轮必然报"尚未开始拉取"（假警报）
	select {
	case <-ctx.Done():
		return
	case <-time.After(20 * time.Second):
	}
	// 先跑一轮，让状态页立刻有数据
	if _, err := r.RunOnce(ctx); err != nil {
		r.log.Warn("首轮巡检失败", "err", err)
	}
	t := time.NewTicker(r.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := r.RunOnce(ctx); err != nil {
				r.log.Warn("巡检失败", "err", err)
			}
		}
	}
}
