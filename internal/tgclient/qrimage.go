package tgclient

import (
	"context"
	"errors"
	"sync"
	"time"

	"rsc.io/qr"
)

// ErrQRNotReady 二维码尚未生成（还在连接 Telegram / 等待 token 导出）。
var ErrQRNotReady = errors.New("二维码尚未就绪")

// qrPNG 缓存当前 token 对应的 PNG：前端每 2 秒轮询一次，
// 同一个 token 反复编码纯浪费；token 轮换（过期重导出）后自动失效。
var (
	qrPNGMu    sync.Mutex
	qrPNGToken string
	qrPNGBuf   []byte
	qrPNGAt    time.Time
)

// QrImage 返回当前登录二维码的 PNG 字节（放大 8 倍保证浏览器里清晰）。
func (s *Service) QrImage(ctx context.Context) ([]byte, error) {
	s.authMu.Lock()
	token := s.qrToken
	s.authMu.Unlock()
	if token.Empty() {
		return nil, ErrQRNotReady
	}
	link := token.URL()

	qrPNGMu.Lock()
	defer qrPNGMu.Unlock()
	if qrPNGBuf != nil && qrPNGToken == link && time.Since(qrPNGAt) < time.Minute {
		return qrPNGBuf, nil
	}
	code, err := qr.Encode(link, qr.M)
	if err != nil {
		return nil, err
	}
	code.Scale = 8 // 1 模块 8 像素，浏览器里清晰
	png := code.PNG()
	qrPNGToken, qrPNGBuf, qrPNGAt = link, png, time.Now()
	return qrPNGBuf, nil
}
