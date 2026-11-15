package parser

import "testing"

// 连发的多集帖子（第14-15集 / 第16-17集 …）会被 Telegram 合成一个相册，
// 每条视频自带 caption：集号必须以本条 caption 为准，不能按组内序号偏移。
func TestAlbumConsecutiveCaptions(t *testing.T) {
	r := NewAlbumResolver()
	caps := []string{"《母上攻略》第14-15集 连载中 全系列完整版 #ai #母上攻略",
		"《母上攻略》第16-17集 连载中 全系列完整版 #ai #母上攻略",
		"《母上攻略》第18-19集 连载中 全系列完整版 #ai #母上攻略"}
	want := []int{14, 16, 18}
	for pos, c := range caps {
		p, err := New().Parse(c, "")
		if err != nil {
			t.Fatalf("%q: %v", c, err)
		}
		if got := r.Resolve(6, 555, p, true, "", pos); got != want[pos] {
			t.Errorf("pos %d: got %d, want %d", pos, got, want[pos])
		}
	}
}

// 一条共享 caption 声明范围时，组内各成员仍按序号落集。
func TestAlbumSharedRangeStillSequential(t *testing.T) {
	r := NewAlbumResolver()
	p, err := New().Parse("【我的妈妈是体育教师】EP5-8 完整剧集", "")
	if err != nil {
		t.Fatal(err)
	}
	for pos, want := range map[int]int{0: 5, 1: 6, 2: 7, 3: 8} {
		if got := r.Resolve(6, 777, p, true, "", pos); got != want {
			t.Errorf("pos %d: got %d, want %d", pos, got, want)
		}
	}
}
