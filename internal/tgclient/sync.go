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

// isDramaChannel 只采集频道名包含「短剧」的频道
func isDramaChannel(name string) bool {
	return strings.Contains(name, "短剧")
}

const (
	// syncWorkers 频道级并发：各频道互不依赖，网络等待可以重叠；
	// 数据库只有一个连接，写入仍然串行，3 个并发足够把吞吐顶到网络上限。
	syncWorkers = 3
	// maxScanBuffer 单个频道一次缓存多少条历史消息再按时间顺序入库。
	// Telegram 只支持「从新往旧」翻页，要按「最早 → 最新」入库只能先扫一遍；
	// 一个频道几万条消息会占几十 MB 内存，超过这里就分段处理。
	maxScanBuffer = 12000
	// ingestPageSize 每页入库条数（一个事务）
	ingestPageSize = 100
)

// KickChatSync 在后台补拉单个频道：先补离线期间的新消息，再按「最早 → 最新」回填历史。
// 频道管理面板「启用」或「立即同步」时调用；同一频道同时只跑一份任务。
// 返回 false 表示没启动（未登录，或该频道已经在同步中）。
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
		// 大频道历史回填可能很久，给足时间；进程退出时由 context.Background 自然结束
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

// ChatSyncing 该频道是否正在后台同步（管理面板用来显示状态）
func (s *Service) ChatSyncing(tgChatID int64) bool {
	s.syncChatMu.Lock()
	defer s.syncChatMu.Unlock()
	return s.syncChats[tgChatID]
}

// RefreshChats 立即向 Telegram 拉一次对话列表，把新加入或刚改名的频道补进库。
// 频道管理面板打开时调用：只更新聊天元数据（名字、用户名、access_hash），
// UpsertChat 不会覆盖已手动开关的 enabled，所以不会把停用的频道重新打开。
// 加锁职责在 refreshDialogs 内部，这里不必重复持锁。
func (s *Service) RefreshChats(ctx context.Context) error {
	api := s.API()
	if api == nil || !s.IsAuthorized() {
		return errors.New("未登录 Telegram，无法更新频道")
	}
	return s.refreshDialogs(ctx, api)
}

// startHistorySync 同步全部频道：先补拉离线期间的新消息，再从最新往最旧回填历史；
// 每个频道内部严格按「最早 → 最新」的顺序入库。
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
				// 频道可能在同步过程中被关闭：每处理一个频道前确认一次启用状态，
				// 关闭后立刻停止采集（已入库的数据只是被隐藏）。
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

// catchUpChat 只拉取比 last_top_id 更新的消息，避免每次启动重扫整个频道。
// 首次同步（LastTopID=0）没有基准点，交给 syncChatHistory 从头回填。
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
		// 全局最小间隔：一次补拉可能连着翻几十页，不节流就是一条密集请求线
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
		// 升序入库，保证同组分段编号与集号推进顺序正确
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
		// 不足一页说明已经追上 last_top_id，停止
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

// toMessages 过滤掉服务消息等非普通消息
func toMessages(list []tg.MessageClass) []*tg.Message {
	out := make([]*tg.Message, 0, len(list))
	for _, mc := range list {
		if msg, ok := mc.(*tg.Message); ok {
			out = append(out, msg)
		}
	}
	return out
}

// syncChatHistory 回填频道历史，严格按「频道最早 → 最新」的顺序入库。
//
// Telegram 的 messages.getHistory 只能从新往旧翻页（OffsetID 之前的消息），
// 没有「从最旧开始」的参数，所以先整段扫描缓存到内存，再从旧到新分批入库：
// 集号/相册分段/相册 caption 继承都依赖时间顺序推进，倒序入库会把范围帖、
// 相册成员分批错位。回填方向（往更旧的历史走）不变，断点仍然是 last_offset_id。
func (s *Service) syncChatHistory(ctx context.Context, api *tg.Client, c *storage.Chat) int {
	page, truncated := s.scanHistory(ctx, api, c)
	if len(page) == 0 {
		return 0
	}
	// 扫描结果是「从新到旧」，反过来就是频道的时间顺序
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
	// 记录断点：下次从这个更旧的位置继续往历史方向回填
	_ = s.db.UpdateChatSyncPoint(c.TgChatID, int64(oldest.ID))
	// 记录最新消息 ID：本次进程之后只补拉比它更新的内容
	_ = s.db.UpdateChatTopMsg(c.TgChatID, int64(newest.ID))
	if truncated {
		log.Printf("频道 %s 历史超过 %d 条，已分段回填至消息 %d", c.Name, maxScanBuffer, oldest.ID)
	} else {
		log.Printf("频道 %s 历史回填完成：%d 条消息（%d → %d）", c.Name, len(page), oldest.ID, newest.ID)
	}
	return inserted
}

// scanHistory 从断点往更旧的方向扫完整个频道，返回按消息 ID 从新到旧的消息。
// 第二个返回值表示是否因为缓存上限提前停止。
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

// refreshDialogs 分页拉取全部对话，只保存频道/超级群。
// dialogsMu 保证「打开频道管理」与启动同步等入口不会并发翻同一份对话列表。
func (s *Service) refreshDialogs(ctx context.Context, api *tg.Client) error {
	s.dialogsMu.Lock()
	defer s.dialogsMu.Unlock()
	var (
		offsetID   int
		offsetPeer tg.InputPeerClass = &tg.InputPeerEmpty{}
	)
	// 这一轮对话列表里出现的频道：全部翻完之后，库里不在其中的频道说明已经退出，
	// 需要连同数据一起删掉，否则面板会一直显示「已启用」。
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
			// 拿不到标准列表时不做「频道已退出」的判断，避免误删
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

// pruneMissingChats 删掉已经不在 Telegram 对话列表里的频道：账号退出/被移出的
// 频道如果继续留在库里，面板会一直显示「已启用」，采集也会对不存在的频道反复报错。
// 连同该频道已入库的来源、剧集、预览图和本地文件一起删，顺带省出空间。
func (s *Service) pruneMissingChats(seen map[int64]bool) {
	chats, err := s.db.ListChats()
	if err != nil {
		log.Printf("检查已退出频道失败：%v", err)
		return
	}
	purged := 0
	for _, c := range chats {
		if seen[c.TgChatID] {
			// 停用的频道不再保留采集数据（用户要求关闭频道即释放空间），
			// 频道行留着，重新启用后从头同步。
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
	// 一次性回收空间，比每个频道都 VACUUM 快得多
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

// pageCaptions 收集一页消息里每个相册（grouped_id）的非空 caption。
// 频道常把 caption 挂在相册最后一条，按 message_id 升序先处理组内成员时
// 库里还查不到 caption，这些成员会被当成「无字幕垃圾视频」丢弃。
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
