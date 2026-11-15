package storage

import (
	"database/sql"
	"fmt"
	"log"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB 媒体库数据访问层
type DB struct {
	db *sql.DB
	// baseDir 是数据库所在目录（data/）。清理频道数据时只删这个目录下的缓存文件，
	// 防止副本库（调试用）里的路径指向正式库的文件被误删。
	baseDir string
}

// Open 打开数据库并完成建表与 PRAGMA 调优
func Open(path string) (*DB, error) {
	// PRAGMA 调优：
	//   synchronous=NORMAL  WAL 下无需每次 fsync，写入快一个数量级且不会丢库
	//   cache_size=-8000    page cache 提到约 8MB，列表/聚合查询少读盘
	//   temp_store=MEMORY   排序/临时表走内存（标签聚合、去重常用）
	//   mmap_size=128MB     只读页走 mmap，启动与首屏查询更快
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)"+
		"&_pragma=foreign_keys(ON)&_pragma=synchronous(NORMAL)&_pragma=cache_size(-8000)"+
		"&_pragma=temp_store(MEMORY)&_pragma=mmap_size(134217728)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("打开数据库失败: %w", err)
	}
	// SQLite 单文件写入需串行，连接数收敛避免锁竞争
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

// Close 关闭数据库
func (d *DB) Close() error { return d.db.Close() }

// VersionTag 返回媒体库内容的轻量版本指纹（剧数/集数/最大更新时间/收藏数/启用频道指纹），
// 供 /api/shows、/api/tags 计算 ETag：内容没变时直接回 304。
// 刻意不含观看历史——播放中每 10 秒上报一次进度，不能因此让整页列表缓存失效。
// 启用频道必须算进来：关掉频道后该频道的剧会从列表消失，但剧/集行数不变，
// 少了这一项前端会一直命中 304 看到旧列表。用「条数 + id 之和」而不是只记条数：
// 关一个又开一个时条数不变，只比条数会漏掉这次变化。
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

// HistoryTag 观看历史的版本指纹，只有 /api/bootstrap（含「继续观看」）需要。
func (d *DB) HistoryTag() string {
	var n, maxUpd int64
	_ = d.db.QueryRow(`SELECT COUNT(*), COALESCE(MAX(updated_at), 0) FROM history`).Scan(&n, &maxUpd)
	return fmt.Sprintf("h%d-%d", n, maxUpd)
}

// execQuerier 抽象 *sql.DB 与 *sql.Tx：同一套 SQL 既可在自动提交下执行，
// 也可放进事务里批量执行（重建媒体库时把上万条 UPSERT 合并成一次提交）。
type execQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func (d *DB) migrate() error {
	if _, err := d.db.Exec(schemaSQL); err != nil {
		return err
	}
	// episodes 的 UNIQUE (show_id, episode_no) 已自带 sqlite_autoindex，覆盖同样的查询；
	// 旧版本额外建的这两个索引只会拖慢重建写入，统一清掉（老库升级时执行）。
	_, _ = d.db.Exec(`DROP INDEX IF EXISTS idx_episodes_show`)
	_, _ = d.db.Exec(`DROP INDEX IF EXISTS idx_episodes_show_no`)
	// 旧库补列（已存在时忽略报错）
	_, _ = d.db.Exec(`ALTER TABLE sources ADD COLUMN priority INTEGER NOT NULL DEFAULT 0`)
	_, _ = d.db.Exec(`ALTER TABLE chats ADD COLUMN last_top_id INTEGER NOT NULL DEFAULT 0`)
	// 非短剧条目（泛分类词/拉群话术建的假剧）标记位：升级后由 RecomputeHiddenShows 填充
	_, _ = d.db.Exec(`ALTER TABLE shows ADD COLUMN hidden INTEGER NOT NULL DEFAULT 0`)
	// 收藏的「追剧基线」：老库没有这一列，补上后按当前最新集初始化一次，
	// 否则收藏时早已存在的集会全被当成更新推给用户。
	_, _ = d.db.Exec(`ALTER TABLE favorites ADD COLUMN base_ep INTEGER NOT NULL DEFAULT 0`)
	// 原始文案改为挂在来源／预览图上（见 dropRawMessages）：只有「一条消息对应
	// 一个视频或一张图」时才有值，不再为此单独维护一张与 sources 等长的表。
	_, _ = d.db.Exec(`ALTER TABLE sources ADD COLUMN caption TEXT NOT NULL DEFAULT ''`)
	_, _ = d.db.Exec(`ALTER TABLE show_photos ADD COLUMN grouped_id INTEGER NOT NULL DEFAULT 0`)
	_, _ = d.db.Exec(`ALTER TABLE show_photos ADD COLUMN caption TEXT NOT NULL DEFAULT ''`)
	// 清晰度：variants_checked 表示这条来源已经向 Telegram 查过一次 alt_documents，
	// 之后不再重复请求（历史库靠播放页首次打开时补一次）。
	_, _ = d.db.Exec(`ALTER TABLE sources ADD COLUMN variants_checked INTEGER NOT NULL DEFAULT 0`)
	// 副文档的编码（h264/h265/av1）：浏览器对 HEVC 支持不稳定，同清晰度优先给 h264。
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
	// 老库没有 last_top_id，用已入库的最大消息 ID 初始化，避免升级后把整个频道重拉一遍
	if v, ok, _ := d.GetKV("chat_top_backfilled"); !ok || v != "1" {
		if _, err := d.db.Exec(`UPDATE chats SET last_top_id = COALESCE(
	(SELECT MAX(message_id) FROM sources src WHERE src.chat_id = chats.id), 0)`); err == nil {
			_ = d.SetKV("chat_top_backfilled", "1")
		}
	}
	if err := d.dropRawMessages(); err != nil {
		return err
	}
	// 同一条视频（chat_id + message_id）只应有一条来源；早期补全会把已经入库的
	// 消息再插一次（segment_index 递增），详情页选集里同一集会出现两份，这里清理。
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

// dropRawMessages 把 raw_messages 里唯一不可替代的 caption 并入 sources /
// show_photos，然后把这张表删掉并回收空间。
//
// 这张表与 sources 是 1:1 的（同一条视频消息一行），其中 info_json 存的文档参数
// （doc_id / access_hash / file_reference / 体积 / mime / 时长 / 分辨率）sources
// 本来就有，属于整列纯冗余；真正只有它才有的就是 caption 文案。把文案挪到来源行
// 之后单库可从 27MB 降到 13MB 左右。
//
// 代价：rebuild 不能再「复活」当初因缺 caption 被丢弃的消息——那些消息本来也
// 没有可用文案，且这次合并是一次性的，不影响后续增量入库。
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
	// DROP 只把页挂回 freelist，文件不会变小，必须 VACUUM 重写一次
	if _, err := d.db.Exec(`VACUUM`); err != nil {
		return err
	}
	log.Printf("raw_messages 已合并并删除，数据库空间已回收")
	return nil
}

// hasTable 判断表是否存在（用于老库的一次性迁移）
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
