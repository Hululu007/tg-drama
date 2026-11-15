package rankings

// rules.go —— 榜单名称匹配规则文件（match-rules.yaml）。
//
// 目标：用户不改代码、不重新打包，只编辑数据目录下的 match-rules.yaml
// 就能新增/调整「榜单剧名 → 本地片库」的匹配规则。程序在文件缺失时
// 自动生成带中文说明的模板；文件改动后下一次匹配自动生效（按 mtime+大小
// 检测，无需重启）；写错的条目会被跳过并通过 /api/rankings/rules 汇报。

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"tgdrama/internal/parser"
)

// RulesFileName 规则文件名（位于数据目录，与 rankings.json 同级）。
const RulesFileName = "match-rules.yaml"

// 规则文件与各类目的大小上限，防止手滑把几十 MB 的东西贴进来。
const (
	maxRulesBytes     = 256 << 10 // 单个规则文件最大 256KB
	maxAliasEntries   = 300
	maxStripEntries   = 200
	maxReplaceEntries = 100
	maxBlockedEntries = 300
	maxTitleRunes     = 120
)

// MatchRules 规则文件对应的 YAML 结构（面向用户，字段名尽量口语化）。
type MatchRules struct {
	Aliases  map[string]string `yaml:"aliases"`       // 榜单名 → 库内名，最高优先级
	Strip    []string          `yaml:"strip_suffixes"` // 追加的尾部装饰词（与内置词表合并）
	Replace  []RuleReplace     `yaml:"replacements"`   // 正则替换（进阶）
	Blocked  []string          `yaml:"blocked_titles"` // 黑名单：这些榜单条目永不匹配
	Fuzzy    RuleFuzzy         `yaml:"fuzzy"`          // 模糊匹配参数
}

// RuleReplace 一条正则替换规则。
type RuleReplace struct {
	Pattern string `yaml:"pattern"` // Go 正则表达式
	Replace string `yaml:"replace"` // 替换为（可用 $1 引用分组），留空表示删除
}

// RuleFuzzy 模糊匹配参数；零值表示沿用程序默认。
type RuleFuzzy struct {
	Enabled       *bool   `yaml:"enabled"`        // 关闭后只做精确/去装饰匹配
	MinChars      int     `yaml:"min_chars"`      // 短于该字数的标题不做模糊（默认 4）
	Threshold     float64 `yaml:"threshold"`      // 模糊门槛（默认 0.94）
	ThresholdLong float64 `yaml:"threshold_long"` // 长名门槛（默认 0.88）
	LongAt        int     `yaml:"long_at"`        // 多少个字以上算长名（默认 6）
}

// compiledRules 编译后的运行时结构；nil 等价于 builtinRules()。
type compiledRules struct {
	aliasNorm map[string]string   // Normalize(键) → 值
	strip     []string            // 内置+用户去尾词（去重）
	replaces  []compiledReplace   // 编译通过的正则
	blocked   map[string]bool     // Normalize(黑名单标题)
	fuzzyEnabled       bool
	fuzzyMin           int
	fuzzyThreshold     float64
	fuzzyThresholdLong float64
	fuzzyLongAt        int
}

type compiledReplace struct {
	re   *regexp.Regexp
	repl string
}

// builtinRules 不加载任何用户规则时的默认行为（与历史硬编码完全一致）。
func builtinRules() *compiledRules {
	return &compiledRules{
		strip:              append([]string(nil), decorationSuffixes...),
		aliasNorm:          map[string]string{},
		blocked:            map[string]bool{},
		fuzzyEnabled:       true,
		fuzzyMin:           4,
		fuzzyThreshold:     0.94,
		fuzzyThresholdLong: 0.88,
		fuzzyLongAt:        6,
	}
}

