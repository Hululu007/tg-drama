package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"tgdrama/internal/config"
	"tgdrama/internal/storage"
	"tgdrama/internal/tgclient"
)

func newTestEnv(t testing.TB) (*Server, *storage.DB, *config.Config) {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Default()
	cfg.Data.Dir = dir
	cfg.Server.Listen = ":0"
	if err := cfg.EnsureDirs(); err != nil {
		t.Fatal(err)
	}
	store, err := storage.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	svc, err := tgclient.New(cfg, store)
	if err != nil {
		t.Fatal(err)
	}
	srv := NewServer(store, svc)
	srv.SetCacheDir(cfg.CacheDir())
	return srv, store, cfg
}

func TestClearPhotosCache(t *testing.T) {
	srv, store, cfg := newTestEnv(t)
	h := srv.Handler()

	showID, _, _ := seedShow(t, store)
	photoPath := filepath.Join(cfg.PhotoDir(), "photo-123.jpg")
	if err := os.WriteFile(photoPath, []byte("fake-jpeg-bytes"), 0o644); err != nil {
		t.Fatal(err)
	}
	const docID = int64(123)
	if _, _, err := store.UpsertShowPhoto(&storage.ShowPhoto{ShowID: showID, Width: 720, Height: 1280},
		docID, 999, []byte("ref"), 1, 1000, 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPhotoCache(docID, photoPath); err != nil {
		t.Fatal(err)
	}
	photos, err := store.ListShowPhotos(showID)
	if err != nil || len(photos) != 1 {
		t.Fatalf("预览图记录 = %d, err=%v", len(photos), err)
	}
	photoID := photos[0].ID

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/cache/usage", nil))
	var usage map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &usage); err != nil {
		t.Fatal(err)
	}
	if got := int64(usage["photos_bytes"].(float64)); got <= 0 {
		t.Fatalf("photos_bytes = %d，期望 > 0", got)
	}
	if n := int(usage["photos_files"].(float64)); n != 1 {
		t.Fatalf("photos_files = %d，期望 1", n)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/api/cache/clear?kind=photos", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("清理接口状态码 = %d", rec.Code)
	}
	var res map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	if n := int(res["freed_files"].(float64)); n != 1 {
		t.Fatalf("freed_files = %d，期望 1", n)
	}
	if _, err := os.Stat(photoPath); err == nil {
		t.Fatal("预览图文件应被删除")
	}
	path, err := store.GetShowPhotoPath(photoID)
	if err != nil {
		t.Fatal(err)
	}
	if path != "" {
		t.Fatalf("cache_path 应被清空，实际 = %q", path)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/photos/"+itoa(photoID), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("清缓存后访问预览图状态码 = %d，期望 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct == "" {
		t.Fatal("清缓存后应返回占位图")
	}

	photos, err = store.ListShowPhotos(showID)
	if err != nil || len(photos) != 1 {
		t.Fatalf("清缓存后记录应保留，实际 %d 条, err=%v", len(photos), err)
	}
}
