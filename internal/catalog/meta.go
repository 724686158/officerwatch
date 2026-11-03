// Package catalog —— 官员元数据（中文译名 / 吨位 / NPC 角色 ID）。
//
// 分工：
//   - 船型、阵营、船组名：运行时从 ESI 同步（权威、会变）
//   - 中文译名、吨位、NPC 角色 ID：本仓库维护（data/officers.json，变化极慢）
//
// 为什么中文译名要自己维护：CCP 的 SDE 只有英文名，官方中文客户端没有公开
// 这一层对照表。这里的译名统一用**音译**，不冒充官方译名。
//
// 为什么不拿角色 ID 当主键：killmail 里 NPC 攻击者只有 ship_type_id，
// 没有 character_id（NPC 不是"角色"）。角色 ID 只用于展示和查资料。
package catalog

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"sync"
)

//go:embed data/officers.json
var metaJSON []byte

// Meta 一位官员的人工元数据。
type Meta struct {
	TypeID      int64  `json:"typeId"`
	Name        string `json:"name"`
	Zh          string `json:"zh"`
	Faction     string `json:"faction"`
	FactionZh   string `json:"factionZh"`
	Class       string `json:"class"`   // battleship / cruiser / frigate
	ClassZh     string `json:"classZh"` // 战列 / 巡洋 / 护卫
	CharacterID int64  `json:"characterId"`
	Group       string `json:"group"`
}

type metaFile struct {
	Factions map[string]string `json:"factions"`
	Classes  map[string]string `json:"classes"`
	Officers []Meta            `json:"officers"`
}

var (
	metaOnce   sync.Once
	metaByType map[int64]Meta
	factionZh  map[string]string
	classZh    map[string]string
	metaErr    error
)

func loadMeta() {
	metaOnce.Do(func() {
		var f metaFile
		if err := json.Unmarshal(metaJSON, &f); err != nil {
			metaErr = fmt.Errorf("解析 officers.json 失败: %w", err)
			return
		}
		metaByType = make(map[int64]Meta, len(f.Officers))
		for _, m := range f.Officers {
			metaByType[m.TypeID] = m
		}
		factionZh = f.Factions
		classZh = f.Classes
	})
}

// MetaFor 查一位官员的元数据；ESI 新出现、我们还没维护译名的返回 false。
func MetaFor(typeID int64) (Meta, bool) {
	loadMeta()
	m, ok := metaByType[typeID]
	return m, ok
}

// FactionZh 阵营中文名（"Serpentis" → "天蛇"）。
func FactionZh(faction string) string {
	loadMeta()
	if s, ok := factionZh[faction]; ok {
		return s
	}
	return faction
}

// ClassZh 吨位中文名（"battleship" → "战列"）。
func ClassZh(class string) string {
	loadMeta()
	if s, ok := classZh[class]; ok {
		return s
	}
	return class
}

// Count 已维护的官员数（自检 / 测试用）。
func Count() int {
	loadMeta()
	return len(metaByType)
}

// MetaError 元数据文件解析错误（启动时自检用）。
func MetaError() error {
	loadMeta()
	return metaErr
}
