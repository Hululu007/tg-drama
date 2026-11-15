package web

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"
)

// diskCache 视频块的磁盘缓存：边播边落盘，带位图索引（index.json）。
// 重启后索引仍有效；整集缓存完成后重播 = 本地播放（不再访问 Telegram）。
//
// 目录结构：data/mediacache/doc_<docID>/
//
//	├── b<idx>.bin    块数据（布局与内存一致：块 0 = 128KB 头块，其后 1MB）
//	└── index.json    位图索引（块序号 → 字节数、file_size、访问时间）
type diskCache struct {
	dir      string
	quota    int64 // 字节；0 = 关闭
	mu       sync.Mutex
	indexes  map[int64]*diskIndex // docID → 索引（懒加载）
	usage    int64                // 当前占用（增量维护）
	minFlush time.Duration        // 索引落盘最小间隔（避免每块都写索引）
}

// diskIndex 单个来源文档的位图索引。
type diskIndex struct {
	Version  int             `json:"v"`
	SourceID int64           `json:"source"`
	DocID    int64           `json:"doc"`
	FileSize int64           `json:"file_size"`
	HeadSize int64           `json:"head_size"`
	MaxBlock int64           `json:"block_size"`
	Blocks   map[int64]int64 `json:"blocks"` // 块序号 → 实际字节数
	Updated  int64           `json:"updated"`
	Accessed int64           `json:"accessed"`
}

const diskIndexVersion = 1

func newDiskCache(dir string, quotaBytes int64) *diskCache {
	if dir == "" || quotaBytes <= 0 {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	d := &diskCache{
		dir:      dir,
		quota:    quotaBytes,
		indexes:  map[int64]*diskIndex{},
		minFlush: 5 * time.Second,
	}
	d.prune()
	return d
}

func (d *diskCache) docDir(docID int64) string {
	return filepath.Join(d.dir, fmt.Sprintf("doc_%d", docID))
}

func (d *diskCache) blockFile(docID int64, idx int64) string {
	return filepath.Join(d.docDir(docID), fmt.Sprintf("b%d.bin", idx))
}

// loadIndex 读取（或惰性初始化）文档的位图索引。nil 接收者安全（缓存未启用）。
func (d *diskCache) loadIndex(docID int64) *diskIndex {
	if d == nil {
		return &diskIndex{Version: diskIndexVersion, Blocks: map[int64]int64{}}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.loadIndexLocked(docID)
}

func (d *diskCache) loadIndexLocked(docID int64) *diskIndex {
	if idx, ok := d.indexes[docID]; ok {
		return idx
	}
	idx := &diskIndex{
		Version: diskIndexVersion, DocID: docID,
		HeadSize: 0, MaxBlock: 0,
		Blocks: map[int64]int64{},
	}
	if raw, err := os.ReadFile(filepath.Join(d.docDir(docID), "index.json")); err == nil {
		var parsed diskIndex
		if json.Unmarshal(raw, &parsed) == nil && parsed.Version == diskIndexVersion && parsed.DocID == docID {
			idx = &parsed
			if idx.Blocks == nil {
				idx.Blocks = map[int64]int64{}
			}
		}
	}
	d.indexes[docID] = idx
	for _, size := range idx.Blocks {
		d.usage += size
	}
	return idx
}

// flushIndexLocked 原子写回位图索引（节流：距上次落盘不足 minFlush 时跳过，
// 但内存里的状态始终是最新的——进程内断电丢失一个索引条目的代价只是重下几块）。
func (d *diskCache) flushIndexLocked(docID int64, force bool) {
	idx := d.indexes[docID]
	if idx == nil {
		return
	}
	now := time.Now().Unix()
	if !force && now-idx.Updated < int64(d.minFlush.Seconds()) {
		return
	}
	idx.Updated = now
	idx.Accessed = now
	raw, err := json.Marshal(idx)
	if err != nil {
		return
	}
	dir := d.docDir(docID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	tmp := filepath.Join(dir, "index.json.tmp")
	if os.WriteFile(tmp, raw, 0o644) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, "index.json"))
	}
}

// hasBlock 该块是否已在磁盘上（大小与索引一致）。nil 接收者安全。
func (d *diskCache) hasBlock(docID, idx int64) bool {
	if d == nil {
		return false
	}
	idxDoc := d.loadIndex(docID)
	d.mu.Lock()
	defer d.mu.Unlock()
	size, ok := idxDoc.Blocks[idx]
	return ok && size > 0
}

// serveBlock 从磁盘读一个块；命中时刷新访问时间（节流）。nil 接收者安全。
func (d *diskCache) serveBlock(docID, idx int64) ([]byte, bool) {
	if d == nil {
		return nil, false
	}
	idxDoc := d.loadIndex(docID)
	d.mu.Lock()
	size, ok := idxDoc.Blocks[idx]
	d.mu.Unlock()
	if !ok || size <= 0 {
		return nil, false
	}
	data, err := os.ReadFile(d.blockFile(docID, idx))
	if err != nil || int64(len(data)) != size {
		// 索引说有但文件没了/坏了：剔除并落盘修正
		d.mu.Lock()
		delete(idxDoc.Blocks, idx)
		d.usage -= size
		d.mu.Unlock()
		d.flushIndexLocked(docID, true)
		return nil, false
	}
	d.mu.Lock()
	now := time.Now().Unix()
	if now-idxDoc.Accessed >= int64(d.minFlush.Seconds()) {
		idxDoc.Accessed = now
		d.flushIndexLocked(docID, false)
	}
	d.mu.Unlock()
	return data, true
}

