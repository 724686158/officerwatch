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
