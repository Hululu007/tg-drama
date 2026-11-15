package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"tgdrama/internal/parser"
)

// MergeShowResult 一次「合并重复短剧」的结果，仅用于离线工具输出。
type MergeShowResult struct {
	Backup           string `json:"backup"`
	FromShowID       int64  `json:"from_show_id"`
	ToShowID         int64  `json:"to_show_id"`
	MovedSources     int    `json:"moved_sources"`
	MovedEpisodes    int    `json:"moved_episodes"`
	MergedEpisodes   int    `json:"merged_episodes"`
	MovedPhotos      int    `json:"moved_photos"`
	DroppedPhotos    int    `json:"dropped_conflicting_photos"`
	MovedTags        int    `json:"moved_tags"`
	MovedFavorites   int    `json:"moved_favorites"`
	MovedHistories   int    `json:"moved_histories"`
	CoverTransferred bool   `json:"cover_transferred"`
}

// MergeShowInto 把 fromID 合并进 toID：只搬来源/剧集/图片/标签/收藏/观看记录，
// 保留双方已有的 ID；fromID 剧行最后删除，toID 的 ID 与名称不变。
//
// 与 RebuildFromRaw / MergeDuplicateShows 不同，这里不做整库重解析：
//   - 单事务，任何冲突（同集两边都有观看记录、来源唯一键冲突、名称不相关）整体回滚；
//   - 写前先用独立只读连接做含 WAL 的一致性备份，备份失败不写库；
//   - 不修改任何 episode_no，不改 toID 的名称与隐藏状态；
//   - 重复执行会因 fromID 已不存在而报错，不会二次改动。
//
// 只应在离线（服务未写入）时调用；调用方必须显式给出 from/to 与备份路径。
func MergeShowInto(ctx context.Context, path string, fromID, toID int64, backupPath string) (*MergeShowResult, error) {
	result := &MergeShowResult{FromShowID: fromID, ToShowID: toID}
	if fromID <= 0 || toID <= 0 || fromID == toID {
		return result, errors.New("merge requires two different positive show IDs")
	}
	if strings.TrimSpace(backupPath) == "" {
		return result, errors.New("merge requires a new backup path")
	}
	abs, err := nameDatabasePath(path)
	if err != nil {
		return result, err
	}
	db, err := openNameDatabase(ctx, abs, true, false)
	if err != nil {
		return result, err
	}
	defer db.Close()
	tx, err := db.BeginTx(ctx, nil) // _txlock=immediate：合并期间不允许其他写入
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	var fromName, fromNorm, fromCover string
	if err := tx.QueryRowContext(ctx, `SELECT name, norm_name, cover_path FROM shows WHERE id=?`, fromID).
		Scan(&fromName, &fromNorm, &fromCover); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, fmt.Errorf("show %d does not exist", fromID)
		}
		return result, err
	}
	var toName, toNorm, toCover string
	if err := tx.QueryRowContext(ctx, `SELECT name, norm_name, cover_path FROM shows WHERE id=?`, toID).
		Scan(&toName, &toNorm, &toCover); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return result, fmt.Errorf("target show %d does not exist", toID)
		}
		return result, err
	}
	// 只允许合并「名字互相包含」的剧，避免误把两部无关的剧并到一起。
	fn, tn := parser.Normalize(fromName), parser.Normalize(toName)
	if fn == "" || tn == "" || (!strings.Contains(fn, tn) && !strings.Contains(tn, fn)) {
		return result, fmt.Errorf("refuse merge: show names %q and %q are unrelated", fromName, toName)
	}
	if fromNorm == toNorm {
		return result, fmt.Errorf("refuse merge: both shows already share normalized name %q", fromNorm)
	}

	type epRow struct{ id, no int64 }
	var fromEps []epRow
	rows, err := tx.QueryContext(ctx, `SELECT id, episode_no FROM episodes WHERE show_id=? ORDER BY episode_no, id`, fromID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var e epRow
		if err := rows.Scan(&e.id, &e.no); err != nil {
			rows.Close()
			return result, err
		}
		fromEps = append(fromEps, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}
	toEps := map[int64]int64{}
	rows, err = tx.QueryContext(ctx, `SELECT id, episode_no FROM episodes WHERE show_id=?`, toID)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var id, no int64
		if err := rows.Scan(&id, &no); err != nil {
			rows.Close()
			return result, err
		}
		toEps[no] = id
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return result, err
	}

	// 预检：来源唯一键 (chat_id,message_id,segment_index) 在两边重复时无法搬移。
	for _, e := range fromEps {
		target, ok := toEps[e.no]
		if !ok {
			continue
		}
		var dup int
		if err := tx.QueryRowContext(ctx, `
SELECT COUNT(*) FROM sources a JOIN sources b
  ON a.chat_id=b.chat_id AND a.message_id=b.message_id AND a.segment_index=b.segment_index
WHERE a.episode_id=? AND b.episode_id=?`, e.id, target).Scan(&dup); err != nil {
			return result, err
		}
		if dup > 0 {
			return result, fmt.Errorf("refuse merge: episode %d has sources that already exist in episode %d", e.id, target)
		}
	}
	// 预检：同一集两边都有观看记录时无法判断保留哪条，整体拒绝。
	for _, e := range fromEps {
		target, ok := toEps[e.no]
		if !ok {
			continue
		}
		var fromHist, toHist int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM history WHERE episode_id=?`, e.id).Scan(&fromHist); err != nil {
			return result, err
		}
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM history WHERE episode_id=?`, target).Scan(&toHist); err != nil {
			return result, err
		}
		if fromHist > 0 && toHist > 0 {
			return result, fmt.Errorf("refuse merge: episodes %d and %d both have watch history", e.id, target)
		}
	}

	// 备份与改名工具同一套保证：先备份成功，再写库。
	backup, err := backupNameDatabase(ctx, abs, backupPath)
	if err != nil {
		return result, err
	}
	result.Backup = backup

	for _, e := range fromEps {
		target, exists := toEps[e.no]
		if !exists {
			if _, err := tx.ExecContext(ctx, `UPDATE episodes SET show_id=? WHERE id=?`, toID, e.id); err != nil {
				return result, err
			}
			result.MovedEpisodes++
			continue
		}
		if r, err := tx.ExecContext(ctx, `UPDATE history SET episode_id=? WHERE episode_id=?`, target, e.id); err != nil {
			return result, err
		} else if n, _ := r.RowsAffected(); n > 0 {
			result.MovedHistories += int(n)
		}
		r, err := tx.ExecContext(ctx, `UPDATE sources SET episode_id=? WHERE episode_id=?`, target, e.id)
		if err != nil {
			return result, err
		}
		if n, _ := r.RowsAffected(); n > 0 {
			result.MovedSources += int(n)
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM episodes WHERE id=?`, e.id); err != nil {
			return result, err
		}
		result.MergedEpisodes++
	}

	// 图片：唯一键是 doc_id，两边不会重复；仍用 OR IGNORE 兜底，冲突的留在原剧随后删除。
	if r, err := tx.ExecContext(ctx, `UPDATE OR IGNORE show_photos SET show_id=? WHERE show_id=?`, toID, fromID); err != nil {
		return result, err
	} else if n, _ := r.RowsAffected(); n > 0 {
		result.MovedPhotos = int(n)
	}
	var leftPhotos int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM show_photos WHERE show_id=?`, fromID).Scan(&leftPhotos); err != nil {
		return result, err
	}
	if leftPhotos > 0 {
		if _, err := tx.ExecContext(ctx, `DELETE FROM show_photos WHERE show_id=?`, fromID); err != nil {
			return result, err
		}
		result.DroppedPhotos = leftPhotos
	}

	if r, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO favorites(show_id, created_at, base_ep)
SELECT ?, created_at, base_ep FROM favorites WHERE show_id=?`, toID, fromID); err != nil {
		return result, err
	} else if n, _ := r.RowsAffected(); n > 0 {
		result.MovedFavorites = int(n)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM favorites WHERE show_id=?`, fromID); err != nil {
		return result, err
	}
	if r, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO show_tags(show_id, tag_id)
