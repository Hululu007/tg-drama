package rankings

// rules_test.go —— 规则文件引擎的单测：模板、五类机制、坏条目、热加载。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tgdrama/internal/storage"
)

func mustRules(t *testing.T, text string) *compiledRules {
	t.Helper()
	rs, problems, err := compileRulesText(text)
	if err != nil {
		t.Fatalf("规则编译失败: %v（问题: %v）", err, problems)
	}
	if len(problems) != 0 {
		t.Fatalf("规则编译不应有问题: %v", problems)
	}
	return rs
}

// 缺文件 → 自动写模板 + 等价内置默认行为。
func TestRulesTemplateAutoCreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, RulesFileName)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("测试开始前模板不应存在")
	}
	rs, problems, exists, err := loadRulesFile(path)
	if err != nil {
		t.Fatalf("缺文件不该报错: %v", err)
	}
	if exists {
		t.Fatal("第一次加载 exists 应为 false")
	}
	if len(problems) != 0 {
		t.Fatalf("模板不应有问题: %v", problems)
	}
	if rs == nil || rs.fuzzyThreshold != 0.94 || rs.fuzzyMin != 4 {
		t.Fatalf("缺文件应回退内置默认: %+v", rs)
	}
	raw, err := os.ReadFile(path)
	if err != nil || len(raw) == 0 {
		t.Fatalf("模板应已写盘: %v", err)
	}
	if !strings.Contains(string(raw), "aliases") || !strings.Contains(string(raw), "blocked_titles") {
		t.Fatal("模板应包含全部五个区块的说明")
	}
}

// 别名：榜单名 → 库内名，最高优先级。
func TestRulesAlias(t *testing.T) {
	text := `
aliases:
  "岳母大人驾到": "我的岳母苏沛真"
  "隔壁的高三": "高三爱情故事"
`
	rs := mustRules(t, text)
	idx := buildShowIndexWithRules(testShows(), rs)
	sh, score, via := idx.matchReason("岳母大人驾到")
	if sh == nil || sh.ID != 1 {
		t.Fatalf("别名应指向岳母剧，实际 %+v", sh)
	}
	if via != "别名→完整名" || score != 1 {
		t.Fatalf("命中方式应为 别名→完整名/1，实际 %s/%.2f", via, score)
	}
	// 带装饰的榜单名也要先清理再查别名表
	sh, _, via = idx.matchReason("《岳母大人驾到》完整版")
	if sh == nil || sh.ID != 1 || via != "别名→完整名" {
		t.Fatalf("清理后别名应命中，实际 %+v via=%s", sh, via)
	}
	// 别名值本身可以带装饰：括号装饰被剥掉后按完整名命中
	text2 := `
aliases:
  "岳母大人驾到": "我的岳母苏沛真（高清版）"
`
	rs2 := mustRules(t, text2)
	idx2 := buildShowIndexWithRules(testShows(), rs2)
	sh, _, via = idx2.matchReason("岳母大人驾到")
	if sh == nil || sh.ID != 1 || via != "别名→完整名" {
		t.Fatalf("别名值带装饰应剥掉装饰后命中，实际 %+v via=%s", sh, via)
	}
}

// 追加去尾词：内置词表没有的装饰。
func TestRulesStripSuffix(t *testing.T) {
	idx0 := buildShowIndex(testShows())
	if sh, _ := idx0.match("高三爱情故事星光版"); sh != nil {
		t.Fatal("默认规则不该命中自造的「星光版」装饰")
	}
	text := `
strip_suffixes:
  - 星光版
  - 4K修复
`
	rs := mustRules(t, text)
	idx := buildShowIndexWithRules(testShows(), rs)
	sh, _, via := idx.matchReason("高三爱情故事星光版")
	if sh == nil || sh.ID != 8 || via != "完整名" {
		t.Fatalf("追加去尾词后应命中，实际 %+v via=%s", sh, via)
	}
	// 与内置词表共存：内置的「完整版」依然可用
	if sh, _ := idx.match("秘密教学（完整版）"); sh == nil || sh.ID != 7 {
		t.Fatalf("内置去尾词不应被规则覆盖失效: %+v", sh)
	}
}

