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
	// ErrNotAuthorized 未登录 Telegram，无法执行搜索
	ErrNotAuthorized = errors.New("未登录 Telegram")
	// ErrFillTooSoon 同一部剧刚搜索过，避免重复打接口
	ErrFillTooSoon = errors.New("刚刚搜索过，请稍后再试")
)

const (
	// fillCooldown 同一部剧两次自动补全之间的最小间隔
	fillCooldown = 10 * time.Minute
	// fillForceCooldown 用户手动点「搜索补全」时的最小间隔（比自动触发宽松）
	fillForceCooldown = 30 * time.Second
	// fillMaxPages 一次补全最多翻几页搜索结果
	fillMaxPages = 3
	// fillPageLimit 每页搜索结果条数
	fillPageLimit = 100
)

// FillMissingEpisodes 用「#剧名」在全账号范围内搜索，把媒体库里缺失的选集补回来。
//
// 频道连载时常常把某一集发在另一个频道、或者当初同步时因为空 caption 被丢掉，
// 详情页发现选集不连贯就调这里：全局搜 hashtag，按频道分组后走正常入库流程，
// 命中已存在的消息会被去重跳过，所以重复调用只补真正缺的部分。
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

	// 三轮搜索，前一轮补到东西就停：
	//   1. 全局 #剧名 —— 精确，只命中带标签的连载频道；
	//   2. 频道内搜索 —— 全局搜索依赖服务端索引，私有频道/冷门剧搜不到，
	//      逐个数已知频道搜最稳（这是「搜索补全无效」的主要修复）；
	//   3. 全局裸剧名 —— 范围最大，只写进媒体库已有频道，避免建出同名杂剧。
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

// nameVariants 返回可用于搜索的剧名写法：原样（空格归一）和去掉空格的紧凑写法。
// 例如 "The Forbidden Flower" → ["The Forbidden Flower", "TheForbiddenFlower"]；
// 中文剧名两种写法相同，只返回一个。
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

// fillByQuery 跑一轮全局搜索并入库，返回新增来源数。
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

// ingestSearchPage 把搜索结果按频道分组后走正常入库流程，返回新增来源条数。
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
		// 裸剧名搜索范围很大：只写进媒体库里已经存在的频道，避免把同名杂帖
		// 顺手建成一堆新剧（#标签 那轮仍然可以补建频道）。
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
		// 入库要求升序：相册分段编号与范围帖集号都依赖时间顺序
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

// ensureChat 找到（或补建）搜索结果所属频道的本地记录。
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

// beginFill 记录本次补全时间；冷却期内重复请求直接拒绝，避免详情页反复触发打爆接口。
// 手动补全（force）用更短的冷却，用户点一次就能立刻重试。
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

// chatsOf 取搜索结果里附带的频道信息（含 access_hash）
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

// channelHashes 收集搜索结果里频道的 access_hash，翻页参数需要它，否则会 PEER_ID_INVALID
func channelHashes(chats []tg.ChatClass) map[int64]int64 {
	out := make(map[int64]int64, len(chats))
	for _, cls := range chats {
		if ch, ok := cls.(*tg.Channel); ok {
			out[ch.ID] = ch.AccessHash
		}
	}
	return out
}

// peerForSearch 把消息的 peer 转成翻页参数需要的 input peer
func peerForSearch(p tg.PeerClass, hashes map[int64]int64) tg.InputPeerClass {
	if pc, ok := p.(*tg.PeerChannel); ok {
		return &tg.InputPeerChannel{ChannelID: pc.ChannelID, AccessHash: hashes[pc.ChannelID]}
	}
	return &tg.InputPeerEmpty{}
}

// fillInKnownChats 在媒体库已收录的频道里逐个搜索剧名。
//
// Telegram 的全局搜索（messages.searchGlobal）只覆盖账号能看到的公开索引，
// 私有频道 / 未加频道的消息搜不到，结果常常是「搜索补全无效」。频道内搜索
// （messages.search + 该频道 peer）不受这个限制，而且范围小、命中率高。
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
		// messages.search 的 peer 必须是真的频道/超级群
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

// searchChatPages 在单个频道里翻页搜索，返回命中的消息。
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

// fillAlbumLookups 一次补全最多为多少个相册回拉历史（每个相册一次请求）
const fillAlbumLookups = 40

// expandAlbumMembers 把搜索结果里的相册补全成整组。
//
// Telegram 全局搜索只返回文本命中（带 caption）的那一条，相册里没有 caption
// 的成员不会出现在结果里。典型症状：频道发「《淫魔事务所》第7-8集」两条视频，
// 搜索只带回第 8 集那条（caption 挂在它上面），补全后仍然缺第 7 集。
// 这里对每个相册按消息 ID 往回拉一小段历史，把同 grouped_id 的成员补齐，
// 入库时再按组内序号分集，7、8 两集就都回来了。
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
		// getHistory 只往回翻（ID 小于 OffsetID），而相册的 caption 可能挂在
		// 第一条上（成员 ID 更大），所以窗口要跨过 m.ID 两侧：相册最多 10 条，
		// OffsetID = m.ID+10 就能把 m.ID-10 .. m.ID+10 全取回来。
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
