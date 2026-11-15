package web

import (
	"testing"

	"tgdrama/internal/storage"
)

// 合并后常见的情况：同一集既有单集文件，也有「第1-2集」合集包。
// 默认线路必须是单集文件，哪怕它更短。
func TestPickFromSourcesSkipsMultiEpisodeBundle(t *testing.T) {
	srcs := []*storage.Source{
		{ID: 1, FileSize: 100, Duration: 360, FileName: "剧名1.mp4"},
		{ID: 2, FileSize: 200, Duration: 720, Caption: "《后悔有妻  第1-2集》", FileName: "竖屏(1).mp4"},
	}
	if got := pickFromSources(srcs); got.ID != 1 {
		t.Fatalf("默认线路 = %d，期望单集文件 1", got.ID)
	}
	// 全部来源都只是「整剧文案 + 单集时长」时，不能误当成合集包（真实库常见）：
	// 三条 360s 单集共享「第1-3集 连载中」文案，长度才是真正的判据。
	shared := []*storage.Source{
		{ID: 1, FileSize: 100, Duration: 360, Caption: "《剧名》第1-3集 连载中 全系列完整版"},
		{ID: 2, FileSize: 110, Duration: 360, Caption: "《剧名》第1-3集 连载中 全系列完整版"},
		{ID: 3, FileSize: 120, Duration: 720, Caption: "《剧名  第1-2集》"},
	}
	if got := pickFromSources(shared); got.ID == 3 {
		t.Fatal("720s 合集包仍被选成默认线路")
	}
	// 只有合集包时仍要能播（不能返回 nil）
	if got := pickFromSources(srcs[1:]); got == nil || got.ID != 2 {
		t.Fatalf("全部是合集包时应回退到合集包，得到 %+v", got)
	}
	// 集号范围写在文件名里同样识别
	bundle := &storage.Source{ID: 3, FileSize: 1, FileName: "剧名 第3-4集.mp4"}
	if !sourceSpansMultipleEpisodes(bundle) {
		t.Fatal("文件名范围未识别为合集包")
	}
	// 单集/季数不能被误判
	for _, s := range []*storage.Source{
		{FileName: "剧名 第3集.mp4"},
		{FileName: "剧名 第三集.mp4"},
		{Caption: "《剧名》第二季 第3集"},
		{FileName: "剧名3.mp4"},
		{Caption: "更新至第12集"},
	} {
		if sourceSpansMultipleEpisodes(s) {
			t.Errorf("误判为合集包: %+v", s)
		}
	}
}
