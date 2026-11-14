package storage

import (
	"path/filepath"
	"testing"
)

func TestListShowsSortByEpisodes(t *testing.T) {
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

	mustExec(`INSERT INTO shows (id, name, norm_name, cover_path, created_at, updated_at) VALUES (1, '一集剧', '一集剧', 'data/covers/1.jpg', 0, 300)`)
	mustExec(`INSERT INTO shows (id, name, norm_name, created_at, updated_at) VALUES (2, '三集剧', '三集剧', 0, 100)`)
	mustExec(`INSERT INTO shows (id, name, norm_name, created_at, updated_at) VALUES (3, '两集剧', '两集剧', 0, 200)`)
	eps := map[int64]int{1: 1, 2: 3, 3: 2}
	src := 1000
	for showID, n := range eps {
		for ep := 1; ep <= n; ep++ {
			res, err := d.db.Exec(`INSERT INTO episodes (show_id, episode_no, created_at, updated_at) VALUES (?, ?, 0, 0)`, showID, ep)
			if err != nil {
				t.Fatalf("episode: %v", err)
			}
			epID, _ := res.LastInsertId()
			src++
			mustExec(`INSERT INTO sources (episode_id, chat_id, doc_id, message_id, created_at) VALUES (?, 1, ?, ?, 0)`, epID, src, src)
		}
	}

	natural, err := d.ListShows("", "", 10, 0)
	if err != nil {
		t.Fatalf("natural: %v", err)
	}
	if len(natural) != 3 || natural[0].ID != 1 {
		t.Fatalf("自然排序应最近更新优先，得到 %d 部、首条 %d", len(natural), natural[0].ID)
	}

	sorted, err := d.ListShows("", "episodes", 10, 0)
	if err != nil {
		t.Fatalf("episodes: %v", err)
	}
	if len(sorted) != 3 {
		t.Fatalf("集数排序返回 %d 部", len(sorted))
	}
	for i, want := range []struct {
		id  int64
		eps int
	}{{2, 3}, {3, 2}, {1, 1}} {
		if sorted[i].ID != want.id || sorted[i].EpCount != want.eps {
			t.Fatalf("第 %d 部 = id %d/%d 集，期望 id %d/%d 集", i, sorted[i].ID, sorted[i].EpCount, want.id, want.eps)
		}
	}
}

func TestListFavoriteShowsSortByEpisodes(t *testing.T) {
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
	mustExec(`INSERT INTO shows (id, name, norm_name, created_at, updated_at) VALUES (1, '一集剧', '一集剧', 0, 0)`)
	mustExec(`INSERT INTO shows (id, name, norm_name, created_at, updated_at) VALUES (2, '三集剧', '三集剧', 0, 0)`)
	eps := map[int64]int{1: 1, 2: 3}
	src := 2000
	for showID, n := range eps {
		for ep := 1; ep <= n; ep++ {
			res, err := d.db.Exec(`INSERT INTO episodes (show_id, episode_no, created_at, updated_at) VALUES (?, ?, 0, 0)`, showID, ep)
			if err != nil {
				t.Fatalf("episode: %v", err)
			}
			epID, _ := res.LastInsertId()
			src++
			mustExec(`INSERT INTO sources (episode_id, chat_id, doc_id, message_id, created_at) VALUES (?, 1, ?, ?, 0)`, epID, src, src)
		}
	}

	mustExec(`INSERT INTO favorites (show_id, created_at) VALUES (2, 100)`)
	mustExec(`INSERT INTO favorites (show_id, created_at) VALUES (1, 200)`)

	natural, err := d.ListFavoriteShows("", 10, 0)
	if err != nil {
		t.Fatalf("natural: %v", err)
	}
	if len(natural) != 2 || natural[0].ID != 1 {
		t.Fatalf("自然排序应最近收藏优先，得到 %v", natural)
	}
	sorted, err := d.ListFavoriteShows("episodes", 10, 0)
	if err != nil {
		t.Fatalf("episodes: %v", err)
	}
	if len(sorted) != 2 || sorted[0].ID != 2 || sorted[0].EpCount != 3 || sorted[1].ID != 1 {
		t.Fatalf("集数排序错误：%d 部，首条 id=%d", len(sorted), sorted[0].ID)
	}
}
