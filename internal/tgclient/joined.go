package tgclient

import (
	"context"
	"time"

	"github.com/gotd/td/tg"

	"tgdrama/internal/storage"
)

// joinedCacheTTL 推荐面板「是否已加入」结果的缓存时间。
// 这个接口每次点击都要问一次 TG（channels.getChannels），而「已加入/已退出」
// 在几分钟内不会变，短缓存能省掉反复点推荐产生的请求。
const joinedCacheTTL = 60 * time.Second

// JoinedChannels 检查这些频道当前是否仍在这个账号的对话里（没退出、没被踢）。
// 走 channels.getChannels：对已退出的频道返回的 Channel 会带 left 标志。
// 返回 nil 表示拿不到实时状态（未登录/网络失败），调用方自行回退本地记录。
func (s *Service) JoinedChannels(ctx context.Context, chats []*storage.Chat) map[int64]bool {
	if len(chats) == 0 {
		return map[int64]bool{}
	}
	s.joinedMu.Lock()
	if len(s.joinedVals) > 0 && time.Since(s.joinedAt) < joinedCacheTTL {
		out := make(map[int64]bool, len(s.joinedVals))
		for k, v := range s.joinedVals {
			out[k] = v
		}
		s.joinedMu.Unlock()
		return out
	}
	s.joinedMu.Unlock()

	api := s.API()
	if api == nil {
		return nil
	}
	in := make([]tg.InputChannelClass, 0, len(chats))
	for _, c := range chats {
		if c.TgChatID == 0 {
			continue
		}
		in = append(in, &tg.InputChannel{ChannelID: c.TgChatID, AccessHash: c.AccessHash})
	}
	if len(in) == 0 {
		return map[int64]bool{}
	}
	// 推荐面板是交互路径，不能让一次请求把页面卡住：超时就退回本地判断
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	res, err := api.ChannelsGetChannels(ctx, in)
	if err != nil {
		return nil
	}
	out := make(map[int64]bool, len(in))
	for _, chat := range res.GetChats() {
		ch, ok := chat.(*tg.Channel)
		if !ok {
			continue
		}
		out[ch.ID] = !ch.Left
	}
	s.joinedMu.Lock()
	s.joinedVals = out
	s.joinedAt = time.Now()
	s.joinedMu.Unlock()
	return out
}
