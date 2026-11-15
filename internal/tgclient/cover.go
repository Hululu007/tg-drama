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
	// coverConcurrency 同时进行的「按需封面」数量（详情/列表里点开某部剧时触发）
	coverConcurrency = 2
	// backfillWorkers 启动后回填存量封面的并发数
	backfillWorkers = 3
	// backfillBudget 每次启动最多回填多少部剧的封面。
	//
	// 早先这里是无上限的 `for {}`：库里有四千多部剧没有封面，每部都要先拉一次
	// 消息再下一次缩略图，12 个协程只隔 150ms——启动即把账号按在限流边缘。
	// 更糟的是「没有缩略图」的剧永远留在待回填列表里，只要同批有一部成功，
	// 外层循环就会再来一遍，等于对着同一批剧无限重试。现在改成：每次启动
	// 只处理有限数量，其余交给「用户看到哪部就补哪部」的按需路径。
	backfillBudget = 120
	// coverTryCooldown 同一部剧两次封面拉取的最小间隔（失败也算一次）。
	// 没有缩略图的剧重试多少次都不会成功，冷却期内直接跳过。
	coverTryCooldown = 15 * time.Minute
	// backfillAsk 单次查询取多少候选，用于跳过冷却期内的条目
	backfillAsk = 600
)

// setThumbCover 下载文档缩略图写入短剧封面，返回是否成功。
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

// pickThumb 返回面积最大的可下载位图缩略图
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

// downloadThumb 通过文件位置下载指定尺寸的缩略图并落库。
// 失败原因（如 FILE_REFERENCE_EXPIRED）交给调用方处理与记录。
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

// ensureCoverFromThumb 入库时为新剧生成封面
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

// claimCover 占用某部剧的封面拉取名额：冷却期内（或刚失败过）返回 false。
// 记的是「尝试」而不是「成功」——没有缩略图的剧重试多少次都不会成功，
// 不记失败就会在每次启动/每次滚动列表时重复请求。
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

// RequeueCover 按需补一张封面：列表/详情请求到「没有封面」的短剧时调用。
//
// 这是「回填」的主要来源——用户看到哪部剧就给哪部剧补图，滚动一页最多触发
// 三十次，而且每部剧有 15 分钟冷却；比启动时对着四千多部剧猛拉安全得多，
// 也只在真正有人看得见的时候花流量。
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
			// 已有足够多的封面在拉，这次跳过；下次访问还会来
			return
		}
		defer func() { <-s.coverSem }()
		// 先拿到名额再记「尝试过」：名额满了被跳过的剧不能被冷却挡住，
		// 否则它会在 15 分钟里一直显示占位图。
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

// BackfillCovers 为存量无封面短剧回填缩略图封面。
// 每次启动只处理 backfillBudget 部，并发 3 个并走全局节流；剩下的由
// RequeueCover（用户翻到哪部补哪部）接手。
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
		// 一次多取一些再按冷却过滤：失败过的剧会一直留在「无封面」列表里，
		// 只取预算那么多的话，一个失败前缀就能把整批名额卡死，后面的剧再也轮不到。
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

// backfillShowCover 为单部剧回填缩略图封面。
// 库里只留索引，不再保存文档的缩略图清单，所以这里直接重新拉一次来源消息
// 拿最新的 file_reference 与缩略图类型——旧做法先试库里存的引用，实测几乎
// 必然 FILE_REFERENCE_EXPIRED，白跑一次下载。
func (s *Service) backfillShowCover(ctx context.Context, api *tg.Client, showID int64) bool {
	if !s.rpcGate.wait(ctx, coverFetchGap) {
		return false
	}
	doc, err := s.refreshShowDoc(ctx, api, showID)
	if err != nil {
		// 没缩略图/消息不可用也可能返回错误，统一按失败处理，冷却期后再说
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

// refreshShowDoc 重新拉取该剧任一来源消息，返回带最新 file_reference 的视频文档
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
