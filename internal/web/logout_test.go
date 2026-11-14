package web

import (
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"tgdrama/internal/config"
	"tgdrama/internal/storage"
	"tgdrama/internal/tgclient"
)

func TestAuthLogout(t *testing.T) {
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
	h := NewServer(store, svc).Handler()

	sessionPath := cfg.SessionPath()
	if err := os.WriteFile(sessionPath, []byte(`{"key":"test"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if code, out := doJSON(t, h, http.MethodPost, "/api/account/logout", nil); code != http.StatusOK || out["ok"] != true {
		t.Fatalf("退出登录应返回 200 ok: %d %v", code, out)
	}
	if _, err := os.Stat(sessionPath); !os.IsNotExist(err) {
		t.Fatalf("会话文件应被删除，stat err = %v", err)
	}

	code, out := doJSON(t, h, http.MethodGet, "/api/account/state", nil)
	if code != http.StatusOK || out["authorized"] != false {
		t.Fatalf("退出后 authorized 应为 false: %d %v", code, out)
	}
}