// emptyWhetherBuiltin 用户规则为空文件时也走 builtin 的全套默认值。
func (m *MatchRules) compile() (*compiledRules, []string) {
	rs := builtinRules()
	var problems []string

	// ---- 别名 ----
	if len(m.Aliases) > maxAliasEntries {
		problems = append(problems, fmt.Sprintf("aliases 超过 %d 条，请精简", maxAliasEntries))
	}
	if len(m.Aliases) > 0 {
		rs.aliasNorm = make(map[string]string, len(m.Aliases))
		for k, v := range m.Aliases {
			nk := parser.Normalize(k)
			if nk == "" {
				problems = append(problems, fmt.Sprintf("aliases 键 %q 清理后为空，已跳过", truncForMsg(k)))
				continue
			}
			if strings.TrimSpace(v) == "" {
				problems = append(problems, fmt.Sprintf("aliases 键 %q 的值为空，已跳过", truncForMsg(k)))
				continue
			}
			rs.aliasNorm[nk] = strings.TrimSpace(v)
		}
	}

	// ---- 去尾词（与内置合并去重）----
	if len(m.Strip) > maxStripEntries {
		problems = append(problems, fmt.Sprintf("strip_suffixes 超过 %d 条，请精简", maxStripEntries))
	}
	// seen 先装内置词：用户把内置词原样写进文件时不产生重复
	seen := map[string]bool{}
	for _, w := range rs.strip {
		seen[w] = true
	}
	for _, w := range m.Strip {
		w = strings.TrimSpace(w)
		if w == "" || seen[w] {
			continue
		}
		seen[w] = true
		rs.strip = append(rs.strip, w)
	}
	// 长词优先：先剥「未删减版」再轮到「未删减」，结果更干净
	sort.Slice(rs.strip, func(i, j int) bool { return len(rs.strip[i]) > len(rs.strip[j]) })

	// ---- 正则替换 ----
	if len(m.Replace) > maxReplaceEntries {
		problems = append(problems, fmt.Sprintf("replacements 超过 %d 条，请精简", maxReplaceEntries))
	}
	for i, rr := range m.Replace {
		pat := strings.TrimSpace(rr.Pattern)
		if pat == "" {
			problems = append(problems, fmt.Sprintf("replacements 第 %d 条 pattern 为空，已跳过", i+1))
			continue
		}
		if len(pat) > maxTitleRunes*2 {
			problems = append(problems, fmt.Sprintf("replacements 第 %d 条 pattern 过长，已跳过", i+1))
			continue
		}
		re, err := regexp.Compile(pat)
		if err != nil {
			problems = append(problems, fmt.Sprintf("replacements 第 %d 条正则无法编译：%v", i+1, err))
			continue
		}
		rs.replaces = append(rs.replaces, compiledReplace{re: re, repl: rr.Replace})
	}

	// ---- 黑名单 ----
	if len(m.Blocked) > maxBlockedEntries {
		problems = append(problems, fmt.Sprintf("blocked_titles 超过 %d 条，请精简", maxBlockedEntries))
	}
	if len(m.Blocked) > 0 {
		rs.blocked = make(map[string]bool, len(m.Blocked))
		for _, t := range m.Blocked {
			nt := parser.Normalize(t)
			if nt == "" {
				continue
			}
			rs.blocked[nt] = true
		}
	}

	// ---- 模糊参数 ----
	setThr, setThrLong := false, false
	if m.Fuzzy.Enabled != nil {
		rs.fuzzyEnabled = *m.Fuzzy.Enabled
	}
	if m.Fuzzy.MinChars > 0 {
		rs.fuzzyMin = clampInt(m.Fuzzy.MinChars, 2, 20)
	}
	if m.Fuzzy.Threshold > 0 {
		setThr = true
		t := m.Fuzzy.Threshold
		if t < 0.5 || t > 1.0 {
			problems = append(problems, fmt.Sprintf("fuzzy.threshold=%.2f 超出 0.5~1.0，已按边界处理", t))
			t = clampFloat(t, 0.5, 1.0)
		}
		rs.fuzzyThreshold = t
	}
	if m.Fuzzy.ThresholdLong > 0 {
		setThrLong = true
		t := m.Fuzzy.ThresholdLong
		if t < 0.5 || t > 1.0 {
			problems = append(problems, fmt.Sprintf("fuzzy.threshold_long=%.2f 超出 0.5~1.0，已按边界处理", t))
			t = clampFloat(t, 0.5, 1.0)
		}
		rs.fuzzyThresholdLong = t
	}
	if m.Fuzzy.LongAt > 0 {
		rs.fuzzyLongAt = clampInt(m.Fuzzy.LongAt, 2, 50)
	}

	// 只在用户把两个门槛都写出来且长短倒挂时自动对齐；
	// 只写一个时尊重默认值与所写值的组合，不擅自替用户做主。
	if setThr && setThrLong && rs.fuzzyThresholdLong > rs.fuzzyThreshold {
		problems = append(problems, "fuzzy.threshold_long 大于 threshold，已自动对齐")
		rs.fuzzyThresholdLong = rs.fuzzyThreshold
	}
	return rs, problems
}

