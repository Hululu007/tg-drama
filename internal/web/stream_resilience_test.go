package web

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gotd/td/bin"
	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"tgdrama/internal/storage"
)

func slowFetcher(release *atomic.Bool) sourceFetchFunc {
	return func(ctx context.Context, src *storage.Source, offset int64, limit int) ([]byte, *storage.Source, error) {
		if !release.Load() {

			<-make(chan struct{})
		}
		data := make([]byte, limit)
		for i := range data {
			data[i] = byte(offset)
		}
		return data, src, nil
	}
}

func TestStreamStuckChunkDoesNotBlockForever(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	lines, err := store.ListEpisodeLines(ep1)
	if err != nil || len(lines) == 0 || len(lines[0].Segments) == 0 {
		t.Fatalf("测试数据缺少线路: %v", err)
	}
	src := &lines[0].Segments[0]

	var release atomic.Bool
	srv.applyStreamOptions(256*1024, 64)
	srv.chunkTimeout = 150 * time.Millisecond
	srv.chunkOverallTimeout = 600 * time.Millisecond
	srv.fetchChunk = slowFetcher(&release)

	start := time.Now()
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/stream/"+itoa(src.ID), nil))
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("挂死块未被超时打断，耗时 %v", elapsed)
	}
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("首块失败应 502，得到 %d", rec.Code)
	}

	release.Store(true)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/api/stream/"+itoa(src.ID), nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("恢复后应 200，得到 %d %s", rec2.Code, rec2.Body.String())
	}
	if int64(rec2.Body.Len()) != src.FileSize {
		t.Fatalf("恢复后响应体长度 = %d，期望 %d", rec2.Body.Len(), src.FileSize)
	}
}

func TestFetchFileChunkHangTimeout(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	lines, _ := store.ListEpisodeLines(ep1)
	src := &lines[0].Segments[0]

	inv := &scriptedInvoker{hangOf: 1, payload: make([]byte, 256*1024)}
	api := tg.NewClient(inv)
	srv.chunkTimeout = 200 * time.Millisecond

	start := time.Now()
	data, _, err := srv.fetchFileChunk(context.Background(), api, src, 0, 256*1024)
	if err != nil {
		t.Fatalf("挂死后应重试成功，得到 %v", err)
	}
	if len(data) != 256*1024 {
		t.Fatalf("数据长度 = %d", len(data))
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("挂死请求应被打断后恢复，实际耗时 %v", elapsed)
	}
	if inv.calls.Load() != 2 {

		t.Fatalf("应调用 2 次（1 挂死 + 1 重试），实际 %d", inv.calls.Load())
	}
}

type refreshInvoker struct {
	getFileCalls atomic.Int32
	getMsgCalls  atomic.Int32
	payload      []byte
}

func (f *refreshInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	switch in := input.(type) {
	case *tg.UploadGetFileRequest:
		if f.getFileCalls.Add(1) == 1 {
			return tgerr.New(400, "FILE_REFERENCE_EXPIRED")
		}
		if box, ok := output.(*tg.UploadFileBox); ok && box != nil {

			box.File = &tg.UploadFile{Bytes: make([]byte, in.Limit)}
		}
		return nil
	case *tg.ChannelsGetMessagesRequest:
		_ = in
		f.getMsgCalls.Add(1)
		if box, ok := output.(*tg.MessagesMessagesBox); ok && box != nil {
			doc := &tg.Document{
				ID:            42,
				AccessHash:    7,
				FileReference: []byte("fresh-reference"),
				MimeType:      "video/mp4",
				Size:          1 << 20,
			}
			msg := &tg.Message{
				ID: 1000,
				Media: &tg.MessageMediaDocument{
					Document: doc,
				},
			}
			box.Messages = &tg.MessagesChannelMessages{
				Count:    1,
				Messages: []tg.MessageClass{msg},
			}
		}
		return nil
	}
	return nil
}

