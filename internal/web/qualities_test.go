package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"tgdrama/internal/storage"
)

// seedVariantSource 给某集补一条带清晰度版本的来源，返回来源与变体。
func seedVariantSource(t *testing.T, srv *Server, store *storage.DB, ep int64) (*storage.Source, []*storage.SourceVariant) {
	t.Helper()
	lines, err := store.ListEpisodeLines(ep)
	if err != nil || len(lines) == 0 || len(lines[0].Segments) == 0 {
		t.Fatalf("缺少来源: %v", err)
	}
	src := lines[0].Segments[0]
	vars := []storage.SourceVariant{
		{DocID: 7001, AccessHash: 1, FileReference: []byte("r720"), FileSize: 700, Duration: 60, Width: 720, Height: 1280, Mime: "video/mp4"},
		{DocID: 7002, AccessHash: 2, FileReference: []byte("r480"), FileSize: 400, Duration: 60, Width: 480, Height: 854, Mime: "video/mp4"},
	}
	if err := store.ReplaceSourceVariantsByID(src.ID, vars); err != nil {
		t.Fatal(err)
	}
	_ = srv
	list, err := store.ListSourceVariants(src.ID)
	if err != nil || len(list) != 2 {
		t.Fatalf("variants = %+v, %v", list, err)
	}
	return &src, list
}

// /qualities 按短边从高到低返回，标签是短边像素；没有副版本时只有主版本。
func TestEpisodeQualitiesList(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	seedVariantSource(t, srv, store, ep1)
	h := srv.Handler()

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/episodes/"+itoa(ep1)+"/qualities", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("qualities = %d %s", rec.Code, rec.Body.String())
	}
	var body struct {
		List []qualityView `json:"list"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.List) != 2 {
		t.Fatalf("qualities = %+v", body.List)
	}
	if body.List[0].Label != "720P" || body.List[1].Label != "480P" {
		t.Fatalf("labels = %q,%q", body.List[0].Label, body.List[1].Label)
	}
	if body.List[0].VariantID == 0 {
		t.Fatal("variant id missing")
	}

	// 不存在的集：404（前端据此不显示入口）
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/episodes/"+itoa(ep1+9999)+"/qualities", nil))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("missing episode should 404, got %d", rec.Code)
	}
}

// 选清晰度后必须真的用变体自己的 doc 取流；跨集或跨线路的变体拒绝。
func TestEpisodePlayVariant(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, ep2 := seedShow(t, store)
	src, vars := seedVariantSource(t, srv, store, ep1)
	srv.applyStreamOptions(256*1024, 64)

	var mu sync.Mutex
	var docs []int64
	srv.fetchChunk = func(ctx context.Context, s *storage.Source, off int64, limit int) ([]byte, *storage.Source, error) {
		mu.Lock()
		docs = append(docs, s.DocID)
		mu.Unlock()
		data := make([]byte, limit)
		for i := range data {
			data[i] = byte(off / 1024)
		}
		return data, s, nil
	}
	h := srv.Handler()

	play := func(ep int64, query string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/episodes/"+itoa(ep)+"/play"+query, nil)
		req.Header.Set("Range", "bytes=0-1023")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// 选 480P 变体
	rec := play(ep1, "?vq="+itoa(vars[1].ID))
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("variant play = %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	if len(docs) != 1 || docs[0] != vars[1].DocID {
		t.Fatalf("streamed doc = %v, want %d", docs, vars[1].DocID)
	}
	mu.Unlock()

	etag480 := rec.Header().Get("ETag")

	// 切回 720P：缓存键按文档隔离，必须再用 720 的 doc 取，不能命中 480 的块
	rec = play(ep1, "?vq="+itoa(vars[0].ID))
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("variant play = %d", rec.Code)
	}
	if etag := rec.Header().Get("ETag"); etag == "" || etag == etag480 {
		t.Fatalf("ETag must differ per document: %q vs %q", etag480, etag)
	}
	mu.Lock()
	if len(docs) != 2 || docs[1] != vars[0].DocID {
		t.Fatalf("streamed docs = %v", docs)
	}
	mu.Unlock()

	// 跨集使用该变体：拒绝
	if rec := play(ep2, "?vq="+itoa(vars[0].ID)); rec.Code != http.StatusNotFound {
		t.Fatalf("cross-episode variant should 404, got %d", rec.Code)
	}
	// 不存在的变体：拒绝
	if rec := play(ep1, "?vq=999999"); rec.Code != http.StatusNotFound {
		t.Fatalf("missing variant should 404, got %d", rec.Code)
	}
	_ = src
}

// 同一分辨率可能同时有 H.264 与 HEVC 两个版本：菜单只留一档，优先浏览器能播的编码。
func TestOneQualityPerHeight(t *testing.T) {
	list := []qualityView{
		{VariantID: 1, Width: 1080, Height: 1920, FileSize: 100, Codec: "h265"},
		{VariantID: 2, Width: 1080, Height: 1920, FileSize: 200, Codec: "h264"},
		{VariantID: 3, Width: 720, Height: 1280, FileSize: 50, Codec: "h264"},
		{VariantID: 4, Width: 720, Height: 1280, FileSize: 60, Codec: "h265"},
	}
	got := oneQualityPerHeight(list)
	if len(got) != 2 {
		t.Fatalf("dedupe = %+v", got)
	}
	if got[0].VariantID != 2 || got[1].VariantID != 3 {
		t.Fatalf("preferred = %d,%d; want 2,3", got[0].VariantID, got[1].VariantID)
	}
	// 主版本优先（原始文件必然能播）
	withMain := append([]qualityView{{VariantID: 0, Width: 1080, Height: 1920, IsMain: true}}, list...)
	if got := oneQualityPerHeight(withMain); len(got) != 2 || !got[0].IsMain {
		t.Fatalf("main should win its height: %+v", got)
	}
}
