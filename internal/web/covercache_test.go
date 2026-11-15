package web

import (
	"bytes"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 没有封面的短剧也要有图：接口返回占位 SVG，且只在内存里缓存，
// 不再往磁盘写几千个 .svg 小文件。第二次请求直接命中内存，内容一致。
func TestCoverPlaceholderInMemory(t *testing.T) {
	srv, store := newTestServerRaw(t)
	cacheDir := t.TempDir()
	srv.SetCacheDir(cacheDir)
	h := srv.Handler()

	showID, _, _ := seedShow(t, store) // 剧名「测试短剧」，没有封面

	req := httptest.NewRequest(http.MethodGet, "/api/covers/"+itoa(showID), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("无封面时状态码 = %d，期望 200（不再 404）", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "image/svg+xml") {
		t.Fatalf("Content-Type = %q，期望 image/svg+xml", ct)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "<svg") || !strings.Contains(body, "测") {
		t.Fatalf("占位图应包含剧名首字: %s", body)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "covers")); err == nil {
		t.Fatal("占位图不该再写入磁盘缓存")
	}

	// 占位图必须是可重新校验的：真封面下载完之后浏览器不会继续用旧的占位图。
	// 早先是 max-age=86400，新封面要等一天才出现。
	if cc := rec.Header().Get("Cache-Control"); strings.Contains(cc, "max-age=8") {
		t.Fatalf("占位图 Cache-Control = %q，不能长缓存（真图到了换不回来）", cc)
	}

	// 第二次请求走内存缓存，内容与 ETag 都一致；带 If-None-Match 时返回 304
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/covers/"+itoa(showID), nil))
	if rec2.Code != http.StatusOK || rec2.Body.String() != body {
		t.Fatalf("缓存后的响应不一致: %d", rec2.Code)
	}
	req3 := httptest.NewRequest(http.MethodGet, "/api/covers/"+itoa(showID), nil)
	req3.Header.Set("If-None-Match", rec2.Header().Get("ETag"))
	rec3 := httptest.NewRecorder()
	h.ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNotModified {
		t.Fatalf("带 If-None-Match 的状态码 = %d，期望 304", rec3.Code)
	}
}

// 有真实封面文件时优先发文件，不要用占位图盖掉
func TestCoverPrefersRealFile(t *testing.T) {
	srv, store := newTestServerRaw(t)
	cacheDir := t.TempDir()
	srv.SetCacheDir(cacheDir)
	h := srv.Handler()

	showID, _, _ := seedShow(t, store)
	coverPath := filepath.Join(t.TempDir(), "show.jpg")
	const jpeg = "\xff\xd8\xff\xe0JFIF-fake-cover"
	if err := os.WriteFile(coverPath, []byte(jpeg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.SetShowCover(showID, coverPath); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/covers/"+itoa(showID), nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if rec.Body.String() != jpeg {
		t.Fatal("应发送真实封面文件内容")
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "covers", itoa(showID)+".svg")); err == nil {
		t.Fatal("有真实封面时不该生成占位图")
	}
}

// 封面文件被删掉（换库/清缓存）时回落到占位图，而不是 404
func TestCoverFallsBackWhenFileMissing(t *testing.T) {
	srv, store := newTestServerRaw(t)
	srv.SetCacheDir(t.TempDir())
	h := srv.Handler()

	showID, _, _ := seedShow(t, store)
	if err := store.SetShowCover(showID, filepath.Join(t.TempDir(), "gone.jpg")); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/covers/"+itoa(showID), nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "<svg") {
		t.Fatalf("封面文件丢失时应回落到占位图: %d", rec.Code)
	}
}

// 支持 WebP 的客户端拿到转码后的图，并落到 webp 缓存目录；ETag 命中返回 304
func TestCoverServedAsWebP(t *testing.T) {
	srv, store := newTestServerRaw(t)
	cacheDir := t.TempDir()
	srv.SetCacheDir(cacheDir)
	h := srv.Handler()

	showID, _, _ := seedShow(t, store)
	src := filepath.Join(t.TempDir(), "show.jpg")
	var buf bytes.Buffer
	img := image.NewRGBA(image.Rect(0, 0, 640, 360))
	for y := 0; y < 360; y++ {
		for x := 0; x < 640; x++ {
			img.Set(x, y, color.RGBA{uint8(x % 256), uint8(y % 256), 120, 255})
		}
	}
	if err := jpeg.Encode(&buf, img, &jpeg.Options{Quality: 90}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, buf.Bytes(), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.SetShowCover(showID, src); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/covers/"+itoa(showID), nil)
	req.Header.Set("Accept", "image/webp,image/*,*/*;q=0.8")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("状态码 = %d", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "image/webp" {
		t.Fatalf("Content-Type = %q，期望 image/webp", ct)
	}
	// WebP 容器以 "RIFF....WEBP" 开头
	body := rec.Body.Bytes()
	if len(body) < 12 || !bytes.HasPrefix(body, []byte("RIFF")) || !bytes.Contains(body[:12], []byte("WEBP")) {
		t.Fatalf("响应不是 WebP 数据: %q", body[:min(12, len(body))])
	}
	if len(body) >= len(buf.Bytes()) {
		t.Fatalf("转码后体积 %d 应小于原图 %d", len(body), buf.Len())
	}
	cached := filepath.Join(cacheDir, "webp", "covers", itoa(showID)+".webp")
	if _, err := os.Stat(cached); err != nil {
		t.Fatalf("WebP 没写到 %s: %v", cached, err)
	}

	// 再请求一次：带 If-None-Match 应拿到 304
	req2 := httptest.NewRequest(http.MethodGet, "/api/covers/"+itoa(showID), nil)
	req2.Header.Set("Accept", "image/webp")
	req2.Header.Set("If-None-Match", rec.Header().Get("ETag"))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusNotModified {
		t.Fatalf("带 If-None-Match 的状态码 = %d，期望 304", rec2.Code)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
