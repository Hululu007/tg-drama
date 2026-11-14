package storage

import (
	"path/filepath"
	"testing"
)

func TestRecomputeHiddenShowsIdempotent(t *testing.T) {
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
	mustExec(`INSERT INTO chats (id, tg_chat_id, name, enabled, created_at) VALUES (1, 101, '短剧频道', 1, 0)`)

	for _, row := range []struct {
		id   int64
		name string
		capt string
	}{
		{1, "正常短剧", "《正常短剧》第1集 #正常短剧"},
		{2, "推特福利", "推特极品 反差婊 第1集"},
		{3, "露脸", "露脸 第1集"},
	} {
		mustExec(`INSERT INTO shows (id, name, norm_name, created_at, updated_at) VALUES (?, ?, ?, 0, 0)`,
			row.id, row.name, row.name)
		mustExec(`INSERT INTO episodes (id, show_id, episode_no, created_at, updated_at) VALUES (?, ?, 1, 0, 0)`,
			row.id, row.id)
		mustExec(`INSERT INTO sources (episode_id, chat_id, message_id, caption, created_at) VALUES (?, 1, ?, ?, 0)`,
			row.id, row.id, row.capt)
	}

	first, err := d.RecomputeHiddenShows()
	if err != nil {
		t.Fatalf("first recompute: %v", err)
	}

	if first != 2 {
		t.Fatalf("首次应隐藏 2 个（社交搬运 + 泛分类词），实际 %d", first)
	}
	hiddenAfterFirst := d.CountHiddenShows()

	second, err := d.RecomputeHiddenShows()
	if err != nil {
		t.Fatalf("second recompute: %v", err)
	}
	if second != first {
		t.Fatalf("第二次重算隐藏数 = %d，期望与第一次一致（%d）", second, first)
	}
	if got := d.CountHiddenShows(); got != hiddenAfterFirst {
		t.Fatalf("重算后隐藏总数 = %d，期望 %d（不应来回跳）", got, hiddenAfterFirst)
	}

	var hidden int
	if err := d.db.QueryRow(`SELECT hidden FROM shows WHERE id = 1`).Scan(&hidden); err != nil {
		t.Fatalf("query: %v", err)
	}
	if hidden != 0 {
		t.Fatal("正常短剧被误标记为隐藏")
	}
}
