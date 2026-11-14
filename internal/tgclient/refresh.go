package tgclient

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"tgdrama/internal/storage"
)

func (s *Service) RefreshSource(ctx context.Context, src *storage.Source) (*storage.Source, error) {
	doc, alts, err := s.messageDocs(ctx, s.API(), src.ChatID, src.MessageID)
	if err != nil {
		return nil, err
	}
	if err := s.db.UpdateSourceLocation(src.ID, doc.ID, doc.AccessHash, doc.FileReference); err != nil {
		return nil, err
	}
	syncVariants(s.db, src.ID, alts)
	return s.db.GetSource(src.ID)
}

const variantFetchGap = 350 * time.Millisecond

const variantRetryCooldown = 2 * time.Minute

var variantGate pacer

var (
	variantMu    sync.Mutex
	variantRetry = map[int64]time.Time{}
)

func VariantsCoolingDown(sourceID int64) bool {
	variantMu.Lock()
	defer variantMu.Unlock()
	at, ok := variantRetry[sourceID]
	if !ok {
		return false
	}
	if time.Now().After(at) {
		delete(variantRetry, sourceID)
		return false
	}
	return true
}

func markVariantFailure(sourceID int64) {
	variantMu.Lock()
	variantRetry[sourceID] = time.Now().Add(variantRetryCooldown)
	variantMu.Unlock()
}

func (s *Service) RefreshSourceVariants(ctx context.Context, src *storage.Source) ([]*storage.SourceVariant, error) {
	api := s.API()
	if api == nil {
		return nil, ErrNotAuthorized
	}
	if !variantGate.wait(ctx, variantFetchGap) {
		return nil, ctx.Err()
	}
	doc, alts, err := s.messageDocs(ctx, api, src.ChatID, src.MessageID)
	if err != nil {
		markVariantFailure(src.ID)
		return nil, err
	}

	if doc.ID != 0 {
		_ = s.db.UpdateSourceLocation(src.ID, doc.ID, doc.AccessHash, doc.FileReference)
	}
	if err := s.db.ReplaceSourceVariantsByID(src.ID, alts); err != nil {
		return nil, err
	}
	if err := s.db.MarkVariantsChecked(src.ID); err != nil {
		return nil, err
	}
	return s.db.ListSourceVariants(src.ID)
}

func (s *Service) RefreshVariant(ctx context.Context, variantID int64) (*storage.SourceVariant, error) {
	v, err := s.db.GetSourceVariant(variantID)
	if err != nil {
		return nil, err
	}
	src, err := s.db.GetSource(v.SourceID)
	if err != nil {
		return nil, err
	}
	if _, err := s.RefreshSourceVariants(ctx, src); err != nil {
		return nil, err
	}
	return s.db.GetSourceVariant(variantID)
}

func syncVariants(db *storage.DB, sourceID int64, alts []storage.SourceVariant) {
	if len(alts) == 0 {
		return
	}
	_ = db.ReplaceSourceVariantsByID(sourceID, alts)
	_ = db.MarkVariantsChecked(sourceID)
}

func (s *Service) messageDoc(ctx context.Context, api *tg.Client, chatPK int64, messageID int) (*tg.Document, error) {
	doc, _, err := s.messageDocs(ctx, api, chatPK, messageID)
	return doc, err
}

func (s *Service) messageDocs(ctx context.Context, api *tg.Client, chatPK int64, messageID int) (*tg.Document, []storage.SourceVariant, error) {
	if api == nil {
		return nil, nil, fmt.Errorf("Telegram 客户端未就绪")
	}
	chat, err := s.db.GetChat(chatPK)
	if err != nil {
		return nil, nil, err
	}

	res, err := api.ChannelsGetMessages(ctx, &tg.ChannelsGetMessagesRequest{
		Channel: &tg.InputChannel{
			ChannelID:  chat.TgChatID,
			AccessHash: chat.AccessHash,
		},
		ID: []tg.InputMessageClass{&tg.InputMessageID{ID: messageID}},
	})
	if err != nil {
		return nil, nil, err
	}

	var msgs []tg.MessageClass
	switch m := res.(type) {
	case *tg.MessagesMessages:
		msgs = m.Messages
	case *tg.MessagesMessagesSlice:
		msgs = m.Messages
	case *tg.MessagesChannelMessages:

		msgs = m.Messages
	}
	for _, mc := range msgs {
		msg, ok := mc.(*tg.Message)
		if !ok || msg.ID != messageID {
			continue
		}
		media, ok := msg.Media.(*tg.MessageMediaDocument)
		if !ok {
			continue
		}
		doc, ok := media.Document.(*tg.Document)
		if !ok {
			continue
		}
		return doc, altVariants(msg, doc), nil
	}
	return nil, nil, fmt.Errorf("消息 %d 已不可用，无法刷新", messageID)
}
