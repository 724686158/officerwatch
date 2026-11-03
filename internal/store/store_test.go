package store

import (
	"path/filepath"
	"testing"
	"time"

	"officerwatch/internal/model"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("打开测试库失败: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestOfficerUpsertKeepsToggle(t *testing.T) {
	s := newTestStore(t)
	list := []model.Officer{
		{TypeID: 13654, Name: "Cormack Vaaja", Faction: "Serpentis", GroupID: 574, GroupName: "Asteroid Serpentis Officer", Enabled: true},
		{TypeID: 13661, Name: "Brynn Jerdola", Faction: "Serpentis", GroupID: 574, GroupName: "Asteroid Serpentis Officer", Enabled: false},
	}
	if added, err := s.UpsertOfficers(list); err != nil || added != 2 {
		t.Fatalf("首次写入应新增 2 条，得到 %d, err=%v", added, err)
	}

	// 用户关掉了 13654
	if err := s.SetOfficerEnabled(13654, false); err != nil {
		t.Fatal(err)
	}
	// 再同步一次名录（模拟 24 小时后的例行同步）
	if added, err := s.UpsertOfficers(list); err != nil || added != 0 {
		t.Fatalf("重复同步不该新增，得到 %d, err=%v", added, err)
	}
	o, ok, err := s.OfficerByType(13654)
	if err != nil || !ok {
		t.Fatalf("查不到官员: %v %v", ok, err)
	}
	if o.Enabled {
		t.Fatal("例行的名录同步把用户关掉的开关又打开了 —— 这是前身踩过的坑")
	}
}

func TestKillIdempotentAndOverview(t *testing.T) {
	s := newTestStore(t)
	_, err := s.UpsertOfficers([]model.Officer{{TypeID: 13654, Name: "Cormack Vaaja", Faction: "Serpentis", Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	k := Kill{
		KillmailID: 132678033, OfficerTypeID: 13654, Role: model.RoleOfficerKilledPlayer,
		KillmailTime: time.Now().UTC().Add(-time.Hour), TotalValue: 1.16e9,
		SystemName: "Jita", RegionName: "The Forge", VictimCharID: 2117711114, VictimShipName: "Vexor",
		NotifyStatus: "sent",
	}
	ins, err := s.InsertKill(&k, "{}")
	if err != nil || !ins {
		t.Fatalf("首次插入应成功: %v %v", ins, err)
	}
	ins, err = s.InsertKill(&k, "{}")
	if err != nil {
		t.Fatal(err)
	}
	if ins {
		t.Fatal("同一条 killmail 重复插入应被忽略（主键幂等）")
	}

	list, total, err := s.ListKills(KillFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if total != 1 || len(list) != 1 || list[0].OfficerName != "Cormack Vaaja" {
		t.Fatalf("列表查询不对: total=%d len=%d", total, len(list))
	}
	if list[0].Faction != "Serpentis" || list[0].ZKBURL == "" {
		t.Fatalf("关联字段没填上: %+v", list[0])
	}

	ov, err := s.GetOverview()
	if err != nil {
		t.Fatal(err)
	}
	if ov.Total != 1 || ov.Today != 1 || ov.ValueToday != 1.16e9 {
		t.Fatalf("概览统计不对: %+v", ov)
	}
	if ov.OfficersOn != 1 || ov.OfficersAll != 1 {
		t.Fatalf("官员计数不对: %+v", ov)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	s := newTestStore(t)
	def := s.LoadSettings()
	if def.Role != model.RoleOfficerKilledPlayer || !def.NotifyEnabled {
		t.Fatalf("默认设置不对: %+v", def)
	}
	st := def
	st.MinValue = 2e8
	st.Role = "both"
	st.Factions = []string{"Serpentis", "Guristas"}
	st.QuietEnabled = true
	st.QuietStart, st.QuietEnd = 23, 7
	if err := s.SaveSettings(st); err != nil {
		t.Fatal(err)
	}
	got := s.LoadSettings()
	if got.MinValue != 2e8 || got.Role != "both" || len(got.Factions) != 2 || !got.QuietEnabled {
		t.Fatalf("设置没存住: %+v", got)
	}
}

func TestDailyFillsMissingDays(t *testing.T) {
	s := newTestStore(t)
	_, _ = s.UpsertOfficers([]model.Officer{{TypeID: 13654, Name: "Cormack Vaaja", Faction: "Serpentis"}})
	now := time.Now().UTC()
	_, err := s.InsertKill(&Kill{KillmailID: 1, OfficerTypeID: 13654, Role: "npc_kill",
		KillmailTime: now.AddDate(0, 0, -3), TotalValue: 1e8, NotifyStatus: "sent"}, "{}")
	if err != nil {
		t.Fatal(err)
	}
	daily, err := s.Daily(7)
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 7 {
		t.Fatalf("应补齐 7 天，得到 %d", len(daily))
	}
	sum := 0
	for _, d := range daily {
		sum += d.Kills
	}
	if sum != 1 {
		t.Fatalf("总击杀应为 1，得到 %d", sum)
	}
}

func TestEventTrim(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 520; i++ {
		s.AddEvent("info", "test", "e")
	}
	ev, err := s.ListEvents(500)
	if err != nil {
		t.Fatal(err)
	}
	if len(ev) > 500 {
		t.Fatalf("事件表没有裁剪，%d 条", len(ev))
	}
}
