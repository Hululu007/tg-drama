package storage

import "strings"

// manualShowAliases 是用户逐个确认过的「同一部剧的不同写法」。
// 重建时先把候选名映射到规范名，再按规范名归并，避免同剧被拆成多条。
var manualShowAliases = map[string]string{
	"有五个姐姐的我2": "有五个姐姐的我 第二季",
}

// manualHiddenPrefixes 是用户确认「不是短剧」、需要从列表隐藏的条目名（不区分大小写的前缀匹配）。
// 这些名字用通用规则判断不了（工具视频、频道名、带番号的片子），所以单独列出来。
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

// manualVisibleNames 是「虽然是分类词、但确实是剧名」的例外，重算隐藏时直接放行。
var manualVisibleNames = map[string]bool{
	"偷窥": true, // 《偷窥》是标题容器里的真剧名，不是 #偷窥 这种分类词
}

// manualHiddenMatch 判断剧名是否命中人工维护的隐藏清单。
func manualHiddenMatch(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, p := range manualHiddenPrefixes {
		if strings.HasPrefix(n, p) {
			return true
		}
	}
	return false
}
