package parser

import "testing"

// 频道把「#制服 #巨乳」这类分类词当标签，真剧名写在文件名里：
// 这时不能建出一部叫「制服」的剧，要用文件名里的剧名。
func TestGenreTagFallsBackToFileName(t *testing.T) {
	p := New()
	got, err := p.Parse("#制服 #巨乳 #学生妹 刚下海的制服诱惑，今天在酒店实拍，完整版看简介", "公车系列 第一集.mp4")
	if err != nil {
		t.Fatalf("解析失败: %v", err)
	}
	if got.ShowName != "公车系列" {
		t.Fatalf("剧名=%q，应为「公车系列」", got.ShowName)
	}
}

// 整条帖子的 # 全是分类词、又没有《》/【】容器：剩下的「标题」是描述文案，
// 不该建剧（文件名也是哈希时由 ingest 丢弃）。
func TestGenreTagsWithProseTitleNotShow(t *testing.T) {
	p := New()
	caption := "#高颜值 #露脸 #原相机 #反差\n今天在酒店拍的一组照片，大家看看效果"
	if _, err := p.Parse(caption, ""); err == nil {
		t.Fatalf("全是分类标签 + 描述文案的帖子不该建剧")
	}
}
