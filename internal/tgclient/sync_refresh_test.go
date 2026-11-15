package tgclient

import (
	"context"
	"testing"
)

// TestRefreshChatsRequiresLogin 未登录时刷新频道列表应直接报错，
// 让前端回退到本地列表而不是一直等 Telegram。
func TestRefreshChatsRequiresLogin(t *testing.T) {
	s := &Service{}
	if err := s.RefreshChats(context.Background()); err == nil {
		t.Fatal("未登录时应返回错误")
	}
}
