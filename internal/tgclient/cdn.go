package tgclient

import (
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"

	"tgdrama/internal/storage"
)

// ErrCDNTokenInvalid CDN 令牌失效（FILE_TOKEN_INVALID / REQUEST_TOKEN_INVALID）：
// 需要回到主 DC 重新走 upload.fileCdnRedirect 拿新令牌。
var ErrCDNTokenInvalid = errors.New("CDN 令牌失效")

// CDNRedirect 主 DC upload.getFile 返回的 CDN 重定向会话。
// 文件由 CDN DC 分发：字节需要用密钥/IV 做 AES-256-CTR 解密，分片从 CDN DC 读取，
// 哈希从主 DC 获取并逐窗校验。
type CDNRedirect struct {
	DCID      int
	FileToken []byte
	Key       [32]byte
	IV        [16]byte
	Hashes    []tg.FileHash
}

// NewCDNRedirect 从 upload.fileCdnRedirect 构造会话。
func NewCDNRedirect(r *tg.UploadFileCDNRedirect) *CDNRedirect {
	if r == nil {
		return nil
	}
	out := &CDNRedirect{
		DCID:      r.DCID,
		FileToken: r.FileToken,
		Hashes:    r.FileHashes,
	}
	copy(out.Key[:], r.EncryptionKey)
	copy(out.IV[:], r.EncryptionIv)
	return out
}

// cdnRequestLimits 与 gotd downloader 相同的 CDN 分片规则（官方 CDN 文档）：
// offset/limit 必须被 4KB 整除，单请求不超过 1MB 且落在 1MB 窗口内。
const (
	cdnMinChunk = 4 * 1024
	cdnMaxChunk = 1024 * 1024
	cdnRetries  = 3
)

type cdnRequestRange struct {
	offset int64
	limit  int
}

// largestCDNValidLimit 不超过 max、能整除 1MB 且被 4KB 整除的最大分片。
func largestCDNValidLimit(max int) int {
	for size := max; size >= cdnMinChunk; size -= cdnMinChunk {
		if cdnMaxChunk%size == 0 {
			return size
		}
	}
	return 0
}

// buildCDNRequestPlan 把 [offset, offset+limit) 拆成合法的 CDN 请求序列。
func buildCDNRequestPlan(offset int64, limit int) ([]cdnRequestRange, error) {
	if limit <= 0 || offset < 0 || offset%cdnMinChunk != 0 || limit%cdnMinChunk != 0 {
		return nil, fmt.Errorf("CDN 请求参数非法 offset=%d limit=%d", offset, limit)
	}
	remaining := limit
	current := offset
	plan := make([]cdnRequestRange, 0, 1+limit/cdnMaxChunk)
	for remaining > 0 {
		mbLeft := cdnMaxChunk - int(current%cdnMaxChunk)
		maxForStep := remaining
		if maxForStep > mbLeft {
			maxForStep = mbLeft
		}
		step := largestCDNValidLimit(maxForStep)
		if step == 0 {
			return nil, fmt.Errorf("CDN 请求计划失败 offset=%d limit=%d", offset, limit)
		}
		plan = append(plan, cdnRequestRange{offset: current, limit: step})
		current += int64(step)
		remaining -= step
	}
	return plan, nil
}

// decrypt 解密 CDN 分片。官方规则（core.telegram.org/cdn#decrypting-files）：
// AES-256-CTR，IV 的后 4 字节（big endian）替换为 offset/16。
func decryptCDNChunk(src []byte, offset int64, red *CDNRedirect) ([]byte, error) {
	block, err := aes.NewCipher(red.Key[:])
	if err != nil {
		return nil, fmt.Errorf("CDN 解密创建 cipher: %w", err)
	}
	iv := make([]byte, len(red.IV))
	copy(iv, red.IV[:])
	binary.BigEndian.PutUint32(iv[len(iv)-4:], uint32(offset/16))
	dst := make([]byte, len(src))
	cipher.NewCTR(block, iv).XORKeyStream(dst, src)
	return dst, nil
}

// hashFor 取覆盖 offset 窗口的哈希（窗口 128KB，与 Telegram CDN 哈希窗口一致）。
// 窗口起点必须是窗口大小的倍数，所以按窗口起点查找。
func hashFor(hashes []tg.FileHash, offset int64) (tg.FileHash, bool) {
	window := int64(128 * 1024)
	start := offset - offset%window
	for _, h := range hashes {
		if h.Offset == start || h.Offset == offset {
			return h, true
		}
	}
	return tg.FileHash{}, false
}

