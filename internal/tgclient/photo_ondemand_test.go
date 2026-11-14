package tgclient

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"tgdrama/internal/config"
	"tgdrama/internal/storage"
)

func TestWarmPhotosAdoptsExistingFile(t *testing.T) {
	dir := t.TempDir()
	db, err := storage.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer db.Close()

	cfg := config.Default()
	cfg.Data.Dir = dir
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	svc, err := New(cfg, db)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}

	chatPK, err := db.UpsertChat(&storage.Chat{
		TgChatID: 900, AccessHash: 1, Name: "测试短剧频道", Type: "channel", Enabled: true,
	})
	if err != nil {
		t.Fatalf("upsert chat: %v", err)
	}
	showID, _, err := db.UpsertShow("测试剧", "测试剧")
	if err != nil {
		t.Fatalf("upsert show: %v", err)
	}
	const docID = int64(4242)
	if _, _, err := db.UpsertShowPhoto(&storage.ShowPhoto{ShowID: showID, Width: 720, Height: 1280},
		docID, 777, []byte("ref"), chatPK, 501, 0, ""); err != nil {
		t.Fatalf("upsert photo: %v", err)
	}

	path := filepath.Join(cfg.PhotoDir(), fmt.Sprintf("photo-%d.jpg", docID))
	if err := os.WriteFile(path, []byte("fake-jpeg"), 0o644); err != nil {
		t.Fatalf("write photo: %v", err)
	}

	if err := svc.warmShowPhotos(context.Background(), nil, showID); err != nil {
		t.Fatalf("warm show photos: %v", err)
	}

	photos, err := db.ListShowPhotos(showID)
	if err != nil || len(photos) != 1 {
		t.Fatalf("photos = %d, err = %v", len(photos), err)
	}
	got, err := db.GetShowPhotoPath(photos[0].ID)
	if err != nil {
		t.Fatalf("get path: %v", err)
	}
	if got != path {
		t.Fatalf("cache_path = %q，期望 %q", got, path)
	}
}
