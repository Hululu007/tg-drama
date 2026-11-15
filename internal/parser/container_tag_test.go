package parser

import "testing"

// 两种标题容器共存时，用 #标签 选与它完全一致的那个。
func TestContainerMatchesTag(t *testing.T) {
	cases := []struct{ caption, want string }{
		{
			"⚡️【AI&真人女优梦幻联动 乐园触手】全网首发 ⚡️ #乐园触手  #ai成人短剧\n📱 《乐园触手》EP1 完整版\n剧情介绍：一座远海孤岛上的豪华酒店。",
			"乐园触手",
		},
		{
			"⚡️ 【双生道侣】 新剧首更 内群更新至第九集 ⚡️ #双生道侣\n📱 《双生道侣 / 小院青梅先门将至》EP1-5 合集",
			"双生道侣",
		},
		{
			"⚡️ 【兰若夜宴-聂小倩】最新剧集预览 ⚡️ #兰若夜宴聂小倩\n《倩》叁·当劫｜再宽我一夜",
			"兰若夜宴聂小倩", // 标签与容器归一化后相同，采用标签写法
		},
	}
	for _, c := range cases {
		got, err := New().Parse(c.caption, "")
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		if got.ShowName != c.want {
			t.Errorf("show name = %q, want %q", got.ShowName, c.want)
		}
	}
}
