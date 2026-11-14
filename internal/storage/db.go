package storage

import (
	"database/sql"
	"fmt"
	"log"
	"path/filepath"

	_ "modernc.org/sqlite"
)

type DB struct {
	db *sql.DB

	baseDir string
}

func Open(path string) (*DB, error) {

	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"+
		"&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-8000)"+
		"&_pragma=temp_store(MEMORY)&_pragma=mmap_size(134217728)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}

	sqlDB.SetMaxOpenConns(1)
	baseDir := filepath.Dir(path)
	if abs, aerr := filepath.Abs(baseDir); aerr == nil {
		baseDir = abs
	}
	d := &DB{db: sqlDB, baseDir: baseDir}
	if err := d.migrate(); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *DB) Close() error { return d.db.Close() }

func (d *DB) VersionTag() string {
	var shows, eps, maxUpd, favs, liveChats, liveSum, hidden int64
	_ = d.db.QueryRow(`SELECT
	(SELECT COUNT(*) FROM shows),
	(SELECT COUNT(*) FROM episodes),
	(SELECT COALESCE(MAX(updated_at), 0) FROM shows),
	(SELECT COUNT(*) FROM favorites),
	(SELECT COUNT(*) FROM chats WHERE enabled = 1),
	(SELECT COALESCE(SUM(id), 0) FROM chats WHERE enabled = 1),
	(SELECT COUNT(*) FROM shows WHERE hidden = 1)`).Scan(&shows, &eps, &maxUpd, &favs, &liveChats, &liveSum, &hidden)
	return fmt.Sprintf("v%d-%d-%d-%d-c%d-%d-x%d", shows, eps, maxUpd, favs, liveChats, liveSum, hidden)
}

func (d *DB) HistoryTag() string {
	var n, maxUpd int64
	_ = d.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(updated_at), 0) FROM history`).Scan(&n, &maxUpd)
	return fmt.Sprintf("h%d-%d", n, maxUpd)
}

type execQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func (d *DB) migrate() error {
	if _, err := d.db.Exec(schemaSQL); err != nil {
		return err
	}

	_, _ = d.db.Exec(`DROP INDEX IF EXISTS idx_episodes_show`)
	_, _ = d.db.Exec(`DROP INDEX IF EXISTS idx_episodes_show_no`)

	_, _ = d.db.Exec(`ALTER TABLE sources ADD COLUMN priority INTEGER NOT NULL DEFAULT 0`)
	_, _ = d.db.Exec(`ALTER TABLE chats ADD COLUMN last_top_id INTEGER NOT NULL DEFAULT 0`)

	_, _ = d.db.Exec(`ALTER TABLE shows ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0`)

	_, _ = d.db.Exec(`ALTER TABLE favorites ADD COLUMN base_ep INTEGER NOT NULL DEFAULT 0`)

	_, _ = d.db.Exec(`ALTER TABLE sources ADD COLUMN caption TEXT NOT NULL DEFAULT ''`)
	_, _ = d.db.Exec(`ALTER TABLE show_photos ADD COLUMN grouped_id INTEGER NOT NULL DEFAULT 0`)
	_, _ = d.db.Exec(`ALTER TABLE show_photos ADD COLUMN caption TEXT NOT NULL DEFAULT ''`)

	_, _ = d.db.Exec(`ALTER TABLE sources ADD COLUMN variants_checked INTEGER NOT NULL DEFAULT 0`)

	_, _ = d.db.Exec(`ALTER TABLE source_variants ADD COLUMN codec TEXT NOT NULL DEFAULT ''`)
	if v, ok, _ := d.GetKV("fav_base_ep_v1"); !ok || v != "1" {
		if _, err := d.db.Exec(`UPDATE favorites SET base_ep = COALESCE((
	SELECT MAX(e.episode_no) FROM episodes e
	WHERE e.show_id = favorites.show_id AND e.episode_no >= 1
	  AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = e.id)), 0)
WHERE base_ep = 0`); err == nil {
			_ = d.SetKV("fav_base_ep_v1", "1")
		}
	}

	if v, ok, _ := d.GetKV("chat_top_backfilled"); !ok || v != "1" {
		if _, err := d.db.Exec(`UPDATE chats SET last_top_id = COALESCE(
	(SELECT MAX(message_id) FROM sources src WHERE src.chat_id = chats.id), 0)`); err == nil {
			_ = d.SetKV("chat_top_backfilled", "1")
		}
	}
	if err := d.dropRawMessages(); err != nil {
		return err
	}

	if v, ok, _ := d.GetKV("src_dedup_v1"); !ok || v != "1" {
		if _, err := d.db.Exec(`DELETE FROM sources
	WHERE id NOT IN (SELECT MIN(id) FROM sources GROUP BY chat_id, message_id)
	  AND EXISTS (SELECT 1 FROM sources s2
	              WHERE s2.chat_id = sources.chat_id
	                AND s2.message_id = sources.message_id
	                AND s2.episode_id = sources.episode_id)`); err == nil {
			_ = d.SetKV("src_dedup_v1", "1")
		}
	}
	return nil
}

func (d *DB) dropRawMessages() error {
	if !d.hasTable("raw_messages") {
		return nil
	}
	log.Printf("正在把 raw_messages 的文案并入来源表…")
	if _, err := d.db.Exec(`