// 正则替换：处理中间夹杂的杂质。
func TestRulesReplacement(t *testing.T) {
	text := `
replacements:
  - pattern: '^短剧[:：]'
    replace: ''
`
	rs := mustRules(t, text)
	idx := buildShowIndexWithRules(testShows(), rs)
	sh, _, via := idx.matchReason("短剧：秘密教学")
	if sh == nil || sh.ID != 7 || via != "完整名" {
		t.Fatalf("正则去前缀后应命中，实际 %+v via=%s", sh, via)
	}
	// 正则坏条目：跳过 + 问题清单，其余规则继续生效
	text2 := `
replacements:
  - pattern: '[bad'
    replace: ''
aliases:
  "隔壁的高三": "高三爱情故事"
`
	rs2, problems, err := compileRulesText(text2)
	if err != nil {
		t.Fatalf("坏正则不该让整体失败: %v", err)
	}
	if len(problems) != 1 || !strings.Contains(problems[0], "正则无法编译") {
		t.Fatalf("应报告正则问题，实际 %v", problems)
	}
	idx2 := buildShowIndexWithRules(testShows(), rs2)
	if sh, _ := idx2.match("隔壁的高三"); sh == nil || sh.ID != 8 {
		t.Fatalf("坏正则被跳过后别名仍应生效: %+v", sh)
	}
}

// 黑名单：直接拒绝。
func TestRulesBlocked(t *testing.T) {
	text := `
blocked_titles:
  - 母上攻略
  - 热播推荐
`
	rs := mustRules(t, text)
	idx := buildShowIndexWithRules(testShows(), rs)
	if sh, _, via := idx.matchReason("母上攻略"); sh != nil || via != "黑名单" {
		t.Fatalf("黑名单应拒绝，实际 %+v via=%s", sh, via)
	}
	if sh, _, via := idx.matchReason("热播推荐"); sh != nil || via != "黑名单" {
		t.Fatalf("非剧名黑名单也应拒绝，实际 %+v via=%s", sh, via)
	}
	// 黑名单只影响榜单侧条目，不影响别名之外的常规去装饰
	if sh, _ := idx.match("母上攻略 全集"); sh != nil {
		t.Fatal("带装饰的黑名单条目同样应被拒绝")
	}
}

// 模糊参数：调门槛、关模糊、短名保护。
func TestRulesFuzzy(t *testing.T) {
	// 默认门槛下「岳母苏沛真」(5字) 与「我的岳母苏沛真」(7字) dice≈0.8，不命中
	idx0 := buildShowIndex(testShows())
	if sh, _ := idx0.match("岳母苏沛真"); sh != nil {
		t.Fatalf("默认门槛不该命中 %+v", sh)
	}
	// 调低门槛 → 命中
	text := `
fuzzy:
  threshold: 0.7
`
	rs := mustRules(t, text)
	idx := buildShowIndexWithRules(testShows(), rs)
	sh, score, via := idx.matchReason("岳母苏沛真")
	if sh == nil || sh.ID != 1 || via != "模糊" {
		t.Fatalf("低门槛应模糊命中，实际 %+v %.2f via=%s", sh, score, via)
	}
	// 关闭模糊 → 同一标题也不命中
	text2 := `
fuzzy:
  enabled: false
  threshold: 0.5
`
	rs2 := mustRules(t, text2)
	idx2 := buildShowIndexWithRules(testShows(), rs2)
	if sh, _ := idx2.match("岳母苏沛真"); sh != nil {
		t.Fatalf("关闭模糊后不应命中 %+v", sh)
	}
	// 精确匹配不受 fuzzy.enabled 影响
	if sh, _ := idx2.match("我的岳母苏沛真"); sh == nil || sh.ID != 1 {
		t.Fatal("关闭模糊不能影响精确匹配")
	}
	// 门槛倒挂自动对齐并报告
	rs3, problems, err := compileRulesText("fuzzy:\n  threshold: 0.8\n  threshold_long: 0.9\n")
	if err != nil || len(problems) == 0 {
		t.Fatalf("门槛倒挂应报问题: %v %v", err, problems)
	}
	if rs3.fuzzyThresholdLong != 0.8 {
		t.Fatalf("倒挂应自动对齐，实际 %v", rs3.fuzzyThresholdLong)
	}
}

// 未知字段拼写检查 + 语法错误路径。
func TestRulesProblems(t *testing.T) {
	_, problems, err := compileRulesText("alias:\n  a: b\n")
	if err != nil {
		t.Fatalf("未知字段不该整体失败: %v", err)
	}
	if len(problems) == 0 || !strings.Contains(strings.Join(problems, ";"), "alias") {
		t.Fatalf("应提示未知字段 alias，实际 %v", problems)
	}
	if _, _, err := compileRulesText("aliases: [忘带冒号"); err == nil {
		t.Fatal("YAML 语法错误应返回 err")
	}
	if _, _, err := compileRulesText(""); err == nil {
		t.Fatal("空规则应返回 err（用户可能想清空，引导手写注释文件）")
	}
}