SELECT ?, tag_id FROM show_tags WHERE show_id=?`, toID, fromID); err != nil {
		return result, err
	} else if n, _ := r.RowsAffected(); n > 0 {
		result.MovedTags = int(n)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM show_tags WHERE show_id=?`, fromID); err != nil {
		return result, err
	}
	if toCover == "" && fromCover != "" {
		if _, err := tx.ExecContext(ctx, `UPDATE shows SET cover_path=? WHERE id=?`, fromCover, toID); err != nil {
			return result, err
		}
		result.CoverTransferred = true
	}

	// 删除前确认原剧已无任何引用，避免把孤儿数据留在库里。
	var refs int
	if err := tx.QueryRowContext(ctx, `
SELECT (SELECT COUNT(*) FROM episodes WHERE show_id=?) +
       (SELECT COUNT(*) FROM show_photos WHERE show_id=?) +
       (SELECT COUNT(*) FROM show_tags WHERE show_id=?) +
       (SELECT COUNT(*) FROM favorites WHERE show_id=?)`, fromID, fromID, fromID, fromID).Scan(&refs); err != nil {
		return result, err
	}
	if refs != 0 {
		return result, fmt.Errorf("refuse merge: show %d still has %d dependent rows", fromID, refs)
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM shows WHERE id=?`, fromID); err != nil {
		return result, err
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	return result, nil
}
