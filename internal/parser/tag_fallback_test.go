package parser

import "testing"

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

func TestGenreTagsWithProseTitleNotShow(t *testing.T) {
	p := New()
	caption := "#高颜值 #露脸 #原相机 #反差\n今天在酒店拍的一组照片，大家看看效果"
	if _, err := p.Parse(caption, ""); err == nil {
		t.Fatalf("全是分类标签 + 描述文案的帖子不该建剧")
	}
}
