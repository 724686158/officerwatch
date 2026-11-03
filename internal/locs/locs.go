// Package locs 把 EVE 的"星体名"做成本地索引。
//
// 背景（2026-11 使用者强调）：这套东西是用来**抢官员**的 —— 官员把玩家打死后，
// 死亡点就是官员所在的星体（某条小行星带、某个异常、某个空间站/星门）。
// 差一条带，就是白跑一趟。所以"死亡位置"必须精确到星体，而不是只给星系。
//
// 难点：zkillboard 只给一个 locationID；ESI 的 /universe/names 解不了星体 ID
// （实测返回 "Ensure all IDs are valid"），开源库里也没有便宜的在线查询。
// 于是把 SDE 的 mapDenormalize 裁成 (itemID, 星系ID, 组ID, 名字) 四列打包进二进制
// （约 2.4MB gzip，45 万行），首次启动灌进 SQLite，之后是纯本地主键查询。
package locs

import (
	"bufio"
	"bytes"
	"compress/gzip"
	"database/sql"
	_ "embed"
	"fmt"
	"strings"
)

//go:embed data/celestials.tsv.gz
var packed []byte

// Loc 一个星体。
type Loc struct {
	ID       int64  `json:"id"`
	SystemID int64  `json:"systemId"`
	GroupID  int64  `json:"groupId"`
	Name     string `json:"name"`
}

// Kind 返回中文类型名（邮件里用）。
func (l Loc) Kind() string {
	switch l.GroupID {
	case 9:
		return "小行星带"
	case 995:
		return "异常"
	case 15:
		return "空间站"
	case 10:
		return "星门"
	case 7:
		return "行星"
	case 8:
		return "卫星"
	default:
		return ""
	}
}

// Short 把 "H-S80W 9 - Asteroid Belt 3" 化简成 "9 - Asteroid Belt 3"（邮件里不必重复星系名）。
func (l Loc) Short(systemName string) string {
	s := l.Name
	if systemName != "" {
		prefix := systemName + " "
		if strings.HasPrefix(s, prefix) {
			s = strings.TrimPrefix(s, prefix)
		}
	}
	// 空间站名通常是 "系统 9 - 卫星 3 - 某某仓库"，同样去掉前缀
	return s
}

// Ensure 首次运行时把打包的星体表灌进 SQLite（一次约 3~5 秒），之后直接查表。
//
// 用 SQLite 而不是内存 map：45 万条驻留内存要几十 MB，而这台机器只有 3.8G，
// 还要给 MySQL / 1Panel 留位置；主键查询本身是微秒级。
func Ensure(db *sql.DB) (int, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(1) FROM celestials`).Scan(&n); err != nil {
		return 0, err
	}
	if n > 0 {
		return n, nil
	}
	gz, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return 0, fmt.Errorf("解压星体表失败: %w", err)
	}
	defer gz.Close()

	tx, err := db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT OR REPLACE INTO celestials(item_id, system_id, group_id, name) VALUES(?,?,?,?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	sc := bufio.NewScanner(gz)
	sc.Buffer(make([]byte, 0, 256*1024), 2*1024*1024)
	count := 0
	for sc.Scan() {
		parts := strings.SplitN(sc.Text(), "\t", 4)
		if len(parts) != 4 {
			continue
		}
		var id, sys, grp int64
		fmt.Sscan(parts[0], &id)
		fmt.Sscan(parts[1], &sys)
		fmt.Sscan(parts[2], &grp)
		if id == 0 || sys == 0 {
			continue
		}
		if _, err := stmt.Exec(id, sys, grp, parts[3]); err != nil {
			return count, err
		}
		count++
	}
	if err := sc.Err(); err != nil {
		return count, err
	}
	if err := tx.Commit(); err != nil {
		return count, err
	}
	return count, nil
}

// Lookup 查一个星体。
func Lookup(db *sql.DB, id int64) (Loc, bool) {
	if id == 0 {
		return Loc{}, false
	}
	var l Loc
	err := db.QueryRow(`SELECT item_id, system_id, group_id, name FROM celestials WHERE item_id = ?`, id).
		Scan(&l.ID, &l.SystemID, &l.GroupID, &l.Name)
	if err != nil {
		return Loc{}, false
	}
	return l, true
}
