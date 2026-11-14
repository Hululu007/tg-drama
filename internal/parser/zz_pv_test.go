package parser

import "testing"

func TestPVCaptionUsesContainerName(t *testing.T) {
	p := New()
	cap := "😅今日首推剧集 😅\n《刀剑少女在我家》PV预告 #黄果短剧 #奇幻 #都市 #美少女 #剑灵\n男主从地毯掏回来的锈剑在擦拭的过程中割破了他的手一滴精血唤醒了剑中的她\n都市剧情😁奇幻美少女🔥"
	got, err := p.Parse(cap, "")
	if err != nil {
		t.Fatalf("parse err: %v", err)
	}
	if got.ShowName != "刀剑少女在我家" {
		t.Fatalf("show=%q want 刀剑少女在我家 (tags=%v)", got.ShowName, got.Tags)
	}
}

func TestGenericTagNotShowName(t *testing.T) {
	p := New()
	cap := "美少女白袜足交榨精服务！小脚丫实在太可爱了\n#美少女 #白袜 #足交"
	if _, err := p.Parse(cap, ""); err == nil {
		t.Fatalf("generic tag caption should not create a show")
	}
}
