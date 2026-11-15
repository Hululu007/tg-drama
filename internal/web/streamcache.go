package web

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

// chunkKey 视频块缓存的键：来源 ID + 文档 ID + 块序号。
// 同一集切清晰度时来源行相同、文档不同（alt_documents），必须把 doc 并进键里，
// 否则 720p 会命中 1080p 的块。
type chunkKey struct {
	source int64
	doc    int64
	index  int64
}

// chunkEntry LRU 链表里的一个块。
type chunkEntry struct {
	key  chunkKey
	data []byte
	// pinned 的头部块不参与 LRU 淘汰：换清晰度/换线路/重试都会重新拉 moov，
	// 头两块固定留在内存里，起播省 1~3 个 RTT。
	pinned bool
}

// chunkCall 同一块的并发合并：多个 HTTP 请求同时要同一块时只向 Telegram 发一次请求。
type chunkCall struct {
	done chan struct{}
	data []byte
	err  error
	// semReleased 保证全局信号量只被归还一次（正常完成 / 看门狗放弃二选一），
	// 由 c.mu 保护。
	semReleased bool
}

// chunkResult fn 协程带回的读取结果
type chunkResult struct {
	data []byte
	err  error
}

// streamCache 在线播放的块缓存：
//   - LRU 限制内存占用，拖动进度条回到已看过的位置时直接命中；
//   - 同一块正在读取时其他请求等待同一个结果，避免重复消耗 Telegram 连接；
//   - 全局信号量固定为 1：任何时刻最多只有一个 upload.getFile 在飞，避免 TG Web 端风控限流。
type streamCache struct {
	chunkSize int64
	maxBytes  int64

	mu            sync.Mutex
	lru           *list.List
	items         map[chunkKey]*list.Element
	size          int64
	inflight      map[chunkKey]*chunkCall
	pinnedSources map[sourceDocKey]struct{}

	sem chan struct{}
}

func newStreamCache(chunkSize, maxBytes int64) *streamCache {
	if chunkSize <= 0 {
		chunkSize = 256 * 1024
	}
	if maxBytes < chunkSize {
		maxBytes = chunkSize
	}
	return &streamCache{
		chunkSize:     chunkSize,
		maxBytes:      maxBytes,
		lru:           list.New(),
		items:         make(map[chunkKey]*list.Element),
		inflight:      make(map[chunkKey]*chunkCall),
		pinnedSources: make(map[sourceDocKey]struct{}),
		// 单线程：并发播放/拖动也排队，绝不同时向 Telegram 发多个块请求
		sem: make(chan struct{}, 1),
	}
}

// get 命中时把块移到队尾（最近使用）。
func (c *streamCache) get(k chunkKey) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	c.lru.MoveToBack(el)
	return el.Value.(*chunkEntry).data, true
}

// peek 只查缓存不更新 LRU 顺序：预读前先看下一块是否已存在，避免重复发起。
func (c *streamCache) peek(k chunkKey) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	return el.Value.(*chunkEntry).data, true
}

// put 写入缓存并按 LRU 淘汰多余内存。单块超过上限时不缓存，避免刚放进就被淘汰。
// index < pinHeadBlocks 的头部块标记 pinned：不淘汰，但计入 size（超限时优先淘汰
// 其它 pinned 之外的老块，pinned 集合由 maxPinnedSources 限制）。
func (c *streamCache) put(k chunkKey, data []byte) {
	if int64(len(data)) > c.maxBytes {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		c.lru.MoveToBack(el)
		return
	}
	pin := k.index >= 0 && k.index < pinHeadBlocks
	// pinned 容量控制：同一来源的头部块一并失效，避免集合无限增长。
	if pin {
		c.pinSource(k.source, k.doc)
		c.pinnedSources[sourceDocKey{source: k.source, doc: k.doc}] = struct{}{}
	}
	el := c.lru.PushBack(&chunkEntry{key: k, data: data, pinned: pin})
	c.items[k] = el
	c.size += int64(len(data))
	c.evict(el)
}

// sourceDocKey pinned 容量管理的单位：一个来源的一个文档。
type sourceDocKey struct {
	source int64
	doc    int64
}

// pinHeadBlocks 每个来源固定缓存的头部块数（moov 通常在前 1MB 内）。
const pinHeadBlocks = 2

// maxPinnedSources 最多固定缓存的来源文档数；超限时踢掉最旧的一组头部块。
const maxPinnedSources = 8

// pinSource 超过 pinned 来源上限时，删除最旧来源组（LRU 队首方向第一个
// pinned 来源）的全部头部块，给新来源腾出固定缓存的位子。
func (c *streamCache) pinSource(source, doc int64) {
	if len(c.pinnedSources) < maxPinnedSources {
		return
	}
	if c.pinnedSources[sourceDocKey{source: source, doc: doc}] != struct{}{} {
		return // 已存在，不扩容
	}
	victim := sourceDocKey{}
	found := false
	for el := c.lru.Front(); el != nil; el = el.Next() {
		ent := el.Value.(*chunkEntry)
		if !ent.pinned {
			continue
		}
		key := sourceDocKey{source: ent.key.source, doc: ent.key.doc}
		if key == (sourceDocKey{source: source, doc: doc}) {
			return
		}
		victim, found = key, true
		break
	}
	if !found {
		return
	}
	// 先收集 victim 组的元素再统一删除（Remove 会使 Next 失效，不能边走边删）
	var doomed []*list.Element
	for el := c.lru.Front(); el != nil; el = el.Next() {
		ent := el.Value.(*chunkEntry)
		if ent.pinned && ent.key.source == victim.source && ent.key.doc == victim.doc {
			doomed = append(doomed, el)
		}
	}
	for _, el := range doomed {
		ent := el.Value.(*chunkEntry)
		delete(c.items, ent.key)
		c.size -= int64(len(ent.data))
		c.lru.Remove(el)
	}
	delete(c.pinnedSources, victim)
}

