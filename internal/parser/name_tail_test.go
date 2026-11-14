package parser

import "testing"

func TestTrimEpisodeNoiseFromName(t *testing.T) {
	p := New()
	cases := []struct{ caption, file, want string }{
		{"#AI #短剧 #真人 长生录1-10", "", "长生录"},
		{"#短剧 #真人 长生录11-14", "", "长生录"},
		{"#AI #短剧 武侠幻想情色剧 全12", "", "武侠幻想情色剧"},
		{"#短剧 #真人 #亿粉归零第3集", "", "亿粉归零"},
		{"", "岳母的秘密 第1集_watermarked.mp4", "岳母的秘密"},
		{"", "教师凌辱记第二季_EP-2_watermarked.mp4", "教师凌辱记第二季"},
		{"", "第十五集预告.mp4", ""},

		{"", "长生录 32集.mp4", "长生录"},

		{"", "预告_wm.mp4", ""},
		{"", "pv预告.mp4", "pv预告"},

		{"", "滴滴代操10水印.mp4", "滴滴代操"},
		{"", "废柴杂役水印.mp4", "废柴杂役"},
		{"", "艳母 EP4 processed.mp4", "艳母"},

		{"", "水印工具.mp4", "水印工具"},
	}
	for _, c := range cases {
		p, err := p.Parse(c.caption, c.file)
		got := ""
		if err == nil {
			got = p.ShowName
		}
		if got != c.want {
			t.Errorf("Parse(%q,%q) name=%q err=%v, want %q", c.caption, c.file, got, err, c.want)
		}
	}
}

func TestDualCheckTagBeatsContainer(t *testing.T) {
	p := New()
	cases := []struct{ caption, want string }{
		{"《秘密教学2》第5集 #秘密教学", "秘密教学"},
		{"【母上攻略】第5集 完整版 #母上攻略", "母上攻略"},
	}
	for _, c := range cases {
		p, err := p.Parse(c.caption, "")
		if err != nil {
			t.Errorf("Parse(%q) err=%v", c.caption, err)
			continue
		}
		if p.ShowName != c.want {
			t.Errorf("Parse(%q) name=%q, want %q", c.caption, p.ShowName, c.want)
		}
	}
}

func TestPreviewOnlyNameIsJunk(t *testing.T) {
	for _, n := range []string{"预告 wm", "pv预告", "PV预告", "预告"} {
		if !IsJunkName(n) {
			t.Errorf("IsJunkName(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"恶鬼新娘", "母上攻略", "预告片之家"} {
		if IsJunkName(n) {
			t.Errorf("IsJunkName(%q) = true, want false", n)
		}
	}
}
