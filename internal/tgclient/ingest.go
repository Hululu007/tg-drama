package tgclient

import (
	"context"
	"fmt"
	"log"
	"regexp"
	"sort"
	"strings"

	"github.com/gotd/td/tg"

	"tgdrama/internal/docinfo"
	"tgdrama/internal/parser"
	"tgdrama/internal/storage"
)

// matchTagShow 用 #标签 匹配已入库短剧；只对没有标题容器的帖子启用，
// 且标签必须归一化后等于某部剧的剧名（不拿泛标签猜）。
func (s *Service) matchTagShow(parsed *parser.Parsed, norm string) (string, string) {
	for _, tag := range parsed.Tags {
		tn := parser.Normalize(tag)
		if tn == "" || tn == norm {
			continue
		}
		if name, ok := s.showNameByNorm(tn); ok {
			return name, tn
		}
	}
	return parsed.ShowName, norm
}

// showNameByNorm 在内存索引里查剧名。同步开始时整表载入一次（一千多部剧不到
// 1ms），入库时发现新剧再增量补充，避免每条消息都查一次 shows 表。
func (s *Service) showNameByNorm(norm string) (string, bool) {
	s.normMu.Lock()
	defer s.normMu.Unlock()
	if s.normNames == nil {
		names, err := s.db.AllShowNorms()
		if err != nil {
			return "", false
		}
		s.normNames = names
	}
	name, ok := s.normNames[norm]
	return name, ok
}

// showIDByNorm 在内存索引里查剧 ID（相册图片归属用，避免每条图都写一次库）。
func (s *Service) showIDByNorm(norm string) (int64, bool) {
	s.normMu.Lock()
	defer s.normMu.Unlock()
	if s.normIDs == nil {
		ids, err := s.db.AllShowIDs()
		if err != nil {
			return 0, false
		}
		s.normIDs = ids
	}
	id, ok := s.normIDs[norm]
	return id, ok
}

// rememberShow 把新建的短剧补进内存索引
func (s *Service) rememberShow(norm, name string) {
	s.normMu.Lock()
	if s.normNames == nil {
		s.normNames = map[string]string{}
	}
	s.normNames[norm] = name
	s.normMu.Unlock()
}

// rememberShowID 把新建短剧的 ID 补进内存索引（与 rememberShow 分开加锁调用，
// 避免两条路径互相嵌套拿锁）
func (s *Service) rememberShowID(norm string, id int64) {
	if norm == "" || id <= 0 {
		return
	}
	s.normMu.Lock()
	if s.normIDs == nil {
		s.normIDs = map[string]int64{}
	}
	s.normIDs[norm] = id
	s.normMu.Unlock()
}

// ingestMessage 单条消息入库（实时更新与补拉用）
func (s *Service) ingestMessage(ctx context.Context, c *storage.Chat, msg *tg.Message, groupCaption string) error {
	caps := map[int64]string{}
	if msg.GroupedID != 0 && strings.TrimSpace(groupCaption) != "" {
		caps[msg.GroupedID] = groupCaption
	}
	_, err := s.ingestPage(ctx, c, []*tg.Message{msg}, caps)
	return err
}

