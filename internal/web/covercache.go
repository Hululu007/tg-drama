package web

import (
	"container/list"
	"fmt"
	"hash/fnv"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"tgdrama/internal/storage"
)

// 图片缓存：没有封面的短剧不再回 404，而是即时生成一张带剧名首字的占位图。
// 占位图只有几百字节，放在内存 LRU 里就够（4000 部剧全在内存也就 1MB 出头），
// 不再往磁盘写几千个 .svg 小文件——磁盘缓存只留给真正的图片。
var placeholderCache = newSVGMemo(4096)

// svgMemo 定长 LRU：key -> SVG 内容
type svgMemo struct {
	mu  sync.Mutex
	cap int
	ll  *list.List // 头部最新
	m   map[string]*list.Element
}

type svgEntry struct {
	key  string
	body []byte
	etag string
}

func newSVGMemo(capacity int) *svgMemo {
	return &svgMemo{cap: capacity, ll: list.New(), m: make(map[string]*list.Element, capacity)}
}

func (c *svgMemo) get(key string) ([]byte, string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		c.ll.MoveToFront(el)
		e := el.Value.(*svgEntry)
		return e.body, e.etag
	}
	return nil, ""
}

func (c *svgMemo) put(key string, body []byte, etag string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.m[key]; ok {
		c.ll.MoveToFront(el)
		el.Value.(*svgEntry).body = body
		return
	}
	el := c.ll.PushFront(&svgEntry{key: key, body: body, etag: etag})
	c.m[key] = el
	for c.ll.Len() > c.cap {
		last := c.ll.Back()
		if last == nil {
			break
		}
		c.ll.Remove(last)
		delete(c.m, last.Value.(*svgEntry).key)
	}
}

// fallbackPalette 占位图配色：按短剧 ID 取模，同一部剧颜色始终一致
var fallbackPalette = [][2]string{
	{"#3b4a6b", "#1f2937"},
	{"#5b3a5e", "#2b1b2e"},
	{"#2f4f4a", "#16211f"},
	{"#6b4a2f", "#2e1f13"},
	{"#3a3f6b", "#1c1f36"},
	{"#6b2f3f", "#2e1620"},
}

// serveCover 发封面：优先把真实封面转成 WebP；没有文件（或文件丢了）就发占位图，
// 同时让后台按需补一张——用户翻到哪部就补哪部，不必启动时对着几千部剧猛拉。
func (s *Server) serveCover(w http.ResponseWriter, r *http.Request, show *storage.Show, id int64) {
	if show != nil && show.CoverPath != "" {
		if s.serveImage(w, r, show.CoverPath, "covers/"+strconv.FormatInt(id, 10), coverMaxWidth) {
			return
		}
	}
	// 只有「库里确实存在、但还没有封面文件」的剧才去补：查不到 show 说明
	// 这条 ID 已被合并/隐藏，补了也没人看。
	if s.svc != nil && show != nil {
		s.svc.RequeueCover(id)
	}
	name := ""
	if show != nil {
		name = show.Name
	}
	s.servePlaceholder(w, r, name, id, "covers")
}

// servePhoto 发剧集预览图：真实图片转 WebP；缓存文件丢了就发占位图，不返回破图。
func (s *Server) servePhoto(w http.ResponseWriter, r *http.Request, path string, id int64) {
	if path != "" {
		if s.serveImage(w, r, path, "photos/"+strconv.FormatInt(id, 10), photoMaxWidth) {
			return
		}
	}
	// 走到这里说明发的是占位图：必须把 X-Photo-Ready 改回 0，
	// 否则前端会把封面占位当成真图一直留着（真图可能再也不会来）。
	w.Header().Set("X-Photo-Ready", "0")
	s.servePlaceholder(w, r, "", id, "photos")
}

