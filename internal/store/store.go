// Package store 是存储层：SQLite（modernc，纯 Go，无 CGO）。
//
// 为什么不用那台机器上的共享 MySQL：本项目是**单实例、写少读少、要能整包搬走**的东西
// （本地跑一份、服务器跑一份，同一个二进制），SQLite 一个文件即全部状态，
// 备份 = 复制文件，迁移 = scp 文件。共享 MySQL 留给多服务共享的业务库。
package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"officerwatch/internal/model"
)

// Store 包一层 *sql.DB，顺便把 schema 版本号藏在 meta 表里。
type Store struct {
	db   *sql.DB
	path string
}

const schemaVersion = "1"

// Open 打开（必要时创建）数据库并建表。
func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("创建数据目录失败: %w", err)
	}
	dsn := "file:" + path + "?_pragma=busy_timeout(8000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// 写是串行的（一个采集协程 + 少量 HTTP 写），读多路并发；WAL 下这个组合最省心。
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(4)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, path: path}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close 关闭数据库。
func (s *Store) Close() error { return s.db.Close() }

// DB 暴露底层连接，给需要独立表的包用（目前只有 internal/locs 的星体索引）。
func (s *Store) DB() *sql.DB { return s.db }

// Path 返回数据库文件路径（状态页要显示大小）。
func (s *Store) Path() string { return s.path }

