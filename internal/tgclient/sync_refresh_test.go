package tgclient

import (
	"context"
	"testing"
)

func TestRefreshChatsRequiresLogin(t *testing.T) {
	s := &Service{}
	if err := s.RefreshChats(context.Background()); err == nil {
		t.Fatal("未登录时应返回错误")
	}
}
