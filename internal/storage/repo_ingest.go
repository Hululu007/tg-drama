package storage

import (
	"database/sql"
	"strings"
)

type PendingSource struct {
	ShowName  string
	NormName  string
	Intro     string
	Tags      []string
	EpisodeNo int
	Source    Source

	Variants []SourceVariant
	Raw      RawRecord
}

type RawRecord struct {
	MessageID int
	GroupedID int64
	Text      string
	Info      string
}

type IngestResult struct {
	Inserted int
	SourceID []int64
	NewShows map[string]int64
}

func (d *DB) IngestBatch(chatPK int64, items []PendingSource, raws []RawRecord) (*IngestResult, error) {
	res := &IngestResult{
		SourceID: make([]int64, len(items)),
		NewShows: map[string]int64{},
	}
	if len(items) == 0 && len(raws) == 0 {
		return res, nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	insSrc, err := tx.Prepare(`
INSERT OR IGNORE INTO sources (
    episode_id, chat_id, message_id, grouped_id, segment_index,
    file_name, file_size, duration, width, height, mime,
    doc_id, access_hash, file_reference, caption, download_status, created_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`)
	if err != nil {
		return nil, err
	}
	defer insSrc.Close()

	selMsgSrc, err := tx.Prepare(`SELECT id FROM sources WHERE chat_id = ? AND message_id = ? LIMIT 1`)
	if err != nil {
		return nil, err
	}
	defer selMsgSrc.Close()
	selSeg, err := tx.Prepare(`SELECT COUNT(*) FROM sources WHERE episode_id = ? AND chat_id = ?`)
	if err != nil {
		return nil, err
	}
	defer selSeg.Close()
	insTag, err := tx.Prepare(`INSERT INTO tags (name) VALUES (?) ON CONFLICT(name) DO NOTHING`)
	if err != nil {
		return nil, err
	}
	defer insTag.Close()
	selTag, err := tx.Prepare(`SELECT id FROM tags WHERE name = ?`)
	if err != nil {
		return nil, err
	}
	defer selTag.Close()
	insShowTag, err := tx.Prepare(`INSERT OR IGNORE INTO show_tags (show_id, tag_id) VALUES (?, ?)`)
	if err != nil {
		return nil, err
	}
	defer insShowTag.Close()
	updIntro, err := tx.Prepare(`UPDATE shows SET intro = ? WHERE id = ? AND intro = ''`)
	if err != nil {
		return nil, err
	}
	defer updIntro.Close()

	showIDs := make(map[string]int64, len(items))
	epIDs := make(map[[2]int64]int64, len(items))
	segCount := make(map[int64]int, len(items))
	tagIDs := make(map[string]int64, 32)

	for i := range items {
		it := &items[i]
		showID, ok := showIDs[it.NormName]
		if !ok {
			id, inserted, uerr := upsertShow(tx, it.ShowName, it.NormName)
			if uerr != nil {
				return nil, uerr
			}
			showIDs[it.NormName] = id
			showID = id
			if inserted {
				res.NewShows[it.NormName] = id
			}
		}
		epKey := [2]int64{showID, int64(it.EpisodeNo)}
		epID, ok := epIDs[epKey]
		if !ok {
			id, _, uerr := upsertEpisode(tx, showID, it.EpisodeNo)
			if uerr != nil {
				return nil, uerr
			}
			epIDs[epKey] = id
			epID = id
		}
		var existSrc int64
		switch qerr := selMsgSrc.QueryRow(chatPK, it.Source.MessageID).Scan(&existSrc); qerr {
		case nil:

			res.SourceID[i] = existSrc
			if len(it.Variants) > 0 {
				if verr := ReplaceSourceVariants(tx, existSrc, it.Variants); verr != nil {
					return nil, verr
				}
				if _, verr := tx.Exec(`UPDATE sources SET variants_checked = 1 WHERE id = ?`, existSrc); verr != nil {
					return nil, verr
				}
			}
			continue
		case sql.ErrNoRows:
		default:
			return nil, qerr
		}

		seg, ok := segCount[epID]
		if !ok {
			if qerr := selSeg.QueryRow(epID, chatPK).Scan(&seg); qerr != nil {
				return nil, qerr
			}
		}
		segCount[epID] = seg + 1

		s := it.Source
		r, ierr := insSrc.Exec(epID, chatPK, s.MessageID, s.GroupedID, seg,
			s.FileName, s.FileSize, s.Duration, s.Width, s.Height, s.Mime,
			s.DocID, s.AccessHash, s.FileReference, it.Raw.Text, nowUnix())
		if ierr != nil {
			return nil, ierr
		}
		if n, _ := r.RowsAffected(); n == 0 {

			continue
		}
		id, _ := r.LastInsertId()
		res.SourceID[i] = id
		res.Inserted++
		if len(it.Variants) > 0 {
			if verr := ReplaceSourceVariants(tx, id, it.Variants); verr != nil {
				return nil, verr
			}

			if _, verr := tx.Exec(`UPDATE sources SET variants_checked = 1 WHERE id = ?`, id); verr != nil {
				return nil, verr
			}
		}

		for _, tag := range it.Tags {
			tag = strings.TrimSpace(tag)
			if tag == "" {
				continue
			}
			tid, ok := tagIDs[tag]
			if !ok {
				if _, terr := insTag.Exec(tag); terr != nil {
					return nil, terr
				}
				if qerr := selTag.QueryRow(tag).Scan(&tid); qerr != nil {
					return nil, qerr
				}
				tagIDs[tag] = tid
			}
			if _, terr := insShowTag.Exec(showID, tid); terr != nil {
				return nil, terr
			}
		}
		if it.Intro != "" {
			if _, ierr := updIntro.Exec(it.Intro, showID); ierr != nil {
				return nil, ierr
			}
		}
	}
	if err := stashPending(tx, chatPK, items, raws); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return res, nil
}

func stashPending(tx *sql.Tx, chatPK int64, items []PendingSource, raws []RawRecord) error {
	if len(items) > 0 {
		ph := strings.TrimSuffix(strings.Repeat("?,", len(items)), ",")
		args := make([]any, 0, len(items)+1)
		args = append(args, chatPK)
		for i := range items {
			args = append(args, items[i].Source.MessageID)
		}
		if _, err := tx.Exec(
			`DELETE FROM pending_msgs WHERE chat_id = ? AND message_id IN (`+ph+`)`, args...); err != nil {
			return err
		}
	}
	if len(raws) == 0 {
		return nil
	}
	insStmt, err := tx.Prepare(`
INSERT INTO pending_msgs (chat_id, message_id, grouped_id, caption, info_json, created_at)
VALUES (?, ?, ?, ?, ?, ?)
ON CONFLICT(chat_id, message_id) DO UPDATE SET
    caption = CASE WHEN pending_msgs.caption = '' THEN excluded.caption ELSE pending_msgs.caption END`)
	if err != nil {
		return err
	}
	defer insStmt.Close()
	now := nowUnix()
	for _, r := range raws {
		if r.MessageID <= 0 || r.GroupedID == 0 || strings.TrimSpace(r.Info) == "" {
			continue
		}
		if _, err := insStmt.Exec(chatPK, r.MessageID, r.GroupedID, r.Text, r.Info, now); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) HasPendingMessage(chatPK int64, messageID int) bool {
	var n int
	_ = d.db.QueryRow(`SELECT COUNT(*) FROM pending_msgs WHERE chat_id = ? AND message_id = ?`,
		chatPK, messageID).Scan(&n)
	return n > 0
}

func (d *DB) ExistingRawMessages(chatPK int64, ids []int) (map[int]bool, error) {
	out := make(map[int]bool, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	const chunk = 400
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")
		args := make([]any, 0, len(part)*2+2)
		args = append(args, chatPK)
		for _, id := range part {
			args = append(args, id)
		}
		args = append(args, chatPK)
		for _, id := range part {
			args = append(args, id)
		}
		rows, err := d.db.Query(`SELECT message_id FROM sources WHERE chat_id = ? AND message_id IN (`+ph+
			`) UNION SELECT message_id FROM show_photos WHERE chat_id = ? AND message_id IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = true
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *DB) RawMessageTexts(chatPK int64, ids []int) (map[int]string, error) {
	out := make(map[int]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	const chunk = 400
	for start := 0; start < len(ids); start += chunk {
		end := start + chunk
		if end > len(ids) {
			end = len(ids)
		}
		part := ids[start:end]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")
		args := make([]any, 0, len(part)*2+2)
		args = append(args, chatPK)
		for _, id := range part {
			args = append(args, id)
		}
		args = append(args, chatPK)
		for _, id := range part {
			args = append(args, id)
		}
		rows, err := d.db.Query(`SELECT message_id, caption FROM sources WHERE chat_id = ? AND message_id IN (`+ph+
			`) UNION ALL SELECT message_id, caption FROM show_photos WHERE chat_id = ? AND message_id IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id int
			var text string
			if err := rows.Scan(&id, &text); err != nil {
				rows.Close()
				return nil, err
			}
			if out[id] == "" {
				out[id] = text
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}
	return out, nil
}

func (d *DB) GroupSourceMessages(chatPK int64, groupedIDs []int64) (map[int64][]int, error) {
	out := make(map[int64][]int, len(groupedIDs))
	const chunk = 400
	for start := 0; start < len(groupedIDs); start += chunk {
		end := start + chunk
		if end > len(groupedIDs) {
			end = len(groupedIDs)
		}
		part := groupedIDs[start:end]
		args := make([]any, 0, len(part)+1)
		args = append(args, chatPK)
		for _, g := range part {
			args = append(args, g)
		}
		rows, err := d.db.Query(`SELECT grouped_id, message_id FROM sources WHERE chat_id = ? AND grouped_id IN (`+
			strings.TrimSuffix(strings.Repeat("?,", len(part)), ",")+`) ORDER BY message_id`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var gid int64
			var msgID int
			if err := rows.Scan(&gid, &msgID); err != nil {
				rows.Close()
				return nil, err
			}
			out[gid] = append(out[gid], msgID)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func (d *DB) AllShowNorms() (map[string]string, error) {
	rows, err := d.db.Query(`SELECT norm_name, name FROM shows`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string, 2048)
	for rows.Next() {
		var norm, name string
		if err := rows.Scan(&norm, &name); err != nil {
			return nil, err
		}
		out[norm] = name
	}
	return out, rows.Err()
}

func (d *DB) AllShowIDs() (map[string]int64, error) {
	rows, err := d.db.Query(`SELECT norm_name, id FROM shows`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int64, 2048)
	for rows.Next() {
		var norm string
		var id int64
		if err := rows.Scan(&norm, &id); err != nil {
			return nil, err
		}
		out[norm] = id
	}
	return out, rows.Err()
}