// evict 按 LRU 淘汰到限额内；pinned 块在内存超限时也可以被淘汰
// （只是不因「变旧」而淘汰——超限是硬约束，pinned 让位于不超限）。
func (c *streamCache) evict(newest *list.Element) {
	for c.size > c.maxBytes && c.lru.Len() > 1 {
		old := c.lru.Front()
		if old == newest {
			break
		}
		// 先淘汰非 pinned 的；全剩 pinned 且仍超限时淘汰最旧的 pinned
		var victim *list.Element
		for el := old; el != nil; el = el.Next() {
			if el == newest {
				break
			}
			if !el.Value.(*chunkEntry).pinned {
				victim = el
				break
			}
		}
		if victim == nil {
			victim = old // 全是 pinned：退化为普通 LRU
		}
		ent := victim.Value.(*chunkEntry)
		if ent.pinned {
			delete(c.pinnedSources, sourceDocKey{source: ent.key.source, doc: ent.key.doc})
		}
		c.lru.Remove(victim)
		delete(c.items, ent.key)
		c.size -= int64(len(ent.data))
	}
}

// abandonTimeout 等待单块读取完成的上限：超过后看门狗放弃本次等待，
// 直接以错误返回调用方（播放继续重试），fn 协程留在后台慢慢收尾。
// 没有这一层时，fn 若不响应 ctx 取消（例如 gotd RPC 在服务端 ACK 丢失后
// 挂死在等待响应），全局信号量会被永久占住，所有播放请求排死队——
// 这就是「一直缓冲中、重试也无效」的根因。
const abandonTimeout = 30 * time.Second

// releaseSem 归还全局信号量，且保证只归还一次（看门狗与 fn 完成路径竞用时，
// 谁先拿到 mu 谁负责释放，另一方直接跳过）。
func (c *streamCache) releaseSem(call *chunkCall) {
	c.mu.Lock()
	if call.semReleased {
		c.mu.Unlock()
		return
	}
	call.semReleased = true
	c.mu.Unlock()
	<-c.sem
}

// fetch 取一块：先查缓存，再合并同一块的并发读取，最后在全局信号量内真正拉取。
// fn 只会在"当前调用负责发起"时执行一次，其余并发调用等待它的结果。
func (c *streamCache) fetch(ctx context.Context, k chunkKey, fn func(context.Context) ([]byte, error)) ([]byte, error) {
	if data, ok := c.get(k); ok {
		return data, nil
	}

	c.mu.Lock()
	if call, ok := c.inflight[k]; ok {
		c.mu.Unlock()
		select {
		case <-call.done:
			return call.data, call.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	call := &chunkCall{done: make(chan struct{})}
	c.inflight[k] = call
	c.mu.Unlock()

	select {
	case c.sem <- struct{}{}:
	case <-ctx.Done():
		c.releaseSem(call)
		c.finish(k, call, nil, ctx.Err())
		return nil, ctx.Err()
	}

	// fn 在独立协程里跑，主调用带看门狗等待：超时后主调用直接失败返回
	//（信号量同步归还），fn 的结果只进缓存、不再通知任何人。
	// 没有这一层时，fn 若不响应 ctx 取消（例如 gotd RPC 在服务端 ACK 丢失后
	// 挂死在等响应），全局信号量会被永久占住，所有播放请求排死队——
	// 这就是「一直缓冲中、重试也无效」的根因。
	resCh := make(chan chunkResult, 1)
	go func() {
		d, e := fn(ctx)
		resCh <- chunkResult{d, e}
	}()

	timer := time.NewTimer(abandonTimeout)
	defer timer.Stop()
	select {
	case res := <-resCh:
		c.releaseSem(call)
		c.finish(k, call, res.data, res.err)
		return res.data, res.err
	case <-ctx.Done():
		c.releaseSem(call)
		c.abandon(k, call, ctx.Err())
		return nil, ctx.Err()
	case <-timer.C:
		c.releaseSem(call)
		c.abandon(k, call, errAbandoned)
		// fn 可能还挂在网络上：等它结束后把结果写进缓存（若成功），
		// 之后的请求就能直接命中；结果失败则被丢弃。
		// 注意必须在 goroutine 内部接收 resCh：go f(<-ch) 的参数求值
		// 发生在当前协程，会在这里永久阻塞，看门狗就白做了。
		go func() {
			res := <-resCh
			c.releaseSem(call)
			if res.err == nil && len(res.data) > 0 {
				c.put(k, res.data)
			}
		}()
		return nil, errAbandoned
	}
}

// abandon 把等待中的调用标记为放弃：等待方拿到错误返回，
// inflight 槽位清掉（后续同块请求会重新发起，而不是等一个挂死的旧请求）。
func (c *streamCache) abandon(k chunkKey, call *chunkCall, err error) {
	c.mu.Lock()
	if cur, ok := c.inflight[k]; ok && cur == call {
		delete(c.inflight, k)
	}
	c.mu.Unlock()
	close(call.done)
}

// errAbandoned 看门狗放弃等待时返回给调用方的错误。
var errAbandoned = errors.New("块读取超时")

func (c *streamCache) finish(k chunkKey, call *chunkCall, data []byte, err error) {
	c.mu.Lock()
	call.data, call.err = data, err
	if cur, ok := c.inflight[k]; ok && cur == call {
		delete(c.inflight, k)
	}
	c.mu.Unlock()
	close(call.done)
	if err == nil && len(data) > 0 {
		c.put(k, data)
	}
}