// storeBlock 把一个块写入磁盘缓存并更新位图索引；随后检查配额淘汰。nil 接收者安全。
func (d *diskCache) storeBlock(docID, idx int64, data []byte, fileSize, sourceID int64) {
	if d == nil || len(data) <= 0 {
		return
	}
	idxDoc := d.loadIndex(docID)
	d.mu.Lock()
	if old, ok := idxDoc.Blocks[idx]; ok && old == int64(len(data)) {
		d.mu.Unlock()
		return // 已存在且大小一致
	}
	dir := d.docDir(docID)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		d.mu.Unlock()
		return
	}
	tmp := filepath.Join(dir, fmt.Sprintf("b%d.tmp", idx))
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		d.mu.Unlock()
		return
	}
	if err := os.Rename(tmp, d.blockFile(docID, idx)); err != nil {
		d.mu.Unlock()
		return
	}
	if old, ok := idxDoc.Blocks[idx]; ok {
		d.usage -= old
	}
	idxDoc.Blocks[idx] = int64(len(data))
	if idxDoc.FileSize == 0 && fileSize > 0 {
		idxDoc.FileSize = fileSize
	}
	if idxDoc.SourceID == 0 && sourceID > 0 {
		idxDoc.SourceID = sourceID
	}
	if idxDoc.HeadSize == 0 && idx == 0 {
		idxDoc.HeadSize = int64(len(data))
	}
	if idxDoc.MaxBlock == 0 && idx > 0 {
		idxDoc.MaxBlock = int64(len(data))
	}
	d.usage += int64(len(data))
	// 索引立即落盘：块文件已存在而索引缺失会导致重启后重下该块。
	// index.json 只有几 KB，每块写一次的开销可以忽略。
	d.flushIndexLocked(docID, true)
	d.mu.Unlock()
	d.evictIfNeeded()
}

// evictIfNeeded 总量超过配额时按「最久未访问的来源文档」整组淘汰。
func (d *diskCache) evictIfNeeded() {
	if d.quota <= 0 || d.usage <= d.quota {
		return
	}
	d.mu.Lock()
	type entry struct {
		docID int64
		at    int64
	}
	var docs []entry
	for docID, idx := range d.indexes {
		docs = append(docs, entry{docID, idx.Accessed})
	}
	d.mu.Unlock()
	sort.Slice(docs, func(i, j int) bool { return docs[i].at < docs[j].at })
	for _, e := range docs {
		if d.usage <= d.quota*9/10 {
			break
		}
		d.dropDoc(e.docID)
	}
}

// dropDoc 删除一个来源文档的全部磁盘缓存。
func (d *diskCache) dropDoc(docID int64) {
	d.mu.Lock()
	idx := d.indexes[docID]
	if idx != nil {
		for _, size := range idx.Blocks {
			d.usage -= size
		}
	}
	delete(d.indexes, docID)
	d.mu.Unlock()
	_ = os.RemoveAll(d.docDir(docID))
}

// prune 启动时清理：删除没有有效索引的目录、索引里已不存在的块文件。
func (d *diskCache) prune() {
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) < 4 || e.Name()[:4] != "doc_" {
			continue
		}
		dir := filepath.Join(d.dir, e.Name())
		raw, err := os.ReadFile(filepath.Join(dir, "index.json"))
		if err != nil {
			_ = os.RemoveAll(dir)
			continue
		}
		var idx diskIndex
		if json.Unmarshal(raw, &idx) != nil || idx.Version != diskIndexVersion {
			_ = os.RemoveAll(dir)
			continue
		}
		files, _ := os.ReadDir(dir)
		for _, f := range files {
			name := f.Name()
			if name == "index.json" {
				continue
			}
			var blockIdx int64
			if _, err := fmt.Sscanf(name, "b%d.bin", &blockIdx); err != nil {
				_ = os.Remove(filepath.Join(dir, name))
				continue
			}
			if _, ok := idx.Blocks[blockIdx]; !ok {
				_ = os.Remove(filepath.Join(dir, name))
			}
		}
	}
}

// usage 当前磁盘占用（字节与文件数，实时统计）。
func (d *diskCache) usageStats() (int64, int) {
	var total int64
	var files int
	entries, err := os.ReadDir(d.dir)
	if err != nil {
		return 0, 0
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		sub, err := os.ReadDir(filepath.Join(d.dir, e.Name()))
		if err != nil {
			continue
		}
		for _, f := range sub {
			if info, err := f.Info(); err == nil {
				total += info.Size()
				files++
			}
		}
	}
	return total, files
}

// clear 清空全部视频块缓存（正在播放的流会失败一次并自动回退 Telegram 重取）。
func (d *diskCache) clear() {
	d.mu.Lock()
	d.indexes = map[int64]*diskIndex{}
	d.usage = 0
	d.mu.Unlock()
	_ = os.RemoveAll(d.dir)
	_ = os.MkdirAll(d.dir, 0o755)
}
