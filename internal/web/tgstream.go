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
	// streamDefaultChunk 首播/拖动时单次 upload.getFile 的默认块大小。
	// 单线程顺序读取：块小首帧快，但请求次数多；官方上限 1MB，请求数最少。
	streamDefaultChunk = 1024 * 1024
	// streamDefaultCacheMB 已读块的内存缓存上限。
	streamDefaultCacheMB = 128
	// streamMaxSingleRequest Telegram 单次 upload.getFile 上限 1MB（官方 files API 文档：
	// "limit must not exceed 1048576 (1 MB)"），超出返回 LIMIT_INVALID。
	streamMaxSingleRequest = 1024 * 1024
	// streamHeadBlock 块 0 的固定大小：首帧出画只等 128KB，不再等整块 1MB。
	// moov 在前部（MP4 faststart），128KB 足够浏览器开始解码。
	streamHeadBlock = 128 * 1024
	// streamFastStart 播放从大块中间恢复（续播/拖动）时，先不加缓存地拉
	// 这么多字节出画，避免等完整 1MB 块。
	streamFastStart = 128 * 1024
	// chunkFetchTimeout 单次 upload.getFile 的硬超时。
	// gotd 的 RPC 在服务端 ACK 之后若响应丢失（代理黑洞、DC 抖动）会一直等下去，
	// 而块缓存的全局信号量只有 1：一个请求挂死，后面所有播放请求（包括重试）
	// 都会永远排队，界面表现为「一直缓冲中」。必须给单次请求一个上限，
	// 超时后放弃本次尝试走退避重试，信号量能被释放，队列才能流动。
	chunkFetchTimeout = 15 * time.Second
	// streamFloodWaitCap 播放路径愿意原地等待 FLOOD_WAIT 的上限。
	// 更长的等待不如直接失败：前端会自动切线路/提示，而不是让连接挂着占住信号量。
	streamFloodWaitCap = 20 * time.Second
	// chunkOverallTimeout 单块读取（含内部重试与限流等待）的整体上限。
	// 无论下面怎么重试，占住全局信号量的时间都不能没有尽头。
	chunkOverallTimeout = 30 * time.Second
)

// errStreamNotAuthorized 未登录/会话失效时返回，前端据此提示扫码而不是"读取失败"。
var errStreamNotAuthorized = errors.New("未登录 Telegram，无法在线播放")

// floodWaitErr 账号级限流（FLOOD_WAIT 超过可原地等待的上限）。
// HTTP 层据此返回 503 + Retry-After，前端倒计时后自动重试。
type floodWaitErr struct {
	wait int64 // 建议等待秒数
	err  error
}

func (e *floodWaitErr) Error() string { return e.err.Error() }
func (e *floodWaitErr) Unwrap() error { return e.err }

// 块布局（满足官方硬性规则：单次 upload.getFile 不得跨越 1MB 窗口，
// 即 offset/(1MB) == (offset+limit-1)/(1MB)）：
//
//	块 0  = [0, 128KB)          头块（首帧快启，moov 在前部）
//	块 1  = [128KB, 1MB)        补齐第一个 1MB 窗口
//	块 N≥2 = [(N-1)*1MB, N*1MB) 与 1MB 窗口一一对齐
//
// mem/disk 缓存与 CDN 分片都按此布局工作。
const (
	streamHeadSize = 128 * 1024
	streamWindow   = 1024 * 1024
)

// blockOffset 块 idx 在文件内的起始偏移。
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

// blockLen 块 idx 的字节长度（最后一个块被文件大小截断）。
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

// blockEnd 块 idx 的结束偏移（不含）。
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

// blockIndexAt 字节 offset 所在的块序号。
func (s *Server) blockIndexAt(offset int64) int64 {
	if offset < s.headSize {
		return 0
	}
	if offset < streamWindow {
		return 1
	}
	return 2 + (offset-streamWindow)/streamWindow
}

// lastBlockIdx 文件的最后一个块序号。
func (s *Server) lastBlockIdx(fileSize int64) int64 {
	return s.blockIndexAt(fileSize - 1)
}

// sourceFetchFunc 读取来源文件 [offset, offset+limit) 的字节。
// 抽成函数类型便于测试时替换成内存假数据。
type sourceFetchFunc func(ctx context.Context, src *storage.Source, offset int64, limit int) ([]byte, *storage.Source, error)

// streamState 一次播放 HTTP 请求内共享的读取状态。
// cur 持有当前来源：upload.getFile 返回 FILE_REFERENCE_EXPIRED 时会重新拉消息
// 刷新 file_reference 并落库；刷新结果必须对同一请求的后续块生效，
// 否则每个块都会先失败一次、再刷新一次，请求量翻倍（慢且容易触发限流）。
// 预读协程与主循环并发访问，用互斥锁保护。
type streamState struct {
	mu  sync.Mutex
	src storage.Source
	// cdn 非 nil 表示该文件由 CDN DC 分发（upload.fileCdnRedirect 已确认）。
	cdn *tgclient.CDNRedirect
}

func (st *streamState) current() storage.Source {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.src
}

