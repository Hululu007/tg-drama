package web

import (
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestBootstrap 首屏聚合接口应一次返回标签、首屏列表、收藏数与继续观看
func TestBootstrap(t *testing.T) {
	h, store := newTestServer(t)
	showID, _, _ := seedShow(t, store)
	if err := store.AddShowTags(showID, []string{"甜宠"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ToggleFavorite(showID); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/bootstrap", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Tags          []map[string]any `json:"tags"`
		Shows         []map[string]any `json:"shows"`
		Total         int64            `json:"total"`
		FavoriteTotal int64            `json:"favorite_total"`
		ContinueList  []map[string]any `json:"continue_list"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tags) != 1 || out.Tags[0]["name"] != "甜宠" {
		t.Fatalf("tags = %v", out.Tags)
	}
	if len(out.Shows) != 1 || out.Total != 1 {
		t.Fatalf("shows = %v total = %d", out.Shows, out.Total)
	}
	if out.FavoriteTotal != 1 {
		t.Fatalf("favorite_total = %d", out.FavoriteTotal)
	}
	if out.ContinueList == nil {
		t.Fatal("continue_list 应为空数组而不是 null")
	}
}

// TestGzipJSON 带 Accept-Encoding: gzip 时 JSON 接口应压缩并可被正常解压
func TestGzipJSON(t *testing.T) {
	h, store := newTestServer(t)
	seedShow(t, store)

	req := httptest.NewRequest(http.MethodGet, "/api/shows", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if enc := rec.Header().Get("Content-Encoding"); enc != "gzip" {
		t.Fatalf("Content-Encoding = %q", enc)
	}
	zr, err := gzip.NewReader(rec.Body)
	if err != nil {
		t.Fatalf("不是合法的 gzip 响应: %v", err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("解压后不是 JSON: %v (%s)", err, body)
	}
	if list, ok := out["list"].([]any); !ok || len(list) != 1 {
		t.Fatalf("list = %v", out["list"])
	}
}

// TestETagNotModified 媒体库未变化时，带 If-None-Match 的请求应返回 304，省掉整包 JSON
func TestETagNotModified(t *testing.T) {
	h, store := newTestServer(t)
	seedShow(t, store)

	first := httptest.NewRecorder()
	h.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/tags", nil))
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("缺少 ETag 响应头")
	}

	req := httptest.NewRequest(http.MethodGet, "/api/tags", nil)
	req.Header.Set("If-None-Match", etag)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotModified {
		t.Fatalf("status = %d, 期望 304", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("304 不应带响应体，实际 %d 字节", rec.Body.Len())
	}

	// 数据变化后 ETag 必须跟着变，否则前端会一直吃旧缓存
	showID, _, _ := seedShow(t, store)
	if _, err := store.ToggleFavorite(showID); err != nil {
		t.Fatal(err)
	}
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/tags", nil))
	if rec2.Header().Get("ETag") == etag {
		t.Fatal("收藏变化后 ETag 未变")
	}
}

// TestGzipSkipsBinary 视频流/封面等二进制接口不能被 gzip 包装（否则 Range 与 Content-Length 失效）
func TestGzipSkipsBinary(t *testing.T) {
	h, store := newTestServer(t)
	seedShow(t, store)
	req := httptest.NewRequest(http.MethodGet, "/api/covers/1", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if enc := rec.Header().Get("Content-Encoding"); enc == "gzip" {
		t.Fatal("封面不应走 gzip")
	}
}
