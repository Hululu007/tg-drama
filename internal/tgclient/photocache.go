package tgclient

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/gotd/td/tg"
	"github.com/gotd/td/tgerr"
)

const showWarmCooldown = 5 * time.Minute

const warmConcurrency = 2

func (s *Service) RequeueShowPhotos(showID int64) {
	if showID <= 0 || s.db == nil {
		return
	}
	api := s.API()
	if api == nil {
		return
	}
	s.mediaMu.Lock()
	if s.showWarm == nil {
		s.showWarm = map[int64]time.Time{}
	}

	if time.Now().Before(s.floodUntil) {
		s.mediaMu.Unlock()
		return
	}
	if at, ok := s.showWarm[showID]; ok && time.Since(at) < showWarmCooldown {
		s.mediaMu.Unlock()
		return
	}
	for id, at := range s.showWarm {
		if time.Since(at) > showWarmCooldown {
			delete(s.showWarm, id)
		}
	}
	s.showWarm[showID] = time.Now()
	s.mediaMu.Unlock()

	go func() {

		if s.warmSem != nil {
			s.warmSem <- struct{}{}
			defer func() { <-s.warmSem }()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := s.warmShowPhotos(ctx, api, showID); err != nil {
			log.Printf("预热预览图失败 show=%d: %v", showID, err)
		}
	}()
}

func (s *Service) noteFlood(err error) bool {
	d, ok := tgerr.AsFloodWait(err)
	if !ok {
		return false
	}
	s.mediaMu.Lock()
	if until := time.Now().Add(d + time.Second); until.After(s.floodUntil) {
		s.floodUntil = until
	}
	s.mediaMu.Unlock()
	return true
}

func (s *Service) floodPaused() bool {
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	return time.Now().Before(s.floodUntil)
}

func (s *Service) SetPlaybackActive(active bool) {
	s.mediaMu.Lock()
	if active {
		s.playbackStreams++
	} else if s.playbackStreams > 0 {
		s.playbackStreams--
	}
	s.mediaMu.Unlock()
}

func (s *Service) PlaybackDeferred() bool {
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	return s.playbackStreams > 0 || time.Now().Before(s.floodUntil)
}

func (s *Service) warmShowPhotos(ctx context.Context, api *tg.Client, showID int64) error {
	locs, err := s.db.ListShowPhotoLocs(showID)
	if err != nil {
		return err
	}
	byChat := make(map[int64][]int, 2)
	missing := 0
	for _, l := range locs {

		if l.CachePath != "" {
			if fi, serr := os.Stat(l.CachePath); serr == nil && fi.Size() > 0 {
				continue
			}
		} else if l.DocID > 0 {
			if p := filepath.Join(s.cfg.PhotoDir(), fmt.Sprintf("photo-%d.jpg", l.DocID)); fileExistsSize(p) {
				_ = s.db.SetPhotoCache(l.DocID, p)
				continue
			}
		}
		if l.MessageID <= 0 {
			continue
		}
		byChat[l.ChatID] = append(byChat[l.ChatID], l.MessageID)
		missing++
	}
	if missing == 0 {
		return nil
	}
	for chatPK, ids := range byChat {
		if s.PlaybackDeferred() {
			return nil
		}

		if !s.warmGate.wait(ctx, warmBatchGap) {
			return nil
		}
		chat, cerr := s.db.GetChat(chatPK)
		if cerr != nil {
			continue
		}

		if !chat.Enabled {
			continue
		}
		if len(ids) > 100 {
			ids = ids[:100]
		}
		req := &tg.ChannelsGetMessagesRequest{
			Channel: &tg.InputChannel{ChannelID: chat.TgChatID, AccessHash: chat.AccessHash},
		}
		for _, id := range ids {
			req.ID = append(req.ID, &tg.InputMessageID{ID: id})
		}
		res, gerr := api.ChannelsGetMessages(ctx, req)
		if gerr != nil {
			if s.noteFlood(gerr) {
				log.Printf("预热预览图触发限流，暂停后续请求 show=%d chat=%d", showID, chatPK)
				return nil
			}
			log.Printf("预热预览图取消息失败 show=%d chat=%d: %v", showID, chatPK, gerr)
			continue
		}
		for _, msg := range toMessages(messagesOf(res)) {
			media, ok := msg.Media.(*tg.MessageMediaPhoto)
			if !ok {
				continue
			}
			photo, ok := media.Photo.(*tg.Photo)
			if !ok {
				continue
			}
			if size := largestPhotoSize(photo); size != nil {
				s.enqueuePhoto(showID, photo, size)
			}
		}
	}
	return nil
}

func (s *Service) PhotoCacheUsage() (int64, int) {
	var total int64
	var n int
	_ = filepath.WalkDir(s.cfg.PhotoDir(), func(path string, d os.DirEntry, err error) error {
		if err != nil || d == nil || d.IsDir() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
			n++
		}
		return nil
	})
	return total, n
}

func (s *Service) ClearPhotoCache() (int64, int, error) {
	freed, files := s.PhotoCacheUsage()
	dir := s.cfg.PhotoDir()
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		return 0, 0, err
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}
	prefix := dir + string(os.PathSeparator)
	if err := s.db.ResetPhotoCache(prefix); err != nil {
		return 0, 0, err
	}
	return freed, files, nil
}

func (s *Service) RequeuePhoto(photoID int64) {
	if photoID <= 0 || s.db == nil || s.API() == nil {
		return
	}
	loc, err := s.db.GetPhotoLocation(photoID)
	if err != nil || loc.ShowID <= 0 {
		return
	}
	s.RequeueShowPhotos(loc.ShowID)
}

func fileExistsSize(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}
