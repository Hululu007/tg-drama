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

// rebuildVersion 重建版本号：解析规则升级后 +1 即可触发全库重建
// （v31：收紧「# 全是分类词」的标签定名，历史假剧靠重解析修正）
const rebuildVersion = "43"

// rawDoc 是 pending_msgs.info_json 里 tg.Document 的子集，
// 用于把「同步时因缺 caption 被丢弃」的视频重新纳入媒体库。
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

// RebuildFromRaw 基于来源行上的原始 caption 按最新解析规则重建全库映射：
// 剧名/剧集归属、分段顺序、预览图、标签与简介。一次性迁移，kv 记录版本。
// 下载状态与已下载文件不受影响。
//
// 性能：解析阶段纯内存计算；落库阶段把上万条 UPSERT 收进一个事务，
// 分段重编号也批量提交，避免每条语句各自 fsync（旧实现 40–60 秒）。
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

	// 1. 载入来源的原始文案；同时记录每个相册（grouped_id）的 caption，
	// 供无 caption 的相册成员共用，避免把文件名里的模板集号当成集号。
	// 文案直接存在 sources.caption 上，没有 caption 的消息不占行。
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

	// 2. 载入全部视频来源（按频道+消息顺序，保证相册连续分集可重放）
	type srcRow struct {
		id, episodeID, chatID, groupedID int64
		messageID                        int
		fileName                         string
		// 以下字段只对「复活」的待归属消息有意义：sources 里还没有它的行，
		// 需要靠 info_json 里的文档信息新建来源。
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

	// 2.1 复活待归属的视频：相册 caption 只挂在其中一条消息上，先入库的那些
	// 成员当初读不到 caption、文件名又是哈希，被判定为垃圾丢掉了。它们留在
	// pending_msgs 里，这里用同组 caption 重新解析后补成正式来源。
	// （原先这一步读的是与 sources 等长的 raw_messages，现已收窄到这一小撮。）
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
			// 没有文件名的文档无法判断剧集归属，保持原样跳过
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

	// 3. 逐条重新解析（先只算不落库）：需要先拿到「有《》/【】标题容器」的
	// 可信剧名集合，才能用 #标签 给推广/合集帖定名。
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
	groupedCount := map[gKey]int{} // 相册内已分配的视频数
	groupSize := map[gKey]int{}    // 相册内视频总数（判断是否「单视频 + 范围文案」）
	for _, s := range srcs {
		if s.groupedID != 0 {
			groupSize[gKey{s.chatID, s.groupedID}]++
		}
	}
	albums := parser.NewAlbumResolver() // 相册集号分配器（与在线采集同一套规则）
	cands := make([]candidate, 0, len(srcs))
	var dropped []int64 // 无法识别的垃圾来源 ID

	for _, s := range srcs {
		text := texts[msgKey{s.chatID, s.messageID}]
		fromCaption := strings.TrimSpace(text) != ""
		// 相册 caption 只挂在其中一条消息上，其余成员共用同组 caption，
		// 否则会退回到文件名里的模板集号（如「EP110 全10集」）。
		if !fromCaption && s.groupedID != 0 {
			if gt := groupText[gKey{s.chatID, s.groupedID}]; gt != "" {
				text, fromCaption = gt, true
			}
		}
		c := candidate{src: s, text: text, fromCaption: fromCaption}
		if pp, perr := p.Parse(text, s.fileName); perr == nil && pp.NormName != "" {
			c.showName, c.norm, c.parsed = pp.ShowName, pp.NormName, pp
		}
		// 垃圾识别：随机串/哈希/采集站命名（faststart_oybfvg54、Qhwfres4nq btc8），
		// 以及水印包名（甄选视频、合集）。有《》/【】/#标签 的名字也要过一遍。
		if c.norm != "" && parser.IsJunkName(c.showName) {
			if s.id != 0 {
				dropped = append(dropped, s.id)
			}
			continue
		}
		if c.norm == "" {
			// 解析失败：用视频文件名兜底；清洗后仍为空（纯哈希/720p 类
			// 垃圾文件名）则丢弃该来源，避免污染媒体库
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
		// 集号分配与在线采集一致：相册声明范围 > 单集 caption > 文件名 > 相册起点
		k := gKey{s.chatID, s.groupedID}
		c.epNo = albums.ResolveSized(s.chatID, s.groupedID, c.parsed, fromCaption, s.fileName, groupedCount[k], groupSize[k])
		if s.groupedID != 0 {
			groupedCount[k]++
		}
		cands = append(cands, c)
	}

	// 3.05 多标题预览帖：一条 caption 依次列出 N 部剧的预览（如「《母上攻略》第6集
	// 《兄弟妈妈好好操》第7集 …」），相册里正好有 N 个视频时按出现顺序一一对应，
	// 避免把 6 部剧的预览片段全算成第一部剧同一集的 6 个分段。
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

	// 3.1 #标签辅助定名：没有标题容器的帖子，若带有一个「正好等于已有可信剧名」
	// 的标签，就归到那部剧，而不是新建「爱情公寓 91」这类营销后缀近似名。
	// 只信《》/【】容器里出现的剧名，避免把 #ai短剧 这类泛标签当剧名。
	canon := map[string]string{}
	for _, c := range cands {
		if c.parsed != nil && c.parsed.HasTitle && c.norm != "" {
			if _, ok := canon[c.norm]; !ok {
				canon[c.norm] = c.showName
			}
		}
	}
	tagMatched := 0
	// 用户确认过的异名归类：先把候选名映射到规范名，再按规范名归并。
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

	groupedShow := map[gKey]int64{} // 相册 → 剧ID（供图片反查）
	renamed := 0
	revivedBack := 0

	// 3.2 落库：所有 UPSERT/UPDATE 放进一个事务，一次提交。
	// 先把剧/集/标签读进内存做字典，逐条来源只做「取 ID、必要时插入」，
	// 回填用临时表 + 批量 VALUES，几千条来源也只产生上百条语句。
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
	// 一次性打散 segment_index，避免 (chat_id, message_id, segment_index) 唯一键冲突
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

	// 批量回填 episode_id：临时表 + 一条 UPDATE 顶掉数千条单行 UPDATE
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

	// 批量写剧-标签关联（已按剧去重）
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

	// 简介只在原本为空时补一次
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

	// 4. 重编分段号：同剧同频道按消息顺序；查询完再开事务，避免单连接被占
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

	// 5. 预览图重新归属：先按自身 caption 解析，失败则按同相册视频反查。
	// 先把剧名索引和相册分组读进内存，再在一个事务里批量 UPDATE，
	// 避免每张图 2–3 次单独查询/提交（旧实现在 2600 张图上要 ~0.6 秒）。
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
			// 相册图片：与视频同 grouped，反查同相册视频所属剧
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

	// 6. 清理孤儿数据：无来源的集、无集无图的剧
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
	// 6.1 只剩预览图、没有剧集，剧名又是分类词的假剧（#制服 / #诱惑 这类
	// 旧规则按标签建的）：连图一起清掉，否则列表里会一直挂着「诱惑」这种条目。
	// 有正经剧名的「纯图集」不在此列。
	if n, err := d.purgeJunkPhotoOnlyShows(); err != nil {
		return err
	} else if n > 0 {
		log.Printf("已清理 %d 部只有图、名字是分类词的假剧", n)
	}

	log.Printf("清理孤儿数据耗时 %s", time.Since(cleanStart).Round(time.Millisecond))

	// 重建后统计信息已过期，ANALYZE 让查询规划器重新选索引（首屏列表受益明显）
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

// purgeJunkPhotoOnlyShows 删除「没有剧集、只剩预览图、剧名本身是分类词」的假剧。
// 这些是旧解析规则把 #制服 / #诱惑 这类标签当剧名建出来的空壳，图片已无从
// 归属到真剧，留着只会让列表里出现「诱惑」这种条目；连同 show_photos /
// show_tags 行一起删。返回删除的剧数。
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

// buildRebuildIntro 与 ingest 的 buildIntro 相同：取前 200 字做简介
func buildRebuildIntro(text string) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) > 200 {
		return string(runes[:200])
	}
	return text
}

// hasHan 是否包含汉字
func hasHan(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}
