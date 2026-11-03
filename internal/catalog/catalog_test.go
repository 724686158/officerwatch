package catalog

import "testing"

func TestFaction(t *testing.T) {
	cases := map[string]string{
		"Asteroid Serpentis Officer":            "Serpentis",
		"Asteroid Blood Raiders Officer":        "Blood Raiders",
		"Asteroid Sansha's Nation Officer":      "Sansha's Nation",
		"Asteroid Angel Cartel Officer Frigate": "Angel Cartel",
		"Asteroid Rogue Drone Officer":          "Rogue Drone",
		"Asteroid Guristas Officer Cruiser":     "Guristas",
		"Officer":                               "Officer",
	}
	for in, want := range cases {
		if got := Faction(in); got != want {
			t.Errorf("Faction(%q)=%q，期望 %q", in, got, want)
		}
	}
}

func TestDefaultEnabledMatchesLegacy(t *testing.T) {
	// 前身 EVE-RedisQ 盯的是天蛇 4 + 血袭者 4，默认值必须与之对齐
	want := []int64{13661, 13654, 13667, 13659, 13561, 13564, 13557, 13573}
	if len(DefaultEnabled) != len(want) {
		t.Fatalf("默认开启数应为 %d，得到 %d", len(want), len(DefaultEnabled))
	}
	for _, id := range want {
		if !DefaultEnabled[id] {
			t.Errorf("typeID %d 应该默认开启", id)
		}
	}
	if len(KnownGroups) != 16 {
		t.Errorf("已知官员船组应为 16 个，得到 %d", len(KnownGroups))
	}
}

func TestOfficerMetaComplete(t *testing.T) {
	if err := MetaError(); err != nil {
		t.Fatalf("元数据解析失败: %v", err)
	}
	// 34 位全服官员都应有中文译名与吨位
	if got := Count(); got < 34 {
		t.Fatalf("元数据只有 %d 位，应为 34 位（新加官员请跑 tools/build-officer-meta.py）", got)
	}
	validClass := map[string]bool{"battleship": true, "cruiser": true, "frigate": true}
	for _, tid := range []int64{13654, 13659, 13661, 13667, 84948, 84951} {
		m, ok := MetaFor(tid)
		if !ok {
			t.Fatalf("缺少 typeID %d 的元数据", tid)
		}
		if m.Zh == "" {
			t.Errorf("%s（%d）没有中文译名", m.Name, tid)
		}
		if !validClass[m.Class] {
			t.Errorf("%s 吨位取值非法: %q", m.Name, m.Class)
		}
		if m.Faction != "Serpentis" {
			t.Errorf("%s 阵营应为 Serpentis，得到 %q", m.Name, m.Faction)
		}
	}
	// Serpentis 的结构：4 战列 + 1 巡洋 + 1 护卫
	want := map[string]int64{"battleship": 4, "cruiser": 1, "frigate": 1}
	for _, tid := range []int64{13654, 13659, 13661, 13667, 84948, 84951} {
		m, _ := MetaFor(tid)
		want[m.Class]--
	}
	for cls, n := range want {
		if n != 0 {
			t.Errorf("Serpentis 的 %s 数量不对，差 %d", cls, n)
		}
	}
}

func TestFactionAndClassZh(t *testing.T) {
	if got := FactionZh("Serpentis"); got != "天蛇" {
		t.Errorf("Serpentis 中文名应为天蛇，得到 %q", got)
	}
	if got := FactionZh("Unknown Faction"); got != "Unknown Faction" {
		t.Errorf("未知阵营应原样返回，得到 %q", got)
	}
	if got := ClassZh("battleship"); got != "战列" {
		t.Errorf("battleship 应为战列，得到 %q", got)
	}
}
