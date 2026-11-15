package web

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"tgdrama/internal/storage"
	"tgdrama/internal/tgclient"
)

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// writeJSONETag 带 ETag 的 JSON 响应：浏览器二次打开同一页时携带 If-None-Match，
// 媒体库没变化就直接 304（零响应体），省掉整包 JSON 传输与解析。
func writeJSONETag(w http.ResponseWriter, r *http.Request, etag string, v any) {
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "private, max-age=0, must-revalidate")
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func quoteETag(s string) string {
	return "" + s + ""
}

func parsePathID(r *http.Request, key string) (int64, error) {
	return strconv.ParseInt(r.PathValue(key), 10, 64)
}

// ---- 鉴权 ----

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"authorized": s.svc.IsAuthorized(),
		"phase":      s.svc.AuthPhase(),
		"message":    s.svc.AuthMessage(),
		"mode":       s.svc.LoginMode(),
	})
}

// handleAuthMode 切换登录方式：PC 端默认扫码，移动端默认账号（手机号 + 验证码）。
// 前端按设备算好偏好后调这里，后端据此决定生成二维码还是等手机号。
func (s *Server) handleAuthMode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode string `json:"mode"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := s.svc.SetLoginMode(body.Mode); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"mode": s.svc.LoginMode()})
}

// handleSubmitPhone 账号登录第一步：提交手机号，Telegram 会发验证码
func (s *Server) handleSubmitPhone(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Phone string `json:"phone"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := s.svc.SubmitPhone(body.Phone); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleSubmitCode 账号登录第二步：提交验证码
func (s *Server) handleSubmitCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := s.svc.SubmitCode(body.Code); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleQR(w http.ResponseWriter, r *http.Request) {
	if s.svc.LoginMode() == tgclient.ModePhone {
		writeError(w, http.StatusNotFound, "当前是账号登录，请用手机号登录")
		return
	}
	loginURL, expires, ok := s.svc.QRInfo()
	if !ok {
		msg := "二维码生成中，正在连接 Telegram…"
		if m := s.svc.AuthMessage(); m != "" {
			msg = m
		}
		writeError(w, http.StatusNotFound, msg)
		return
	}
	// 二维码 token 只有 30 秒有效期：剩不到 8 秒就别再交给前端了，
	// 否则用户扫到的是一张必然报 AUTH_TOKEN_EXPIRED 的过期码。
	if !expires.After(time.Now().Add(8 * time.Second)) {
		writeError(w, http.StatusNotFound, "二维码即将过期，正在刷新…")
		return
	}
	// expires 是 unix 秒：前端据此在过期前主动换码，而不是按固定间隔轮询
	resp := map[string]any{"url": loginURL, "expires": expires.Unix()}
	// PNG 以 data URL 内联返回：部分广告拦截扩展按 qr.png 这类路径模式
	// 拦截二维码图片请求，内联数据不触发图片过滤。
	if pngData, err := s.svc.QrImage(r.Context()); err == nil && len(pngData) > 0 {
		resp["img"] = "data:image/png;base64," + base64.StdEncoding.EncodeToString(pngData)
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleQRImage(w http.ResponseWriter, r *http.Request) {
	pngData, err := s.svc.QrImage(r.Context())
	if err != nil {
		if errors.Is(err, tgclient.ErrQRNotReady) {
			// 用 503 而不是 404：<img> 出错后前端会按 Retry-After 继续重试
			w.Header().Set("Retry-After", "2")
			writeError(w, http.StatusServiceUnavailable, "二维码生成中，请稍候")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "no-cache")
	_, _ = w.Write(pngData)
}

func (s *Server) handleSubmitPassword(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if body.Password == "" {
		writeError(w, http.StatusBadRequest, "密码不能为空")
		return
	}
	if err := s.svc.SubmitPassword(body.Password); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleAuthLogout 退出登录：注销授权并清除本地会话，随后客户端以未登录状态重连
func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Logout(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- 媒体库 ----

func (s *Server) handleTags(w http.ResponseWriter, r *http.Request) {
	tags, err := s.store.ListTags()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if tags == nil {
		tags = []*storage.TagCount{}
	}
	writeJSONETag(w, r, quoteETag(s.store.VersionTag()+"-tags"), tags)
}

// handleBootstrap 首屏聚合接口：一份响应返回标签、首屏短剧、收藏数与「继续观看」，
// 把原来 3 个往返（/api/tags + /api/favorites + /api/shows + /api/history）合并成 1 个。
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	tags, err := s.store.ListTags()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	shows, err := s.store.ListShows("", "", 30, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	total, _ := s.store.CountShows()
	favTotal, _ := s.store.CountFavoriteShows()
	favUpdates, _ := s.store.CountFavoriteUpdates()
	cont, _ := s.store.ListHistory(10)
	if tags == nil {
		tags = []*storage.TagCount{}
	}
	if shows == nil {
		shows = []*storage.Show{}
	}
	if cont == nil {
		cont = []*storage.HistoryItem{}
	}
	writeJSONETag(w, r, quoteETag(s.store.VersionTag()+"-"+s.store.HistoryTag()+"-boot"), map[string]any{
		"tags":             tags,
		"shows":            shows,
		"total":            total,
		"favorite_total":   favTotal,
		"favorite_updates": favUpdates,
		"continue_list":    cont,
		"page":             1,
	})
}

func (s *Server) handleShows(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	if page < 1 {
		page = 1
	}
	const size = 30
	// sortBy："" 自然（最近更新/最近收藏在前），"episodes" 按可播集数倒序
	sortBy := ""
	if q.Get("sort") == "episodes" {
		sortBy = "episodes"
	}
	var (
		shows []*storage.Show
		total int64
		err   error
	)
	if q.Get("fav") == "1" {
		shows, err = s.store.ListFavoriteShows(sortBy, size, (page-1)*size)
		total, _ = s.store.CountFavoriteShows()
	} else {
		shows, err = s.store.ListShows(q.Get("tag"), sortBy, size, (page-1)*size)
		// 总数必须按当前分类统计，前端据此判断是否还有下一页
		total, _ = s.store.CountShowsByTag(q.Get("tag"))
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// 收藏列表带「新增集数」，它依赖观看历史，所以 ETag 要把历史指纹算进来，
	// 否则看完一集后徽标不会消失（列表一直被 304 命中）。
	etag := quoteETag(fmt.Sprintf("%s-shows-%s-%s-%d-%d", s.store.VersionTag(), sortBy, q.Get("tag"), boolInt(q.Get("fav") == "1"), page))
	if q.Get("fav") == "1" {
		etag = quoteETag(fmt.Sprintf("%s-%s-favShows-%s-%d", s.store.VersionTag(), s.store.HistoryTag(), sortBy, page))
	}
	body := map[string]any{
		"list":  shows,
		"total": total,
		"page":  page,
	}
	if q.Get("fav") == "1" {
		// 收藏页的更新角标需要同一份口径的合计，省掉一次额外请求
		updates, _ := s.store.CountFavoriteUpdates()
		body["updates_total"] = updates
	}
	writeJSONETag(w, r, etag, body)
}

func (s *Server) handleShowDetail(w http.ResponseWriter, r *http.Request) {
	id, err := parsePathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的短剧ID")
		return
	}
	show, err := s.store.GetShow(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "短剧不存在")
		return
	}
	episodes, err := s.store.ListEpisodes(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if episodes == nil {
		episodes = []*storage.Episode{}
	}
	photos, err := s.store.ListShowPhotos(id)
	if err != nil {
		photos = []*storage.ShowPhoto{}
	}
	if photos == nil {
		photos = []*storage.ShowPhoto{}
	}
	// 打开详情页时整页预热：缺图的按频道合并成一次批量取消息，再并发下载。
	// 预览图不再在采集阶段全量下载（那样会白占几百 MB），改成这里按需拉。
	if s.svc != nil && len(photos) > 0 {
		s.svc.RequeueShowPhotos(id)
	}
	// 观看进度（按最近观看倒序），第一条作为「继续播放」
	progress, err := s.store.ListShowProgress(id)
	if err != nil {
		progress = nil
	}
	if progress == nil {
		progress = []*storage.WatchProgress{}
	}
	var resume *storage.WatchProgress
	if len(progress) > 0 {
		resume = progress[0]
	}
	favorited, _ := s.store.IsFavorite(id)
	writeJSON(w, http.StatusOK, map[string]any{
		"show":      show,
		"episodes":  episodes,
		"photos":    photos,
		"favorited": favorited,
		"progress":  progress,
		"continue":  resume,
	})
}

// handleFillEpisodes 详情页发现选集不连贯时，用「#剧名」在全省范围搜索补全缺失的集
func (s *Server) handleFillEpisodes(w http.ResponseWriter, r *http.Request) {
	id, err := parsePathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的短剧ID")
		return
	}
	show, err := s.store.GetShow(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "短剧不存在")
		return
	}
	episodes, err := s.store.ListEpisodes(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// force：用户手动点「搜索补全」，即使 1..最大集号之间没有空洞也要去搜
	// （连载中的剧经常是「只同步到第 2 集」，没有洞，自动补全永远不会触发）。
	var body struct {
		Force bool `json:"force"`
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	force := body.Force || r.URL.Query().Get("force") == "1"
	missing := missingEpisodes(episodes)
	if len(missing) == 0 && !force {
		writeJSON(w, http.StatusOK, map[string]any{"added": 0, "missing": []int{}, "searched": false})
		return
	}
	timeout := 25 * time.Second
	if force {
		timeout = 60 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	added, ferr := s.svc.FillMissingEpisodes(ctx, id, show.Name, force)
	switch {
	case errors.Is(ferr, tgclient.ErrNotAuthorized):
		writeJSON(w, http.StatusOK, map[string]any{"added": 0, "missing": missing, "searched": false, "error": ferr.Error()})
		return
	case errors.Is(ferr, tgclient.ErrFillTooSoon):
		writeJSON(w, http.StatusOK, map[string]any{"added": 0, "missing": missing, "searched": false, "error": ferr.Error()})
		return
	case ferr != nil:
		writeJSON(w, http.StatusBadGateway, map[string]any{"added": added, "missing": missing, "searched": true, "error": ferr.Error()})
		return
	}
	// 补到东西就重新算一遍缺口，前端提示里的「缺 X-Y 集」才是补全之后的真实状态
	if added > 0 {
		if eps, eerr := s.store.ListEpisodes(id); eerr == nil {
			missing = missingEpisodes(eps)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "missing": missing, "searched": true})
}

// missingEpisodes 找出 1..最大集号 之间没有可播来源的集号，最多检查到 300 集
func missingEpisodes(episodes []*storage.Episode) []int {
	have := make(map[int]bool, len(episodes))
	max := 0
	for _, e := range episodes {
		if e == nil || e.EpisodeNo <= 0 {
			continue
		}
		have[e.EpisodeNo] = true
		if e.EpisodeNo > max {
			max = e.EpisodeNo
		}
	}
	if max <= 0 || max > 300 {
		return nil
	}
	out := make([]int, 0, 8)
	for i := 1; i <= max; i++ {
		if !have[i] {
			out = append(out, i)
		}
	}
	return out
}

// handlePhoto 提供剧集预览图
func (s *Server) handlePhoto(w http.ResponseWriter, r *http.Request) {
	id, err := parsePathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的图片ID")
		return
	}
	path, err := s.store.GetShowPhotoPath(id)
	if err != nil {
		// 记录不存在：这个 ID 本来就没有图
		writeError(w, http.StatusNotFound, "图片不存在")
		return
	}
	// 记录还在但本地文件被清理／丢失：先发占位图，同时让后台按需重新下载，
	// 清缓存之后图片会自己长回来，不会永久变成灰块。
	// 占位优先用这部剧的封面（真实画面），没有封面才用剧名首字的 SVG。
	//
	// X-Photo-Ready 告诉前端这一张是不是真图：0=封面/SVG 占位（还没下载好），
	// 1=真图。前端据此在超时后把一直出不来真图的预览图撤掉。
	if path == "" || !fileExists(path) {
		w.Header().Set("X-Photo-Ready", "0")
		if s.svc != nil {
			s.svc.RequeuePhoto(id)
		}
		if s.servePhotoFallback(w, r, id) {
			return
		}
	} else {
		w.Header().Set("X-Photo-Ready", "1")
	}
	// 有记录但缓存还没落到本地（或文件被清掉）时发占位图，前端不会出现破图
	s.servePhoto(w, r, path, id)
}

func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	keyword := r.URL.Query().Get("q")
	if keyword == "" {
		writeJSON(w, http.StatusOK, map[string]any{"list": []*storage.Show{}})
		return
	}
	shows, err := s.store.SearchShows(keyword, 30)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"list": shows})
}

func (s *Server) handleEpisodeLines(w http.ResponseWriter, r *http.Request) {
	id, err := parsePathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的剧集ID")
		return
	}
	lines, err := s.store.ListEpisodeLines(id)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if lines == nil {
		lines = []*storage.PlayLine{}
	}
	// 播放器线路面板要展示每个频道的可播放版本（时长/分辨率/大小），
	// 以及默认会播哪一条（active），不必让前端重复实现选源规则。
	type lineView struct {
		ChatID   int64  `json:"chat_id"`
		ChatName string `json:"chat_name"`
		// Segments 保留完整来源列表：经典界面仍在按线路/分段切换播放
		Segments     []storage.Source `json:"segments"`
		SegmentCount int              `json:"segment_count"`
		SourceID     int64            `json:"source_id"`
		Duration     float64          `json:"duration"`
		FileSize     int64            `json:"file_size"`
		Width        int              `json:"width"`
		Height       int              `json:"height"`
		Active       bool             `json:"active"`
	}
	var all []*storage.Source
	for _, line := range lines {
		for i := range line.Segments {
			all = append(all, &line.Segments[i])
		}
	}
	defaultSrc := pickFromSources(all)
	views := make([]lineView, 0, len(lines))
	for _, line := range lines {
		ptrs := make([]*storage.Source, 0, len(line.Segments))
		for i := range line.Segments {
			ptrs = append(ptrs, &line.Segments[i])
		}
		best := pickFromSources(ptrs)
		// 默认来源属于这条线路时，面板展示默认来源的信息（否则会显示同线路里
		// 那个更长的合集包时长，与默认实际播放的文件不符）。
		isDefault := false
		if defaultSrc != nil {
			for i := range line.Segments {
				if line.Segments[i].ID == defaultSrc.ID {
					best = &line.Segments[i]
					isDefault = true
					break
				}
			}
		}
		segs := line.Segments
		if segs == nil {
			segs = []storage.Source{}
		}
		v := lineView{
			ChatID: line.ChatID, ChatName: line.ChatName,
			Segments: segs, SegmentCount: len(segs),
		}
		if best != nil {
			v.SourceID = best.ID
			v.Duration = best.Duration
			v.FileSize = best.FileSize
			v.Width = best.Width
			v.Height = best.Height
		}
		// active 表示「默认会播这条线路」：以全局选出的默认来源所在线路为准。
		// 逐线路各挑各的会把只有合集包的线路也标成 active（默认却播别条），
		// 面板上就会出现两条「当前」。
		v.Active = isDefault
		views = append(views, v)
	}
	// 播放页需要剧集本身（集数）和所属短剧（标题、选集列表）
	episode, err := s.store.GetEpisode(id)
	if err != nil {
		writeError(w, http.StatusNotFound, "剧集不存在")
		return
	}
	progress, _ := s.store.GetEpisodeProgress(id)
	resp := map[string]any{
		"lines":    views,
		"episode":  episode,
		"progress": progress,
	}
	if show, err := s.store.GetShow(episode.ShowID); err == nil {
		resp["show"] = show
	}
	// 选集列表随 lines 一次返回，播放页不必再请求 /api/shows/{id}
	if eps, err := s.store.ListEpisodes(episode.ShowID); err == nil {
		if eps == nil {
			eps = []*storage.Episode{}
		}
		resp["episodes"] = eps
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleEpisodePlay 给宿主播放器用的稳定播放地址：自动选一路来源后直接输出视频流。
// 可选 ?chat=<频道ID> 指定线路，播放器切线路时用它固定到某个频道。
// 可选 ?vq=<变体ID> 指定清晰度（Telegram alt_documents），不传则用当前线路的默认版本。
// 不走 302：部分宿主播放器不跟随跳转、或跳转后丢掉 Range/请求头，直接出流更稳。
func (s *Server) handleEpisodePlay(w http.ResponseWriter, r *http.Request) {
	id, err := parsePathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的剧集ID")
		return
	}
	if _, err := s.store.GetEpisode(id); err != nil {
		writeError(w, http.StatusNotFound, "剧集不存在")
		return
	}
	var chatID int64
	if v := strings.TrimSpace(r.URL.Query().Get("chat")); v != "" {
		chatID, err = strconv.ParseInt(v, 10, 64)
		if err != nil || chatID <= 0 {
			writeError(w, http.StatusBadRequest, "无效的线路")
			return
		}
	}
	var variantID int64
	if v := strings.TrimSpace(r.URL.Query().Get("vq")); v != "" {
		variantID, err = strconv.ParseInt(v, 10, 64)
		if err != nil || variantID <= 0 {
			writeError(w, http.StatusBadRequest, "无效的清晰度")
			return
		}
	}
	picked, err := s.pickEpisodeSource(id, chatID)
	if err != nil {
		writeError(w, http.StatusNotFound, "该线路暂无可播放的视频")
		return
	}
	if variantID > 0 {
		fixed, verr := s.sourceFromVariant(id, chatID, variantID)
		if verr != nil {
			writeError(w, http.StatusNotFound, verr.Error())
			return
		}
		picked = fixed
	}
	s.serveSource(w, r, picked)
}

// sourceFromVariant 把清晰度变体转成可流式播放的伪来源：
// 用变体自己的 doc_id/access_hash/file_reference/size，来源 ID 取负值以区分缓存与引用刷新。
func (s *Server) sourceFromVariant(episodeID, chatID, variantID int64) (*storage.Source, error) {
	v, err := s.store.GetSourceVariant(variantID)
	if err != nil {
		return nil, errors.New("该清晰度不存在")
	}
	src, err := s.store.GetSource(v.SourceID)
	if err != nil || src.EpisodeID != episodeID {
		return nil, errors.New("该清晰度不属于这一集")
	}
	if chatID > 0 && src.ChatID != chatID {
		return nil, errors.New("该清晰度不属于所选线路")
	}
	out := *src
	out.ID = -v.ID
	out.FileSize = v.FileSize
	out.Duration = v.Duration
	out.Width, out.Height = v.Width, v.Height
	out.Mime = v.Mime
	out.DocID, out.AccessHash, out.FileReference = v.DocID, v.AccessHash, v.FileReference
	return &out, nil
}

// qualityView 播放页清晰度菜单的一项。variant_id=0 表示主版本（Auto 的基准）。
type qualityView struct {
	VariantID int64  `json:"variant_id"`
	Height    int    `json:"height"`
	Width     int    `json:"width"`
	FileSize  int64  `json:"file_size"`
	Label     string `json:"label"`
	IsMain    bool   `json:"is_main"`
	// Codec 只用于同高度去重时的取舍，不下发前端
	Codec string `json:"-"`
}

// handleEpisodeQualities 返回这一集当前线路可选的清晰度。
// Telegram 的 alt_documents 是唯一来源：没有副版本时列表只有主版本，前端据此隐藏入口。
// 历史库入库时没存过副文档，这里按来源懒补一次（每来源只查一次，失败有冷却）。
func (s *Server) handleEpisodeQualities(w http.ResponseWriter, r *http.Request) {
	id, err := parsePathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的剧集ID")
		return
	}
	if _, err := s.store.GetEpisode(id); err != nil {
		writeError(w, http.StatusNotFound, "剧集不存在")
		return
	}
	var chatID int64
	if v := strings.TrimSpace(r.URL.Query().Get("chat")); v != "" {
		chatID, err = strconv.ParseInt(v, 10, 64)
		if err != nil || chatID <= 0 {
			writeError(w, http.StatusBadRequest, "无效的线路")
			return
		}
	}
	picked, err := s.pickEpisodeSource(id, chatID)
	if err != nil {
		writeError(w, http.StatusNotFound, "该线路暂无可播放的视频")
		return
	}
	if !picked.VariantsChecked && s.svc != nil && s.svc.IsAuthorized() && !tgclient.VariantsCoolingDown(picked.ID) {
		// 变体补查放到后台：接口立即返回当前已知档位，前端几秒后二次拉取。
		// 同步补查最长 8 秒会卡住整个清晰度接口——默认档位迟迟不生效，
		// 表现为「一直缓冲中，手动换清晰度才好」（2026-11-14 用户反馈）。
		if _, busy := s.refreshingVariants.LoadOrStore(picked.ID, true); !busy {
			go func() {
				defer s.refreshingVariants.Delete(picked.ID)
				bctx, bcancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer bcancel()
				if _, verr := s.svc.RefreshSourceVariants(bctx, picked); verr != nil {
					log.Printf("清晰度补查失败 source=%d doc=%d: %v", picked.ID, picked.DocID, verr)
				}
			}()
		}
	}
	variants, err := s.store.ListSourceVariants(picked.ID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	list := make([]qualityView, 0, len(variants)+1)
	if picked.Height > 0 || picked.Width > 0 {
		list = append(list, qualityView{
			// 主版本用 -1：0 是「自动」的保留值，两者混用会让菜单同时标出两个「当前」。
			VariantID: -1, Height: picked.Height, Width: picked.Width,
			FileSize: picked.FileSize, Label: qualityLabel(picked.Width, picked.Height), IsMain: true,
		})
	}
	for _, v := range variants {
		list = append(list, qualityView{
			VariantID: v.ID, Height: v.Height, Width: v.Width,
			FileSize: v.FileSize, Label: qualityLabel(v.Width, v.Height), Codec: v.Codec,
		})
	}
	list = oneQualityPerHeight(list)
	writeJSON(w, http.StatusOK, map[string]any{
		"list":  list,
		"total": len(list),
	})
}

// oneQualityPerHeight 同一清晰度只保留一档，和 Telegram 的菜单一致：
// 同一条消息常同时带 H.264 与 HEVC 两个同分辨率版本。
// 取舍顺序：主版本（能正常播的原始文件）> H.264（浏览器兼容）> 体积更大的（画质更高）。
func oneQualityPerHeight(list []qualityView) []qualityView {
	best := map[int]qualityView{}
	order := make([]int, 0, len(list))
	for _, q := range list {
		h := shortSide(q.Width, q.Height)
		key := h
		if key == 0 {
			// 分辨率未知的各自独立，避免被合并掉
			key = -int(q.VariantID) - 1
		}
		cur, ok := best[key]
		if !ok {
			best[key] = q
			order = append(order, key)
			continue
		}
		if preferQuality(q, cur) {
			best[key] = q
		}
	}
	out := make([]qualityView, 0, len(order))
	for _, k := range order {
		out = append(out, best[k])
	}
	sort.SliceStable(out, func(i, j int) bool {
		hi, hj := shortSide(out[i].Width, out[i].Height), shortSide(out[j].Width, out[j].Height)
		if hi != hj {
			return hi > hj
		}
		return out[i].IsMain && !out[j].IsMain
	})
	return out
}

func preferQuality(a, b qualityView) bool {
	if a.IsMain != b.IsMain {
		return a.IsMain
	}
	pa, pb := playableCodec(a.Codec), playableCodec(b.Codec)
	if pa != pb {
		return pa
	}
	return a.FileSize > b.FileSize
}

// playableCodec 浏览器能直接解码的编码；空编码按可播处理（老数据/原始上传）。
func playableCodec(c string) bool {
	c = strings.ToLower(strings.TrimSpace(c))
	return c == "" || c == "h264" || c == "avc1"
}

// qualityLabel 按短边生成清晰度标签（竖屏 1080×1920 是 1080P），与播放器分辨率文案一致。
func qualityLabel(w, h int) string {
	s := shortSide(w, h)
	if s <= 0 {
		return ""
	}
	return strconv.Itoa(s) + "P"
}

func shortSide(w, h int) int {
	if w <= 0 || h <= 0 {
		return 0
	}
	if w < h {
		return w
	}
	return h
}

// pickFromSources 从一批来源里挑「正片」：
//   - 先排除标题声明了跨集范围的来源（`第1-2集` 这类多集合集文件）：
//     它包含的不是这一集的内容，只在没有单集来源时才作为兜底；
//   - 再取时长最长的那一组：频道常把预告片/试看和正片发成同一集，短片段不能当正片；
//   - 同长度时取体积最小的那个：码率越低网页拉流越快，卡顿越少；
//   - 时长信息缺失（老数据没有 DocumentAttributeVideo）时视为同一组，同样取最小。
func pickFromSources(srcs []*storage.Source) *storage.Source {
	list := make([]*storage.Source, 0, len(srcs))
	for _, s := range srcs {
		if s != nil && s.FileSize > 0 {
			list = append(list, s)
		}
	}
	if len(list) == 0 {
		return nil
	}
	// 多集合集文件（标题写 `第1-2集`）单独放一边：默认线路必须是单集文件。
	if single := filterMultiEpisodeBundles(list); len(single) > 0 {
		list = single
	}
	maxDur := list[0].Duration
	for _, s := range list[1:] {
		if s.Duration > maxDur {
			maxDur = s.Duration
		}
	}
	cand := list[:0]
	for _, s := range list {
		if s.Duration == maxDur {
			cand = append(cand, s)
		}
	}
	sort.SliceStable(cand, func(i, j int) bool {
		if cand[i].FileSize != cand[j].FileSize {
			return cand[i].FileSize < cand[j].FileSize
		}
		return cand[i].ID < cand[j].ID
	})
	return cand[0]
}

// reMultiEpisodeTitle 标题里声明了跨集范围：第1-2集 / 第一至二集 / 1-2合集。
// 这类文件是一个合集包，不属于任何单独一集。
var reMultiEpisodeTitle = regexp.MustCompile(`(?i)(?:第\s*(?:[0-9]{1,4}|[零一二三四五六七八九十百千两]{1,4})\s*[-~－～—–至到]\s*(?:[0-9]{1,4}|[零一二三四五六七八九十百千两]{1,4})\s*(?:[集话話回]|合集|合辑|合輯)|EP\s*-?\s*[0-9]{1,3}\s*[-~－～—–]\s*(?:EP\s*-?\s*)?[0-9]{1,3})`)

// sourceSpansMultipleEpisodes 该来源是否是多集合集文件。
// 只看标题（文件名或文案）：分集文件常见「剧名 第1集」「剧名1」，不含范围。
func sourceSpansMultipleEpisodes(s *storage.Source) bool {
	if s == nil {
		return false
	}
	return reMultiEpisodeTitle.MatchString(s.FileName) || reMultiEpisodeTitle.MatchString(s.Caption)
}

// filterMultiEpisodeBundles 去掉「多集合集包」来源；没有可替代的单集来源时返回空，
// 由调用方回退到原列表。
//
// 只有「标题声明跨集范围」还不够：同一相册的每条视频常共享
// 「第1-3集 连载中 全系列完整版」这种整剧文案，文件本身仍是一集。
// 因此再加时长判据——合集包必须明显更长（≥1.5 倍）于同集其他来源，
// 例如真实库合并进来的 720s「第1-2集」对比三条 360s 单集。
func filterMultiEpisodeBundles(list []*storage.Source) []*storage.Source {
	if len(list) < 2 {
		return nil
	}
	// 同集的「单集典型时长」：用未声明跨集范围的来源，没有则用全部来源的中位数。
	var normal []float64
	for _, s := range list {
		if !sourceSpansMultipleEpisodes(s) && s.Duration > 0 {
			normal = append(normal, s.Duration)
		}
	}
	base := medianDuration(list)
	if len(normal) > 0 {
		base = medianOf(normal)
	}
	if base <= 0 {
		return nil
	}
	out := make([]*storage.Source, 0, len(list))
	for _, s := range list {
		if sourceSpansMultipleEpisodes(s) && s.Duration >= 1.5*base {
			continue
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func medianDuration(list []*storage.Source) float64 {
	ds := make([]float64, 0, len(list))
	for _, s := range list {
		if s.Duration > 0 {
			ds = append(ds, s.Duration)
		}
	}
	return medianOf(ds)
}

func medianOf(ds []float64) float64 {
	if len(ds) == 0 {
		return 0
	}
	sort.Float64s(ds)
	// 取偏小的一侧：偶数个来源时偏低值，避免一条超长合集把基准抬高、
	// 反而让合集自己通过判据。
	return ds[(len(ds)-1)/2]
}

// pickEpisodeSource 挑一个可播放来源：同一集可能有多段 / 多频道。
// 指定 chatID 时只在该频道的来源里挑（播放器切线路）。返回的指针来自 lines 切片副本，
// 调用方只读使用。
func (s *Server) pickEpisodeSource(episodeID, chatID int64) (*storage.Source, error) {
	lines, err := s.store.ListEpisodeLines(episodeID)
	if err != nil {
		return nil, err
	}
	var all []*storage.Source
	for _, line := range lines {
		if chatID > 0 && line.ChatID != chatID {
			continue
		}
		for i := range line.Segments {
			all = append(all, &line.Segments[i])
		}
	}
	picked := pickFromSources(all)
	if picked == nil {
		return nil, errors.New("该集暂无可播放的视频")
	}
	return picked, nil
}

func (s *Server) handleCover(w http.ResponseWriter, r *http.Request) {
	id, err := parsePathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的短剧ID")
		return
	}
	// 没有封面（或封面文件已丢）时发占位图并写进磁盘缓存，
	// 不返回 404：列表里每张卡都要有图。
	show, err := s.store.GetShow(id)
	if err != nil {
		show = nil
	}
	s.serveCover(w, r, show, id)
}

// ---- 观看历史 / 收藏 ----

// handleHistory 观看历史：按剧聚合，每部剧只留最近一集
func (s *Server) handleHistory(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	list, err := s.store.ListHistory(limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []*storage.HistoryItem{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"list": list})
}

// handleSaveHistory 播放器定时上报观看进度
func (s *Server) handleSaveHistory(w http.ResponseWriter, r *http.Request) {
	var body struct {
		EpisodeID int64   `json:"episode_id"`
		Position  float64 `json:"position"`
		Duration  float64 `json:"duration"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.EpisodeID <= 0 {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if _, err := s.store.GetEpisode(body.EpisodeID); err != nil {
		writeError(w, http.StatusNotFound, "剧集不存在")
		return
	}
	if err := s.store.SaveHistory(body.EpisodeID, body.Position, body.Duration); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleDeleteHistory 删除单集观看记录
func (s *Server) handleDeleteHistory(w http.ResponseWriter, r *http.Request) {
	id, err := parsePathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的剧集ID")
		return
	}
	if err := s.store.DeleteHistory(id); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleClearHistory 清空全部观看记录
func (s *Server) handleClearHistory(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ClearHistory(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleFavorites 收藏列表（收藏页用 /api/shows?fav=1 分页，此接口返回全量用于计数）
func (s *Server) handleFavorites(w http.ResponseWriter, r *http.Request) {
	list, err := s.store.ListFavoriteShows("", 500, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if list == nil {
		list = []*storage.Show{}
	}
	total, _ := s.store.CountFavoriteShows()
	updates, _ := s.store.CountFavoriteUpdates()
	writeJSON(w, http.StatusOK, map[string]any{
		"list": list, "total": total, "updates_total": updates,
	})
}

// handleFavoritesSummary 只要收藏数与更新角标数字，用于首页/播放页刷新徽标。
// 比拉整个收藏列表轻得多（不查剧集、标签）。
func (s *Server) handleFavoritesSummary(w http.ResponseWriter, r *http.Request) {
	total, err := s.store.CountFavoriteShows()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	updates, _ := s.store.CountFavoriteUpdates()
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "updates_total": updates})
}

// handleToggleFavorite 切换收藏状态
func (s *Server) handleToggleFavorite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ShowID int64 `json:"show_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.ShowID <= 0 {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	favorited, err := s.store.ToggleFavorite(body.ShowID)
	if errors.Is(err, storage.ErrShowNotFound) {
		writeError(w, http.StatusNotFound, err.Error())
		return
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	updates, _ := s.store.CountFavoriteUpdates()
	writeJSON(w, http.StatusOK, map[string]any{"favorited": favorited, "updates_total": updates})
}

// ---- 管理 ----

// handleChats 频道管理面板的数据：频道列表 + 每个频道的消息/来源数量 + 是否正在同步
func (s *Server) handleChats(w http.ResponseWriter, r *http.Request) {
	s.writeChats(w, false, "")
}

// handleChatsRefresh 打开频道管理时先向 Telegram 拉一次对话列表再返回：
// 新加入的频道、改过名的频道立刻出现在面板里，省得用户以为「只有这些」。
// 没登录或拉取失败时退回本地列表，面板照常可用。
func (s *Server) handleChatsRefresh(w http.ResponseWriter, r *http.Request) {
	refreshed := false
	var refreshErr string
	if s.svc != nil && s.svc.IsAuthorized() {
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		if err := s.svc.RefreshChats(ctx); err == nil {
			refreshed = true
		} else {
			// 失败也把原因带回去：前端要能区分「更新成功」和「显示的是本地旧列表」，
			// 否则用户会以为频道真的只有这些。
			refreshErr = err.Error()
		}
		cancel()
	}
	s.writeChats(w, refreshed, refreshErr)
}

// writeChats 输出频道面板数据；refreshed 表示刚刚成功向 Telegram 拉过对话列表，
// refreshErr 非空时表示这次刷新失败、返回的是本地列表。
func (s *Server) writeChats(w http.ResponseWriter, refreshed bool, refreshErr string) {
	list, err := s.store.ListChatStats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	enabled, syncing := 0, 0
	for i := range list {
		if list[i].Enabled {
			enabled++
		}
		if s.svc != nil && s.svc.ChatSyncing(list[i].TgChatID) {
			syncing++
			list[i].Syncing = true
		}
	}
	if list == nil {
		list = []storage.ChatStat{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"list": list, "total": len(list), "enabled": enabled, "syncing": syncing, "refreshed": refreshed,
		"refresh_error": refreshErr,
	})
}

// ChatRecommend 推荐面板里的一条频道：带可加入的 Telegram 链接
type ChatRecommend struct {
	Name     string `json:"name"`
	Username string `json:"username"`
	TgChatID int64  `json:"tg_chat_id"`
	URL      string `json:"url"`
	// Joined 当前账号是否还在这个频道里：已加入的行前端显示「已加入」且不可点，
	// 避免重复点开 Telegram 加入。
	Joined bool `json:"joined"`
}

// defaultRecommendChannels 推荐面板的固定清单：取「当前已启用并加入的短剧频道」
// 提前固化下来，所有账号看到的都一样——新账号登录时即使没有任何短剧频道，
// 也能从推荐里一键加入，不会出现「没有频道可用」。
// 没有公开用户名的私有频道拿不到对外链接，别人点了也加不进去，不列入推荐。
var defaultRecommendChannels = []ChatRecommend{
	{Name: "小黄鸭AI短剧｜成人短剧｜黄果短剧｜免费短剧｜黄豆短剧", Username: "aiduanju_huang_666_888", TgChatID: 4425471457},
	{Name: "AI成人短剧 免费看 【黄果短剧】", Username: "ASC2256", TgChatID: 3894893955},
	{Name: "Ai成人短剧 免费看", Username: "Air1855555", TgChatID: 4341680939},
}

// handleChatRecommend 返回推荐频道：固定清单全量展示，与当前账号是否已加入无关。
// 「已加入」只是角标：本地有这个频道时再问一次 Telegram 拿实时状态，
// 拿不到状态就按已加入显示（能采集说明在对话列表里）。
func (s *Server) handleChatRecommend(w http.ResponseWriter, r *http.Request) {
	local, err := s.store.ListChats()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	byID := make(map[int64]*storage.Chat, len(local))
	for _, c := range local {
		byID[c.TgChatID] = c
	}
	match := make([]*storage.Chat, 0, len(defaultRecommendChannels))
	for _, d := range defaultRecommendChannels {
		if c := byID[d.TgChatID]; c != nil {
			match = append(match, c)
		}
	}
	var joined map[int64]bool
	if s.svc != nil && len(match) > 0 {
		joined = s.svc.JoinedChannels(r.Context(), match)
	}
	list := make([]ChatRecommend, 0, len(defaultRecommendChannels))
	for _, d := range defaultRecommendChannels {
		u := strings.TrimSpace(strings.TrimPrefix(d.Username, "@"))
		in, known := joined[d.TgChatID]
		d.Username = u
		if u != "" {
			d.URL = "https://t.me/" + u
		}
		d.Joined = known && in
		if !known && byID[d.TgChatID] != nil {
			d.Joined = true
		}
		list = append(list, d)
	}
	writeJSON(w, http.StatusOK, map[string]any{"list": list, "total": len(list)})
}

// handleChatsJoinRecommend 一键加入全部推荐频道：
// 已是成员的跳过，加入成功/已存在的频道入库并强制启用，随后立刻回填历史。
// 首次使用引导调用；失败逐个返回，不互相影响。
func (s *Server) handleChatsJoinRecommend(w http.ResponseWriter, r *http.Request) {
	if s.svc == nil || !s.svc.IsAuthorized() {
		writeError(w, http.StatusServiceUnavailable, "未登录 Telegram，无法加入频道")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	usernames := make([]string, 0, len(defaultRecommendChannels))
	for _, c := range defaultRecommendChannels {
		usernames = append(usernames, c.Username)
	}
	res := s.svc.JoinChannels(ctx, usernames)
	writeJSON(w, http.StatusOK, res)
}

// handleToggleChat 启用/停用频道；启用后立刻在后台补拉该频道的消息
func (s *Server) handleToggleChat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TgChatID int64 `json:"tg_chat_id"`
		Enabled  bool  `json:"enabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if err := s.store.SetChatEnabled(body.TgChatID, body.Enabled); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	started := false
	if body.Enabled && s.svc != nil {
		started = s.svc.KickChatSync(body.TgChatID)
	}
	// 停用频道时把已入库的数据一并删掉：停用的频道不再采集，留着只会占空间
	// （用户明确要求「关闭频道时同步删除数据库数据」）。频道行保留，方便重新启用。
	var purged *storage.PurgeStats
	if !body.Enabled {
		st, err := s.store.PurgeChatData(body.TgChatID)
		if err != nil {
			log.Printf("停用频道后清理数据失败: %v", err)
		} else {
			purged = &st
			s.store.Vacuum()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "syncing": started, "enabled": body.Enabled, "purged": purged})
}

// handleChatSync 手动让某个频道立即在后台同步（不改变启用状态）
func (s *Server) handleChatSync(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TgChatID int64 `json:"tg_chat_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.TgChatID == 0 {
		writeError(w, http.StatusBadRequest, "请求格式错误")
		return
	}
	if s.svc == nil || !s.svc.IsAuthorized() {
		writeError(w, http.StatusServiceUnavailable, "未登录 Telegram，无法同步")
		return
	}
	if !s.svc.KickChatSync(body.TgChatID) {
		writeJSON(w, http.StatusOK, map[string]any{"started": false, "message": "该频道正在同步中"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"started": true})
}

func (s *Server) handleDownloadStatus(w http.ResponseWriter, r *http.Request) {
	if s.svc == nil {
		writeJSON(w, http.StatusOK, map[string]any{"current": "", "done": 0, "failed": 0, "pending": 0, "total": 0})
		return
	}
	writeJSON(w, http.StatusOK, s.svc.Queue().Status())
}

// handleCacheClear 清缓存。
// kind=photos 只清预览图（索引、封面、观看记录都不动，图片会在访问时按需重下）；
// kind=video 清视频块磁盘缓存（边播边落盘的那份，清掉后重播需重新从 Telegram 拉流）；
// 不带 kind 时保留旧行为：清空历史遗留的本地视频缓存。
func (s *Server) handleCacheClear(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("kind") == "video" {
		if s.disk == nil {
			writeError(w, http.StatusBadRequest, "视频磁盘缓存未启用")
			return
		}
		before, files := s.disk.usageStats()
		s.disk.clear()
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "freed_bytes": before, "freed_files": files,
		})
		return
	}
	if r.URL.Query().Get("kind") == "photos" {
		if s.svc == nil {
			writeError(w, http.StatusServiceUnavailable, "Telegram 客户端未就绪")
			return
		}
		freed, files, err := s.svc.ClearPhotoCache()
		if err != nil {
			writeError(w, http.StatusInternalServerError, err.Error())
			return
		}
		// 转码出来的 WebP 也一起清掉，否则磁盘上还留着一份
		freed += s.ClearWebPCache("photos")
		writeJSON(w, http.StatusOK, map[string]any{
			"ok": true, "freed_bytes": freed, "freed_files": files,
		})
		return
	}
	if err := s.svc.Queue().ClearMediaCache(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// handleCacheUsage 缓存占用统计（前端按钮上显示「预览图缓存 xx MB」）
func (s *Server) handleCacheUsage(w http.ResponseWriter, r *http.Request) {
	var size int64
	var files int
	if s.svc != nil {
		size, files = s.svc.PhotoCacheUsage()
	}
	var vBytes int64
	var vFiles int
	if s.disk != nil {
		vBytes, vFiles = s.disk.usageStats()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"photos_bytes": size, "photos_files": files,
		"video_bytes": vBytes, "video_files": vFiles,
	})
}
