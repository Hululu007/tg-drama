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

// JoinResult 一键加入推荐频道的汇总结果。
type JoinResult struct {
	Joined  []string `json:"joined"`     // 本次成功加入
	Already []string `json:"already"`    // 加入前就已是成员
	Failed  []string `json:"failed"`     // 加入失败（带用户名）
	FailErr []string `json:"failed_err"` // 与 Failed 一一对应的错误
}

// JoinChannels 逐个加入指定用户名的公开频道：
//   - contacts.resolveUsername 解析出频道与 access_hash；
//   - channels.joinChannel 加入（已是成员返回 USER_ALREADY_PARTICIPANT，视为成功）；
//   - upsert 频道信息并强制启用，随后踢一次历史回填，首页马上有内容。
//
// 每个频道独立处理：单个失败不影响其余。
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

// joinOneChannel 加入单个频道；返回 joined=true 表示本次真正加入了。
func (s *Service) joinOneChannel(ctx context.Context, username string) (bool, error) {
	api := s.API()
	if api == nil {
		return false, ErrNotAuthorized
	}
	// 用户名限流：三个推荐频道一次用完，间隔几十毫秒足够
	if !variantGate.wait(ctx, 300*time.Millisecond) {
		return false, ctx.Err()
	}
	resolved, err := api.ContactsResolveUsername(ctx, &tg.ContactsResolveUsernameRequest{
		Username: username,
	})
	if err != nil {
		return false, fmt.Errorf("解析 @%s 失败: %w", username, err)
	}
	// Peer 是 peerChannel 引用，实际频道信息在 Chats 列表里
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
			joined = false // 已是成员
		} else {
			return false, fmt.Errorf("加入 @%s 失败: %w", username, err)
		}
	} else {
		joined = true
	}

	// 频道信息入库并强制启用（停用过的推荐频道也一并恢复）
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
	// 新加入/启用的频道立刻回填历史，首页马上有内容
	s.KickChatSync(peer.ID)
	return joined, nil
}
