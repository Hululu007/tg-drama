package storage

import (
	"database/sql"
	"errors"
)

// ErrShowNotFound 收藏时指定的短剧不存在
var ErrShowNotFound = errors.New("短剧不存在")

// progressRatio 计算 0-1 的观看比例
func progressRatio(position, duration float64, finished bool) float64 {
	if finished {
		return 1
	}
	if duration <= 0 {
		return 0
	}
	r := position / duration
	if r < 0 {
		return 0
	}
	if r > 1 {
		return 1
	}
	return r
}

// SaveHistory 记录或更新某集的观看进度。
// position/duration 单位为秒；duration 未知时传 0，会保留库里已有的总时长。
// 播放到 95% 以上视为看完，记 finished，前端据此在选集里打勾。
func (d *DB) SaveHistory(episodeID int64, position, duration float64) error {
	if episodeID <= 0 {
		return errors.New("无效的剧集ID")
	}
	if position < 0 {
		position = 0
	}
	if duration < 0 {
		duration = 0
	}
	finished := 0
	if duration > 0 && position >= duration*0.95 {
		finished = 1
	}
	_, err := d.db.Exec(`
INSERT INTO history (episode_id, position, duration, finished, updated_at)
VALUES (?, ?, ?, ?, ?)
ON CONFLICT(episode_id) DO UPDATE SET
    position   = excluded.position,
    duration   = CASE WHEN excluded.duration > 0 THEN excluded.duration ELSE history.duration END,
    finished   = excluded.finished,
    updated_at = excluded.updated_at`,
		episodeID, position, duration, finished, nowUnix())
	return err
}

