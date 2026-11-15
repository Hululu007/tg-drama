package rankings

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"golang.org/x/net/html"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 " +
	"(KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// source 一个榜单来源
type source struct {
	Key   string
	Name  string
	Site  string
	URL   string
	Fetch func(ctx context.Context, c *http.Client) ([]Item, error)
}

// defaultSources 五个榜单来源。顺序即前端页签顺序。
func defaultSources() []*source {
	return []*source{
		{Key: "yeguo", Name: "野果热播榜", Site: "野果短剧",
			URL: "https://yeguodj.com/rank/drama/", Fetch: fetchYeguo},
		{Key: "dj51", Name: "51热播榜", Site: "51短剧",
			URL: "https://51djweb.com/ranks/hot/", Fetch: fetchHGList("https://51djweb.com/ranks/hot/")},
		{Key: "huangguo", Name: "黄果短剧榜", Site: "黄果短剧榜",
			URL: "https://g3vyeo.fmprunmzd.cc/rank/", Fetch: fetchHuangguoRank},
		{Key: "huangtao", Name: "黄桃热播榜", Site: "黄桃短剧",
			URL: "https://aihuangdou.com/rankings/", Fetch: fetchHuangtao},
		{Key: "huangguoai", Name: "黄果热播榜", Site: "黄果短剧",
			URL: "https://huangguoai.com/ranks/hot/", Fetch: fetchHGList("https://huangguoai.com/ranks/hot/")},
	}
}

func httpGet(ctx context.Context, c *http.Client, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/json;q=0.9,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	req.Header.Set("Cache-Control", "no-cache")
	resp, err := c.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 榜单页通常 100~250KB，8MB 上限足够，也避免异常大响应拖住内存
	body, err := limitRead(resp.Body, 8<<20)
	if err != nil {
		return nil, err
	}
	if len(body) < 1024 {
		return nil, fmt.Errorf("响应过短（%d 字节）", len(body))
	}
	return body, nil
}

// ---------- 野果短剧（Nuxt payload） ----------

func fetchYeguo(ctx context.Context, c *http.Client) ([]Item, error) {
	raw, err := httpGet(ctx, c, "https://yeguodj.com/rank/drama/_payload.json")
	if err != nil {
		return nil, err
	}
	return parseYeguoPayload(raw)
}

func parseYeguoPayload(raw []byte) ([]Item, error) {
	root, err := decodeNuxtPayload(raw)
	if err != nil {
		return nil, fmt.Errorf("解析 payload: %w", err)
	}
	list := findRankingList(root)
	if len(list) == 0 {
		return nil, fmt.Errorf("榜单数据为空")
	}
	items := make([]Item, 0, len(list))
	for i, m := range list {
		title := strVal(m, "title", "name")
		if title == "" {
			continue
		}
		id := intVal(m, "video_id", "id")
		heat := strVal(m, "play_count_text", "chase_count_text")
		if heat == "" {
			if n := intVal(m, "chase_count"); n > 0 {
				heat = fmt.Sprintf("%d 追剧", n)
			}
		}
		u := ""
		if id > 0 {
			u = fmt.Sprintf("https://yeguodj.com/drama/detail/%d/", id)
		}
		items = append(items, Item{
			Rank:  i + 1,
			Title: title,
			URL:   u,
			Cover: strVal(m, "cover", "cover_url"),
			Heat:  heat,
		})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("榜单数据为空")
	}
	return normalizeItems(items), nil
}

// ---------- 51短剧 / 黄果短剧（同一套 hg-rank-item 模板） ----------

func fetchHGList(pageURL string) func(context.Context, *http.Client) ([]Item, error) {
	return func(ctx context.Context, c *http.Client) ([]Item, error) {
		raw, err := httpGet(ctx, c, pageURL)
		if err != nil {
			return nil, err
		}
		return parseHGItems(raw, pageURL)
	}
}

func parseHGItems(raw []byte, pageURL string) ([]Item, error) {
	doc, err := parseHTMLDoc(raw)
	if err != nil {
		return nil, err
	}
	nodes := collect(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "hg-rank-item")
	})
	items := make([]Item, 0, len(nodes))
	for i, n := range nodes {
		title := strings.TrimSpace(attr(n, "data-track-title"))
		var link *html.Node
		if h := findFirst(n, func(x *html.Node) bool {
			return x.Type == html.ElementNode && x.Data == "h2"
		}); h != nil {
			link = findFirst(h, func(x *html.Node) bool {
				return x.Type == html.ElementNode && x.Data == "a" && attr(x, "href") != ""
			})
			if title == "" {
				title = textOf(link)
			}
		}
		title = cleanRankTitle(title)
		if title == "" {
			continue
		}
		rank := atoiSafe(attr(n, "data-rank"))
		if rank <= 0 {
			rank = i + 1
		}
		heat := ""
		if hn := findFirst(n, func(x *html.Node) bool {
			return x.Type == html.ElementNode && hasClass(x, "hg-rank-item__heat-value")
		}); hn != nil {
			heat = cleanHeat(textOf(hn))
		}
		items = append(items, Item{
			Rank:  rank,
			Title: title,
			URL:   absURL(pageURL, attr(link, "href")),
			Cover: imgSrc(findFirst(n, func(x *html.Node) bool {
				return x.Type == html.ElementNode && x.Data == "img"
			})),
			Heat: heat,
		})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("页面里没有解析到榜单条目")
	}
	return normalizeItems(items), nil
}

