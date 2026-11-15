package web

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"tgdrama/internal/rankings"
)

// handleRankings 榜单页签数据：各榜单 + 与本地片库匹配后的可播条目。
func (s *Server) handleRankings(w http.ResponseWriter, r *http.Request) {
	if s.rank == nil {
		writeJSON(w, http.StatusOK, rankings.Payload{Boards: []rankings.BoardView{}})
		return
	}
	writeJSON(w, http.StatusOK, s.rank.Payload())
}

// handleRankingsRefresh 手动触发一次抓取。仍然受 12 小时间隔约束：
// 没到时间直接回 200 + too_soon，前端据此提示，不会真的去打外部站点。
func (s *Server) handleRankingsRefresh(w http.ResponseWriter, r *http.Request) {
	if s.rank == nil {
		writeError(w, http.StatusBadRequest, "榜单功能未启用")
		return
	}
	err := s.rank.Refresh(r.Context(), false)
	switch {
	case err == nil:
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": s.rank.Payload()})
	case errors.Is(err, rankings.ErrTooSoon):
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":      false,
			"reason":  "too_soon",
			"message": "每 12 小时只更新一次，还没到下次更新时间",
			"data":    s.rank.Payload(),
		})
	default:
		writeError(w, http.StatusInternalServerError, err.Error())
	}
}

// handleRankingsRulesGet 查看规则文件现状（内容/条数/问题清单）。
func (s *Server) handleRankingsRulesGet(w http.ResponseWriter, r *http.Request) {
	if s.rank == nil {
		writeError(w, http.StatusBadRequest, "榜单功能未启用")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": s.rank.RulesStatus()})
}

// handleRankingsRulesPut 保存新规则文本（请求体就是 YAML 原文）。
// 校验通过才落盘；返回 problems 让前端/用户知道哪些条目被跳过了。
func (s *Server) handleRankingsRulesPut(w http.ResponseWriter, r *http.Request) {
	if s.rank == nil {
		writeError(w, http.StatusBadRequest, "榜单功能未启用")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 256<<10))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": "规则内容超过 256KB 或读取失败",
		})
		return
	}
	problems, err := s.rank.UpdateRules(string(body))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"ok": false, "error": err.Error(), "problems": problems,
		})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "problems": problems, "data": s.rank.RulesStatus(),
	})
}

// handleRankingsRulesTest 用当前规则试匹配一批标题，预览命中方式。
func (s *Server) handleRankingsRulesTest(w http.ResponseWriter, r *http.Request) {
	if s.rank == nil {
		writeError(w, http.StatusBadRequest, "榜单功能未启用")
		return
	}
	var req struct {
		Titles []string `json:"titles"`
		Title  string   `json:"title"` // 单标题的便捷写法
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "请求体需要是 JSON：{\"titles\": [\"...\"]}")
		return
	}
	if req.Title != "" && len(req.Titles) == 0 {
		req.Titles = []string{req.Title}
	}
	if len(req.Titles) == 0 {
		writeError(w, http.StatusBadRequest, "缺少 titles 字段")
		return
	}
	if len(req.Titles) > 50 {
		req.Titles = req.Titles[:50]
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "data": s.rank.TestMatch(req.Titles)})
}
