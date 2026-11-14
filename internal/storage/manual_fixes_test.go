package storage

import "testing"

func TestManualHiddenPrefixes(t *testing.T) {
	hidden := []string{
		"水印处理中心",
		"ReelShort",
		"reelshort 第二季",
		"等一个人 7 5周卡剧",
		"B中爱情故事 朱雅篇5",
		"水果派 260820D 爱她就喝干她的水 番号 MIKR",
		"AV 剧情 定拍 勾引 日军 中尉 的女人",
		"AV女优生存指南厨",
		"小姨请出山 第38代传人 我的小姨不简单 小姨的双面人生",
	}
	for _, n := range hidden {
		if !manualHiddenMatch(n) {
			t.Errorf("manualHiddenMatch(%q) = false，期望 true", n)
		}
	}
	kept := []string{"等一个人回来2", "有五个姐姐的我 第二季", "AV女优生存指南", "水印工具"}
	for _, n := range kept {
		if manualHiddenMatch(n) {
			t.Errorf("manualHiddenMatch(%q) = true，期望 false", n)
		}
	}
}

func TestManualShowAlias(t *testing.T) {
	got, ok := manualShowAliases["有五个姐姐的我2"]
	if !ok || got != "有五个姐姐的我 第二季" {
		t.Fatalf("别名映射 = %q, %v；期望「有五个姐姐的我 第二季」", got, ok)
	}
}
