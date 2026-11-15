package storage

import (
	"database/sql"
	"errors"
	"sort"
	"strings"

	"tgdrama/internal/parser"
)

// UpsertShow 按归一化剧名查找或新建短剧
func (d *DB) UpsertShow(name, normName string) (int64, bool, error) {
	return upsertShow(d.db, name, normName)
}

func upsertShow(q execQuerier, name, normName string) (id int64, inserted bool, err error) {
	var existing int64
	err = q.QueryRow(`SELECT id FROM shows WHERE norm_name = ?`, normName).Scan(&existing)
	switch {
	case err == nil:
		_, err = q.Exec(`UPDATE shows SET name = ?, updated_at = ? WHERE id = ?`,
			name, nowUnix(), existing)
		return existing, false, err
	case errors.Is(err, sql.ErrNoRows):
		res, ierr := q.Exec(`
INSERT INTO shows (name, norm_name, status, created_at, updated_at)
VALUES (?, ?, '连载中', ?, ?)`, name, normName, nowUnix(), nowUnix())
		if ierr != nil {
			return 0, false, ierr
		}
		id, ierr = res.LastInsertId()
		return id, true, ierr
	default:
		return 0, false, err
	}
}

// FindShowByNorm 按归一化剧名查已有短剧的展示名（#标签归属用）
// GroupSource 相册内一条已入库来源的归属信息（用于多标题预览帖重分配）
type GroupSource struct {
	ID        int64
	MessageID int
	EpisodeID int64
}

