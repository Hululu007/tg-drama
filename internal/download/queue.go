// Package download 负责清理历史遗留的本地媒体缓存。
// 新版本不再把视频下载到本地：播放全部走 Telegram 流式代理（/api/stream），
// 这里只保留状态统计与清缓存，供老数据和经典界面使用。
package download

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"

	"tgdrama/internal/config"
	"tgdrama/internal/storage"
)

// Queue 媒体缓存清理与状态查询
type Queue struct {
	db      *storage.DB
	cfg     *config.Config
	current atomic.Value // 保留字段：历史上是「当前下载文件名」，现在恒为空
}

// NewQueue 创建队列
func NewQueue(db *storage.DB, cfg *config.Config) *Queue {
	q := &Queue{db: db, cfg: cfg}
	q.current.Store("")
	return q
}

// Run 记录运行上下文。不再有后台下载任务，只把上次中断留下的 downloading
// 状态重置，避免状态接口一直显示「下载中」。
func (q *Queue) Run(ctx context.Context) {
	_ = q.db.ResetDownloading()
	<-ctx.Done()
}

// ClearMediaCache 清空历史遗留的本地视频缓存目录并重置下载状态。
// 个别文件被占用时跳过继续，尽量清空；无论成败都重置下载状态。
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

// StatusInfo 下载状态摘要
type StatusInfo struct {
	Current string `json:"current"`
	Done    int64  `json:"done"`
	Failed  int64  `json:"failed"`
	Pending int64  `json:"pending"`
	Total   int64  `json:"total"`
}

// Status 查询下载状态
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
