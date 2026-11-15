package tgclient

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gotd/td/tg"

	"tgdrama/internal/storage"
)

// ingestPhoto 处理相册图片：只登记定位信息，不下载。
//
// 采集时全量下载预览图是最亏的一步：实测库里 99% 的剧从没被打开过，
// 却先把每张相册图都从 Telegram 拉了一遍（5600 张约 600MB）。现在只写索引，
// 详情页真的被访问、前端请求 /api/photos/{id} 时才触发一次按需下载
// （见 Service.RequeuePhoto），本地已有文件的旧数据照旧复用。
func (s *Service) ingestPhoto(ctx context.Context, c *storage.Chat, msg *tg.Message) error {
	media, ok := msg.Media.(*tg.MessageMediaPhoto)
	if !ok {
		return nil
	}
	photo, ok := media.Photo.(*tg.Photo)
	if !ok {
		return nil
	}

	var showID int64
	parsed, perr := s.parser.Parse(msg.Message, "")
	if perr == nil && parsed.NormName != "" {
		// 内存索引里已有就直接用 ID：图片消息十条里九条属于已入库的剧，
		// 每条都 UpsertShow 会变成「每张图一次 UPDATE + 提交」，
		// 还会把这部剧的 updated_at 顶到列表最前面。
		if id, ok := s.showIDByNorm(parsed.NormName); ok {
			showID = id
		} else {
			showID, _, _ = s.db.UpsertShow(parsed.ShowName, parsed.NormName)
			s.rememberShowID(parsed.NormName, showID)
		}
	} else if msg.GroupedID != 0 {
		showID = s.db.FindShowByGrouped(c.ID, msg.GroupedID)
	}
	if showID == 0 {
		return nil
	}

	best := largestPhotoSize(photo)
	if best == nil {
		return nil
	}
	if _, _, err := s.db.UpsertShowPhoto(&storage.ShowPhoto{
		ShowID: showID, Width: best.W, Height: best.H,
	}, photo.ID, photo.AccessHash, photo.FileReference, c.ID, int64(msg.ID),
		msg.GroupedID, msg.Message); err != nil {
		return err
	}

	// 旧版本已经下载过的图：补缓存标记；顺带当封面（通常是海报，比视频抽帧好）
	path := filepath.Join(s.cfg.PhotoDir(), fmt.Sprintf("photo-%d.jpg", photo.ID))
	if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
		if err := s.db.SetPhotoCache(photo.ID, path); err != nil {
			return err
		}
		if has, _ := s.db.HasCover(showID); !has {
			_ = s.db.SetShowCover(showID, path)
		}
	}
	return nil
}

// photoMaxWidth 预览图下载的宽带上限。
// Telegram 的相册图往往 1280~2560 宽、单张 100KB 以上，而详情页里它最高只显示
// 140px、全屏放大也就 720px 级别，拉原图纯粹是浪费带宽和磁盘。优先取不超过
// 这个宽度的最大尺寸（通常是 Telegram 的 "y"），没有更小的才回退原图。
const photoMaxWidth = 1280

// largestPhotoSize 返回宽度不超过 photoMaxWidth 的最大尺寸；
// 全部超限时退化为最大尺寸，保证总有图可下。
func largestPhotoSize(photo *tg.Photo) *tg.PhotoSize {
	var pick, largest *tg.PhotoSize
	consider := func(ps *tg.PhotoSize) {
		if largest == nil || ps.W*ps.H > largest.W*largest.H {
			largest = ps
		}
		if ps.W > 0 && ps.W <= photoMaxWidth {
			if pick == nil || ps.W*ps.H > pick.W*pick.H {
				pick = ps
			}
		}
	}
	for _, sz := range photo.Sizes {
		switch ps := sz.(type) {
		case *tg.PhotoSize:
			consider(ps)
		case *tg.PhotoSizeProgressive:
			consider(&tg.PhotoSize{Type: ps.Type, W: ps.W, H: ps.H})
		}
	}
	if pick != nil {
		return pick
	}
	return largest
}
