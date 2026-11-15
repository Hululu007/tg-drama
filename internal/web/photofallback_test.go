package web

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"tgdrama/internal/storage"
)

// 预览图还没下载时用该剧封面占位：返回的是封面图本身（不是剧名首字的 SVG），
// 并且必须带 no-cache —— 否则浏览器会把封面当成这张预览图长期缓存，
// 真图落盘后也换不回来。
func TestPhotoUsesCoverAsPlaceholder(t *testing.T) {
	srv, store := newTestServerRaw(t)
	h := srv.Handler()

	showID, _, _ := seedShow(t, store)
	coverPath := filepath.Join(t.TempDir(), "cover.jpg")
	const coverBytes = "\xff\xd8\xff\xe0JFIF-show-cover"
	if err := os.WriteFile(coverPath, []byte(coverBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.SetShowCover(showID, coverPath); err != nil {
		t.Fatal(err)
	}
	photoID, _, err := store.UpsertShowPhoto(&storage.ShowPhoto{ShowID: showID, Width: 720, Height: 1280},
		555, 777, []byte("ref"), 1, 1000, 0, "")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/photos/"+itoa(photoID), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if got := rec.Body.String(); got != coverBytes {
		t.Fatalf("应发封面内容占位，实际 %q", got)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-cache" {
		t.Fatalf("Cache-Control = %q，期望 no-cache（否则封面会被长期缓存）", cc)
	}
	// 占位必须标记成「还不是真图」：前端靠这个头在超时后把预览图撤掉
	if got := rec.Header().Get("X-Photo-Ready"); got != "0" {
		t.Fatalf("占位响应 X-Photo-Ready = %q，期望 0", got)
	}

	// 真图落盘后，同一个 URL 必须立刻换成真图
	photoPath := filepath.Join(t.TempDir(), "photo-555.jpg")
	const photoBytes = "\xff\xd8\xff\xe0JFIF-real-photo"
	if err := os.WriteFile(photoPath, []byte(photoBytes), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.SetPhotoCache(555, photoPath); err != nil {
		t.Fatal(err)
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/photos/"+itoa(photoID), nil))
	if got := rec2.Body.String(); got != photoBytes {
		t.Fatalf("真图下好后应发真图，实际 %q", got)
	}
	if got := rec2.Header().Get("X-Photo-Ready"); got != "1" {
		t.Fatalf("真图响应 X-Photo-Ready = %q，期望 1", got)
	}
}

// 封面也没有时，退回带剧名首字的 SVG，不能返回空/404
func TestPhotoFallsBackToSVGWhenNoCover(t *testing.T) {
	srv, store := newTestServerRaw(t)
	h := srv.Handler()

	showID, _, _ := seedShow(t, store) // 无封面
	photoID, _, err := store.UpsertShowPhoto(&storage.ShowPhoto{ShowID: showID, Width: 720, Height: 1280},
		556, 778, []byte("ref"), 1, 1001, 0, "")
	if err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/photos/"+itoa(photoID), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "image/svg+xml") {
		t.Fatalf("Content-Type = %q，期望 SVG 占位", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "测") {
		t.Fatalf("占位图应带剧名首字: %s", body)
	}
	if got := rec.Header().Get("X-Photo-Ready"); got != "0" {
		t.Fatalf("SVG 占位 X-Photo-Ready = %q，期望 0", got)
	}
}
