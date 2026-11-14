package tgclient

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sort"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"tgdrama/internal/storage"
)

var (
	ErrNotAuthorized = errors.New("未登录 Telegram")

	ErrFillTooSoon = errors.New("刚刚搜索过，请稍后再试")
)

const (
	fillCooldown = 10 * time.Minute

	fillForceCooldown = 30 * time.Second

	fillMaxPages = 3

	fillPageLimit = 100
)

func (s *Service) FillMissingEpisodes(ctx context.Context, showID int64, showName string, force bool) (int, error) {
	api := s.API()
	if api == nil || !s.IsAuthorized() {
		return 0, ErrNotAuthorized
	}
	names := nameVariants(showName)
	if len(names) == 0 {
		return 0, fmt.Errorf("剧名为空")
	}
	if !s.beginFill(showID, force) {
		return 0, ErrFillTooSoon
	}

	failures := 0
	run := func(tag string, fn func() (int, error)) int {
		n, err := fn()
		if err != nil {
			failures++
			log.Printf("补全选集搜索失败（%s / %s）：%v", showName, tag, err)
		}
		return n
	}
	total := 0
	for _, n := range names {
		if total > 0 {
			break
		}
		if strings.HasPrefix(n, "#") {
			continue
		}
		total += run("hashtag "+n, func() (int, error) {
			return s.fillByQuery(ctx, api, "#"+n, false)
		})
	}
	for _, n := range names {
		if total > 0 {
			break
		}
		total += run("频道内 "+n, func() (int, error) {
			return s.fillInKnownChats(ctx, api, n)
		})
	}
	for _, n := range names {
		if total > 0 {
			break
		}
		total += run("裸名 "+n, func() (int, error) {
			return s.fillByQuery(ctx, api, n, true)
		})
	}
	if total == 0 && failures > 0 {
		return 0, fmt.Errorf("搜索 Telegram 失败，请检查网络或代理")
	}
	if total > 0 {
		log.Printf("补全选集：%s 补回 %d 条来源", showName, total)
	}
	return total, nil
}

func nameVariants(showName string) []string {
	raw := strings.Join(strings.Fields(strings.TrimSpace(showName)), " ")
	if raw == "" {
		return nil
	}
	compact := strings.ReplaceAll(raw, " ", "")
	if compact == raw {
		return []string{raw}
	}
	return []string{raw, compact}
}

func (s *Service) fillByQuery(ctx context.Context, api *tg.Client, query string, knownOnly bool) (int, error) {
	total := 0
	offsetID := 0
	var offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
	for page := 0; page < fillMaxPages; page++ {
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		res, err := api.MessagesSearchGlobal(ctx, &tg.MessagesSearchGlobalRequest{
			Q: query, Filter: &tg.InputMessagesFilterEmpty{},
			OffsetPeer: offsetPeer, OffsetID: offsetID, Limit: fillPageLimit,
		})
		if err != nil {
			if d, ok := tgerr.AsFloodWait(err); ok {
				if !sleepCtx(ctx, d+time.Second) {
					return total, ctx.Err()
				}
				page--
				continue
			}
			return total, err
		}
		msgs := toMessages(messagesOf(res))
		if len(msgs) == 0 {
			break
		}
		n, ierr := s.ingestSearchPage(ctx, res, msgs, knownOnly)
		total += n
		if ierr != nil {
			log.Printf("补全选集写入失败（%q）：%v", query, ierr)
		}
		last := msgs[len(msgs)-1]
		offsetID = last.ID
		offsetPeer = peerForSearch(last.PeerID, channelHashes(chatsOf(res)))
		if len(msgs) < fillPageLimit {
			break
		}
	}
	return total, nil
}

func (s *Service) ingestSearchPage(ctx context.Context, res tg.MessagesMessagesClass, msgs []*tg.Message, knownOnly bool) (int, error) {
	chats := chatsOf(res)
	grouped := make(map[int64][]*tg.Message, 8)
	order := make([]int64, 0, 8)
	for _, m := range msgs {
		pc, ok := m.PeerID.(*tg.PeerChannel)
		if !ok {
			continue
		}
		id := int64(pc.ChannelID)
		if _, seen := grouped[id]; !seen {
			order = append(order, id)
		}
		grouped[id] = append(grouped[id], m)
	}

	total := 0
	var firstErr error
	budget := fillAlbumLookups
	for _, id := range order {

		known, _ := s.db.GetChatByTgID(id)
		if knownOnly && (known == nil || known.ID == 0) {
			continue
		}
		chat, err := s.ensureChat(id, chats)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		if chat == nil {
			continue
		}
		group := s.expandAlbumMembers(ctx, chat, grouped[id], &budget)

		sort.SliceStable(group, func(i, j int) bool { return group[i].ID < group[j].ID })
		n, ierr := s.ingestPage(ctx, chat, group, pageCaptions(group))
		total += n
		if ierr != nil {
			log.Printf("搜索结果入库失败 chat=%s: %v", chat.Name, ierr)
			if firstErr == nil {
				firstErr = ierr
			}
		}
	}
	return total, firstErr
}

func (s *Service) ensureChat(tgID int64, chats []tg.ChatClass) (*storage.Chat, error) {
	if c, err := s.db.GetChatByTgID(tgID); err == nil && c != nil {
		return c, nil
	}
	for _, cls := range chats {
		ch, ok := cls.(*tg.Channel)
		if !ok || int64(ch.ID) != tgID {
			continue
		}
		chatType := "supergroup"
		if ch.Broadcast {
			chatType = "channel"
		}
		if _, err := s.db.UpsertChat(&storage.Chat{
			TgChatID:   tgID,
			AccessHash: ch.AccessHash,
			Name:       ch.Title,
			Username:   ch.Username,
			Type:       chatType,
			Enabled:    isDramaChannel(ch.Title),
		}); err != nil {
			return nil, err
		}
		return s.db.GetChatByTgID(tgID)
	}
	return nil, nil
}

