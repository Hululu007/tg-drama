package web

import (
	"compress/gzip"
	"embed"
	"io/fs"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"tgdrama/internal/storage"
	"tgdrama/internal/tgclient"
)

//go:embed static
var staticFS embed.FS

type Server struct {
	store *storage.DB
	svc   *tgclient.Service

	chunks *streamCache

	headSize   int64
	blockSize  int64
	fetchChunk sourceFetchFunc

	chunkTimeout time.Duration

	chunkOverallTimeout time.Duration

	disk *diskCache

	refreshingVariants sync.Map

	cacheDir string
}

func NewServer(store *storage.DB, svc *tgclient.Service) *Server {
	s := &Server{store: store, svc: svc}
	s.applyStreamOptions(streamDefaultChunk, streamDefaultCacheMB)
	s.fetchChunk = s.fetchChunkFromTelegram
	return s
}

func (s *Server) SetCacheDir(dir string) { s.cacheDir = dir }

func (s *Server) SetStreamOptions(chunkKB, cacheMB, diskCacheMB int) {
	s.applyStreamOptions(int64(chunkKB)*1024, cacheMB)
	s.setDiskCache(diskCacheMB)
	if s.fetchChunk == nil {
		s.fetchChunk = s.fetchChunkFromTelegram
	}
}

func (s *Server) applyStreamOptions(chunkSize int64, cacheMB int) {
	if chunkSize < 64*1024 {
		chunkSize = 64 * 1024
	}
	if chunkSize > streamMaxSingleRequest {
		chunkSize = streamMaxSingleRequest
	}

	chunkSize -= chunkSize % 4096
	if cacheMB < 8 {
		cacheMB = 8
	}
	if cacheMB > 1024 {
		cacheMB = 1024
	}
	s.blockSize = chunkSize

	s.headSize = streamHeadBlock
	if s.headSize > s.blockSize {
		s.headSize = s.blockSize
	}
	s.headSize -= s.headSize % 4096
	s.chunks = newStreamCache(chunkSize, int64(cacheMB)<<20)
	if s.chunkTimeout <= 0 || s.chunkTimeout > chunkFetchTimeout {
		s.chunkTimeout = chunkFetchTimeout
	}
	if s.chunkOverallTimeout <= 0 {
		s.chunkOverallTimeout = chunkOverallTimeout
	}
}

