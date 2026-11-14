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

func (s *Service) rememberShow(norm, name string) {
	s.normMu.Lock()
	if s.normNames == nil {
		s.normNames = map[string]string{}
	}
	s.normNames[norm] = name
	s.normMu.Unlock()
}

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

func (s *Service) ingestMessage(ctx context.Context, c *storage.Chat, msg *tg.Message, groupCaption string) error {
	caps := map[int64]string{}
	if msg.GroupedID != 0 && strings.TrimSpace(groupCaption) != "" {
		caps[msg.GroupedID] = groupCaption
	}
	_, err := s.ingestPage(ctx, c, []*tg.Message{msg}, caps)
	return err
}

func (s *Service) ingestPage(ctx context.Context, c *storage.Chat, msgs []*tg.Message, caps map[int64]string) (int, error) {
	if len(msgs) == 0 {
		return 0, nil
	}

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

		for gid, multi := range multis {
			s.reassignMultiAlbum(c, gid, multi)
		}
	}
	for _, p := range photos {
		_ = s.ingestPhoto(ctx, c, p)
	}
	return added, nil
}

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

	if fromCaption && (parser.IsNonDramaCaption(text) ||
		(!parsed.HasTitle && parser.IsAdCaption(text))) {
		return nil, nil, false
	}

	if perr == nil && !parsed.HasTitle && norm != "" && len(parsed.Tags) > 0 {
		showName, norm = s.matchTagShow(parsed, norm)
	}
	if perr != nil || norm == "" {

		showName = parser.NameFromFileName(fileName)
		if junkStem(showName) {
			return nil, nil, false
		}
		norm = parser.Normalize(showName)
		if norm == "" {
			return nil, nil, false
		}
	}

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

		Raw:      storage.RawRecord{Text: text},
		Variants: altVariants(msg, doc),
	}
	return item, parsed.Multi, true
}

func altVariants(msg *tg.Message, main *tg.Document) []storage.SourceVariant {
	media, ok := msg.Media.(*tg.MessageMediaDocument)
	if !ok || media == nil {
		return nil
	}
	var out []storage.SourceVariant
	seen := map[int64]bool{}
	if main != nil {
		seen[main.ID] = true
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
