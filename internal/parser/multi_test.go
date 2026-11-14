package parser

import "testing"

func TestMultiEntries(t *testing.T) {
	text := "⚡️ 放一波内群最新更新的一些剧集预览吧 😎\n\n《母上攻略》第6集  《兄弟妈妈好好操》第7集  《重生之征服郭伯母》第27集\n《夹着精液回家的女人》第3集 《台球少女》第3集 《琼明女神录》最新剧集"
	got := MultiEntries(text)
	want := []MultiEntry{
		{"母上攻略", 6},
		{"兄弟妈妈好好操", 7},
		{"重生之征服郭伯母", 27},
		{"夹着精液回家的女人", 3},
		{"台球少女", 3},
		{"琼明女神录", 0},
	}
	if len(got) != len(want) {
		t.Fatalf("条目数 %d，期望 %d：%+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("第 %d 项 %+v，期望 %+v", i, got[i], want[i])
		}
	}
	if MultiEntries("《母上攻略》第6集") != nil {
		t.Error("单个标题不应返回多标题列表")
	}
	if MultiEntries("随便一段没有标题的话 第3集") != nil {
		t.Error("没有标题容器时应返回 nil")
	}
}

func TestParseMultiAlbum(t *testing.T) {
	p := New()
	text := "《母上攻略》第6集 《兄弟妈妈好好操》第7集 《琼明女神录》最新剧集"
	got, err := p.Parse(text, "")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if len(got.Multi) != 3 {
		t.Fatalf("Multi 长度 %d，期望 3", len(got.Multi))
	}
	if got.ShowName != "母上攻略" || got.EpisodeNo != 6 {
		t.Errorf("剧名/集号 = %q/%d，期望 母上攻略/6", got.ShowName, got.EpisodeNo)
	}
}
