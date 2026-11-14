package tgclient

import (
	"context"
	"fmt"
	"log"
	"path/filepath"
	"sync"
	"time"

	"github.com/gotd/td/telegram/downloader"
	"github.com/gotd/td/tg"
)

const (
	coverConcurrency = 2

	backfillWorkers = 3

	backfillBudget = 120

	coverTryCooldown = 15 * time.Minute

	backfillAsk = 600
)

func (s *Service) setThumbCover(ctx context.Context, api *tg.Client, showID int64, doc *tg.Document) bool {
	best := pickThumb(doc)
	if best == nil {
		return false
	}
	if err := s.downloadThumb(ctx, api, showID, doc.ID, doc.AccessHash, doc.FileReference, best.Type); err != nil {
		log.Printf("下载缩略图封面失败 show=%d thumb=%q: %v", showID, best.Type, err)
		return false
	}
	return true
}

func pickThumb(doc *tg.Document) *tg.PhotoSize {
	var best *tg.PhotoSize
	for _, t := range doc.Thumbs {
		switch ps := t.(type) {
		case *tg.PhotoSize:
			if best == nil || ps.W*ps.H > best.W*best.H {
				best = ps
			}
		case *tg.PhotoSizeProgressive:
			if best == nil || ps.W*ps.H > best.W*best.H {
				best = &tg.PhotoSize{Type: ps.Type, W: ps.W, H: ps.H}
			}
		}
	}
	return best
}

func (s *Service) downloadThumb(ctx context.Context, api *tg.Client, showID, docID, accessHash int64,
	fileRef []byte, thumbType string) error {
	path := filepath.Join(s.cfg.CoverDir(), fmt.Sprintf("show-%d.jpg", showID))
	loc := &tg.InputDocumentFileLocation{
		ID: docID, AccessHash: accessHash,
		FileReference: fileRef, ThumbSize: thumbType,
	}
	if !s.dlGate.wait(ctx, mediaDownloadGap) {
		return ctx.Err()
	}
	if _, err := downloader.NewDownloader().Download(api, loc).ToPath(ctx, path); err != nil {
		return err
	}
	return s.db.SetShowCover(showID, path)
}

func (s *Service) ensureCoverFromThumb(ctx context.Context, showID int64, doc *tg.Document) {
	if ok, _ := s.db.HasCover(showID); ok {
		return
	}
	api := s.API()
	if api == nil {
		return
	}
	s.setThumbCover(ctx, api, showID, doc)
}

func (s *Service) claimCover(showID int64, cooldown time.Duration) bool {
	s.coverMu.Lock()
	defer s.coverMu.Unlock()
	if s.coverTry == nil {
		s.coverTry = map[int64]time.Time{}
	}
	if at, ok := s.coverTry[showID]; ok && time.Since(at) < cooldown {
		return false
	}
	if len(s.coverTry) > 8000 {
		for id, at := range s.coverTry {
			if time.Since(at) > cooldown {
				delete(s.coverTry, id)
			}
		}
	}
	s.coverTry[showID] = time.Now()
	return true
}

func (s *Service) RequeueCover(showID int64) {
	if showID <= 0 || s.db == nil {
		return
	}
	api := s.API()
	if api == nil || s.PlaybackDeferred() {
		return
	}
	go func() {
		select {
		case s.coverSem <- struct{}{}:
		default:

			return
		}
		defer func() { <-s.coverSem }()

		if !s.claimCover(showID, coverTryCooldown) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if !s.rpcGate.wait(ctx, coverFetchGap) {
			return
		}
		s.backfillShowCover(ctx, api, showID)
	}()
}

func (s *Service) BackfillCovers(ctx context.Context, api *tg.Client) {
	s.coverMu.Lock()
	if s.coverRunning {
		s.coverMu.Unlock()
		return
	}
	s.coverRunning = true
	s.coverMu.Unlock()
	defer func() {
		s.coverMu.Lock()
		s.coverRunning = false
		s.coverMu.Unlock()
	}()

	attempted := 0
	for attempted < backfillBudget {
		select {
		case <-ctx.Done():
			return
		default:
		}

		ids, err := s.db.ListShowsWithoutCover(backfillAsk)
		if err != nil {
			log.Printf("查询待回填封面短剧失败: %v", err)
			return
		}
		todo := make([]int64, 0, len(ids))
		for _, id := range ids {
			if attempted+len(todo) >= backfillBudget {
				break
			}
			if s.claimCover(id, coverTryCooldown) {
				todo = append(todo, id)
			}
		}
		if len(todo) == 0 {
			return
		}
		attempted += len(todo)

		var (
			mu         sync.Mutex
			progressed bool
			wg         sync.WaitGroup
		)
		ch := make(chan int64, len(todo))
		for _, id := range todo {
			ch <- id
		}
		close(ch)
		for i := 0; i < backfillWorkers; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for id := range ch {
					select {
					case <-ctx.Done():
						return
					default:
					}
					if s.PlaybackDeferred() {
						return
					}
					if s.backfillShowCover(ctx, api, id) {
						mu.Lock()
						progressed = true
						mu.Unlock()
					}
				}
			}()
		}
		wg.Wait()
		if !progressed {
			return
		}
	}
}

func (s *Service) backfillShowCover(ctx context.Context, api *tg.Client, showID int64) bool {
	if !s.rpcGate.wait(ctx, coverFetchGap) {
		return false
	}
	doc, err := s.refreshShowDoc(ctx, api, showID)
	if err != nil {

		s.noteFlood(err)
		log.Printf("回填封面失败 show=%d: %v", showID, err)
		return false
	}
	if thumb := pickThumb(doc); thumb != nil {
		if err := s.downloadThumb(ctx, api, showID, doc.ID, doc.AccessHash, doc.FileReference, thumb.Type); err == nil {
			return true
		} else {
			s.noteFlood(err)
			log.Printf("回填封面失败 show=%d: %v", showID, err)
		}
	}
	return false
}

func (s *Service) refreshShowDoc(ctx context.Context, api *tg.Client, showID int64) (*tg.Document, error) {
	anchor, ok, err := s.db.GetShowDocAnchor(showID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, fmt.Errorf("没有可用于刷新的来源消息")
	}
	return s.messageDoc(ctx, api, anchor.ChatPK, anchor.MessageID)
}
