package download

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"

	"tgdrama/internal/config"
	"tgdrama/internal/storage"
)

type Queue struct {
	db      *storage.DB
	cfg     *config.Config
	current atomic.Value
}

func NewQueue(db *storage.DB, cfg *config.Config) *Queue {
	q := &Queue{db: db, cfg: cfg}
	q.current.Store("")
	return q
}

func (q *Queue) Run(ctx context.Context) {
	_ = q.db.ResetDownloading()
	<-ctx.Done()
}

func (q *Queue) ClearMediaCache() error {
	_ = filepath.WalkDir(q.cfg.MediaDir(), func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		_ = os.Remove(path)
		return nil
	})
	q.current.Store("")
	return q.db.ResetMediaDownloads()
}

type StatusInfo struct {
	Current string `json:"current"`
	Done    int64  `json:"done"`
	Failed  int64  `json:"failed"`
	Pending int64  `json:"pending"`
	Total   int64  `json:"total"`
}

func (q *Queue) Status() StatusInfo {
	done, failed, pending := q.db.CountSourceStatuses()
	cur, _ := q.current.Load().(string)
	return StatusInfo{
		Current: cur,
		Done:    done,
		Failed:  failed,
		Pending: pending,
		Total:   done + failed + pending,
	}
}
