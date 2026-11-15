package tgclient

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
)

const (
	mediaCover = iota // 视频缩略图 → 短剧封面
	mediaPhoto        // 相册图片 → 预览图/备用封面
)

// mediaJob 一次后台媒体下载任务
type mediaJob struct {
	kind   int
	showID int64
	doc    *tg.Document
	photo  *tg.Photo
	size   *tg.PhotoSize
}

const (
	// mediaWorkers 下载协程数：封面/预览图都是小文件，但并发太高会同时打出
	// 多条 upload.getFile，是账号级限流最容易踩的点；4 个配合 dlGate 的
	// 最小间隔，实测一页 20 张预览图仍在 2 秒内出完。
	mediaWorkers = 4
	// mediaQueueSize 队列长度；满了直接丢弃，交给 BackfillCovers 补
	mediaQueueSize = 4096
	// mediaDownloadGap 两次媒体下载之间的最小间隔
	mediaDownloadGap = 120 * time.Millisecond
)

// startMediaWorkers 启动媒体下载协程池。封面与预览图原来在入库主流程里同步下载，
// 每部新剧都要等一次网络往返，首次同步会被串行拖住好几分钟。
func (s *Service) startMediaWorkers(ctx context.Context) {
	for i := 0; i < mediaWorkers; i++ {
		go func() {
			for {
				select {
				case <-ctx.Done():
					return
				case job := <-s.mediaCh:
					s.runMediaJob(ctx, job)
				}
			}
		}()
	}
}

// enqueueCover 把短剧封面下载排入队列（同一部剧只排一次）
func (s *Service) enqueueCover(showID int64, doc *tg.Document) {
	if showID <= 0 || doc == nil {
		return
	}
	s.mediaMu.Lock()
	if s.coverPending[showID] {
		s.mediaMu.Unlock()
		return
	}
	s.coverPending[showID] = true
	s.mediaMu.Unlock()
	select {
	case s.mediaCh <- mediaJob{kind: mediaCover, showID: showID, doc: doc}:
	default:
		s.mediaMu.Lock()
		delete(s.coverPending, showID)
		s.mediaMu.Unlock()
	}
}

// enqueuePhoto 把相册预览图下载排入队列
func (s *Service) enqueuePhoto(showID int64, photo *tg.Photo, size *tg.PhotoSize) {
	if showID <= 0 || photo == nil || size == nil {
		return
	}
	job := mediaJob{kind: mediaPhoto, showID: showID, photo: photo, size: size}
	select {
	case s.mediaCh <- job:
	default:
	}
}

func (s *Service) runMediaJob(ctx context.Context, job mediaJob) {
	api := s.API()
	if api == nil {
		return
	}
	// 限流静默期或播放进行中都不发下载：继续拉只会把 FLOOD_WAIT 越拖越长，
	// 或者抢播放的带宽；图片下次打开详情页会重新排队，不会丢。
	if s.PlaybackDeferred() {
		return
	}
	switch job.kind {
	case mediaCover:
		defer func() {
			s.mediaMu.Lock()
			delete(s.coverPending, job.showID)
			s.mediaMu.Unlock()
		}()
		if ok, _ := s.db.HasCover(job.showID); ok {
			return
		}
		s.setThumbCover(ctx, api, job.showID, job.doc)
	case mediaPhoto:
		path := filepath.Join(s.cfg.PhotoDir(), fmt.Sprintf("photo-%d.jpg", job.photo.ID))
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			// 文件已经在本地，但库里可能还没记下路径（下载完成后进程被杀、
			// 或库里被清过）。不补这一笔的话，详情页每次打开都以为缺图，
			// 会一遍遍重新排队下载同一张——白耗流量也白挨限流。
			_ = s.db.SetPhotoCache(job.photo.ID, path)
			if has, _ := s.db.HasCover(job.showID); !has {
				_ = s.db.SetShowCover(job.showID, path)
			}
			return
		}
		if !s.dlGate.wait(ctx, mediaDownloadGap) {
			return
		}
		loc := &tg.InputPhotoFileLocation{
			ID: job.photo.ID, AccessHash: job.photo.AccessHash,
			FileReference: job.photo.FileReference, ThumbSize: job.size.Type,
		}
		if _, err := downloader.NewDownloader().Download(api, loc).ToPath(ctx, path); err != nil {
			// 下载通道也可能返回 FLOOD_WAIT，记下来让后面的预热批次先停一停
			s.noteFlood(err)
			log.Printf("下载预览图失败 show=%d photo=%d: %v", job.showID, job.photo.ID, err)
			return
		}
		if err := s.db.SetPhotoCache(job.photo.ID, path); err != nil {
			return
		}
		// 无封面时用相册图片充当封面（通常是海报，质量优于视频抽帧）
		if has, _ := s.db.HasCover(job.showID); !has {
			_ = s.db.SetShowCover(job.showID, path)
		}
	}
}
