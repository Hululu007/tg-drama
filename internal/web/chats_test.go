package web

import (
	"net/http"
	"testing"

	"tgdrama/internal/storage"
)

// TestChatsPanel 频道管理面板：列表带统计、启用开关、未知频道报错
func TestChatsPanel(t *testing.T) {
	h, store := newTestServer(t)
	seedShow(t, store) // 建一个已启用的频道 + 2 条来源

	if _, err := store.UpsertChat(&storage.Chat{
		TgChatID: 9001, Name: "待启用频道", Type: "channel", Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}

	code, out := doJSON(t, h, http.MethodGet, "/api/chats", nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/chats = %d", code)
	}
	list, _ := out["list"].([]any)
	if len(list) != 2 {
		t.Fatalf("频道数 = %d，期望 2：%v", len(list), out)
	}
	if enabled, _ := out["enabled"].(float64); enabled != 1 {
		t.Fatalf("已启用数 = %v，期望 1", out["enabled"])
	}
	first, _ := list[0].(map[string]any)
	if first["name"] != "测试频道" {
		t.Fatalf("已启用频道应排在最前，实际 %v", first["name"])
	}
	if n, _ := first["source_count"].(float64); n != 2 {
		t.Fatalf("source_count = %v，期望 2", first["source_count"])
	}

	// 启用「待启用频道」：未登录时不会真的同步，但开关状态要落库
	code, out = doJSON(t, h, http.MethodPost, "/api/chats/toggle", map[string]any{
		"tg_chat_id": 9001, "enabled": true,
	})
	if code != http.StatusOK || out["ok"] != true {
		t.Fatalf("toggle = %d %v", code, out)
	}
	if sync, _ := out["syncing"].(bool); sync {
		t.Fatalf("未登录时不应该启动同步：%v", out)
	}
	c, err := store.GetChatByTgID(9001)
	if err != nil || c == nil || !c.Enabled {
		t.Fatalf("启用状态未落库：%v %v", c, err)
	}

	// 停用
	if code, _ = doJSON(t, h, http.MethodPost, "/api/chats/toggle", map[string]any{
		"tg_chat_id": 9001, "enabled": false,
	}); code != http.StatusOK {
		t.Fatalf("停用 = %d", code)
	}
	if c, _ = store.GetChatByTgID(9001); c == nil || c.Enabled {
		t.Fatalf("停用状态未落库：%v", c)
	}

	// 未知频道
	if code, _ = doJSON(t, h, http.MethodPost, "/api/chats/toggle", map[string]any{
		"tg_chat_id": 424242, "enabled": true,
	}); code != http.StatusBadRequest {
		t.Fatalf("未知频道应返回 400，实际 %d", code)
	}

	// 手动同步：未登录 503
	if code, _ = doJSON(t, h, http.MethodPost, "/api/chats/sync", map[string]any{
		"tg_chat_id": 9001,
	}); code != http.StatusServiceUnavailable {
		t.Fatalf("未登录同步应返回 503，实际 %d", code)
	}

	// 打开面板时的「更新频道」：未登录也要能返回本地列表，只是 refreshed=false
	code, out = doJSON(t, h, http.MethodPost, "/api/chats/refresh", nil)
	if code != http.StatusOK {
		t.Fatalf("POST /api/chats/refresh = %d", code)
	}
	if out["refreshed"] != false {
		t.Fatalf("未登录不应标记已刷新：%v", out)
	}
	if list, _ = out["list"].([]any); len(list) != 2 {
		t.Fatalf("刷新后频道数 = %d，期望 2：%v", len(list), out)
	}
}
