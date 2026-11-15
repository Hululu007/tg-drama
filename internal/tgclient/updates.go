package tgclient

import (
	"context"
	"log"

	"github.com/gotd/td/tg"

	"tgdrama/internal/storage"
)

func (s *Service) handleNewMessage(ctx context.Context, e tg.Entities, update *tg.UpdateNewMessage) error {
	msg, ok := update.Message.(*tg.Message)
	if !ok {
		return nil
	}
	return s.handleRealTimeMessage(ctx, msg, e)
}

func (s *Service) handleNewChannelMessage(ctx context.Context, e tg.Entities, update *tg.UpdateNewChannelMessage) error {
	msg, ok := update.Message.(*tg.Message)
	if !ok {
		return nil
	}
	return s.handleRealTimeMessage(ctx, msg, e)
}

// handleRealTimeMessage 新消息实时入库，持续更新
func (s *Service) handleRealTimeMessage(ctx context.Context, msg *tg.Message, e tg.Entities) error {
	pc, ok := msg.PeerID.(*tg.PeerChannel)
	if !ok {
		return nil
	}
	var channel *tg.Channel
	if e.Channels != nil {
		channel = e.Channels[pc.ChannelID]
	}
	if channel == nil {
		log.Printf("实时消息缺少频道实体，跳过 msg=%d", msg.ID)
		return nil
	}

	chatType := "supergroup"
	if channel.Broadcast {
		chatType = "channel"
	}
	chatPK, err := s.db.UpsertChat(&storage.Chat{
		TgChatID:   channel.ID,
		AccessHash: channel.AccessHash,
		Name:       channel.Title,
		Username:   channel.Username,
		Type:       chatType,
		Enabled:    isDramaChannel(channel.Title),
	})
	if err != nil {
		return err
	}
	chat, err := s.db.GetChat(chatPK)
	if err != nil {
		return err
	}
	if !chat.Enabled {
		return nil
	}
	log.Printf("收到新视频: %s -> %s", chat.Name, truncateText(msg.Message, 40))
	if err := s.ingestMessage(ctx, chat, msg, ""); err != nil {
		return err
	}
	// 实时消息也要推进最新游标，避免下次启动重复补拉
	_ = s.db.UpdateChatTopMsg(chat.TgChatID, int64(msg.ID))
	return nil
}

func truncateText(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n])
}
