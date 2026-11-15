package storage

import (
	"database/sql"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func nowUnix() int64 { return time.Now().Unix() }

type rowScanner interface {
	Scan(dest ...any) error
}

// UpsertChat 插入或更新频道信息，返回内部主键。
// 插入时按 c.Enabled 决定是否采集；已存在的频道不覆盖 enabled（保留手动开关）。
func (d *DB) UpsertChat(c *Chat) (int64, error) {
	enabled := 0
	if c.Enabled {
		enabled = 1
	}
	_, err := d.db.Exec(`
INSERT INTO chats (tg_chat_id, access_hash, name, username, type, enabled, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(tg_chat_id) DO UPDATE SET
    access_hash = excluded.access_hash,
    name        = excluded.name,
    username    = excluded.username,
    type        = excluded.type`,
		c.TgChatID, c.AccessHash, c.Name, c.Username, c.Type, enabled, nowUnix())
	if err != nil {
		return 0, err
	}
	got, err := d.GetChatByTgID(c.TgChatID)
	if err != nil {
		return 0, err
	}
	return got.ID, nil
}

// GetChatByTgID 按 Telegram 频道 ID 查询
func (d *DB) GetChatByTgID(tgID int64) (*Chat, error) {
	row := d.db.QueryRow(`
SELECT id, tg_chat_id, access_hash, name, username, type, enabled, last_offset_id, last_top_id, synced_at, created_at
FROM chats WHERE tg_chat_id = ?`, tgID)
	return scanChat(row)
}

// GetChat 按内部主键查询
func (d *DB) GetChat(id int64) (*Chat, error) {
	row := d.db.QueryRow(`
SELECT id, tg_chat_id, access_hash, name, username, type, enabled, last_offset_id, last_top_id, synced_at, created_at
FROM chats WHERE id = ?`, id)
	return scanChat(row)
}

func scanChat(r rowScanner) (*Chat, error) {
	var c Chat
	var enabled int
	if err := r.Scan(&c.ID, &c.TgChatID, &c.AccessHash, &c.Name, &c.Username, &c.Type,
		&enabled, &c.LastOffset, &c.LastTopID, &c.SyncedAt, &c.CreatedAt); err != nil {
		return nil, err
	}
	c.Enabled = enabled == 1
	return &c, nil
}

// ListChats 全部频道
func (d *DB) ListChats() ([]*Chat, error) {
	rows, err := d.db.Query(`
SELECT id, tg_chat_id, access_hash, name, username, type, enabled, last_offset_id, last_top_id, synced_at, created_at
FROM chats ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// ChatStat 频道管理面板用的频道信息：带原始消息数与来源数
type ChatStat struct {
	ID           int64  `json:"id"`
	TgChatID     int64  `json:"tg_chat_id"`
	Name         string `json:"name"`
	Username     string `json:"username"`
	Type         string `json:"type"`
	Enabled      bool   `json:"enabled"`
	MessageCount int    `json:"message_count"`
	SourceCount  int    `json:"source_count"`
	LastOffset   int64  `json:"last_offset"`
	LastTopID    int64  `json:"last_top_id"`
	SyncedAt     int64  `json:"synced_at"`
	Syncing      bool   `json:"syncing"` // 仅接口返回时填充：是否正在后台同步
}

// ListChatStats 全部频道 + 每个频道已入库的消息数/来源数，按「已启用优先、来源多的优先」排序
func (d *DB) ListChatStats() ([]ChatStat, error) {
	rows, err := d.db.Query(`
SELECT c.id, c.tg_chat_id, COALESCE(c.name, ''), COALESCE(c.username, ''), COALESCE(c.type, ''),
       c.enabled, c.last_offset_id, c.last_top_id, c.synced_at,
       (SELECT COUNT(*) FROM sources s WHERE s.chat_id = c.id)
     + (SELECT COUNT(*) FROM show_photos p WHERE p.chat_id = c.id),
       (SELECT COUNT(*) FROM sources s WHERE s.chat_id = c.id)
FROM chats c
ORDER BY c.enabled DESC,
         (SELECT COUNT(*) FROM sources s WHERE s.chat_id = c.id) DESC,
         c.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatStat
	for rows.Next() {
		var c ChatStat
		var enabled int
		if err := rows.Scan(&c.ID, &c.TgChatID, &c.Name, &c.Username, &c.Type, &enabled,
			&c.LastOffset, &c.LastTopID, &c.SyncedAt, &c.MessageCount, &c.SourceCount); err != nil {
			return nil, err
		}
		c.Enabled = enabled == 1
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListEnabledChats 已启用的频道
func (d *DB) ListEnabledChats() ([]*Chat, error) {
	rows, err := d.db.Query(`
SELECT id, tg_chat_id, access_hash, name, username, type, enabled, last_offset_id, last_top_id, synced_at, created_at
FROM chats WHERE enabled = 1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Chat
	for rows.Next() {
		c, err := scanChat(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

// SetChatEnabled 启用/停用频道采集
func (d *DB) SetChatEnabled(tgID int64, enabled bool) error {
	v := 0
	if enabled {
		v = 1
	}
	res, err := d.db.Exec(`UPDATE chats SET enabled = ? WHERE tg_chat_id = ?`, v, tgID)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return errors.New("频道不存在")
	}
	return nil
}

// UpdateChatSyncPoint 更新历史同步断点
func (d *DB) UpdateChatSyncPoint(tgID int64, lastOffset int64) error {
	_, err := d.db.Exec(`UPDATE chats SET last_offset_id = ?, synced_at = ? WHERE tg_chat_id = ?`,
		lastOffset, nowUnix(), tgID)
	return err
}

// UpdateChatTopMsg 记录频道已同步到的最新消息 ID，只增不减
func (d *DB) UpdateChatTopMsg(tgID int64, topID int64) error {
	if topID <= 0 {
		return nil
	}
	_, err := d.db.Exec(`UPDATE chats SET last_top_id = ? WHERE tg_chat_id = ? AND last_top_id < ?`,
		topID, tgID, topID)
	return err
}

// PurgeStats 清理一个频道后的删除统计，接口直接回给前端展示
type PurgeStats struct {
	Sources  int `json:"sources"`
	Episodes int `json:"episodes"`
	Photos   int `json:"photos"`
	Shows    int `json:"shows"`
	Files    int `json:"files"`
}

// PurgeChatData 删除某个频道已入库的全部数据：视频来源、预览图、因此失去全部
// 来源的剧集，以及失去全部剧集和预览图的剧，顺带清掉对应的本地缓存文件。
// 频道行本身保留（enabled 由调用方改），面板里还能看到并重新启用。
// 同一部剧/同一集可能被多个频道转发共用：只要还有别的来源就不删。
func (d *DB) PurgeChatData(tgID int64) (PurgeStats, error) {
	var st PurgeStats
	var chatPK int64
	err := d.db.QueryRow(`SELECT id FROM chats WHERE tg_chat_id = ?`, tgID).Scan(&chatPK)
	if errors.Is(err, sql.ErrNoRows) {
		return st, errors.New("频道不存在")
	} else if err != nil {
		return st, err
	}
	tx, err := d.db.Begin()
	if err != nil {
		return st, err
	}
	defer tx.Rollback()

	// 先收集要删的本地文件：视频与预览图，提交后再落盘删除，避免事务回滚后文件没了
	var files []string
	for _, q := range []string{
		`SELECT file_path FROM sources WHERE chat_id = ? AND file_path <> ''`,
		`SELECT cache_path FROM show_photos WHERE chat_id = ? AND cache_path <> ''`,
	} {
		rows, err := tx.Query(q, chatPK)
		if err != nil {
			return st, err
		}
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil && p != "" {
				files = append(files, p)
			}
		}
		rows.Close()
	}

	var epsBefore, showsBefore int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM episodes`).Scan(&epsBefore); err != nil {
		return st, err
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM shows`).Scan(&showsBefore); err != nil {
		return st, err
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM sources WHERE chat_id = ?`, chatPK).Scan(&st.Sources); err != nil {
		return st, err
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM show_photos WHERE chat_id = ?`, chatPK).Scan(&st.Photos); err != nil {
		return st, err
	}

	if _, err := tx.Exec(`DELETE FROM sources WHERE chat_id = ?`, chatPK); err != nil {
		return st, err
	}
	if _, err := tx.Exec(`DELETE FROM show_photos WHERE chat_id = ?`, chatPK); err != nil {
		return st, err
	}
	if _, err := tx.Exec(`DELETE FROM pending_msgs WHERE chat_id = ?`, chatPK); err != nil {
		return st, err
	}
	if _, err := tx.Exec(`DELETE FROM episodes WHERE NOT EXISTS (
SELECT 1 FROM sources s WHERE s.episode_id = episodes.id)`); err != nil {
		return st, err
	}

	// 没有剧集也没有预览图的剧整部删掉；show_tags 没有级联，必须先删
	coverRows, err := tx.Query(`SELECT id, cover_path FROM shows WHERE NOT EXISTS (
SELECT 1 FROM episodes e WHERE e.show_id = shows.id)
AND NOT EXISTS (SELECT 1 FROM show_photos p WHERE p.show_id = shows.id)`)
	if err != nil {
		return st, err
	}
	var deadShows []int64
	for coverRows.Next() {
		var id int64
		var cover string
		if err := coverRows.Scan(&id, &cover); err != nil {
			continue
		}
		deadShows = append(deadShows, id)
		if cover != "" {
			files = append(files, cover)
		}
	}
	coverRows.Close()
	for _, id := range deadShows {
		if _, err := tx.Exec(`DELETE FROM show_tags WHERE show_id = ?`, id); err != nil {
			return st, err
		}
		if _, err := tx.Exec(`DELETE FROM shows WHERE id = ?`, id); err != nil {
			return st, err
		}
	}

	var epsAfter, showsAfter int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM episodes`).Scan(&epsAfter); err != nil {
		return st, err
	}
	if err := tx.QueryRow(`SELECT COUNT(*) FROM shows`).Scan(&showsAfter); err != nil {
		return st, err
	}
	st.Episodes = epsBefore - epsAfter
	st.Shows = showsBefore - showsAfter

	if err := tx.Commit(); err != nil {
		return st, err
	}

	for _, p := range files {
		if d.removeCachedFile(p) {
			st.Files++
		}
	}
	return st, nil
}

// removeCachedFile 只删数据库目录下的缓存文件：调试用的副本库可能带着指向
// 正式库的绝对路径，直接 os.Remove 会把正式库的封面/预览图删掉。
func (d *DB) removeCachedFile(p string) bool {
	base := d.baseDir + string(os.PathSeparator)
	for _, cand := range []string{p, filepath.Join(d.baseDir, p)} {
		abs, err := filepath.Abs(cand)
		if err != nil || !strings.HasPrefix(abs, base) {
			continue
		}
		if err := os.Remove(abs); err == nil {
			return true
		}
	}
	return false
}

// Vacuum 回收已删除行占用的磁盘空间。SQLite 删行只把页标记为空闲，库文件不会
// 变小；停用/退出频道清掉大量来源后调一次，才能真正省出空间。
// 有同步在写时可能拿不到锁，失败忽略即可（下次重启会再试）。
func (d *DB) Vacuum() {
	if _, err := d.db.Exec(`VACUUM`); err != nil {
		log.Printf("VACUUM 跳过：%v", err)
	}
}

// DeleteChat 频道已不在账号的对话列表里：删掉它的入库数据和频道行本身。
func (d *DB) DeleteChat(tgID int64) (PurgeStats, error) {
	st, err := d.PurgeChatData(tgID)
	if err != nil {
		return st, err
	}
	_, err = d.db.Exec(`DELETE FROM chats WHERE tg_chat_id = ?`, tgID)
	return st, err
}

// GetGroupCaption 返回同一相册（grouped_id）中任一非空 caption。
// 相册 caption 只挂在一条消息上，其余成员入库时靠它补齐分集信息。
// 文案现在存在来源行／预览图行上，两张表都查一遍。
func (d *DB) GetGroupCaption(chatPK, groupedID int64) (string, error) {
	var text string
	err := d.db.QueryRow(`
SELECT caption FROM sources
WHERE chat_id = ? AND grouped_id = ? AND caption <> ''
ORDER BY message_id LIMIT 1`, chatPK, groupedID).Scan(&text)
	if err == nil {
		return text, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	err = d.db.QueryRow(`
SELECT caption FROM show_photos
WHERE chat_id = ? AND grouped_id = ? AND caption <> ''
ORDER BY message_id LIMIT 1`, chatPK, groupedID).Scan(&text)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return text, err
}

// GetKV 读取键值，不存在返回空串和 false
func (d *DB) GetKV(k string) (string, bool, error) {
	var v string
	err := d.db.QueryRow(`SELECT v FROM kv WHERE k = ?`, k).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return v, true, nil
}

// SetKV 写入键值
func (d *DB) SetKV(k, v string) error {
	_, err := d.db.Exec(`
INSERT INTO kv (k, v) VALUES (?, ?)
ON CONFLICT(k) DO UPDATE SET v = excluded.v`, k, v)
	return err
}
