package rankings

import (
	"regexp"
	"sort"
	"strings"

	"tgdrama/internal/parser"
	"tgdrama/internal/storage"
)

// 片名后缀装饰词：站点喜欢在榜单标题上挂「合集」「完整版」「更新至20集」这类尾巴，
// 本地库里的剧名通常没有。匹配前统一剥掉，但不改动剧名主体。
var decorationSuffixes = []string{
	"导演剪辑版", "未删减版", "无删减版", "完整版", "加长版", "珍藏版", "修复版",
	"未删减", "无删减", "高清版", "超清版", "蓝光版", "国语版", "粤语版", "中字版",
	"高清", "超清", "蓝光", "国语", "粤语", "中字", "中文字幕", "双语",
	"合集", "全集", "已完结", "连载中",
}

var (
	reTailEpisodes = regexp.MustCompile(`(?:全|共)?\d+集(?:全|完结)?$|更新(?:至|到)\d+集$|第\d+集$`)
	reTailSeason   = regexp.MustCompile(`第(?:\d+|[一二三四五六七八九十]+)季$`)
	reDecoBracket  = regexp.MustCompile(`[（(【\[]\s*(?:合集|全集|完整版|未删减|无删减|高清|超清|蓝光|国语|中字|中文字幕|已完结|连载中|第[\d一二三四五六七八九十]+季)\s*[)）】\]]`)
)

// cleanRankTitle 去掉书名号、多余空白等展示层装饰。
func cleanRankTitle(s string) string {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "《》〈〉「」『』\"'“”‘’ ")
	s = strings.Join(strings.Fields(s), " ")
	return s
}

// titleVariants 生成用于匹配的归一化变体。
// 例：「我的岳母苏沛真（完整版）」→ [我的岳母苏沛真完整版 我的岳母苏沛真]
func titleVariants(s string) []string {
	base := cleanRankTitle(s)
	if base == "" {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		n := parser.Normalize(v)
		if n == "" || seen[n] {
			return
		}
		seen[n] = true
		out = append(out, n)
	}

	add(base)
	// 去掉括号装饰
	cur := reDecoBracket.ReplaceAllString(base, "")
	add(cur)
	for i := 0; i < 3; i++ {
		next := cur
		for _, suf := range decorationSuffixes {
			next = strings.TrimSuffix(next, suf)
		}
		next = reTailEpisodes.ReplaceAllString(next, "")
		if next == cur {
			break
		}
		cur = next
		add(cur)
	}
	// 季数单独处理：先按「带季」精确匹配，匹配不到再用「去季」兜底
	if loc := reTailSeason.FindStringIndex(cur); loc != nil {
		add(cur[:loc[0]])
	}
	if cur != base {
		if loc := reTailSeason.FindStringIndex(base); loc != nil {
			add(base[:loc[0]])
		}
	}
	return out
}

type showEntry struct {
	show  *storage.Show
	grams map[string]int
}

type showIndex struct {
	entries []*showEntry
	// byExact 完整归一化名；byLoose 去掉「合集/完整版/第 N 季」等装饰后的名字。
	// 分开存是为了优先命中完整名：库里同时有「某剧」和「某剧第二季」时，
	// 榜单写「某剧第二季」不会因为集数多就被错配到第一季。
	byExact map[string][]*storage.Show
	byLoose map[string][]*storage.Show
}

func buildShowIndex(shows []*storage.Show) *showIndex {
	idx := &showIndex{
		byExact: make(map[string][]*storage.Show, len(shows)),
		byLoose: make(map[string][]*storage.Show, len(shows)*2),
	}
	for _, sh := range shows {
		if sh == nil {
			continue
		}
		norm := parser.Normalize(sh.Name)
		if norm == "" {
			continue
		}
		e := &showEntry{show: sh, grams: bigrams(norm)}
		idx.entries = append(idx.entries, e)
		idx.byExact[norm] = append(idx.byExact[norm], sh)
		for _, v := range titleVariants(sh.Name) {
			if v != norm {
				idx.byLoose[v] = append(idx.byLoose[v], sh)
			}
		}
	}
	return idx
}

