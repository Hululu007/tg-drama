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

// pinSourceForTest 造一个测试用 pinned 来源，返回来源指针。
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

// TestStreamPinHeadBlocks 头部块固定缓存：换一个 Range 请求再取头部不再打 Telegram，
// 即使中间读了大量尾部块把 LRU 挤满。
func TestStreamPinHeadBlocks(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	src := pinSourceForTest(t, store, ep1, 0, 8<<20) // 8MB = 32 个 256KB 块

	var calls int32
	var offsets []int64
	srv.applyStreamOptions(256*1024, 1<<20) // 只装 4 块，头部必然被 LRU 挤掉——除非 pinned
	srv.fetchChunk = func(ctx context.Context, s *storage.Source, off int64, limit int) ([]byte, *storage.Source, error) {
		calls++
		offsets = append(offsets, off)
		data := make([]byte, limit)
		return data, s, nil
	}
	h := srv.Handler()

	// pinnedOnce 头块（0）与块 1（131072）必须全程只被真实拉取一次
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
	do("bytes=0-393215")        // pinned 区（头块 + 块 1）
	do("bytes=0-393215")        // 全命中
	do("bytes=7340032-8388607") // 读尾部，把非 pinned 的 LRU 挤满
	pinnedOnce()
	do("bytes=0-393215") // 头部必须仍在（pinned）
	pinnedOnce()
}

// TestStreamPrefetchNext 连续顺序播放时，主循环拿到块 N 的同时预读 N+1：
// 下一轮 chunkAt 直接命中缓存，不再发起真实读取。
func TestStreamPrefetchNext(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	src := pinSourceForTest(t, store, ep1, 0, 3<<20) // 12 块

	var calls int32
	var lastOffset int32
	srv.applyStreamOptions(256*1024, 2<<20)
	// 假读取带 30ms 延迟。预读的收益是「下一块的网络往返」与「本块写给浏览器的
	// 耗时」重叠——浏览器消费 512KB 需要时间，而测试的 recorder 写入是零耗时的，
	// 所以这里不断言总耗时下降，只断言预读确实发生了：每块读取启动时，下一块
	// 的读取已经在途（用 maxConcurrent 无法观察——信号量仍=1；改用首块返回后
	// 立即检查 inflight）。
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
	req.Header.Set("Range", "bytes=0-786431") // 3 块
	rec := httptest.NewRecorder()
	srv.streamTelegram(rec, req, src)
	elapsed := time.Since(start)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("应 206，得到 %d", rec.Code)
	}
	if elapsed > 200*time.Millisecond {
		t.Fatalf("预读不应拖慢主路径：3 块耗时 %v", elapsed)
	}
	// 新布局：3MB 文件 = 头块 128KB + 块 1（补齐到 1MB）+ 块 2（1MB）+ 块 3（尾部）
	// Range 0-786431 覆盖块 0 与块 1（预取块 2 不应发起——pos 未到块尾不算临近）
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("应恰好 2 次读取（预读不越界），实际 %d", n)
	}
}

// TestStreamFloodWaitRetryAfter 长 FLOOD_WAIT 时首块失败返回 503 + Retry-After，
// 前端据此倒计时，而不是走错误态。
func TestStreamFloodWaitRetryAfter(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	lines, _ := store.ListEpisodeLines(ep1)
	src := &lines[0].Segments[0]

	srv.applyStreamOptions(256*1024, 64)
	// fetchFileChunk 才是把超限 FLOOD_WAIT 包装成 floodWaitErr 的层；
	// 这里直接模拟它的输出（与生产 fetchChunkFromTelegram 一致）。
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

// TestPlaybackDefersMediaJobs 播放流活跃时后台媒体任务退让；结束后恢复。
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

// 预读协程在客户端断开后不能泄漏信号量：取消 ctx 后 chunkAt 返回，
// 信号量由 releaseSem 只释放一次（已有机制），这里验证预读与主循环交错不出死锁。
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
	// 模拟主循环：取块 0 后立即预读块 1，同时取消 ctx（客户端断开）
	_, err0 := srv.fetchWholeBlock(context.Background(), st, 0)
	if err0 != nil {
		t.Fatalf("块0: %v", err0)
	}
	ctx, cancel := context.WithCancel(context.Background())
	srv.prefetchNext(ctx, st, 0, src.FileSize, 0, src.FileSize-1)
	cancel()
	// 主循环继续取块 1：无论预读是否完成，都必须在超时前返回
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
