package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/tgerr"

	"tgdrama/internal/storage"
)

func pinSourceForTest(t *testing.T, store *storage.DB, ep int64, docID int64, size int64) *storage.Source {
	t.Helper()
	lines, err := store.ListEpisodeLines(ep)
	if err != nil || len(lines) == 0 || len(lines[0].Segments) == 0 {
		t.Fatalf("缺少来源: %v", err)
	}
	src := lines[0].Segments[0]
	if docID != 0 {
		if err := store.UpdateSourceLocation(src.ID, docID, src.AccessHash, src.FileReference); err != nil {
			t.Fatal(err)
		}
		src.DocID = docID
	}
	if size > 0 {
		src.FileSize = size
		if err := store.SetSourceTestSizeForTest(src.ID, size); err != nil {
			t.Fatal(err)
		}
	}
	return &src
}

func TestStreamPinHeadBlocks(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	src := pinSourceForTest(t, store, ep1, 0, 8<<20)

	var calls int32
	var offsets []int64
	srv.applyStreamOptions(256*1024, 1<<20)
	srv.fetchChunk = func(ctx context.Context, s *storage.Source, off int64, limit int) ([]byte, *storage.Source, error) {
		calls++
		offsets = append(offsets, off)
		data := make([]byte, limit)
		return data, s, nil
	}
	h := srv.Handler()

	pinnedOnce := func() {
		t.Helper()
		for _, want := range []int64{0, 131072} {
			n := 0
			for _, off := range offsets {
				if off == want {
					n++
				}
			}
			if n != 1 {
				t.Fatalf("pinned 块 offset=%d 被取了 %d 次（应 1 次）：offsets=%v", want, n, offsets)
			}
		}
	}
	do := func(rng string) {
		req := httptest.NewRequest(http.MethodGet, "/api/stream/"+itoa(src.ID), nil)
		req.Header.Set("Range", rng)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusPartialContent && rec.Code != http.StatusOK {
			t.Fatalf("range %s: %d %s", rng, rec.Code, rec.Body.String())
		}
	}
	do("bytes=0-393215")
	do("bytes=0-393215")
	do("bytes=7340032-8388607")
	pinnedOnce()
	do("bytes=0-393215")
	pinnedOnce()
}

func TestStreamPrefetchNext(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	src := pinSourceForTest(t, store, ep1, 0, 3<<20)

	var calls int32
	var lastOffset int32
	srv.applyStreamOptions(256*1024, 2<<20)

	var started int32
	srv.fetchChunk = func(ctx context.Context, s *storage.Source, off int64, limit int) ([]byte, *storage.Source, error) {
		atomic.AddInt32(&started, 1)
		atomic.StoreInt32(&lastOffset, int32(off))
		time.Sleep(30 * time.Millisecond)
		atomic.AddInt32(&calls, 1)
		data := make([]byte, limit)
		return data, s, nil
	}

	start := time.Now()
	req := httptest.NewRequest(http.MethodGet, "/api/stream/"+itoa(src.ID), nil)
	req.Header.Set("Range", "bytes=0-786431")
	rec := httptest.NewRecorder()
	srv.streamTelegram(rec, req, src)
	elapsed := time.Since(start)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("应 206，得到 %d", rec.Code)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("预读不应拖慢主路径：3 块耗时 %v", elapsed)
	}

	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("应恰好 2 次读取（预读不越界），实际 %d", n)
	}
}

func TestStreamFloodWaitRetryAfter(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	lines, _ := store.ListEpisodeLines(ep1)
	src := &lines[0].Segments[0]

	srv.applyStreamOptions(256*1024, 64)

	srv.fetchChunk = func(ctx context.Context, s *storage.Source, off int64, limit int) ([]byte, *storage.Source, error) {
		return nil, s, &floodWaitErr{wait: 120, err: tgerr.New(420, "FLOOD_WAIT_120")}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/stream/"+itoa(src.ID), nil)
	rec := httptest.NewRecorder()
	srv.streamTelegram(rec, req, src)
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("长 FLOOD_WAIT 应 503，得到 %d %s", rec.Code, rec.Body.String())
	}
	if ra := rec.Header().Get("Retry-After"); ra == "" {
		t.Fatal("Retry-After header missing")
	}
	if !strings.Contains(rec.Body.String(), "限流") {
		t.Fatalf("错误信息应说明限流: %s", rec.Body.String())
	}
}

func TestPlaybackDefersMediaJobs(t *testing.T) {
	srv, _ := newTestServerRaw(t)
	svc := srv.svc
	if svc == nil {
		t.Skip("svc 未初始化")
	}
	svc.SetPlaybackActive(true)
	if !svc.PlaybackDeferred() {
		t.Fatal("播放中应退让")
	}
	svc.SetPlaybackActive(true)
	svc.SetPlaybackActive(false)
	if !svc.PlaybackDeferred() {
		t.Fatal("两路播放只关掉一路仍应退让")
	}
	svc.SetPlaybackActive(false)
	if svc.PlaybackDeferred() {
		t.Fatal("全部结束且无限流时应恢复")
	}
}

func TestPrefetchDoesNotDeadlock(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	src := pinSourceForTest(t, store, ep1, 0, 2<<20)

	var inflight atomic.Int32
	srv.applyStreamOptions(256*1024, 2<<20)
	srv.fetchChunk = func(ctx context.Context, s *storage.Source, off int64, limit int) ([]byte, *storage.Source, error) {
		inflight.Add(1)
		defer inflight.Add(-1)
		select {
		case <-ctx.Done():
			return nil, s, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
		data := make([]byte, limit)
		return data, s, nil
	}
	st := &streamState{src: *src}

	_, err0 := srv.fetchWholeBlock(context.Background(), st, 0)
	if err0 != nil {
		t.Fatalf("块0: %v", err0)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv.prefetchNext(ctx, st, 0, src.FileSize, 0, src.FileSize-1)
	cancel()

	done := make(chan error, 1)
	go func() {
		_, err := srv.fetchWholeBlock(context.Background(), st, 1)
		done <- err
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("预读与主循环交错导致死锁")
	}
	if inflight.Load() > 1 {
		t.Fatalf("并发读取超过 1：%d", inflight.Load())
	}
}
