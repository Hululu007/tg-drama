package storage

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPurgeChatDataKeepsSharedEpisodes(t *testing.T) {
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
	mustExec(`INSERT INTO chats (id, tg_chat_id, name, enabled, created_at) VALUES (1, 101, '要删的频道', 1, 0)`)
	mustExec(`INSERT INTO chats (id, tg_chat_id, name, enabled, created_at) VALUES (2, 102, '保留的频道', 1, 0)`)

	mustExec(`INSERT INTO shows (id, name, norm_name, created_at, updated_at) VALUES (1, '共享剧', '共享剧', 0, 0)`)
	mustExec(`INSERT INTO episodes (id, show_id, episode_no, created_at, updated_at) VALUES (1, 1, 1, 0, 0)`)
	mustExec(`INSERT INTO sources (id, episode_id, chat_id, message_id, created_at) VALUES (1, 1, 1, 10, 0)`)
	mustExec(`INSERT INTO sources (id, episode_id, chat_id, message_id, created_at) VALUES (2, 1, 2, 20, 0)`)
	mustExec(`INSERT INTO show_photos (show_id, chat_id, message_id, doc_id, created_at) VALUES (1, 1, 11, 1001, 0)`)

	mustExec(`INSERT INTO shows (id, name, norm_name, created_at, updated_at) VALUES (2, '独有剧', '独有剧', 0, 0)`)
	mustExec(`INSERT INTO episodes (id, show_id, episode_no, created_at, updated_at) VALUES (2, 2, 1, 0, 0)`)
	mustExec(`INSERT INTO sources (id, episode_id, chat_id, message_id, created_at) VALUES (3, 2, 1, 12, 0)`)
	mustExec(`INSERT INTO show_photos (show_id, chat_id, message_id, doc_id, created_at) VALUES (2, 1, 13, 1002, 0)`)
	mustExec(`INSERT INTO history (episode_id, position, duration, updated_at) VALUES (2, 12, 100, 0)`)
	mustExec(`INSERT INTO favorites (show_id, created_at) VALUES (2, 0)`)

	st, err := d.PurgeChatData(101)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if st.Sources != 2 || st.Episodes != 1 || st.Photos != 2 || st.Shows != 1 {
		t.Fatalf("统计不对: %+v", st)
	}

	count := func(q string, args ...any) int {
		t.Helper()
		var n int
		if err := d.db.QueryRow(q, args...).Scan(&n); err != nil {
			t.Fatalf("count: %v", err)
		}
		return n
	}
	if n := count(`SELECT COUNT(*) FROM sources WHERE chat_id = 1`); n != 0 {
		t.Fatalf("频道 1 的来源应清空，剩 %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM show_photos WHERE chat_id = 1`); n != 0 {
		t.Fatalf("频道 1 的预览图应清空，剩 %d", n)
	}
	if n := count(`SELECT COUNT(*) FROM episodes WHERE id = 1`); n != 1 {
		t.Fatalf("共享剧集被误删")
	}
	if n := count(`SELECT COUNT(*) FROM shows WHERE id = 1`); n != 1 {
		t.Fatalf("共享剧被误删")
	}
	if n := count(`SELECT COUNT(*) FROM episodes WHERE id = 2`); n != 0 {
		t.Fatalf("独有剧集应删除")
	}
	if n := count(`SELECT COUNT(*) FROM shows WHERE id = 2`); n != 0 {
		t.Fatalf("独有剧应删除")
	}
	if n := count(`SELECT COUNT(*) FROM history WHERE episode_id = 2`); n != 0 {
		t.Fatalf("删除剧集时观看记录应级联删除")
	}
	if n := count(`SELECT COUNT(*) FROM favorites WHERE show_id = 2`); n != 0 {
		t.Fatalf("删除剧时收藏应级联删除")
	}

	if n := count(`SELECT COUNT(*) FROM chats WHERE tg_chat_id = 101`); n != 1 {
		t.Fatalf("频道行不该被删")
	}
}

func TestDeleteChatRemovesRow(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer d.Close()
	if _, err := d.db.Exec(`INSERT INTO chats (id, tg_chat_id, name, enabled, created_at) VALUES (1, 101, '已退出', 1, 0)`); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := d.DeleteChat(101); err != nil {
		t.Fatalf("delete: %v", err)
	}
	var n int
	if err := d.db.QueryRow(`SELECT COUNT(*) FROM chats`).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	if n != 0 {
		t.Fatalf("频道行应删除")
	}
	if _, err := d.DeleteChat(999); err == nil {
		t.Fatalf("不存在的频道应报错")
	}
}

func TestPurgeChatDataRemovesOnlyFilesUnderDataDir(t *testing.T) {
	root := t.TempDir()
	dataDir := filepath.Join(root, "data")
	if err := os.MkdirAll(filepath.Join(dataDir, "photos"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	inside := filepath.Join(dataDir, "photos", "a.jpg")
	outside := filepath.Join(root, "covers", "b.jpg")
	if err := os.MkdirAll(filepath.Dir(outside), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	for _, f := range []string{inside, outside} {
		if err := os.WriteFile(f, []byte("x"), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	d, err := Open(filepath.Join(dataDir, "test.db"))
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
	mustExec(`INSERT INTO chats (id, tg_chat_id, name, enabled, created_at) VALUES (1, 101, '频道', 1, 0)`)
	mustExec(`INSERT INTO shows (id, name, norm_name, cover_path, created_at, updated_at) VALUES (1, '剧', '剧', ?, 0, 0)`, outside)
	mustExec(`INSERT INTO episodes (id, show_id, episode_no, created_at, updated_at) VALUES (1, 1, 1, 0, 0)`)
	mustExec(`INSERT INTO sources (id, episode_id, chat_id, message_id, file_path, created_at) VALUES (1, 1, 1, 5, ?, 0)`, inside)

	st, err := d.PurgeChatData(101)
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if st.Files != 1 {
		t.Fatalf("只该删 data 目录内的 1 个文件，实际 %d", st.Files)
	}
	if _, err := os.Stat(inside); !os.IsNotExist(err) {
		t.Fatalf("data 目录内的视频文件应删除")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("data 目录外的封面文件不能删: %v", err)
	}
}
