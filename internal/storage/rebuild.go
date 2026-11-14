package storage

import (
	"encoding/base64"
	"encoding/json"
	"log"
	"sort"
	"strings"
	"time"

	"tgdrama/internal/parser"
)

const rebuildVersion = "43"

type rawDoc struct {
	ID            int64  `json:"ID"`
	AccessHash    int64  `json:"AccessHash"`
	FileReference string `json:"FileReference"`
	Size          int64  `json:"Size"`
	MimeType      string `json:"MimeType"`
	Attributes    []struct {
		Duration float64 `json:"Duration"`
		W        int     `json:"W"`
		H        int     `json:"H"`
		FileName string  `json:"FileName"`
	} `json:"Attributes"`
}

func (d *DB) RebuildFromRaw(p *parser.Parser) error {
	done, _, err := d.GetKV("rebuild_v" + rebuildVersion)
	if err != nil {
		return err
	}
	if done == "1" {
		return nil
	}
	log.Printf("开始按新解析规则重建媒体库映射（v%s）…", rebuildVersion)
	loadStart := time.Now()

	type msgKey struct {
		chat int64
		msg  int
	}
	type gKey struct {
		chat, grouped int64
	}
	texts := map[msgKey]string{}
	groupText := map[gKey]string{}
	rows, err := d.db.Query(`
SELECT chat_id, message_id, grouped_id, caption FROM sources WHERE caption <> ''
UNION ALL
SELECT chat_id, message_id, grouped_id, caption FROM show_photos WHERE caption <> ''
ORDER BY 1, 2`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var k msgKey
		var grouped int64
		var text string
		if err := rows.Scan(&k.chat, &k.msg, &grouped, &text); err == nil {
			texts[k] = text
			if grouped != 0 && strings.TrimSpace(text) != "" {
				gk := gKey{k.chat, grouped}
				if groupText[gk] == "" {
					groupText[gk] = text
				}
			}
		}
	}
	rows.Close()

	type srcRow struct {
		id, episodeID, chatID, groupedID int64
		messageID                        int
		fileName                         string

		revived    bool
		fileSize   int64
		duration   float64
		width      int
		height     int
		mime       string
		docID      int64
		accessHash int64
		fileRef    []byte
	}
	rows, err = d.db.Query(`
SELECT id, episode_id, chat_id, grouped_id, message_id, file_name
FROM sources ORDER BY chat_id, message_id, id`)
	if err != nil {
		return err
	}
	var srcs []srcRow
	for rows.Next() {
		var s srcRow
		if err := rows.Scan(&s.id, &s.episodeID, &s.chatID, &s.groupedID,
			&s.messageID, &s.fileName); err != nil {
			rows.Close()
			return err
		}
		srcs = append(srcs, s)
	}
	rows.Close()

	revived := 0
	rows, err = d.db.Query(`
SELECT p.chat_id, p.message_id, p.grouped_id, p.info_json
FROM pending_msgs p
WHERE p.info_json LIKE '%"MimeType":"video/%'
  AND NOT EXISTS (SELECT 1 FROM sources s
                  WHERE s.chat_id = p.chat_id AND s.message_id = p.message_id)
ORDER BY p.chat_id, p.message_id`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var chatID, groupedID int64
		var msgID int
		var info string
		if err := rows.Scan(&chatID, &msgID, &groupedID, &info); err != nil {
			continue
		}
		var doc rawDoc
		if json.Unmarshal([]byte(info), &doc) != nil || doc.ID == 0 {
			continue
		}
		s := srcRow{chatID: chatID, messageID: msgID, groupedID: groupedID,
			revived: true, fileSize: doc.Size, mime: doc.MimeType,
			docID: doc.ID, accessHash: doc.AccessHash}
		for _, a := range doc.Attributes {
			if a.Duration > 0 {
				s.duration = a.Duration
			}
			if a.W > 0 && a.H > 0 {
				s.width, s.height = a.W, a.H
			}
			if a.FileName != "" {
				s.fileName = a.FileName
			}
		}
		if s.fileName == "" {

			continue
		}
		if doc.FileReference != "" {
			if raw, derr := base64.StdEncoding.DecodeString(doc.FileReference); derr == nil {
				s.fileRef = raw
			}
		}
		srcs = append(srcs, s)
		revived++
	}
	rows.Close()
	sort.SliceStable(srcs, func(i, j int) bool {
		if srcs[i].chatID != srcs[j].chatID {
			return srcs[i].chatID < srcs[j].chatID
		}
		if srcs[i].messageID != srcs[j].messageID {
			return srcs[i].messageID < srcs[j].messageID
		}
		return srcs[i].id < srcs[j].id
	})

	log.Printf("载入原文与来源 %d 条（含待复活视频 %d 条，耗时 %s）",
		len(srcs), revived, time.Since(loadStart).Round(time.Millisecond))

	type candidate struct {
		src         srcRow
		text        string
		fromCaption bool
		showName    string
		norm        string
		epNo        int
		parsed      *parser.Parsed
		dropped     bool
	}
	parseStart := time.Now()
	groupedCount := map[gKey]int{}
	groupSize := map[gKey]int{}
	for _, s := range srcs {
		if s.groupedID != 0 {
			groupSize[gKey{s.chatID, s.groupedID}]++
		}
	}
	albums := parser.NewAlbumResolver()
	cands := make([]candidate, 0, len(srcs))
	var dropped []int64

	for _, s := range srcs {
		text := texts[msgKey{s.chatID, s.messageID}]
		fromCaption := strings.TrimSpace(text) != ""

		if !fromCaption && s.groupedID != 0 {
			if gt := groupText[gKey{s.chatID, s.groupedID}]; gt != "" {
				text, fromCaption = gt, true
			}
		}
		c := candidate{src: s, text: text, fromCaption: fromCaption}
		if pp, perr := p.Parse(text, s.fileName); perr == nil && pp.NormName != "" {
			c.showName, c.norm, c.parsed = pp.ShowName, pp.NormName, pp
		}

		if c.norm != "" && parser.IsJunkName(c.showName) {
			if s.id != 0 {
				dropped = append(dropped, s.id)
			}
			continue
		}
		if c.norm == "" {

			cleaned := parser.NameFromFileName(s.fileName)
			if parser.IsJunkName(cleaned) || parser.IsJunkShowName(cleaned) {
				if s.id != 0 {
					dropped = append(dropped, s.id)
				}
				continue
			}
			c.showName, c.norm = cleaned, parser.Normalize(cleaned)
			if c.norm == "" {
				if s.id != 0 {
					dropped = append(dropped, s.id)
				}
				continue
			}
		}

		k := gKey{s.chatID, s.groupedID}
		c.epNo = albums.ResolveSized(s.chatID, s.groupedID, c.parsed, fromCaption, s.fileName, groupedCount[k], groupSize[k])
		if s.groupedID != 0 {
			groupedCount[k]++
		}
		cands = append(cands, c)
	}

	multiApplied := 0
	{
		groupIdx := map[gKey][]int{}
		for i := range cands {
			if cands[i].src.groupedID != 0 {
				k := gKey{cands[i].src.chatID, cands[i].src.groupedID}
				groupIdx[k] = append(groupIdx[k], i)
			}
		}
		for _, idxs := range groupIdx {
			var multi []parser.MultiEntry
			for _, i := range idxs {
				if cands[i].parsed != nil && len(cands[i].parsed.Multi) >= 2 {
					multi = cands[i].parsed.Multi
					break
				}
			}
			if len(multi) < 2 || len(idxs) != len(multi) {
				continue
			}
			for pos, i := range idxs {
				m := multi[pos]
				n := parser.Normalize(m.Name)
				if n == "" {
					continue
				}
				cands[i].showName, cands[i].norm = m.Name, n
				if m.Ep > 0 {
					cands[i].epNo = m.Ep
				}
				multiApplied++
			}
		}
	}

	canon := map[string]string{}
	for _, c := range cands {
		if c.parsed != nil && c.parsed.HasTitle && c.norm != "" {
			if _, ok := canon[c.norm]; !ok {
				canon[c.norm] = c.showName
			}
		}
	}
	tagMatched := 0

	for i := range cands {
		if canon, ok := manualShowAliases[cands[i].showName]; ok {
			cands[i].showName, cands[i].norm = canon, parser.Normalize(canon)
		}
	}
	for i := range cands {
		c := &cands[i]
		if c.parsed == nil || c.parsed.HasTitle || len(c.parsed.Tags) == 0 || c.norm == "" {
			continue
		}
		for _, tg := range c.parsed.Tags {
			tn := parser.Normalize(tg)
			if tn == "" || tn == c.norm {
				continue
			}
			if name, ok := canon[tn]; ok {
				c.showName, c.norm = name, tn
				tagMatched++
				break
			}
		}
	}
	log.Printf("重建解析完成：%d/%d 条来源（解析耗时 %s），丢弃 %d 条垃圾来源，标签辅助归并 %d 条，多标题预览分配 %d 条",
		len(cands), len(srcs), time.Since(parseStart).Round(time.Millisecond), len(dropped), tagMatched, multiApplied)

	groupedShow := map[gKey]int64{}
	renamed := 0
	revivedBack := 0

	writeStart := time.Now()

	showIDByNorm := map[string]int64{}
	showNameByID := map[int64]string{}
	showHasIntro := map[int64]bool{}
	if srows, serr := d.db.Query(`SELECT id, name, norm_name, intro FROM shows`); serr == nil {
		for srows.Next() {
			var id int64
			var name, norm, intro string
			if err := srows.Scan(&id, &name, &norm, &intro); err == nil {
				showIDByNorm[norm] = id
				showNameByID[id] = name
				if strings.TrimSpace(intro) != "" {
					showHasIntro[id] = true
				}
			}
		}
		srows.Close()
	}
	epIDByKey := map[[2]int64]int64{}
	if erows, eerr := d.db.Query(`SELECT id, show_id, episode_no FROM episodes`); eerr == nil {
		for erows.Next() {
			var id, showID int64
			var epNo int
			if err := erows.Scan(&id, &showID, &epNo); err == nil {
				epIDByKey[[2]int64{showID, int64(epNo)}] = id
			}
		}
		erows.Close()
	}
	tagIDByName := map[string]int64{}
	if trows, terr := d.db.Query(`SELECT id, name FROM tags`); terr == nil {
		for trows.Next() {
			var id int64
			var name string
			if err := trows.Scan(&id, &name); err == nil {
				tagIDByName[name] = id
			}
		}
		trows.Close()
	}

	tx, err := d.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM show_tags`); err != nil {
		return err
	}

	if _, err := tx.Exec(`UPDATE sources SET segment_index = -id`); err != nil {
		return err
	}
	delSrc, err := tx.Prepare(`DELETE FROM sources WHERE id = ?`)
	if err != nil {
		return err
	}
	defer delSrc.Close()
	insShow, err := tx.Prepare(`
INSERT INTO shows (name, norm_name, status, created_at, updated_at)
VALUES (?, ?, '连载中', ?, ?)`)
	if err != nil {
		return err
	}
	defer insShow.Close()
	renShow, err := tx.Prepare(`UPDATE shows SET name = ?, updated_at = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer renShow.Close()
	insEp, err := tx.Prepare(`
INSERT INTO episodes (show_id, episode_no, created_at, updated_at)
VALUES (?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insEp.Close()
	touchShow, err := tx.Prepare(`UPDATE shows SET updated_at = ? WHERE id = ?`)
	if err != nil {
		return err
	}
	defer touchShow.Close()
	insSrc, err := tx.Prepare(`
INSERT INTO sources (episode_id, chat_id, message_id, grouped_id, segment_index,
    file_name, file_size, duration, width, height, mime, doc_id, access_hash,
    file_reference, caption, download_status, created_at)
VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, 'pending', ?)`)
	if err != nil {
		return err
	}
	defer insSrc.Close()
	insTag, err := tx.Prepare(`INSERT INTO tags (name) VALUES (?) ON CONFLICT(name) DO NOTHING`)
	if err != nil {
		return err
	}
	defer insTag.Close()

	for _, id := range dropped {
		if _, err := delSrc.Exec(id); err != nil {
			return err
		}
	}

	type epPair struct{ srcID, epID int64 }
	epPairs := make([]epPair, 0, len(cands))
	showTags := map[[2]int64]struct{}{}
	introFor := map[int64]string{}

	for _, c := range cands {
		showID, ok := showIDByNorm[c.norm]
		if !ok {
			res, ierr := insShow.Exec(c.showName, c.norm, nowUnix(), nowUnix())
			if ierr != nil {
				continue
			}
			id, ierr := res.LastInsertId()
			if ierr != nil {
				continue
			}
			showID = id
			showIDByNorm[c.norm] = id
			showNameByID[id] = c.showName
		} else if showNameByID[showID] != c.showName {
			if _, uerr := renShow.Exec(c.showName, nowUnix(), showID); uerr == nil {
				showNameByID[showID] = c.showName
			}
		}

		ekey := [2]int64{showID, int64(c.epNo)}
		epID, ok := epIDByKey[ekey]
		if !ok {
			res, ierr := insEp.Exec(showID, c.epNo, nowUnix(), nowUnix())
			if ierr != nil {
				continue
			}
			id, ierr := res.LastInsertId()
			if ierr != nil {
				continue
			}
			epID = id
			epIDByKey[ekey] = id
			_, _ = touchShow.Exec(nowUnix(), showID)
		}
		if c.src.revived {
			cap := texts[msgKey{c.src.chatID, c.src.messageID}]
			if cap == "" && c.src.groupedID != 0 {
				cap = groupText[gKey{c.src.chatID, c.src.groupedID}]
			}
			if _, ierr := insSrc.Exec(epID, c.src.chatID, c.src.messageID, c.src.groupedID,
				c.src.fileName, c.src.fileSize, c.src.duration, c.src.width, c.src.height,
				c.src.mime, c.src.docID, c.src.accessHash, c.src.fileRef, cap, nowUnix()); ierr != nil {
				log.Printf("复活视频入库失败 chat=%d msg=%d: %v", c.src.chatID, c.src.messageID, ierr)
				continue
			}
			revivedBack++
		} else {
			epPairs = append(epPairs, epPair{c.src.id, epID})
		}
		renamed++
		if c.src.groupedID != 0 {
			k := gKey{c.src.chatID, c.src.groupedID}
			if groupedShow[k] == 0 {
				groupedShow[k] = showID
			}
		}
		if c.parsed != nil {
			for _, t := range c.parsed.Tags {
				if t == "" {
					continue
				}
				tagID, ok := tagIDByName[t]
				if !ok {
					res, ierr := insTag.Exec(t)
					if ierr != nil {
						continue
					}
					id, ierr := res.LastInsertId()
					if ierr != nil {
						continue
					}
					tagID = id
					tagIDByName[t] = tagID
				}
				showTags[[2]int64{showID, tagID}] = struct{}{}
			}
		}
		if !showHasIntro[showID] {
			if intro := buildRebuildIntro(c.text); intro != "" {
				introFor[showID] = intro
				showHasIntro[showID] = true
			}
		}
	}

	if len(epPairs) > 0 {
		if _, err := tx.Exec(`CREATE TEMP TABLE IF NOT EXISTS rebuild_ep_map (
    src_id INTEGER PRIMARY KEY, ep_id INTEGER NOT NULL)`); err != nil {
			return err
		}
		for start := 0; start < len(epPairs); start += 200 {
			end := start + 200
			if end > len(epPairs) {
				end = len(epPairs)
			}
			var sb strings.Builder
			sb.WriteString(`INSERT OR REPLACE INTO rebuild_ep_map (src_id, ep_id) VALUES `)
			args := make([]any, 0, (end-start)*2)
			for i := start; i < end; i++ {
				if i > start {
					sb.WriteString(",")
				}
				sb.WriteString("(?,?)")
				args = append(args, epPairs[i].srcID, epPairs[i].epID)
			}
			if _, err := tx.Exec(sb.String(), args...); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`UPDATE sources SET episode_id = (
    SELECT ep_id FROM rebuild_ep_map WHERE src_id = sources.id)
WHERE id IN (SELECT src_id FROM rebuild_ep_map)`); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM rebuild_ep_map`); err != nil {
			return err
		}
	}

	if len(showTags) > 0 {
		pairs := make([][2]int64, 0, len(showTags))
		for k := range showTags {
			pairs = append(pairs, k)
		}
		for start := 0; start < len(pairs); start += 200 {
			end := start + 200
			if end > len(pairs) {
				end = len(pairs)
			}
			var sb strings.Builder
			sb.WriteString(`INSERT OR IGNORE INTO show_tags (show_id, tag_id) VALUES `)
			args := make([]any, 0, (end-start)*2)
			for i := start; i < end; i++ {
				if i > start {
					sb.WriteString(",")
				}
				sb.WriteString("(?,?)")
				args = append(args, pairs[i][0], pairs[i][1])
			}
			if _, err := tx.Exec(sb.String(), args...); err != nil {
				return err
			}
		}
	}

	for showID, intro := range introFor {
		if err := setShowIntro(tx, showID, intro); err != nil {
			return err
		}
	}

	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("重建写库耗时 %s，补回历史丢弃的视频 %d 条",
		time.Since(writeStart).Round(time.Millisecond), revivedBack)

	segStart := time.Now()
	rows, err = d.db.Query(`
SELECT id, episode_id, chat_id, message_id FROM sources ORDER BY episode_id, chat_id, message_id, id`)
	if err != nil {
		return err
	}
	type segRow struct {
		id, epID, chatID int64
		messageID        int
	}
	var segs []segRow
	for rows.Next() {
		var s segRow
		if err := rows.Scan(&s.id, &s.epID, &s.chatID, &s.messageID); err == nil {
			segs = append(segs, s)
		}
	}
	rows.Close()

	if len(segs) > 0 {
		segTx, err := d.db.Begin()
		if err != nil {
			return err
		}
		defer segTx.Rollback()
		stmt, err := segTx.Prepare(`UPDATE sources SET segment_index = ? WHERE id = ?`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		lastKey := struct {
			ep, chat int64
		}{-1, -1}
		segIdx := 0
		for _, s := range segs {
			if s.epID != lastKey.ep || s.chatID != lastKey.chat {
				lastKey.ep, lastKey.chat = s.epID, s.chatID
				segIdx = 0
			}
			if _, err := stmt.Exec(segIdx, s.id); err != nil {
				return err
			}
			segIdx++
		}
		if err := segTx.Commit(); err != nil {
			return err
		}
	}
	log.Printf("已更新 %d 条来源归属与分段（耗时 %s）", renamed, time.Since(segStart).Round(time.Millisecond))

	photoStart := time.Now()
	normToID := map[string]int64{}
	if nrows, err := d.db.Query(`SELECT id, norm_name FROM shows`); err == nil {
		for nrows.Next() {
			var id int64
			var norm string
			if err := nrows.Scan(&id, &norm); err == nil && norm != "" {
				normToID[norm] = id
			}
		}
		nrows.Close()
	}
	groupedByMsg := map[msgKey]int64{}
	if grows, err := d.db.Query(`SELECT chat_id, message_id, grouped_id FROM sources WHERE grouped_id != 0`); err == nil {
		for grows.Next() {
			var chatID, grouped int64
			var msgID int
			if err := grows.Scan(&chatID, &msgID, &grouped); err == nil {
				groupedByMsg[msgKey{chatID, msgID}] = grouped
			}
		}
		grows.Close()
	}

	rows, err = d.db.Query(`SELECT id, chat_id, message_id, grouped_id FROM show_photos`)
	if err != nil {
		return err
	}
	type photoRow struct {
		id, chatID, groupedID int64
		messageID             int
	}
	var photos []photoRow
	for rows.Next() {
		var ph photoRow
		if err := rows.Scan(&ph.id, &ph.chatID, &ph.messageID, &ph.groupedID); err == nil {
			photos = append(photos, ph)
		}
	}
	rows.Close()

	type photoSet struct {
		photoID, showID int64
	}
	sets := make([]photoSet, 0, len(photos))
	for _, ph := range photos {
		var target int64
		if parsed, perr := p.Parse(texts[msgKey{ph.chatID, ph.messageID}], ""); perr == nil && parsed.NormName != "" {
			target = normToID[parsed.NormName]
		}
		if target == 0 {

			grouped := ph.groupedID
			if grouped == 0 {
				grouped = groupedByMsg[msgKey{ph.chatID, ph.messageID}]
			}
			if grouped != 0 {
				target = groupedShow[gKey{ph.chatID, grouped}]
			}
		}
		if target != 0 {
			sets = append(sets, photoSet{ph.id, target})
		}
	}
	remapped := 0
	if len(sets) > 0 {
		ptx, err := d.db.Begin()
		if err != nil {
			return err
		}
		defer ptx.Rollback()
		stmt, err := ptx.Prepare(`UPDATE show_photos SET show_id = ? WHERE id = ?`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for _, s := range sets {
			if _, err := stmt.Exec(s.showID, s.photoID); err != nil {
				return err
			}
			remapped++
		}
		if err := ptx.Commit(); err != nil {
			return err
		}
	}
	log.Printf("预览图重新归属 %d 张（耗时 %s）", remapped, time.Since(photoStart).Round(time.Millisecond))

	cleanStart := time.Now()
	if _, err := d.db.Exec(`DELETE FROM episodes WHERE id NOT IN (SELECT DISTINCT episode_id FROM sources)`); err != nil {
		return err
	}
	if _, err := d.db.Exec(`DELETE FROM show_tags WHERE show_id NOT IN (
SELECT DISTINCT show_id FROM episodes
UNION SELECT DISTINCT show_id FROM show_photos)`); err != nil {
		return err
	}
	res, err := d.db.Exec(`DELETE FROM shows WHERE id NOT IN (
SELECT DISTINCT show_id FROM episodes
UNION SELECT DISTINCT show_id FROM show_photos)`)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n > 0 {
		log.Printf("已清理 %d 部空壳短剧", n)
	}

	if n, err := d.purgeJunkPhotoOnlyShows(); err != nil {
		return err
	} else if n > 0 {
		log.Printf("已清理 %d 部只有图、名字是分类词的假剧", n)
	}

	log.Printf("清理孤儿数据耗时 %s", time.Since(cleanStart).Round(time.Millisecond))

	anaStart := time.Now()
	if _, err := d.db.Exec("ANALYZE"); err != nil {
		log.Printf("ANALYZE 失败（不影响使用）: %v", err)
	} else {
		log.Printf("ANALYZE 耗时 %s", time.Since(anaStart).Round(time.Millisecond))
	}

	_ = d.SetKV("rebuild_v"+rebuildVersion, "1")
	log.Printf("媒体库重建完成（函数内总计 %s）", time.Since(loadStart).Round(time.Millisecond))
	return nil
}

func (d *DB) purgeJunkPhotoOnlyShows() (int, error) {
	rows, err := d.db.Query(`SELECT id, name FROM shows WHERE id NOT IN (
SELECT DISTINCT show_id FROM episodes)`)
	if err != nil {
		return 0, err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			continue
		}
		if parser.IsJunkShowName(name) {
			ids = append(ids, id)
		}
	}
	rows.Close()
	if len(ids) == 0 {
		return 0, nil
	}
	tx, err := d.db.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	for _, id := range ids {
		for _, q := range []string{
			`DELETE FROM show_photos WHERE show_id = ?`,
			`DELETE FROM show_tags WHERE show_id = ?`,
			`DELETE FROM shows WHERE id = ?`,
		} {
			if _, err := tx.Exec(q, id); err != nil {
				return 0, err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func buildRebuildIntro(text string) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) > 200 {
		return string(runes[:200])
	}
	return text
}

func hasHan(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}