// match 返回最可信的本地短剧与匹配分（0 表示没匹配上）。
//
// 规则刻意保守：本地库是用户的片库，错配比漏配更烦人。
//   - 完整归一化名相同：直接命中；
//   - 去掉「合集/完整版/第 N 集」等装饰后相同：命中；
//   - 两个字的短名只接受完整匹配，避免「艳母」错配到「艳母的秘密」；
//   - 其余走二元组相似度，且要求长度接近。
func (idx *showIndex) match(title string) (*storage.Show, float64) {
	variants := titleVariants(title)
	if len(variants) == 0 {
		return nil, 0
	}
	// 1) 完整归一化名（最可靠）
	for _, v := range variants {
		if list := idx.byExact[v]; len(list) > 0 {
			return pickBest(list), 1
		}
	}
	// 2) 去掉装饰词后的名字
	for _, v := range variants {
		if list := idx.byLoose[v]; len(list) > 0 {
			return pickBest(list), 0.98
		}
	}

	// 2) 模糊匹配（短名不参与）
	key := variants[0]
	if len([]rune(key)) < 4 {
		return nil, 0
	}
	keyRunes := []rune(key)
	keyGrams := bigrams(key)
	var best *storage.Show
	bestScore := 0.0
	for _, e := range idx.entries {
		name := parser.Normalize(e.show.Name)
		if name == "" {
			continue
		}
		nameRunes := []rune(name)
		// 长度差太大直接跳过，避免长标题吃掉短标题
		if abs(len(nameRunes)-len(keyRunes)) > maxInt(3, len(keyRunes)/3) {
			continue
		}
		score := diceGrams(keyGrams, len(keyRunes), e.grams, len(nameRunes))
		if score > bestScore {
			bestScore = score
			best = e.show
		}
	}
	if best == nil {
		return nil, 0
	}
	minLen := minInt(len(keyRunes), len([]rune(parser.Normalize(best.Name))))
	threshold := 0.94
	if minLen >= 6 {
		threshold = 0.88
	}
	if bestScore >= threshold {
		return best, bestScore
	}
	return nil, 0
}

func pickBest(list []*storage.Show) *storage.Show {
	if len(list) == 1 {
		return list[0]
	}
	cp := append([]*storage.Show(nil), list...)
	sort.SliceStable(cp, func(i, j int) bool {
		if cp[i].EpCount != cp[j].EpCount {
			return cp[i].EpCount > cp[j].EpCount
		}
		return cp[i].UpdatedAt > cp[j].UpdatedAt
	})
	return cp[0]
}

// bigrams 预先算好二元组，模糊匹配时避免反复分配。
func bigrams(s string) map[string]int {
	r := []rune(s)
	m := make(map[string]int, len(r))
	for i := 0; i+1 < len(r); i++ {
		m[string(r[i:i+2])]++
	}
	return m
}

// diceGrams 用预计算好的二元组表算 Dice 相似度，避免逐对分配 map。
func diceGrams(ag map[string]int, aLen int, bg map[string]int, bLen int) float64 {
	if aLen < 2 || bLen < 2 {
		return 0
	}
	inter := 0
	for k, ca := range ag {
		if cb := bg[k]; cb > 0 {
			inter += minInt(ca, cb)
		}
	}
	return 2 * float64(inter) / float64(aLen+bLen-2)
}

// matchBoard 把一份榜单匹配到本地库；同一部剧在榜里出现多次只保留名次最高的一次。
func matchBoard(items []Item, idx *showIndex) []MatchedItem {
	out := make([]MatchedItem, 0, len(items))
	seen := make(map[int64]bool, len(items))
	for _, it := range items {
		sh, score := idx.match(it.Title)
		if sh == nil || sh.ID <= 0 {
			continue
		}
		if seen[sh.ID] {
			continue
		}
		seen[sh.ID] = true
		out = append(out, MatchedItem{
			Rank:  it.Rank,
			Title: it.Title,
			Heat:  it.Heat,
			URL:   it.URL,
			Show:  sh,
			score: score,
		})
	}
	// 名次必须保持榜单原顺序（已按 rank 升序）
	sort.SliceStable(out, func(i, j int) bool { return out[i].Rank < out[j].Rank })
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func abs(a int) int {
	if a < 0 {
		return -a
	}
	return a
}
