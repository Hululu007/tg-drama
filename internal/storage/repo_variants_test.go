package storage

import (
	"path/filepath"
	"testing"
)

func TestSourceVariantsUpsertAndPreserveIDs(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := d.db.Exec(q, args...); err != nil {
			t.Fatalf("exec: %v\n%s", err, q)
		}
	}
	mustExec(`INSERT INTO chats (id, tg_chat_id, name, enabled, created_at) VALUES (1, 101, '频道', 1, 0)`)
	mustExec(`INSERT INTO shows (id, name, norm_name, created_at, updated_at) VALUES (1, '测试剧', '测试剧', 0, 0)`)
	mustExec(`INSERT INTO episodes (id, show_id, episode_no, created_at, updated_at) VALUES (1, 1, 1, 0, 0)`)
	mustExec(`INSERT INTO sources (id, episode_id, chat_id, message_id, doc_id, file_size, duration, width, height, created_at)
	         VALUES (1, 1, 1, 100, 900, 1000, 60, 1080, 1920, 0)`)

	vars := []SourceVariant{
		{DocID: 901, AccessHash: 11, FileReference: []byte("a"), FileSize: 500, Duration: 60, Width: 720, Height: 1280, Mime: "video/mp4"},
		{DocID: 902, AccessHash: 12, FileReference: []byte("b"), FileSize: 300, Duration: 60, Width: 480, Height: 854, Mime: "video/mp4"},
	}
	if err := d.ReplaceSourceVariantsByID(1, vars); err != nil {
		t.Fatal(err)
	}
	list, err := d.ListSourceVariants(1)
	if err != nil || len(list) != 2 {
		t.Fatalf("list = %+v, %v", list, err)
	}

	if list[0].DocID != 901 || list[1].DocID != 902 {
		t.Fatalf("order = %d,%d", list[0].DocID, list[1].DocID)
	}
	id901 := list[0].ID

	vars2 := []SourceVariant{
		{DocID: 901, AccessHash: 11, FileReference: []byte("a2"), FileSize: 500, Duration: 60, Width: 720, Height: 1280, Mime: "video/mp4"},
		{DocID: 903, AccessHash: 13, FileReference: []byte("c"), FileSize: 200, Duration: 60, Width: 360, Height: 640, Mime: "video/mp4"},
	}
	if err := d.ReplaceSourceVariantsByID(1, vars2); err != nil {
		t.Fatal(err)
	}
	list, err = d.ListSourceVariants(1)
	if err != nil || len(list) != 2 {
		t.Fatalf("after refresh = %+v, %v", list, err)
	}
	if list[0].ID != id901 || string(list[0].FileReference) != "a2" {
		t.Fatalf("variant id changed or ref not refreshed: %+v", list[0])
	}
	if list[1].DocID != 903 {
		t.Fatalf("new variant missing: %+v", list[1])
	}

	if err := d.ReplaceSourceVariantsByID(1, nil); err != nil {
		t.Fatal(err)
	}
	if list, _ := d.ListSourceVariants(1); len(list) != 0 {
		t.Fatalf("expected empty, got %+v", list)
	}

	if err := d.MarkVariantsChecked(1); err != nil {
		t.Fatal(err)
	}
	if ok, err := d.SourceVariantsChecked(1); err != nil || !ok {
		t.Fatalf("checked = %v, %v", ok, err)
	}
	if _, err := d.GetSourceVariant(99999); err == nil {
		t.Fatal("missing variant should error")
	}
}
