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

// 本地已经有 photo-<docID>.jpg、但库里 cache_path 是空的（下载完成后进程被杀、
// 或旧版本留下的记录）时，预热必须直接补上缓存标记，而不是再发一次
// channels.getMessages + 重新下载一遍。否则详情页每次打开都会重复请求同一张图。
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

	// 文件已在磁盘上，库里却没记路径
	path := filepath.Join(cfg.PhotoDir(), fmt.Sprintf("photo-%d.jpg", docID))
	if err := os.WriteFile(path, []byte("fake-jpeg"), 0o644); err != nil {
		t.Fatalf("write photo: %v", err)
	}

	// 传 nil api：如果没有走「本地已存在」分支，就会因为缺 API 而白跑，
	// 这里只验证不会发请求也不报错，且缓存标记被补上。
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