func TestStreamRefreshedSourcePersists(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	lines, err := store.ListEpisodeLines(ep1)
	if err != nil || len(lines) == 0 || len(lines[0].Segments) == 0 {
		t.Fatalf("测试数据缺少线路: %v", err)
	}
	src := &lines[0].Segments[0]

	inv := &refreshInvoker{payload: make([]byte, 256*1024)}

	srv.svc.SetAPIForTest(tg.NewClient(inv))

	src.DocID = 42
	if err := store.UpdateSourceLocation(src.ID, 42, 7, []byte("old")); err != nil {
		t.Fatal(err)
	}

	srv.applyStreamOptions(256*1024, 64)
	srv.chunkTimeout = 5 * time.Second
	srv.fetchChunk = srv.fetchChunkFromTelegram

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/stream/"+itoa(src.ID), nil)
	req.Header.Set("Range", "bytes=0-1048575")
	srv.streamTelegram(rec, req, src)
	if rec.Code != http.StatusPartialContent {
		t.Fatalf("应 206（带 Range 请求），得到 %d %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 1<<20 {
		t.Fatalf("响应体长度 = %d，期望 %d", rec.Body.Len(), 1<<20)
	}

	if n := inv.getFileCalls.Load(); n != 3 {
		t.Fatalf("upload.getFile 应调用 3 次（头块失败+成功，块 1 一次），实际 %d", n)
	}
	if n := inv.getMsgCalls.Load(); n != 1 {
		t.Fatalf("刷新消息应只拉 1 次，实际 %d", n)
	}
}

type scriptedInvoker struct {
	calls    atomic.Int32
	floodOf  int
	floodSec int
	hangOf   int
	payload  []byte
}

func (f *scriptedInvoker) Invoke(ctx context.Context, input bin.Encoder, output bin.Decoder) error {
	n := f.calls.Add(1)
	if f.hangOf > 0 && int(n) == f.hangOf {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.floodOf > 0 && int(n) == f.floodOf {
		return tgerr.New(420, "FLOOD_WAIT_"+itoa(int64(f.floodSec)))
	}
	if box, ok := output.(*tg.UploadFileBox); ok && box != nil {
		box.File = &tg.UploadFile{Bytes: f.payload}
	}
	return nil
}

func TestFetchFileChunkFloodShortWait(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	lines, _ := store.ListEpisodeLines(ep1)
	src := &lines[0].Segments[0]

	inv := &scriptedInvoker{floodOf: 1, floodSec: 1, payload: make([]byte, 256*1024)}
	api := tg.NewClient(inv)
	srv.chunkTimeout = 5 * time.Second

	data, _, err := srv.fetchFileChunk(context.Background(), api, src, 0, 256*1024)
	if err != nil {
		t.Fatalf("FLOOD_WAIT 1s 应等待后成功，得到 %v", err)
	}
	if len(data) != 256*1024 {
		t.Fatalf("数据长度 = %d", len(data))
	}
	if inv.calls.Load() != 2 {
		t.Fatalf("应调用 2 次（1 次 FLOOD_WAIT + 1 次成功），实际 %d", inv.calls.Load())
	}
}

func TestFetchFileChunkFloodLongReturn(t *testing.T) {
	srv, store := newTestServerRaw(t)
	_, ep1, _ := seedShow(t, store)
	lines, _ := store.ListEpisodeLines(ep1)
	src := &lines[0].Segments[0]

	inv := &scriptedInvoker{floodOf: 1, floodSec: 3000}
	api := tg.NewClient(inv)
	srv.chunkTimeout = 5 * time.Second

	start := time.Now()
	_, _, err := srv.fetchFileChunk(context.Background(), api, src, 0, 256*1024)
	if err == nil {
		t.Fatal("长 FLOOD_WAIT 应返回错误")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatal("长 FLOOD_WAIT 不应原地等待")
	}
}
