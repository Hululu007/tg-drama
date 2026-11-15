package tgclient

import (
	"path/filepath"
	"testing"

	"tgdrama/internal/storage"
)

// 账号里已经退出的频道不能继续留在库里，否则面板一直显示「已启用」；
// 拉不到对话列表时（错误）不调用这个函数，所以这里只测「确认退出」的路径。
func TestPruneMissingChatsDropsDepartedChats(t *testing.T) {
	db, err := storage.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	for _, c := range []*storage.Chat{
		{TgChatID: 101, Name: "还在的频道", Enabled: true},
		{TgChatID: 102, Name: "已退出的频道", Enabled: true},
	} {
		if _, err := db.UpsertChat(c); err != nil {
			t.Fatalf("seed chat: %v", err)
		}
	}

	s := &Service{db: db}
	s.pruneMissingChats(map[int64]bool{101: true})

	chats, err := db.ListChats()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(chats) != 1 || chats[0].TgChatID != 101 {
		t.Fatalf("应只留下还在的频道，实际 %+v", chats)
	}
}
