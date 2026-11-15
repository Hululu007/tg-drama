package web

import (
	"net/http"

	"tgdrama/internal/storage"
)

// handleStream 视频播放：已缓存的本地文件直接响应（支持 Range 拖动）；
// 未缓存时直接从 Telegram 实时流式代理，不先下载落盘。
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

// serveSource 输出一段视频：磁盘块缓存齐了走磁盘；否则 MTProto 实时流式代理。
func (s *Server) serveSource(w http.ResponseWriter, r *http.Request, src *storage.Source) {
	if src == nil {
		writeError(w, http.StatusNotFound, "视频不存在")
		return
	}
	s.streamTelegram(w, r, src)
}
