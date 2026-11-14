package storage

import (
	"database/sql"
	"errors"
	"sort"
	"strings"

	"tgdrama/internal/parser"
)

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

type GroupSource struct {
	ID        int64
	MessageID int
	EpisodeID int64
}

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

type ShowDocAnchor struct {
	ChatPK    int64
	MessageID int
}

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

const playableCond = `
  EXISTS (
    SELECT 1 FROM episodes e2
    WHERE e2.show_id = s.id AND e2.episode_no >= 1
      AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = e2.id` + liveSrcCond + `)
)`

const playableShowWhere = `
WHERE s.hidden = 0 AND ` + playableCond

const searchShowWhere = `
WHERE ` + playableCond

const singleNoCoverCond = `
  AND NOT (s.cover_path = '' AND 1 = (SELECT COUNT(DISTINCT e3.episode_no) FROM episodes e3
       WHERE e3.show_id = s.id AND e3.episode_no >= 1
         AND EXISTS (SELECT 1 FROM sources src WHERE src.episode_id = e3.id` + liveSrcCond + `)))`

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

func showOrderBy(sortBy string) string {
	if sortBy == "episodes" {
		return ` ORDER BY ep_count DESC, s.updated_at DESC`
	}
	return ` ORDER BY s.updated_at DESC`
}

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

func (d *DB) CountShows() (int64, error) {
	var n int64
	err := d.db.QueryRow(`SELECT COUNT(*) FROM shows s` + listShowWhere).Scan(&n)
	return n, err
}

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

		_, _ = q.Exec(`UPDATE shows SET updated_at = ? WHERE id = ?`, nowUnix(), showID)
		id, ierr = res.LastInsertId()
		return id, true, ierr
	default:
		return 0, false, err
	}
}

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

func (d *DB) ResetDownloading() error {
	_, err := d.db.Exec(`UPDATE sources SET download_status = 'pending' WHERE download_status = 'downloading'`)
	return err
}

func (d *DB) RequestEpisodePriority(episodeID int64) error {
	_, err := d.db.Exec(`
UPDATE sources SET priority = 1,
    download_status = CASE WHEN download_status = 'downloading' THEN 'pending' ELSE download_status END
WHERE episode_id = ?`, episodeID)
	return err
}

func (d *DB) ResetMediaDownloads() error {
	_, err := d.db.Exec(`
UPDATE sources SET download_status = 'pending', file_path = '', priority = 0
WHERE download_status IN ('done', 'downloading')`)
	return err
}

func (d *DB) UpdateSourceLocation(sourceID, docID, accessHash int64, fileReference []byte) error {
	_, err := d.db.Exec(`
UPDATE sources SET doc_id = ?, access_hash = ?, file_reference = ?
WHERE id = ?`, docID, accessHash, fileReference, sourceID)
	return err
}

func (d *DB) SetSourceTestSizeForTest(sourceID, size int64) error {
	_, err := d.db.Exec(`UPDATE sources SET file_size = ? WHERE id = ?`, size, sourceID)
	return err
}

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

func (d *DB) MarkSourceDone(id int64, filePath string, size int64) error {
	_, err := d.db.Exec(`
UPDATE sources SET download_status = 'done', file_path = ?, file_size = ?, error = '' WHERE id = ?`,
		filePath, size, id)
	return err
}

func (d *DB) MarkSourceFailed(id int64, reason string) error {
	_, err := d.db.Exec(`UPDATE sources SET download_status = 'failed', error = ? WHERE id = ?`,
		reason, id)
	return err
}

func (d *DB) SetShowCover(showID int64, coverPath string) error {
	_, err := d.db.Exec(`UPDATE shows SET cover_path = ? WHERE id = ? AND cover_path = ''`,
		coverPath, showID)
	return err
}

func (d *DB) SetShowIntro(showID int64, intro string) error {
	return setShowIntro(d.db, showID, intro)
}

func setShowIntro(q execQuerier, showID int64, intro string) error {
	_, err := q.Exec(`UPDATE shows SET intro = ? WHERE id = ? AND intro = ''`,
		intro, showID)
	return err
}

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

func (d *DB) CountSourcesByGrouped(chatPK int64, groupedID int64) int {
	var n int
	_ = d.db.QueryRow(`
SELECT COUNT(*) FROM sources WHERE chat_id = ? AND grouped_id = ?`,
		chatPK, groupedID).Scan(&n)
	return n
}

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

func linePlayScore(l *PlayLine) (float64, int64) {
	var dur float64
	var size int64
	for i := range l.Segments {
		dur += l.Segments[i].Duration
		size += l.Segments[i].FileSize
	}
	return dur, size
}

func (d *DB) ListEpisodeLines(episodeID int64) ([]*PlayLine, error) {

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

var socialPostKeywords = []string{"推特", "twitter", "onlyfans", "福利姬"}

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

	titled, container, err := d.identifiedShows()
	if err != nil {
		return 0, err
	}
	social, err := d.listSocialOnlyShows()
	if err != nil {
		return 0, err
	}

	for _, id := range social {
		if !container[id] {
			junk = append(junk, id)
		}
	}

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

		if got.Container {
			container[id] = true
		}
	}
	return titled, container, rows.Err()
}

func (d *DB) CountHiddenShows() int64 {
	var n int64
	_ = d.db.QueryRow(`SELECT COUNT(*) FROM shows WHERE hidden = 1`).Scan(&n)
	return n
}
