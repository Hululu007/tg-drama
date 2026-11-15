package web

import (
	"net/http"
	"testing"

	"tgdrama/internal/storage"
)

// 关闭频道后：该频道带来的剧从列表、搜索、收藏、详情与播放线路里一并消失；
// 数据不删，重新启用后立刻恢复。
func TestDisabledChatHidesShows(t *testing.T) {
	h, store := newTestServer(t)
	showID, ep1, _ := seedShow(t, store)
	if err := store.AddShowTags(showID, []string{"短剧"}); err != nil {
		t.Fatal(err)
	}
	if code, out := doJSON(t, h, http.MethodPost, "/api/favorites/toggle",
		map[string]any{"show_id": showID}); code != http.StatusOK {
		t.Fatalf("收藏失败: %d %v", code, out)
	}

	showCount := func(path, stage string) int {
		t.Helper()
		code, out := doJSON(t, h, http.MethodGet, path, nil)
		if code != http.StatusOK {
			t.Fatalf("%s: %s 返回 %d %v", stage, path, code, out)
		}
		list, _ := out["list"].([]any)
		return len(list)
	}

	if n := showCount("/api/shows", "启用时"); n != 1 {
		t.Fatalf("启用时列表 = %d，期望 1", n)
	}

	if err := store.SetChatEnabled(100, false); err != nil {
		t.Fatal(err)
	}

	if n := showCount("/api/shows", "关闭后"); n != 0 {
		t.Fatalf("关闭频道后列表 = %d，期望 0", n)
	}
	if n := showCount("/api/shows?fav=1", "关闭后"); n != 0 {
		t.Fatalf("关闭频道后收藏列表 = %d，期望 0", n)
	}
	if n := showCount("/api/search?q=测试短剧", "关闭后"); n != 0 {
		t.Fatalf("关闭频道后搜索结果 = %d，期望 0", n)
	}
	if tags, err := store.ListTags(); err != nil {
		t.Fatal(err)
	} else if len(tags) != 0 {
		t.Fatalf("关闭频道后标签 = %d 个，期望 0", len(tags))
	}
	if code, _ := doJSON(t, h, http.MethodGet, "/api/shows/"+itoa(showID), nil); code != http.StatusNotFound {
		t.Fatalf("关闭频道后详情返回 %d，期望 404", code)
	}
	if code, out := doJSON(t, h, http.MethodGet, "/api/episodes/"+itoa(ep1)+"/lines", nil); code != http.StatusOK {
		t.Fatalf("线路接口返回 %d", code)
	} else if lines, _ := out["lines"].([]any); len(lines) != 0 {
		t.Fatalf("关闭频道后线路 = %d 条，期望 0", len(lines))
	}

	// 重新启用：全部恢复
	if err := store.SetChatEnabled(100, true); err != nil {
		t.Fatal(err)
	}
	if n := showCount("/api/shows", "重新启用"); n != 1 {
		t.Fatalf("重新启用后列表 = %d，期望 1", n)
	}
	if code, _ := doJSON(t, h, http.MethodGet, "/api/shows/"+itoa(showID), nil); code != http.StatusOK {
		t.Fatalf("重新启用后详情返回 %d，期望 200", code)
	}
}

// 关闭频道必须让 ETag 变化，否则前端会一直命中 304 看到旧列表。
func TestDisabledChatChangesVersionTag(t *testing.T) {
	_, store := newTestServer(t)
	if _, err := store.UpsertChat(&storage.Chat{TgChatID: 200, Name: "频道", Enabled: true}); err != nil {
		t.Fatal(err)
	}
	before := store.VersionTag()
	if err := store.SetChatEnabled(200, false); err != nil {
		t.Fatal(err)
	}
	if after := store.VersionTag(); after == before {
		t.Fatalf("关闭频道后 VersionTag 未变化: %s", after)
	}
}

func TestChatRecommendShowsAllDefaults(t *testing.T) {
	h, store := newTestServer(t)

	// 账号里没有任何频道时也要完整展示默认清单：新账号才能从这里加入。
	code, out := doJSON(t, h, http.MethodGet, "/api/chats/recommend", nil)
	if code != http.StatusOK {
		t.Fatalf("推荐接口返回 %d %v", code, out)
	}
	list, _ := out["list"].([]any)
	if len(list) != len(defaultRecommendChannels) {
		t.Fatalf("推荐频道数 = %d，期望固定 %d 条: %v", len(list), len(defaultRecommendChannels), list)
	}
	first := list[0].(map[string]any)
	if first["url"] != "https://t.me/aiduanju_huang_666_888" || first["joined"] != false {
		t.Fatalf("未加入时也应展示并可点: %v", first)
	}

	// 已加入的频道（本地有 chats 行）带上「已加入」角标，但不影响列表条数。
	if _, err := store.UpsertChat(&storage.Chat{TgChatID: 4425471457, Name: "小黄鸭", Username: "aiduanju_huang_666_888"}); err != nil {
		t.Fatal(err)
	}
	code, out = doJSON(t, h, http.MethodGet, "/api/chats/recommend", nil)
	if code != http.StatusOK {
		t.Fatalf("推荐接口返回 %d %v", code, out)
	}
	list, _ = out["list"].([]any)
	if len(list) != len(defaultRecommendChannels) {
		t.Fatalf("加入后推荐频道数变为 %d，期望仍为 %d", len(list), len(defaultRecommendChannels))
	}
	if got := list[0].(map[string]any)["joined"]; got != true {
		t.Fatalf("已加入频道的 joined 应为 true: %v", got)
	}
}