func clampInt(v, lo, hi int) int {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func clampFloat(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func truncForMsg(s string) string {
	r := []rune(s)
	if len(r) <= 20 {
		return s
	}
	return string(r[:20]) + "…"
}

// loadRulesFile 读取并编译规则文件。
//   - 文件不存在：写入模板，返回默认规则（exists=false，不算错误）；
//   - YAML 语法错误：返回 err，由调用方决定沿用上一份可用规则；
//   - 条目级问题（正则编译失败、空值等）：跳过该条并记入 problems。
func loadRulesFile(path string) (rs *compiledRules, problems []string, exists bool, err error) {
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			ensureRulesTemplate(path)
			return builtinRules(), nil, false, nil
		}
		return nil, nil, false, fmt.Errorf("规则文件无法读取: %w", readErr)
	}
	if len(raw) > maxRulesBytes {
		return nil, nil, true, fmt.Errorf("规则文件超过 %dKB，请精简", maxRulesBytes>>10)
	}
	rs, problems, err = compileRulesText(string(raw))
	return rs, problems, true, err
}

// compileRulesText 把 YAML 文本编译成运行时规则。
// 语法错误返回 err（整体拒绝）；字段拼写错误只报问题（按空规则处理该字段）。
func compileRulesText(text string) (*compiledRules, []string, error) {
	if strings.TrimSpace(text) == "" {
		// 空文件视为损坏（用户可能误清空）：走「保留上一份好规则」路径更安全
		return nil, nil, errors.New("规则内容为空")
	}
	var m MatchRules
	if err := yaml.Unmarshal([]byte(text), &m); err != nil {
		return nil, nil, fmt.Errorf("YAML 解析失败: %w", err)
	}
	var problems []string
	// 宽松模式通过后再用严格模式探一遍未知字段，帮用户抓拼写错误
	var strict MatchRules
	sd := yaml.NewDecoder(bytes.NewReader([]byte(text)))
	sd.KnownFields(true)
	if serr := sd.Decode(&strict); serr != nil {
		msg := serr.Error()
		if strings.Contains(msg, "not found") {
			problems = append(problems, "存在无法识别的字段（请检查拼写，忽略大小写）: "+
				strings.Join(unknownFields(msg), ", "))
		}
	}
	rs, probs := m.compile()
	problems = append(problems, probs...)
	return rs, problems, nil
}

// unknownFields 从 yaml.v3 的报错文本里抠出字段名（一行一个 "field x not found"）。
func unknownFields(msg string) []string {
	var out []string
	for _, m := range regexp.MustCompile(`field (\S+) not found`).FindAllStringSubmatch(msg, -1) {
		if len(m) >= 2 {
			out = append(out, m[1])
		}
	}
	return out
}

// ensureRulesTemplate 尽力把模板写到磁盘（失败只打日志，不影响功能）。
func ensureRulesTemplate(path string) {
	if err := os.WriteFile(path, []byte(rulesTemplate), 0o644); err != nil {
		return // 目录只读等情况：静默跳过，下次再试
	}
}

// RulesStatus 给前端/排查用：文件内容 + 生效情况 + 问题清单。
type RulesStatus struct {
	Path     string   `json:"path"`
	Exists   bool     `json:"exists"`
	Content  string   `json:"content"`
	Problems []string `json:"problems"`
	Aliases  int      `json:"aliases"`
	Strip    int      `json:"strip_suffixes"`
	Replaces int      `json:"replacements"`
	Blocked  int      `json:"blocked_titles"`
	Fuzzy    string   `json:"fuzzy"`
}

