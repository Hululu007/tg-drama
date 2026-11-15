package parser

import "testing"

func TestRealCaptions(t *testing.T) {
	p := New()
	cases := []struct {
		text     string
		wantName string
		wantEp   int
	}{
		{"📱 【母上攻略】 第1-2集  EP1-2 黄豆魔改原创短剧  置顶甄选内部会员群同步更新至3集", "母上攻略", 1},
		{"🤣 【母上攻略】EP18 更多原档高清完整剧集见内群  #黄豆短剧 #成人短剧", "母上攻略", 18},
		{"🌟【母上攻略】第十二集 预览   #黄豆短剧 #成人短剧", "母上攻略", 12},
		{"🌟 【母上攻略】完整剧集内群连载中 #母上攻略 《母上攻略》EP13", "母上攻略", 13},
		{"#ai成人短剧 #被拖进无尽深渊的母亲 | 📱《被拖进无尽深渊的母亲》第1-2集 全系列完整版", "被拖进无尽深渊的母亲", 1},
		{"《葡萄成熟时》第二集 全剧连载中🤣🤣", "葡萄成熟时", 2},
		{"火热上映《催眠神瞳》第3集切片 全集已更", "催眠神瞳", 3},
		{"性奴豪乳老师刘艳 短剧视频在线观看vert黄果短剧 merge", "性奴豪乳老师刘艳", 0},
		// caption 中的日期/时间/编号不是集号（0913 19:30）
		{"📱【真的她】 精采预告 0913 19:30 内群同步更新 #黄果短剧", "真的她", 0},
		// 「第049期」是开发日志期号，不是集号
		{"《少妇白洁》AI电影开发日志 第049期｜删掉可惜，拿来当宣传片", "少妇白洁", 0},
		// 「EP72分02秒」是时长，不是 EP72
		{"🌟【存在感：零】最新剧集 《存在感：零》EP72分02秒 饭局餐桌下的小秘密", "存在感 零", 0},
		// 剧名尾数与「第二季」都不能当集号
		{"《等一个人回来2》第二季 最新更新", "等一个人回来2", 0},
		{"《穿过薄雾的早晨2》第二季 已完结", "穿过薄雾的早晨2", 0},
		// 无「第」的区间写法
		{"📱【千金归来】1-10集 全集打包", "千金归来", 1},
		// 简介里的「第十集」不影响标题区已声明的范围
		{"《葡萄成熟时》第1-4集 剧情介绍：本剧已更新到第十集", "葡萄成熟时", 1},
		// 没有《》/【】，标题区是营销长文：#标签 是唯一的定名依据
		{"#母子同欢 #岳母交换计划 #母上攻略 这是咋滴了，现在怎么那么多熟女母子类的剧啊，大家爱好都那么雷同吗？😂 不说了，完整版都在内群", "母子同欢", 0},
		// #标签 与正文标题不一致时以标签为准（发布者自己打的聚合键）
		{"#存在感零 📱《存在感：零》第5集 EP-5 置顶甄选内部会员群更新全集", "存在感零", 5},
		{"#从地狱归来深渊调教 【从地狱归来：深渊调教】第4集 EP4", "从地狱归来深渊调教", 4},
		// 前面的标签是分类词时跳过，继续找真正的剧名标签
		{"#剧情 #真人 #千金归来 第3集 全系列完整版", "千金归来", 3},
		// 只有分类词标签时回退到标题容器
		{"#剧情 #调教 《千金归来》第1集", "千金归来", 1},
		// 标签与标题都可信但互相冲突（不是包含关系）时以标题为准
		{"#母上攻略 《隔壁空姐》第3集 内群连载", "隔壁空姐", 3},
		// #姐姐 这类亲属/角色词是分类标签，不能当剧名
		{"#姐姐 《姐姐的诱惑》第2集", "姐姐的诱惑", 2},
		// 标题只有营销词时用标签定名
		{"🌟 新剧上线 #母上攻略 第4集", "母上攻略", 4},
		// 标签比标题干净（标题带尾数）时用标签
		{"《穿过薄雾的早晨2》第二季 第6集 #穿过薄雾的早晨", "穿过薄雾的早晨", 6},
	}
	for _, c := range cases {
		got, err := p.Parse(c.text, "")
		if err != nil {
			t.Errorf("%q 解析失败: %v", c.text, err)
			continue
		}
		if got.ShowName != c.wantName || got.EpisodeNo != c.wantEp {
			t.Errorf("%q → 剧名=%q 集=%d (end=%d)，期望 %q 集%d",
				c.text, got.ShowName, got.EpisodeNo, got.EpEnd, c.wantName, c.wantEp)
		}
	}

	fileCases := []struct {
		fileName string
		wantName string
		wantEp   int
	}{
		{"母上攻略EP-7_watermarked.mp4", "母上攻略", 7},
		{"恶鬼新娘_watermarked.mp4", "恶鬼新娘", 0},
		{"未亡人雪女 第1集_watermarked.mp4", "未亡人雪女", 1},
		// 4 位以上编号（相机/相册序号）不是集号
		{"VID_20260713_000853_765.mp4", "", 0},
		{"IMG_0529.mp4", "", 0},
		// UUID 片段中的 e452 不是 E 集号
		{"cf2b7fed-e452-47eb-6067-4eb8ef08715e.mp4", "", 0},
	}
	for _, c := range fileCases {
		got, err := p.Parse("", c.fileName)
		if err != nil {
			t.Errorf("文件名 %q 解析失败: %v", c.fileName, err)
			continue
		}
		if c.wantName != "" && got.ShowName != c.wantName {
			t.Errorf("文件名 %q → 剧名=%q，期望 %q", c.fileName, got.ShowName, c.wantName)
		}
		if got.EpisodeNo != c.wantEp {
			t.Errorf("文件名 %q → 集=%d，期望 %d", c.fileName, got.EpisodeNo, c.wantEp)
		}
	}
}

func TestHasTitle(t *testing.T) {
	p := New()
	cases := []struct {
		text  string
		title bool
	}{
		{"《母上攻略》第1集", true},
		{"📱【千金归来】1-10集", true},
		// 剧名来自 #标签 时同样算「可信来源」，重建时可直接作为归并锚点
		{"母上攻略 第1集 #母上攻略", true},
		{"#母子同欢 这是咋滴了，现在怎么那么多熟女母子类的剧啊，大家爱好都那么雷同吗？", true},
		{"母上攻略 第1集", false},
	}
	for _, c := range cases {
		got, err := p.Parse(c.text, "")
		if err != nil {
			t.Fatalf("%q 解析失败: %v", c.text, err)
		}
		if got.HasTitle != c.title {
			t.Errorf("%q → HasTitle=%v，期望 %v", c.text, got.HasTitle, c.title)
		}
	}
}