// ---------- 黄果短剧榜（前三名大卡 + 4 名以后列表） ----------

func fetchHuangguoRank(ctx context.Context, c *http.Client) ([]Item, error) {
	const pageURL = "https://g3vyeo.fmprunmzd.cc/rank/"
	raw, err := httpGet(ctx, c, pageURL)
	if err != nil {
		return nil, err
	}
	return parseHuangguoRank(raw, pageURL)
}

func parseHuangguoRank(raw []byte, pageURL string) ([]Item, error) {
	doc, err := parseHTMLDoc(raw)
	if err != nil {
		return nil, err
	}
	var items []Item

	// 前 3 名：div.cdw.topw
	for _, n := range collect(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "cdw") && hasClass(n, "topw")
	}) {
		title := cleanRankTitle(textOf(findFirst(n, func(x *html.Node) bool {
			return x.Type == html.ElementNode && x.Data == "h3"
		})))
		if title == "" {
			continue
		}
		rank := atoiSafe(textOf(findFirst(n, func(x *html.Node) bool {
			return x.Type == html.ElementNode && hasClass(x, "rn")
		})))
		heat := ""
		if p := findFirst(n, func(x *html.Node) bool {
			return x.Type == html.ElementNode && x.Data == "p"
		}); p != nil {
			heat = cleanHeat(textOf(p))
		}
		link := findFirst(n, func(x *html.Node) bool {
			return x.Type == html.ElementNode && x.Data == "a" && attr(x, "href") != ""
		})
		items = append(items, Item{
			Rank:  rank,
			Title: title,
			URL:   absURL(pageURL, attr(link, "href")),
			Cover: imgSrc(findFirst(n, func(x *html.Node) bool {
				return x.Type == html.ElementNode && x.Data == "img"
			})),
			Heat: heat,
		})
	}

	// 4 名以后：li.rk
	for _, n := range collect(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && n.Data == "li" && hasClass(n, "rk")
	}) {
		title := cleanRankTitle(textOf(findFirst(n, func(x *html.Node) bool {
			return x.Type == html.ElementNode && x.Data == "h3"
		})))
		if title == "" {
			continue
		}
		rank := atoiSafe(textOf(findFirst(n, func(x *html.Node) bool {
			return x.Type == html.ElementNode && hasClass(x, "rn")
		})))
		heat := ""
		if p := findFirst(n, func(x *html.Node) bool {
			return x.Type == html.ElementNode && x.Data == "p"
		}); p != nil {
			heat = cleanHeat(textOf(p))
		}
		link := findFirst(n, func(x *html.Node) bool {
			return x.Type == html.ElementNode && x.Data == "a" && attr(x, "href") != ""
		})
		items = append(items, Item{
			Rank:  rank,
			Title: title,
			URL:   absURL(pageURL, attr(link, "href")),
			Cover: imgSrc(findFirst(n, func(x *html.Node) bool {
				return x.Type == html.ElementNode && x.Data == "img"
			})),
			Heat: heat,
		})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("页面里没有解析到榜单条目")
	}
	return normalizeItems(items), nil
}

// ---------- 黄桃短剧（冠军 / 亚军季军 / 完整排名） ----------

func fetchHuangtao(ctx context.Context, c *http.Client) ([]Item, error) {
	const pageURL = "https://aihuangdou.com/rankings/"
	raw, err := httpGet(ctx, c, pageURL)
	if err != nil {
		return nil, err
	}
	return parseHuangtao(raw, pageURL)
}

