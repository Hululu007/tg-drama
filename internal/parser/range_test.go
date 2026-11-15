package parser

import "testing"

// 频道文案真实格式：「【剧名】EP3-4 内群连载更新至第12集」。
// 进度说明里的数字不是本条的集号，必须取位置靠前的 EP3-4。
func TestEpisodeRangeWithProgressText(t *testing.T) {
	cases := []struct {
		name    string
		caption string
		ep, end int
	}{
		{
			"进度在范围之后",
			"🌟 【金牌经纪人】EP3-4 内群连载更新至第12集  #金牌经纪人  #御姐 #成人短剧\n剧情介绍：金牌经纪人掌握现代娱乐圈的游戏规则。",
			3, 4,
		},
		{
			"进度在范围之前",
			"⚡️【漂亮干姐姐】新剧首更 完整剧集内群更新至第八集  ⚡️ #漂亮干姐姐\n📱 《漂亮干姐姐》EP1-5\n剧情介绍：漂亮姐姐每天面试回家。",
			1, 5,
		},
		{
			"中文进度 + EP 范围",
			"⚡️【透明冰棒/不融你手只融你口】  内群已更第三集 ⚡️ #透明冰棒\n《透明冰棒/不融你手只融你口》EP1-2\n剧情介绍：社畜林然睡醒竟发现自己变成透明人。",
			1, 2,
		},
		{
			"更新至在范围内",
			"🌟【偷拍妈妈换游戏皮肤】EP4-5  内群更新至第6集|原档高清 #ai短剧",
			4, 5,
		},
		{
			"只有进度说明时兜底",
			"《某短剧》内群更新至第7集",
			7, 7,
		},
	}
	for _, c := range cases {
		got, err := New().Parse(c.caption, "")
		if err != nil {
			t.Fatalf("%s: parse error %v", c.name, err)
		}
		if got.EpisodeNo != c.ep || got.EpEnd != c.end {
			t.Errorf("%s: episode = %d-%d, want %d-%d", c.name, got.EpisodeNo, got.EpEnd, c.ep, c.end)
		}
	}
}

// 相册声明范围后，成员按组内序号依次落入 [lo,hi]；超出范围的成员算作
// 最后一集的分段，而不是回到起始集。
func TestAlbumResolveRange(t *testing.T) {
	r := NewAlbumResolver()
	parsed := &Parsed{ShowName: "金牌经纪人", NormName: "金牌经纪人", EpisodeNo: 3, EpEnd: 4}
	for pos, want := range map[int]int{0: 3, 1: 4, 2: 4} {
		if got := r.Resolve(1, 555, parsed, true, "金牌.mp4", pos); got != want {
			t.Errorf("pos %d: episode = %d, want %d", pos, got, want)
		}
	}
}
