package tgclient

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/gotd/td/tg"

	"tgdrama/internal/storage"
)

// RefreshSource 重新拉取频道消息刷新 file_reference，实现 download.Refresher。
// 顺带同步消息里的其它清晰度（alt_documents），播放页不必再单独查一次。
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

// variantFetchGap 两次按需查询清晰度之间的最小间隔。
// 与其它只读 RPC 同级，避免翻播放页时把请求打成密集直线。
const variantFetchGap = 350 * time.Millisecond

const variantRetryCooldown = 2 * time.Minute

// variantGate 按需查询清晰度的全局节流闸门（零值可用）。
var variantGate pacer

// variantCool 同一条来源查询失败后的冷却，避免反复打开同一集时连续打请求。
var (
	variantMu    sync.Mutex
	variantRetry = map[int64]time.Time{}
)

// VariantsCoolingDown 该来源是否在失败冷却期内。
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

// RefreshSourceVariants 按需补齐某条来源的清晰度列表（播放页首次打开时调用）。
// 只发一次 messages.getMessages，结果落库并标记 variants_checked；成功后不再重复请求。
// 失败返回错误且不写标记，下次打开可再试（受 variantRetryCooldown 限制）。
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
	// 主文档引用也可能顺带过期，一起刷新
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

// RefreshVariant 刷新一个清晰度变体的 file_reference，返回更新后的记录。
// 变体引用独立于主文档，一个过期不代表另一个也过期；刷新时按来源重拉一次消息，
// 再把同一 doc_id 的新引用取回来。
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

// syncVariants 把刷新到的副文档写库；失败不影响主流程（下次按需查询会再补）。
func syncVariants(db *storage.DB, sourceID int64, alts []storage.SourceVariant) {
	if len(alts) == 0 {
		return
	}
	_ = db.ReplaceSourceVariantsByID(sourceID, alts)
	_ = db.MarkVariantsChecked(sourceID)
}

// messageDoc 重新拉取一条频道消息，返回其中视频文档的最新定位信息。
// file_reference 会随服务端状态过期（FILE_REFERENCE_EXPIRED），库里存的是入库当时的
// 引用，过期后必须重新拉消息才能拿到可用的新引用——播放源刷新与封面回填都走它。
func (s *Service) messageDoc(ctx context.Context, api *tg.Client, chatPK int64, messageID int) (*tg.Document, error) {
	doc, _, err := s.messageDocs(ctx, api, chatPK, messageID)
	return doc, err
}

// messageDocs 同 messageDoc，并带回同一条消息里的其它清晰度版本。
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
		// 频道消息返回 messages.channelMessages
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
