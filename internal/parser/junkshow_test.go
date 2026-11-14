package parser

import "testing"

func TestIsJunkShowName(t *testing.T) {
	junk := []string{
		"露脸", "大学生", "颜值", "极品", "抖音", "成人漫剧", "少妇", "女大",
		"私房", "海角社区", "第一视角", "里番", "电影解说", "下",
		"点击24小时自助进内部群",
		"广 告 2元飞机号点击购买 TG全域推广全网最低价 官方客服",
		"点此开通 立刻进群追更",
		"24 小时自助 随时能进",
		"群主推荐超稳定VPN",
		"付费一次或按月 自助入群看全",

		"完整1", "完整2", "花絮3", "预告12",

		"偷窥4", "竖屏", "横屏",
	}
	for _, n := range junk {
		if !IsJunkShowName(n) {
			t.Errorf("%q 应判为非短剧", n)
		}
	}

	real := []string{
		"少妇白洁", "我的邪恶APP", "失踪的飞机杯", "熟女自助餐", "高三爱情故事",
		"妈妈你逃不出我的手掌心", "母上攻略", "重生之我不是舔狗", "极品家丁",
		"风流御厨 下", "秘密教学", "东京牛头人", "偷窥人生",
	}
	for _, n := range real {
		if IsJunkShowName(n) {
			t.Errorf("%q 是正常剧名，不该被判为非短剧", n)
		}
	}
}

func TestIsNonDramaSingleName(t *testing.T) {
	junk := []string{
		"演唱会福利", "众筹电影院", "6月22日 美图视频消除", "恋爱辅助器宣发",
		"三万英尺片花", "AC娘逛漫展", "街拍系列第2季", "井怜vlog 性爱自慰",
	}
	for _, n := range junk {
		if !IsNonDramaSingleName(n) {
			t.Errorf("%q 应判为「单集但不是短剧」", n)
		}
	}
	real := []string{"远亲不如近邻", "贤惠的妻子", "十三力", "神雕侠侣 小龙女 尹志平"}
	for _, n := range real {
		if IsNonDramaSingleName(n) {
			t.Errorf("%q 是正常剧名，不该被判为单集非短剧", n)
		}
	}
}

func TestCleanNameStripsAdTail(t *testing.T) {
	if got := cleanName("风流御厨 下 群主推荐超稳定VPN"); got != "风流御厨 下" {
		t.Errorf("cleanName = %q，期望 %q", got, "风流御厨 下")
	}
}

func TestIsAdCaption(t *testing.T) {
	ads := []string{
		"点此开通 立刻进群追更",
		"全网好剧持续收进群 预告频道只放前两",
		"内部群 完整片库 不靠预告切片追更",
		"付费一次或按月 自助入群看全",
	}
	for _, s := range ads {
		if !IsAdCaption(s) {
			t.Errorf("%q 应判为引流帖", s)
		}
	}
	normal := []string{
		"《母上攻略》第12集 剧情介绍：...",
		"秘密教学 EP08 高清",
	}
	for _, s := range normal {
		if IsAdCaption(s) {
			t.Errorf("%q 是正常剧集文案，不该判为引流帖", s)
		}
	}
}

func TestParseGenericTitleUnrecognized(t *testing.T) {
	p := New()
	if _, err := p.Parse("露脸", ""); err == nil {
		t.Fatal("泛分类标题应返回无法识别")
	}
}

func TestNameFromFileName(t *testing.T) {
	cases := map[string]string{
		"AI成人短剧儿媳觉醒公公的秘密藏不住_家庭伦理连续剧_黑料718.mp4": "儿媳觉醒公公的秘密藏不住",

		"偷窥4_watermarked.mp4": "",
		"竖屏.mp4":              "",
		"琼明神女录7.mp4":          "琼明神女录",
		"母子同欢 第一集.mp4":        "母子同欢",
	}
	for in, want := range cases {
		if got := NameFromFileName(in); got != want {
			t.Errorf("NameFromFileName(%q) = %q, want %q", in, got, want)
		}
	}
}
