// Package legacy 把前身 EVE-RedisQ 的 killmails.db 迁进来。
//
// 老库只有一张 officer_killmails，15 条真实记录（2025-12 ~ 2026-02）。
// 迁移不是考古癖：新站的"累计/最大价值/历史"要有连续性，否则第一天上线的统计页是空的。
package legacy

import (
	"database/sql"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"officerwatch/internal/store"
)

// Row 老库里的一行。
type Row struct {
	KillID       int64
	KillmailTime string
	TotalValue   float64
	VictimCharID int64
	VictimShipID int64
	OfficerType  int64
	ImportedAt   string
	SentEmail    bool
	Details      string
}

// Read 读老库。文件不存在/表不存在都返回空切片（不算错误，方便幂等重跑）。
func Read(path string) ([]Row, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, err
	}
	defer db.Close()

	rows, err := db.Query(`SELECT kill_id, killmail_time, total_value,
		COALESCE(victim_character_id,0), COALESCE(victim_ship_type_id,0),
		COALESCE(attacker_ship_type_id,0), COALESCE(imported_at,''), COALESCE(sent_email,0), COALESCE(details,'')
		FROM officer_killmails ORDER BY kill_id`)
	if err != nil {
		return nil, fmt.Errorf("读取旧库失败（可能是文件不是 officer_killmails 结构）: %w", err)
	}
	defer rows.Close()

	out := []Row{}
	for rows.Next() {
		var r Row
		var sent int
		if err := rows.Scan(&r.KillID, &r.KillmailTime, &r.TotalValue, &r.VictimCharID,
			&r.VictimShipID, &r.OfficerType, &r.ImportedAt, &sent, &r.Details); err != nil {
			return nil, err
		}
		r.SentEmail = sent == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// Import 把老记录写进新库。返回新增条数。
func Import(st *store.Store, rows []Row) (int, error) {
	added := 0
	for _, r := range rows {
		t, err := time.Parse(time.RFC3339, r.KillmailTime)
		if err != nil {
			t = time.Now().UTC()
		}
		status := "legacy"
		if r.SentEmail {
			status = "sent"
		}
		k := &store.Kill{
			KillmailID:     r.KillID,
			OfficerTypeID:  r.OfficerType,
			Role:           "npc_kill",
			KillmailTime:   t,
			TotalValue:     r.TotalValue,
			VictimCharID:   r.VictimCharID,
			VictimShipID:   r.VictimShipID,
			VictimShipName: "",
			AttackerCount:  0,
			FinalBlowShip:  r.OfficerType,
			NotifyStatus:   status,
			NotifiedAt:     r.ImportedAt,
		}
		ins, err := st.InsertKill(k, `{"legacy":true}`)
		if err != nil {
			return added, err
		}
		if ins {
			added++
		}
	}
	return added, nil
}
