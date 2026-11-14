package storage

import (
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
)

type ShowPhoto struct {
	ID     int64 `json:"id"`
	ShowID int64 `json:"show_id"`
	Width  int   `json:"width"`
	Height int   `json:"height"`

	EpisodeNo int `json:"episode_no,omitempty"`
}

func (d *DB) UpsertShowPhoto(p *ShowPhoto, docID, accessHash int64, fileRef []byte,
	chatID, messageID, groupedID int64, caption string) (id int64, inserted bool, err error) {
	var existing int64
	err = d.db.QueryRow(`SELECT id FROM show_photos WHERE doc_id = ?`, docID).Scan(&existing)
	switch {
	case err == nil:
		return existing, false, nil
	case !errors.Is(err, sql.ErrNoRows):
		return 0, false, err
	}
	res, ierr := d.db.Exec(`
INSERT INTO show_photos (show_id, chat_id, message_id, doc_id, access_hash, file_reference,
    width, height, grouped_id, caption, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ShowID, chatID, messageID, docID, accessHash, fileRef,
		p.Width, p.Height, groupedID, caption, nowUnix())
	if ierr != nil {
		return 0, false, ierr
	}
	id, ierr = res.LastInsertId()
	return id, true, ierr
}

const primaryPhotoChatSub = `(
    SELECT q.chat_id FROM show_photos q
    JOIN chats c ON c.id = q.chat_id AND c.enabled = 1
    WHERE q.show_id = ?
    GROUP BY q.chat_id
    ORDER BY COUNT(*) DESC, q.chat_id
    LIMIT 1)`

const (
	photoMaxGap = 400

	photoLatestEps = 3

	photoPerEpisodeMax = 6

	photoMaxTotal = 18
)

func (d *DB) ListShowPhotos(showID int64) ([]*ShowPhoto, error) {
	type cand struct {
		p      ShowPhoto
		chatID int64
		msgID  int
		gap    int
	}
	rows, err := d.db.Query(`
SELECT id, show_id, width, height, chat_id, message_id FROM show_photos
WHERE show_id = ? AND chat_id = `+primaryPhotoChatSub+`
ORDER BY id`, showID, showID)
	if err != nil {
		return nil, err
	}
	var cands []cand
	for rows.Next() {
		var c cand
		if err := rows.Scan(&c.p.ID, &c.p.ShowID, &c.p.Width, &c.p.Height, &c.chatID, &c.msgID); err != nil {
			rows.Close()
			return nil, err
		}
		c.gap = -1
		cands = append(cands, c)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(cands) == 0 {
		return nil, nil
	}

	type msgEp struct {
		chatID int64
		msgID  int
		ep     int
	}
	var msgs []msgEp
	mrows, err := d.db.Query(`
SELECT s.chat_id, s.message_id, e.episode_no
FROM sources s JOIN episodes e ON e.id = s.episode_id
WHERE e.show_id = ? AND e.episode_no >= 1`, showID)
	if err != nil {
		return nil, err
	}
	for mrows.Next() {
		var m msgEp
		if err := mrows.Scan(&m.chatID, &m.msgID, &m.ep); err != nil {
			mrows.Close()
			return nil, err
		}
		msgs = append(msgs, m)
	}
	mrows.Close()
	if err := mrows.Err(); err != nil {
		return nil, err
	}

	known := false
	for i := range cands {
		best, bestEp := photoMaxGap+1, 0
		for _, m := range msgs {
			if m.chatID != cands[i].chatID {
				continue
			}
			gap := cands[i].msgID - m.msgID
			if gap < 0 {
				gap = -gap
			}
			if gap < best {
				best, bestEp = gap, m.ep
			}
		}
		if bestEp > 0 && best <= photoMaxGap {
			cands[i].gap, cands[i].p.EpisodeNo = best, bestEp
			known = true
		}
	}

	if !known {
		out := make([]*ShowPhoto, 0, len(cands))
		for i := range cands {
			if len(out) >= photoMaxTotal {
				break
			}
			out = append(out, &cands[i].p)
		}
		return out, nil
	}

	eps := make([]int, 0, photoLatestEps*2)
	seen := map[int]bool{}
	for i := range cands {
		ep := cands[i].p.EpisodeNo
		if ep > 0 && !seen[ep] {
			seen[ep] = true
			eps = append(eps, ep)
		}
	}
	sort.Sort(sort.Reverse(sort.IntSlice(eps)))
	if len(eps) > photoLatestEps {
		eps = eps[:photoLatestEps]
	}
	keep := make(map[int]bool, len(eps))
	for _, ep := range eps {
		keep[ep] = true
	}

	sort.SliceStable(cands, func(i, j int) bool {
		ei, ej := cands[i].p.EpisodeNo, cands[j].p.EpisodeNo
		if ei != ej {
			return ei < ej
		}
		return cands[i].gap < cands[j].gap
	})
	out := make([]*ShowPhoto, 0, len(cands))
	perEp := map[int]int{}
	for i := range cands {
		ep := cands[i].p.EpisodeNo
		if ep == 0 || !keep[ep] {
			continue
		}
		if perEp[ep] >= photoPerEpisodeMax {
			continue
		}
		if len(out) >= photoMaxTotal {
			break
		}
		perEp[ep]++
		out = append(out, &cands[i].p)
	}
	return out, nil
}

type PhotoLoc struct {
	ID        int64
	DocID     int64
	ChatID    int64
	MessageID int
	CachePath string
}

func (d *DB) ListShowPhotoLocs(showID int64) ([]PhotoLoc, error) {
	shown, err := d.ListShowPhotos(showID)
	if err != nil {
		return nil, err
	}
	if len(shown) == 0 {
		return nil, nil
	}
	ids := make([]string, 0, len(shown))
	for _, p := range shown {
		ids = append(ids, strconv.FormatInt(p.ID, 10))
	}
	rows, err := d.db.Query(`
SELECT id, doc_id, chat_id, message_id, cache_path FROM show_photos
WHERE id IN (` + strings.Join(ids, ",") + `)
ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PhotoLoc
	for rows.Next() {
		var p PhotoLoc
		if err := rows.Scan(&p.ID, &p.DocID, &p.ChatID, &p.MessageID, &p.CachePath); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) GetShowBrief(showID int64) (name, cover string, err error) {
	err = d.db.QueryRow(`SELECT name, cover_path FROM shows WHERE id = ?`, showID).
		Scan(&name, &cover)
	return name, cover, err
}

func (d *DB) GetShowPhotoPath(photoID int64) (string, error) {
	var path string
	err := d.db.QueryRow(
		`SELECT cache_path FROM show_photos WHERE id = ?`, photoID).Scan(&path)
	if err != nil {
		return "", err
	}
	return path, nil
}

type PhotoLocation struct {
	ID            int64
	ShowID        int64
	ChatID        int64
	MessageID     int64
	DocID         int64
	AccessHash    int64
	FileReference []byte
}

func (d *DB) GetPhotoLocation(photoID int64) (*PhotoLocation, error) {
	var p PhotoLocation
	err := d.db.QueryRow(`
SELECT id, show_id, chat_id, message_id, doc_id, access_hash, file_reference
FROM show_photos WHERE id = ?`, photoID).Scan(
		&p.ID, &p.ShowID, &p.ChatID, &p.MessageID, &p.DocID, &p.AccessHash, &p.FileReference)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (d *DB) ResetPhotoCache(prefix string) error {
	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`UPDATE show_photos SET cache_path = '' WHERE cache_path <> ''`); err != nil {
		return err
	}
	if prefix != "" {
		if _, err := tx.Exec(
			`UPDATE shows SET cover_path = '', updated_at = ?
			 WHERE cover_path <> '' AND substr(cover_path, 1, ?) = ?`,
			nowUnix(), len(prefix), prefix); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (d *DB) SetPhotoCache(docID int64, path string) error {
	_, err := d.db.Exec(`UPDATE show_photos SET cache_path = ? WHERE doc_id = ? AND cache_path = ''`,
		path, docID)
	return err
}

func (d *DB) FindShowByGrouped(chatID, groupedID int64) int64 {
	var showID int64
	_ = d.db.QueryRow(`
SELECT e.show_id FROM sources s JOIN episodes e ON e.id = s.episode_id
WHERE s.chat_id = ? AND s.grouped_id = ? LIMIT 1`, chatID, groupedID).Scan(&showID)
	return showID
}

func (d *DB) CountSourcesByEpisode(chatID, episodeID int64) int {
	var n int
	_ = d.db.QueryRow(`
SELECT COUNT(*) FROM sources WHERE episode_id = ? AND chat_id = ?`,
		episodeID, chatID).Scan(&n)
	return n
}
