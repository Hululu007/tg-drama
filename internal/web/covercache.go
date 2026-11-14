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

var placeholderCache = newSVGMemo(4096)

type svgMemo struct {
	mu  sync.Mutex
	cap int
	ll  *list.List
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

var fallbackPalette = [][2]string{
	{"#3b4a6b", "#1f2937"},
	{"#5b3a5e", "#2b1b2e"},
	{"#2f4f4a", "#16211f"},
	{"#6b4a2f", "#2e1f13"},
	{"#3a3f6b", "#1c1f36"},
	{"#6b2f3f", "#2e1620"},
}

func (s *Server) serveCover(w http.ResponseWriter, r *http.Request, show *storage.Show, id int64) {
	if show != nil && show.CoverPath != "" {
		if s.serveImage(w, r, show.CoverPath, "covers/"+strconv.FormatInt(id, 10), coverMaxWidth) {
			return
		}
	}

	if s.svc != nil && show != nil {
		s.svc.RequeueCover(id)
	}
	name := ""
	if show != nil {
		name = show.Name
	}
	s.servePlaceholder(w, r, name, id, "covers")
}

func (s *Server) servePhoto(w http.ResponseWriter, r *http.Request, path string, id int64) {
	if path != "" {
		if s.serveImage(w, r, path, "photos/"+strconv.FormatInt(id, 10), photoMaxWidth) {
			return
		}
	}

	w.Header().Set("X-Photo-Ready", "0")
	s.servePlaceholder(w, r, "", id, "photos")
}

func (s *Server) servePhotoFallback(w http.ResponseWriter, r *http.Request, photoID int64) bool {

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

func (s *Server) servePlaceholder(w http.ResponseWriter, r *http.Request, name string, id int64, kind string) {

	key := kind + "/" + strconv.FormatInt(id, 10) + "/" + name
	body, etag := placeholderCache.get(key)
	if body == nil {
		body = []byte(placeholderSVG(name, id))
		etag = `"svg-` + hashKey(key) + `"`
		placeholderCache.put(key, body, etag)
	}
	h := w.Header()
	h.Set("Content-Type", "image/svg+xml; charset=utf-8")

	h.Set("Cache-Control", "no-cache")
	h.Set("ETag", etag)
	if inm := r.Header.Get("If-None-Match"); inm != "" && strings.Contains(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	_, _ = w.Write(body)
}

func hashKey(key string) string {
	h := fnv.New32a()
	_, _ = h.Write([]byte(key))
	return strconv.FormatUint(uint64(h.Sum32()), 36)
}

func placeholderSVG(name string, id int64) string {
	c := fallbackPalette[int(id%int64(len(fallbackPalette)))]

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

func escapeXML(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}