func (s *Server) setDiskCache(diskMB int) {
	if diskMB <= 0 || s.cacheDir == "" {
		s.disk = nil
		return
	}
	quota := int64(diskMB) << 20
	dir := filepath.Join(filepath.Dir(s.cacheDir), "mediacache")
	s.disk = newDiskCache(dir, quota)
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/account/state", s.handleAuthStatus)
	mux.HandleFunc("POST /api/account/mode", s.handleAuthMode)
	mux.HandleFunc("GET /api/account/qr", s.handleQR)
	mux.HandleFunc("GET /api/account/qr.png", s.handleQRImage)
	mux.HandleFunc("POST /api/account/phone", s.handleSubmitPhone)
	mux.HandleFunc("POST /api/account/code", s.handleSubmitCode)
	mux.HandleFunc("POST /api/account/password", s.handleSubmitPassword)
	mux.HandleFunc("POST /api/account/logout", s.handleAuthLogout)

	mux.HandleFunc("GET /api/bootstrap", s.handleBootstrap)
	mux.HandleFunc("GET /api/tags", s.handleTags)
	mux.HandleFunc("GET /api/shows", s.handleShows)
	mux.HandleFunc("GET /api/shows/{id}", s.handleShowDetail)
	mux.HandleFunc("POST /api/shows/{id}/fill", s.handleFillEpisodes)
	mux.HandleFunc("GET /api/search", s.handleSearch)
	mux.HandleFunc("GET /api/episodes/{id}/lines", s.handleEpisodeLines)
	mux.HandleFunc("GET /api/episodes/{id}/play", s.handleEpisodePlay)
	mux.HandleFunc("GET /api/episodes/{id}/qualities", s.handleEpisodeQualities)
	mux.HandleFunc("GET /api/covers/{id}", s.handleCover)
	mux.HandleFunc("GET /api/photos/{id}", s.handlePhoto)
	mux.HandleFunc("GET /api/stream/{id}", s.handleStream)

	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("POST /api/history", s.handleSaveHistory)
	mux.HandleFunc("DELETE /api/history", s.handleClearHistory)
	mux.HandleFunc("DELETE /api/history/{id}", s.handleDeleteHistory)
	mux.HandleFunc("GET /api/favorites", s.handleFavorites)
	mux.HandleFunc("GET /api/favorites/summary", s.handleFavoritesSummary)
	mux.HandleFunc("POST /api/favorites/toggle", s.handleToggleFavorite)

	mux.HandleFunc("GET /api/chats", s.handleChats)
	mux.HandleFunc("POST /api/chats/refresh", s.handleChatsRefresh)
	mux.HandleFunc("GET /api/chats/recommend", s.handleChatRecommend)
	mux.HandleFunc("POST /api/chats/toggle", s.handleToggleChat)
	mux.HandleFunc("POST /api/chats/sync", s.handleChatSync)
	mux.HandleFunc("POST /api/chats/join-recommend", s.handleChatsJoinRecommend)
	mux.HandleFunc("GET /api/download/status", s.handleDownloadStatus)
	mux.HandleFunc("GET /api/cache/usage", s.handleCacheUsage)
	mux.HandleFunc("POST /api/cache/clear", s.handleCacheClear)

	staticSub, _ := fs.Sub(staticFS, "static")

	serveRedirectHome := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusFound)
	}
	mux.HandleFunc("GET /classic", serveRedirectHome)
	mux.HandleFunc("GET /login", serveRedirectHome)

	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, staticSub, "index.html")
	}
	mux.HandleFunc("GET /{$}", serveIndex)
	fileServer := http.FileServer(http.FS(staticSub))
	mux.Handle("GET /", withStaticCache(fileServer))

	return withGzip(withCORS(mux))
}

type gzipResponseWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	started bool
	skip    bool
}

func (g *gzipResponseWriter) WriteHeader(code int) {
	if g.started {
		return
	}
	g.started = true

	if code == http.StatusNotModified || code == http.StatusNoContent {
		g.skip = true
		g.ResponseWriter.WriteHeader(code)
		return
	}
	g.Header().Set("Content-Encoding", "gzip")
	g.Header().Add("Vary", "Accept-Encoding")
	g.Header().Del("Content-Length")
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.started {
		g.WriteHeader(http.StatusOK)
	}
	if g.skip {
		return g.ResponseWriter.Write(b)
	}
	return g.gz.Write(b)
}

func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead || !gzipEligible(r.URL.Path) ||
			!strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}
		gz, err := gzip.NewWriterLevel(w, gzip.BestSpeed)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		gw := &gzipResponseWriter{ResponseWriter: w, gz: gz}
		next.ServeHTTP(gw, r)
		if !gw.started || gw.skip {
			return
		}
		_ = gz.Close()
	})
}

func withStaticCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		next.ServeHTTP(w, r)
	})
}

func gzipEligible(path string) bool {
	switch {
	case path == "/",
		strings.HasSuffix(path, ".js"),
		strings.HasSuffix(path, ".css"),
		strings.HasSuffix(path, ".html"),
		strings.HasSuffix(path, ".json"):
		return true
	}
	if !strings.HasPrefix(path, "/api/") {
		return false
	}
	switch {
	case strings.HasPrefix(path, "/api/stream/"),
		strings.HasPrefix(path, "/api/covers/"),
		strings.HasPrefix(path, "/api/photos/"),
		(strings.HasPrefix(path, "/api/episodes/") && strings.HasSuffix(path, "/play")),
		path == "/api/account/qr.png":
		return false
	}
	return true
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Range")
		w.Header().Set("Access-Control-Expose-Headers", "Content-Length, Content-Range, Accept-Ranges, Content-Type, ETag")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
