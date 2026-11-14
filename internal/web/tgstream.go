package web

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"tgdrama/internal/storage"
	"tgdrama/internal/tgclient"
)

const (
	streamDefaultChunk = 1024 * 1024

	streamDefaultCacheMB = 128

	streamMaxSingleRequest = 1024 * 1024

	streamHeadBlock = 128 * 1024

	streamFastStart = 128 * 1024

	chunkFetchTimeout = 15 * time.Second

	streamFloodWaitCap = 20 * time.Second

	chunkOverallTimeout = 30 * time.Second
)

var errStreamNotAuthorized = errors.New("未登录 Telegram，无法在线播放")

type floodWaitErr struct {
	wait int64
	err  error
}

func (e *floodWaitErr) Error() string { return e.err.Error() }
func (e *floodWaitErr) Unwrap() error { return e.err }

const (
	streamHeadSize = 128 * 1024
	streamWindow   = 1024 * 1024
)

func (s *Server) blockOffset(idx int64) int64 {
	switch {
	case idx <= 0:
		return 0
	case idx == 1:
		return s.headSize
	default:
		return (idx - 1) * streamWindow
	}
}

func (s *Server) blockLen(idx, fileSize int64) int64 {
	off := s.blockOffset(idx)
	if off >= fileSize {
		return 0
	}
	bEnd := s.blockEnd(idx)
	if bEnd > fileSize {
		bEnd = fileSize
	}
	return bEnd - off
}

func (s *Server) blockEnd(idx int64) int64 {
	switch {
	case idx <= 0:
		return s.headSize
	case idx == 1:
		return streamWindow
	default:
		return idx * streamWindow
	}
}

func (s *Server) blockIndexAt(offset int64) int64 {
	if offset < s.headSize {
		return 0
	}
	if offset < streamWindow {
		return 1
	}
	return 2 + (offset-streamWindow)/streamWindow
}

func (s *Server) lastBlockIdx(fileSize int64) int64 {
	return s.blockIndexAt(fileSize - 1)
}

type sourceFetchFunc func(ctx context.Context, src *storage.Source, offset int64, limit int) ([]byte, *storage.Source, error)

type streamState struct {
	mu  sync.Mutex
	src storage.Source

	cdn *tgclient.CDNRedirect
}

func (st *streamState) current() storage.Source {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.src
}

func (st *streamState) update(src *storage.Source) {
	if src == nil {
		return
	}
	st.mu.Lock()
	st.src = *src
	st.mu.Unlock()
}

