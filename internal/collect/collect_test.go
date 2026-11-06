package collect

import (
	"testing"
	"time"

	"officerwatch/internal/model"
	"officerwatch/internal/store"
)

func km(attackers []model.Attacker, victim model.Victim) *model.Killmail {
	return &model.Killmail{KillmailID: 1, KillmailTime: "2026-11-03T10:00:00Z", SolarSystemID: 30000142,
		Attackers: attackers, Victim: victim}
}

func TestMatchOfficer(t *testing.T) {
	officers := map[int64]bool{13654: true, 13661: true}

	t.Run("官员打死玩家", func(t *testing.T) {
		m := MatchOfficer(km([]model.Attacker{
			{CharacterID: 2110000001, ShipTypeID: 4302},          // 玩家
			{CharacterID: 0, ShipTypeID: 13654, FinalBlow: true}, // 官员 NPC
		}, model.Victim{CharacterID: 9000001, ShipTypeID: 626}), officers)
		if m == nil || m.Role != model.RoleOfficerKilledPlayer || m.OfficerTypeID != 13654 {
			t.Fatalf("期望识别为官员击杀，得到 %+v", m)
		}
		if !m.FinalBlowByOfficer {
			t.Error("final blow 信息丢了")
		}
	})

	t.Run("玩家打死官员", func(t *testing.T) {
		m := MatchOfficer(km([]model.Attacker{
			{CharacterID: 2110000001, ShipTypeID: 17703},
		}, model.Victim{CharacterID: 0, ShipTypeID: 13661}), officers)
		if m == nil || m.Role != model.RoleOfficerDown || m.OfficerTypeID != 13661 {
			t.Fatalf("期望识别为官员被爆，得到 %+v", m)
		}
	})

	t.Run("玩家开同名船型不算官员", func(t *testing.T) {
		// 有 character_id 的攻击者永远不是 NPC，即便船型撞上了名录
		m := MatchOfficer(km([]model.Attacker{
			{CharacterID: 2110000001, ShipTypeID: 13654},
		}, model.Victim{CharacterID: 9000001, ShipTypeID: 626}), officers)
		if m != nil {
			t.Fatalf("不该匹配，得到 %+v", m)
		}
	})

	t.Run("无关击杀", func(t *testing.T) {
		m := MatchOfficer(km([]model.Attacker{{CharacterID: 1, ShipTypeID: 670}},
			model.Victim{CharacterID: 2, ShipTypeID: 626}), officers)
		if m != nil {
			t.Fatal("不该匹配")
		}
	})

	t.Run("官员被爆优先于官员击杀", func(t *testing.T) {
		m := MatchOfficer(km([]model.Attacker{{CharacterID: 0, ShipTypeID: 13654}},
			model.Victim{CharacterID: 0, ShipTypeID: 13661}), officers)
		if m == nil || m.Role != model.RoleOfficerDown {
			t.Fatalf("期望 officer_down 优先，得到 %+v", m)
		}
	})
}

func TestDecide(t *testing.T) {
	base := store.Kill{OfficerTypeID: 13654, Faction: "Serpentis",
		Role: model.RoleOfficerKilledPlayer, TotalValue: 5e8}
	now := time.Date(2026, 11, 3, 20, 0, 0, 0, time.Local)

	def := store.DefaultSettings()
	if ok, why := Decide(nil, def, base, now); !ok {
		t.Fatalf("默认口径应放行，被拦：%s", why)
	}

	t.Run("通知总开关", func(t *testing.T) {
		st := def
		st.NotifyEnabled = false
		if ok, _ := Decide(nil, st, base, now); ok {
			t.Fatal("总开关关闭时不该发")
		}
	})

	t.Run("角色口径", func(t *testing.T) {
		st := def
		st.Role = model.RoleOfficerDown
		if ok, _ := Decide(nil, st, base, now); ok {
			t.Fatal("只报官员被爆时，官员击杀不该发")
		}
	})

	t.Run("价值阈值", func(t *testing.T) {
		st := def
		st.MinValue = 1e9
		if ok, _ := Decide(nil, st, base, now); ok {
			t.Fatal("低于阈值不该发")
		}
	})

	t.Run("阵营白名单", func(t *testing.T) {
		st := def
		st.Factions = []string{"Guristas"}
		if ok, _ := Decide(nil, st, base, now); ok {
			t.Fatal("不在白名单不该发")
		}
		st.Factions = []string{"Serpentis"}
		if ok, _ := Decide(nil, st, base, now); !ok {
			t.Fatal("在白名单应该发")
		}
	})

	t.Run("静默时段", func(t *testing.T) {
		st := def
		st.QuietEnabled = true
		st.QuietStart = 19
		st.QuietEnd = 21
		if ok, _ := Decide(nil, st, base, now); ok {
			t.Fatal("静默时段内不该发")
		}
		// 跨零点窗口：23 → 7
		st.QuietStart, st.QuietEnd = 23, 7
		if ok, why := Decide(nil, st, base, now); !ok {
			t.Fatalf("20 点不在 23-7 窗口内，应该发（被拦：%s）", why)
		}
		late := time.Date(2026, 11, 3, 2, 0, 0, 0, time.Local)
		if ok, _ := Decide(nil, st, base, late); ok {
			t.Fatal("2 点在 23-7 窗口内，不该发")
		}
	})
}

func TestFreshEnough(t *testing.T) {
	now := time.Date(2026, 11, 6, 4, 32, 0, 0, time.UTC)
	cases := []struct {
		name string
		t    time.Time
		want bool
	}{
		{"刚刚", now.Add(-30 * time.Second), true},
		{"5 分钟前", now.Add(-5 * time.Minute), true},
		{"29 分钟前", now.Add(-29 * time.Minute), true},
		{"31 分钟前", now.Add(-31 * time.Minute), false},
		{"昨天的", now.Add(-24 * time.Hour), false},
		{"零值", time.Time{}, false},
	}
	for _, c := range cases {
		if got := FreshEnough(c.t, now, 30*time.Minute); got != c.want {
			t.Errorf("%s：FreshEnough=%v，期望 %v", c.name, got, c.want)
		}
	}
	// 门槛为 0 时用默认 30 分钟
	if FreshEnough(now.Add(-45*time.Minute), now, 0) {
		t.Error("maxAge=0 应退回 30 分钟默认值，45 分钟前的不该推送")
	}
}
