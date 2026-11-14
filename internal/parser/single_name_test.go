package parser

import "testing"

func TestSingleEpNameFixes(t *testing.T) {
	p := New()
	cases := []struct{ cap, file, want string }{
		{"📱 《偷窥》 第1-4集\n冷淡婚姻里的李爱被长期偷窥。", "偷窺4.mp4", "偷窥"},
		{"", "【爱琳】《我在斗罗开后宫》序章（侍女铃兰篇）.mp4", "我在斗罗开后宫"},
		{"【全集完结】《春暖桃花开》第1~61集🔥#都市 #男频 #短剧", "x.mp4", "春暖桃花开"},
		{"#短剧#韩国短剧  【#新手妈妈的女儿】3小时一口气看完", "IMG_5613.MP4", "新手妈妈的女儿"},
		{"", "狂飙｜AI短剧在线观看｜香蕉短剧.mp4", "狂飙"},
		{"", "听爸爸的话1.mp4", "听爸爸的话"},
		{"", "尤物讨妖队1免费在线观看｜91成人短剧.mp4", "尤物讨妖队"},
	}
	for _, c := range cases {
		got, err := p.Parse(c.cap, c.file)
		if err != nil {
			t.Fatalf("Parse(%q,%q) err=%v want %q", c.cap, c.file, err, c.want)
		}
		if got.ShowName != c.want {
			t.Errorf("Parse(%q,%q) name=%q want %q", c.cap, c.file, got.ShowName, c.want)
		}
	}
	if _, err := p.Parse("", "第一集（肉戏）.mp4"); err == nil {
		t.Error("纯集号文件名不应建剧")
	}
}