func (s *Server) streamTelegram(w http.ResponseWriter, r *http.Request, src *storage.Source) {
	if s.fetchChunk == nil {
		writeError(w, http.StatusServiceUnavailable, "在线播放未初始化")
		return
	}
	size := src.FileSize
	if size <= 0 {
		writeError(w, http.StatusNotFound, "缺少文件大小信息，无法在线播放")
		return
	}

	start, end, ranged, err := parseRangeHeader(r.Header.Get("Range"), size)
	if err != nil {
		w.Header().Set("Content-Range", fmt.Sprintf("bytes */%d", size))
		writeError(w, http.StatusRequestedRangeNotSatisfiable, "Range 无效")
		return
	}
	length := end - start + 1

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()

	st := &streamState{src: *src}

	var head []byte
	pos := start

	firstIdx := s.blockIndexAt(pos)
	bOff := s.blockOffset(firstIdx)
	bLen := s.blockLen(firstIdx, size)
	if data, ok := s.cachedBlock(st, firstIdx); ok {
		from := int(pos - bOff)
		to := int(bLen)
		if rest := end - pos + 1; rest < int64(to) {
			to = int(rest)
		}
		head = data[from:to]
		pos = bOff + int64(to)
	} else if pos != bOff {
		piece := int64(streamFastStart)
		if rest := end - pos + 1; rest < piece {
			piece = rest
		}
		if wr := int64(bLen) - int64(pos-bOff); wr < piece {
			piece = wr
		}
		fast, ferr := s.fetchChunkUncached(ctx, st, pos, int(piece))
		if ferr != nil {
			var fw *floodWaitErr
			status := http.StatusBadGateway
			msg := "Telegram 读取失败: " + ferr.Error()
			if errors.As(ferr, &fw) {

				status = http.StatusServiceUnavailable
				msg = fmt.Sprintf("Telegram 限流中，%d 秒后自动恢复", fw.wait+1)
				w.Header().Set("Retry-After", strconv.Itoa(int(fw.wait)+1))
			} else if errors.Is(ferr, errStreamNotAuthorized) {
				status = http.StatusServiceUnavailable
			}
			writeError(w, status, msg)
			return
		}
		head = fast
		pos += int64(len(head))
	} else {
		var err error
		var fw *floodWaitErr
		for attempt := 0; attempt < 3; attempt++ {
			head, err = s.fetchWholeBlock(ctx, st, firstIdx)
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return
			}
			if isAuthError(err) {
				writeError(w, http.StatusServiceUnavailable, err.Error())
				return
			}
			if errors.As(err, &fw) {
				break
			}
			if !waitCtx(ctx, time.Duration(300*(1<<attempt))*time.Millisecond) {
				return
			}
		}
		if err != nil {
			if errors.As(err, &fw) {

				w.Header().Set("Retry-After", strconv.Itoa(int(fw.wait)+1))
				writeError(w, http.StatusServiceUnavailable,
					fmt.Sprintf("Telegram 限流中，%d 秒后自动恢复", fw.wait+1))
				return
			}
			writeError(w, http.StatusBadGateway, "Telegram 读取失败: "+err.Error())
			return
		}
		to := int(bLen)
		if rest := end - pos + 1; rest < int64(to) {
			to = int(rest)
		}
		head = head[:to]
		pos = bOff + int64(to)
	}

	if s.svc != nil {
		s.svc.SetPlaybackActive(true)
		defer s.svc.SetPlaybackActive(false)
	}

	ct := src.Mime
	if ct == "" {
		ct = "video/mp4"
	}
	h := w.Header()
	h.Set("Content-Type", ct)
	h.Set("Accept-Ranges", "bytes")
	h.Set("Content-Length", strconv.FormatInt(length, 10))

	h.Set("ETag", fmt.Sprintf("\"src-%d-%d-%d\"", src.ID, src.DocID, size))

	h.Set("Cache-Control", "private, max-age=3600")
	h.Set("Content-Disposition", "inline")
	if ranged {
		h.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, size))
		w.WriteHeader(http.StatusPartialContent)
	} else {
		w.WriteHeader(http.StatusOK)
	}
	if r.Method == http.MethodHead {
		return
	}

	flusher, _ := w.(http.Flusher)

	if len(head) > 0 {
		if _, err := w.Write(head); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	if pos <= end {
		if err := s.streamRange(ctx, w, flusher, st, size, pos, end); err != nil {

			return
		}
	}
}