// ingestPage 按 message_id 升序把一页消息入库，返回新增媒体条目数。
//
// 与逐条入库相比做了三件事：①整页消息共用一个数据库事务（实测 700µs/条 →
// 100µs/条）②已入库消息、相册成员数、剧名索引都批量预取到内存 ③封面/预览图
// 下载移到后台协程池，不再阻塞同步。msgs 必须是升序，相册分段编号依赖这个顺序。
func (s *Service) ingestPage(ctx context.Context, c *storage.Chat, msgs []*tg.Message, caps map[int64]string) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}
	// 关闭的频道一律不入库：历史同步、实时更新、补全搜索都经过这里，
	// 在数据入口统一拦一道，避免某条路径漏检又把关闭频道的消息写进库。
	if c == nil || !c.Enabled {
		return 0, nil
	}
	ids := make([]int, 0, len(msgs))
	for _, m := range msgs {
		ids = append(ids, m.ID)
	}
	texts, err := s.db.RawMessageTexts(c.ID, ids)
	if err != nil {
		return 0, err
	}

	// 相册成员顺序：把「已入库的来源」和「本页所有视频」并在一起按消息 ID 排序，
	// 用消息的真实序号当 pos。只数已入库成员（旧做法）会让跨页/补拉的相册
	// 从中间开始编号——「第7-8集」只补到第 7 集那条时，它会被算成第 8 集。
	gids := make([]int64, 0, 8)
	seen := make(map[int64]bool, 8)
	pageMembers := make(map[int64][]int, 8)
	for _, m := range msgs {
		media, ok := m.Media.(*tg.MessageMediaDocument)
		if !ok || !media.Video || m.GroupedID == 0 {
			continue
		}
		if !seen[m.GroupedID] {
			seen[m.GroupedID] = true
			gids = append(gids, m.GroupedID)
		}
		pageMembers[m.GroupedID] = append(pageMembers[m.GroupedID], m.ID)
	}
	existingMembers, err := s.db.GroupSourceMessages(c.ID, gids)
	if err != nil {
		return 0, err
	}
	groupPos := make(map[int]int, len(msgs))
	groupSize := make(map[int64]int, len(pageMembers))
	for gid, pageIDs := range pageMembers {
		all := dedupeInts(append(append([]int{}, existingMembers[gid]...), pageIDs...))
		sort.Ints(all)
		groupSize[gid] = len(all)
		for i, id := range all {
			groupPos[id] = i
		}
	}
	// 只有空 caption 被丢弃过的消息才需要重试：它们 raw_messages 里 text 为空、
	// 也没有 source（第 7 集视频就卡在这里）。同相册的 caption 现在是可见的，
	// 补拉/重建后就能正确落集。
	capsSeen := make(map[int64]string, 4)
	groupCaption := func(gid int64) string {
		if gid == 0 {
			return ""
		}
		if v, ok := capsSeen[gid]; ok {
			return v
		}
		gt := strings.TrimSpace(caps[gid])
		if gt == "" {
			gt, _ = s.db.GetGroupCaption(c.ID, gid)
		}
		capsSeen[gid] = gt
		return gt
	}

	items := make([]storage.PendingSource, 0, len(msgs))
	docs := make([]*tg.Document, 0, len(msgs))
	multis := make(map[int64][]parser.MultiEntry)
	var photos []*tg.Message
	// 解析失败被丢弃的相册成员：只有它们在库里留一份待归属记录，
	// 等同组 caption 到位后由 RebuildFromRaw 补成正式来源。
	rejected := make([]storage.RawRecord, 0, 4)

	for _, msg := range msgs {
		if _, ok := msg.Media.(*tg.MessageMediaPhoto); ok {
			if _, done := texts[msg.ID]; !done {
				photos = append(photos, msg)
			}
			continue
		}
		media, ok := msg.Media.(*tg.MessageMediaDocument)
		if !ok || !media.Video {
			continue
		}
		doc, ok := media.Document.(*tg.Document)
		if !ok {
			continue
		}
		if _, ok := texts[msg.ID]; ok && (strings.TrimSpace(texts[msg.ID]) != "" ||
			(msg.GroupedID == 0 || groupCaption(msg.GroupedID) == "")) {
			// 已完整处理过：有 caption（解析过）或者本来就没有可用的相册 caption
			continue
		}
		pos := groupPos[msg.ID]
		item, multi, ok := s.buildPending(c, msg, doc, caps[msg.GroupedID], pos, groupSize[msg.GroupedID])
		if !ok {
			rejected = append(rejected, storage.RawRecord{
				MessageID: msg.ID, GroupedID: msg.GroupedID, Text: msg.Message,
				Info: docinfo.Marshal(doc),
			})
			continue
		}
		items = append(items, *item)
		docs = append(docs, doc)
		if len(multi) >= 2 && msg.GroupedID != 0 {
			multis[msg.GroupedID] = multi
		}
	}

	added := 0
	{
		res, err := s.db.IngestBatch(c.ID, items, rejected)
		if err != nil {
			return 0, err
		}
		added = res.Inserted
		for i := range items {
			if res.SourceID[i] == 0 {
				continue
			}
			if showID, ok := res.NewShows[items[i].NormName]; ok {
				s.rememberShow(items[i].NormName, items[i].ShowName)
				s.rememberShowID(items[i].NormName, showID)
				s.enqueueCover(showID, docs[i])
			}
		}
		// 多标题预览帖：一条 caption 依次列了多部剧的预览，相册里正好 N 个视频时
		// 按出现顺序重新分配，避免把 N 部剧的片段全算成第一部剧同一集。
		for gid, multi := range multis {
			s.reassignMultiAlbum(c, gid, multi)
		}
	}
	for _, p := range photos {
		_ = s.ingestPhoto(ctx, c, p)
	}
	return added, nil
}

