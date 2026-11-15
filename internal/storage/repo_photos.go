package storage

import (
	"database/sql"
	"errors"
	"sort"
	"strconv"
	"strings"
)

// ShowPhoto 剧集预览图（来自频道相册图片）
type ShowPhoto struct {
	ID     int64 `json:"id"`
	ShowID int64 `json:"show_id"`
	Width  int   `json:"width"`
	Height int   `json:"height"`
	// EpisodeNo 是这张图「就近归属」的集号，只用于前端分组/提示。
	// 0 表示判不出来（图片和该剧的视频不在同一批消息里）。
	EpisodeNo int `json:"episode_no,omitempty"`
}

// UpsertShowPhoto 按 doc_id 去重保存预览图定位信息。
// caption / grouped_id 存的是这条图片消息的原始文案和相册号，重建媒体库时
// 用来给同相册里没有 caption 的视频补齐剧集归属。
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

// primaryPhotoChatCond 预览图「只认一个频道」的筛选条件。
//
// 一部剧的相册图可能散在多个频道（实测 8749 部里 467 部跨 2~7 个频道），
// 逐个频道取消息在快速切剧时是多余的网络往返。这里选定「预览图最多的启用频道」
// 作为该剧的图片源：每部剧只问一个频道、只发一次 channels.getMessages，
// 同时顺带把已关闭频道的图片挡掉（和关闭频道后内容隐藏的规则保持一致）。
// 并列时取 chat_id 小的，保证多次查询/重启后选择稳定。
const primaryPhotoChatSub = `(
    SELECT q.chat_id FROM show_photos q
    JOIN chats c ON c.id = q.chat_id AND c.enabled = 1
    WHERE q.show_id = ?
    GROUP BY q.chat_id
    ORDER BY COUNT(*) DESC, q.chat_id
    LIMIT 1)`

// 详情页预览图的裁剪参数。
//
// 频道里图片和视频通常同批发布（实测 91% 的图与所属剧的视频消息相差 ≤20 条），
// 而「挂错」的广告图会横跨几百上千条消息——频道早期的推广图被算进了后来入库的剧。
// 用「与视频消息的距离」就能把这类广告图挡掉，不必分析图片内容。
const (
	// photoMaxGap 图与所属剧任一视频消息的最大条数差，超过即视为无关（广告/引流图）
	photoMaxGap = 400
	// photoLatestEps 详情页只展示最新这几集的预览图
	photoLatestEps = 3
	// photoPerEpisodeMax 每集最多展示几张，避免一集发几十张把预览条刷爆
	photoPerEpisodeMax = 6
	// photoMaxTotal 兜底上限（判不出归属时也用它）
	photoMaxTotal = 18
)

// ListShowPhotos 列出详情页要展示的预览图（只取该剧的主频道）。
//
// 返回的每一张都满足：离该剧某条视频消息不远（排除挂错的频道广告图），
// 且属于最新的 photoLatestEps 集。这样详情页不再把「上一集/无关推广」的图
// 全铺出来，按需下载的请求量也随之下降。
//
// 这里不过滤 cache_path：预览图文件被清理后记录仍在，前端会先显示封面占位、
// 同时按需重新下载，清缓存不会让整页图片永久消失。
func (d *DB) ListShowPhotos(showID int64) ([]*ShowPhoto, error) {
	type cand struct {
		p      ShowPhoto
		chatID int64
		msgID  int
		gap    int // 到最近一条视频消息的条数差；-1 表示判不出来
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

	// 该剧每条来源消息对应的集号，用来把图就近归到某一集。
	// 连接池只有一个连接，上面必须先把 show_photos 读完再查这里。
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

	// 一张都判不出归属的剧（图都来自别的频道，或只同步到图片没同步到视频）：
	// 保持原样按顺序限量返回，不要让整条预览条凭空消失。
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

	// 只保留最新 N 集
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

	// 同一集内离视频越近越可能是官方剧照，按这个顺序取前 photoPerEpisodeMax 张
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

// PhotoLoc 预览图在 Telegram 上的定位信息（批量按需下载用）
type PhotoLoc struct {
	ID        int64
	DocID     int64
	ChatID    int64
	MessageID int
	CachePath string
}

// ListShowPhotoLocs 列出某部剧预览图的定位信息（只取主频道），用于详情页预热。
// 只返回一个频道意味着预热最多发一次 channels.getMessages；同时复用
// ListShowPhotos 的裁剪结果，只预热详情页真正会显示的那几张（最新 3 集、
// 离视频消息足够近），不再为早已不展示的旧集/广告图白白发下载请求。
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

// GetShowBrief 取剧名与封面路径，用于预览图未下载时的封面占位。
// 只查两列：详情页一页十几张图都会走这里，不能像 GetShow 那样做可播过滤与标签聚合。
func (d *DB) GetShowBrief(showID int64) (name, cover string, err error) {
	err = d.db.QueryRow(`SELECT name, cover_path FROM shows WHERE id = ?`, showID).
		Scan(&name, &cover)
	return name, cover, err
}

// GetShowPhotoPath 查询预览图本地缓存路径
func (d *DB) GetShowPhotoPath(photoID int64) (string, error) {
	var path string
	err := d.db.QueryRow(
		`SELECT cache_path FROM show_photos WHERE id = ?`, photoID).Scan(&path)
	if err != nil {
		return "", err
	}
	return path, nil
}

// PhotoLocation 预览图在 Telegram 上的定位信息（清掉本地文件后重新下载用）
type PhotoLocation struct {
	ID            int64
	ShowID        int64
	ChatID        int64
	MessageID     int64
	DocID         int64
	AccessHash    int64
	FileReference []byte
}

// GetPhotoLocation 查询某张预览图的 TG 定位信息
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

// ResetPhotoCache 把预览图的本地缓存标记清空，并清掉「用预览图当封面」的短剧封面，
// 让它们回到「没有封面」的状态（之后由后台回填或用户访问时重新下载）。
// prefix 是预览图目录（含结尾分隔符），只匹配该目录下的路径。
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

// SetPhotoCache 记录预览图本地缓存路径
func (d *DB) SetPhotoCache(docID int64, path string) error {
	_, err := d.db.Exec(`UPDATE show_photos SET cache_path = ? WHERE doc_id = ? AND cache_path = ''`,
		path, docID)
	return err
}

// FindShowByGrouped 通过相册内已入库的视频反查短剧 ID，查不到返回 0
func (d *DB) FindShowByGrouped(chatID, groupedID int64) int64 {
	var showID int64
	_ = d.db.QueryRow(`
SELECT e.show_id FROM sources s JOIN episodes e ON e.id = s.episode_id
WHERE s.chat_id = ? AND s.grouped_id = ? LIMIT 1`, chatID, groupedID).Scan(&showID)
	return showID
}

// CountSourcesByEpisode 统计某频道某集已入库的分段数
func (d *DB) CountSourcesByEpisode(chatID, episodeID int64) int {
	var n int
	_ = d.db.QueryRow(`
SELECT COUNT(*) FROM sources WHERE episode_id = ? AND chat_id = ?`,
		episodeID, chatID).Scan(&n)
	return n
}
