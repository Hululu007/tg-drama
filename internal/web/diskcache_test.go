package web

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestDiskCacheServeAndRestart(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	lines, _ := store.ListEpisodeLines(ep1)
	src := &lines[0].Segments[0]

	dir := t.TempDir()
	srv.disk = newDiskCache(filepath.Join(dir, "mediacache"), 64<<20)
	var calls int32
	srv.applyStreamOptions(1024*1024, 64)
	srv.fetchChunk = fakeFetcher(&calls, src.FileSize)
	h := srv.Handler()

	do := func() int {
		req := httptest.NewRequest(http.MethodGet, "/api/stream/"+itoa(src.ID), nil)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("应 200，得到 %d %s", rec.Code, rec.Body.String())
		}
		return rec.Body.Len()
	}
	if n := do(); n != int(src.FileSize) {
		t.Fatalf("首次长度 = %d", n)
	}
	first := calls
	if n := do(); n != int(src.FileSize) {
		t.Fatalf("二次长度 = %d", n)
	}
	if calls != first {
		t.Fatalf("落盘后应全部命中磁盘，不再打 Telegram: first=%d now=%d", first, calls)
	}

	srv2 := &Server{store: store, svc: srv.svc}
	srv2.applyStreamOptions(1024*1024, 64)
	srv2.disk = newDiskCache(filepath.Join(dir, "mediacache"), 64<<20)
	var calls2 int32
	srv2.fetchChunk = fakeFetcher(&calls2, src.FileSize)
	req := httptest.NewRequest(http.MethodGet, "/api/stream/"+itoa(src.ID), nil)
	rec := httptest.NewRecorder()
	srv2.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || rec.Body.Len() != int(src.FileSize) {
		t.Fatalf("重启后应 200 且长度正确: %d %d", rec.Code, rec.Body.Len())
	}
	if calls2 != 0 {
		t.Fatalf("重启后应命中磁盘（0 次 Telegram），实际 %d", calls2)
	}
}

func TestDiskCacheEviction(t *testing.T) {
	dir := t.TempDir()
	dc := newDiskCache(filepath.Join(dir, "mc"), 3<<20)
	if dc == nil {
		t.Fatal("disk cache 未启用")
	}
	block := make([]byte, 1<<20)
	dc.storeBlock(1, 0, block, 1<<20, 1)
	dc.storeBlock(2, 0, block, 1<<20, 2)
	dc.storeBlock(3, 0, block, 1<<20, 3)

	dc.mu.Lock()
	dc.indexes[1].Accessed = time.Now().Unix() + 100
	dc.indexes[2].Accessed = 1
	dc.indexes[3].Accessed = 50
	dc.mu.Unlock()

	dc.storeBlock(3, 1, block, 3<<20, 3)
	dc.evictIfNeeded()

	if dc.hasBlock(2, 0) {
		t.Fatal("最久未访问的文档 2 应被淘汰")
	}
	if !dc.hasBlock(1, 0) {
		t.Fatal("最近访问的文档 1 不应被淘汰")
	}
	if b, _ := dc.usageStats(); b > 3<<20 {
		t.Fatalf("配额未生效: %d", b)
	}
}

func TestDiskCacheClear(t *testing.T) {
	dir := t.TempDir()
	dc := newDiskCache(filepath.Join(dir, "mc"), 64<<20)
	dc.storeBlock(7, 0, []byte("hello"), 5, 7)
	if b, _ := dc.usageStats(); b == 0 {
		t.Fatal("写入失败")
	}
	dc.clear()
	if b, n := dc.usageStats(); b != 0 || n != 0 {
		t.Fatalf("clear 后 usage = %d/%d", b, n)
	}
	if dc.hasBlock(7, 0) {
		t.Fatal("clear 后索引应清空")
	}
}