UPDATE sources SET caption = COALESCE((
    SELECT rm.text FROM raw_messages rm
    WHERE rm.chat_id = sources.chat_id AND rm.message_id = sources.message_id
      AND rm.text <> '' LIMIT 1), '')
WHERE caption = ''`); err != nil {
		return err
	}
	if _, err := d.db.Exec(`
UPDATE show_photos SET
    grouped_id = COALESCE((SELECT rm.grouped_id FROM raw_messages rm
        WHERE rm.chat_id = show_photos.chat_id AND rm.message_id = show_photos.message_id), 0),
    caption = COALESCE((SELECT rm.text FROM raw_messages rm
        WHERE rm.chat_id = show_photos.chat_id AND rm.message_id = show_photos.message_id
          AND rm.text <> ''), '')
WHERE grouped_id = 0 OR caption = ''`); err != nil {
		return err
	}
	if _, err := d.db.Exec(`DROP TABLE raw_messages`); err != nil {
		return err
	}

	if _, err := d.db.Exec(`VACUUM`); err != nil {
		return err
	}
	log.Printf("raw_messages 已合并并删除，数据库空间已回收")
	return nil
}

func (d *DB) hasTable(name string) bool {
	var n int
	_ = d.db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&n)
	return n > 0
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS chats (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    tg_chat_id     INTEGER NOT NULL UNIQUE,
    access_hash    INTEGER NOT NULL DEFAULT 0,
    name           TEXT NOT NULL,
    username       TEXT NOT NULL DEFAULT '',
    type           TEXT NOT NULL DEFAULT '',
    enabled        INTEGER NOT NULL DEFAULT 1,
    last_offset_id INTEGER NOT NULL DEFAULT 0,
    last_top_id    INTEGER NOT NULL DEFAULT 0,
    synced_at      INTEGER NOT NULL DEFAULT 0,
    created_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS shows (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    name        TEXT NOT NULL,
    norm_name   TEXT NOT NULL UNIQUE,
    cover_path  TEXT NOT NULL DEFAULT '',
    intro       TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL DEFAULT '连载中',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    hidden      INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS episodes (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    show_id     INTEGER NOT NULL,
    episode_no  INTEGER NOT NULL,
    title       TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    updated_at  INTEGER NOT NULL,
    UNIQUE (show_id, episode_no),
    FOREIGN KEY (show_id) REFERENCES shows (id)
);

CREATE TABLE IF NOT EXISTS sources (
    id              INTEGER PRIMARY KEY AUTOINCREMENT,
    episode_id      INTEGER NOT NULL,
    chat_id         INTEGER NOT NULL,
    message_id      INTEGER NOT NULL,
    grouped_id      INTEGER NOT NULL DEFAULT 0,
    segment_index   INTEGER NOT NULL DEFAULT 0,
    file_name       TEXT NOT NULL DEFAULT '',
    file_path       TEXT NOT NULL DEFAULT '',
    file_size       INTEGER NOT NULL DEFAULT 0,
    duration        REAL NOT NULL DEFAULT 0,
    width           INTEGER NOT NULL DEFAULT 0,
    height          INTEGER NOT NULL DEFAULT 0,
    mime            TEXT NOT NULL DEFAULT '',
    doc_id          INTEGER NOT NULL DEFAULT 0,
    access_hash     INTEGER NOT NULL DEFAULT 0,
    file_reference  BLOB,
    download_status TEXT NOT NULL DEFAULT 'pending',
    priority        INTEGER NOT NULL DEFAULT 0,
    caption         TEXT NOT NULL DEFAULT '',
    error           TEXT NOT NULL DEFAULT '',
    variants_checked INTEGER NOT NULL DEFAULT 0,
    created_at      INTEGER NOT NULL,
    UNIQUE (chat_id, message_id, segment_index),
    FOREIGN KEY (episode_id) REFERENCES episodes (id),
    FOREIGN KEY (chat_id) REFERENCES chats (id)
);

CREATE TABLE IF NOT EXISTS tags (
    id   INTEGER PRIMARY KEY AUTOINCREMENT,
    name TEXT NOT NULL UNIQUE
);

CREATE TABLE IF NOT EXISTS show_tags (
    show_id INTEGER NOT NULL,
    tag_id  INTEGER NOT NULL,
    PRIMARY KEY (show_id, tag_id),
    FOREIGN KEY (show_id) REFERENCES shows (id),
    FOREIGN KEY (tag_id) REFERENCES tags (id)
);

CREATE TABLE IF NOT EXISTS show_photos (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    show_id        INTEGER NOT NULL,
    chat_id        INTEGER NOT NULL,
    message_id     INTEGER NOT NULL,
    doc_id         INTEGER NOT NULL,
    access_hash    INTEGER NOT NULL DEFAULT 0,
    file_reference BLOB,
    width          INTEGER NOT NULL DEFAULT 0,
    height         INTEGER NOT NULL DEFAULT 0,
    cache_path     TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    grouped_id     INTEGER NOT NULL DEFAULT 0,
    caption        TEXT NOT NULL DEFAULT '',
    UNIQUE (doc_id),
    FOREIGN KEY (show_id) REFERENCES shows (id)
);

CREATE TABLE IF NOT EXISTS kv (
    k TEXT PRIMARY KEY,
    v TEXT NOT NULL DEFAULT ''
);

-- 待归属的消息索引：解析时因「空 caption + 哈希文件名」被丢弃的视频。
-- 相册 caption 可能挂在同组另一条消息上、且那条更晚才入库，先把定位参数留在这里，
-- 等同组 caption 到位后再补成正式来源。只存这类极少数条目（实测千分之一），
-- 替代原先与 sources 等长的 raw_messages 表。
CREATE TABLE IF NOT EXISTS pending_msgs (
    chat_id     INTEGER NOT NULL,
    message_id  INTEGER NOT NULL,
    grouped_id  INTEGER NOT NULL DEFAULT 0,
    caption     TEXT NOT NULL DEFAULT '',
    info_json   TEXT NOT NULL DEFAULT '',
    created_at  INTEGER NOT NULL,
    PRIMARY KEY (chat_id, message_id)
);

-- 观看历史：一集一行，只存进度，剧ID通过 episodes 关联（合并短剧时不会失效）
CREATE TABLE IF NOT EXISTS history (
    episode_id INTEGER PRIMARY KEY,
    position   REAL NOT NULL DEFAULT 0,
    duration   REAL NOT NULL DEFAULT 0,
    finished   INTEGER NOT NULL DEFAULT 0,
    updated_at INTEGER NOT NULL,
    FOREIGN KEY (episode_id) REFERENCES episodes (id) ON DELETE CASCADE
);

-- 收藏：一部剧一行
-- base_ep 是收藏时的最新集号（追剧基线）：只有用户已经看到这一集，之后新增的集才算「更新」
CREATE TABLE IF NOT EXISTS favorites (
    show_id    INTEGER PRIMARY KEY,
    created_at INTEGER NOT NULL,
    base_ep    INTEGER NOT NULL DEFAULT 0,
    FOREIGN KEY (show_id) REFERENCES shows (id) ON DELETE CASCADE
);

-- 视频的其它清晰度版本（Telegram messageMediaDocument.alt_documents）。
-- 每个变体是独立文档，有自己的 doc_id/access_hash/file_reference/size；
-- 主文档始终在 sources 行上，本表只存「副版本」，没有副版本时整表无记录。
CREATE TABLE IF NOT EXISTS source_variants (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    source_id      INTEGER NOT NULL,
    doc_id         INTEGER NOT NULL UNIQUE,
    access_hash    INTEGER NOT NULL DEFAULT 0,
    file_reference BLOB,
    file_size      INTEGER NOT NULL DEFAULT 0,
    duration       REAL NOT NULL DEFAULT 0,
    width          INTEGER NOT NULL DEFAULT 0,
    height         INTEGER NOT NULL DEFAULT 0,
    mime           TEXT NOT NULL DEFAULT '',
    codec          TEXT NOT NULL DEFAULT '',
    created_at     INTEGER NOT NULL,
    FOREIGN KEY (source_id) REFERENCES sources (id)
);

CREATE INDEX IF NOT EXISTS idx_source_variants_source ON source_variants (source_id);
CREATE INDEX IF NOT EXISTS idx_sources_episode  ON sources (episode_id);
CREATE INDEX IF NOT EXISTS idx_sources_status   ON sources (download_status);
CREATE INDEX IF NOT EXISTS idx_shows_updated    ON shows (updated_at);
CREATE INDEX IF NOT EXISTS idx_history_updated  ON history (updated_at);
CREATE INDEX IF NOT EXISTS idx_favorites_created ON favorites (created_at);
-- 按标签筛选/标签计数：show_tags 的主键是 (show_id, tag_id)，按 tag_id 反查需要独立索引
CREATE INDEX IF NOT EXISTS idx_show_tags_tag     ON show_tags (tag_id);
-- 详情页取预览图（WHERE show_id = ? GROUP BY chat_id）原来没有索引，
-- 每次打开详情都要全扫 show_photos（实测 21k 行 / 20ms 上下，详情页会扫两遍）。
-- (show_id, chat_id) 正好覆盖「取主频道」那个子查询，是覆盖索引。
CREATE INDEX IF NOT EXISTS idx_show_photos_show  ON show_photos (show_id, chat_id);
`
