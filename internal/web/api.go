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

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"authorized": s.svc.IsAuthorized(),
		"phase":      s.svc.AuthPhase(),
		"message":    s.svc.AuthMessage(),
		"mode":       s.svc.LoginMode(),
	})
}

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

	if !expires.After(time.Now().Add(8 * time.Second)) {
		writeError(w, http.StatusNotFound, "二维码即将过期，正在刷新…")
		return
	}

	resp := map[string]any{"url": loginURL, "expires": expires.Unix()}

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

func (s *Server) handleAuthLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.svc.Logout(r.Context()); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

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

		total, _ = s.store.CountShowsByTag(q.Get("tag"))
	}
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

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

	if s.svc != nil && len(photos) > 0 {
		s.svc.RequeueShowPhotos(id)
	}

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

	if added > 0 {
		if eps, eerr := s.store.ListEpisodes(id); eerr == nil {
			missing = missingEpisodes(eps)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"added": added, "missing": missing, "searched": true})
}

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

func (s *Server) handlePhoto(w http.ResponseWriter, r *http.Request) {
	id, err := parsePathID(r, "id")
	if err != nil {
		writeError(w, http.StatusBadRequest, "无效的图片ID")
		return
	}
	path, err := s.store.GetShowPhotoPath(id)
	if err != nil {

		writeError(w, http.StatusNotFound, "图片不存在")
		return
	}

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

	type lineView struct {
		ChatID   int64  `json:"chat_id"`
		ChatName string `json:"chat_name"`

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

		v.Active = isDefault
		views = append(views, v)
	}

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

	if eps, err := s.store.ListEpisodes(episode.ShowID); err == nil {
		if eps == nil {
			eps = []*storage.Episode{}
		}
		resp["episodes"] = eps
	}
	writeJSON(w, http.StatusOK, resp)
}

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

type qualityView struct {
	VariantID int64  `json:"variant_id"`
	Height    int    `json:"height"`
	Width     int    `json:"width"`
	FileSize  int64  `json:"file_size"`
	Label     string `json:"label"`
	IsMain    bool   `json:"is_main"`

	Codec string `json:"-"`
}

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

func oneQualityPerHeight(list []qualityView) []qualityView {
	best := map[int]qualityView{}
	order := make([]int, 0, len(list))
	for _, q := range list {
		h := shortSide(q.Width, q.Height)
		key := h
		if key == 0 {

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

func playableCodec(c string) bool {
	c = strings.ToLower(strings.TrimSpace(c))
	return c == "" || c == "h264" || c == "avc1"
}

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

var reMultiEpisodeTitle = regexp.MustCompile(`(?i)(?:第\s*(?:[0-9]{1,4}|[零一二三四五六七八九十百千两]{1,4})\s*[-~－～—–至到]\s*(?:[0-9]{1,4}|[零一二三四五六七八九十百千两]{1,4})\s*(?:[集话話回]|合集|合辑|合輯)|EP\s*-?\s*[0-9]{1,3}\s*[-~－～—–]\s*(?:EP\s*-?\s*)?[0-9]{1,3})`)

func sourceSpansMultipleEpisodes(s *storage.Source) bool {
	if s == nil {
		return false
	}
	return reMultiEpisodeTitle.MatchString(s.FileName) || reMultiEpisodeTitle.MatchString(s.Caption)
}

func filterMultiEpisodeBundles(list []*storage.Source) []*storage.Source {
	if len(list) < 2 {
		return nil
	}

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

	return ds[(len(ds)-1)/2]
}

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

	show, err := s.store.GetShow(id)
	if err != nil {
		show = nil
	}
	s.serveCover(w, r, show, id)
}

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

func (s *Server) handleClearHistory(w http.ResponseWriter, r *http.Request) {
	if err := s.store.ClearHistory(); err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

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

func (s *Server) handleFavoritesSummary(w http.ResponseWriter, r *http.Request) {
	total, err := s.store.CountFavoriteShows()
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	updates, _ := s.store.CountFavoriteUpdates()
	writeJSON(w, http.StatusOK, map[string]any{"total": total, "updates_total": updates})
}

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

func (s *Server) handleChats(w http.ResponseWriter, r *http.Request) {
	s.writeChats(w, false, "")
}

func (s *Server) handleChatsRefresh(w http.ResponseWriter, r *http.Request) {
	refreshed := false
	var refreshErr string
	if s.svc != nil && s.svc.IsAuthorized() {
		ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
		if err := s.svc.RefreshChats(ctx); err == nil {
			refreshed = true
		} else {

			refreshErr = err.Error()
		}
		cancel()
	}
	s.writeChats(w, refreshed, refreshErr)
}

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

type ChatRecommend struct {
	Name     string `json:"name"`
	Username string `json:"username"`
	TgChatID int64  `json:"tg_chat_id"`
	URL      string `json:"url"`

	Joined bool `json:"joined"`
}

var defaultRecommendChannels = []ChatRecommend{
	{Name: "小黄鸭AI短剧｜成人短剧｜黄果短剧｜免费短剧｜黄豆短剧", Username: "aiduanju_huang_666_888", TgChatID: 4425471457},
	{Name: "AI成人短剧 免费看 【黄果短剧】", Username: "ASC2256", TgChatID: 3894893955},
	{Name: "Ai成人短剧 免费看", Username: "Air1855555", TgChatID: 4341680939},
}

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
