package parser

import "testing"

func TestParsePVAlbumCaption(t *testing.T) {
	const caption = "😅今日首推剧集 😅\n《刀剑少女在我家》PV预告 #黄果短剧 #奇幻 #都市 #美少女 #剑灵\n" +
		"男主从地毯掏回来的锈剑在擦拭的过程中割破了他的手一滴精血唤醒了剑中的她\n" +
		"都市剧情😁奇幻美少女🔥\n\n🎞今天更新了 10 部+，你追的那部也在"
	p := New()
	got, err := p.Parse(caption, "")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.NormName != "刀剑少女在我家" {
		t.Fatalf("剧名应为「刀剑少女在我家」，得到 %q（tags=%v）", got.ShowName, got.Tags)
	}
}
