package web

import (
	"net/http"

	"tgdrama/internal/storage"
)

func (s *Server) handleStream(w http.ResponseWriter, r *http.Request) {
	id, err := parsePathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的视频ID")
		return
	}
	src, err := s.store.GetSource(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "视频不存在")
		return
	}

	s.serveSource(w, r, src)
}

func (s *Server) serveSource(w http.ResponseWriter, r *http.Request, src *storage.Source) {
	if src == nil {
		writeError(w, http.StatusNotFound, "视频不存在")
		return
	}
	s.streamTelegram(w, r, src)
}
