package tgclient

import (
	"context"
	"errors"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"tgdrama/internal/storage"
)

func isDramaChannel(name string) bool {
	return strings.Contains(name, "短剧")
}

const (
	syncWorkers = 3

	maxScanBuffer = 12000

	ingestPageSize = 100
)

func (s *Service) KickChatSync(tgChatID int64) bool {
	api := s.API()
	if api == nil || !s.IsAuthorized() {
		return false
	}
	if !s.beginChatSync(tgChatID) {
		return false
	}
	go func() {
		defer s.endChatSync(tgChatID)

		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Hour)
		defer cancel()
		c, err := s.db.GetChatByTgID(tgChatID)
		if err != nil || c == nil {
			log.Printf("频道同步：找不到频道 %d: %v", tgChatID, err)
			return
		}
		if !c.Enabled {
			log.Printf("频道 %s 已关闭，跳过同步", c.Name)
			return
		}
		log.Printf("频道 %s 开始后台同步…", c.Name)
		n := s.catchUpChat(ctx, api, c)
		n += s.syncChatHistory(ctx, api, c)
		log.Printf("频道 %s 后台同步完成：%d 条消息", c.Name, n)
	}()
	return true
}

func (s *Service) beginChatSync(tgChatID int64) bool {
	s.syncChatMu.Lock()
	defer s.syncChatMu.Unlock()
	if s.syncChats[tgChatID] {
		return false
	}
	s.syncChats[tgChatID] = true
	return true
}

func (s *Service) endChatSync(tgChatID int64) {
	s.syncChatMu.Lock()
	delete(s.syncChats, tgChatID)
	s.syncChatMu.Unlock()
}

func (s *Service) ChatSyncing(tgChatID int64) bool {
	s.syncChatMu.Lock()
	defer s.syncChatMu.Unlock()
	return s.syncChats[tgChatID]
}

func (s *Service) RefreshChats(ctx context.Context) error {
	api := s.API()
	if api == nil || !s.IsAuthorized() {
		return errors.New("未登录 Telegram，无法更新频道")
	}
	return s.refreshDialogs(ctx, api)
}

func (s *Service) startHistorySync(ctx context.Context, api *tg.Client) {
	if !s.markSyncing() {
		return
	}
	if err := s.refreshDialogs(ctx, api); err != nil {
		log.Printf("刷新对话列表失败: %v", err)
	}
	chats, err := s.db.ListEnabledChats()
	if err != nil {
		log.Printf("读取频道列表失败: %v", err)
		return
	}
	if len(chats) == 0 {
		return
	}

	var (
		mu    sync.Mutex
		total int
		wg    sync.WaitGroup
	)
	jobs := make(chan *storage.Chat)
	for i := 0; i < syncWorkers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for c := range jobs {
				if ctx.Err() != nil {
					return
				}

				if fresh, err := s.db.GetChat(c.ID); err != nil || !fresh.Enabled {
					continue
				}
				n := s.catchUpChat(ctx, api, c)
				n += s.syncChatHistory(ctx, api, c)
				mu.Lock()
				total += n
				mu.Unlock()
			}
		}()
	}
	for _, c := range chats {
		select {
		case <-ctx.Done():
			close(jobs)
			wg.Wait()
			return
		case jobs <- c:
		}
	}
	close(jobs)
	wg.Wait()
	log.Printf("同步完成：%d 个频道，新增/更新 %d 条消息", len(chats), total)
}

func (s *Service) catchUpChat(ctx context.Context, api *tg.Client, c *storage.Chat) int {
	top := c.LastTopID
	if top <= 0 {
		return 0
	}
	peer := &tg.InputPeerChannel{ChannelID: c.TgChatID, AccessHash: c.AccessHash}
	offsetID := 0
	inserted := 0
	for {
		select {
		case <-ctx.Done():
			return inserted
		default:
		}

		if !s.rpcGate.wait(ctx, historyPageGap) {
			return inserted
		}
		res, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: peer, OffsetID: offsetID, MinID: int(top), Limit: 100,
		})
		if err != nil {
			if d, ok := tgerr.AsFloodWait(err); ok {
				if !sleepCtx(ctx, d+time.Second) {
					return inserted
				}
				continue
			}
			log.Printf("补拉频道 %s 新消息失败: %v", c.Name, err)
			return inserted
		}
		msgs := messagesOf(res)
		if len(msgs) == 0 {
			return inserted
		}

		sort.SliceStable(msgs, func(i, j int) bool {
			return messageIDOf(msgs[i]) < messageIDOf(msgs[j])
		})
		page := toMessages(msgs)
		caps := pageCaptions(page)
		n, ierr := s.ingestPage(ctx, c, page, caps)
		if ierr != nil {
			log.Printf("入库失败 chat=%s: %v", c.Name, ierr)
		}
		inserted += n

		minID, maxID := 0, 0
		for _, msg := range page {
			if minID == 0 || msg.ID < minID {
				minID = msg.ID
			}
			if msg.ID > maxID {
				maxID = msg.ID
			}
		}
		if maxID > 0 {
			_ = s.db.UpdateChatTopMsg(c.TgChatID, int64(maxID))
			if int64(maxID) > c.LastTopID {
				c.LastTopID = int64(maxID)
			}
		}

		if len(msgs) < 100 || minID == 0 {
			return inserted
		}
		offsetID = minID
	}
}