// ListGroupSources 按消息顺序列出某相册（grouped_id）已入库的来源。
func (d *DB) ListGroupSources(chatID, groupedID int64) ([]GroupSource, error) {
	rows, err := d.db.Query(`
SELECT id, message_id, episode_id FROM sources
WHERE chat_id = ? AND grouped_id = ? ORDER BY message_id, id`,
		chatID, groupedID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupSource
	for rows.Next() {
		var g GroupSource
		if err := rows.Scan(&g.ID, &g.MessageID, &g.EpisodeID); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// MoveSource 把一条来源改挂到另一集（多标题预览帖按顺序重新分配时使用）。
func (d *DB) MoveSource(srcID, episodeID int64) error {
	_, err := d.db.Exec(`UPDATE sources SET episode_id = ?, segment_index = 0 WHERE id = ?`,
		episodeID, srcID)
	return err
}

func (d *DB) FindShowByNorm(norm string) (name string, ok bool, err error) {
	err = d.db.QueryRow(`SELECT name FROM shows WHERE norm_name = ?`, norm).Scan(&name)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return name, true, nil
}

// GetShow 按主键查询短剧（含标签与集数）
func (d *DB) GetShow(id int64) (*Show, error) {
	row := d.db.QueryRow(showSelectSQL+searchShowWhere+` AND s.id = ?`, id)
	s, err := scanShow(row)
	if err != nil {
		return nil, err
	}
	if err := d.hydrateTags(s); err != nil {
		return nil, err
	}
	return s, nil
}

// ShowDocAnchor 一条来源消息的定位（sources.chat_id 是 chats 的主键）
type ShowDocAnchor struct {
	ChatPK    int64
	MessageID int
}

// GetShowDocAnchor 取该短剧任一来源消息的定位，用于 file_reference 过期后重新拉消息
func (d *DB) GetShowDocAnchor(showID int64) (ShowDocAnchor, bool, error) {
	var a ShowDocAnchor
	err := d.db.QueryRow(`
SELECT s.chat_id, s.message_id FROM sources s
JOIN episodes e ON e.id = s.episode_id
WHERE e.show_id = ?
ORDER BY s.id LIMIT 1`, showID).Scan(&a.ChatPK, &a.MessageID)
	if errors.Is(err, sql.ErrNoRows) {
		return a, false, nil
	}
	if err != nil {
		return a, false, err
	}
	return a, true, nil
}

// 集数统计口径（修复虚高/漏算的核心）：
//
//	ep_count  = 有视频来源的 distinct 集数（真正能播放的集数）
//	ep_latest = 有视频来源的最大集号（已更新到第几集，也是总集数的最佳估计）
//
// 历史实现直接 COUNT(*) episodes：合并/重建留下的空集、相册连续编号多建的集
// 都会被算进去，导致「集数虚高」；而按集号去重则避免同一集多条来源被重复计数。
//
// 所有「有来源」判定都走 liveSrcCond：只认启用中的频道。关闭频道后其来源仍留在
// 库里（重新开启立刻恢复），但不再计入剧库的列表、详情、标签、搜索与收藏。
// 各子查询作用域独立，因此统一用 src 作别名。
const liveSrcCond = `
          AND src.chat_id IN (SELECT lc.id FROM chats lc WHERE lc.enabled = 1)`

const showBaseColumns = `
       s.id, s.name, s.norm_name, s.cover_path, s.intro, s.status,
       s.created_at, s.updated_at,
       (SELECT COUNT(DISTINCT e.episode_no) FROM episodes e
        WHERE e.show_id = s.id AND e.episode_no >= 1
          AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = e.id` + liveSrcCond + `)) AS ep_count,
       (SELECT COALESCE(MAX(e.episode_no), 0) FROM episodes e
        WHERE e.show_id = s.id AND e.episode_no >= 1
          AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = e.id` + liveSrcCond + `)) AS ep_latest`

const showSelectSQL = `SELECT ` + showBaseColumns + `
FROM shows s`

// playableCond 只保留「至少有一集可播」的短剧（集号 >=1 且有启用频道的视频来源）。
// 纯广告/合集空壳/只剩 0 集残留的条目不再出现在列表、搜索、收藏与标签计数里。
const playableCond = `
  EXISTS (
    SELECT 1 FROM episodes e2
    WHERE e2.show_id = s.id AND e2.episode_no >= 1
      AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = e2.id` + liveSrcCond + `)
)`

// playableShowWhere 列表用的条件：可播且未隐藏。
// hidden = 1 的是被判定为「不是短剧」的条目（泛分类词/拉群话术建的假剧/无法识别的单集），
// 数据保留但不再出现在列表，规则调整后重新评估即可恢复。
const playableShowWhere = `
WHERE s.hidden = 0 AND ` + playableCond

// searchShowWhere 搜索与详情用的条件：只要可播就行，不看 hidden——
// 「无法识别的单集」按需求只是从列表里摘掉，搜索仍要能找到并打开详情。
const searchShowWhere = `
WHERE ` + playableCond

// singleNoCoverCond 隐藏「只有 1 集可播且没有封面」的零散抓取（日期帖、粉丝福利、
// 双扩展名残留）：这类条目没有「第几集」的追剧结构，也没有封面可看。
// 注意只对「无封面」生效：有封面的单集条目（正常短剧只发了一集）仍然展示。
// 只作用于 ListShows / CountShows / CountShowsByTag / ListTags，搜索/详情/收藏不受影响。
const singleNoCoverCond = `
  AND NOT (s.cover_path = '' AND 1 = (SELECT COUNT(DISTINCT e3.episode_no) FROM episodes e3
       WHERE e3.show_id = s.id AND e3.episode_no >= 1
         AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = e3.id` + liveSrcCond + `)))`

// listShowWhere 列表/标签计数使用的过滤条件：可播 + 隐藏「单集且无封面」。
const listShowWhere = playableShowWhere + singleNoCoverCond

func scanShow(r rowScanner) (*Show, error) {
	var s Show
	if err := r.Scan(&s.ID, &s.Name, &s.NormName, &s.CoverPath, &s.Intro, &s.Status,
		&s.CreatedAt, &s.UpdatedAt, &s.EpCount, &s.EpLatest); err != nil {
		return nil, err
	}
	s.Tags = []string{}
	return &s, nil
}

func (d *DB) hydrateTags(s *Show) error {
	rows, err := d.db.Query(`
SELECT t.name FROM tags t
JOIN show_tags st ON st.tag_id = t.id
WHERE st.show_id = ? ORDER BY t.name`, s.ID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return err
		}
		s.Tags = append(s.Tags, name)
	}
	return nil
}

// showOrderBy 列表排序：默认「自然」按最近更新；sortBy == "episodes" 按可播集数倒序。
// 集数相同时仍按更新时间，保证分页稳定（ep_count 是 showBaseColumns 里的别名）。
func showOrderBy(sortBy string) string {
	if sortBy == "episodes" {
		return ` ORDER BY ep_count DESC, s.updated_at DESC`
	}
	return ` ORDER BY s.updated_at DESC`
}

// ListShows 分页列出短剧，可按标签过滤，sortBy 控制排序（"" 自然 / "episodes" 集数倒序）
func (d *DB) ListShows(tag, sortBy string, limit, offset int) ([]*Show, error) {
	var rows *sql.Rows
	var err error
	if tag == "" {
		rows, err = d.db.Query(showSelectSQL+listShowWhere+showOrderBy(sortBy)+`
 LIMIT ? OFFSET ?`, limit, offset)
	} else {
		rows, err = d.db.Query(showSelectSQL+`
JOIN show_tags st ON st.show_id = s.id
JOIN tags t ON t.id = st.tag_id`+listShowWhere+`
  AND t.name = ?`+showOrderBy(sortBy)+`
 LIMIT ? OFFSET ?`, tag, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return d.collectShows(rows)
}

// SearchShows 本地数据库实时搜索，不访问 Telegram
func (d *DB) SearchShows(keyword string, limit int) ([]*Show, error) {
	rows, err := d.db.Query(showSelectSQL+searchShowWhere+`
  AND s.name LIKE ?
ORDER BY s.updated_at DESC LIMIT ?`, "%"+keyword+"%", limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return d.collectShows(rows)
}

// collectShows 收集短剧列表并批量填充标签。
// 注意：必须先读完并关闭外层结果集，再执行标签查询——
// 连接池只有 1 个连接，嵌套查询会互相等锁导致死锁。
// 标签改为一条 IN 查询批量取回（原来每部剧一次，列表 30 部就是 31 次查询）。
func (d *DB) collectShows(rows *sql.Rows) ([]*Show, error) {
	defer rows.Close()
	var out []*Show
	for rows.Next() {
		s, err := scanShow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return d.finishShows(out)
}

// finishShows 批量填充标签（调用前外层结果集必须已读完，连接池只有 1 个连接）。
func (d *DB) finishShows(out []*Show) ([]*Show, error) {
	if len(out) == 0 {
		return out, nil
	}
	args := make([]any, 0, len(out))
	byID := make(map[int64]*Show, len(out))
	for _, s := range out {
		args = append(args, s.ID)
		byID[s.ID] = s
	}
	tagRows, err := d.db.Query(`
SELECT st.show_id, t.name FROM show_tags st
JOIN tags t ON t.id = st.tag_id
WHERE st.show_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(out)), ",")+`)
ORDER BY t.name`, args...)
	if err != nil {
		return nil, err
	}
	defer tagRows.Close()
	for tagRows.Next() {
		var showID int64
		var name string
		if err := tagRows.Scan(&showID, &name); err != nil {
			return nil, err
		}
		if s := byID[showID]; s != nil {
			s.Tags = append(s.Tags, name)
		}
	}
	return out, tagRows.Err()
}

// CountShows 可播短剧总数（与列表口径一致，隐藏 0 可播的空壳条目）
func (d *DB) CountShows() (int64, error) {
	var n int64
	err := d.db.QueryRow(`SELECT COUNT(*) FROM shows s` + listShowWhere).Scan(&n)
	return n, err
}

// CountShowsByTag 某标签下的短剧数量；tag 为空时返回总数。
// 分页判定依赖这个数量，不能再用全库总数，否则按标签筛选时永远翻不完。
func (d *DB) CountShowsByTag(tag string) (int64, error) {
	if tag == "" {
		return d.CountShows()
	}
	var n int64
	err := d.db.QueryRow(`
SELECT COUNT(DISTINCT s.id) FROM shows s
JOIN show_tags st ON st.show_id = s.id
JOIN tags t ON t.id = st.tag_id`+listShowWhere+`
  AND t.name = ?`, tag).Scan(&n)
	return n, err
}

// UpsertEpisode 按剧ID+集号查找或新建剧集
func (d *DB) UpsertEpisode(showID int64, episodeNo int) (int64, bool, error) {
	return upsertEpisode(d.db, showID, episodeNo)
}

func upsertEpisode(q execQuerier, showID int64, episodeNo int) (id int64, inserted bool, err error) {
	var existing int64
	err = q.QueryRow(`SELECT id FROM episodes WHERE show_id = ? AND episode_no = ?`,
		showID, episodeNo).Scan(&existing)
	switch {
	case err == nil:
		return existing, false, nil
	case errors.Is(err, sql.ErrNoRows):
		res, ierr := q.Exec(`
INSERT INTO episodes (show_id, episode_no, created_at, updated_at)
VALUES (?, ?, ?, ?)`, showID, episodeNo, nowUnix(), nowUnix())
		if ierr != nil {
			return 0, false, ierr
		}
		// 新剧集产生，更新短剧的更新时间
		_, _ = q.Exec(`UPDATE shows SET updated_at = ? WHERE id = ?`, nowUnix(), showID)
		id, ierr = res.LastInsertId()
		return id, true, ierr
	default:
		return 0, false, err
	}
}

// ListEpisodes 列出某部剧的可播剧集（只含有启用频道视频来源的集，
// 避免把空集/残留集、以及只剩关闭频道来源的集展示成选集入口）
func (d *DB) ListEpisodes(showID int64) ([]*Episode, error) {
	rows, err := d.db.Query(`
SELECT id, show_id, episode_no, title, created_at, updated_at
FROM episodes
WHERE show_id = ?
  AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = episodes.id`+liveSrcCond+`)
ORDER BY episode_no`, showID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Episode
	for rows.Next() {
		var e Episode
		if err := rows.Scan(&e.ID, &e.ShowID, &e.EpisodeNo, &e.Title, &e.CreatedAt, &e.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &e)
	}
	return out, nil
}

// GetEpisode 按主键查询剧集
func (d *DB) GetEpisode(id int64) (*Episode, error) {
	var e Episode
	err := d.db.QueryRow(`
SELECT id, show_id, episode_no, title, created_at, updated_at
FROM episodes WHERE id = ?`, id).Scan(&e.ID, &e.ShowID, &e.EpisodeNo, &e.Title, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &e, nil
}

// UpsertSource 按频道+消息+分段唯一键查找或新建视频来源
func (d *DB) UpsertSource(s *Source) (id int64, inserted bool, err error) {
	var existing int64
	err = d.db.QueryRow(`
SELECT id FROM sources WHERE chat_id = ? AND message_id = ? AND segment_index = ?`,
		s.ChatID, s.MessageID, s.SegmentIndex).Scan(&existing)
	switch {
	case err == nil:
		return existing, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, false, err
	}
	res, err := d.db.Exec(`
INSERT INTO sources (
    episode_id, chat_id, message_id, grouped_id, segment_index,
    file_name, file_size, duration, width, height, mime,
    doc_id, access_hash, file_reference, caption, download_status, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`,
		s.EpisodeID, s.ChatID, s.MessageID, s.GroupedID, s.SegmentIndex,
		s.FileName, s.FileSize, s.Duration, s.Width, s.Height, s.Mime,
		s.DocID, s.AccessHash, s.FileReference, s.Caption, nowUnix())
	if err != nil {
		return 0, false, err
	}
	id, err = res.LastInsertId()
	return id, true, err
}

// GetSource 按主键查询来源（含频道名）
func (d *DB) GetSource(id int64) (*Source, error) {
	row := d.db.QueryRow(sourceSelectSQL+` WHERE s.id = ?`, id)
	return scanSource(row)
}

const sourceSelectSQL = `
SELECT s.id, s.episode_id, s.chat_id, c.name, c.username, s.message_id, s.grouped_id, s.segment_index,
       s.file_name, s.file_path, s.file_size, s.duration, s.width, s.height, s.mime,
       s.doc_id, s.access_hash, s.file_reference, s.caption, s.download_status, s.error, s.created_at,
       s.variants_checked
FROM sources s JOIN chats c ON c.id = s.chat_id`

func scanSource(r rowScanner) (*Source, error) {
	var s Source
	if err := r.Scan(
		&s.ID, &s.EpisodeID, &s.ChatID, &s.ChatName, &s.ChatUsername, &s.MessageID, &s.GroupedID, &s.SegmentIndex,
		&s.FileName, &s.FilePath, &s.FileSize, &s.Duration, &s.Width, &s.Height, &s.Mime,
		&s.DocID, &s.AccessHash, &s.FileReference, &s.Caption, &s.DownloadStatus, &s.Error, &s.CreatedAt,
		&s.VariantsChecked,
	); err != nil {
		return nil, err
	}
	return &s, nil
}

// ListPendingSources 取出待下载的来源（用户点播的优先）
func (d *DB) ListPendingSources(limit int) ([]*Source, error) {
	rows, err := d.db.Query(sourceSelectSQL+`
WHERE s.download_status IN ('pending', 'failed')
ORDER BY s.priority DESC, s.id LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// ResetDownloading 将上次运行遗留的 downloading 状态重置为 pending
func (d *DB) ResetDownloading() error {
	_, err := d.db.Exec(`UPDATE sources SET download_status = 'pending' WHERE download_status = 'downloading'`)
	return err
}

// RequestEpisodePriority 用户点播某集：该集全部来源插队优先下载，
// 并解除可能卡死的 downloading 状态。
func (d *DB) RequestEpisodePriority(episodeID int64) error {
	_, err := d.db.Exec(`
UPDATE sources SET priority = 1,
    download_status = CASE WHEN download_status = 'downloading' THEN 'pending' ELSE download_status END
WHERE episode_id = ?`, episodeID)
	return err
}

// ResetMediaDownloads 清空视频缓存后重置全部下载状态为待下载
func (d *DB) ResetMediaDownloads() error {
	_, err := d.db.Exec(`
UPDATE sources SET download_status = 'pending', file_path = '', priority = 0
WHERE download_status IN ('done', 'downloading')`)
	return err
}

// UpdateSourceLocation 刷新文件定位信息（file_reference 过期后使用）
func (d *DB) UpdateSourceLocation(sourceID, docID, accessHash int64, fileReference []byte) error {
	_, err := d.db.Exec(`
UPDATE sources SET doc_id = ?, access_hash = ?, file_reference = ?
WHERE id = ?`, docID, accessHash, fileReference, sourceID)
	return err
}

// SetSourceTestSizeForTest 覆盖来源的文件大小，仅供测试构造大文件场景。
func (d *DB) SetSourceTestSizeForTest(sourceID, size int64) error {
	_, err := d.db.Exec(`UPDATE sources SET file_size = ? WHERE id = ?`, size, sourceID)
	return err
}

// ListEpisodePendingSources 列出某集待下载（pending/failed）的来源
func (d *DB) ListEpisodePendingSources(episodeID int64, limit int) ([]*Source, error) {
	rows, err := d.db.Query(sourceSelectSQL+`
WHERE s.episode_id = ? AND s.download_status IN ('pending', 'failed')
ORDER BY s.priority DESC, s.id LIMIT ?`, episodeID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Source
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// MarkSourceDownloading 认领来源开始下载（pending/failed → downloading）。
// 返回 false 表示已被其他协程认领，跳过防止重复下载。
func (d *DB) MarkSourceDownloading(id int64) (bool, error) {
	res, err := d.db.Exec(`
UPDATE sources SET download_status = 'downloading', error = ''
WHERE id = ? AND download_status IN ('pending', 'failed')`, id)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// MarkSourceDone 下载完成
func (d *DB) MarkSourceDone(id int64, filePath string, size int64) error {
	_, err := d.db.Exec(`
UPDATE sources SET download_status = 'done', file_path = ?, file_size = ?, error = '' WHERE id = ?`,
		filePath, size, id)
	return err
}

// MarkSourceFailed 下载失败并记录原因
func (d *DB) MarkSourceFailed(id int64, reason string) error {
	_, err := d.db.Exec(`UPDATE sources SET download_status = 'failed', error = ? WHERE id = ?`,
		reason, id)
	return err
}

// SetShowCover 更新短剧封面
func (d *DB) SetShowCover(showID int64, coverPath string) error {
	_, err := d.db.Exec(`UPDATE shows SET cover_path = ? WHERE id = ? AND cover_path = ''`,
		coverPath, showID)
	return err
}

// SetShowIntro 补充短剧简介（仅在为空时写入）
func (d *DB) SetShowIntro(showID int64, intro string) error {
	return setShowIntro(d.db, showID, intro)
}

func setShowIntro(q execQuerier, showID int64, intro string) error {
	_, err := q.Exec(`UPDATE shows SET intro = ? WHERE id = ? AND intro = ''`,
		intro, showID)
	return err
}

// AddShowTags 给短剧关联标签（自动建标签）
func (d *DB) AddShowTags(showID int64, tags []string) error {
	return addShowTags(d.db, showID, tags)
}

func addShowTags(q execQuerier, showID int64, tags []string) error {
	for _, t := range tags {
		if _, err := q.Exec(`INSERT INTO tags (name) VALUES (?) ON CONFLICT(name) DO NOTHING`, t); err != nil {
			return err
		}
		var tagID int64
		if err := q.QueryRow(`SELECT id FROM tags WHERE name = ?`, t).Scan(&tagID); err != nil {
			return err
		}
		if _, err := q.Exec(`INSERT OR IGNORE INTO show_tags (show_id, tag_id) VALUES (?, ?)`,
			showID, tagID); err != nil {
			return err
		}
	}
	return nil
}

// ListTags 全部标签及对应短剧数量（只统计启用频道提供的来源）
func (d *DB) ListTags() ([]*TagCount, error) {
	rows, err := d.db.Query(`
SELECT t.name, COUNT(st.show_id) AS n
FROM tags t JOIN show_tags st ON st.tag_id = t.id
WHERE EXISTS (
    SELECT 1 FROM episodes e
    WHERE e.show_id = st.show_id AND e.episode_no >= 1
      AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = e.id` + liveSrcCond + `)
)
  AND COALESCE((SELECT s2.hidden FROM shows s2 WHERE s2.id = st.show_id), 0) = 0
  AND NOT (
    COALESCE((SELECT s2.cover_path FROM shows s2 WHERE s2.id = st.show_id), '') = ''
    AND (SELECT COUNT(DISTINCT e4.episode_no) FROM episodes e4
         WHERE e4.show_id = st.show_id AND e4.episode_no >= 1
           AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = e4.id` + liveSrcCond + `)) = 1
  )
GROUP BY t.id ORDER BY n DESC, t.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*TagCount
	for rows.Next() {
		var tc TagCount
		if err := rows.Scan(&tc.Name, &tc.Count); err != nil {
			return nil, err
		}
		out = append(out, &tc)
	}
	return out, nil
}

// CountSourcesByGrouped 统计某频道内同一 grouped_id 已入库的分段数，用于分段编号
func (d *DB) CountSourcesByGrouped(chatPK int64, groupedID int64) int {
	var n int
	_ = d.db.QueryRow(`
SELECT COUNT(*) FROM sources WHERE chat_id = ? AND grouped_id = ?`,
		chatPK, groupedID).Scan(&n)
	return n
}

// CountSourceStatuses 统计各状态来源数量（done/failed/pending+downloading）
func (d *DB) CountSourceStatuses() (done, failed, pending int64) {
	rows, err := d.db.Query(`
SELECT download_status, COUNT(*) FROM sources GROUP BY download_status`)
	if err != nil {
		return 0, 0, 0
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int64
		if err := rows.Scan(&status, &n); err != nil {
			continue
		}
		switch status {
		case "done":
			done += n
		case "failed":
			failed += n
		default:
			pending += n
		}
	}
	return done, failed, pending
}

// linePlayScore 线路排序分：整条线路的视频总时长越长越像正片。
// 时长缺失时用文件总大小兜底；短预告片线路会排到最后，播放器默认线路不会选到它。
func linePlayScore(l *PlayLine) (float64, int64) {
	var dur float64
	var size int64
	for i := range l.Segments {
		dur += l.Segments[i].Duration
		size += l.Segments[i].FileSize
	}
	return dur, size
}

// ListEpisodeLines 按频道分组返回可播放线路（不再要求已下载，由流式接口实时读取）。
// 只列启用中的频道：关闭频道的来源不再作为线路出现。
// 线路按「视频总时长」从长到短排序：同一集里预告片/试看片段线路排在正片之后，
// 经典播放器默认取第 1 条线路就不会先播预告片；线路内的分段顺序保持不变。
func (d *DB) ListEpisodeLines(episodeID int64) ([]*PlayLine, error) {
	// 不再要求已下载：未缓存来源由 /api/stream 实时代理
	rows, err := d.db.Query(sourceSelectSQL+`
WHERE s.episode_id = ? AND c.enabled = 1
ORDER BY s.chat_id, s.segment_index`, episodeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var lines []*PlayLine
	byChat := map[int64]*PlayLine{}
	for rows.Next() {
		s, err := scanSource(rows)
		if err != nil {
			return nil, err
		}
		line, ok := byChat[s.ChatID]
		if !ok {
			line = &PlayLine{ChatID: s.ChatID, ChatName: s.ChatName, Segments: []Source{}}
			byChat[s.ChatID] = line
			lines = append(lines, line)
		}
		line.Segments = append(line.Segments, *s)
	}
	sort.SliceStable(lines, func(i, j int) bool {
		di, si := linePlayScore(lines[i])
		dj, sj := linePlayScore(lines[j])
		if di != dj {
			return di > dj
		}
		if si != sj {
			return si > sj
		}
		return lines[i].ChatID < lines[j].ChatID
	})
	return lines, nil
}

// socialPostKeywords 社交平台搬运标记（与 parser 的 reSocialPost 保持一致，
// 这里用 SQL LIKE 重写一遍是因为要按「全部来源文案」做聚合判断）。
var socialPostKeywords = []string{"推特", "twitter", "onlyfans", "福利姬"}

// RecomputeHiddenShows 重新评估整库「哪些条目其实不是短剧」，把命中的标记成
// hidden（整库不再展示，数据保留）。两类目标：
//  1. 剧名只是「露脸」「大学生」这类泛分类词，或整名是「点此开通 立刻进群追更」
//     这类拉群话术——把频道里互不相干的视频糊成一部「剧」的产物；
//  2. 该剧所有来源的文案都带推特/OnlyFans 博主标记——短剧频道顺手转发的福利视频，
//     不是一部剧。
//
// 每次全量重算（两千多行不到 20ms），这样规则放宽后旧条目能自动恢复显示。
func (d *DB) RecomputeHiddenShows() (int, error) {
	rows, err := d.db.Query(`SELECT id, name FROM shows`)
	if err != nil {
		return 0, err
	}
	var junk []int64
	for rows.Next() {
		var (
			id   int64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return 0, err
		}
		if manualVisibleNames[strings.TrimSpace(name)] {
			continue
		}
		if parser.IsJunkShowName(name) || manualHiddenMatch(name) {
			junk = append(junk, id)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	// 「准确识别」= 来源文案里有可信剧名（《》/【】容器或 # 标签）且与当前剧名一致。
	// 只有一集、又对不上任何可信剧名的，基本是从文件名/裸词硬猜出来的名字
	// （偷窥4、竖屏、完整1…），先隐藏，搜索仍可查到。
	titled, container, err := d.identifiedShows()
	if err != nil {
		return 0, err
	}
	social, err := d.listSocialOnlyShows()
	if err != nil {
		return 0, err
	}
	// 文案里只是提到「推特原创」「推特大佬手搓」的，往往是正常短剧的发布说明；
	// 只有《》/【】标题容器也对不上时，才按社交平台搬运的福利视频处理。
	// 这里不用 #标签 兜底：福利视频的标签就是 #试衣间 #寸止 这类话题词，不是剧名。
	for _, id := range social {
		if !container[id] {
			junk = append(junk, id)
		}
	}
	// 只有一集、名字又明显不是短剧（演唱会/福利/众筹/美图修图/宣发物料…）的条目，
	// 用名字关键词单独拦一遍：这类名字提取不出「第几集」，和「正常短剧只发了一集」
	// 的区别就在内容本身，不能靠集数一刀切。
	singles, err := d.listNonDramaSingleShows()
	if err != nil {
		return 0, err
	}
	junk = append(junk, singles...)
	oneEps, err := d.listSingleEpisodeShows()
	if err != nil {
		return 0, err
	}
	for _, id := range oneEps {
		if !titled[id] {
			junk = append(junk, id)
		}
	}
	// 同一条目可能同时命中多条规则（例如「推特福利」既像社交搬运又是单集福利名），
	// 去重后再标记，隐藏数才是真实条目数。
	uniq := junk[:0]
	seen := make(map[int64]struct{}, len(junk))
	for _, id := range junk {
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		uniq = append(uniq, id)
	}
	junk = uniq
	if _, err := d.db.Exec(`UPDATE shows SET hidden = 0 WHERE hidden <> 0`); err != nil {
		return 0, err
	}
	if len(junk) == 0 {
		return 0, nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.Prepare(`UPDATE shows SET hidden = 1 WHERE id = ?`)
	if err != nil {
		return 0, err
	}
	for _, id := range junk {
		if _, err := stmt.Exec(id); err != nil {
			stmt.Close()
			return 0, err
		}
	}
	stmt.Close()
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(junk), nil
}

// listSocialOnlyShows 找出「所有来源文案都是社交平台搬运」的剧：
// 只要还有一条来源的文案是正常剧集帖，就不算。文案就在 sources.caption 上。
//
// 注意这里不能加 hidden = 0：RecomputeHiddenShows 是「先算集合、再统一重置并
// 标记」，如果只在上次没被隐藏的剧里找，上次已经隐藏的那批就永远算不进来，
// 结果下一次重算时它们会被解隐藏、重新出现在列表里——隐藏标记于是在两次启动
// 之间来回跳（实测 133 → 120 → 133）。
func (d *DB) listSocialOnlyShows() ([]int64, error) {
	conds := make([]string, 0, len(socialPostKeywords))
	for _, k := range socialPostKeywords {
		conds = append(conds, "x.caption LIKE '%"+k+"%'")
	}
	like := "(" + strings.Join(conds, " OR ") + ")"
	rows, err := d.db.Query(`
SELECT s.id FROM shows s
WHERE EXISTS (SELECT 1 FROM episodes e1 JOIN sources x1 ON x1.episode_id = e1.id
              WHERE e1.show_id = s.id)
  AND EXISTS (SELECT 1 FROM episodes e2
              JOIN sources x ON x.episode_id = e2.id
              WHERE e2.show_id = s.id AND ` + like + `)
  AND NOT EXISTS (SELECT 1 FROM episodes e3
                  JOIN sources y ON y.episode_id = e3.id
                  WHERE e3.show_id = s.id AND y.caption <> '' AND NOT ` +
		strings.ReplaceAll(like, "x.caption", "y.caption") + `)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// listNonDramaSingleShows 找出「只有一集可播、名字明显不是短剧」的条目。
// 判据只有名字关键词（演唱会/福利/众筹/美图视频消除/宣发/片花/漫展/街拍/vlog…），
// 不看集数多少：正常的短剧只发一集也照样展示，演唱会、福利视频、修图工具这类才隐藏。
func (d *DB) listNonDramaSingleShows() ([]int64, error) {
	rows, err := d.db.Query(`
SELECT s.id, s.name FROM shows s
WHERE 1 = (SELECT COUNT(DISTINCT e.episode_no) FROM episodes e
           WHERE e.show_id = s.id AND e.episode_no >= 1
             AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = e.id` + liveSrcCond + `))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var (
			id   int64
			name string
		)
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		if parser.IsNonDramaSingleName(name) {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// listSingleEpisodeShows 找出「只有一集可播」的条目 id。
// 这类条目里混着大量从文件名硬猜出来的名字，用户要求先隐藏、只留搜索可见。
func (d *DB) listSingleEpisodeShows() ([]int64, error) {
	rows, err := d.db.Query(`
SELECT s.id FROM shows s
WHERE 1 = (SELECT COUNT(DISTINCT e.episode_no) FROM episodes e
           WHERE e.show_id = s.id AND e.episode_no >= 1
             AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = e.id` + liveSrcCond + `))`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// identifiedShows 返回两组「来源文案里认出了剧名」的剧 id：
//   - titled：剧名来自《》/【】容器或 # 标签，且与当前剧名一致；
//   - container：剧名只来自《》/【】容器（更严格，社交搬运筛查用）。
//
// 从文件名或正文裸词猜出来的名字两组里都没有。
func (d *DB) identifiedShows() (titled, container map[int64]bool, err error) {
	rows, err := d.db.Query(`
SELECT s.id, s.name, src.caption
FROM shows s
JOIN episodes e ON e.show_id = s.id
JOIN sources src ON src.episode_id = e.id
WHERE src.caption <> ''`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	names := map[int64]string{}
	titled = map[int64]bool{}
	container = map[int64]bool{}
	p := parser.New()
	for rows.Next() {
		var (
			id          int64
			name, ctext string
		)
		if err := rows.Scan(&id, &name, &ctext); err != nil {
			return nil, nil, err
		}
		if titled[id] && container[id] {
			continue
		}
		if _, ok := names[id]; !ok {
			names[id] = name
		}
		got, err := p.Parse(ctext, "")
		if err != nil || !(got.HasTitle || got.FromTag) {
			continue
		}
		if parser.Normalize(got.ShowName) != parser.Normalize(names[id]) {
			continue
		}
		titled[id] = true
		// Container 只在剧名确实取自《》/【】容器时置位；HasTitle 在
		// 剧名取自 #标签 时也会置位，不能用来判断有没有标题容器。
		if got.Container {
			container[id] = true
		}
	}
	return titled, container, rows.Err()
}

// CountHiddenShows 已隐藏的条目数（版本指纹用：清理规则变了要让前端缓存失效）
func (d *DB) CountHiddenShows() int64 {
	var n int64
	_ = d.db.QueryRow(`SELECT COUNT(*) FROM shows WHERE hidden = 1`).Scan(&n)
	return n
}