// buildPending 把一条视频消息解析成待入库结构；返回 false 表示应丢弃（垃圾视频）。
func (s *Service) buildPending(c *storage.Chat, msg *tg.Message, doc *tg.Document,
	groupCaption string, pos, groupSize int) (*storage.PendingSource, []parser.MultiEntry, bool) {

	var (
		duration float64
		w, h     int
		fileName string
	)
	for _, attr := range doc.Attributes {
		switch a := attr.(type) {
		case *tg.DocumentAttributeVideo:
			duration, w, h = a.Duration, a.W, a.H
		case *tg.DocumentAttributeFilename:
			fileName = a.FileName
		}
	}
	if fileName == "" {
		fileName = fmt.Sprintf("doc-%d.mp4", doc.ID)
	}

	// 相册 caption 可能挂在同组另一条消息上：取同组 caption，
	// 避免用文件名里的模板集号（如「EP110 全10集」）错误分集。
	text := msg.Message
	fromCaption := strings.TrimSpace(text) != ""
	if !fromCaption && msg.GroupedID != 0 {
		if strings.TrimSpace(groupCaption) != "" {
			text, fromCaption = groupCaption, true
		} else if gt, _ := s.db.GetGroupCaption(c.ID, msg.GroupedID); gt != "" {
			text, fromCaption = gt, true
		}
	}
	parsed, perr := s.parser.Parse(text, fileName)
	showName, norm := parsed.ShowName, parsed.NormName
	// 短剧频道里混着的非短剧内容，整条丢弃：
	//   ① 拉群引流帖（正文是「点此开通 立刻进群追更」，视频只是几秒试看片段）
	//   ② 转发的推特 / OnlyFans 博主福利视频（文案以「推特极品…」开头）
	if fromCaption && (parser.IsNonDramaCaption(text) ||
		(!parsed.HasTitle && parser.IsAdCaption(text))) {
		return nil, nil, false
	}
	// #标签 辅助定名：没有《》/【】标题容器的帖子，若某个标签正好等于已入库的
	// 剧名，就归到该剧，避免「爱情公寓 91」这类营销后缀近似名重复建剧。
	if perr == nil && !parsed.HasTitle && norm != "" && len(parsed.Tags) > 0 {
		showName, norm = s.matchTagShow(parsed, norm)
	}
	if perr != nil || norm == "" {
		// 无法识别时用清洗后的文件名兜底建剧；清洗后为空（纯哈希/720p 类
		// 垃圾文件名）则跳过，避免污染媒体库
		showName = parser.NameFromFileName(fileName)
		if junkStem(showName) {
			return nil, nil, false
		}
		norm = parser.Normalize(showName)
		if norm == "" {
			return nil, nil, false
		}
	}
	// 解析成功也可能只是采集站/哈希文件名（faststart_oybfvg54、Qhwfres4nq btc8），
	// 这类名字不能建剧，否则媒体库里全是无意义条目；泛分类词（露脸/大学生/颜值）
	// 与广告话术同样不能当剧名。
	if parser.IsJunkName(showName) || parser.IsJunkShowName(showName) {
		return nil, nil, false
	}
	s.albumMu.Lock()
	epNo := s.albums.ResolveSized(c.ID, msg.GroupedID, parsed, fromCaption, fileName, pos, groupSize)
	s.albumMu.Unlock()
	if epNo <= 0 {
		epNo = 1
	}

	item := &storage.PendingSource{
		ShowName:  showName,
		NormName:  norm,
		Intro:     buildIntro(msg.Message),
		Tags:      parsed.Tags,
		EpisodeNo: epNo,
		Source: storage.Source{
			ChatID: c.ID, MessageID: msg.ID, GroupedID: msg.GroupedID,
			FileName: fileName, FileSize: doc.Size, Duration: duration,
			Width: w, Height: h, Mime: doc.MimeType,
			DocID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference,
		},
		// 存「实际用于解析的文案」：消息自己的 caption；自己没写时存同相册的
		// groupCaption。相册 caption 只挂在其中一条上，如果这里丢掉它，重建时
		// 同组其它成员就只能退回文件名猜剧名（这是之前脏名反复出现的根因之一）。
		Raw:      storage.RawRecord{Text: text},
		Variants: altVariants(msg, doc),
	}
	return item, parsed.Multi, true
}

