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

// showWarmCooldown 详情页整页预热的冷却（按剧记）：同一部剧 5 分钟内重复打开
// 只问 TG 一次。用户来回切换同一部剧时不会被反复请求放大。
const showWarmCooldown = 5 * time.Minute

// warmConcurrency 同时进行的预热批次数。快速切剧会连续触发预热，每个批次至少要
// 发一次 channels.getMessages，限成 2 个就能把瞬时请求压到很低，又不会明显排队。
const warmConcurrency = 2

// RequeueShowPhotos 一次性把某部剧缺的预览图都排进下载队列。
//
// 详情页一打开就会请求这一页所有预览图，逐张走 channels.getMessages 要十几次
// 网络往返；这里按频道把它们合并成一次批量取消息，再交给下载协程池并发拉取，
// 首屏从「十几秒逐张出图」变成「一次往返 + 并发下载」。
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
	// 风控静默期内不再发请求：这时候去问 TG 只会让等待时间继续变长。
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
		// 先排队拿预热名额，再开始计时：排队时间不该算进 30 秒超时里
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

// noteFlood 记录 TG 返回的 FLOOD_WAIT，返回是否命中。命中后一段时间内
// 所有预热请求都直接跳过，避免在限流期内继续请求、把等待时间越滚越长。
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

// floodPaused 当前是否处于限流静默期
func (s *Service) floodPaused() bool {
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	return time.Now().Before(s.floodUntil)
}

// SetPlaybackActive 由 web 层报告「当前有播放流」：true 时后台封面/预览图下载
// 全部暂停，避免与播放取流抢带宽和限流额度。计数制支持多路同时播放。
func (s *Service) SetPlaybackActive(active bool) {
	s.mediaMu.Lock()
	if active {
		s.playbackStreams++
	} else if s.playbackStreams > 0 {
		s.playbackStreams--
	}
	s.mediaMu.Unlock()
}

// PlaybackDeferred 播放进行中或限流静默期内，后台媒体任务都应退让。
func (s *Service) PlaybackDeferred() bool {
	s.mediaMu.Lock()
	defer s.mediaMu.Unlock()
	return s.playbackStreams > 0 || time.Now().Before(s.floodUntil)
}

// warmShowPhotos 批量取该剧缺图的消息，逐张排进下载队列
func (s *Service) warmShowPhotos(ctx context.Context, api *tg.Client, showID int64) error {
	locs, err := s.db.ListShowPhotoLocs(showID)
	if err != nil {
		return err
	}
	byChat := make(map[int64][]int, 2)
	missing := 0
	for _, l := range locs {
		// 本地已有文件就直接补缓存标记，不再浪费一次请求去取消息。
		// 旧数据的 cache_path 可能是空的（下载完成后进程被杀/库被清过），
		// 只按库里的字段判断会以为全程缺图，每次打开详情页都重新发一遍。
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
		// 全局最小间隔：快速切剧时每部剧都要发一次 getMessages，
		// 没有闸门的话几次切换就会连成一条密集请求。
		if !s.warmGate.wait(ctx, warmBatchGap) {
			return nil
		}
		chat, cerr := s.db.GetChat(chatPK)
		if cerr != nil {
			continue
		}
		// 已关闭/已退出的频道不再发请求：剧可能还靠别的频道在库里显示，
		// 但对着一个退出的频道取消息只会白挨一次错误。
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

// PhotoCacheUsage 统计预览图缓存占用的字节数与文件数
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

// ClearPhotoCache 删除本地预览图文件并清空库里的缓存标记，返回释放的字节数与文件数。
// 只动预览图：索引、封面、观看记录都不变；清掉的图会在下次访问时按需重新下载。
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

// RequeuePhoto 单张预览图缺失时的补偿：交给整页批量预热处理。
// 早先是「一张图一次 channels.getMessages」，而详情页一打开前端就会请求这一页
// 所有预览图，一页十几张就会在一两秒内打出十几次请求；快速切剧时请求量按
// 「剧数 × 图数」翻倍，这是最容易触发风控的路径。改成按剧批量后，一页只发
// 一次请求，且同一部剧 5 分钟内只发这一次（见 showWarmCooldown）。
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

// fileExistsSize 本地文件存在且非空
func fileExistsSize(path string) bool {
	if path == "" {
		return false
	}
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir() && fi.Size() > 0
}
