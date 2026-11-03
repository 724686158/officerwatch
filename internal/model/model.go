// Package model 定义外部数据源（zKillboard R2Z2 / zkb API / ESI）与内部存储共用的结构体。
//
// 历史包袱说明：这个项目的前身（EVE-RedisQ）把 ESI 的 JSON 直接塞进匿名 struct，
// 字段散落在三个文件里各写一遍。这里统一成 Killmail，所有来源都先转成它再入库。
package model

import "time"

// Payload 是一条 killmail 的完整来源包，等价于 R2Z2 的 <sequence>.json。
type Payload struct {
	KillmailID     int64    `json:"killmail_id"`
	Hash           string   `json:"hash"`
	SequenceID     int64    `json:"sequence_id,omitempty"`
	UploadedAt     int64    `json:"uploaded_at,omitempty"`
	SequenceUpdate int64    `json:"sequence_updated,omitempty"`
	ESI            Killmail `json:"esi"`
	ZKB            ZKB      `json:"zkb"`
}

// Killmail ESI killmail 结构（只保留我们真正用得上的字段）。
type Killmail struct {
	KillmailID    int64      `json:"killmail_id"`
	KillmailTime  string     `json:"killmail_time"`
	SolarSystemID int64      `json:"solar_system_id"`
	Attackers     []Attacker `json:"attackers"`
	Victim        Victim     `json:"victim"`
}

// Attacker NPC 攻击者没有 character_id —— 这正是"官员"的识别依据（NPC 不会是人类）。
type Attacker struct {
	CharacterID   int64 `json:"character_id"`
	CorporationID int64 `json:"corporation_id"`
	AllianceID    int64 `json:"alliance_id"`
	ShipTypeID    int64 `json:"ship_type_id"`
	DamageDone    int64 `json:"damage_done"`
	FinalBlow     bool  `json:"final_blow"`
	WeaponTypeID  int64 `json:"weapon_type_id"`
}

// Victim 被击杀方。NPC 受害者同样没有 character_id。
type Victim struct {
	CharacterID   int64 `json:"character_id"`
	CorporationID int64 `json:"corporation_id"`
	AllianceID    int64 `json:"alliance_id"`
	ShipTypeID    int64 `json:"ship_type_id"`
	DamageTaken   int64 `json:"damage_taken"`
}

// ZKB zKillboard 的附加元数据。
type ZKB struct {
	LocationID     int64    `json:"locationID"`
	Hash           string   `json:"hash"`
	FittedValue    float64  `json:"fittedValue"`
	DroppedValue   float64  `json:"droppedValue"`
	DestroyedValue float64  `json:"destroyedValue"`
	TotalValue     float64  `json:"totalValue"`
	Points         int      `json:"points"`
	Npc            bool     `json:"npc"`
	Solo           bool     `json:"solo"`
	Awox           bool     `json:"awox"`
	Labels         []string `json:"labels"`
	Href           string   `json:"href"`
}

// ParsedTime 把 ESI 的时间字符串转成 time.Time；解析失败时返回零值，调用方自行降级。
func (k *Killmail) ParsedTime() time.Time {
	t, err := time.Parse(time.RFC3339, k.KillmailTime)
	if err != nil {
		return time.Time{}
	}
	return t
}

// Role 描述"官员在这条击杀里扮演什么角色"。
//
//	RoleOfficerKilledPlayer 官员（NPC）打死了玩家 —— 前身的语义，也是主页面的默认口径
//	RoleOfficerDown         玩家打死了官员 —— 官员掉了官员装备，更值钱，也值得报
const (
	RoleOfficerKilledPlayer = "npc_kill"
	RoleOfficerDown         = "officer_down"
)

// Match 是一条 killmail 与某个官员的匹配结果。
type Match struct {
	OfficerTypeID int64
	Role          string
	// AttackerShipTypeID 保留原始的攻击者船型（官员击杀时 = OfficerTypeID）
	AttackerShipTypeID int64
	FinalBlowByOfficer bool
}

// Officer 官员名录条目。名录来自 ESI 的 Entity 分类里名字含 "Officer" 的船组，
// 是权威数据（和游戏客户端同源），不需要手工维护 ID 表。
type Officer struct {
	TypeID       int64  `json:"typeId"`
	Name         string `json:"name"`
	Faction      string `json:"faction"`
	GroupID      int64  `json:"groupId"`
	GroupName    string `json:"groupName"`
	Enabled      bool   `json:"enabled"`
	Source       string `json:"source"` // seed / esi
	DiscoveredAt string `json:"discoveredAt,omitempty"`

	// 统计字段（查询时 join 出来，不入 officers 表）
	KillCount  int64   `json:"killCount"`
	LastSeen   string  `json:"lastSeen,omitempty"`
	TotalValue float64 `json:"totalValue"`
	MaxValue   float64 `json:"maxValue"`
}
