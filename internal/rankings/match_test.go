package rankings

import (
	"testing"

	"tgdrama/internal/storage"
)

func testShows() []*storage.Show {
	mk := func(id int64, name string, eps int) *storage.Show {
		return &storage.Show{ID: id, Name: name, NormName: name, EpCount: eps}
	}
	return []*storage.Show{
		mk(1, "我的岳母苏沛真", 35),
		mk(2, "母上攻略", 22),
		mk(3, "艳母", 10),
		mk(4, "艳母的秘密", 5),
		mk(5, "有五个姐姐的我", 20),
		mk(6, "有五个姐姐的我第二季", 10),
		mk(7, "秘密教学", 15),
		mk(8, "高三爱情故事", 30),
	}
}

func TestMatchExact(t *testing.T) {
	idx := buildShowIndex(testShows())
	cases := []struct {
		title string
		want  int64
	}{
		{"我的岳母苏沛真", 1},
		{"母上攻略", 2},
		{"秘密教学", 7},
		{"高三爱情故事", 8},
	}
	for _, c := range cases {
		sh, score := idx.match(c.title)
		if sh == nil || sh.ID != c.want {
			t.Fatalf("《%s》应匹配到 %d，实际 %+v", c.title, c.want, sh)
		}
		if score < 0.99 {
			t.Fatalf("《%s》完全匹配分值应接近 1，实际 %.2f", c.title, score)
		}
	}
}

func TestMatchDecoration(t *testing.T) {
	idx := buildShowIndex(testShows())
	cases := []string{
		"我的岳母苏沛真（完整版）",
		"我的岳母苏沛真 全集",
		"《母上攻略》",
		"母上攻略 更新至22集",
		"秘密教学 高清",
	}
	want := []int64{1, 1, 2, 2, 7}
	for i, title := range cases {
		sh, _ := idx.match(title)
		if sh == nil || sh.ID != want[i] {
			t.Fatalf("《%s》应匹配到 %d，实际 %+v", title, want[i], sh)
		}
	}
}

func TestMatchSeasonPrefersExactSeason(t *testing.T) {
	idx := buildShowIndex(testShows())
	sh, _ := idx.match("有五个姐姐的我第二季")
	if sh == nil || sh.ID != 6 {
		t.Fatalf("带季标题应优先匹配第二季，实际 %+v", sh)
	}
	sh, _ = idx.match("有五个姐姐的我")
	if sh == nil || sh.ID != 5 {
		t.Fatalf("不带季标题应匹配第一季，实际 %+v", sh)
	}
}

func TestMatchShortNameIsStrict(t *testing.T) {
	idx := buildShowIndex(testShows())
	// 两个字的名字只接受完全一致，不能因为包含关系错配到更长的剧
	sh, _ := idx.match("艳母")
	if sh == nil || sh.ID != 3 {
		t.Fatalf("《艳母》应匹配到短名本身，实际 %+v", sh)
	}
	sh, _ = idx.match("艳母的秘密")
	if sh == nil || sh.ID != 4 {
		t.Fatalf("《艳母的秘密》应匹配到长名，实际 %+v", sh)
	}
	if sh, _ := idx.match("艳母外传"); sh != nil {
		t.Fatalf("不存在的《艳母外传》不该硬配到 %+v", sh)
	}
}

func TestMatchUnknown(t *testing.T) {
	idx := buildShowIndex(testShows())
	for _, title := range []string{"完全不存在的剧名", "123456", "更新至20集"} {
		if sh, score := idx.match(title); sh != nil {
			t.Fatalf("《%s》不该匹配到 %s（%.2f）", title, sh.Name, score)
		}
	}
}

func TestMatchBoardDedup(t *testing.T) {
	idx := buildShowIndex(testShows())
	items := []Item{
		{Rank: 1, Title: "我的岳母苏沛真"},
		{Rank: 2, Title: "我的岳母苏沛真（完整版）"},
		{Rank: 3, Title: "母上攻略"},
		{Rank: 4, Title: "不存在的剧"},
		{Rank: 5, Title: "秘密教学"},
	}
	got := matchBoard(items, idx)
	if len(got) != 3 {
		t.Fatalf("应匹配 3 部（同剧去重），实际 %d: %+v", len(got), got)
	}
	if got[0].Rank != 1 || got[1].Rank != 3 || got[2].Rank != 5 {
		t.Fatalf("名次顺序异常: %+v", got)
	}
}