// altVariants 把消息的 alt_documents 转成清晰度版本列表。
// Telegram 的 alt_documents 是「同一视频的其它清晰度」，每项都是完整 Document，
// 有自己的 id/access_hash/file_reference/size 与 DocumentAttributeVideo，
// 取流必须用变体自己的参数。只收 video/* 且体积有效的文档：
// HLS quality file（application/x-mpegurl）只是指向目标文档的索引，不是可直接播放的视频。
func altVariants(msg *tg.Message, main *tg.Document) []storage.SourceVariant {
	media, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok || media == nil {
		return nil
	}
	var out []storage.SourceVariant
	seen := map[int64]bool{}
	if main != nil {
		seen[main.ID] = true // 主文档已经在 sources 行上，不重复存
	}
	for _, d := range media.AltDocuments {
		doc, ok := d.(*tg.Document)
		if !ok || doc == nil || doc.ID == 0 || doc.Size <= 0 || seen[doc.ID] {
			continue
		}
		if !strings.HasPrefix(strings.ToLower(doc.MimeType), "video/") {
			continue
		}
		v := storage.SourceVariant{
			DocID: doc.ID, AccessHash: doc.AccessHash, FileReference: doc.FileReference,
			FileSize: doc.Size, Mime: doc.MimeType,
		}
		for _, attr := range doc.Attributes {
			if va, ok := attr.(*tg.DocumentAttributeVideo); ok {
				v.Width, v.Height, v.Duration = va.W, va.H, va.Duration
				v.Codec = va.VideoCodec
				break
			}
		}
		seen[doc.ID] = true
		out = append(out, v)
	}
	return out
}

// reassignMultiAlbum 把多标题预览帖的相册成员按 caption 中的标题顺序重新归属。
// 只有「相册成员数 == 标题数」时才动手，避免把正常的同集分段拆散。
func (s *Service) reassignMultiAlbum(c *storage.Chat, groupedID int64, multi []parser.MultiEntry) {
	srcs, err := s.db.ListGroupSources(c.ID, groupedID)
	if err != nil || len(srcs) != len(multi) {
		return
	}
	moved := 0
	for i, gs := range srcs {
		m := multi[i]
		norm := parser.Normalize(m.Name)
		if norm == "" {
			continue
		}
		showID, _, err := s.db.UpsertShow(m.Name, norm)
		if err != nil {
			continue
		}
		s.rememberShow(norm, m.Name)
		epNo := m.Ep
		if epNo <= 0 {
			epNo = 1
		}
		epID, _, err := s.db.UpsertEpisode(showID, epNo)
		if err != nil || epID == gs.EpisodeID {
			continue
		}
		if err := s.db.MoveSource(gs.ID, epID); err == nil {
			moved++
		}
	}
	if moved > 0 {
		log.Printf("多标题预览帖按顺序重新归属 %d 条来源（相册 %d）", moved, groupedID)
	}
}

// reJunkStem 匹配 Telegram 自动命名/采集残留（doc-123、VID_000853、纯数字等），
// 这些文件名不能当剧名，否则会建出一堆「doc 514364」垃圾条目。
var reJunkStem = regexp.MustCompile(`(?i)^(doc|vid|img|video|file|photo|image|temp)[\s_\-]*[0-9]*$`)

func junkStem(name string) bool {
	if parser.IsJunkName(name) {
		return true
	}
	return reJunkStem.MatchString(strings.TrimSpace(name))
}

func buildIntro(text string) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	if len(runes) > 200 {
		return string(runes[:200])
	}
	return text
}

// dedupeInts 去重并保持升序（相册成员消息 ID 合并时用）
func dedupeInts(in []int) []int {
	if len(in) < 2 {
		return in
	}
	sort.Ints(in)
	out := in[:1]
	for _, v := range in[1:] {
		if v != out[len(out)-1] {
			out = append(out, v)
		}
	}
	return out
}
