package parser

import "testing"

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
