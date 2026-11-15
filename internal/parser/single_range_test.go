package parser

import "testing"

// 单个视频配「第3-12集」这类搬运来的模板文案时，文件名里的真实集号更可信。
func TestSingleVideoRangeUsesFileName(t *testing.T) {
	cases := []struct {
		caption string
		file    string
		want    int
	}{
		{"《同学的离异妈妈》第3-12集\n剧情介绍：隔壁离异妈妈搬进新公寓", "同学的离异妈妈12.mp4", 12},
		{"《同学的离异妈妈》第13-14集", "同学的离异妈妈14.mp4", 14},
		{"《同学的离异妈妈》第15-18集", "同学的离异妈妈15.mp4", 15},
		// 文件名集号不在声明范围内（模板文案/无关后缀）时仍按范围起点
		{"《同学的离异妈妈》第1-7集", "720p (13).mp4", 1},
		{"《同学的离异妈妈》第1-7集", "同学的离异妈妈1.mov", 1},
	}
	for _, c := range cases {
		p, err := New().Parse(c.caption, c.file)
		if err != nil {
			t.Fatalf("parse %q: %v", c.caption, err)
		}
		r := NewAlbumResolver()
		got := r.ResolveSized(1, 14348450159048757, p, true, c.file, 0, 1)
		if got != c.want {
			t.Errorf("caption %q file %q = %d, want %d", c.caption, c.file, got, c.want)
		}
	}
	// 多视频相册仍按组内序号，不被文件名本地编号带偏
	alb := NewAlbumResolver()
	p, _ := New().Parse("《剧名》第3-12集", "")
	for pos, want := range map[int]int{0: 3, 1: 4, 9: 12} {
		if got := alb.ResolveSized(2, 999, p, true, "12.mov", pos, 10); got != want {
			t.Errorf("album pos %d = %d, want %d", pos, got, want)
		}
	}
}
