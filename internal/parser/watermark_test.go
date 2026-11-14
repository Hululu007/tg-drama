package parser

import "testing"

func TestWatermarkPrefixIgnored(t *testing.T) {
	p := New()
	cases := []struct{ caption, want string }{
		{"阿舞 #韵母攻略 播放第二集", "韵母攻略"},
		{"西瓜视频~ #狐妖小红娘 第1集", "狐妖小红娘"},
		{"🎬 阿舞 #韵母攻略 第3集 完整版", "韵母攻略"},
	}
	for _, c := range cases {
		pp, err := p.Parse(c.caption, "")
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.caption, err)
		}
		if pp.ShowName != c.want {
			t.Errorf("Parse(%q).ShowName = %q, want %q", c.caption, pp.ShowName, c.want)
		}
		if !pp.FromTag {
			t.Errorf("Parse(%q) 应标记 FromTag", c.caption)
		}
	}
}

func TestTagTruncatedByPunctuation(t *testing.T) {
	p := New()
	for _, c := range []struct{ caption, want string }{
		{"#妈妈，你可是我最爱的人 第1集", "妈妈，你可是我最爱的人"},
		{"《妈妈，你可是我最爱的人》第2集 #妈妈，你可是我最爱的人", "妈妈，你可是我最爱的人"},
	} {
		pp, err := p.Parse(c.caption, "")
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.caption, err)
		}
		if pp.ShowName != c.want {
			t.Errorf("Parse(%q).ShowName = %q, want %q", c.caption, pp.ShowName, c.want)
		}
	}
}

func TestTagAfterLongTitleStillTitle(t *testing.T) {
	p := New()
	cases := []struct{ caption, want string }{

		{"#AI #短剧 #真人 #韩漫 美丽新世界1-42 合集", "美丽新世界"},
		{"#真人 #熟女 《母上攻略》第3集", "母上攻略"},
	}
	for _, c := range cases {
		pp, err := p.Parse(c.caption, "")
		if err != nil {
			t.Fatalf("Parse(%q): %v", c.caption, err)
		}
		if pp.ShowName != c.want {
			t.Errorf("Parse(%q).ShowName = %q, want %q", c.caption, pp.ShowName, c.want)
		}
	}
}
