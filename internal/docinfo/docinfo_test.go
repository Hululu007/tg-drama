package docinfo

import (
	"encoding/json"
	"strings"
	"testing"
)

// 压缩后必须只留索引字段：内联缩略图字节、VideoThumbs、Flags 都要丢，
// 但下载缩略图（ID/AccessHash/FileReference/Thumbs）和重建映射
// （Size/MimeType/Attributes 里的时长与分辨率/文件名）需要的字段不能少。
func TestCompactStripsInlineThumbBytes(t *testing.T) {
	raw := `{"Flags":1,"ID":5897914468859387151,"AccessHash":5612855056145980810,` +
		`"FileReference":"BQAAAADqTjv2AAAvEWr1BEz98XKKOtzyJ/bW1FgncVpL","Date":1790609887,` +
		`"MimeType":"video/mp4","Size":2719954,` +
		`"Thumbs":[{"Type":"i","Bytes":"` + strings.Repeat("QUJD", 200) + `"},` +
		`{"Type":"m","W":320,"H":320,"Size":29843}],` +
		`"VideoThumbs":[{"Bytes":"` + strings.Repeat("QUJD", 200) + `","W":320,"H":180}],` +
		`"DCID":4,"Attributes":[{"Flags":34,"Duration":92.5,"W":720,"H":1280,"SupportsStreaming":true},` +
		`{"Flags":0,"FileName":"abc.mp4"}]}`

	out, changed := Compact(raw)
	if !changed {
		t.Fatal("完整文档 JSON 应当被压缩")
	}
	if len(out) >= len(raw)/4 {
		t.Fatalf("压缩效果不足：%d → %d", len(raw), len(out))
	}
	for _, bad := range []string{"Bytes", "VideoThumbs", "Flags", "Date", "SupportsStreaming"} {
		if strings.Contains(out, bad) {
			t.Fatalf("压缩后仍包含 %q: %s", bad, out)
		}
	}

	var idx Index
	if err := json.Unmarshal([]byte(out), &idx); err != nil {
		t.Fatalf("压缩结果不是合法 JSON: %v", err)
	}
	if idx.ID != 5897914468859387151 || idx.AccessHash != 5612855056145980810 {
		t.Fatalf("文档定位字段丢了: %+v", idx)
	}
	if len(idx.FileReference) == 0 {
		t.Fatal("file_reference 丢了，下载封面会多一次请求")
	}
	if idx.Size != 2719954 || idx.MimeType != "video/mp4" {
		t.Fatalf("体积/类型丢了，重建时无法识别视频: %+v", idx)
	}
	if len(idx.Thumbs) != 1 || idx.Thumbs[0].Type != "m" || idx.Thumbs[0].W != 320 {
		t.Fatalf("缩略图索引不对（应只剩可下载的位图）: %+v", idx.Thumbs)
	}
	if len(idx.Attributes) != 2 || idx.Attributes[0].Duration != 92.5 ||
		idx.Attributes[0].W != 720 || idx.Attributes[1].FileName != "abc.mp4" {
		t.Fatalf("属性索引不对: %+v", idx.Attributes)
	}
}

// 已经是精简格式的不该反复改写（幂等），空值也不该报错
func TestCompactIdempotent(t *testing.T) {
	compact := `{"ID":1,"AccessHash":2,"Size":3,"MimeType":"video/mp4"}`
	if out, changed := Compact(compact); changed || out != compact {
		t.Fatalf("已精简的数据不该再变: %q changed=%v", out, changed)
	}
	if out, changed := Compact(""); changed || out != "" {
		t.Fatal("空值应原样返回")
	}
	if out, changed := Compact("not json"); changed || out != "not json" {
		t.Fatal("非法 JSON 应原样返回")
	}
}
