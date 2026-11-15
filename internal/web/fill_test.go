package web

import (
	"net/http"
	"testing"

	"tgdrama/internal/storage"
)

// seedGapShow 造一部「第 1、3 集可播、缺第 2 集」的短剧
func seedGapShow(t *testing.T, store *storage.DB) int64 {
	t.Helper()
	showID, _, err := store.UpsertShow("缺集测试剧", "缺集测试剧")
	if err != nil {
		t.Fatal(err)
	}
	chatID, err := store.UpsertChat(&storage.Chat{TgChatID: 200, Name: "缺集频道", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	for i, no := range []int{1, 3} {
		epID, _, err := store.UpsertEpisode(showID, no)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := store.UpsertSource(&storage.Source{
			EpisodeID:      epID,
			ChatID:         chatID,
			MessageID:      2000 + i,
			FileName:       "gap.mp4",
			DownloadStatus: "pending",
		}); err != nil {
			t.Fatal(err)
		}
	}
	return showID
}

// TestFillEpisodesNoGap 选集连续时不应该触发搜索
func TestFillEpisodesNoGap(t *testing.T) {
	h, store := newTestServer(t)
	showID, _, _ := seedShow(t, store)

	code, out := doJSON(t, h, http.MethodPost, "/api/shows/"+itoa(showID)+"/fill", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d %v", code, out)
	}
	if out["searched"] != false {
		t.Fatalf("不该发起搜索: %v", out)
	}
	if n, _ := out["added"].(float64); n != 0 {
		t.Fatalf("added = %v", out["added"])
	}
}

// TestFillEpisodesGapReportsMissing 缺集但未登录时应把缺失集号与提示返回给前端
func TestFillEpisodesGapReportsMissing(t *testing.T) {
	h, store := newTestServer(t)
	showID := seedGapShow(t, store)

	code, out := doJSON(t, h, http.MethodPost, "/api/shows/"+itoa(showID)+"/fill", map[string]any{})
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d %v", code, out)
	}
	missing, _ := out["missing"].([]any)
	if len(missing) != 1 || missing[0].(float64) != 2 {
		t.Fatalf("缺失集号 = %v", out["missing"])
	}
	if out["error"] == nil {
		t.Fatalf("未登录时应返回 error: %v", out)
	}
}

// TestFillEpisodesForceIgnoresNoGap 选集没有空洞时，force 仍然要发起搜索
// （连载剧只同步到第 2 集时就是这种情形，自动补全永远不会触发）
func TestFillEpisodesForceIgnoresNoGap(t *testing.T) {
	h, store := newTestServer(t)
	showID, _, _ := seedShow(t, store)

	code, out := doJSON(t, h, http.MethodPost, "/api/shows/"+itoa(showID)+"/fill", map[string]any{"force": true})
	if code != http.StatusOK {
		t.Fatalf("状态码 = %d %v", code, out)
	}
	if out["error"] == nil {
		t.Fatalf("force 应该真的去搜索（未登录时返回 error）: %v", out)
	}
	if missing, _ := out["missing"].([]any); len(missing) != 0 {
		t.Fatalf("该剧没有空洞: %v", out["missing"])
	}
}

// TestFillEpisodesNotFound 不存在的短剧返回 404
func TestFillEpisodesNotFound(t *testing.T) {
	h, _ := newTestServer(t)
	code, out := doJSON(t, h, http.MethodPost, "/api/shows/9999/fill", map[string]any{})
	if code != http.StatusNotFound {
		t.Fatalf("状态码 = %d %v", code, out)
	}
}
