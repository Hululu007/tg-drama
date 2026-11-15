package parser

import "testing"

// 真实案例：频道把「剧名 + PV预告 + 一堆题材标签」写在同一行。
// 旧规则取到了 #美少女 当剧名，建出一部叫「美少女」的假剧；
// 现在必须以《》里的官方剧名为准。
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