// CDNChunk 从 CDN DC 读取一段已解密、已校验的文件数据。
// src 仅用于哈希校验失败时的日志定位；令牌失效返回 ErrCDNTokenInvalid。
func (s *Service) CDNChunk(ctx context.Context, red *CDNRedirect,
	src *storage.Source, offset int64, limit int) ([]byte, error) {

	if red == nil {
		return nil, ErrCDNTokenInvalid
	}
	if offset%cdnMinChunk != 0 || limit%cdnMinChunk != 0 {
		// 调用方传入的块偏移/长度必须与 CDN 规则对齐（块布局已保证）
		return nil, fmt.Errorf("CDN 分片未对齐 offset=%d limit=%d", offset, limit)
	}
	api := s.API()
	if api == nil {
		return nil, ErrNotAuthorized
	}
	// CDN DC 连接（含官方 CDN 授权握手），gotd 在 client 级按 DC 缓存连接池
	invoker, err := s.client.CDN(ctx, red.DCID, 2)
	if err != nil {
		return nil, fmt.Errorf("连接 CDN DC %d: %w", red.DCID, err)
	}
	cdnAPI := tg.NewClient(invoker)

	plan, err := buildCDNRequestPlan(offset, limit)
	if err != nil {
		return nil, err
	}
	hashes := append([]tg.FileHash(nil), red.Hashes...)

	var out []byte
	for _, req := range plan {
		data, err := s.cdnRequestChunk(ctx, cdnAPI, api, red, req.offset, req.limit, hashes)
		if err != nil {
			return nil, err
		}
		out = append(out, data...)
		if len(data) < req.limit {
			break // 到达文件尾
		}
	}

	// 哈希校验：解密后的每个 128KB 窗口必须与 Telegram 给出的 sha256 一致
	for wStart := offset - offset%(128*1024); wStart < offset+int64(len(out)); wStart += 128 * 1024 {
		wEnd := wStart + 128*1024
		if wEnd > offset+int64(len(out)) {
			wEnd = offset + int64(len(out))
		}
		from := wStart - offset
		to := wEnd - offset
		if from < 0 || to > int64(len(out)) || to <= from {
			continue
		}
		h, ok := hashFor(hashes, wStart)
		if !ok {
			// 向主 DC 补取该窗口的哈希
			fetched, err := api.UploadGetCDNFileHashes(ctx, &tg.UploadGetCDNFileHashesRequest{
				FileToken: red.FileToken, Offset: wStart,
			})
			if err != nil {
				return nil, fmt.Errorf("获取 CDN 哈希 offset=%d: %w", wStart, err)
			}
			hashes = append(hashes, fetched...)
			h, ok = hashFor(hashes, wStart)
			if !ok {
				return nil, fmt.Errorf("CDN 哈希缺失 offset=%d", wStart)
			}
		}
		sum := sha256.Sum256(out[from:to])
		if !bytes.Equal(sum[:], h.Hash) {
			return nil, fmt.Errorf("CDN 分片哈希不匹配 offset=%d len=%d", from+offset, to-from)
		}
	}
	return out, nil
}

// cdnRequestChunk 单个分片子请求：读 CDN、解密、处理令牌失效/需要重传。
func (s *Service) cdnRequestChunk(ctx context.Context, cdnAPI, master *tg.Client,
	red *CDNRedirect, offset int64, limit int, hashes []tg.FileHash) ([]byte, error) {

	var lastErr error
	for attempt := 0; attempt < cdnRetries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		res, err := cdnAPI.UploadGetCDNFile(ctx, &tg.UploadGetCDNFileRequest{
			FileToken: red.FileToken,
			Offset:    offset,
			Limit:     limit,
		})
		if err != nil {
			if d, ok := tgerr.AsFloodWait(err); ok {
				if !waitCtx(ctx, d+time.Second) {
					return nil, ctx.Err()
				}
				continue
			}
			if tgerr.Is(err, "FILE_TOKEN_INVALID", "REQUEST_TOKEN_INVALID") {
				return nil, ErrCDNTokenInvalid
			}
			lastErr = err
			if attempt < cdnRetries-1 {
				time.Sleep(time.Duration(300*(1<<attempt)) * time.Millisecond)
			}
			continue
		}
		switch typed := res.(type) {
		case *tg.UploadCDNFile:
			return decryptCDNChunk(typed.Bytes, offset, red)
		case *tg.UploadCDNFileReuploadNeeded:
			// 该窗口已被 CDN 丢弃：请主 DC 重新上传后重试
			fetched, rerr := master.UploadReuploadCDNFile(ctx, &tg.UploadReuploadCDNFileRequest{
				FileToken:    red.FileToken,
				RequestToken: typed.RequestToken,
			})
			if rerr == nil {
				for _, h := range fetched {
					if _, dup := hashFor(hashes, h.Offset); !dup {
						hashes = append(hashes, h)
					}
				}
				red.Hashes = hashes
			}
			continue
		default:
			return nil, fmt.Errorf("CDN 未知响应类型 %T", res)
		}
	}
	if lastErr == nil {
		lastErr = errors.New("CDN 分片重试次数用尽")
	}
	return nil, lastErr
}

// waitCtx 等待 d 或 ctx 结束（web 包有同名实现，这里复制一份避免反向依赖）。
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
