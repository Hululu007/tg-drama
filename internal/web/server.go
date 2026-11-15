// Package web 提供媒体库 HTTP API、流播放与前端静态资源服务
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

	"tgdrama/internal/rankings"
	"tgdrama/internal/storage"
	"tgdrama/internal/tgclient"
)

//go:embed static
var staticFS embed.FS

// Server Web 服务
type Server struct {
	store *storage.DB
	svc   *tgclient.Service

	// 在线播放（Telegram 流式代理）：单协程顺序按块读取，避免并发请求触发 TG 风控
	chunks *streamCache
	// 块布局：块 0 = headSize（128KB 快启头块），其后每块 blockSize（1MB）
	headSize   int64
	blockSize  int64
	fetchChunk sourceFetchFunc
	// chunkTimeout 单次 upload.getFile 的硬超时（防挂死占住全局读取信号量）
	chunkTimeout time.Duration
	// chunkOverallTimeout 单块读取（含内部重试/限流等待）的整体上限
	chunkOverallTimeout time.Duration
	// disk 视频块的磁盘缓存（边播边落盘，重启有效；nil=未启用）
	disk *diskCache
	// refreshingVariants 正在后台补查清晰度的来源（docID 去重）
	refreshingVariants sync.Map

	// cacheDir 磁盘图片缓存目录（没有真实封面时生成的占位图存在这里）
	cacheDir string

	// rank 外部榜单服务（nil 表示关闭排行榜）
	rank *rankings.Service
}

// NewServer 创建 Web 服务
func NewServer(store *storage.DB, svc *tgclient.Service) *Server {
	s := &Server{store: store, svc: svc}
	s.applyStreamOptions(streamDefaultChunk, streamDefaultCacheMB)
	s.fetchChunk = s.fetchChunkFromTelegram
	return s
}

// SetCacheDir 指定磁盘图片缓存目录（占位封面写在这里）
func (s *Server) SetCacheDir(dir string) { s.cacheDir = dir }

// SetRankings 挂上榜单服务（nil 表示关闭排行榜页签）
func (s *Server) SetRankings(r *rankings.Service) { s.rank = r }

// SetStreamOptions 覆盖在线播放参数（来自 config.yaml 的 stream 段）。
// 播放固定单线程顺序读取，不再有 parallel 参数：Telegram Web 端多线程读取无效且容易触发风控限流。
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
	// 块大小按 4KB 对齐，配合 Telegram 的 offset/limit 要求
	chunkSize -= chunkSize % 4096
	if cacheMB < 8 {
		cacheMB = 8
	}
	if cacheMB > 1024 {
		cacheMB = 1024
	}
	s.blockSize = chunkSize
	// 头块不超过主块大小（配置很小的 chunk_kb 时头块=主块）
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

// setDiskCache 启用/关闭视频块磁盘缓存（dir 取 cacheDir 同级的 mediacache）。
func (s *Server) setDiskCache(diskMB int) {
	if diskMB <= 0 || s.cacheDir == "" {
		s.disk = nil
		return
	}
	quota := int64(diskMB) << 20
	dir := filepath.Join(filepath.Dir(s.cacheDir), "mediacache")
	s.disk = newDiskCache(dir, quota)
}

