package tgclient

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gotd/td/tg"

	"tgdrama/internal/storage"
)

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

const photoMaxWidth = 1280

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
