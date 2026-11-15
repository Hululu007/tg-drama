package parser

import "testing"

// 只有两个标题容器时不是多标题预览帖：一个是帖子标题、另一个常在简介里
// 引用别的剧名。强行按顺序分配会把同一部剧拆成两部、集号还会串。
func TestMultiEntriesRejectsTwoTitles(t *testing.T) {
	captions := []string{
		"🌟【男以稀为贵】EP1-2  内群更新至第5集  #AI短剧\n作品简介：政府出台《强制怀孕配额法》，规定年满二十岁的男性必须协助多名女性怀孕。",
		"⚡️ 【双生道侣】 新剧首更 内群更新至第九集  ⚡️ #双生道侣\n📱 《双生道侣 / 小院青梅先门将至》EP1-5 合集",
		"⚡️【透明冰棒/不融你手只融你口】  内群已更第三集 ⚡️ #透明冰棒\n《透明冰棒/不融你手只融你口》EP1-2",
	}
	for _, c := range captions {
		got, err := New().Parse(c, "")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if len(got.Multi) != 0 {
			t.Errorf("multi should be empty, got %+v", got.Multi)
		}
		if got.EpisodeNo != 1 {
			t.Errorf("episode = %d, want 1", got.EpisodeNo)
		}
	}
}

// 真正的多标题预览帖（>=3 部剧）仍然按顺序返回。
func TestMultiEntriesThreePlus(t *testing.T) {
	caption := "《母上攻略》第6集\n《兄弟妈妈好好操》第7集\n《重生之征服郭伯母》第27集"
	if got := MultiEntries(caption); len(got) != 3 {
		t.Fatalf("multi = %+v, want 3 entries", got)
	}
}
