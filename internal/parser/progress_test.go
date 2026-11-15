package parser

import "testing"

// 「第4-5合集 / 1-6合集」这类区间后面跟的是「合集」而不是「集」，
// 以前范围识别不到，只剩「内群更新至第六集」可匹配，整组相册被记成第 6 集。
func TestRangeWithCollectionSuffix(t *testing.T) {
	cases := []struct {
		caption string
		ep, end int
	}{
		{"🌟【结束被三个哥哥轮奸的日子】第4-5合集  内群更新至第六集  #ai短剧", 4, 5},
		{"⚡️【莞城那些年】 1-6合集  连载中  #ai短剧", 1, 6},
		{"🌟【AV拍摄指南】完整剧集30集内群连载更新至18集 《AV拍摄指南》第1-8合集", 1, 8},
		{"🌟【龙女奇遇记】EP1-6合集   #龙女奇遇记", 1, 6},
		{"🌟【谋一世锦绣】EP4-5合集  内群连载中", 4, 5},
	}
	for _, c := range cases {
		p, err := New().Parse(c.caption, "")
		if err != nil {
			t.Fatalf("%q: %v", c.caption, err)
		}
		if p.EpisodeNo != c.ep || p.EpEnd != c.end {
			t.Errorf("%q: episode = %d-%d, want %d-%d", c.caption, p.EpisodeNo, p.EpEnd, c.ep, c.end)
		}
		if p.ProgressEp {
			t.Errorf("%q: ProgressEp = true, want false", c.caption)
		}
	}
}

// caption 只写进度（更新至第5集）时，真实集号应取文件名里的 EP3，
// 不能把频道进度 5 当成这条视频的集号。
func TestProgressDoesNotBeatFileName(t *testing.T) {
	p, err := New().Parse("《剧名》内群更新至第5集", "剧名_EP3.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if !p.ProgressEp {
		t.Fatalf("ProgressEp = false, want true")
	}
	r := NewAlbumResolver()
	if got := r.Resolve(1, 0, p, true, "剧名_EP3.mp4", 0); got != 3 {
		t.Errorf("Resolve = %d, want 3 (文件名集号)", got)
	}
}

// 同一相册共享「更新至第N集」的进度 caption 时，每条成员按自己的文件名分集。
func TestAlbumProgressCaptionUsesFileName(t *testing.T) {
	p, err := New().Parse("【剧名】完整剧集内群更新至第6集 #剧名", "")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]int{"剧名3.mp4": 3, "剧名4.mp4": 4, "剧名5.mp4": 5}
	pos := 0
	for fn, ep := range want {
		r := NewAlbumResolver()
		if got := r.Resolve(7, 999, p, true, fn, pos); got != ep {
			t.Errorf("%s: got %d, want %d", fn, got, ep)
		}
		pos++
	}
}
