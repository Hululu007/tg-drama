package parser

import "testing"

// 标题容器按出现位置取：正文开头的【剧名】优先于简介里引用的《别的剧》。
func TestTitleContainerOrder(t *testing.T) {
	cases := []struct {
		caption string
		want    string
	}{
		{
			"🌟【男以稀为贵】EP1-2  内群更新至第5集 #男以稀为贵\n作品简介：政府出台《强制怀孕配额法》，规定年满二十岁的男性必须协助多名女性怀孕。",
			"男以稀为贵",
		},
		{
			"⚡️ 【双生道侣】 内群更新至第九集 ⚡️ #双生道侣\n《双生道侣 / 小院青梅先门将至》EP1-5 合集\n剧情介绍：一剑劈裂妖王的男人。",
			"双生道侣",
		},
		{
			"《金牌经纪人》EP3-4 内群连载更新至第12集",
			"金牌经纪人",
		},
	}
	for _, c := range cases {
		got, err := New().Parse(c.caption, "")
		if err != nil {
			t.Fatalf("parse %q: %v", c.caption[:12], err)
		}
		if got.ShowName != c.want {
			t.Errorf("show name = %q, want %q", got.ShowName, c.want)
		}
	}
}
