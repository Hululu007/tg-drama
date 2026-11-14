package docinfo

import (
	"encoding/json"
	"strings"

	"github.com/gotd/td/tg"
)

type Thumb struct {
	Type string `json:"Type"`
	W    int    `json:"W,omitempty"`
	H    int    `json:"H,omitempty"`
	Size int    `json:"Size,omitempty"`
}

type Attr struct {
	Duration float64 `json:"Duration,omitempty"`
	W        int     `json:"W,omitempty"`
	H        int     `json:"H,omitempty"`
	FileName string  `json:"FileName,omitempty"`
}

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

func Marshal(doc *tg.Document) string {
	b, err := json.Marshal(FromDocument(doc))
	if err != nil {
		return ""
	}
	return string(b)
}

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

	kept := idx.Thumbs[:0]
	for _, t := range idx.Thumbs {
		if t.Type == "i" || t.Type == "j" {
			continue
		}
		kept = append(kept, t)
	}
	idx.Thumbs = kept

	b, err := json.Marshal(idx)
	if err != nil {
		return raw, false
	}
	if len(b) >= len(raw) {
		return raw, false
	}
	return string(b), true
}