func (s *Server) streamRange(ctx context.Context, w io.Writer, flusher http.Flusher,
	st *streamState, size, start, end int64) error {

	pos := start
	for pos <= end {
		if err := ctx.Err(); err != nil {
			return err
		}
		idx := s.blockIndexAt(pos)
		bOff := s.blockOffset(idx)
		bLen := s.blockLen(idx, size)
		if bLen <= 0 {
			return nil
		}
		bEnd := bOff + bLen

		writeEnd := bEnd
		if end < writeEnd-1 {
			writeEnd = end + 1
		}
		from := int(pos - bOff)
		to := int(writeEnd - bOff)

		if data, ok := s.cachedBlock(st, idx); ok {
			if from < 0 || to > len(data) || from > to {
				return fmt.Errorf("缓存切片越界：块 %d [%d,%d) len=%d", idx, from, to, len(data))
			}
			if _, err := w.Write(data[from:to]); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
			pos = bOff + int64(to)
			s.prefetchNext(ctx, st, idx, size, pos, end)
			continue
		}

		if pos == bOff {

			var data []byte
			var err error
			for attempt := 0; attempt < 3; attempt++ {
				data, err = s.fetchWholeBlock(ctx, st, idx)
				if err == nil {
					break
				}
				if ctx.Err() != nil {
					return ctx.Err()
				}
				if isAuthError(err) {
					return err
				}
				if !waitCtx(ctx, time.Duration(300*(1<<attempt))*time.Millisecond) {
					return ctx.Err()
				}
			}
			if err != nil {
				srcNow := st.current()
				log.Printf("播放整块读取失败 source=%d doc=%d idx=%d off=%d len=%d: %v",
					srcNow.ID, srcNow.DocID, idx, bOff, bLen, err)
				return err
			}
			if from < 0 || to > len(data) || from > to {
				return fmt.Errorf("分块数据不完整：块 %d len=%d", idx, len(data))
			}
			if _, err := w.Write(data[from:to]); err != nil {
				return err
			}
			if flusher != nil {
				flusher.Flush()
			}
			pos = bOff + int64(to)
			s.prefetchNext(ctx, st, idx, size, pos, end)
			continue
		}

		piece := writeEnd - pos
		if piece > streamFastStart {
			piece = streamFastStart
		}
		var data []byte
		var err error
		for attempt := 0; attempt < 3; attempt++ {
			data, err = s.fetchChunkUncached(ctx, st, pos, int(piece))
			if err == nil {
				break
			}
			if ctx.Err() != nil {
				return ctx.Err()
			}
			if isAuthError(err) {
				return err
			}
			if !waitCtx(ctx, time.Duration(300*(1<<attempt))*time.Millisecond) {
				return ctx.Err()
			}
		}
		if err != nil {
			srcNow := st.current()
			log.Printf("播放残段读取失败 source=%d doc=%d off=%d len=%d: %v",
				srcNow.ID, srcNow.DocID, pos, piece, err)
			return err
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		if flusher != nil {
			flusher.Flush()
		}
		pos += int64(len(data))
	}
	return nil
}

func (s *Server) prefetchNext(ctx context.Context, st *streamState, idx int64, size, pos, end int64) {
	bEnd := s.blockEnd(idx)
	if pos < bEnd && bEnd-pos > streamFastStart {
		return
	}
	next := idx + 1
	if s.blockOffset(next) >= size || pos > end {
		return
	}
	if s.blockCached(st, next) {
		return
	}
	go func() {
		pctx, cancel := context.WithTimeout(ctx, s.chunkOverallTimeout)
		defer cancel()
		_, _ = s.fetchWholeBlock(pctx, st, next)
	}()
}

func (s *Server) blockCached(st *streamState, idx int64) bool {
	_, ok := s.cachedBlock(st, idx)
	return ok
}

func (s *Server) cachedBlock(st *streamState, idx int64) ([]byte, bool) {
	src := st.current()
	key := chunkKey{source: src.ID, doc: src.DocID, index: idx}
	if data, ok := s.chunks.peek(key); ok {
		return data, true
	}
	if s.disk != nil {
		if data, ok := s.disk.serveBlock(src.DocID, idx); ok {
			return data, true
		}
	}
	return nil, false
}

type cdnRedirectErr struct {
	redirect *tgclient.CDNRedirect
}

func (e *cdnRedirectErr) Error() string { return "文件由 CDN DC 分发" }

func (s *Server) fetchChunkWithCDN(ctx context.Context, st *streamState,
	src *storage.Source, offset int64, limit int) ([]byte, *storage.Source, error) {

	for cdnAttempt := 0; cdnAttempt < 2 && st.cdn != nil; cdnAttempt++ {
		data, err := s.svc.CDNChunk(ctx, st.cdn, src, offset, limit)
		if err == nil {
			return data, src, nil
		}
		if errors.Is(err, tgclient.ErrCDNTokenInvalid) {

			st.cdn = nil
			continue
		}
		if d, ok := tgerr.AsFloodWait(err); ok && d > streamFloodWaitCap {
			return nil, src, &floodWaitErr{wait: int64(d.Seconds()), err: err}
		}
		return nil, src, err
	}

	data, refreshed, err := s.fetchChunk(ctx, src, offset, limit)
	if err == nil {
		return data, refreshed, nil
	}
	var red *cdnRedirectErr
	if errors.As(err, &red) {

		st.cdn = red.redirect
		data, cerr := s.svc.CDNChunk(ctx, st.cdn, src, offset, limit)
		if cerr == nil {
			return data, refreshed, nil
		}
		if errors.Is(cerr, tgclient.ErrCDNTokenInvalid) {
			st.cdn = nil
		}
		return nil, refreshed, cerr
	}
	return nil, refreshed, err
}

func (s *Server) fetchChunkUncached(ctx context.Context, st *streamState, offset int64, limit int) ([]byte, error) {
	src := st.current()
	data, refreshed, err := s.fetchChunkWithCDN(ctx, st, &src, offset, limit)
	if err != nil {
		return nil, err
	}
	st.update(refreshed)
	if len(data) > limit {
		data = data[:limit]
	}
	return data, nil
}

func (s *Server) fetchWholeBlock(ctx context.Context, st *streamState, idx int64) ([]byte, error) {
	src := st.current()
	off := s.blockOffset(idx)
	bLen := s.blockLen(idx, src.FileSize)
	if bLen <= 0 {
		return nil, nil
	}
	key := chunkKey{source: src.ID, doc: src.DocID, index: idx}
	if data, ok := s.chunks.peek(key); ok {
		return data, nil
	}
	overallCtx, cancel := context.WithTimeout(ctx, s.chunkOverallTimeout)
	defer cancel()
	return s.chunks.fetch(overallCtx, key, func(ctx context.Context) ([]byte, error) {
		cur := src
		data, refreshed, err := s.fetchChunkWithCDN(ctx, st, &cur, off, int(bLen))
		if err != nil {
			return nil, err
		}
		st.update(refreshed)
		if len(data) != int(bLen) {
			return nil, fmt.Errorf("Telegram 返回数据不足（期望 %d 字节，实际 %d 字节）", bLen, len(data))
		}
		if s.disk != nil {
			s.disk.storeBlock(src.DocID, idx, data, src.FileSize, src.ID)
		}
		return data, nil
	})
}

func (s *Server) refreshStreamSource(ctx context.Context, src *storage.Source) (*storage.Source, error) {
	if s.svc == nil {
		return nil, errStreamNotAuthorized
	}
	if src.ID >= 0 {
		return s.svc.RefreshSource(ctx, src)
	}
	v, err := s.svc.RefreshVariant(ctx, -src.ID)
	if err != nil {
		return nil, err
	}
	out := *src
	out.DocID, out.AccessHash, out.FileReference = v.DocID, v.AccessHash, v.FileReference
	out.FileSize, out.Width, out.Height, out.Mime = v.FileSize, v.Width, v.Height, v.Mime
	return &out, nil
}

func (s *Server) fetchChunkFromTelegram(ctx context.Context, src *storage.Source,
	offset int64, limit int) ([]byte, *storage.Source, error) {

	if s.svc == nil {
		return nil, src, errStreamNotAuthorized
	}
	api := s.svc.API()
	if api == nil {
		return nil, src, errStreamNotAuthorized
	}
	data, refreshed, err := s.fetchFileChunk(ctx, api, src, offset, limit)
	if err != nil && isAuthError(err) {
		return nil, src, errStreamNotAuthorized
	}
	return data, refreshed, err
}

func (s *Server) fetchFileChunk(ctx context.Context, api *tg.Client,
	src *storage.Source, offset int64, limit int) ([]byte, *storage.Source, error) {

	const align int64 = 4096
	alignedOffset := offset - offset%align
	skip := int(offset - alignedOffset)
	alignedLimit := ((int64(skip) + int64(limit) + align - 1) / align) * align
	if alignedLimit > streamMaxSingleRequest {
		alignedLimit = streamMaxSingleRequest
	}

	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		if ctx.Err() != nil {
			return nil, src, ctx.Err()
		}
		req := &tg.UploadGetFileRequest{

			Precise: true,
			Location: &tg.InputDocumentFileLocation{
				ID: src.DocID, AccessHash: src.AccessHash,
				FileReference: src.FileReference,
			},
			Offset: alignedOffset,
			Limit:  int(alignedLimit),
		}
		req.SetFlags()

		attemptCtx, cancel := context.WithTimeout(ctx, s.chunkTimeout)
		res, err := api.UploadGetFile(attemptCtx, req)
		cancel()
		if err == nil {
			switch f := res.(type) {
			case *tg.UploadFile:
				data := f.Bytes
				if skip > 0 {
					if skip >= len(data) {
						return nil, src, errors.New("偏移超出文件范围")
					}
					data = data[skip:]
				}
				if len(data) > limit {
					data = data[:limit]
				}
				return data, src, nil
			case *tg.UploadFileCDNRedirect:

				return nil, src, &cdnRedirectErr{redirect: tgclient.NewCDNRedirect(f)}
			default:
				return nil, src, fmt.Errorf("未知响应类型 %T", res)
			}
		}
		lastErr = err

		if d, ok := tgerr.AsFloodWait(err); ok {
			if d > streamFloodWaitCap {

				return nil, src, &floodWaitErr{wait: int64(d.Seconds()), err: err}
			}
			if !waitCtx(ctx, d+time.Second) {
				return nil, src, ctx.Err()
			}
			continue
		}
		if isFileRefError(err) && s.svc != nil {

			rctx, rcancel := context.WithTimeout(ctx, s.chunkTimeout)
			refreshed, rerr := s.refreshStreamSource(rctx, src)
			rcancel()
			if rerr != nil {
				return nil, src, rerr
			}
			src = refreshed
			continue
		}

		if isAuthError(err) {
			return nil, src, err
		}
		if attempt < 3 {
			if !waitCtx(ctx, time.Duration(250*(1<<attempt))*time.Millisecond) {
				return nil, src, ctx.Err()
			}
			continue
		}
	}
	return nil, src, lastErr
}

