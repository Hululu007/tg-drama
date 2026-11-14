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

var ErrCDNTokenInvalid = errors.New("CDN 令牌失效")

type CDNRedirect struct {
	DCID      int
	FileToken []byte
	Key       [32]byte
	IV        [16]byte
	Hashes    []tg.FileHash
}

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

const (
	cdnMinChunk = 4 * 1024
	cdnMaxChunk = 1024 * 1024
	cdnRetries  = 3
)

type cdnRequestRange struct {
	offset int64
	limit  int
}

func largestCDNValidLimit(max int) int {
	for size := max; size >= cdnMinChunk; size -= cdnMinChunk {
		if cdnMaxChunk%size == 0 {
			return size
		}
	}
	return 0
}

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

func (s *Service) CDNChunk(ctx context.Context, red *CDNRedirect,
	src *storage.Source, offset int64, limit int) ([]byte, error) {

	if red == nil {
		return nil, ErrCDNTokenInvalid
	}
	if offset%cdnMinChunk != 0 || limit%cdnMinChunk != 0 {

		return nil, fmt.Errorf("CDN 分片未对齐 offset=%d limit=%d", offset, limit)
	}
	api := s.API()
	if api == nil {
		return nil, ErrNotAuthorized
	}

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
			break
		}
	}

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
