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

type diskCache struct {
	dir      string
	quota    int64
	mu       sync.Mutex
	indexes  map[int64]*diskIndex
	usage    int64
	minFlush time.Duration
}

type diskIndex struct {
	Version  int             `json:"v"`
	SourceID int64           `json:"source"`
	DocID    int64           `json:"doc"`
	FileSize int64           `json:"file_size"`
	HeadSize int64           `json:"head_size"`
	MaxBlock int64           `json:"block_size"`
	Blocks   map[int64]int64 `json:"blocks"`
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

func (d *diskCache) storeBlock(docID, idx int64, data []byte, fileSize, sourceID int64) {
	if d == nil || len(data) <= 0 {
		return
	}
	idxDoc := d.loadIndex(docID)
	d.mu.Lock()
	if old, ok := idxDoc.Blocks[idx]; ok && old == int64(len(data)) {
		d.mu.Unlock()
		return
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

	d.flushIndexLocked(docID, true)
	d.mu.Unlock()
	d.evictIfNeeded()
}

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

func (d *diskCache) clear() {
	d.mu.Lock()
	d.indexes = map[int64]*diskIndex{}
	d.usage = 0
	d.mu.Unlock()
	_ = os.RemoveAll(d.dir)
	_ = os.MkdirAll(d.dir, 0o755)
}
