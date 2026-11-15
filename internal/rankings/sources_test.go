package rankings

import (
	"compress/gzip"
	"io"
	"os"
	"strings"
	"testing"
)

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	f, err := os.Open("testdata/" + name)
	if err != nil {
		t.Fatalf("打开样本 %s: %v", name, err)
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatalf("解压样本 %s: %v", name, err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("读取样本 %s: %v", name, err)
	}
	return raw
}

func checkItems(t *testing.T, board string, items []Item, wantMin int) {
	t.Helper()
	if len(items) < wantMin {
		t.Fatalf("%s 只解析到 %d 条，期望至少 %d 条", board, len(items), wantMin)
	}
	seen := map[string]bool{}
	for i, it := range items {
		if it.Rank != i+1 {
			t.Fatalf("%s 第 %d 条排名为 %d，应该是 %d", board, i, it.Rank, i+1)
		}
		if strings.TrimSpace(it.Title) == "" {
			t.Fatalf("%s 第 %d 条标题为空", board, i)
		}
		if seen[it.Title] {
			t.Fatalf("%s 标题重复: %s", board, it.Title)
		}
		seen[it.Title] = true
		if i < 3 {
			if !strings.HasPrefix(it.URL, "http") {
				t.Fatalf("%s 第 %d 条链接异常: %q", board, i, it.URL)
			}
			if !strings.HasPrefix(it.Cover, "http") {
				t.Fatalf("%s 第 %d 条封面异常: %q", board, i, it.Cover)
			}
		}
	}
}

func TestParseYeguo(t *testing.T) {
	items, err := parseYeguoPayload(readFixture(t, "yeguo_payload.json.gz"))
	if err != nil {
		t.Fatal(err)
	}
	checkItems(t, "野果", items, 15)
	if items[0].Heat == "" {
		t.Fatalf("野果第一条缺少热度：%+v", items[0])
	}
}

func TestParseDJ51(t *testing.T) {
	items, err := parseHGItems(readFixture(t, "dj51.html.gz"), "https://51djweb.com/ranks/hot/")
	if err != nil {
		t.Fatal(err)
	}
	checkItems(t, "51短剧", items, 15)
}

func TestParseHuangguoAI(t *testing.T) {
	items, err := parseHGItems(readFixture(t, "huangguoai.html.gz"), "https://huangguoai.com/ranks/hot/")
	if err != nil {
		t.Fatal(err)
	}
	checkItems(t, "黄果短剧", items, 15)
}

func TestParseHuangguoRank(t *testing.T) {
	items, err := parseHuangguoRank(readFixture(t, "huangguo.html.gz"), "https://g3vyeo.fmprunmzd.cc/rank/")
	if err != nil {
		t.Fatal(err)
	}
	checkItems(t, "黄果短剧榜", items, 30)
}

func TestParseHuangtao(t *testing.T) {
	items, err := parseHuangtao(readFixture(t, "huangtao.html.gz"), "https://aihuangdou.com/rankings/")
	if err != nil {
		t.Fatal(err)
	}
	checkItems(t, "黄桃", items, 30)
}

func TestHeatIsNumeric(t *testing.T) {
	// 热度只能出现数字（可带 万/W），不能把「AI短剧 全16集 9.8分」这种分类文案当热度
	cases := map[string][]Item{}
	y, _ := parseYeguoPayload(readFixture(t, "yeguo_payload.json.gz"))
	cases["野果"] = y
	a, _ := parseHGItems(readFixture(t, "dj51.html.gz"), "https://51djweb.com/ranks/hot/")
	cases["51"] = a
	g, _ := parseHuangguoRank(readFixture(t, "huangguo.html.gz"), "https://g3vyeo.fmprunmzd.cc/rank/")
	cases["黄果短剧榜"] = g
	h, _ := parseHuangtao(readFixture(t, "huangtao.html.gz"), "https://aihuangdou.com/rankings/")
	cases["黄桃"] = h
	e, _ := parseHGItems(readFixture(t, "huangguoai.html.gz"), "https://huangguoai.com/ranks/hot/")
	cases["黄果热播榜"] = e
	for name, items := range cases {
		for _, it := range items {
			if it.Heat == "" {
				continue
			}
			for _, r := range it.Heat {
				if (r >= '0' && r <= '9') || r == '.' || r == 'W' || r == 'w' || r == '万' || r == '+' {
					continue
				}
				t.Fatalf("%s《%s》热度含非数值字符: %q", name, it.Title, it.Heat)
			}
		}
	}
}

func TestNormalizeItemsFillsRanks(t *testing.T) {
	got := normalizeItems([]Item{
		{Rank: 3, Title: "C"},
		{Rank: 0, Title: "A"},
		{Rank: 3, Title: "B"},
		{Rank: 1, Title: "A"},
	})
	if len(got) != 3 {
		t.Fatalf("去重后应为 3 条，实际 %d", len(got))
	}
	for i, it := range got {
		if it.Rank != i+1 {
			t.Fatalf("排名应连续: %+v", got)
		}
	}
	if got[0].Title != "A" {
		t.Fatalf("排序异常: %+v", got)
	}
}
