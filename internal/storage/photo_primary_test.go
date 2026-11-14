package storage

import (
	"path/filepath"
	"testing"
)

func TestListShowPhotosUsesSingleEnabledChat(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()

	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.db.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	mustExec(`INSERT INTO chats (id, tg_chat_id, name, enabled, created_at) VALUES (1, 101, 'A', 1, 0)`)
	mustExec(`INSERT INTO chats (id, tg_chat_id, name, enabled, created_at) VALUES (2, 102, 'B', 1, 0)`)
	mustExec(`INSERT INTO chats (id, tg_chat_id, name, enabled, created_at) VALUES (3, 103, 'C', 0, 0)`)
	mustExec(`INSERT INTO shows (id, name, norm_name, created_at, updated_at) VALUES (1, '测试剧', '测试剧', 0, 0)`)

	add := func(chatID, msgID int64) {
		mustExec(`INSERT INTO show_photos (show_id, chat_id, message_id, doc_id, width, height, created_at)
		          VALUES (1, ?, ?, ?, 720, 1280, 0)`, chatID, msgID, chatID*1000+msgID)
	}
	for i := int64(1); i <= 2; i++ {
		add(1, i)
	}
	for i := int64(1); i <= 3; i++ {
		add(2, i)
	}
	for i := int64(1); i <= 5; i++ {
		add(3, i)
	}

	photos, err := d.ListShowPhotos(1)
	if err != nil {
		t.Fatalf("list photos: %v", err)
	}
	if len(photos) != 3 {
		t.Fatalf("应只返回图最多的启用频道 B 的 3 张，得到 %d 张", len(photos))
	}

	locs, err := d.ListShowPhotoLocs(1)
	if err != nil {
		t.Fatalf("list locs: %v", err)
	}
	if len(locs) != 3 {
		t.Fatalf("预热定位应只有 3 条，得到 %d 条", len(locs))
	}
	chats := map[int64]bool{}
	for _, l := range locs {
		chats[l.ChatID] = true
	}
	if len(chats) != 1 || !chats[2] {
		t.Fatalf("预热只能落在一个频道（应为首选频道 2），得到 %v", chats)
	}

	mustExec(`UPDATE chats SET enabled = 0 WHERE id = 2`)
	photos, err = d.ListShowPhotos(1)
	if err != nil {
		t.Fatalf("list photos after disable: %v", err)
	}
	if len(photos) != 2 {
		t.Fatalf("B 关闭后应回落到 A 的 2 张，得到 %d 张", len(photos))
	}

	mustExec(`UPDATE chats SET enabled = 0 WHERE id = 1`)
	photos, err = d.ListShowPhotos(1)
	if err != nil {
		t.Fatalf("list photos all disabled: %v", err)
	}
	if len(photos) != 0 {
		t.Fatalf("所有频道关闭后不应返回图片，得到 %d 张", len(photos))
	}
}
