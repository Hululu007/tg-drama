// Package docinfo 定义 raw_messages.info_json 的精简表示。
//
// 数据库只当索引用：一条消息在库里需要记住的只有「去哪儿取这个文件」——
// 文档 ID、访问哈希、文件引用、体积、类型、分辨率/时长，以及缩略图的
// 类型与尺寸（真正的图片字节在下载时按需向 Telegram 取）。
//
// gotd 的 json.Marshal(*tg.Document) 会把 Thumbs[].Bytes 和 VideoThumbs
// 里的内联缩略图数据一起写进去，单条 800+ 字节，两万多条就是二十多兆。
// 这个包负责在写入时就只留必要字段，并提供把历史数据压小的工具函数。
package docinfo

import (
	"encoding/json"
	"strings"

	"github.com/gotd/td/tg"
)

// Thumb 一条缩略图的索引信息（不含图片字节）
type Thumb struct {
	Type string `json:"Type"`
	W    int    `json:"W,omitempty"`
	H    int    `json:"H,omitempty"`
	Size int    `json:"Size,omitempty"`
}

// Attr 文档属性的索引信息（视频时长/分辨率、文件名）
type Attr struct {
	Duration float64 `json:"Duration,omitempty"`
	W        int     `json:"W,omitempty"`
	H        int     `json:"H,omitempty"`
	FileName string  `json:"FileName,omitempty"`
}

// Index 一条文档在库里保留的全部信息
type Index struct {
	ID            int64   `json:"ID"`
	AccessHash    int64   `json:"AccessHash"`
	FileReference []byte  `json:"FileReference,omitempty"`
	Size          int64   `json:"Size,omitempty"`
	MimeType      string  `json:"MimeType,omitempty"`
	DCID          int     `json:"DCID,omitempty"`
	Thumbs        []Thumb `json:"Thumbs,omitempty"`
	Attributes    []Attr  `json:"Attributes,omitempty"`
}

// FromDocument 把 Telegram 文档压成索引结构
func FromDocument(doc *tg.Document) Index {
	idx := Index{
		ID:            doc.ID,
		AccessHash:    doc.AccessHash,
		FileReference: doc.FileReference,
		Size:          doc.Size,
		MimeType:      doc.MimeType,
		DCID:          doc.DCID,
	}
	for _, t := range doc.Thumbs {
		switch ps := t.(type) {
		case *tg.PhotoSize:
			// Type "i" 是 LZ 压缩的 stripped 缩略图，"j" 是 SVG path，都下载不了
			if ps.Type == "i" || ps.Type == "j" {
				continue
			}
			idx.Thumbs = append(idx.Thumbs, Thumb{Type: ps.Type, W: ps.W, H: ps.H, Size: ps.Size})
		case *tg.PhotoSizeProgressive:
			if ps.Type == "i" || ps.Type == "j" {
				continue
			}
			idx.Thumbs = append(idx.Thumbs, Thumb{Type: ps.Type, W: ps.W, H: ps.H})
		}
	}
	for _, a := range doc.Attributes {
		switch at := a.(type) {
		case *tg.DocumentAttributeVideo:
			idx.Attributes = append(idx.Attributes, Attr{
				Duration: at.Duration, W: at.W, H: at.H,
			})
		case *tg.DocumentAttributeFilename:
			idx.Attributes = append(idx.Attributes, Attr{FileName: at.FileName})
		}
	}
	return idx
}

// Marshal 序列化为索引 JSON（写入 raw_messages.info_json）
func Marshal(doc *tg.Document) string {
	b, err := json.Marshal(FromDocument(doc))
	if err != nil {
		return ""
	}
	return string(b)
}

// Compact 把历史遗留的「完整文档 JSON」压成索引 JSON。
// 返回压缩后的内容和是否真的有变化；解析失败时原样返回。
func Compact(raw string) (string, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw, false
	}
	var idx Index
	if err := json.Unmarshal([]byte(raw), &idx); err != nil {
		return raw, false
	}
	if idx.ID == 0 && idx.AccessHash == 0 {
		return raw, false
	}
	// 丢掉 "i"（LZ 压缩条带）和 "j"（SVG path）这类下载不出来的缩略图，
	// 它们在库里只会占体积，选缩略图时也用不上。
	kept := idx.Thumbs[:0]
	for _, t := range idx.Thumbs {
		if t.Type == "i" || t.Type == "j" {
			continue
		}
		kept = append(kept, t)
	}
	idx.Thumbs = kept
	// 重新序列化后 Thumbs/Attributes 里只剩索引字段，Bytes 之类的内联数据被丢掉
	b, err := json.Marshal(idx)
	if err != nil {
		return raw, false
	}
	if len(b) >= len(raw) {
		return raw, false
	}
	return string(b), true
}