// Size 返回数据库文件大小（字节）。
func (s *Store) Size() int64 {
	fi, err := os.Stat(s.path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS meta (k TEXT PRIMARY KEY, v TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS officers (
			type_id       INTEGER PRIMARY KEY,
			name          TEXT NOT NULL,
			group_id      INTEGER NOT NULL DEFAULT 0,
			group_name    TEXT NOT NULL DEFAULT '',
			faction       TEXT NOT NULL DEFAULT '',
			enabled       INTEGER NOT NULL DEFAULT 0,
			source        TEXT NOT NULL DEFAULT 'esi',
			discovered_at TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS kills (
			killmail_id            INTEGER PRIMARY KEY,
			officer_type_id        INTEGER NOT NULL,
			role                   TEXT NOT NULL,
			killmail_time          TEXT NOT NULL,
			sequence_id            INTEGER NOT NULL DEFAULT 0,
			total_value            REAL NOT NULL DEFAULT 0,
			fitted_value           REAL NOT NULL DEFAULT 0,
			dropped_value          REAL NOT NULL DEFAULT 0,
			destroyed_value        REAL NOT NULL DEFAULT 0,
			points                 INTEGER NOT NULL DEFAULT 0,
			npc                    INTEGER NOT NULL DEFAULT 0,
			solo                   INTEGER NOT NULL DEFAULT 0,
			awox                   INTEGER NOT NULL DEFAULT 0,
			labels                 TEXT NOT NULL DEFAULT '',
			solar_system_id        INTEGER NOT NULL DEFAULT 0,
			system_name            TEXT NOT NULL DEFAULT '',
			region_id              INTEGER NOT NULL DEFAULT 0,
			region_name            TEXT NOT NULL DEFAULT '',
			victim_char_id         INTEGER NOT NULL DEFAULT 0,
			victim_corp_id         INTEGER NOT NULL DEFAULT 0,
			victim_alliance_id     INTEGER NOT NULL DEFAULT 0,
			victim_ship_type_id    INTEGER NOT NULL DEFAULT 0,
			victim_ship_name       TEXT NOT NULL DEFAULT '',
			attacker_count         INTEGER NOT NULL DEFAULT 0,
			final_blow_ship_type_id INTEGER NOT NULL DEFAULT 0,
			kill_hash              TEXT NOT NULL DEFAULT '',
			notify_status          TEXT NOT NULL DEFAULT 'pending',
			notify_error           TEXT NOT NULL DEFAULT '',
			notified_at            TEXT NOT NULL DEFAULT '',
			raw_json               TEXT NOT NULL DEFAULT '{}',
			created_at             TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_kills_time    ON kills(killmail_time DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_kills_officer ON kills(officer_type_id, killmail_time DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_kills_value   ON kills(total_value DESC)`,
		`CREATE INDEX IF NOT EXISTS idx_kills_status  ON kills(notify_status)`,
		`CREATE TABLE IF NOT EXISTS events (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			ts      TEXT NOT NULL,
			level   TEXT NOT NULL,
			kind    TEXT NOT NULL,
			message TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts DESC)`,
		`CREATE TABLE IF NOT EXISTS patrol_runs (
			id      INTEGER PRIMARY KEY AUTOINCREMENT,
			ts      TEXT NOT NULL,
			ok      INTEGER NOT NULL,
			summary TEXT NOT NULL,
			checks  TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_patrol_ts ON patrol_runs(ts DESC)`,
		`CREATE TABLE IF NOT EXISTS celestials (
			item_id   INTEGER PRIMARY KEY,
			system_id INTEGER NOT NULL,
			group_id  INTEGER NOT NULL,
			name      TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_celestials_system ON celestials(system_id)`,
		`CREATE TABLE IF NOT EXISTS name_cache (
			kind       TEXT NOT NULL,
			id         INTEGER NOT NULL,
			name       TEXT NOT NULL,
			extra      TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL,
			PRIMARY KEY (kind, id)
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("建表失败: %w (%s)", err, firstLine(q))
		}
	}
	// 增量列：SQLite 没有 IF NOT EXISTS 的 ADD COLUMN，靠"重复执行会报 duplicate
	// column"来幂等 —— 只忽略这一种错误。
	alters := [][3]string{
		{"kills", "victim_name", "TEXT NOT NULL DEFAULT ''"},
		{"kills", "victim_corp_name", "TEXT NOT NULL DEFAULT ''"},
		{"kills", "victim_alliance_name", "TEXT NOT NULL DEFAULT ''"},
		{"kills", "system_sec", "REAL NOT NULL DEFAULT 0"},
		{"kills", "location_id", "INTEGER NOT NULL DEFAULT 0"},
		{"kills", "location_name", "TEXT NOT NULL DEFAULT ''"},
		{"kills", "jumps", "INTEGER NOT NULL DEFAULT 0"},
		{"officers", "zh_name", "TEXT NOT NULL DEFAULT ''"},
		{"officers", "ship_class", "TEXT NOT NULL DEFAULT ''"},
		{"officers", "character_id", "INTEGER NOT NULL DEFAULT 0"},
	}
	for _, a := range alters {
		q := fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", a[0], a[1], a[2])
		if _, err := s.db.Exec(q); err != nil && !strings.Contains(err.Error(), "duplicate column") {
			return fmt.Errorf("加列失败 %s.%s: %w", a[0], a[1], err)
		}
	}
	return s.SetMeta("schema_version", schemaVersion)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i > 0 {
		return s[:i]
	}
	return s
}

// ─────────────────────────────── meta

// GetMeta 读一个元数据键，不存在返回 ""。
func (s *Store) GetMeta(k string) string {
	var v string
	if err := s.db.QueryRow(`SELECT v FROM meta WHERE k = ?`, k).Scan(&v); err != nil {
		return ""
	}
	return v
}

// SetMeta 写元数据。
func (s *Store) SetMeta(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO meta(k, v) VALUES(?, ?)
		ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}

// ─────────────────────────────── officers

// UpsertOfficers 写入/更新官员名录。
//
// 注意 enabled 的处理：老记录已经存在时**不覆盖**用户的选择（否则每次同步名录
// 都会把用户关掉的官员又打开一遍）。新记录按传入的 Enabled 落库。
func (s *Store) UpsertOfficers(list []model.Officer) (added int, err error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	now := time.Now().UTC().Format(time.RFC3339)
	for _, o := range list {
		var exists int
		if err := tx.QueryRow(`SELECT COUNT(1) FROM officers WHERE type_id = ?`, o.TypeID).Scan(&exists); err != nil {
			return added, err
		}
		if exists > 0 {
			// 名字/归属会变（CCP 加新船、改名），中文名与吨位跟着元数据走，每次同步都覆盖
			_, err = tx.Exec(`UPDATE officers SET name = ?, group_id = ?, group_name = ?, faction = ?,
				zh_name = ?, ship_class = ?, character_id = ?
				WHERE type_id = ?`, o.Name, o.GroupID, o.GroupName, o.Faction,
				o.Zh, o.ShipClass, o.CharacterID, o.TypeID)
		} else {
			en := 0
			if o.Enabled {
				en = 1
			}
			src := o.Source
			if src == "" {
				src = "esi"
			}
			_, err = tx.Exec(`INSERT INTO officers(type_id, name, group_id, group_name, faction, enabled, source,
				discovered_at, zh_name, ship_class, character_id)
				VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, o.TypeID, o.Name, o.GroupID, o.GroupName, o.Faction,
				en, src, now, o.Zh, o.ShipClass, o.CharacterID)
			added++
		}
		if err != nil {
			return added, err
		}
	}
	return added, tx.Commit()
}

// ListOfficers 返回名录 + 每个官员的实战统计（左连接 kills）。
func (s *Store) ListOfficers() ([]model.Officer, error) {
	rows, err := s.db.Query(`
		SELECT o.type_id, o.name, o.zh_name, o.ship_class, o.character_id,
		       o.group_id, o.group_name, o.faction, o.enabled, o.source, o.discovered_at,
		       COUNT(k.killmail_id) AS cnt,
		       COALESCE(SUM(k.total_value), 0) AS total,
		       COALESCE(MAX(k.total_value), 0) AS maxv,
		       COALESCE(MAX(k.killmail_time), '') AS last
		FROM officers o
		LEFT JOIN kills k ON k.officer_type_id = o.type_id
		GROUP BY o.type_id
		ORDER BY o.faction, o.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []model.Officer{}
	for rows.Next() {
		var o model.Officer
		var en int
		if err := rows.Scan(&o.TypeID, &o.Name, &o.Zh, &o.ShipClass, &o.CharacterID,
			&o.GroupID, &o.GroupName, &o.Faction, &en, &o.Source,
			&o.DiscoveredAt, &o.KillCount, &o.TotalValue, &o.MaxValue, &o.LastSeen); err != nil {
			return nil, err
		}
		o.Enabled = en == 1
		out = append(out, o)
	}
	return out, rows.Err()
}

// OfficerByType 查单个官员。
func (s *Store) OfficerByType(typeID int64) (model.Officer, bool, error) {
	var o model.Officer
	var en int
	err := s.db.QueryRow(`SELECT type_id, name, zh_name, ship_class, character_id,
		group_id, group_name, faction, enabled, source, discovered_at
		FROM officers WHERE type_id = ?`, typeID).
		Scan(&o.TypeID, &o.Name, &o.Zh, &o.ShipClass, &o.CharacterID,
			&o.GroupID, &o.GroupName, &o.Faction, &en, &o.Source, &o.DiscoveredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return o, false, nil
	}
	if err != nil {
		return o, false, err
	}
	o.Enabled = en == 1
	return o, true, nil
}

// SetOfficerEnabled 打开/关闭某个官员的监控。
func (s *Store) SetOfficerEnabled(typeID int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := s.db.Exec(`UPDATE officers SET enabled = ? WHERE type_id = ?`, v, typeID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// EnabledOfficerTypes 返回当前开着的官员船型（采集端用它做快速判定）。
func (s *Store) EnabledOfficerTypes() (map[int64]bool, error) {
	rows, err := s.db.Query(`SELECT type_id FROM officers WHERE enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ErrNotFound 通用"记录不存在"。
var ErrNotFound = errors.New("not found")

// ─────────────────────────────── kills

// Kill 是 kills 表的一行（也是 API 返回给前端的形状）。
type Kill struct {
	KillmailID         int64     `json:"killmailId"`
	OfficerTypeID      int64     `json:"officerTypeId"`
	OfficerName        string    `json:"officerName"`
	OfficerZh          string    `json:"officerZh"`
	OfficerClass       string    `json:"officerClass"` // battleship / cruiser / frigate
	Faction            string    `json:"faction"`
	Role               string    `json:"role"`
	KillmailTime       time.Time `json:"killmailTime"`
	TotalValue         float64   `json:"totalValue"`
	FittedValue        float64   `json:"fittedValue"`
	DroppedValue       float64   `json:"droppedValue"`
	DestroyedValue     float64   `json:"destroyedValue"`
	Points             int       `json:"points"`
	Npc                bool      `json:"npc"`
	Solo               bool      `json:"solo"`
	Awox               bool      `json:"awox"`
	Labels             []string  `json:"labels"`
	SystemID           int64     `json:"systemId"`
	SystemName         string    `json:"systemName"`
	RegionID           int64     `json:"regionId"`
	RegionName         string    `json:"regionName"`
	VictimCharID       int64     `json:"victimCharId"`
	VictimName         string    `json:"victimName"`
	VictimCorpName     string    `json:"victimCorpName"`
	VictimAllianceName string    `json:"victimAllianceName"`
	SystemSec          float64   `json:"systemSec"`
	LocationID         int64     `json:"locationId"`
	LocationName       string    `json:"locationName"`
	Jumps              int       `json:"jumps"`
	// OfficerKillRank / OfficerKillTotal：这条损失在该官员历史上的排名（仅详情接口填）
	OfficerKillRank  int       `json:"officerKillRank,omitempty"`
	OfficerKillTotal int       `json:"officerKillTotal,omitempty"`
	VictimCorpID     int64     `json:"victimCorpId"`
	VictimAlliance   int64     `json:"victimAllianceId"`
	VictimShipID     int64     `json:"victimShipTypeId"`
	VictimShipName   string    `json:"victimShipName"`
	AttackerCount    int       `json:"attackerCount"`
	FinalBlowShip    int64     `json:"finalBlowShipTypeId"`
	Hash             string    `json:"hash"`
	NotifyStatus     string    `json:"notifyStatus"`
	NotifyError      string    `json:"notifyError,omitempty"`
	NotifiedAt       string    `json:"notifiedAt,omitempty"`
	SeenAt           time.Time `json:"seenAt"`
	ZKBURL           string    `json:"zkbUrl"`
}

// InsertKill 插入一条击杀；已存在返回 false（不报错）。
//
// 幂等是刻意的：R2Z2 会重复推送同一条 killmail（重新解析/换 sequence），
// zkb 轮询也会重叠，主键冲突直接当"已处理"。
func (s *Store) InsertKill(k *Kill, raw string) (bool, error) {
	labels := strings.Join(k.Labels, ",")
	res, err := s.db.Exec(`INSERT INTO kills(
		killmail_id, officer_type_id, role, killmail_time, total_value, fitted_value, dropped_value, destroyed_value,
		points, npc, solo, awox, labels, solar_system_id, system_name, region_id, region_name,
		victim_char_id, victim_corp_id, victim_alliance_id, victim_ship_type_id, victim_ship_name,
		victim_name, victim_corp_name, victim_alliance_name, system_sec,
		location_id, location_name, jumps,
		attacker_count, final_blow_ship_type_id, kill_hash, notify_status, notify_error, notified_at,
		raw_json, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(killmail_id) DO NOTHING`,
		k.KillmailID, k.OfficerTypeID, k.Role, k.KillmailTime.UTC().Format(time.RFC3339), k.TotalValue,
		k.FittedValue, k.DroppedValue, k.DestroyedValue, k.Points, b2i(k.Npc), b2i(k.Solo), b2i(k.Awox),
		labels, k.SystemID, k.SystemName, k.RegionID, k.RegionName,
		k.VictimCharID, k.VictimCorpID, k.VictimAlliance, k.VictimShipID, k.VictimShipName,
		k.VictimName, k.VictimCorpName, k.VictimAllianceName, k.SystemSec,
		k.LocationID, k.LocationName, k.Jumps,
		k.AttackerCount, k.FinalBlowShip, k.Hash, k.NotifyStatus, k.NotifyError, k.NotifiedAt,
		raw, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// MarkNotify 记录通知结果。
func (s *Store) MarkNotify(killmailID int64, status, errMsg string) error {
	_, err := s.db.Exec(`UPDATE kills SET notify_status = ?, notify_error = ?, notified_at = ? WHERE killmail_id = ?`,
		status, errMsg, time.Now().UTC().Format(time.RFC3339), killmailID)
	return err
}

// KillFilter 列表查询条件。
type KillFilter struct {
	Limit    int
	Offset   int
	Officer  int64   // 0 = 不限
	Faction  string  // "" = 不限
	Role     string  // "" = 不限
	MinValue float64 // 0 = 不限
	MaxValue float64 // 0 = 不限
	Since    string  // RFC3339 或空
	Status   string  // "" = 不限
	SortBy   string  // value | time
	Query    string  // 搜系统名/受害者 ID
}

// ListKills 分页查询，返回行 + 总数。
func (s *Store) ListKills(f KillFilter) ([]Kill, int, error) {
	if f.Limit <= 0 || f.Limit > 500 {
		f.Limit = 50
	}
	where := []string{"1=1"}
	args := []any{}
	if f.Officer > 0 {
		where = append(where, "k.officer_type_id = ?")
		args = append(args, f.Officer)
	}
	if f.Faction != "" {
		where = append(where, "o.faction = ?")
		args = append(args, f.Faction)
	}
	if f.Role != "" {
		where = append(where, "k.role = ?")
		args = append(args, f.Role)
	}
	if f.MinValue > 0 {
		where = append(where, "k.total_value >= ?")
		args = append(args, f.MinValue)
	}
	if f.MaxValue > 0 {
		where = append(where, "k.total_value <= ?")
		args = append(args, f.MaxValue)
	}
	if f.Since != "" {
		where = append(where, "k.killmail_time >= ?")
		args = append(args, f.Since)
	}
	if f.Status != "" {
		where = append(where, "k.notify_status = ?")
		args = append(args, f.Status)
	}
	if f.Query != "" {
		where = append(where, "(k.system_name LIKE ? OR k.region_name LIKE ? OR CAST(k.victim_char_id AS TEXT) LIKE ? OR k.victim_ship_name LIKE ?)")
		q := "%" + f.Query + "%"
		args = append(args, q, q, q, q)
	}
	w := strings.Join(where, " AND ")

	var total int
	if err := s.db.QueryRow(`SELECT COUNT(1) FROM kills k JOIN officers o ON o.type_id = k.officer_type_id
		WHERE `+w, args...).Scan(&total); err != nil {
		return nil, 0, err
	}

	order := "k.killmail_time DESC"
	if f.SortBy == "value" {
		order = "k.total_value DESC"
	}
	q := `SELECT k.killmail_id, k.officer_type_id, COALESCE(o.name,''), COALESCE(o.zh_name,''), COALESCE(o.ship_class,''),
		COALESCE(o.faction,''), k.role,
		k.killmail_time, k.total_value, k.fitted_value, k.dropped_value, k.destroyed_value, k.points,
		k.npc, k.solo, k.awox, k.labels, k.solar_system_id, k.system_name, k.region_id, k.region_name,
		k.victim_char_id, k.victim_corp_id, k.victim_alliance_id, k.victim_ship_type_id, k.victim_ship_name,
		k.victim_name, k.victim_corp_name, k.victim_alliance_name, k.system_sec,
		k.location_id, k.location_name, k.jumps,
		k.attacker_count, k.final_blow_ship_type_id, k.kill_hash, k.notify_status, k.notify_error,
		k.notified_at, k.created_at
		FROM kills k JOIN officers o ON o.type_id = k.officer_type_id
		WHERE ` + w + ` ORDER BY ` + order + ` LIMIT ? OFFSET ?`
	args = append(args, f.Limit, f.Offset)

	rows, err := s.db.Query(q, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()

	out := []Kill{}
	for rows.Next() {
		k, err := scanKill(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *k)
	}
	return out, total, rows.Err()
}

type scannable interface{ Scan(...any) error }

func scanKill(r scannable) (*Kill, error) {
	var k Kill
	var npc, solo, awox int
	var labels, ts, created string
	if err := r.Scan(&k.KillmailID, &k.OfficerTypeID, &k.OfficerName, &k.OfficerZh, &k.OfficerClass, &k.Faction, &k.Role,
		&ts, &k.TotalValue, &k.FittedValue, &k.DroppedValue, &k.DestroyedValue, &k.Points,
		&npc, &solo, &awox, &labels, &k.SystemID, &k.SystemName, &k.RegionID, &k.RegionName,
		&k.VictimCharID, &k.VictimCorpID, &k.VictimAlliance, &k.VictimShipID, &k.VictimShipName,
		&k.VictimName, &k.VictimCorpName, &k.VictimAllianceName, &k.SystemSec,
		&k.LocationID, &k.LocationName, &k.Jumps,
		&k.AttackerCount, &k.FinalBlowShip, &k.Hash, &k.NotifyStatus, &k.NotifyError,
		&k.NotifiedAt, &created); err != nil {
		return nil, err
	}
	k.Npc, k.Solo, k.Awox = npc == 1, solo == 1, awox == 1
	if labels != "" {
		k.Labels = strings.Split(labels, ",")
	}
	k.KillmailTime, _ = time.Parse(time.RFC3339, ts)
	k.SeenAt, _ = time.Parse(time.RFC3339, created)
	k.ZKBURL = fmt.Sprintf("https://zkillboard.com/kill/%d/", k.KillmailID)
	return &k, nil
}

// GetKill 取单条（含 raw_json）。
func (s *Store) GetKill(killmailID int64) (*Kill, string, error) {
	q := `SELECT k.killmail_id, k.officer_type_id, COALESCE(o.name,''), COALESCE(o.zh_name,''), COALESCE(o.ship_class,''),
		COALESCE(o.faction,''), k.role,
		k.killmail_time, k.total_value, k.fitted_value, k.dropped_value, k.destroyed_value, k.points,
		k.npc, k.solo, k.awox, k.labels, k.solar_system_id, k.system_name, k.region_id, k.region_name,
		k.victim_char_id, k.victim_corp_id, k.victim_alliance_id, k.victim_ship_type_id, k.victim_ship_name,
		k.victim_name, k.victim_corp_name, k.victim_alliance_name, k.system_sec,
		k.location_id, k.location_name, k.jumps,
		k.attacker_count, k.final_blow_ship_type_id, k.kill_hash, k.notify_status, k.notify_error,
		k.notified_at, k.created_at, k.raw_json,
		(SELECT COUNT(1) FROM kills k2 WHERE k2.officer_type_id = k.officer_type_id AND k2.total_value > k.total_value) + 1,
		(SELECT COUNT(1) FROM kills k3 WHERE k3.officer_type_id = k.officer_type_id)
		FROM kills k JOIN officers o ON o.type_id = k.officer_type_id WHERE k.killmail_id = ?`
	var k Kill
	var npc, solo, awox int
	var labels, ts, created, raw string
	err := s.db.QueryRow(q, killmailID).Scan(&k.KillmailID, &k.OfficerTypeID, &k.OfficerName, &k.OfficerZh, &k.OfficerClass, &k.Faction, &k.Role,
		&ts, &k.TotalValue, &k.FittedValue, &k.DroppedValue, &k.DestroyedValue, &k.Points,
		&npc, &solo, &awox, &labels, &k.SystemID, &k.SystemName, &k.RegionID, &k.RegionName,
		&k.VictimCharID, &k.VictimCorpID, &k.VictimAlliance, &k.VictimShipID, &k.VictimShipName,
		&k.VictimName, &k.VictimCorpName, &k.VictimAllianceName, &k.SystemSec,
		&k.LocationID, &k.LocationName, &k.Jumps,
		&k.AttackerCount, &k.FinalBlowShip, &k.Hash, &k.NotifyStatus, &k.NotifyError,
		&k.NotifiedAt, &created, &raw, &k.OfficerKillRank, &k.OfficerKillTotal)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, "", ErrNotFound
	}
	if err != nil {
		return nil, "", err
	}
	k.Npc, k.Solo, k.Awox = npc == 1, solo == 1, awox == 1
	if labels != "" {
		k.Labels = strings.Split(labels, ",")
	}
	k.KillmailTime, _ = time.Parse(time.RFC3339, ts)
	k.SeenAt, _ = time.Parse(time.RFC3339, created)
	k.ZKBURL = fmt.Sprintf("https://zkillboard.com/kill/%d/", k.KillmailID)
	return &k, raw, nil
}

// ─────────────────────────────── 统计

// Overview 主页面 KPI。
type Overview struct {
	Today       int     `json:"today"`
	Last7d      int     `json:"last7d"`
	Total       int     `json:"total"`
	ValueToday  float64 `json:"valueToday"`
	Value7d     float64 `json:"value7d"`
	ValueTotal  float64 `json:"valueTotal"`
	Biggest     float64 `json:"biggest"`
	OfficersOn  int     `json:"officersOn"`
	OfficersAll int     `json:"officersAll"`
	LastKillAt  string  `json:"lastKillAt"`
	Factions    int     `json:"factions"`
}

// GetOverview 汇总。
func (s *Store) GetOverview() (Overview, error) {
	var o Overview
	now := time.Now().UTC()
	today := now.Format("2006-01-02")
	weekAgo := now.AddDate(0, 0, -7).Format(time.RFC3339)

	err := s.db.QueryRow(`SELECT
		COALESCE(SUM(CASE WHEN substr(killmail_time,1,10) = ? THEN 1 ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN killmail_time >= ? THEN 1 ELSE 0 END), 0),
		COUNT(1),
		COALESCE(SUM(CASE WHEN substr(killmail_time,1,10) = ? THEN total_value ELSE 0 END), 0),
		COALESCE(SUM(CASE WHEN killmail_time >= ? THEN total_value ELSE 0 END), 0),
		COALESCE(SUM(total_value), 0),
		COALESCE(MAX(total_value), 0),
		COALESCE(MAX(killmail_time), '')
		FROM kills`, today, weekAgo, today, weekAgo).
		Scan(&o.Today, &o.Last7d, &o.Total, &o.ValueToday, &o.Value7d, &o.ValueTotal, &o.Biggest, &o.LastKillAt)
	if err != nil {
		return o, err
	}
	if err := s.db.QueryRow(`SELECT COALESCE(SUM(enabled),0), COUNT(1), COUNT(DISTINCT faction) FROM officers`).
		Scan(&o.OfficersOn, &o.OfficersAll, &o.Factions); err != nil {
		return o, err
	}
	return o, nil
}

// DayStat 单日统计。
type DayStat struct {
	Date  string  `json:"date"`
	Kills int     `json:"kills"`
	Value float64 `json:"value"`
}

// Daily 最近 n 天每天一条（没有记录的日期补 0）。
func (s *Store) Daily(days int) ([]DayStat, error) {
	if days <= 0 || days > 365 {
		days = 30
	}
	since := time.Now().UTC().AddDate(0, 0, -days).Format("2006-01-02")
	rows, err := s.db.Query(`SELECT substr(killmail_time,1,10) AS d, COUNT(1), COALESCE(SUM(total_value),0)
		FROM kills WHERE substr(killmail_time,1,10) >= ? GROUP BY d ORDER BY d`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	got := map[string]DayStat{}
	for rows.Next() {
		var d DayStat
		if err := rows.Scan(&d.Date, &d.Kills, &d.Value); err != nil {
			return nil, err
		}
		got[d.Date] = d
	}
	out := []DayStat{}
	for i := days - 1; i >= 0; i-- {
		date := time.Now().UTC().AddDate(0, 0, -i).Format("2006-01-02")
		if d, ok := got[date]; ok {
			out = append(out, d)
		} else {
			out = append(out, DayStat{Date: date})
		}
	}
	return out, rows.Err()
}

// FactionStat 按阵营聚合。
type FactionStat struct {
	Faction string  `json:"faction"`
	Kills   int     `json:"kills"`
	Value   float64 `json:"value"`
}

// FactionStats 按阵营统计。
func (s *Store) FactionStats() ([]FactionStat, error) {
	rows, err := s.db.Query(`SELECT COALESCE(o.faction,''), COUNT(1), COALESCE(SUM(k.total_value),0)
		FROM kills k JOIN officers o ON o.type_id = k.officer_type_id
		GROUP BY o.faction ORDER BY 2 DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []FactionStat{}
	for rows.Next() {
		var f FactionStat
		if err := rows.Scan(&f.Faction, &f.Kills, &f.Value); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// EnrichKill 补全一条历史记录里缺的字段（老库迁移进来的行没有星系/座舰）。
// 只补空字段，不覆盖已有值 —— 重复调用是安全的。
func (s *Store) EnrichKill(killmailID int64, systemID int64, systemName, regionName string, systemSec float64,
	locationID int64, locationName string, jumps int,
	victimShipName, victimName, corpName, allianceName string) error {
	_, err := s.db.Exec(`UPDATE kills SET
		solar_system_id = CASE WHEN system_name = '' THEN ? ELSE solar_system_id END,
		system_name     = CASE WHEN system_name = '' THEN ? ELSE system_name END,
		region_name     = CASE WHEN region_name = '' THEN ? ELSE region_name END,
		system_sec      = CASE WHEN system_sec = 0 THEN ? ELSE system_sec END,
		victim_ship_name = CASE WHEN victim_ship_name = '' THEN ? ELSE victim_ship_name END,
		victim_name      = CASE WHEN victim_name = '' THEN ? ELSE victim_name END,
		victim_corp_name = CASE WHEN victim_corp_name = '' THEN ? ELSE victim_corp_name END,
		victim_alliance_name = CASE WHEN victim_alliance_name = '' THEN ? ELSE victim_alliance_name END,
		location_id   = CASE WHEN location_id = 0 THEN ? ELSE location_id END,
		location_name = CASE WHEN ? <> '' THEN ? ELSE location_name END,
		jumps         = CASE WHEN ? > 0 THEN ? ELSE jumps END
		WHERE killmail_id = ?`,
		systemID, systemName, regionName, systemSec, victimShipName, victimName, corpName, allianceName,
		locationID, locationName, locationName, jumps, jumps, killmailID)
	return err
}

// UpdateKillEconomy 补齐价值构成（老库只存了 total_value）。
func (s *Store) UpdateKillEconomy(killmailID int64, fitted, dropped, destroyed float64, points int, solo bool) error {
	_, err := s.db.Exec(`UPDATE kills SET fitted_value = ?, dropped_value = ?, destroyed_value = ?,
		points = ?, solo = ? WHERE killmail_id = ?`,
		fitted, dropped, destroyed, points, b2i(solo), killmailID)
	return err
}

// KillsNeedingEnrich 找出字段不全的历史记录（老库迁移 / 早期版本写入的行）。
func (s *Store) KillsNeedingEnrich() ([]int64, error) {
	rows, err := s.db.Query(`SELECT killmail_id FROM kills
		WHERE system_name = '' OR system_sec = 0 OR (victim_char_id > 0 AND victim_name = '')
		   OR (total_value > 0 AND dropped_value = 0 AND destroyed_value = 0)
		   OR location_id = 0
		   OR jumps = 0
		ORDER BY killmail_id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// CountNotifiedSince 统计某时刻之后"真的发出去"的条数（限流用）。
func (s *Store) CountNotifiedSince(t time.Time) (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(1) FROM kills WHERE notify_status = 'sent' AND notified_at >= ?`,
		t.UTC().Format(time.RFC3339)).Scan(&n)
	return n, err
}

// ─────────────────────────────── 巡检

// PatrolRun 一轮巡检的记录。
type PatrolRun struct {
	ID      int64     `json:"id"`
	TS      time.Time `json:"ts"`
	OK      bool      `json:"ok"`
	Summary string    `json:"summary"`
	Checks  string    `json:"checks"`
}

// SavePatrolRun 落一轮巡检结果，只保留最近 200 轮。
func (s *Store) SavePatrolRun(ts time.Time, ok bool, summary, checks string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO patrol_runs(ts, ok, summary, checks) VALUES(?,?,?,?)`,
		ts.UTC().Format(time.RFC3339), b2i(ok), summary, checks)
	if err != nil {
		return 0, err
	}
	_, _ = s.db.Exec(`DELETE FROM patrol_runs WHERE id <= (SELECT MAX(id) - 200 FROM patrol_runs)`)
	return res.LastInsertId()
}

// ListPatrolRuns 最近 n 轮。
func (s *Store) ListPatrolRuns(limit int) ([]PatrolRun, error) {
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	rows, err := s.db.Query(`SELECT id, ts, ok, summary, checks FROM patrol_runs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []PatrolRun{}
	for rows.Next() {
		var r PatrolRun
		var ok int
		var ts string
		if err := rows.Scan(&r.ID, &ts, &ok, &r.Summary, &r.Checks); err != nil {
			return nil, err
		}
		r.TS, _ = time.Parse(time.RFC3339, ts)
		r.OK = ok == 1
		out = append(out, r)
	}
	return out, rows.Err()
}

// ─────────────────────────────── events

// Event 一条运行事件（替代前身满屏的 log.Printf，方便前端"运行状态"页直接看）。
type Event struct {
	ID      int64  `json:"id"`
	TS      string `json:"ts"`
	Level   string `json:"level"`
	Kind    string `json:"kind"`
	Message string `json:"message"`
}

// AddEvent 记一条事件。
func (s *Store) AddEvent(level, kind, msg string) {
	_, _ = s.db.Exec(`INSERT INTO events(ts, level, kind, message) VALUES(?,?,?,?)`,
		time.Now().UTC().Format(time.RFC3339), level, kind, msg)
	// 只留最近 500 条，防止事件表无限长。
	_, _ = s.db.Exec(`DELETE FROM events WHERE id <= (SELECT MAX(id) - 500 FROM events)`)
}

// ListEvents 最近的 n 条事件。
func (s *Store) ListEvents(limit int) ([]Event, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id, ts, level, kind, message FROM events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		if err := rows.Scan(&e.ID, &e.TS, &e.Level, &e.Kind, &e.Message); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ─────────────────────────────── 名称缓存

// CacheName 缓存系统/星座/星域/船型名字，避免反复打 ESI。
func (s *Store) CacheName(kind string, id int64, name, extra string) {
	_, _ = s.db.Exec(`INSERT INTO name_cache(kind, id, name, extra, updated_at) VALUES(?,?,?,?,?)
		ON CONFLICT(kind, id) DO UPDATE SET name = excluded.name, extra = excluded.extra, updated_at = excluded.updated_at`,
		kind, id, name, extra, time.Now().UTC().Format(time.RFC3339))
}

// CachedName 读名字缓存；extra 用于系统→星域映射。
func (s *Store) CachedName(kind string, id int64) (name string, extra string, ok bool) {
	err := s.db.QueryRow(`SELECT name, extra FROM name_cache WHERE kind = ? AND id = ?`, kind, id).Scan(&name, &extra)
	return name, extra, err == nil
}

// ─────────────────────────────── settings

// Settings 是可以在页面上直接改的口径（安全相关的凭据不放这里，只在配置文件的 0600 文件里）。
type Settings struct {
	MinValue      float64  `json:"minValue"`
	Role          string   `json:"role"`     // both | npc_kill | officer_down
	Factions      []string `json:"factions"` // 空 = 全部
	QuietEnabled  bool     `json:"quietEnabled"`
	QuietStart    int      `json:"quietStart"` // 0-23
	QuietEnd      int      `json:"quietEnd"`
	MaxPerHour    int      `json:"maxPerHour"`
	AtAll         bool     `json:"atAll"`
	NotifyEnabled bool     `json:"notifyEnabled"`
	BackfillHours int      `json:"backfillHours"`
	// HomeSystem 是"抢官员"的参照点：填星系名（如 Jita），邮件里会写"距 Jita N 跳"。
	HomeSystem string `json:"homeSystem"`
	// Recipients 是邮件收件人，页面上可增删。空 = 用 config.json 里的 smtp.to。
	Recipients []string `json:"recipients"`
}

// DefaultSettings 与前身行为对齐的默认值：只报"被官员击杀"、不限价值、通知开启。
func DefaultSettings() Settings {
	return Settings{
		MinValue:      0,
		Role:          model.RoleOfficerKilledPlayer,
		Factions:      []string{},
		QuietEnabled:  false,
		QuietStart:    2,
		QuietEnd:      9,
		MaxPerHour:    30,
		AtAll:         true,
		NotifyEnabled: true,
		BackfillHours: 0,
		HomeSystem:    "",
	}
}

// LoadSettings 读设置，缺省值兜底。
func (s *Store) LoadSettings() Settings {
	st := DefaultSettings()
	raw := s.GetMeta("settings")
	if raw == "" {
		return st
	}
	_ = json.Unmarshal([]byte(raw), &st)
	return st
}

// SaveSettings 写设置。
func (s *Store) SaveSettings(st Settings) error {
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	return s.SetMeta("settings", string(b))
}
