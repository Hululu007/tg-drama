package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"tgdrama/internal/config"
	"tgdrama/internal/storage"
	"tgdrama/internal/tgclient"
)

func newTestHandler(t *testing.T) http.Handler {
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
	return NewServer(store, svc).Handler()
}

func TestStaticAndAPI(t *testing.T) {
	h := newTestHandler(t)

	cases := []struct {
		path       string
		wantStatus int
		contains   string
	}{
		{"/", http.StatusOK, "短剧库"},
		{"/", http.StatusOK, "登录 Telegram"},
		{"/video.min.js", http.StatusOK, "Video.js"},
		{"/video-js.min.css", http.StatusOK, "vjs-"},
		{"/login", http.StatusFound, ""},
		{"/classic", http.StatusFound, ""},
		{"/classic.js", http.StatusNotFound, ""},
		{"/classic.css", http.StatusNotFound, ""},
		{"/api/account/state", http.StatusOK, `"authorized"`},
		{"/api/shows", http.StatusOK, `"list"`},
		{"/api/tags", http.StatusOK, "[]"},
		{"/api/search?q=test", http.StatusOK, `"list"`},
		{"/api/history", http.StatusOK, `"list"`},
		{"/api/favorites", http.StatusOK, `"total"`},
		{"/api/stream/999", http.StatusNotFound, "视频不存在"},
		// 封面缺失不再 404：发一张占位 SVG（已经落盘缓存），列表里每张卡都有图
		{"/api/covers/999", http.StatusOK, "<svg"},
	}
	for _, c := range cases {
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)

		if rec.Code != c.wantStatus {
			t.Errorf("%s 状态码 = %d, 期望 %d", c.path, rec.Code, c.wantStatus)
		}
		if c.contains != "" && !strings.Contains(rec.Body.String(), c.contains) {
			t.Errorf("%s 响应未包含 %q", c.path, c.contains)
		}
	}
}
