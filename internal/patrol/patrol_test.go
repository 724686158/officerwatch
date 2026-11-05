package patrol

import (
	"errors"
	"testing"
	"time"
)

func now() time.Time { return time.Date(2026, 11, 4, 10, 0, 0, 0, time.UTC) }

func byName(checks []Check, name string) Check {
	for _, c := range checks {
		if c.Name == name {
			return c
		}
	}
	return Check{}
}

func TestEvaluateAllGood(t *testing.T) {
	checks := Evaluate(Inputs{
		IdleSec: 12, Lag: 3, Channels: 1,
		CatalogSynced: now().Add(-2 * time.Hour),
		Now:           now(),
	})
	if HasFailure(checks) {
		t.Fatalf("不该有失败: %+v", checks)
	}
	for _, c := range checks {
		if c.Status != OK {
			t.Errorf("%s 应为 OK，得到 %s（%s）", c.Name, c.Status, c.Detail)
		}
	}
	if Summary(checks) != "全部正常" {
		t.Errorf("摘要不对: %s", Summary(checks))
	}
}

func TestEvaluateIdleDegrades(t *testing.T) {
	warn := Evaluate(Inputs{IdleSec: 15 * 60, Channels: 1, Now: now()})
	if c := byName(warn, "collector_fresh"); c.Status != Warn {
		t.Errorf("15 分钟没拉取应为 warn，得到 %s", c.Status)
	}
	fail := Evaluate(Inputs{IdleSec: 45 * 60, Channels: 1, Now: now()})
	if c := byName(fail, "collector_fresh"); c.Status != Fail {
		t.Errorf("45 分钟没拉取应为 fail，得到 %s", c.Status)
	}
	if !HasFailure(fail) {
		t.Error("应判定为失败")
	}
}

func TestEvaluateFrontierIdleIsWarnNotFail(t *testing.T) {
	// 已追平上游时空闲 40 分钟：不是我们坏了，只该 warn
	checks := Evaluate(Inputs{IdleSec: 40 * 60, AtFrontier: true, Channels: 1, Now: now()})
	c := byName(checks, "collector_fresh")
	if c.Status != Warn {
		t.Errorf("追平上游空闲应为 warn，得到 %s（%s）", c.Status, c.Detail)
	}
	if HasFailure(checks) {
		t.Error("追平上游空闲不该判定为失败")
	}
	// 没追平却空闲 40 分钟，仍然 fail
	checks2 := Evaluate(Inputs{IdleSec: 40 * 60, Channels: 1, Now: now()})
	if byName(checks2, "collector_fresh").Status != Fail {
		t.Error("落后且空闲 40 分钟应判 fail")
	}
}

func TestEvaluateLag(t *testing.T) {
	if c := byName(Evaluate(Inputs{IdleSec: 1, Lag: 800, Channels: 1, Now: now()}), "collector_lag"); c.Status != Warn {
		t.Errorf("落后 800 应为 warn，得到 %s", c.Status)
	}
	if c := byName(Evaluate(Inputs{IdleSec: 1, Lag: 5000, Channels: 1, Now: now()}), "collector_lag"); c.Status != Fail {
		t.Errorf("落后 5000 应为 fail，得到 %s", c.Status)
	}
}

func TestEvaluateDBAndChannels(t *testing.T) {
	checks := Evaluate(Inputs{IdleSec: 1, Channels: 0, DBWriteErr: errors.New("database is locked"), Now: now()})
	if byName(checks, "db_write").Status != Fail {
		t.Error("写库失败应判 fail")
	}
	if byName(checks, "channels").Status != Fail {
		t.Error("没有通道应判 fail（告警发不出去）")
	}
}

func TestEvaluateCatalogAge(t *testing.T) {
	warn := Evaluate(Inputs{IdleSec: 1, Channels: 1, CatalogSynced: now().Add(-10 * 24 * time.Hour), Now: now()})
	if byName(warn, "catalog_age").Status != Warn {
		t.Errorf("10 天未同步应为 warn，得到 %s", byName(warn, "catalog_age").Status)
	}
	fail := Evaluate(Inputs{IdleSec: 1, Channels: 1, CatalogSynced: now().Add(-40 * 24 * time.Hour), Now: now()})
	if byName(fail, "catalog_age").Status != Fail {
		t.Errorf("40 天未同步应为 fail，得到 %s", byName(fail, "catalog_age").Status)
	}
}

func TestEvaluateRestartNoted(t *testing.T) {
	checks := Evaluate(Inputs{IdleSec: 1, Channels: 1, BootCount: 3, BootCountAdded: true, Now: now()})
	if c := byName(checks, "restart"); c.Status != Warn {
		t.Errorf("重启应为 warn，得到 %s", c.Status)
	}
	// warn 不该触发告警
	if HasFailure(checks) {
		t.Error("只有 warn 时不该判定失败")
	}
}

func TestSummaryPicksWorst(t *testing.T) {
	checks := []Check{
		{"a", "甲", OK, ""},
		{"b", "乙", Warn, "慢了"},
		{"c", "丙", Fail, "挂了"},
	}
	got := Summary(checks)
	if got != "失败 丙: 挂了" {
		t.Errorf("摘要应优先报 fail，得到 %q", got)
	}
}