// Handler 构造路由
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 鉴权
	mux.HandleFunc("GET /api/account/state", s.handleAuthStatus)
	mux.HandleFunc("POST /api/account/mode", s.handleAuthMode)
	mux.HandleFunc("GET /api/account/qr", s.handleQR)
	mux.HandleFunc("GET /api/account/qr.png", s.handleQRImage)
	mux.HandleFunc("POST /api/account/phone", s.handleSubmitPhone)
	mux.HandleFunc("POST /api/account/code", s.handleSubmitCode)
	mux.HandleFunc("POST /api/account/password", s.handleSubmitPassword)
	mux.HandleFunc("POST /api/account/logout", s.handleAuthLogout)

	// 媒体库
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

	// 观看历史与收藏
	mux.HandleFunc("GET /api/history", s.handleHistory)
	mux.HandleFunc("POST /api/history", s.handleSaveHistory)
	mux.HandleFunc("DELETE /api/history", s.handleClearHistory)
	mux.HandleFunc("DELETE /api/history/{id}", s.handleDeleteHistory)
	mux.HandleFunc("GET /api/favorites", s.handleFavorites)
	mux.HandleFunc("GET /api/favorites/summary", s.handleFavoritesSummary)
	mux.HandleFunc("POST /api/favorites/toggle", s.handleToggleFavorite)

	// 排行榜（外部公开榜单 + 本地片库匹配）
	mux.HandleFunc("GET /api/rankings", s.handleRankings)
	mux.HandleFunc("POST /api/rankings/refresh", s.handleRankingsRefresh)

	// 排行榜名称匹配规则（用户可编辑的 match-rules.yaml）
	mux.HandleFunc("GET /api/rankings/rules", s.handleRankingsRulesGet)
	mux.HandleFunc("PUT /api/rankings/rules", s.handleRankingsRulesPut)
	mux.HandleFunc("POST /api/rankings/rules/test", s.handleRankingsRulesTest)

	// 管理
	mux.HandleFunc("GET /api/chats", s.handleChats)
	mux.HandleFunc("POST /api/chats/refresh", s.handleChatsRefresh)
	mux.HandleFunc("GET /api/chats/recommend", s.handleChatRecommend)
	mux.HandleFunc("POST /api/chats/toggle", s.handleToggleChat)
	mux.HandleFunc("POST /api/chats/sync", s.handleChatSync)
	mux.HandleFunc("POST /api/chats/join-recommend", s.handleChatsJoinRecommend)
	mux.HandleFunc("GET /api/download/status", s.handleDownloadStatus)
	mux.HandleFunc("GET /api/cache/usage", s.handleCacheUsage)
	mux.HandleFunc("POST /api/cache/clear", s.handleCacheClear)

	// 前端静态资源
	staticSub, _ := fs.Sub(staticFS, "static")
	// 前端已合并为一套：登录、频道管理、设置都在首页（index.html）里。
	// 旧的 /login、/classic 后台入口重定向到首页，避免老收藏链接失效。
	serveRedirectHome := func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/", http.StatusFound)
	}
	mux.HandleFunc("GET /classic", serveRedirectHome)
	mux.HandleFunc("GET /login", serveRedirectHome)
	// 首页不缓存：升级插件后刷新立即生效，避免旧页面配新接口
	serveIndex := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		http.ServeFileFS(w, r, staticSub, "index.html")
	}
	mux.HandleFunc("GET /{$}", serveIndex)
	fileServer := http.FileServer(http.FS(staticSub))
	mux.Handle("GET /", withStaticCache(fileServer))

	// gzip 最外层：JSON 体积压缩后首屏传输可省 80%+（视频/封面等二进制不走 gzip）
	return withGzip(withCORS(mux))
}

// gzipResponseWriter 延迟到 WriteHeader 才声明 Content-Encoding，304/204 保持无体不压缩
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
	// 304/204 没有响应体，不能声明 Content-Encoding，也不能写 gzip 尾巴
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

// withStaticCache 给内嵌前端资源加短缓存：插件升级后 5 分钟内自然过期，
// 平时再次打开不必重复下载 47KB 的 html/js/css。
func withStaticCache(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=300")
		next.ServeHTTP(w, r)
	})
}

// gzipEligible 只有文本类响应才走 gzip：视频流、封面、照片、二维码 PNG 原样透传。
// /play 输出的是真实视频字节（含 206/Range），一旦被 gzip 包裹会破坏播放器拖动，必须排除。
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

// withCORS 允许宿主网页（file:// 或其他端口）直接调用本机 API。
// 插件前端同源访问不受影响；预检请求直接放行。
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
