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
	mediaCover = iota
	mediaPhoto
)

type mediaJob struct {
	kind   int
	showID int64
	doc    *tg.Document
	photo  *tg.Photo
	size   *tg.PhotoSize
}

const (
	mediaWorkers = 4

	mediaQueueSize = 4096

	mediaDownloadGap = 120 * time.Millisecond
)

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

			s.noteFlood(err)
			log.Printf("下载预览图失败 show=%d photo=%d: %v", job.showID, job.photo.ID, err)
			return
		}
		if err := s.db.SetPhotoCache(job.photo.ID, path); err != nil {
			return
		}

		if has, _ := s.db.HasCover(job.showID); !has {
			_ = s.db.SetShowCover(job.showID, path)
		}
	}
}
