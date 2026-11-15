package parser

import (
	"path/filepath"
	"strings"
)

// FileNameEpisode 从文件名解析集号，0 表示无法识别。
// 文件名中的集号通常比"相册连续编号"更可靠（如 母上攻略 19_watermarked.mp4）。
func FileNameEpisode(fileName string) int {
	base := strings.TrimSuffix(fileName, filepath.Ext(fileName))
	base = strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(base)
	ep, _, _, ok := extractEpisode(base, true)
	if !ok {
		return 0
	}
	return ep
}

// albumKey 相册标识（同一频道内的 grouped_id）
type albumKey struct {
	chatID, groupedID int64
}

// albumInfo 相册声明的集号与范围
type albumInfo struct {
	base, lo, hi int
}

// AlbumResolver 解决"一条相册消息配多个视频"的集号分配：
// 优先级为 ①相册 caption 声明范围 [a,b] 内的序号 ②caption 单集号
// ③文件名集号 ④相册起始集号 + 组内序号 ⑤默认 1。
// 相册 caption 只存在其中一条消息上，调用方需把同组 caption 传给每条成员，
// 否则文件名里错误的模板集号（如「EP110 全10集」）会覆盖正确范围。
type AlbumResolver struct {
	groups map[albumKey]albumInfo
}

// NewAlbumResolver 创建相册集号解析器
func NewAlbumResolver() *AlbumResolver {
	return &AlbumResolver{groups: map[albumKey]albumInfo{}}
}

// Resolve 返回某条视频应属的集号。pos 为该视频在相册中的序号（从 0 开始），
// fromCaption 表示 parsed 来自真实 caption（而非文件名兜底）。
func (r *AlbumResolver) Resolve(chatID, groupedID int64, parsed *Parsed, fromCaption bool, fileName string, pos int) int {
	return r.ResolveSized(chatID, groupedID, parsed, fromCaption, fileName, pos, -1)
}

// ResolveSized 与 Resolve 相同，但额外知道同一相册里的视频总数 size
// （<=0 表示未知）。只有「相册里就这一个视频」时，caption 写的范围
// （如「第3-12集」）往往只是搬运来的模板文案，文件名才带着真实集号
// （同学的离异妈妈12.mp4），这时优先用文件名；多视频相册仍按组内序号分集，
// 否则文件名里的 1..N 本地编号会把整组都算成第 1 集。
func (r *AlbumResolver) ResolveSized(chatID, groupedID int64, parsed *Parsed, fromCaption bool, fileName string, pos, size int) int {
	var own, lo, hi int
	if fromCaption && parsed != nil {
		own, lo, hi = parsed.EpisodeNo, parsed.EpisodeNo, parsed.EpEnd
		// 「内群更新至第5集」只是频道进度，不是这条视频的集号：文件名里通常
		// 带着真实集号（xxx_EP3.mp4），优先用它，避免整组相册被记成第 5 集。
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

	// 相册声明了范围：按组内序号落到 [lo,hi]
	if info.hi > info.lo {
		// 单个视频却写着「第3-12集」这类范围：文件名里的集号更可信，
		// 但必须落在声明范围内，否则仍是模板文案（如 720p (13).mp4 配 第1-7集）。
		if size == 1 {
			if fe := FileNameEpisode(fileName); fe >= info.lo && fe <= info.hi {
				return fe
			}
		}
		// 连发的多集帖子（《母上攻略》第14-15集 / 第16-17集 …）会被 Telegram
		// 并进同一个相册，每条视频自带自己的 caption。此时集号以本条为准，
		// 若按组内序号偏移会得到 14/16/18 这类错位集号。
		if own > 0 && own != info.lo {
			return own
		}
		if pos <= info.hi-info.lo {
			return info.lo + pos
		}
		// 成员数多于声明范围（多码率/水印备份等）：多出来的成员算作
		// 最后一集的分段，不能退回到起始集，否则一集里混进别集的视频
		return info.hi
	}
	// 单集 caption：同组多个视频作为同一集的分段
	if own > 0 {
		return own
	}
	if ep := FileNameEpisode(fileName); ep > 0 {
		return ep
	}
	return info.base + pos
}

// ensure 取得（或建立）相册信息。caption 可能晚于无 caption 的成员到达，
// 此时补记声明的范围，供后续成员按序号分集。
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
