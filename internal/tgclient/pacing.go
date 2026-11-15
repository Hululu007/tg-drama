package tgclient

import (
	"context"
	"sync"
	"time"
)

// pacer 一个「全局最小间隔」闸门：所有调用方排队通过，相邻两次放行至少相隔 gap。
//
// 为什么需要它：历史回填、封面回填、预览图预热、选集补全、推荐面板这些路径
// 各自都有并发，但 Telegram 只按账号算总速率。没有一个共享闸门时，一次翻几十页
// 历史 + 同时开着的 8 个下载协程会把请求打成一条密集的直线，这是最快撞上
// FLOOD_WAIT 的形态。这里不做精确令牌桶，只保证「每 gap 秒最多一个请求」，
// 对单账号足够，也不会因为突发配额算法复杂而写错。
//
// 零值可用（未 init 时 gap=0 即不限速），方便测试里直接构造 Service。
type pacer struct {
	mu   sync.Mutex
	next time.Time
}

// wait 阻塞到本次请求可以发出为止；ctx 取消时返回 false。
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

// jitter 在区间内取一个抖动值，避免多个协程的总间隔被算成整齐的等距节拍
// （等距节拍本身就比随机间隔更像脚本）。
func jitter(base, span time.Duration) time.Duration {
	if span <= 0 {
		return base
	}
	// 用纳秒时间的低位做伪随机源，省一个 math/rand 的全局锁
	n := time.Now().UnixNano() % int64(span)
	if n < 0 {
		n = -n
	}
	return base + time.Duration(n)
}

// 各路径的最小间隔。数值按「单账号每分钟几十次请求」估算：
// 一个频道回填上千页历史时约 4 页/秒，低于人工浏览一个频道的强度。
const (
	// historyPageGap 翻一页 messages.getHistory 的最小间隔（回填历史/补拉新消息）
	historyPageGap = 320 * time.Millisecond
	// warmBatchGap 两个「按剧批量取预览图」之间的最小间隔
	warmBatchGap = 700 * time.Millisecond
	// coverFetchGap 两张封面缩略图之间的最小间隔
	coverFetchGap = 450 * time.Millisecond
)
