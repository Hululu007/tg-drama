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

// newTestServer 建一个带真实 SQLite 的测试服务，方便直接造数据
func newTestServer(t *testing.T) (http.Handler, *storage.DB) {
	t.Helper()
	srv, store := newTestServerRaw(t)
	return srv.Handler(), store
}

// newTestServerRaw 同上，但返回 *Server，便于替换流读取实现
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
	// sources.chat_id 外键指向 chats(id)，先建一个频道
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

	// 上报进度
	code, out := doJSON(t, h, http.MethodPost, "/api/history", map[string]any{
		"episode_id": ep1, "position": 120.0, "duration": 600.0,
	})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("保存进度失败: %d %v", code, out)
	}

	// 历史列表按剧聚合
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

	// 详情页带收藏状态、进度、继续播放
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

	// 收藏
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

	// 播放页：lines 里带剧集与进度
	code, out = doJSON(t, h, http.MethodGet, "/api/episodes/"+itoa(ep1)+"/lines", nil)
	if code != http.StatusOK || out["episode"] == nil || out["progress"] == nil {
		t.Fatalf("播放信息缺失: %d %v", code, out)
	}

	// 第二集看完：历史仍只有一条（按剧聚合），且换成第二集
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

	// 删除单条历史
	code, out = doJSON(t, h, http.MethodDelete, "/api/history/"+itoa(ep2), nil)
	if code != http.StatusOK {
		t.Fatalf("删除历史 = %d %v", code, out)
	}
	_, out = doJSON(t, h, http.MethodGet, "/api/history", nil)
	if list = out["list"].([]any); len(list) != 1 || list[0].(map[string]any)["episode_no"].(float64) != 1 {
		t.Fatalf("删除后应剩余第一集: %v", list)
	}

	// 清空历史
	code, _ = doJSON(t, h, http.MethodDelete, "/api/history", nil)
	if code != http.StatusOK {
		t.Fatalf("清空历史 = %d", code)
	}
	_, out = doJSON(t, h, http.MethodGet, "/api/history", nil)
	if list = out["list"].([]any); len(list) != 0 {
		t.Fatalf("清空后历史 = %v", list)
	}

	// 取消收藏
	code, out = doJSON(t, h, http.MethodPost, "/api/favorites/toggle", map[string]any{"show_id": showID})
	if code != http.StatusOK || out["favorited"] != false {
		t.Fatalf("取消收藏: %d %v", code, out)
	}
	_, out = doJSON(t, h, http.MethodGet, "/api/shows?fav=1", nil)
	if out["total"].(float64) != 0 {
		t.Fatalf("取消后收藏数 = %v", out["total"])
	}

	// 异常输入
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

// TestFavoriteUpdates 收藏更新提醒的判定：
//   - 收藏时已有的集不算更新（基线 = 收藏时的最新集）
//   - 还停在第一集不提示
//   - 追到收藏时的最新一集后，新发布的集才计数
//   - 看完新集后归零
func TestFavoriteUpdates(t *testing.T) {
	h, store := newTestServer(t)
	showID, ep1, ep2 := seedShow(t, store)

	// 收藏时这部剧有 2 集，基线 = 2
	if on, err := store.ToggleFavorite(showID); err != nil || !on {
		t.Fatalf("收藏失败: %v", err)
	}
	// 只看了第 1 集
	if err := store.SaveHistory(ep1, 120, 600); err != nil {
		t.Fatal(err)
	}
	// 之后频道更新到第 3 集
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

	// 追到收藏时的最新一集（第 2 集）
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

	// 收藏页接口也要带上同样的数字
	_, out := doJSON(t, h, http.MethodGet, "/api/shows?fav=1", nil)
	if out["updates_total"].(float64) != 1 {
		t.Fatalf("收藏列表 updates_total = %v", out["updates_total"])
	}
	_, out = doJSON(t, h, http.MethodGet, "/api/favorites/summary", nil)
	if out["updates_total"].(float64) != 1 {
		t.Fatalf("汇总接口 updates_total = %v", out["updates_total"])
	}

	// 看完第 3 集后更新归零
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

// TestFavoriteUpdatesIgnoreWhenBehind 收藏时就没追到最新一集（进度落后很多），
// 之后发布的新集不算「这次更新」，避免一收藏就冒出几十集未读。
func TestFavoriteUpdatesIgnoreWhenBehind(t *testing.T) {
	_, store := newTestServer(t)
	showID, ep1, _ := seedShow(t, store)

	if _, err := store.ToggleFavorite(showID); err != nil { // 基线 = 2
		t.Fatal(err)
	}
	if err := store.SaveHistory(ep1, 10, 600); err != nil { // 只看了第 1 集
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
