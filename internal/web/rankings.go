package web

import (
	"errors"
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
