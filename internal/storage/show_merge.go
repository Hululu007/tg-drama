package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"tgdrama/internal/parser"
)

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
	tx, err := db.BeginTx(ctx, nil)
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
