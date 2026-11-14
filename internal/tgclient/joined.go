package tgclient

import (
	"context"
	"time"

	"github.com/gotd/td/tg"

	"tgdrama/internal/storage"
)

const joinedCacheTTL = 60 * time.Second

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
