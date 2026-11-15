package web

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"

	"tgdrama/internal/storage"
)

// TestEpisodePlayStreams 宿主播放器用的 /play 地址直接输出视频流：
// 不再 302 到 /api/stream（部分宿主不跟随跳转，或在跳转后丢掉 Range/请求头）
func TestEpisodePlayStreams(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)

	lines, err := store.ListEpisodeLines(ep1)
	if err != nil || len(lines) == 0 || len(lines[0].Segments) == 0 {
		t.Fatalf("测试数据缺少线路: %v %v", err, lines)
	}
	src := lines[0].Segments[0]
	var calls int32
	srv.applyStreamOptions(256*1024, 64)
	srv.fetchChunk = fakeFetcher(&calls, src.FileSize)

	req := httptest.NewRequest(http.MethodGet, "/api/episodes/"+itoa(ep1)+"/play", nil)
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("play 应直接 200，得到 %d %s", rec.Code, rec.Body.String())
	}
	if loc := rec.Header().Get("Location"); loc != "" {
		t.Fatalf("不应再跳转，Location = %q", loc)
	}
	if got := rec.Header().Get("Content-Type"); got != "video/mp4" {
		t.Fatalf("Content-Type = %q", got)
	}
	if got := rec.Header().Get("Content-Length"); got != strconv.FormatInt(src.FileSize, 10) {
		t.Fatalf("Content-Length = %q", got)
	}

	// 拖动时宿主会带 Range：/play 也要能正常分段
	req = httptest.NewRequest(http.MethodGet, "/api/episodes/"+itoa(ep1)+"/play", nil)
	req.Header.Set("Range", "bytes=500-999")
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusPartialContent || rec.Body.Len() != 500 {
		t.Fatalf("Range 应 206 且 500 字节，得到 %d / %d", rec.Code, rec.Body.Len())
	}

	// 不存在的剧集
	rec = httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/episodes/99999/play", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在剧集应 404，得到 %d", rec.Code)
	}
}

// addSource 给某集补一条来源，用于模拟「预告片 + 正片」混在同一集
func addSource(t *testing.T, store *storage.DB, epID, tgChatID int64, msgID int,
	duration float64, size int64, name string) *storage.Source {
	t.Helper()
	chatID, err := store.UpsertChat(&storage.Chat{
		TgChatID: tgChatID, Name: name, Type: "channel", Enabled: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &storage.Source{
		EpisodeID: epID, ChatID: chatID, MessageID: msgID, SegmentIndex: 0,
		FileName: name, FileSize: size, Duration: duration, Mime: "video/mp4",
	}
	id, _, err := store.UpsertSource(s)
	if err != nil {
		t.Fatal(err)
	}
	got, err := store.GetSource(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// TestPickLongestSource 同一集里混入短预告片时，默认播放必须选时长最长的正片
func TestPickLongestSource(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store) // 原有来源：无时长信息、1MB
	trailer := addSource(t, store, ep1, 200, 2000, 15, 2<<20, "预告片.mp4")
	full := addSource(t, store, ep1, 300, 3000, 900, 8<<20, "正片.mp4")

	picked, err := srv.pickEpisodeSource(ep1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != full.ID || picked.Duration != 900 {
		t.Fatalf("应优先播放 900 秒正片，实际取到 %s(id=%d, 时长=%v)", picked.FileName, picked.ID, picked.Duration)
	}
	if picked.ID == trailer.ID {
		t.Fatal("不能把 15 秒预告片当成正片播放")
	}
	// 经典播放器默认取第 1 条线路，也要是正片线路
	lines, err := store.ListEpisodeLines(ep1)
	if err != nil || len(lines) == 0 {
		t.Fatalf("线路为空: %v", err)
	}
	if lines[0].Segments[0].ID != full.ID {
		t.Fatalf("首条线路应是正片，实际 %s", lines[0].Segments[0].FileName)
	}
}

// TestPickSmallestSizeWhenSameDuration 同一时长下取体积最小的版本：
// 码率越低网页拉流越快，卡顿越少。
func TestPickSmallestSizeWhenSameDuration(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store) // 1MB，无时长
	small := addSource(t, store, ep1, 500, 5000, 600, 4<<20, "低码率.mp4")
	addSource(t, store, ep1, 501, 5001, 600, 12<<20, "中等码率.mp4")
	addSource(t, store, ep1, 502, 5002, 600, 40<<20, "最高码率.mp4")

	picked, err := srv.pickEpisodeSource(ep1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != small.ID {
		t.Fatalf("同长度应选最小体积，实际 %s(%dMB)", picked.FileName, picked.FileSize>>20)
	}
}

// TestPickLineSource 指定线路时只在该频道的来源里挑
func TestPickLineSource(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	lineA := addSource(t, store, ep1, 600, 6000, 900, 8<<20, "频道A正片.mp4")
	lineB := addSource(t, store, ep1, 601, 6001, 1200, 20<<20, "频道B正片.mp4")

	// 不指定线路：时长更长的 B
	picked, err := srv.pickEpisodeSource(ep1, 0)
	if err != nil || picked.ID != lineB.ID {
		t.Fatalf("默认应选时长更长的 B，得到 %v %v", picked, err)
	}
	// 指定 A：即使 B 更长也只播 A
	picked, err = srv.pickEpisodeSource(ep1, lineA.ChatID)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != lineA.ID {
		t.Fatalf("指定线路 A 后应播 A，实际 %s", picked.FileName)
	}
}

// TestCORS 宿主页面（file:// 或其他源）需要能直接调用本机 API
func TestCORS(t *testing.T) {
	h, _ := newTestServer(t)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/tags", nil))
	if got := rec.Header().Get("Access-Control-Allow-Origin"); got != "*" {
		t.Fatalf("Allow-Origin = %q", got)
	}

	rec = httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodOptions, "/api/history", nil)
	req.Header.Set("Origin", "null")
	req.Header.Set("Access-Control-Request-Method", "POST")
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("预检应 204，得到 %d", rec.Code)
	}
	if rec.Header().Get("Access-Control-Allow-Methods") == "" {
		t.Fatal("预检缺少 Allow-Methods")
	}
}