// update 在块读取返回刷新后的来源时更新当前状态（nil 表示没有刷新）。
func (st *streamState) update(src *storage.Source) {
	if src == nil {
		return
	}
	st.mu.Lock()
	st.src = *src
	st.mu.Unlock()
}

// streamTelegram 把 Telegram 视频按 HTTP Range 实时代理给浏览器：
// 不落盘、边取边播；块级缓存让拖动、重播和多个播放器共享同一份数据。
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

	// 播放流开始前的首块探测：失败能以明确的 HTTP 状态码返回（503/502）
	// 快启路径已在下面一并处理
	var head []byte
	pos := start

	// 首段取数提到响应头之前：失败能以明确的 HTTP 状态码返回（503 限流 / 502 其它），
	// 而不是发出 200/206 后掐断连接。首段三种形态：
	//   a) 起点在块中间且未缓存 → ≤128KB 快启段（不入缓存）；
	//   b) 已缓存 → 直接从缓存切片（零网络请求）；
	//   c) 块首起点 → 整块取（入内存+磁盘缓存）。
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
				// 账号级限流：切线路/重试都没用，把等待秒数交给前端做倒计时
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
				break // 限流：跳出循环走 503 + Retry-After
			}
			if !waitCtx(ctx, time.Duration(300*(1<<attempt))*time.Millisecond) {
				return
			}
		}
		if err != nil {
			if errors.As(err, &fw) {
				// 账号级限流：503 + Retry-After，前端倒计时
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
	// 播放流开始：后台封面/预览图下载退让，把带宽与限流额度让给播放
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
	// ETag 必须带上文档 ID：同一来源切清晰度时 URL 可能只差参数，
	// 浏览器若按 URL 命中 1 小时缓存就会继续放旧清晰度的字节。
	h.Set("ETag", fmt.Sprintf("\"src-%d-%d-%d\"", src.ID, src.DocID, size))
	// 同一集内容不会变：允许播放器缓存，重播/后退时直接命中，避免重复向 Telegram 拉流
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
	// 首段最先写给播放器（快启时 ≤128KB，首帧不等整块）
	if len(head) > 0 {
		if _, err := w.Write(head); err != nil {
			return
		}
		if flusher != nil {
			flusher.Flush()
		}
	}

	// 剩余区间交给按字节驱动的块循环（首段已覆盖的部分不再重复）
	if pos <= end {
		if err := s.streamRange(ctx, w, flusher, st, size, pos, end); err != nil {
			// 响应头已提交，只能中断连接让前端切换线路
			return
		}
	}
}

// streamRange 按块序把 [firstIdx, lastIdx] 写入响应：顺序读取、顺序写出，
// 写块的同时预读下一块（prefetchNext）。
// Telegram Web 端对同一文件的并发 upload.getFile 既不会提速，又容易触发风控限流，
// 因此有效并发仍为 1：预读协程占用同一个全局信号量，主循环到下一块时直接命中缓存。
// 这把「取块 RTT」与「写块+浏览器消费」重叠起来，高 RTT 代理下连续播放不再逐块等往返。
// streamRange 把 [start, end] 按块写给浏览器：按字节驱动，
// 每次取数完整落在单个 1MB 窗口内（官方硬性规则）。
//   - 起点在块中间：先拉 ≤128KB 快启段（不入缓存）出画，再拉块内剩余段；
//   - 块首起点：整块取（入内存+磁盘缓存），按需裁剪写入；
//   - 已缓存的块直接从缓存写切片；
//   - 写完当前块后预取下一块（有效并发仍为 1：预读占用同一全局信号量）。
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
		// 本轮要写出的字节区间 [pos, writeEnd)
		writeEnd := bEnd
		if end < writeEnd-1 {
			writeEnd = end + 1
		}
		from := int(pos - bOff)
		to := int(writeEnd - bOff)

		// 已缓存：直接从缓存写切片（不重复拉取）
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
			// 块首起点：整块取（内存+磁盘缓存），写需要的切片
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

		// 块中间起点：先拉 ≤128KB 快启段出画（不入缓存），再拉块内剩余段
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

// prefetchNext 预取下一块进缓存（内存+磁盘），让下一轮整块取直接命中。
// 仅当已经写到当前块结尾（或接近结尾）时才发起——否则会插队到
// 当前块剩余段的取数之前，反而拖慢出流。
func (s *Server) prefetchNext(ctx context.Context, st *streamState, idx int64, size, pos, end int64) {
	bEnd := s.blockEnd(idx)
	if pos < bEnd && bEnd-pos > streamFastStart {
		return // 当前块还剩很多没写：下一轮仍在本块内
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

// blockCached 该块是否已在内存或磁盘缓存中（供快启/预读避免重复拉取）。
func (s *Server) blockCached(st *streamState, idx int64) bool {
	_, ok := s.cachedBlock(st, idx)
	return ok
}

// cachedBlock 从内存/磁盘缓存取整块数据（不发起网络请求）。nil 磁盘安全。
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

// cdnRedirectErr 主 DC 返回 upload.fileCdnRedirect：文件改由 CDN DC 分发。
// 由 fetchChunkWithCDN 捕获并切换到 CDN 取流路径。
type cdnRedirectErr struct {
	redirect *tgclient.CDNRedirect
}

func (e *cdnRedirectErr) Error() string { return "文件由 CDN DC 分发" }

// fetchChunkWithCDN 任意区间的块读取统一入口：已进入 CDN 模式走 CDN 取流；
// 主 DC 返回 CDN 重定向时初始化 CDN 会话并重试同一区间。
func (s *Server) fetchChunkWithCDN(ctx context.Context, st *streamState,
	src *storage.Source, offset int64, limit int) ([]byte, *storage.Source, error) {

	// CDN 模式：令牌失效等错误回退到主 DC 重新走重定向（≤2 次）
	for cdnAttempt := 0; cdnAttempt < 2 && st.cdn != nil; cdnAttempt++ {
		data, err := s.svc.CDNChunk(ctx, st.cdn, src, offset, limit)
		if err == nil {
			return data, src, nil
		}
		if errors.Is(err, tgclient.ErrCDNTokenInvalid) {
			// 令牌过期：清掉会话，主 DC 会重新下发重定向
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
		// 首次发现 CDN 分发：初始化会话并立刻用 CDN 取同一区间
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

// fetchChunkUncached 快启/续播残段路径：绕过块缓存直接拉一小段。
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

// fetchWholeBlock 取整个块（内存→磁盘→Telegram），成功后写入内存与磁盘缓存。
// 通过 chunks.fetch 合并同块的并发读取（单一信号量 + 看门狗语义保持不变）。
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

// refreshStreamSource 刷新正在播放的来源引用。清晰度变体用「负的来源 ID」表示
// （ID = -variantID），与真实 sources 行区分；两者引用各自独立过期。
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

// fetchChunkFromTelegram 默认的块读取实现：调用 upload.getFile。
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

// fetchFileChunk 读取指定偏移的一段数据；file_reference 过期时自动刷新重试。
// Telegram 要求 offset/limit 按 4096 对齐，因此向低对齐发起请求再裁掉多余前缀。
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
			// Precise 关闭 offset/limit 对齐检查，拖到关键帧中间时也能正常取到数据
			Precise: true,
			Location: &tg.InputDocumentFileLocation{
				ID: src.DocID, AccessHash: src.AccessHash,
				FileReference: src.FileReference,
			},
			Offset: alignedOffset,
			Limit:  int(alignedLimit),
		}
		req.SetFlags()
		// 单次请求硬超时：挂死的请求不能占住全局读取信号量，否则所有播放都会卡死
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
				// CDN 分发：交给 CDN 取流器（CDN DC 连接/分片/AES-CTR/哈希校验）
				return nil, src, &cdnRedirectErr{redirect: tgclient.NewCDNRedirect(f)}
			default:
				return nil, src, fmt.Errorf("未知响应类型 %T", res)
			}
		}
		lastErr = err

		if d, ok := tgerr.AsFloodWait(err); ok {
			if d > streamFloodWaitCap {
				// 账号级限流：包成 floodWaitErr 让 HTTP 层转成 503 + Retry-After，
				// 前端做倒计时，而不是走「播不了」错误态。
				return nil, src, &floodWaitErr{wait: int64(d.Seconds()), err: err}
			}
			if !waitCtx(ctx, d+time.Second) {
				return nil, src, ctx.Err()
			}
			continue
		}
		if isFileRefError(err) && s.svc != nil {
			// 刷新 file_reference 也要限时：它同样占用全局信号量
			rctx, rcancel := context.WithTimeout(ctx, s.chunkTimeout)
			refreshed, rerr := s.refreshStreamSource(rctx, src)
			rcancel()
			if rerr != nil {
				return nil, src, rerr
			}
			src = refreshed
			continue
		}
		// 其余错误按瞬时故障处理：Telegram 偶发的连接重置/超时/未成文 RPC 错误
		// 退避后重试，比直接掐断浏览器连接(用户看到「播不了」)友好得多。
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

// isAuthError 判断是否为未登录/会话失效类错误
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

// isFileRefError 判断是否为 file_reference 失效
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

// parseRangeHeader 解析 HTTP Range；返回闭区间 [start, end] 与是否为范围请求。
// 支持 bytes=start-end / bytes=start- / bytes=-suffix。
func parseRangeHeader(header string, size int64) (start, end int64, ranged bool, err error) {
	header = strings.TrimSpace(header)
	if header == "" {
		return 0, size - 1, false, nil
	}
	if !strings.HasPrefix(header, "bytes=") {
		return 0, 0, false, errors.New("不支持的 Range 单位")
	}
	// 只处理单段范围（浏览器播放器始终使用单段）
	spec := strings.TrimSpace(strings.TrimPrefix(header, "bytes="))
	if i := strings.IndexByte(spec, ','); i >= 0 {
		spec = strings.TrimSpace(spec[:i])
	}
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false, errors.New("Range 格式错误")
	}
	if parts[0] == "" {
		// bytes=-N 末尾 N 字节
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