func parseHuangtao(raw []byte, pageURL string) ([]Item, error) {
	doc, err := parseHTMLDoc(raw)
	if err != nil {
		return nil, err
	}
	var items []Item

	pick := func(n *html.Node) (title, link, cover string) {
		h := findFirst(n, func(x *html.Node) bool {
			return x.Type == html.ElementNode && (x.Data == "h2" || x.Data == "h3")
		})
		title = cleanRankTitle(textOf(h))
		a := findFirst(h, func(x *html.Node) bool {
			return x.Type == html.ElementNode && x.Data == "a" && attr(x, "href") != ""
		})
		link = absURL(pageURL, attr(a, "href"))
		if img := findFirst(n, func(x *html.Node) bool {
			return x.Type == html.ElementNode && x.Data == "img" && hasClass(x, "poster-img")
		}); img != nil {
			cover = imgSrc(img)
		}
		return
	}

	// 冠军
	if lead := findFirst(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "rank-lead")
	}); lead != nil {
		title, u, cover := pick(lead)
		if title != "" {
			items = append(items, Item{Rank: 1, Title: title, URL: u, Cover: cover,
				Heat: metricOf(lead, "rank-lead-metric")})
		}
	}
	// 亚军 / 季军
	for i, n := range collect(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "rank-runner")
	}) {
		title, u, cover := pick(n)
		if title == "" {
			continue
		}
		rank := i + 2
		def := func(x *html.Node) bool {
			return x.Type == html.ElementNode && hasClass(x, "rank-runner-eyebrow")
		}
		if label := textOf(findFirst(n, def)); label != "" {
			if r := atoiSafe(digitsAfter(label, "NO.")); r > 0 {
				rank = r
			}
		}
		items = append(items, Item{Rank: rank, Title: title, URL: u, Cover: cover,
			Heat: metricOf(n, "rank-runner-side")})
	}
	// 4 名以后
	for i, n := range collect(doc, func(n *html.Node) bool {
		return n.Type == html.ElementNode && hasClass(n, "rank-row")
	}) {
		title, u, cover := pick(n)
		if title == "" {
			continue
		}
		rank := atoiSafe(textOf(findFirst(n, func(x *html.Node) bool {
			return x.Type == html.ElementNode && hasClass(x, "rank-no")
		})))
		if rank <= 0 {
			rank = i + 4
		}
		items = append(items, Item{Rank: rank, Title: title, URL: u, Cover: cover,
			Heat: metricOf(n, "rank-row-metric")})
	}
	if len(items) == 0 {
		return nil, fmt.Errorf("页面里没有解析到榜单条目")
	}
	return normalizeItems(items), nil
}

// metricOf 取榜单条目里的热度文本（优先取 <b> 里的数值）。
func metricOf(n *html.Node, className string) string {
	box := findFirst(n, func(x *html.Node) bool {
		return x.Type == html.ElementNode && hasClass(x, className)
	})
	if box == nil {
		return ""
	}
	if b := findFirst(box, func(x *html.Node) bool {
		return x.Type == html.ElementNode && x.Data == "b"
	}); b != nil {
		return cleanHeat(textOf(b))
	}
	return cleanHeat(textOf(box))
}

func digitsAfter(s, marker string) string {
	i := strings.Index(s, marker)
	if i < 0 {
		return ""
	}
	var b strings.Builder
	for _, r := range s[i+len(marker):] {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		} else if b.Len() > 0 {
			break
		}
	}
	return b.String()
}

// cleanHeat 只接受真正的热度值：纯数字、带万/W 的数字，或「1234 热度」。
// 榜单条目里的分类/集数文案（「AI短剧 全16集 9.8分」）不能当热度显示。
func cleanHeat(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "　", " "))
	if s == "" {
		return ""
	}
	if strings.Contains(s, "热度") {
		s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "热度"))
	}
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "热力指数"))
	s = strings.ReplaceAll(s, " ", "")
	if s == "" {
		return ""
	}
	// 纯数字 / 数字+万/W / 数字+分 之外一律丢弃
	ok := true
	digits := 0
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9':
			digits++
		case r == '.' || r == 'W' || r == 'w' || r == '万' || r == '+' || r == ',':
		default:
			ok = false
		}
	}
	if !ok || digits == 0 {
		return ""
	}
	return s
}

// normalizeItems 去重、补全空缺排名、按排名排序。
// 同一部剧在页面里出现多次时保留名次更靠前的那条。
func normalizeItems(items []Item) []Item {
	best := make(map[string]int, len(items))
	out := make([]Item, 0, len(items))
	for _, it := range items {
		it.Title = strings.TrimSpace(it.Title)
		if it.Title == "" {
			continue
		}
		key := strings.ToLower(it.Title)
		if idx, ok := best[key]; ok {
			if betterRank(it.Rank, out[idx].Rank) {
				out[idx] = it
			}
			continue
		}
		best[key] = len(out)
		out = append(out, it)
	}
	sort.SliceStable(out, func(i, j int) bool {
		ri, rj := out[i].Rank, out[j].Rank
		if ri <= 0 {
			ri = 1 << 30
		}
		if rj <= 0 {
			rj = 1 << 30
		}
		return ri < rj
	})
	// 补齐缺失/重复的排名：名次必须从 1 连续递增，前端 TOP 徽标才准确
	for i := range out {
		out[i].Rank = i + 1
	}
	return out
}

func betterRank(a, b int) bool {
	if a <= 0 {
		return false
	}
	if b <= 0 {
		return true
	}
	return a < b
}