func (s *Service) beginFill(showID int64, force bool) bool {
	s.fillMu.Lock()
	defer s.fillMu.Unlock()
	if s.fillAt == nil {
		s.fillAt = map[int64]time.Time{}
	}
	cooldown := fillCooldown
	if force {
		cooldown = fillForceCooldown
	}
	if t, ok := s.fillAt[showID]; ok && time.Since(t) < cooldown {
		return false
	}
	s.fillAt[showID] = time.Now()
	return true
}

func chatsOf(res tg.MessagesMessagesClass) []tg.ChatClass {
	switch m := res.(type) {
	case *tg.MessagesMessages:
		return m.Chats
	case *tg.MessagesMessagesSlice:
		return m.Chats
	case *tg.MessagesChannelMessages:
		return m.Chats
	}
	return nil
}

func channelHashes(chats []tg.ChatClass) map[int64]int64 {
	out := make(map[int64]int64, len(chats))
	for _, cls := range chats {
		if ch, ok := cls.(*tg.Channel); ok {
			out[ch.ID] = ch.AccessHash
		}
	}
	return out
}

func peerForSearch(p tg.PeerClass, hashes map[int64]int64) tg.InputPeerClass {
	if pc, ok := p.(*tg.PeerChannel); ok {
		return &tg.InputPeerChannel{ChannelID: pc.ChannelID, AccessHash: hashes[pc.ChannelID]}
	}
	return &tg.InputPeerEmpty{}
}

func (s *Service) fillInKnownChats(ctx context.Context, api *tg.Client, name string) (int, error) {
	chats, err := s.db.ListChats()
	if err != nil {
		return 0, err
	}
	total := 0
	budget := fillAlbumLookups
	for _, c := range chats {
		if c == nil || c.AccessHash == 0 {
			continue
		}

		if c.Type != "channel" && c.Type != "supergroup" {
			continue
		}
		select {
		case <-ctx.Done():
			return total, ctx.Err()
		default:
		}
		msgs := s.searchChatPages(ctx, api, c, name, fillMaxPages)
		if len(msgs) == 0 {
			continue
		}
		log.Printf("补全选集：频道 %s 命中 %d 条消息（%s）", c.Name, len(msgs), name)
		group := s.expandAlbumMembers(ctx, c, msgs, &budget)
		sort.SliceStable(group, func(i, j int) bool { return group[i].ID < group[j].ID })
		n, ierr := s.ingestPage(ctx, c, group, pageCaptions(group))
		total += n
		if ierr != nil {
			log.Printf("频道内搜索入库失败 chat=%s: %v", c.Name, ierr)
		}
	}
	return total, nil
}

func (s *Service) searchChatPages(ctx context.Context, api *tg.Client, c *storage.Chat, query string, maxPages int) []*tg.Message {
	peer := &tg.InputPeerChannel{ChannelID: c.TgChatID, AccessHash: c.AccessHash}
	out := make([]*tg.Message, 0, 64)
	offsetID := 0
	for page := 0; page < maxPages; page++ {
		select {
		case <-ctx.Done():
			return out
		default:
		}
		res, err := api.MessagesSearch(ctx, &tg.MessagesSearchRequest{
			Peer: peer, Q: query, Filter: &tg.InputMessagesFilterEmpty{},
			OffsetID: offsetID, Limit: fillPageLimit,
		})
		if err != nil {
			if d, ok := tgerr.AsFloodWait(err); ok {
				if !sleepCtx(ctx, d+time.Second) {
					return out
				}
				page--
				continue
			}
			log.Printf("频道内搜索失败 chat=%s q=%q: %v", c.Name, query, err)
			return out
		}
		msgs := toMessages(messagesOf(res))
		if len(msgs) == 0 {
			break
		}
		out = append(out, msgs...)
		if len(msgs) < fillPageLimit {
			break
		}
		offsetID = msgs[len(msgs)-1].ID
	}
	return out
}

const fillAlbumLookups = 40

func (s *Service) expandAlbumMembers(ctx context.Context, chat *storage.Chat, msgs []*tg.Message, budget *int) []*tg.Message {
	api := s.API()
	if api == nil || chat == nil || chat.AccessHash == 0 || *budget <= 0 {
		return msgs
	}
	byID := make(map[int]*tg.Message, len(msgs)+8)
	for _, m := range msgs {
		byID[m.ID] = m
	}
	done := make(map[int64]bool, 2)
	peer := &tg.InputPeerChannel{ChannelID: chat.TgChatID, AccessHash: chat.AccessHash}
	for _, m := range msgs {
		if m.GroupedID == 0 || done[m.GroupedID] {
			continue
		}
		done[m.GroupedID] = true
		if *budget <= 0 {
			break
		}
		*budget--

		res, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: peer, OffsetID: m.ID + 10, Limit: 21,
		})
		if err != nil {
			log.Printf("补齐相册成员失败 chat=%s grouped=%d: %v", chat.Name, m.GroupedID, err)
			continue
		}
		for _, one := range toMessages(messagesOf(res)) {
			if one.GroupedID != m.GroupedID || byID[one.ID] != nil {
				continue
			}
			byID[one.ID] = one
		}
	}
	out := make([]*tg.Message, 0, len(byID))
	for _, m := range byID {
		out = append(out, m)
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
