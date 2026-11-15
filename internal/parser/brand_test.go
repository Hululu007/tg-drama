package parser

import "testing"

// TestChannelBrandStripped 发布频道自己的厂牌/水印（ygwp/CBDJ/9PM/2048原创/jile ai/ReelShort）
// 出现在文件名里时不应该留在剧名里；只剩厂牌+序号的整条都不是剧。
func TestChannelBrandStripped(t *testing.T) {
	cases := []struct{ file, want string }{
		{"ygwp 小狐仙.mp4", "小狐仙"},
		{"CBDJ 禁忌师.mp4", "禁忌师"},
		{"9PM成人剧厂牌保姆调教中5_8集完结撒花_黑料718.mp4", "保姆调教中"},
		{"2048原创短剧 - 快乐星球3.mp4", "快乐星球"},
		{"极乐@jile_ai.mp4", "极乐"},
		{"AI大佬自制穿过薄雾的早晨.mp4", "穿过薄雾的早晨"},
	}
	p := New()
	for _, c := range cases {
		got, err := p.Parse("", c.file)
		if err != nil {
			t.Errorf("Parse(%q) 返回错误 %v，期望剧名 %q", c.file, err, c.want)
			continue
		}
		if got.ShowName != c.want {
			t.Errorf("Parse(%q) = %q，期望 %q", c.file, got.ShowName, c.want)
		}
	}
	for _, f := range []string{"cbdj动态7 31 Ai.mp4", "ygwp动态.mp4", "ReelShort.mp4"} {
		if got, err := p.Parse("", f); err == nil {
			t.Errorf("Parse(%q) = %q，期望不建剧", f, got.ShowName)
		}
	}
}
