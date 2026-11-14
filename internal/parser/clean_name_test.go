package parser

import "testing"

func TestCleanFileNameStripsDoubleExt(t *testing.T) {
	cases := map[string]string{
		"乐园触手.mov":             "乐园触手",
		"仙尊圣女胯下助我成仙.mov":       "仙尊圣女胯下助我成仙",
		"荡神.MP4":               "荡神",
		"The Forbidden Flower": "The Forbidden Flower",
		"母上攻略EP-7_watermarked": "母上攻略EP 7",
	}
	for in, want := range cases {
		if got := CleanFileName(in); got != want {
			t.Errorf("CleanFileName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIsJunkNameDates(t *testing.T) {
	for _, n := range []string{"10月2日", "8月4日", "10月2", "2024-10-02", "2024/10/2"} {
		if !IsJunkName(n) {
			t.Errorf("IsJunkName(%q) = false, want true", n)
		}
	}
	for _, n := range []string{"乐园触手", "十月围城", "10月2日更新的剧"} {
		if IsJunkName(n) {
			t.Errorf("IsJunkName(%q) = true, want false", n)
		}
	}
}
func TestParseFileNameStripsExt(t *testing.T) {
	p := New()
	cases := map[string]string{
		"乐园触手 .mov":      "乐园触手",
		"仙尊圣女胯下助我成仙.mov": "仙尊圣女胯下助我成仙",
		"荡神.mov.mp4":     "荡神",
	}
	for in, want := range cases {
		got, err := p.Parse("", in)
		if err != nil {
			t.Fatalf("Parse(%q) error: %v", in, err)
		}
		if got.ShowName != want {
			t.Errorf("Parse(%q).ShowName = %q, want %q", in, got.ShowName, want)
		}
	}
}
