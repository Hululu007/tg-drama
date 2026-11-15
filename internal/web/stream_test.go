package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"

	"tgdrama/internal/storage"
)

// fakeFetcher 生成可预测的字节流，并统计底层读取次数（用于验证缓存与请求合并）
func fakeFetcher(calls *int32, total int64) sourceFetchFunc {
	return func(ctx context.Context, src *storage.Source, offset int64, limit int) ([]byte, *storage.Source, error) {
		if offset >= total {
			return nil, src, nil
		}
		if int64(limit) > total-offset {
			limit = int(total - offset)
		}
		atomic.AddInt32(calls, 1)
		data := make([]byte, limit)
		for i := range data {
			data[i] = byte((offset + int64(i)) % 251)
		}
		return data, src, nil
	}
}

// streamFixture 准备一部剧 + 一路来源，并替换为假读取器
func streamFixture(t *testing.T) (*Server, *storage.Source, *int32) {
	t.Helper()
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	lines, err := store.ListEpisodeLines(ep1)
	if err != nil || len(lines) == 0 || len(lines[0].Segments) == 0 {
		t.Fatalf("测试数据缺少线路: %v", err)
	}
	src := &lines[0].Segments[0]
	var calls int32
	srv.applyStreamOptions(256*1024, 64)
	srv.fetchChunk = fakeFetcher(&calls, src.FileSize)
	return srv, src, &calls
}

func doStream(t *testing.T, srv *Server, method, path, rng string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(method, path, nil)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

// TestStreamFull 无 Range 时返回 200 全量，并带可拖动所需的响应头
func TestStreamFull(t *testing.T) {
	srv, src, _ := streamFixture(t)
	rec := doStream(t, srv, http.MethodGet, "/api/stream/"+itoa(src.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges = %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.FormatInt(src.FileSize, 10) {
		t.Fatalf("Content-Length = %q", got)
	}
	if rec.Header().Get("ETag") == "" {
		t.Fatal("缺少 ETag")
	}
	if int64(rec.Body.Len()) != src.FileSize {
		t.Fatalf("响应体长度 = %d，期望 %d", rec.Body.Len(), src.FileSize)
	}
	if b := rec.Body.Bytes(); b[0] != 0 || b[1] != 1 {
		t.Fatalf("响应体字节不对: %v", b[:4])
	}
}

// TestStreamRange 拖动进度条时的分段请求
func TestStreamRange(t *testing.T) {
	srv, src, _ := streamFixture(t)
	rec := doStream(t, srv, http.MethodGet, "/api/stream/"+itoa(src.ID), "bytes=1000-2000")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("应 206，得到 %d %s", rec.Code, rec.Body.String())
	}
	want := "bytes 1000-2000/" + strconv.FormatInt(src.FileSize, 10)
	if got := rec.Header().Get("Content-Range"); got != want {
		t.Fatalf("Content-Range = %q，期望 %q", got, want)
	}
	if rec.Body.Len() != 1001 {
		t.Fatalf("分段长度 = %d，期望 1001", rec.Body.Len())
	}
	if rec.Body.Bytes()[0] != byte(1000%251) {
		t.Fatalf("分段起始字节不对: %d", rec.Body.Bytes()[0])
	}
}

// TestStreamOpenEnded 播放器常见的 bytes=N- 写法：一直取到结尾
func TestStreamOpenEnded(t *testing.T) {
	srv, src, _ := streamFixture(t)
	rec := doStream(t, srv, http.MethodGet, "/api/stream/"+itoa(src.ID), "bytes=1048570-")
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("应 206，得到 %d", rec.Code)
	}
	if rec.Body.Len() != 6 {
		t.Fatalf("长度 = %d，期望 6", rec.Body.Len())
	}
}

// TestStreamInvalidRange 越界 Range 必须 416，否则播放器会拿到错位数据
func TestStreamInvalidRange(t *testing.T) {
	srv, src, _ := streamFixture(t)
	rec := doStream(t, srv, http.MethodGet, "/api/stream/"+itoa(src.ID), "bytes=99999999-")
	if rec.Code != http.StatusRequestedRangeNotSatisfiable {
		t.Fatalf("应 416，得到 %d", rec.Code)
	}
}

// TestStreamHead HEAD 只回头不回体（宿主播放器探测长度时会用到）
func TestStreamHead(t *testing.T) {
	srv, src, _ := streamFixture(t)
	rec := doStream(t, srv, http.MethodHead, "/api/stream/"+itoa(src.ID), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("应 200，得到 %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("HEAD 不应有响应体，实际 %d", rec.Body.Len())
	}
	if rec.Header().Get("Content-Length") != strconv.FormatInt(src.FileSize, 10) {
		t.Fatal("HEAD 缺少 Content-Length")
	}
}

// TestStreamCacheReuse 重复请求同一段不再打 Telegram，这是拖动不卡的关键
func TestStreamCacheReuse(t *testing.T) {
	srv, src, calls := streamFixture(t)
	doStream(t, srv, http.MethodGet, "/api/stream/"+itoa(src.ID), "bytes=0-1023")
	first := atomic.LoadInt32(calls)
	if first == 0 {
		t.Fatal("第一次请求应该真的去取数据")
	}
	doStream(t, srv, http.MethodGet, "/api/stream/"+itoa(src.ID), "bytes=0-1023")
	if got := atomic.LoadInt32(calls); got != first {
		t.Fatalf("缓存未命中：第一次 %d 次读取，第二次后 %d 次", first, got)
	}
}

// discardWriter 只统计字节数，避免把基准测试耗在内存拷贝上
type discardWriter struct {
	header http.Header
	code   int
	n      int
}

func (d *discardWriter) Header() http.Header         { return d.header }
func (d *discardWriter) WriteHeader(code int)        { d.code = code }
func (d *discardWriter) Write(b []byte) (int, error) { d.n += len(b); return len(b), nil }
func (d *discardWriter) Flush()                      {}

// BenchmarkStreamPipeline 衡量代理自身的开销：假读取器无延迟时能跑多快。
// 真实瓶颈在 Telegram 网络往返，这里只证明分块/缓存/重组不是瓶颈。
func BenchmarkStreamPipeline(b *testing.B) {
	srv, store := newTestServerRaw(b)
	_, ep1, _ := seedShow(b, store)
	lines, err := store.ListEpisodeLines(ep1)
	if err != nil {
		b.Fatal(err)
	}
	src := &lines[0].Segments[0]
	const total = 32 << 20
	srv.applyStreamOptions(256*1024, 128)
	srv.fetchChunk = fakeFetcher(new(int32), total)

	b.SetBytes(total)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := httptest.NewRequest(http.MethodGet, "/api/stream/"+itoa(src.ID), nil)
		req.Header.Set("Range", "bytes=0-"+strconv.FormatInt(total-1, 10))
		w := &discardWriter{header: http.Header{}}
		srv.streamTelegram(w, req, &storage.Source{ID: src.ID, FileSize: total, Mime: "video/mp4"})
		if w.n != total {
			b.Fatalf("写入 %d 字节，期望 %d", w.n, total)
		}
	}
}
