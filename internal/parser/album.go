package parser

import (
	"path/filepath"
	"strings"
)

func FileNameEpisode(fileName string) int {
	base := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	base = strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(base)
	ep, _, _, ok := extractEpisode(base, true)
	if !ok {
		return 0
	}
	return ep
}

type albumKey struct {
	chatID, groupedID int64
}

type albumInfo struct {
	base, lo, hi int
}

type AlbumResolver struct {
	groups map[albumKey]albumInfo
}

func NewAlbumResolver() *AlbumResolver {
	return &AlbumResolver{groups: map[albumKey]albumInfo{}}
}

func (r *AlbumResolver) Resolve(chatID, groupedID int64, parsed *Parsed, fromCaption bool, fileName string, pos int) int {
	return r.ResolveSized(chatID, groupedID, parsed, fromCaption, fileName, pos, -1)
}

func (r *AlbumResolver) ResolveSized(chatID, groupedID int64, parsed *Parsed, fromCaption bool, fileName string, pos, size int) int {
	var own, lo, hi int
	if fromCaption && parsed != nil {
		own, lo, hi = parsed.EpisodeNo, parsed.EpisodeNo, parsed.EpEnd

		if parsed.ProgressEp {
			if fnEp := FileNameEpisode(fileName); fnEp > 0 {
				own, lo, hi = fnEp, fnEp, fnEp
			}
		}
	}

	if groupedID == 0 {
		if own > 0 {
			return own
		}
		if ep := FileNameEpisode(fileName); ep > 0 {
			return ep
		}
		return 1
	}

	key := albumKey{chatID, groupedID}
	info := r.ensure(key, own, lo, hi, fileName)

	if info.hi > info.lo {

		if size == 1 {
			if fe := FileNameEpisode(fileName); fe >= info.lo && fe <= info.hi {
				return fe
			}
		}

		if own > 0 && own != info.lo {
			return own
		}
		if pos <= info.hi-info.lo {
			return info.lo + pos
		}

		return info.hi
	}

	if own > 0 {
		return own
	}
	if ep := FileNameEpisode(fileName); ep > 0 {
		return ep
	}
	return info.base + pos
}

func (r *AlbumResolver) ensure(key albumKey, own, lo, hi int, fileName string) albumInfo {
	info, ok := r.groups[key]
	if !ok {
		base := own
		if base == 0 {
			base = FileNameEpisode(fileName)
		}
		if base == 0 {
			base = 1
		}
		info = albumInfo{base: base, lo: lo, hi: hi}
		r.groups[key] = info
		return info
	}
	if own > 0 && info.lo == 0 && info.hi == 0 {
		info.base, info.lo, info.hi = own, lo, hi
		r.groups[key] = info
	}
	return info
}