func isAuthError(err error) bool {
	var rpcErr *tgerr.Error
	if errors.As(err, &rpcErr) {
		switch rpcErr.Code {
		case 401, 403:
			return true
		}
	}
	msg := err.Error()
	return strings.Contains(msg, "AUTH_KEY") || strings.Contains(msg, "SESSION_REVOKED") ||
		strings.Contains(msg, "USER_DEACTIVATED") || strings.Contains(msg, "未登录")
}

func isFileRefError(err error) bool {
	var rpcErr *tgerr.Error
	if errors.As(err, &rpcErr) {
		return rpcErr.IsType("FILE_REFERENCE_EXPIRED") ||
			rpcErr.IsType("FILE_REFERENCE_INVALID") ||
			rpcErr.IsType("FILE_REFERENCE_EMPTY") ||
			rpcErr.IsType("FILEREF_UPGRADE_NEEDED")
	}
	return strings.Contains(err.Error(), "FILE_REFERENCE")
}

func parseRangeHeader(header string, size int64) (start, end int64, ranged bool, err error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, size - 1, false, nil
	}
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false, errors.New("不支持的 Range 单位")
	}

	spec := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = strings.TrimSpace(spec[:i])
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false, errors.New("Range 格式错误")
	}
	if parts[0] == "" {

		n, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil || n <= 0 {
			return 0, 0, false, errors.New("Range 后缀长度无效")
		}
		if n > size {
			n = size
		}
		return size - n, size - 1, true, nil
	}
	s, e := strconv.ParseInt(parts[0], 10, 64)
	if e != nil || s < 0 || s >= size {
		return 0, 0, false, errors.New("Range 起点无效")
	}
	end = size - 1
	if parts[1] != "" {
		e2, e := strconv.ParseInt(parts[1], 10, 64)
		if e != nil || e2 < s {
			return 0, 0, false, errors.New("Range 终点无效")
		}
		if e2 < size-1 {
			end = e2
		}
	}
	return s, end, true, nil
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func waitCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
