package parser

import "testing"

func TestParse(t *testing.T) {
	cases := []struct {
		caption string
		file    string
		name    string
		ep      int
		tags    []string
	}{
		{"#都市 #逆袭 霸总爱上我 第1集 1080P", "", "霸总爱上我", 1, []string{"都市", "逆袭"}},
		{"【短剧频道】龙王赘婿 第23集 高清", "", "龙王赘婿", 23, []string{}},
		{"穿越王妃 EP01 4K", "", "穿越王妃", 1, []string{}},
		{"", "闪婚老伴_02.mp4", "闪婚老伴", 2, []string{}},
		{"#甜宠 千金丫鬟 第十二集", "", "千金丫鬟", 12, []string{"甜宠"}},
		{"重生之我在古代 E 05", "", "重生之我在古代", 5, []string{}},
		{"#古装 庆余年[03].mkv", "", "庆余年", 3, []string{"古装"}},
	}
	p := New()
	for _, c := range cases {
		got, err := p.Parse(c.caption, c.file)
		if err != nil {
			t.Fatalf("解析 %q 返回错误: %v", c.caption+c.file, err)
		}
		if got.ShowName != c.name {
			t.Errorf("%q 剧名 = %q, 期望 %q", c.caption+c.file, got.ShowName, c.name)
		}
		if got.EpisodeNo != c.ep {
			t.Errorf("%q 集号 = %d, 期望 %d", c.caption+c.file, got.EpisodeNo, c.ep)
		}
		if len(got.Tags) != len(c.tags) {
			t.Errorf("%q 标签 = %v, 期望 %v", c.caption+c.file, got.Tags, c.tags)
		}
	}
}

func TestNormalizeMerge(t *testing.T) {
	p := New()
	a, _ := p.Parse("霸总爱上我 第1集", "")
	b, _ := p.Parse("霸总爱上我 EP01 高清", "")
	if a.NormName != b.NormName {
		t.Fatalf("同剧不同命名应归一为同一键: %s vs %s", a.NormName, b.NormName)
	}
}
