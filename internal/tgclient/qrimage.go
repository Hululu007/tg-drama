package tgclient

import (
	"context"
	"errors"
	"sync"
	"time"

	"rsc.io/qr"
)

var ErrQRNotReady = errors.New("二维码尚未就绪")

var (
	qrPNGMu    sync.Mutex
	qrPNGToken string
	qrPNGBuf   []byte
	qrPNGAt    time.Time
)

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
	code.Scale = 8
	png := code.PNG()
	qrPNGToken, qrPNGBuf, qrPNGAt = link, png, time.Now()
	return qrPNGBuf, nil
}
