package storage

import "strings"

var manualShowAliases = map[string]string{
	"有五个姐姐的我2": "有五个姐姐的我 第二季",
}

var manualHiddenPrefixes = []string{
	"水印处理中心",
	"reelshort",
	"等一个人 7 5周卡剧",
	"b中爱情故事 朱雅篇5",
	"水果派 260820d 爱她就喝干她的水 番号 mikr",
	"av 剧情 定拍 勾引 日军 中尉 的女人",
	"av女优生存指南厨",
	"小姨请出山 第38代传人 我的小姨不简单 小姨的双面人生",
}

var manualVisibleNames = map[string]bool{
	"偷窥": true,
}

func manualHiddenMatch(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, p := range manualHiddenPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}
