package tgclient

import (
	"context"
	"sync"
	"time"
)

type pacer struct {
	mu   sync.Mutex
	next time.Time
}

func (p *pacer) wait(ctx context.Context, gap time.Duration) bool {
	if p == nil || gap <= 0 {
		return ctx.Err() == nil
	}
	now := time.Now()
	p.mu.Lock()
	at := p.next
	if at.Before(now) {
		at = now
	}
	p.next = at.Add(gap)
	p.mu.Unlock()

	d := at.Sub(now)
	if d <= 0 {
		return ctx.Err() == nil
	}
	return sleepCtx(ctx, d)
}

func jitter(base, span time.Duration) time.Duration {
	if span <= 0 {
		return base
	}

	n := time.Now().UnixNano() % int64(span)
	if n < 0 {
		n = -n
	}
	return base + time.Duration(n)
}

const (
	historyPageGap = 320 * time.Millisecond

	warmBatchGap = 700 * time.Millisecond

	coverFetchGap = 450 * time.Millisecond
)
