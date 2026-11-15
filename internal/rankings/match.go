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
	reDecoBracket  = regexp.MustCompile(`[（(【\[]\s*(?:合集|全集|完整版|未删减|无删减|高清|超清|蓝光|国语|中字|中文字幕|已完结|连载中|第[\d一二三四五六七八九十]+季)版?\s*[)）】\]]`)
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
	return buildVariants(s, decorationSuffixes, nil)
}

// buildVariants 变体生成核心，供无规则（titleVariants）与用户规则
// （rules.variants）共用：正则替换 → 去括号装饰 → 循环去尾词 → 去集数尾巴 → 季数变体。
func buildVariants(s string, strip []string, replaces []compiledReplace) []string {
	base := cleanRankTitle(s)
	if base == "" {
		return nil
	}
	// 用户正则替换最先执行：让规则有机会处理中间夹杂的杂质
	for _, rr := range replaces {
		base = rr.re.ReplaceAllString(base, rr.repl)
	}
	if strings.TrimSpace(base) == "" {
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
		for _, suf := range strip {
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

// variants 用户规则视角的变体（nil 规则 = 内置默认）。
func (rs *compiledRules) variants(s string) []string {
	if rs == nil {
		return titleVariants(s)
	}
	return buildVariants(s, rs.strip, rs.replaces)
}

type showEntry struct {
	show  *storage.Show
	grams map[string]int
}

type showIndex struct {
	// rules 当前生效的用户规则；nil 表示内置默认（历史行为）。
	rules *compiledRules
	entries []*showEntry
	// byExact 完整归一化名；byLoose 去掉「合集/完整版/第 N 季」等装饰后的名字。
	// 分开存是为了优先命中完整名：库里同时有「某剧」和「某剧第二季」时，
	// 榜单写「某剧第二季」不会因为集数多就被错配到第一季。
	byExact map[string][]*storage.Show
	byLoose map[string][]*storage.Show
}

func buildShowIndex(shows []*storage.Show) *showIndex {
	return buildShowIndexWithRules(shows, nil)
}

// buildShowIndexWithRules 按指定规则构建匹配索引（rules 为 nil 时用内置默认）。
func buildShowIndexWithRules(shows []*storage.Show, rules *compiledRules) *showIndex {
	idx := &showIndex{
		rules:   rules,
		byExact: make(map[string][]*storage.Show, len(shows)),
		byLoose: make(map[string][]*storage.Show, len(shows)*2),
	}
	variantsOf := titleVariants
	if rules != nil {
		variantsOf = rules.variants
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
		for _, v := range variantsOf(sh.Name) {
			if v != norm {
				idx.byLoose[v] = append(idx.byLoose[v], sh)
			}
		}
	}
	return idx
}

// match 返回最可信的本地短剧与匹配分（0 表示没匹配上）。
func (idx *showIndex) match(title string) (*storage.Show, float64) {
	sh, score, _ := idx.matchReason(title)
	return sh, score
}

// matchReason 与 match 相同，额外返回命中方式，供 /api/rankings/rules/test
// 预览「这条榜单名到底是怎么配上的」。via ∈ 黑名单 / 别名→完整名 / 别名→去装饰 /
// 别名→模糊 / 完整名 / 去装饰 / 模糊；未命中返回空串。
//
// 规则刻意保守：本地库是用户的片库，错配比漏配更烦人。
//   - 黑名单条目（含其任一变体）直接拒绝；
//   - 命中别名时按别名值再走一遍匹配，不再回退（写错别名后果自负）；
//   - 完整归一化名相同：直接命中；
//   - 去掉「合集/完整版/第 N 集」等装饰后相同：命中；
//   - 短于模糊门槛的短名只接受完整匹配，避免「艳母」错配到「艳母的秘密」；
//   - 其余走二元组相似度，且要求长度接近。
func (idx *showIndex) matchReason(title string) (*storage.Show, float64, string) {
	rules := idx.rules
	if rules == nil {
		rules = builtinRules()
	}
	variants := rules.variants(title)
	if len(variants) == 0 {
		return nil, 0, ""
	}
	// 0) 黑名单：任一变体命中即拒绝（写「母上攻略（合集）」也躲不掉）
	for _, v := range variants {
		if rules.blocked[v] {
			return nil, 0, "黑名单"
		}
	}
	// 1) 别名：最高优先级，直接指向库内剧名。对每个变体都查一遍别名表，
	//    所以「岳母大人驾到」和「岳母大人驾到 完整版」都能指向同一部剧。
	if len(rules.aliasNorm) > 0 {
		for _, v := range variants {
			if val, ok := rules.aliasNorm[v]; ok {
				sh, score, via := idx.matchTitle(val, rules)
				if sh != nil {
					return sh, score, "别名→" + via
				}
				// 别名指向的名字也没匹配上：按用户意图不回退
				return nil, 0, ""
			}
		}
	}
	return idx.matchVariants(variants, rules)
}

// matchTitle 标准三级流水线（不处理别名/黑名单），rules 决定变体与模糊参数。
func (idx *showIndex) matchTitle(title string, rules *compiledRules) (*storage.Show, float64, string) {
	return idx.matchVariants(rules.variants(title), rules)
}

func (idx *showIndex) matchVariants(variants []string, rules *compiledRules) (*storage.Show, float64, string) {
	if len(variants) == 0 {
		return nil, 0, ""
	}
	// a) 完整归一化名（最可靠）
	for _, v := range variants {
		if list := idx.byExact[v]; len(list) > 0 {
			return pickBest(list), 1, "完整名"
		}
	}
	// b) 去掉装饰词后的名字
	for _, v := range variants {
		if list := idx.byLoose[v]; len(list) > 0 {
			return pickBest(list), 0.98, "去装饰"
		}
	}

	// c) 模糊匹配（短名不参与；可被规则关闭）
	if !rules.fuzzyEnabled {
		return nil, 0, ""
	}
	key := variants[0]
	keyRunes := []rune(key)
	if len(keyRunes) < rules.fuzzyMin {
		return nil, 0, ""
	}
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
		return nil, 0, ""
	}
	minLen := minInt(len(keyRunes), len([]rune(parser.Normalize(best.Name))))
	threshold := rules.fuzzyThreshold
	if minLen >= rules.fuzzyLongAt {
		threshold = rules.fuzzyThresholdLong
	}
	if bestScore >= threshold {
		return best, bestScore, "模糊"
	}
	return nil, 0, ""
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