func messagesOf(res tg.MessagesMessagesClass) []tg.MessageClass {
	switch m := res.(type) {
	case *tg.MessagesMessages:
		return m.Messages
	case *tg.MessagesMessagesSlice:
		return m.Messages
	case *tg.MessagesChannelMessages:
		return m.Messages
	}
	return nil
}

func toMessages(list []tg.MessageClass) []*tg.Message {
	out := make([]*tg.Message, 0, len(list))
	for _, mc := range list {
		if msg, ok := mc.(*tg.Message); ok {
			out = append(out, msg)
		}
	}
	return out
}

func (s *Service) syncChatHistory(ctx context.Context, api *tg.Client, c *storage.Chat) int {
	page, truncated := s.scanHistory(ctx, api, c)
	if len(page) == 0 {
		return 0
	}

	sort.SliceStable(page, func(i, j int) bool { return page[i].ID < page[j].ID })

	inserted := 0
	for start := 0; start < len(page); start += ingestPageSize {
		if ctx.Err() != nil {
			return inserted
		}
		end := start + ingestPageSize
		if end > len(page) {
			end = len(page)
		}
		part := page[start:end]
		n, err := s.ingestPage(ctx, c, part, pageCaptions(part))
		if err != nil {
			log.Printf("入库失败 chat=%s msg=%d..%d: %v", c.Name, part[0].ID, part[len(part)-1].ID, err)
		}
		inserted += n
	}

	oldest, newest := page[0], page[len(page)-1]

	_ = s.db.UpdateChatSyncPoint(c.TgChatID, int64(oldest.ID))

	_ = s.db.UpdateChatTopMsg(c.TgChatID, int64(newest.ID))
	if truncated {
		log.Printf("频道 %s 历史超过 %d 条，已分段回填至消息 %d", c.Name, maxScanBuffer, oldest.ID)
	} else {
		log.Printf("频道 %s 历史回填完成：%d 条消息（%d → %d）", c.Name, len(page), oldest.ID, newest.ID)
	}
	return inserted
}

func (s *Service) scanHistory(ctx context.Context, api *tg.Client, c *storage.Chat) ([]*tg.Message, bool) {
	peer := &tg.InputPeerChannel{ChannelID: c.TgChatID, AccessHash: c.AccessHash}
	offsetID := int(c.LastOffset)
	var out []*tg.Message
	pages := 0
	for {
		select {
		case <-ctx.Done():
			return out, false
		default:
		}
		if !s.rpcGate.wait(ctx, historyPageGap) {
			return out, false
		}
		res, err := api.MessagesGetHistory(ctx, &tg.MessagesGetHistoryRequest{
			Peer: peer, OffsetID: offsetID, Limit: 100,
		})
		if err != nil {
			if d, ok := tgerr.AsFloodWait(err); ok {
				if !sleepCtx(ctx, d+time.Second) {
					return out, false
				}
				continue
			}
			log.Printf("拉取频道 %s 历史失败: %v", c.Name, err)
			return out, false
		}
		msgs := messagesOf(res)
		if len(msgs) == 0 {
			return out, false
		}
		page := toMessages(msgs)
		out = append(out, page...)
		pages++
		if pages%10 == 0 {
			log.Printf("频道 %s：已扫描 %d 条历史消息…", c.Name, len(out))
		}
		minID := 0
		for _, msg := range page {
			if minID == 0 || msg.ID < minID {
				minID = msg.ID
			}
		}
		if len(msgs) < 100 || minID == 0 {
			return out, false
		}
		offsetID = minID
		if len(out) >= maxScanBuffer {
			return out, true
		}
	}
}

