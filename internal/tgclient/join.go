package tgclient

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"tgdrama/internal/storage"
)

type JoinResult struct {
	Joined  []string `json:"joined"`
	Already []string `json:"already"`
	Failed  []string `json:"failed"`
	FailErr []string `json:"failed_err"`
}

func (s *Service) JoinChannels(ctx context.Context, usernames []string) *JoinResult {
	res := &JoinResult{
		Joined:  []string{},
		Already: []string{},
		Failed:  []string{},
		FailErr: []string{},
	}
	for _, username := range usernames {
		username = strings.Trim(strings.TrimSpace(username), "@")
		if username == "" {
			continue
		}
		joined, err := s.joinOneChannel(ctx, username)
		if err != nil {
			res.Failed = append(res.Failed, username)
			res.FailErr = append(res.FailErr, err.Error())
			continue
		}
		if joined {
			res.Joined = append(res.Joined, username)
		} else {
			res.Already = append(res.Already, username)
		}
	}
	return res
}

func (s *Service) joinOneChannel(ctx context.Context, username string) (bool, error) {
	api := s.API()
	if api == nil {
		return false, ErrNotAuthorized
	}

	if !variantGate.wait(ctx, 300*time.Millisecond) {
		return false, ctx.Err()
	}
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
		Username: username,
	})
	if err != nil {
		return false, fmt.Errorf("解析 @%s 失败: %w", username, err)
	}

	var peer *tg.Channel
	for _, chat := range resolved.Chats {
		if ch, ok := chat.(*tg.Channel); ok && ch.ID == resolved.Peer.(*tg.PeerChannel).ChannelID {
			peer = ch
			break
		}
	}
	if peer == nil {
		return false, fmt.Errorf("@%s 不是频道", username)
	}

	joined := false
	_, err = api.ChannelsJoinChannel(ctx, &tg.InputChannel{
		ChannelID:  peer.ID,
		AccessHash: peer.AccessHash,
	})
	if err != nil {
		if tgerr.Is(err, "USER_ALREADY_PARTICIPANT") {
			joined = false
		} else {
			return false, fmt.Errorf("加入 @%s 失败: %w", username, err)
		}
	} else {
		joined = true
	}

	if _, uerr := s.db.UpsertChat(&storage.Chat{
		TgChatID:   peer.ID,
		AccessHash: peer.AccessHash,
		Name:       peer.Title,
		Username:   username,
		Type:       "channel",
		Enabled:    true,
	}); uerr != nil {
		return joined, fmt.Errorf("频道信息入库失败: %w", uerr)
	}
	if err := s.db.SetChatEnabled(peer.ID, true); err != nil {
		return joined, err
	}

	s.KickChatSync(peer.ID)
	return joined, nil
}