// TestResult 单个标题的匹配预览（用户调规则的“试金石”）。
type TestResult struct {
	Title    string  `json:"title"`
	Matched  bool    `json:"matched"`
	Score    float64 `json:"score"`
	Via      string  `json:"via"` // 别名→完整名 / 别名→去装饰 / 别名→模糊 / 完整名 / 去装饰 / 模糊 / 黑名单
	ShowID   int64   `json:"show_id,omitempty"`
	ShowName string  `json:"show_name,omitempty"`
	EpCount  int     `json:"ep_count,omitempty"`
}

// rulesTemplate 首次自动生成的模板；示例全部注释，保持默认行为不变。
const rulesTemplate = `# ════════════════════════════════════════════════════════════════
#   榜单名称匹配规则（match-rules.yaml）
# ════════════════════════════════════════════════════════════════
# 作用：控制「排行榜」里的榜单剧名如何对应到你本地片库里的剧。
# 改完保存即可生效（下一次刷新榜单时自动加载，无需重启程序）。
#
# 自查小工具（浏览器地址栏/接口工具均可）：
#   GET  /api/rankings/rules                 查看当前规则与问题提示
#   POST /api/rankings/rules/test            预览匹配结果
#        请求体: {"titles": ["榜单上的剧名", "另一个"]}
#
# ── 一、别名 aliases（最直接，推荐优先用）──────────────────────
# 左边 = 榜单上显示的名字；右边 = 你片库里的剧名（要一字不差）。
# 命中别名后直接按右边的名字找剧，不再走后面的模糊猜测。
# aliases:
#   "老丈人走后，我成为了一家之主": "老丈人走后 我成为了一家之主"
#   "妈妈的朋友2最终章": "妈妈的朋友2"
#
# ── 二、追加去尾词 strip_suffixes ──────────────────────────────
# 榜单剧名结尾的装饰词（程序内置了 完整版/高清/合集/第N季 等，无需重复写）。
# 这里补充内置没有的词，匹配前会从结尾剥掉。
# strip_suffixes:
#   - 4K修复
#   - 番外篇
#   - 星光版
#
# ── 三、正则替换 replacements（进阶，懂正则再用）────────────────
# 对剧名做通用替换，先于去尾词执行。pattern 用 Go 正则语法。
# replacements:
#   - pattern: '^短剧[:：]'      # 去掉开头「短剧：」前缀
#     replace: ''
#   - pattern: '[【】]'          # 去掉方头括号
#     replace: ''
#   - pattern: '第([0-9一二三四五六七八九十]+)季$'
#     replace: ''                # 去掉结尾「第N季」（内置已处理，仅作演示）
#
# ── 四、黑名单 blocked_titles ─────────────────────────────────
# 这些榜单条目永远不参与匹配（例如站点塞的广告位、聚合词）。
# blocked_titles:
#   - 热播推荐
#   - 更多精彩
#
# ── 五、模糊匹配参数 fuzzy ────────────────────────────────────
# 精确匹配（含去尾词）都没命中时，按字形相似度兜底。
#   enabled        改成 false 后只做精确/去装饰匹配，绝不猜（最保守）
#   threshold      相似度门槛 0~1，越大越严格（默认 0.94）
#   threshold_long 长剧名门槛（默认 0.88，更宽松）
#   long_at        多少个字算长名（默认 6）
#   min_chars      低于该字数的短名不做模糊（默认 4，防止两字剧名误配）
# fuzzy:
#   enabled: true
#   threshold: 0.94
#   threshold_long: 0.88
#   long_at: 6
#   min_chars: 4
#
# 常见问题：
#   · YAML 语法：冒号和逗号后要有空格；含冒号/引号的值请用引号包起来。
#   · 写错的条目会被跳过，并在 GET /api/rankings/rules 的 problems 里提示。
#   · 改坏了又找不到原因？把本文件清空成只有注释也能正常工作（等于默认规则）。
`