// servePhotoFallback 预览图还没下载好时，用该剧的封面顶上：封面是这部剧真实的
// 画面，比「剧名首字」的占位图更接近最终内容，切换时视觉跳变更小。
// 封面也没有才退回 SVG 占位图。
//
// 响应带 no-cache：真图落盘后浏览器必须回来重新校验，否则会把封面当成这张
// 预览图一直缓存下去，用户永远看不到真图。
func (s *Server) servePhotoFallback(w http.ResponseWriter, r *http.Request, photoID int64) bool {
	// 无论发的是封面还是 SVG，对这张预览图来说都还不是真图
	w.Header().Set("X-Photo-Ready", "0")
	loc, err := s.store.GetPhotoLocation(photoID)
	if err != nil || loc.ShowID <= 0 {
		return false
	}
	name, cover, err := s.store.GetShowBrief(loc.ShowID)
	if err != nil {
		return false
	}
	if fileExists(cover) {
		return s.serveImageCached(w, r, cover,
			"covers/"+strconv.FormatInt(loc.ShowID, 10), coverMaxWidth, "no-cache")
	}
	s.servePlaceholder(w, r, name, photoID, "photos")
	return true
}

// servePlaceholder 生成占位图（SVG：体积几百字节，任意分辨率都清晰），缓存在内存里。
func (s *Server) servePlaceholder(w http.ResponseWriter, r *http.Request, name string, id int64, kind string) {
	// key 带上剧名：剧名解析得更准（或改名）后占位图会跟着更新，不会一直用旧字
	key := kind + "/" + strconv.FormatInt(id, 10) + "/" + name
	body, etag := placeholderCache.get(key)
	if body == nil {
		body = []byte(placeholderSVG(name, id))
		etag = `"svg-` + hashKey(key) + `"`
		placeholderCache.put(key, body, etag)
	}
	h := w.Header()
	h.Set("Content-Type", "image/svg+xml; charset=utf-8")
	// 占位图必须可重新校验（no-cache + ETag）：它只是「真图还没到」的临时状态。
	// 早先这里是 max-age=86400，于是新下载的封面要等浏览器缓存过期才会显示，
	// 用户看到的是「整天都是首字占位图」。带 ETag 后没变时只是一次 304，
	// 本地开销可以忽略。
	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

// hashKey 给占位图算一个稳定的 ETag（同一部剧 + 同一个剧名始终一致）
func hashKey(key string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return strconv.FormatUint(uint64(h.Sum32()), 36)
}

// placeholderSVG 生成占位封面：顶部渐变色块 + 剧名首字，底部是剧名。
func placeholderSVG(name string, id int64) string {
	c := fallbackPalette[int(id%int64(len(fallbackPalette)))]
	// 取第一个可见字符作为主视觉
	initial := "🎬"
	if trimmed := strings.TrimSpace(name); trimmed != "" {
		r, _ := utf8.DecodeRuneInString(trimmed)
		if r != utf8.RuneError {
			initial = string(r)
		}
	}
	title := name
	if utf8.RuneCountInString(title) > 14 {
		runes := []rune(title)
		title = string(runes[:14]) + "…"
	}
	title = escapeXML(title)
	initial = escapeXML(initial)
	return fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="360" height="480" viewBox="0 0 360 480">
<defs><linearGradient id="g" x1="0" y1="0" x2="0" y2="1">
<stop offset="0%%" stop-color="%s"/><stop offset="100%%" stop-color="%s"/>
</linearGradient></defs>
<rect width="360" height="480" fill="url(#g)"/>
<text x="180" y="228" font-size="120" font-family="-apple-system,PingFang SC,Microsoft YaHei,sans-serif"
 fill="#ffffff" fill-opacity="0.92" text-anchor="middle">%s</text>
<text x="180" y="404" font-size="22" font-family="-apple-system,PingFang SC,Microsoft YaHei,sans-serif"
 fill="#ffffff" fill-opacity="0.72" text-anchor="middle">%s</text>
</svg>`, c[0], c[1], initial, title)
}

// escapeXML 转义剧名里的尖括号与 & 号，避免拼出非法 SVG
func escapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