func (s *Service) refreshDialogs(ctx context.Context, api *tg.Client) error {
	s.dialogsMu.Lock()
	defer s.dialogsMu.Unlock()
	var (
		offsetID   int
		offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
	)

	seen := make(map[int64]bool)
	for {
		res, err := api.MessagesGetDialogs(ctx, &tg.MessagesGetDialogsRequest{
			OffsetID: offsetID, OffsetPeer: offsetPeer, Limit: 100,
		})
		if err != nil {
			if d, ok := tgerr.AsFloodWait(err); ok {
				if !sleepCtx(ctx, d+time.Second) {
					return ctx.Err()
				}
				continue
			}
			return err
		}

		var dialogs []tg.DialogClass
		var chats []tg.ChatClass
		switch d := res.(type) {
		case *tg.MessagesDialogs:
			dialogs, chats = d.Dialogs, d.Chats
		case *tg.MessagesDialogsSlice:
			dialogs, chats = d.Dialogs, d.Chats
		default:

			return nil
		}
		if err := s.persistDialogs(dialogs, chats, seen); err != nil {
			return err
		}
		if len(dialogs) < 100 {
			s.pruneMissingChats(seen)
			return nil
		}
		last, ok := dialogs[len(dialogs)-1].(*tg.Dialog)
		if !ok {
			s.pruneMissingChats(seen)
			return nil
		}
		offsetID = last.TopMessage
		offsetPeer = inputPeerOf(last.Peer, chats)
	}
}

func (s *Service) pruneMissingChats(seen map[int64]bool) {
	chats, err := s.db.ListChats()
	if err != nil {
		log.Printf("检查已退出频道失败：%v", err)
		return
	}
	purged := 0
	for _, c := range chats {
		if seen[c.TgChatID] {

			if !c.Enabled {
				if st, err := s.db.PurgeChatData(c.TgChatID); err != nil {
					log.Printf("清理停用频道「%s」数据失败：%v", c.Name, err)
				} else if st.Sources > 0 || st.Photos > 0 {
					purged++
					log.Printf("停用频道「%s」的数据已清理：视频 %d、剧集 %d、预览图 %d、剧 %d、文件 %d",
						c.Name, st.Sources, st.Episodes, st.Photos, st.Shows, st.Files)
				}
			}
			continue
		}
		st, err := s.db.DeleteChat(c.TgChatID)
		if err != nil {
			log.Printf("删除已退出频道「%s」失败：%v", c.Name, err)
			continue
		}
		purged++
		log.Printf("频道「%s」已不在账号的对话列表里，已删除入库数据：视频 %d、剧集 %d、预览图 %d、剧 %d、文件 %d",
			c.Name, st.Sources, st.Episodes, st.Photos, st.Shows, st.Files)
	}

	if purged > 0 {
		s.db.Vacuum()
	}
}

func (s *Service) persistDialogs(dialogs []tg.DialogClass, chats []tg.ChatClass, seen map[int64]bool) error {
	for _, dclass := range dialogs {
		d, ok := dclass.(*tg.Dialog)
		if !ok {
			continue
		}
		pc, ok := d.Peer.(*tg.PeerChannel)
		if !ok {
			continue
		}
		var channel *tg.Channel
		for _, c := range chats {
			if ch, good := c.(*tg.Channel); good && ch.ID == pc.ChannelID {
				channel = ch
				break
			}
		}
		if channel == nil {
			continue
		}
		chatType := "supergroup"
		if channel.Broadcast {
			chatType = "channel"
		}
		sc := &storage.Chat{
			TgChatID:   channel.ID,
			AccessHash: channel.AccessHash,
			Name:       channel.Title,
			Username:   channel.Username,
			Type:       chatType,
			Enabled:    isDramaChannel(channel.Title),
		}
		if _, err := s.db.UpsertChat(sc); err != nil {
			return err
		}
		seen[sc.TgChatID] = true
	}
	return nil
}

func inputPeerOf(p tg.PeerClass, chats []tg.ChatClass) tg.InputPeerClass {
	peer, ok := p.(*tg.PeerChannel)
	if !ok {
		return &tg.InputPeerEmpty{}
	}
	for _, c := range chats {
		if ch, good := c.(*tg.Channel); good && ch.ID == peer.ChannelID {
			return &tg.InputPeerChannel{ChannelID: ch.ID, AccessHash: ch.AccessHash}
		}
	}
	return &tg.InputPeerEmpty{}
}

func pageCaptions(msgs []*tg.Message) map[int64]string {
	caps := make(map[int64]string)
	for _, msg := range msgs {
		if msg.GroupedID == 0 || strings.TrimSpace(msg.Message) == "" {
			continue
		}
		if _, ok := caps[msg.GroupedID]; !ok {
			caps[msg.GroupedID] = msg.Message
		}
	}
	return caps
}

func messageIDOf(m tg.MessageClass) int {
	if msg, ok := m.(*tg.Message); ok {
		return msg.ID
	}
	return 0
}
