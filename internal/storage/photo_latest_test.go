package storage

import (
	"path/filepath"
	"testing"
)

func TestListShowPhotosLatestEpisodesOnly(t *testing.T) {
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
	mustExec(`INSERT INTO shows (id, name, norm_name, created_at, updated_at) VALUES (1, '测试剧', '测试剧', 0, 0)`)

	base := map[int]int{1: 100, 2: 200, 3: 300, 4: 400}
	for ep := 1; ep <= 4; ep++ {
		mustExec(`INSERT INTO episodes (id, show_id, episode_no, created_at, updated_at)
		          VALUES (?, 1, ?, 0, 0)`, ep, ep)
		mustExec(`INSERT INTO sources (episode_id, chat_id, doc_id, message_id, created_at)
		          VALUES (?, 1, ?, ?, 0)`, ep, 9000+ep, base[ep])
	}
	addPhoto := func(docID int64, msgID int) {
		mustExec(`INSERT INTO show_photos (show_id, chat_id, message_id, doc_id, width, height, created_at)
		          VALUES (1, 1, ?, ?, 720, 1280, 0)`, msgID, docID)
	}
	addPhoto(1, 101)
	addPhoto(2, 199)
	for i := 0; i < 3; i++ {
		addPhoto(int64(10+i), 298+i)
	}
	for i := 0; i < 8; i++ {
		addPhoto(int64(20+i), 401+i)
	}
	addPhoto(99, 5000)

	photos, err := d.ListShowPhotos(1)
	if err != nil {
		t.Fatalf("list photos: %v", err)
	}
	if len(photos) != 10 {
		t.Fatalf("应返回 ep2 的 1 张 + ep3 的 3 张 + ep4 的 6 张 = 10 张，得到 %d 张", len(photos))
	}
	perEp := map[int]int{}
	for _, p := range photos {
		if p.ID == 0 {
			t.Fatalf("返回了空记录")
		}
		if p.EpisodeNo < 2 {
			t.Fatalf("旧集（ep%d）的图不应该出现", p.EpisodeNo)
		}
		perEp[p.EpisodeNo]++
	}
	if perEp[2] != 1 || perEp[3] != 3 || perEp[4] != 6 {
		t.Fatalf("按集分布不对：%v", perEp)
	}

	locs, err := d.ListShowPhotoLocs(1)
	if err != nil {
		t.Fatalf("list locs: %v", err)
	}
	if len(locs) != len(photos) {
		t.Fatalf("预热定位数 %d 应与展示数 %d 一致", len(locs), len(photos))
	}
	for _, l := range locs {
		if l.MessageID == 5000 {
			t.Fatalf("广告图不应该进入预热队列")
		}
	}
}

func TestListShowPhotosKeepsUnattributed(t *testing.T) {
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
	mustExec(`INSERT INTO shows (id, name, norm_name, created_at, updated_at) VALUES (2, '无来源剧', '无来源剧', 0, 0)`)
	for i := 0; i < 5; i++ {
		mustExec(`INSERT INTO show_photos (show_id, chat_id, message_id, doc_id, width, height, created_at)
		          VALUES (2, 1, ?, ?, 720, 1280, 0)`, 10+i, 500+i)
	}
	photos, err := d.ListShowPhotos(2)
	if err != nil {
		t.Fatalf("list photos: %v", err)
	}
	if len(photos) != 5 {
		t.Fatalf("判不出归属时应原样返回 5 张，得到 %d 张", len(photos))
	}
	for _, p := range photos {
		if p.EpisodeNo != 0 {
			t.Fatalf("判不出归属的图 episode_no 应为 0，得到 %d", p.EpisodeNo)
		}
	}
}
