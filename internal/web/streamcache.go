package web

import (
	"container/list"
	"context"
	"errors"
	"sync"
	"time"
)

type chunkKey struct {
	source int64
	doc    int64
	index  int64
}

type chunkEntry struct {
	key  chunkKey
	data []byte

	pinned bool
}

type chunkCall struct {
	done chan struct{}
	data []byte
	err  error

	semReleased bool
}

type chunkResult struct {
	data []byte
	err  error
}

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

		sem: make(chan struct{}, 1),
	}
}

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

func (c *streamCache) peek(k chunkKey) ([]byte, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.items[k]
	if !ok {
		return nil, false
	}
	return el.Value.(*chunkEntry).data, true
}

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

	if pin {
		c.pinSource(k.source, k.doc)
		c.pinnedSources[sourceDocKey{source: k.source, doc: k.doc}] = struct{}{}
	}
	el := c.lru.PushBack(&chunkEntry{key: k, data: data, pinned: pin})
	c.items[k] = el
	c.size += int64(len(data))
	c.evict(el)
}

type sourceDocKey struct {
	source int64
	doc    int64
}

const pinHeadBlocks = 2

const maxPinnedSources = 8

func (c *streamCache) pinSource(source, doc int64) {
	if len(c.pinnedSources) < maxPinnedSources {
		return
	}
	if c.pinnedSources[sourceDocKey{source: source, doc: doc}] != struct{}{} {
		return
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

func (c *streamCache) evict(newest *list.Element) {
	for c.size > c.maxBytes && c.lru.Len() > 1 {
		old := c.lru.Front()
		if old == newest {
			break
		}

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
			victim = old
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

const abandonTimeout = 30 * time.Second

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

func (c *streamCache) abandon(k chunkKey, call *chunkCall, err error) {
	c.mu.Lock()
	if cur, ok := c.inflight[k]; ok && cur == call {
		delete(c.inflight, k)
	}
	c.mu.Unlock()
	close(call.done)
}

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