// 服务层：UpdateRules 热加载、坏文件保底、状态汇报。
func TestServiceRulesRoundTrip(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)
	svc := New(nil, dir, 12*time.Hour, 5*time.Second, nil)
	svc.now = func() time.Time { return now }

	st := svc.RulesStatus()
	if st.Exists {
		t.Fatal("初始状态文件不应存在")
	}
	if len(st.Problems) != 0 {
		t.Fatalf("初始不应有问题: %v", st.Problems)
	}

	// 写入别名规则 → 状态立即反映
	problems, err := svc.UpdateRules("aliases:\n  \"隔壁的高三\": \"高三爱情故事\"\n")
	if err != nil {
		t.Fatalf("UpdateRules 失败: %v", err)
	}
	if len(problems) != 0 {
		t.Fatalf("不应有问题: %v", problems)
	}
	st = svc.RulesStatus()
	if !st.Exists || st.Aliases != 1 {
		t.Fatalf("状态应显示 1 条别名: %+v", st)
	}
	if !strings.Contains(st.Content, "隔壁的高三") {
		t.Fatal("状态应返回文件原文")
	}

	// 语法错误：拒绝落盘，上一份好规则保留
	_, err = svc.UpdateRules("aliases: [忘带冒号")
	if err == nil {
		t.Fatal("语法错误应被拒绝")
	}
	st = svc.RulesStatus()
	if st.Aliases != 1 || !strings.Contains(st.Content, "隔壁的高三") {
		t.Fatal("坏文件不应覆盖上一份好规则")
	}

	// 磁盘文件换成带问题条目的规则 → 下次匹配自动生效并汇报
	if err := os.WriteFile(svc.RulesStatus().Path, []byte("replacements:\n  - pattern: '[bad'\n    replace: ''\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	res := svc.TestMatch([]string{"随便什么标题"})
	if len(res) != 1 {
		t.Fatalf("test 结果条数异常: %+v", res)
	}
	st = svc.RulesStatus()
	if len(st.Problems) != 1 || !strings.Contains(st.Problems[0], "正则无法编译") {
		t.Fatalf("坏条目应进入问题清单: %v", st.Problems)
	}

	// test 结果结构：未命中也要有 title/via 字段
	if res[0].Title != "随便什么标题" || res[0].Matched {
		t.Fatalf("未命中结果异常: %+v", res[0])
	}
}

// 规则变化必须让 Payload 的匹配缓存失效（同库同榜单但结果不同）。
func TestRulesInvalidateMatchCache(t *testing.T) {
	dir := t.TempDir()
	now := time.Unix(1_700_000_000, 0)

	// 造一个真的可播库：启用频道 → 剧 → 1 集 → 1 条来源
	db, err := storage.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	chatID, err := db.UpsertChat(&storage.Chat{TgChatID: 111, Name: "测试频道", Type: "channel", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	showID, _, err := db.UpsertShow("高三爱情故事", "高三爱情故事")
	if err != nil {
		t.Fatal(err)
	}
	epID, _, err := db.UpsertEpisode(showID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := db.UpsertSource(&storage.Source{
		EpisodeID: epID, ChatID: chatID, MessageID: 1,
		FileName: "v.mp4", DocID: 5, AccessHash: 6, FileReference: []byte("ref"),
	}); err != nil {
		t.Fatal(err)
	}

	svc := New(db, dir, 12*time.Hour, 5*time.Second, nil)
	svc.now = func() time.Time { return now }
	svc.sources = []*source{fakeSource("a", func() ([]Item, error) {
		return []Item{{Rank: 1, Title: "隔壁的高三"}}, nil
	})}
	svc.ensureBoards()
	if err := svc.Refresh(context.Background(), false); err != nil {
		t.Fatal(err)
	}
	p := svc.Payload()
	if len(p.Boards) != 1 || p.Boards[0].Matched != 0 {
		t.Fatalf("没有规则时不应命中: %+v", p.Boards)
	}
	if _, err := svc.UpdateRules("aliases:\n  \"隔壁的高三\": \"高三爱情故事\"\n"); err != nil {
		t.Fatal(err)
	}
	p = svc.Payload()
	if p.Boards[0].Matched != 1 {
		t.Fatalf("热加载别名后应命中 1 条: %+v", p.Boards[0])
	}
	if it := p.Boards[0].Items[0]; it.Show == nil || it.Show.ID != showID {
		t.Fatalf("命中的应是高三爱情故事: %+v", it)
	}
}
