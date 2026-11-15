package parser

import "testing"

// 剧名候选以集号开头时说明它其实是「第N集 + 营销词」，应改用 #标签 定名。
func TestIsEpisodeishTitle(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"第3集 全系列完整版", true},
		{"第 12 集", true},
		{"EP07 抢先看", true},
		{"E3 花絮", true},
		{"母上攻略", false},
		{"千金归来", false},
	}
	for _, c := range cases {
		if got := isEpisodeishTitle(c.in); got != c.want {
			t.Errorf("isEpisodeishTitle(%q) = %v, want %v", c.in, got, c.want)
		}
	}
}
