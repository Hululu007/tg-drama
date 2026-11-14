package web

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"testing"

	"tgdrama/internal/config"
	"tgdrama/internal/storage"
	"tgdrama/internal/tgclient"
)

func newTestServer(t *testing.T) (http.Handler, *storage.DB) {
	t.Helper()
	srv, store := newTestServerRaw(t)
	return srv.Handler(), store
}

func newTestServerRaw(t testing.TB) (*Server, *storage.DB) {
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
	return NewServer(store, svc), store
}

func doJSON(t *testing.T, h http.Handler, method, path string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func seedShow(t testing.TB, store *storage.DB) (showID, ep1, ep2 int64) {
	t.Helper()
	showID, _, err := store.UpsertShow("测试短剧", "测试短剧")
	if err != nil {
		t.Fatal(err)
	}
	ep1, _, err = store.UpsertEpisode(showID, 1)
	if err != nil {
		t.Fatal(err)
	}
	ep2, _, err = store.UpsertEpisode(showID, 2)
	if err != nil {
		t.Fatal(err)
	}

	chatID, err := store.UpsertChat(&storage.Chat{TgChatID: 100, Name: "测试频道", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, ep := range []int64{ep1, ep2} {
		if _, _, err := store.UpsertSource(&storage.Source{
			EpisodeID:      ep,
			ChatID:         chatID,
			MessageID:      1000 + i,
			SegmentIndex:   0,
			FileName:       "test.mp4",
			FileSize:       1 << 20,
			Mime:           "video/mp4",
			DownloadStatus: "pending",
		}); err != nil {
			t.Fatal(err)
		}
	}
	return showID, ep1, ep2
}

func TestHistoryAndFavorites(t *testing.T) {
	h, store := newTestServer(t)
	showID, ep1, ep2 := seedShow(t, store)

	code, out := doJSON(t, h, http.MethodPost, "/api/history", map[string]any{
		"episode_id": ep1, "position": 120.0, "duration": 600.0,
	})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("保存进度失败: %d %v", code, out)
	}

	code, out = doJSON(t, h, http.MethodGet, "/api/history", nil)
	list, _ := out["list"].([]any)
	if code != http.StatusOK || len(list) != 1 {
		t.Fatalf("历史列表 = %d %v", code, out)
	}
	item := list[0].(map[string]any)
	if item["show_id"].(float64) != float64(showID) || item["episode_no"].(float64) != 1 {
		t.Fatalf("历史条目不对: %v", item)
	}
	if item["progress"].(float64) < 0.19 || item["progress"].(float64) > 0.21 {
		t.Fatalf("进度比例 = %v", item["progress"])
	}

	code, out = doJSON(t, h, http.MethodGet, "/api/shows/"+itoa(showID), nil)
	if code != http.StatusOK || out["favorited"] != false {
		t.Fatalf("详情收藏状态: %d %v", code, out["favorited"])
	}
	prog, _ := out["progress"].([]any)
	if len(prog) != 1 {
		t.Fatalf("详情进度条数 = %d", len(prog))
	}
	cont, _ := out["continue"].(map[string]any)
	if cont == nil || cont["episode_id"].(float64) != float64(ep1) {
		t.Fatalf("继续播放 = %v", out["continue"])
	}

	code, out = doJSON(t, h, http.MethodPost, "/api/favorites/toggle", map[string]any{"show_id": showID})
	if code != http.StatusOK || out["favorited"] != true {
		t.Fatalf("收藏失败: %d %v", code, out)
	}
	code, out = doJSON(t, h, http.MethodGet, "/api/shows?fav=1", nil)
	if code != http.StatusOK || out["total"].(float64) != 1 {
		t.Fatalf("收藏列表: %d %v", code, out)
	}
	_, out = doJSON(t, h, http.MethodGet, "/api/favorites", nil)
	if out["total"].(float64) != 1 {
		t.Fatalf("收藏计数: %v", out["total"])
	}

	code, out = doJSON(t, h, http.MethodGet, "/api/episodes/"+itoa(ep1)+"/lines", nil)
	if code != http.StatusOK || out["episode"] == nil || out["progress"] == nil {
		t.Fatalf("播放信息缺失: %d %v", code, out)
	}

	doJSON(t, h, http.MethodPost, "/api/history", map[string]any{
		"episode_id": ep2, "position": 500.0, "duration": 500.0,
	})
	_, out = doJSON(t, h, http.MethodGet, "/api/history", nil)
	list = out["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("聚合后历史条数 = %d", len(list))
	}
	if item = list[0].(map[string]any); item["episode_no"].(float64) != 2 || item["finished"] != true {
		t.Fatalf("最新历史条目 = %v", item)
	}

	code, out = doJSON(t, h, http.MethodDelete, "/api/history/"+itoa(ep2), nil)
	if code != http.StatusOK {
		t.Fatalf("删除历史 = %d %v", code, out)
	}
	_, out = doJSON(t, h, http.MethodGet, "/api/history", nil)
	if list = out["list"].([]any); len(list) != 1 || list[0].(map[string]any)["episode_no"].(float64) != 1 {
		t.Fatalf("删除后应剩余第一集: %v", list)
	}

	code, _ = doJSON(t, h, http.MethodDelete, "/api/history", nil)
	if code != http.StatusOK {
		t.Fatalf("清空历史 = %d", code)
	}
	_, out = doJSON(t, h, http.MethodGet, "/api/history", nil)
	if list = out["list"].([]any); len(list) != 0 {
		t.Fatalf("清空后历史 = %v", list)
	}

	code, out = doJSON(t, h, http.MethodPost, "/api/favorites/toggle", map[string]any{"show_id": showID})
	if code != http.StatusOK || out["favorited"] != false {
		t.Fatalf("取消收藏: %d %v", code, out)
	}
	_, out = doJSON(t, h, http.MethodGet, "/api/shows?fav=1", nil)
	if out["total"].(float64) != 0 {
		t.Fatalf("取消后收藏数 = %v", out["total"])
	}

	if code, _ = doJSON(t, h, http.MethodPost, "/api/history", map[string]any{"episode_id": 99999}); code != http.StatusNotFound {
		t.Fatalf("不存在剧集应 404，得到 %d", code)
	}
	if code, _ = doJSON(t, h, http.MethodDelete, "/api/history/abc", nil); code != http.StatusBadRequest {
		t.Fatalf("非法ID应 400，得到 %d", code)
	}
	if code, _ = doJSON(t, h, http.MethodPost, "/api/favorites/toggle", map[string]any{"show_id": 99999}); code != http.StatusNotFound {
		t.Fatalf("收藏不存在短剧应 404，得到 %d", code)
	}
}

func itoa(v int64) string {
	return strconv.FormatInt(v, 10)
}

func TestFavoriteUpdates(t *testing.T) {
	h, store := newTestServer(t)
	showID, ep1, ep2 := seedShow(t, store)

	if on, err := store.ToggleFavorite(showID); err != nil || !on {
		t.Fatalf("收藏失败: %v", err)
	}

	if err := store.SaveHistory(ep1, 120, 600); err != nil {
		t.Fatal(err)
	}

	ep3, _, err := store.UpsertEpisode(showID, 3)
	if err != nil {
		t.Fatal(err)
	}
	addSource(t, store, ep3, 700, 7000, 900, 8<<20, "第3集.mp4")

	list, err := store.ListFavoriteShows("", 10, 0)
	if err != nil || len(list) != 1 {
		t.Fatalf("收藏列表 = %v %v", err, list)
	}
	if list[0].Updates != 0 || list[0].WatchedEp != 1 {
		t.Fatalf("停在第一集不应提示更新：updates=%d watched=%d", list[0].Updates, list[0].WatchedEp)
	}

	if err := store.SaveHistory(ep2, 60, 600); err != nil {
		t.Fatal(err)
	}
	list, _ = store.ListFavoriteShows("", 10, 0)
	if list[0].Updates != 1 || list[0].WatchedEp != 2 {
		t.Fatalf("追上后应提示 1 集更新：updates=%d watched=%d", list[0].Updates, list[0].WatchedEp)
	}
	if n, err := store.CountFavoriteUpdates(); err != nil || n != 1 {
		t.Fatalf("角标合计 = %d, %v", n, err)
	}

	_, out := doJSON(t, h, http.MethodGet, "/api/shows?fav=1", nil)
	if out["updates_total"].(float64) != 1 {
		t.Fatalf("收藏列表 updates_total = %v", out["updates_total"])
	}
	_, out = doJSON(t, h, http.MethodGet, "/api/favorites/summary", nil)
	if out["updates_total"].(float64) != 1 {
		t.Fatalf("汇总接口 updates_total = %v", out["updates_total"])
	}

	if err := store.SaveHistory(ep3, 900, 900); err != nil {
		t.Fatal(err)
	}
	list, _ = store.ListFavoriteShows("", 10, 0)
	if list[0].Updates != 0 {
		t.Fatalf("看完后应归零，得到 %d", list[0].Updates)
	}
	if n, _ := store.CountFavoriteUpdates(); n != 0 {
		t.Fatalf("看完后角标应归零，得到 %d", n)
	}
}

func TestFavoriteUpdatesIgnoreWhenBehind(t *testing.T) {
	_, store := newTestServer(t)
	showID, ep1, _ := seedShow(t, store)

	if _, err := store.ToggleFavorite(showID); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveHistory(ep1, 10, 600); err != nil {
		t.Fatal(err)
	}
	ep3, _, _ := store.UpsertEpisode(showID, 3)
	addSource(t, store, ep3, 800, 8000, 900, 8<<20, "第3集.mp4")

	list, _ := store.ListFavoriteShows("", 10, 0)
	if list[0].Updates != 0 {
		t.Fatalf("进度落后时不应计数，得到 %d", list[0].Updates)
	}
	if n, _ := store.CountFavoriteUpdates(); n != 0 {
		t.Fatalf("进度落后时角标应为 0，得到 %d", n)
	}
}