// GetEpisodeProgress 查询某集的观看进度；没有记录返回 (nil, nil)
func (d *DB) GetEpisodeProgress(episodeID int64) (*WatchProgress, error) {
	var p WatchProgress
	err := d.db.QueryRow(`
SELECT h.episode_id, e.show_id, e.episode_no, h.position, h.duration, h.finished, h.updated_at
FROM history h JOIN episodes e ON e.id = h.episode_id
WHERE h.episode_id = ?`, episodeID).Scan(
		&p.EpisodeID, &p.ShowID, &p.EpisodeNo, &p.Position, &p.Duration, &p.Finished, &p.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	p.Progress = progressRatio(p.Position, p.Duration, p.Finished)
	return &p, nil
}

// ListHistory 按短剧聚合的观看历史：每部剧只取最近观看的一集，用于首页「继续观看」。
// 窗口函数在 SQLite 3.25+ 可用（modernc.org/sqlite 内置版本满足）。
// ListHistory 继续观看列表（每部剧取最近一集）。
// 只保留「至少有一集由启用频道提供、且未被标记为非短剧」的条目：关闭频道的
// 内容不该继续出现在首页「继续观看」里（数据保留，重新开启即恢复）。
func (d *DB) ListHistory(limit int) ([]*HistoryItem, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := d.db.Query(`
SELECT episode_id, show_id, name, cover_path, episode_no,
       position, duration, finished, updated_at
FROM (
    SELECT h.episode_id, e.show_id, sh.name AS name, sh.cover_path AS cover_path,
           e.episode_no, h.position, h.duration, h.finished, h.updated_at,
           ROW_NUMBER() OVER (
               PARTITION BY e.show_id
               ORDER BY h.updated_at DESC, h.episode_id DESC
           ) AS rn
    FROM history h
    JOIN episodes e ON e.id = h.episode_id
    JOIN shows sh   ON sh.id = e.show_id
) t
WHERE rn = 1
  AND NOT EXISTS (SELECT 1 FROM shows s2 WHERE s2.id = t.show_id AND s2.hidden = 1)
  AND EXISTS (
      SELECT 1 FROM episodes e2
      WHERE e2.show_id = t.show_id AND e2.episode_no >= 1
        AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = e2.id`+liveSrcCond+`)
  )
ORDER BY updated_at DESC, episode_id DESC
LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*HistoryItem
	for rows.Next() {
		var h HistoryItem
		if err := rows.Scan(&h.EpisodeID, &h.ShowID, &h.ShowName, &h.CoverPath, &h.EpisodeNo,
			&h.Position, &h.Duration, &h.Finished, &h.UpdatedAt); err != nil {
			return nil, err
		}
		h.Progress = progressRatio(h.Position, h.Duration, h.Finished)
		out = append(out, &h)
	}
	return out, rows.Err()
}

// ListShowProgress 某部剧每一集的观看进度（详情页选集打勾用），按最近观看倒序
func (d *DB) ListShowProgress(showID int64) ([]*WatchProgress, error) {
	rows, err := d.db.Query(`
SELECT h.episode_id, e.show_id, e.episode_no, h.position, h.duration, h.finished, h.updated_at
FROM history h JOIN episodes e ON e.id = h.episode_id
WHERE e.show_id = ?
ORDER BY h.updated_at DESC, e.episode_no`, showID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*WatchProgress
	for rows.Next() {
		var p WatchProgress
		if err := rows.Scan(&p.EpisodeID, &p.ShowID, &p.EpisodeNo, &p.Position,
			&p.Duration, &p.Finished, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.Progress = progressRatio(p.Position, p.Duration, p.Finished)
		out = append(out, &p)
	}
	return out, rows.Err()
}

// DeleteHistory 删除某一集的观看记录
func (d *DB) DeleteHistory(episodeID int64) error {
	_, err := d.db.Exec(`DELETE FROM history WHERE episode_id = ?`, episodeID)
	return err
}

// ClearHistory 清空全部观看记录
func (d *DB) ClearHistory() error {
	_, err := d.db.Exec(`DELETE FROM history`)
	return err
}

// IsFavorite 是否已收藏
func (d *DB) IsFavorite(showID int64) (bool, error) {
	var one int
	err := d.db.QueryRow(`SELECT 1 FROM favorites WHERE show_id = ?`, showID).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ToggleFavorite 切换收藏状态，返回切换后的状态
func (d *DB) ToggleFavorite(showID int64) (bool, error) {
	if showID <= 0 {
		return false, ErrShowNotFound
	}
	var one int
	err := d.db.QueryRow(`SELECT 1 FROM favorites WHERE show_id = ?`, showID).Scan(&one)
	switch {
	case err == nil:
		_, err = d.db.Exec(`DELETE FROM favorites WHERE show_id = ?`, showID)
		return false, err
	case !errors.Is(err, sql.ErrNoRows):
		return false, err
	}
	// 先确认短剧存在，避免外键报错变成看不懂的 500
	var exists int
	if err := d.db.QueryRow(`SELECT 1 FROM shows WHERE id = ?`, showID).Scan(&exists); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, ErrShowNotFound
		}
		return false, err
	}
	// 记下收藏当时的更新进度作为追剧基线：之后新增的集才算「更新」，
	// 收藏时早就存在的集（哪怕用户还没看）不会被误报成更新。
	var base int
	if err := d.db.QueryRow(`
SELECT COALESCE(MAX(e.episode_no), 0) FROM episodes e
WHERE e.show_id = ? AND e.episode_no >= 1
  AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = e.id`+liveSrcCond+`)`, showID).Scan(&base); err != nil {
		return false, err
	}
	if _, err := d.db.Exec(`INSERT INTO favorites (show_id, created_at, base_ep) VALUES (?, ?, ?)`,
		showID, nowUnix(), base); err != nil {
		return false, err
	}
	return true, nil
}

// 收藏剧的「更新」口径：
//
//	watched_ep = history 里这部剧看过的最大集号（0 = 没看过）
//	updates    = watched_ep 之后才发布的可播集数
//
// 只有同时满足下面两个条件才计数，避免制造噪音：
//  1. watched_ep >= 2：用户还停在第一集（或根本没开始看）说明只是随手收藏，不提示「更新」；
//  2. watched_ep >= base_ep：收藏时并没有追到最新一集，说明进度落后，
//     此时新增的集不该算成「这次更新」；等他追上新进度后自然会亮起来。
const favWatchedJoinSQL = `
LEFT JOIN (
    SELECT e2.show_id AS show_id, MAX(e2.episode_no) AS max_no
    FROM history h JOIN episodes e2 ON e2.id = h.episode_id
    GROUP BY e2.show_id
) w ON w.show_id = s.id`

const favUpdatesExpr = `
CASE WHEN COALESCE(w.max_no, 0) >= 2 AND COALESCE(w.max_no, 0) >= f.base_ep
     THEN (SELECT COUNT(DISTINCT e3.episode_no) FROM episodes e3
           WHERE e3.show_id = s.id AND e3.episode_no > w.max_no
             AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = e3.id` + liveSrcCond + `))
     ELSE 0 END`

// ListFavoriteShows 分页列出收藏的短剧，最近收藏的在前；同时算出已看到第几集与新增集数。
// sortBy == "episodes" 时改按可播集数倒序（集数相同仍按收藏时间）。
func (d *DB) ListFavoriteShows(sortBy string, limit, offset int) ([]*Show, error) {
	order := ` ORDER BY f.created_at DESC, s.id DESC`
	if sortBy == "episodes" {
		order = ` ORDER BY ep_count DESC, f.created_at DESC, s.id DESC`
	}
	rows, err := d.db.Query(`SELECT `+showBaseColumns+`,
       COALESCE(w.max_no, 0) AS watched_ep,`+favUpdatesExpr+` AS updates
FROM shows s
JOIN favorites f ON f.show_id = s.id`+favWatchedJoinSQL+playableShowWhere+order+`
 LIMIT ? OFFSET ?`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*Show
	for rows.Next() {
		var s Show
		if err := rows.Scan(&s.ID, &s.Name, &s.NormName, &s.CoverPath, &s.Intro, &s.Status,
			&s.CreatedAt, &s.UpdatedAt, &s.EpCount, &s.EpLatest, &s.WatchedEp, &s.Updates); err != nil {
			return nil, err
		}
		s.Tags = []string{}
		out = append(out, &s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return d.finishShows(out)
}

// CountFavoriteUpdates 所有收藏剧的新增集数合计（收藏按钮上的数字徽标）。
// 口径与 ListFavoriteShows 的 updates 完全一致，避免徽标数字和列表对不上。
func (d *DB) CountFavoriteUpdates() (int64, error) {
	var n int64
	err := d.db.QueryRow(`SELECT COALESCE(SUM(updates), 0) FROM (
    SELECT ` + favUpdatesExpr + ` AS updates
    FROM shows s
    JOIN favorites f ON f.show_id = s.id` + favWatchedJoinSQL + playableShowWhere + `
)`).Scan(&n)
	return n, err
}

// CountFavoriteShows 收藏总数（只统计仍可播的剧，与收藏列表口径一致）
func (d *DB) CountFavoriteShows() (int64, error) {
	var n int64
	err := d.db.QueryRow(`
SELECT COUNT(*) FROM favorites f
JOIN shows s ON s.id = f.show_id` + playableShowWhere).Scan(&n)
	return n, err
}
