package web

import (
	"net/http"
	"testing"

	"tgdrama/internal/storage"
)

func historyCount(t *testing.T, h http.Handler, stage string) int {
	t.Helper()
	code, out := doJSON(t, h, http.MethodGet, "/api/bootstrap", nil)
	if code != http.StatusOK {
		t.Fatalf("%s: bootstrap 返回 %d %v", stage, code, out)
	}
	if list, ok := out["continue_list"].([]any); ok {
		return len(list)
	}
	return 0
}

func TestDisabledChatHidesHistory(t *testing.T) {
	h, store := newTestServer(t)
	_, ep1, _ := seedShow(t, store)

	if code, out := doJSON(t, h, http.MethodPost, "/api/history",
		map[string]any{"episode_id": ep1, "position": 30, "duration": 120}); code != http.StatusOK {
		t.Fatalf("上报进度失败: %d %v", code, out)
	}
	if n := historyCount(t, h, "启用时"); n != 1 {
		t.Fatalf("启用时继续观看 = %d，期望 1", n)
	}

	if err := store.SetChatEnabled(100, false); err != nil {
		t.Fatal(err)
	}
	if n := historyCount(t, h, "关闭频道后"); n != 0 {
		t.Fatalf("关闭频道后继续观看 = %d，期望 0（关闭频道的内容不该再出现）", n)
	}

	if err := store.SetChatEnabled(100, true); err != nil {
		t.Fatal(err)
	}
	if n := historyCount(t, h, "重新启用"); n != 1 {
		t.Fatalf("重新启用后继续观看 = %d，期望 1", n)
	}
}

func TestNonDramaSingleHiddenNormalSingleShown(t *testing.T) {
	h, store := newTestServer(t)

	chatID, err := store.UpsertChat(&storage.Chat{TgChatID: 700, Name: "福利频道", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(name, file, caption string, msgID int, cover string) int64 {
		t.Helper()
		sid, _, err := store.UpsertShow(name, name)
		if err != nil {
			t.Fatal(err)
		}
		ep, _, err := store.UpsertEpisode(sid, 1)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.UpsertSource(&storage.Source{
			EpisodeID: ep, ChatID: chatID, MessageID: msgID, FileName: file, Caption: caption,
		}); err != nil {
			t.Fatal(err)
		}
		if cover != "" {
			if err := store.SetShowCover(sid, cover); err != nil {
				t.Fatal(err)
			}
		}
		return sid
	}
	junkID := mk("演唱会福利", "concert.mp4", "", 9101, "")
	dramaID := mk("远亲不如近邻", "drama.mp4", "《远亲不如近邻》第1集 全集", 9102, "covers/drama.jpg")

	n, err := store.RecomputeHiddenShows()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("隐藏条目数 = %d，期望 1", n)
	}

	code, out := doJSON(t, h, http.MethodGet, "/api/shows", nil)
	if code != http.StatusOK {
		t.Fatalf("列表返回 %d %v", code, out)
	}
	list, _ := out["list"].([]any)
	if len(list) != 1 {
		t.Fatalf("列表 = %d 条，期望 1（只留正常单集剧）", len(list))
	}
	if got, _ := list[0].(map[string]any)["id"].(float64); int64(got) != dramaID {
		t.Fatalf("列表里应只剩正常单集剧 %d，实际拿到 %v", dramaID, list[0])
	}

	if code, _ := doJSON(t, h, http.MethodGet, "/api/shows/"+itoa(junkID), nil); code != http.StatusOK {
		t.Fatalf("演唱会条目详情返回 %d，期望 200", code)
	}
	if code, out := doJSON(t, h, http.MethodGet, "/api/search?q=%E6%BC%94%E5%94%B1%E4%BC%9A", nil); code != http.StatusOK {
		t.Fatalf("搜索返回 %d", code)
	} else if list, _ := out["list"].([]any); len(list) != 1 {
		t.Fatalf("搜索应能搜到被隐藏的条目，实际 %d 条", len(list))
	}
}

func TestHiddenShowRemovedFromList(t *testing.T) {
	h, store := newTestServer(t)

	chatID, err := store.UpsertChat(&storage.Chat{TgChatID: 500, Name: "真人自拍频道", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	showID, _, err := store.UpsertShow("露脸", "露脸")
	if err != nil {
		t.Fatal(err)
	}
	epID, _, err := store.UpsertEpisode(showID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpsertSource(&storage.Source{
		EpisodeID: epID, ChatID: chatID, MessageID: 9001, FileName: "a.mp4",
	}); err != nil {
		t.Fatal(err)
	}

	ep2, _, err := store.UpsertEpisode(showID, 2)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.UpsertSource(&storage.Source{
		EpisodeID: ep2, ChatID: chatID, MessageID: 9002, FileName: "b.mp4",
	}); err != nil {
		t.Fatal(err)
	}
	if c, out := doJSON(t, h, http.MethodPost, "/api/history",
		map[string]any{"episode_id": epID, "position": 5, "duration": 60}); c != http.StatusOK {
		t.Fatalf("上报进度失败: %d %v", c, out)
	}

	n, err := store.RecomputeHiddenShows()
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("隐藏条目数 = %d，期望 1", n)
	}

	if code, out := doJSON(t, h, http.MethodGet, "/api/shows", nil); code != http.StatusOK {
		t.Fatalf("列表返回 %d %v", code, out)
	} else if list, _ := out["list"].([]any); len(list) != 0 {
		t.Fatalf("隐藏后列表 = %d 条，期望 0", len(list))
	}
	if code, _ := doJSON(t, h, http.MethodGet, "/api/shows/"+itoa(showID), nil); code != http.StatusOK {
		t.Fatalf("隐藏后详情返回 %d，期望 200（仅列表隐藏）", code)
	}
	if n := historyCount(t, h, "隐藏后"); n != 0 {
		t.Fatalf("隐藏后继续观看 = %d，期望 0", n)
	}

	if _, _, err := store.UpsertShow("真实短剧", "露脸"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RecomputeHiddenShows(); err != nil {
		t.Fatal(err)
	}
	if code, out := doJSON(t, h, http.MethodGet, "/api/shows", nil); code != http.StatusOK {
		t.Fatalf("列表返回 %d %v", code, out)
	} else if list, _ := out["list"].([]any); len(list) != 1 {
		t.Fatalf("恢复后列表 = %d 条，期望 1", len(list))
	}
}
