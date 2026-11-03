// Package catalog 维护官员名录。
//
// 名录不手抄：ESI 的 Entity 分类（category_id=11）里，船组名字含 "Officer" 的就是官员船
// （例如 "Asteroid Serpentis Officer" 恰好 4 条船：Cormack Vaaja / Tuvan Orth /
// Brynn Jerdola / Setele Schellan）。这是和客户端同源的权威数据。
//
// 两级同步：
//   - 常规同步：只拉已知的 16 个官员船组（约 50 次 ESI 请求，几十秒）
//   - 全量重扫：扫完 Entity 分类下 415 个组，找新出现的官员船组（CCP 加新官员时用）
package catalog

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"officerwatch/internal/esi"
	"officerwatch/internal/model"
	"officerwatch/internal/store"
)

// KnownGroups 是当前已知的官员船组（2026-11 快照）。
// 常规同步只打这 16 个组；全量重扫会发现新增的。
var KnownGroups = []int64{
	553, 559, 564, 569, 574, 1174, // 战列/巡洋级：Angel / Blood Raiders / Guristas / Sansha / Serpentis / Rogue Drone
	4795, 4796, 4797, 4798, 4799, 4800, 4801, 4802, 4803, 4804, // 新加的官员巡洋/护卫
}

// DefaultEnabled 是"开箱即报"的官员：前身 EVE-RedisQ 盯的那 8 条船
// （天蛇 4 + 血袭者 4）。其余官员默认关闭，在页面上一键打开。
var DefaultEnabled = map[int64]bool{
	13661: true, 13654: true, 13667: true, 13659: true, // Serpentis
	13561: true, 13564: true, 13557: true, 13573: true, // Blood Raiders
}

// Faction 从船组名里抠出阵营：
//
//	"Asteroid Serpentis Officer"            → Serpentis
//	"Asteroid Blood Raiders Officer"        → Blood Raiders
//	"Asteroid Sansha's Nation Officer"      → Sansha's Nation
//	"Asteroid Rogue Drone Officer Frigate"  → Rogue Drone
func Faction(groupName string) string {
	s := strings.TrimSpace(groupName)
	s = strings.TrimPrefix(s, "Asteroid ")
	if i := strings.Index(s, " Officer"); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}

// withMeta 把人工维护的元数据（中文译名 / 吨位 / NPC 角色 ID）贴到 ESI 来的记录上。
// 查不到就留空——新官员会在图鉴里显示英文名，不会阻塞同步。
func withMeta(o model.Officer) model.Officer {
	if m, ok := MetaFor(o.TypeID); ok {
		o.Zh = m.Zh
		o.ShipClass = m.Class
		o.CharacterID = m.CharacterID
	}
	o.FactionZh = FactionZh(o.Faction)
	o.ShipClassZh = ClassZh(o.ShipClass)
	return o
}

// Syncer 负责把 ESI 的名字同步进 officers 表。
type Syncer struct {
	esi *esi.Client
	res *esi.Resolver
	st  *store.Store
}

// New 构造。
func New(c *esi.Client, res *esi.Resolver, st *store.Store) *Syncer {
	return &Syncer{esi: c, res: res, st: st}
}

// Sync 常规同步：拉已知船组 + 每个船型的名字，写入 officers。
// 返回新增条目数。已存在的官员不会被改 enabled 状态。
func (s *Syncer) Sync(ctx context.Context) (int, error) {
	seen := map[int64]bool{}
	out := []model.Officer{}

	for _, gid := range KnownGroups {
		g, err := s.esi.Group(ctx, gid)
		if err != nil {
			// 单个组失败不致命：下次同步再来
			continue
		}
		faction := Faction(g.Name)
		for _, tid := range g.Types {
			if seen[tid] {
				continue
			}
			seen[tid] = true
			name := s.res.TypeName(ctx, tid)
			out = append(out, withMeta(model.Officer{
				TypeID:    tid,
				Name:      name,
				GroupID:   gid,
				GroupName: g.Name,
				Faction:   faction,
				Enabled:   DefaultEnabled[tid],
				Source:    "esi",
			}))
		}
	}
	if len(out) == 0 {
		return 0, fmt.Errorf("名录同步失败：一个船组都没拿到（检查 ESI 可达性）")
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TypeID < out[j].TypeID })
	added, err := s.st.UpsertOfficers(out)
	if err != nil {
		return added, err
	}
	_ = s.st.SetMeta("catalog_synced_at", time.Now().UTC().Format(time.RFC3339))
	_ = s.st.SetMeta("catalog_count", fmt.Sprint(len(out)))
	return added, nil
}

// DeepScan 全量重扫 Entity 分类下所有组，找出名字含 Officer 的新船组。
// 415 个组 + 新船型的名字，大约 1~2 分钟；返回新发现的官员数。
func (s *Syncer) DeepScan(ctx context.Context) (int, error) {
	cat, err := s.esi.Category(ctx, 11)
	if err != nil {
		return 0, fmt.Errorf("取 Entity 分类失败: %w", err)
	}
	groups := make([]int64, 0, len(cat.Groups))
	groups = append(groups, cat.Groups...)

	found := map[int64]bool{}
	for _, gid := range KnownGroups {
		found[gid] = true
	}

	type groupInfo struct {
		id   int64
		name string
		typ  []int64
	}
	discovered := []groupInfo{}
	for _, gid := range groups {
		if found[gid] {
			continue
		}
		g, err := s.esi.Group(ctx, gid)
		if err != nil {
			continue
		}
		if !strings.Contains(strings.ToLower(g.Name), "officer") {
			continue
		}
		found[gid] = true
		discovered = append(discovered, groupInfo{id: gid, name: g.Name, typ: g.Types})
	}

	out := []model.Officer{}
	for _, g := range discovered {
		faction := Faction(g.name)
		for _, tid := range g.typ {
			out = append(out, withMeta(model.Officer{
				TypeID:    tid,
				Name:      s.res.TypeName(ctx, tid),
				GroupID:   g.id,
				GroupName: g.name,
				Faction:   faction,
				Enabled:   DefaultEnabled[tid],
				Source:    "esi-deep",
			}))
		}
	}
	if len(out) == 0 {
		return 0, nil
	}
	added, err := s.st.UpsertOfficers(out)
	return added, err
}

// Ensure 首次启动时把名录灌进去（officers 表为空才拉）。
func (s *Syncer) Ensure(ctx context.Context) error {
	list, err := s.st.ListOfficers()
	if err != nil {
		return err
	}
	if len(list) > 0 {
		return nil
	}
	_, err = s.Sync(ctx)
	return err
}
