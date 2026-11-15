// Package parser 负责从短剧频道消息的文本/文件名中提取剧名、集数与 #标签
package parser

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Parsed 解析结果
type Parsed struct {
	ShowName   string       // 清洗后的剧名
	NormName   string       // 归一化聚合键
	EpisodeNo  int          // 集号，0 表示未能识别；范围时为起始集
	EpEnd      int          // 范围结束集号，与 EpisodeNo 相同表示单集
	Tags       []string     // #标签（不含 #）
	HasTitle   bool         // 剧名是否来自《》/【】容器或 #标签（可信度高于正文推测）
	FromTag    bool         // 剧名取自 #标签（频道把剧名写在 # 后面，比正文更可靠）
	Container  bool         // 原文里确实有《》/【】标题容器（FromTag 不会把它置位）
	TagName    string       // 被采用的 #标签原文（FromTag 为 true 时有效）
	TitleName  string       // 标题里的候选剧名（《》/【】或标题区），供标签语料校验
	ProgressEp bool         // 集号取自「更新至第N集」这类进度说明，不代表本条视频的集号
	Multi      []MultiEntry // 多标题预览帖：一条 caption 依次列出的多部剧（>=2 项时有效）
}

// MultiEntry 多标题预览帖里的一条「剧名 + 集号」声明。
type MultiEntry struct {
	Name string
	Ep   int
}

// reMultiTitle 匹配《》/〈〉 书名号或非垃圾【剧名】容器。
// 不收 「」『』：它们在中文/日文文案里是对话引号，当容器会把台词、昵称
// （「射臉上」「Chloe Lin」）建成剧名。
var reMultiTitle = regexp.MustCompile(`[《〈【]([^》〉】]{1,40})[》〉】]`)

// MultiEntries 解析「一条相册 caption 列多部剧预览」的格式，例如
// 「《母上攻略》第6集 《兄弟妈妈好好操》第7集 / 《琼明女神录》最新剧集」。
// 出现 >=2 个标题容器时按出现顺序返回，附各自紧随其后的集号（没有则 0）。
func MultiEntries(text string) []MultiEntry {
	text = normalizeTitleWidth(titleZone(text))
	locs := reMultiTitle.FindAllStringSubmatchIndex(text, 12)
	if len(locs) < 2 {
		return nil
	}
	out := make([]MultiEntry, 0, len(locs))
	for i, loc := range locs {
		if !pairedTitleContainer(text[loc[0]:loc[1]]) {
			return nil
		}
		name := TrimNameNoise(cleanName(text[loc[2]:loc[3]]), false)
		if name == "" || isGenericTag(name) || isEpisodeishTitle(name) {
			return nil
		}
		// 集号只在本标题到下一个标题之间的片段里找
		end := len(text)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		seg := text[loc[1]:end]
		ep := 0
		if r := []rune(seg); len(r) > 24 {
			seg = string(r[:24])
		}
		if e, _, _, ok := extractEpisode(text[loc[2]:loc[3]], false); ok {
			ep = e
		} else if e, _, _, ok := extractEpisode(seg, false); ok {
			ep = e
		}
		out = append(out, MultiEntry{Name: name, Ep: ep})
	}
	// 同一个剧的两种写法（《双生道侣》与《双生道侣 / 小院青梅先门将至》）或
	// 完全相同的两个容器，不是两个预览条目：去重后再判断条目数。
	uniq := out[:0]
	for _, e := range out {
		n := Normalize(e.Name)
		dup := false
		for _, u := range uniq {
			un := Normalize(u.Name)
			if n == un || strings.Contains(un, n) || strings.Contains(n, un) {
				dup = true
				break
			}
		}
		if !dup {
			uniq = append(uniq, e)
		}
	}
	// 真正的「多标题预览帖」一次会列 3~6 部剧；只有两个容器时更可能是
	// 「【本剧标题】…简介里引用《别的剧》」，不能按顺序强行分配。
	if len(uniq) < 3 {
		return nil
	}
	return uniq
}

// Parser 短剧解析器
type Parser struct{}

// New 创建解析器
func New() *Parser { return &Parser{} }

var (
	// #标签，支持中文、字母、数字、下划线
	reTag = regexp.MustCompile(`#([\p{L}\p{N}_]+)`)
	// 第1集 / 第01集 / 第一集 / 第1话 / 第1回 / 第1-10集 / 第1~10集
	// 注意：不收「期」——频道里的「第049期」是开发日志期号，不是集号
	reCNEpisode = regexp.MustCompile(`第\s*([0-9]{1,4}|[零一二三四五六七八九十百千两]{1,4})\s*(?:[-~－～—–至到]\s*([0-9]{1,4}|[零一二三四五六七八九十百千两]{1,4}))?\s*(?:[集话話回]|合集|合辑|合輯|全集)`)
	// 1-10集 / 01~10集（无「第」也无 EP 的区间写法）
	reNumRange = regexp.MustCompile(`(?:^|[^0-9])0*([0-9]{1,3})\s*[-~－～—–至到]\s*0*([0-9]{1,3})\s*(?:[集话話回]|合集|合辑|合輯|全集)`)
	// EP1 / E01 / ep 12 / EP-7 / EP9预览（RE2 不支持 lookahead，改用「数字后必须非数字」）
	reEP = regexp.MustCompile(`(?i)\bEP[ _\-]?0*([0-9]{1,3})(?:[^0-9]|$)`)
	// 单独的 E1 / E01：限制 2 位，避免把哈希里 "e452" 这类片段当成集号
	reEShort = regexp.MustCompile(`(?i)\bE[ _\-]?0*([0-9]{1,2})(?:[^0-9]|$)`)
	// EP01-10 / EP1~EP10 / E3-5
	reEPRange = regexp.MustCompile(`(?i)\bEP?[ _]?0*([0-9]{1,3})\s*[-~－～]\s*(?:EP?[ _]?0*)?([0-9]{1,3})\b`)
	// 裸数字兜底：_01 / -01 / .01 / [01] / 【01】 / 空格01
	reBare = regexp.MustCompile(`(?:[\s_\-\.\[\(【】])0*([0-9]{1,3})(?:[\s_\-\.\]\)】]|$)`)
	// 紧贴汉字的集号兜底：剧名3 / 剧名01 等（文件名常见「xxx1_watermarked.mp4」）。
	// 前导零纳入捕获组，保证截断点在第一个数字之前；纯 0 不是有效集号。
	reAttached = regexp.MustCompile(`\p{Han}(0*[1-9][0-9]{0,2})(?:[\s_\-\.\]\)】]|$)`)
	// 《》/〈〉 书名号与非垃圾【】容器内多为正式剧名，优先采用。
	// 「」『』 是对话引号（中文/日文台词），不能当剧名容器。
	reBook = regexp.MustCompile(`[《〈]([^》〉]{1,60})[》〉]`)
	// 成对引号引出剧名：只在「行首（允许前面有【厂牌】/方括号等装饰）+ 引导 + 名字 +
	// 集号」这种明确标题写法时成立（真实库：『情趣318』第1-2集 / 「存在感：零」EP7）。
	// 其余引号是台词，不是剧名。
	reQuotedTitle = regexp.MustCompile(`^\s*(?:[【\[(（][^】\])）]{0,40}[】\])）]|\s)*[『「]([^』」]{1,40})[』」]\s*(?:第\s*[0-9零一二三四五六七八九十百千两]{1,4}\s*(?:[-~－～—–至到]\s*(?:[0-9零一二三四五六七八九十百千两]{1,4}))?\s*(?:[集话話回]|合集|合辑|合輯)|\bEP?\s*-?\s*[0-9]{1,3}|[0-9]{1,4}\s*(?:全集|合集))`)
	// 【方括号】同样是常见剧名容器（如 【母上攻略】EP18）
	reSquare = regexp.MustCompile(`【([^】]{1,60})】`)
	// 括号内容为营销/推广噪声或频道水印的标志词
	reBracketJunk = regexp.MustCompile(`(?i)会员|内群|内部|置顶|同步|更新|连载|已更|完整|预览|投喂|邀请|关注|投稿|合作|广告|粉丝|福利|频道|短剧|传媒|影视|影业|工作室|出品|官方|资源|剧场|爆款|新作|上映|[0-9]{1,3}\s*[集期话回]|\bEP?[ _]?0*[0-9]`)

	// 正文起始标志：从这里往后都是剧情介绍/简介，不再参与集号与剧名解析
	reSectionCut = regexp.MustCompile(`(?i)(剧情介绍|剧情简介|内容简介|剧集简介|剧集介绍|故事简介|剧情|简介|介绍|描述|标签|看点|主演|导演|类型)\s*[:：]`)
	// 括号块，多为频道水印或广告
	reBrackets = regexp.MustCompile(`【[^】]*】|\[[^\]]*\]|「[^」]*」|（[^）]*）|\([^)]*\)`)
	// 画质与营销噪声词（watermarked/watermarks 等整体清除）
	// 含采集站标题尾巴：短剧视频在线观看vert黄果短剧 merge / md5 哈希
	reNoise = regexp.MustCompile(`(?i)(4k|2k|2160p|1080p|720p|480p|蓝光|超清|高清|标清|中字|字幕|完整版|未删减|无删减|抢先版|watermark\w*|\bhd\b|\bh264\b|\bh265\b|\bmp4\b|\bmkv\b|短剧视频(?:在线观看)?|在线观看|\bvert\b|\bmerge\b|md5\s*[0-9a-f]+|tg\s*[:：]?\s*@?\s*[a-z0-9_]{3,}|黄果(?:短剧)?|黄豆(?:短剧)?)`)

	// reAdTail 引流话术尾巴：频道把转化话术直接拼在标题后面
	// （「风流御厨 下 群主推荐超稳定VPN」「某剧 点此开通 立刻进群追更」）。
	// 这些短语不会出现在真正的剧名里，清洗时整段剥掉，剩下的才是剧名。
	reAdTail = regexp.MustCompile(`(?i)(?:群主推荐[^\s]{0,12}|超稳定\s*vpn|\bvpn\b|飞机号|内部群|会员群|官方群|交流群|内部片库|完整片库|预告频道|点此开通|点此自助|点按钮|机器人开通|立刻进群追更|自助入群|自助加入|自助开通|全网最低价|tg全域推广|官方客服|[0-9]+\s*秒进群)`)

	// reAdCTA 明显的转化动作：一条 caption 里出现这些短语，基本就是
	// 「预告片 + 拉人进付费群」的引流帖，不是剧集本身。
	reAdCTA = regexp.MustCompile(`(?i)(?:进群|入群|加群|群里|会员群|内部群|内部片库|点此开通|点此自助|点按钮|开通后|机器人开通|自助开通|自助入群|立刻进群|秒进群|完整片库|预告频道|付费一次|按月|包月|全网最低价|官方客服|群主推荐|24\s*小时自助|\bvpn\b|飞机号|观看完整视频|完整视频观看|点此看全|点此观看|点此查看|看到结局|等预告|追更|tg搜|点击购买|[0-9]+(?:\.[0-9]+)?\s*元)`)
)

// Parse 从 caption（优先）或文件名解析短剧信息
func (p *Parser) Parse(caption, fileName string) (*Parsed, error) {
	text := strings.TrimSpace(caption)
	fromCaption := text != ""
	if !fromCaption {
		// 文件名常带双扩展名（xxx.mov.mp4）或「名称 .mov」这种空格残留，
		// 先去掉末尾视频扩展名再解析，否则会建出「乐园触手 mov」这类剧名
		text = stripTrailingVideoExt(strings.TrimSpace(fileName))
	}
	text = normalizeTitleWidth(text)

	tags := extractTags(text)
	rawText := text
	// 厂牌标记（AI短剧/AI成人短剧）不是剧名，先整体剥掉；文件名路径同样适用。
	text = reChannelBrand.ReplaceAllString(text, " ")
	text = reAIBrand.ReplaceAllString(text, " ")
	text = reTag.ReplaceAllString(text, " ")
	// 集号只在「标题区」里找：正文/简介里的「更新到第十六集」「第049期」不是本集集号
	head := titleZone(text)

	// 以视频扩展名结尾的 caption 实际是文件名，允许裸数字集号
	ep, epEnd, idx, ok, epProgress := extractEpisodeEx(head, !fromCaption || looksLikeFileName(text))

	// 剧名候选：标题容器（《》>【】）与标题区正文分别取一个，
	// 再和 #标签 做「双重验证」，避免把 #姐姐 这类分类词当剧名。
	titleName := ""
	hasTitleContainer := false
	tagName := ""
	if fromCaption {
		tagName, _ = pickShowTag(rawText)
	}
	// 容器只在标题区里找：简介里引用的《别的剧》（「作品简介：《强制怀孕配额法》」）
	// 不是本剧的名字；标题区里没有容器时才退回全文。
	scan := titleZone(rawText)
	bookName, bookIdx := "", -1
	squareName, squareIdx := "", -1
	findContainers := func(s string) {
		bookName, bookIdx = "", -1
		squareName, squareIdx = "", -1
		for _, m := range reBook.FindAllStringSubmatchIndex(s, -1) {
			if !pairedTitleContainer(s[m[0]:m[1]]) {
				continue
			}
			if cleaned := cleanName(s[m[2]:m[3]]); cleaned != "" {
				bookName, bookIdx = cleaned, m[0]
				break
			}
		}
		for _, m := range reSquare.FindAllStringSubmatchIndex(s, -1) {
			if reBracketJunk.MatchString(s[m[2]:m[3]]) {
				continue
			}
			if cleaned := cleanName(s[m[2]:m[3]]); cleaned != "" {
				squareName, squareIdx = cleaned, m[0]
				break
			}
		}
	}
	findContainers(scan)
	if bookIdx < 0 && squareIdx < 0 && scan != text {
		findContainers(text)
	}
	// 行首「引号剧名」+ 集号：真实频道的正式标题写法（『情趣318』第1-2集）。
	// 只在这种结构成立时采纳，避免把台词里的引号当剧名。
	if bookIdx < 0 && squareIdx < 0 {
		if m := reQuotedTitle.FindStringSubmatchIndex(scan); m != nil {
			if cleaned := cleanName(scan[m[2]:m[3]]); cleaned != "" {
				bookName, bookIdx = cleaned, m[0]
			}
		}
	}
	switch {
	case bookIdx >= 0 && squareIdx >= 0:
		// 两种容器都在时，优先与 #标签 完全一致的那个：
		// 【AI&真人女优梦幻联动 乐园触手】与《乐园触手》并存，#乐园触手 指明了后者；
		// 都没有命中标签时，仍以《》为正式剧名。
		bn, sn, tn := Normalize(bookName), Normalize(squareName), Normalize(tagName)
		switch {
		case tn != "" && sn == tn:
			titleName, hasTitleContainer = squareName, true
		case tn != "" && bn == tn:
			titleName, hasTitleContainer = bookName, true
		default:
			// 都没命中标签时以《》为正式剧名：【爱琳】《我在斗罗开后宫》、
			// 【全集完结】《春暖桃花开》里的方括号只是作者/状态，不是剧名。
			titleName, hasTitleContainer = bookName, true
		}
	case bookIdx >= 0:
		titleName, hasTitleContainer = bookName, true
	case squareIdx >= 0:
		titleName, hasTitleContainer = squareName, true
	}
	if titleName == "" {
		nameSource := head
		if ok && idx > 0 && idx <= len(head) {
			nameSource = head[:idx]
		}
		titleName = cleanName(nameSource)
		if titleName == "" {
			// 剧名可能出现在集数之后，退回标题区清洗（不用全文，避免长简介变成剧名）
			titleName = cleanName(head)
		}
	}
	// 标记必须带数字整体剥离，不能逐字裁掉「第/集/期」：它们也可能是正式剧名尾字。
	titleName = strings.TrimRight(titleName, " .-_.·")
	// 标题里残留的集号/范围尾巴（「长生录1-10」「全12」「第二季 EP-2」）不是
	// 剧名的一部分；文件名兜底时连开头的「第一集」也剥掉。
	titleName = TrimNameNoise(titleName, !fromCaption)
	// 标题候选只剩「第3集 全系列」这类集号+营销尾巴时不是剧名，交给 #标签 定名
	if isEpisodeishTitle(titleName) {
		titleName = ""
	}
	// 帖子带了 # 标签，但没有任何一个能当剧名（全是 #美少女 #白袜 这种分类词），
	// 又没有《》/【】标题容器：剩下的「标题」其实是描述文案，不建剧——
	// 改用文件名里的剧名；文件名也是哈希时由 ingest 丢弃。
	if fromCaption && !hasTitleContainer && tagName == "" &&
		len(reTag.FindAllString(rawText, -1)) > 0 && looksLikeProse(titleName) {
		if fb := nameFromFileName(fileName); fb != "" && !IsJunkShowName(fb) {
			titleName = fb
		} else {
			return &Parsed{Tags: tags}, errUnrecognized
		}
	}

	// 没有 caption、也没有《》/【】容器时，只能靠文件名：统一走 nameFromFileName，
	// 它会把竖线分段、厂牌、水印、粘连的集号一起剥掉（「偷窥4」「我的邪恶APP3」）。
	// 有容器时容器名更可信（「AI短剧《教师凌辱》完整版_…」不能把后面的描述当成剧名）。
	if !fromCaption && !hasTitleContainer {
		if fb := nameFromFileName(fileName); fb != "" {
			titleName = fb
		}
	}
	name, fromTag := chooseName(titleName, tagName, hasTitleContainer, rawText)
	// 定名后再剥一次集号/范围尾巴：#标签 本身就可能带集号（#亿粉归零第3集）。
	name = TrimNameNoise(name, false)
	// 剧名完全靠 #标签 定、且原文没有《》/【】容器时，标签很可能只是分类词
	// （#制服 / #原相机 / #漫剧）。不少频道把真正的剧名写在视频文件名里，
	// # 后面全是分类——这时用文件名里的名字，避免建出「制服」这种假剧。
	if fromTag && !hasTitleContainer && tagNameSuspicious(name, titleName, rawText) {
		if fb := nameFromFileName(fileName); fb != "" && !IsJunkShowName(fb) && Normalize(fb) != "" {
			name, fromTag = fb, false
		} else {
			return &Parsed{Tags: tags}, errUnrecognized
		}
	}
	containerFound := hasTitleContainer
	if fromTag {
		hasTitleContainer = true
	}
	// 没有《》/【】标题容器的超长「剧名」基本都是营销文案/引流帖，
	// 不建剧（交给文件名兜底，文件名也垃圾时 ingest 会丢弃）。
	if fromCaption && !hasTitleContainer && utf8.RuneCountInString(name) > 30 {
		return &Parsed{Tags: tags}, errUnrecognized
	}
	// 剧名只是泛分类词（露脸/大学生/颜值）或者整名是拉群话术：这不是短剧，
	// 交给文件名兜底；文件名也是哈希/编号时 ingest 直接丢弃。
	// 标题容器（《》/【】）里的名字是频道自己写的正式剧名，即便撞上分类词
	// （《偷窥》《户外》）也按剧名处理；只有「靠 #标签 定名」时才怀疑。
	if IsJunkShowName(name) && !(hasTitleContainer && !fromTag) {
		return &Parsed{Tags: tags}, errUnrecognized
	}
	norm := Normalize(name)
	if epEnd < ep {
		epEnd = ep
	}
	if norm == "" {
		return &Parsed{ShowName: "", NormName: "", EpisodeNo: ep, EpEnd: epEnd, Tags: tags, ProgressEp: epProgress},
			errUnrecognized
	}
	// 多标题预览帖：一条 caption 依次列出多部剧（相册里通常正好是这些剧的预览片段）
	var multi []MultiEntry
	if fromCaption {
		if ms := MultiEntries(rawText); len(ms) >= 2 {
			multi = ms
		}
	}
	return &Parsed{ShowName: name, NormName: norm, EpisodeNo: ep, EpEnd: epEnd, Tags: tags,
		HasTitle: hasTitleContainer, Container: containerFound, FromTag: fromTag, TagName: tagName, TitleName: titleName,
		ProgressEp: epProgress, Multi: multi}, nil
}

// pairedTitleContainer 校验标题容器的左右符号是否成对：〈…〉 与 《…》 可混用
// （两种都是书名号），【…】 必须自成一对。『』「」 不是剧名容器，直接返回 false。
func pairedTitleContainer(s string) bool {
	r := []rune(s)
	if len(r) < 2 {
		return false
	}
	open, close := r[0], r[len(r)-1]
	bookOpen := open == '《' || open == '〈'
	bookClose := close == '》' || close == '〉'
	if bookOpen || bookClose {
		return bookOpen && bookClose
	}
	return open == '【' && close == '】'
}

// normalizeTitleWidth 折叠全角数字/字母和集号分隔符，保留中文标点与简繁写法。
// 在提取字节位置之前处理，保证集号切片仍然对应同一份文本。
func normalizeTitleWidth(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '０' && r <= '９' || r >= 'Ａ' && r <= 'Ｚ' || r >= 'ａ' && r <= 'ｚ' || r == '－' || r == '～' || r == '＿' {
			return r - 0xFEE0
		}
		if r == '\u3000' {
			return ' '
		}
		return r
	}, s)
}

// Normalize 归一化剧名：小写并只保留汉字、字母、数字，用于跨频道聚合
func Normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// IsJunkName 判断「清洗后的剧名」其实是频道水印/合集包名/随机串。
// 这类名字（faststart_oybfvg54、Qhwfres4nq btc8、IMG_9376.MOV、纯数字编号）
// 多来自 Telegram 自动命名或采集站哈希，不能当剧名。
func IsJunkName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	if len([]rune(name)) > 30 {
		return true
	}
	lower := strings.ToLower(name)
	for _, w := range []string{"甄选", "最全版", "无水印", "原档", "telegram", "预览", "合集", "合辑"} {
		if strings.Contains(lower, w) {
			return true
		}
	}
	if strings.Contains(lower, "zx") && !strings.Contains(name, "自慰") {
		return true
	}
	if reJunkStemName.MatchString(lower) || reJunkStemName.MatchString(compactLatin(lower)) ||
		rePureDigits.MatchString(lower) {
		return true
	}
	// 只剩预告/花絮/水印词的名字（预告_wm.mp4、pv预告.mp4）不是剧名；
	// 用整名匹配，避免误伤「预告犯」这类真剧名
	if rePreviewOnlyName.MatchString(Normalize(lower)) {
		return true
	}
	if reDateName.MatchString(lower) {
		return true
	}
	if junkNames[lower] {
		return true
	}
	// 含汉字的剧名基本可信（「甄选/预览/合集」这类水印词上面已拦掉）
	if containsHan(lower) {
		return false
	}
	return !wordlikeLatin(lower)
}

var (
	// Telegram 自动命名/采集残留：doc-123、faststart_abc、tweeload_xxx、play (12) 等
	reJunkStemName = regexp.MustCompile("^(doc|vid|img|video|file|photo|image|temp|faststart|tweeload|telegram|preview|thumb|play|proxy|manifest|index|master|main|wm|dj|mp4|mov|tmp|e?p?[ _-]?[0-9]+)[\\s_-]*[0-9a-z]*$")
	rePureDigits   = regexp.MustCompile(`^[0-9]+$`)
	// 预告/花絮 + 水印后缀组成的整名
	rePreviewOnlyName = regexp.MustCompile(`^(?:pv|wm|watermarked|video)*(?:预告片?|花絮|预览|片花)(?:pv|wm|watermarked|video)*$`)
	reHasDigit        = regexp.MustCompile(`[0-9]`)
	// 频道里按天发帖时会残留日期当剧名：10月2日 / 8月4日 / 2024-10-02 / 10.2
	reDateName = regexp.MustCompile(`^(?:[0-9]{1,2}\s*月\s*[0-9]{1,2}\s*[日号]?|[0-9]{4}\s*[年./-]\s*[0-9]{1,2}\s*[月./-]\s*[0-9]{1,2}\s*[日号]?)$`)
	// 双扩展名残留：xxx.mov.mp4 清洗后会留下 "xxx mov"
	videoExtTokens = []string{"mp4", "mkv", "mov", "avi", "wmv", "flv", "webm", "m4v", "ts", "rmvb"}
)

// junkExactNames 整名就是「正片/花絮」这类通用词的名字，不是短剧。
var junkExactNames = map[string]bool{
	"正片": true, "花絮": true, "片花": true, "预告": true, "预告片": true,
	"完整": true, "完整版": true, "未删减": true, "未删减版": true, "动态": true,
}

// junkNames 频道厂牌/作者水印，曾被当成剧名入库。
var junkNames = map[string]bool{
	"icicis": true, "9pm": true, "black128x": true, "pupuwaifu": true,
	"joioio": true, "kennys548534": true, "kenny": true, "eroshort": true,
}

// compactLatin 去掉空格/标点，便于识别「play (12)」这类带括号的自动命名。
func compactLatin(s string) string {
	return strings.Map(func(r rune) rune {
		// 汉字等非 ASCII 字符必须保留，否则「50元买断人妻」会被压成 "50" 误判成编号
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r >= 0x80 {
			return r
		}
		return -1
	}, s)
}

// latinConnectors 允许出现在英文标题里的虚词（不要求元音比例）。
var latinConnectors = map[string]bool{
	"the": true, "of": true, "and": true, "a": true, "an": true, "in": true,
	"on": true, "to": true, "my": true, "de": true, "la": true, "le": true,
}

// wordlikeLatin 判断纯拉丁剧名是否像真正的标题。
// 频道里英文剧名很少，绝大多数是无意义随机串（哈希、mux 标记、采集站命名），
// 所以要求每个词都「像单词」，否则判为垃圾名。
func wordlikeLatin(s string) bool {
	tokens := strings.FieldsFunc(s, func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	})
	if len(tokens) == 0 {
		return false
	}
	words := 0
	for _, t := range tokens {
		switch {
		case latinConnectors[t]:
			continue
		case rePureDigits.MatchString(t) && len(t) <= 2:
			continue // 「squid game 2」这类续集序号
		case reHasDigit.MatchString(t):
			return false // 字母里夹数字（ero5hort6 / jk64qlu3by6z）基本都是哈希
		case isLatinWord(t):
			words++
		default:
			return false
		}
	}
	return words > 0
}

// isLatinWord 粗略判断一个纯字母词是否像正常单词（元音够多、没有长串辅音）。
func isLatinWord(t string) bool {
	if len(t) < 4 {
		return false
	}
	vowels, run := 0, 0
	for _, r := range t {
		switch r {
		case 'a', 'e', 'i', 'o', 'u', 'y':
			vowels++
			run = 0
		default:
			run++
			if run >= 5 {
				return false
			}
		}
	}
	return vowels >= 2 && vowels*3 >= len(t)
}

// containsHan 判断字符串里是否含汉字（含汉字的剧名可信度高）。
func containsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

// CleanFileName 从纯文件名兜底生成可读剧名：
// 去掉 #标签符号、下划线与水印/画质噪声（如 母上攻略EP-7_watermarked）。
func CleanFileName(name string) string {
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.ReplaceAll(name, "#", " ")
	name = stripTrailingVideoExt(name)
	// 厂牌标记不是剧名：「AI成人短剧儿媳觉醒…」「AI短剧xxx」
	name = reChannelBrand.ReplaceAllString(name, " ")
	name = reAIBrand.ReplaceAllString(name, " ")
	// 竖线分段：后面几段带「短剧/在线观看/免费」时只保留第一段
	if segs := rePipeSegment.Split(name, -1); len(segs) > 1 {
		promo := false
		for _, s := range segs[1:] {
			if rePipePromo.MatchString(s) {
				promo = true
			}
		}
		if promo && strings.TrimSpace(segs[0]) != "" {
			name = segs[0]
		}
	}
	name = cleanName(name)
	// 清洗会把「.mov」变成「 mov」，清完再来一遍
	return stripTrailingVideoExt(name)
}

// stripTrailingVideoExt 反复去掉末尾的视频扩展名残留：
// "乐园触手.mov" → "乐园触手"，"乐园触手 .mov" → "乐园触手"，"a.mov.mp4" → "a"。
func stripTrailingVideoExt(name string) string {
	name = strings.TrimSpace(name)
	for {
		lower := strings.ToLower(name)
		trimmed := false
		for _, ext := range videoExtTokens {
			if strings.HasSuffix(lower, "."+ext) || strings.HasSuffix(lower, " "+ext) {
				name = strings.TrimSpace(name[:len(name)-len(ext)-1])
				trimmed = true
				break
			}
		}
		if !trimmed {
			return name
		}
	}
}

func extractTags(s string) []string {
	matches := reTag.FindAllStringSubmatch(s, -1)
	seen := map[string]struct{}{}
	out := []string{}
	for _, m := range matches {
		t := strings.ToLower(m[1])
		if isJunkTag(t) {
			continue
		}
		if _, ok := seen[t]; ok {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// chooseName 把「标题候选」和「#标签候选」做双重验证后决定剧名：
//  1. 任一方缺失时用另一方；
//  2. 归一化后相同（或互相包含且长度接近）时用标签——标签版本更干净，
//     能修掉《穿过薄雾的早晨2》/《等一个人回来2》这类标题尾数；
//  3. 标签只是标题的前缀且短很多（#姐姐 vs 《姐姐的诱惑》）说明它是分类词，
//     改用标题；
//  4. 两边都对不上时以标题为准，但标题若是「新剧/预览/福利」这类营销词、
//     或标签名原样出现在正文里，则改用标签。
func chooseName(title, tag string, hasContainer bool, rawText string) (string, bool) {
	if tag == "" {
		return title, false
	}
	// 没有标题容器且标题区很长：是营销文案/吐槽长文，不是剧名，用标签
	if title == "" || isMarketingTitle(title) ||
		(!hasContainer && utf8.RuneCountInString(title) > 30) {
		return tag, true
	}
	t, g := Normalize(title), Normalize(tag)
	if t == "" {
		return tag, true
	}
	if g == "" {
		return title, false
	}
	if t == g {
		return tag, true
	}
	// 标签是标题去掉尾数/季号后的干净版（《穿过薄雾的早晨2》+#穿过薄雾的早晨）：
	// 用标签，避免「…2」和「…」被拆成两部剧。
	if rest, ok := strings.CutPrefix(t, g); ok && reTailSuffix.MatchString(rest) {
		return tag, true
	}
	// 有《》/【】标题容器时，容器里的通常是官方剧名；但如果容器名与 #标签
	// 「互相包含」（同一部剧的干净版/带尾数版），以双重校验匹配到的标签为准，
	// 避免「…2」和「…」被拆成两部剧。只有匹配不上时才用容器标题。
	if strings.Contains(t, g) || strings.Contains(g, t) {
		short, long := utf8.RuneCountInString(g), utf8.RuneCountInString(t)
		if short > long {
			short, long = long, short
		}
		if float64(short) >= 0.6*float64(long) {
			return tag, true
		}
		return title, false
	}
	// 容器与标签不一致时，容器里的就是官方剧名，不再让 #标签 抢名：
	// 「《刀剑少女在我家》PV预告 #奇幻 #都市 #美少女」正文里出现的「美少女」
	// 只是题材词，按它定名会把这条 PV 挂到一部叫「美少女」的假剧上。
	if hasContainer {
		return title, false
	}
	// 「阿舞 #韵母攻略 播放第二集」：水印前缀紧挨标签，标签才是剧名
	if !hasContainer && titleBeforeTag(rawText, title, tag) {
		return tag, true
	}
	if tagInBody(rawText, tag) {
		return tag, true
	}
	return title, false
}

var reLeadEpisode = regexp.MustCompile(`^\s*(?:第\s*[0-9一二三四五六七八九十百千两]{1,4}\s*[集话回]|EP?\s*-?\s*[0-9]{1,4})`)

// reTailSuffix 匹配标题比标签多出来的「尾数」：2 / 第二季 这类非区分性后缀。
var reTailSuffix = regexp.MustCompile(`^(?:[0-9]{1,2}|第[0-9一二三四五六七八九十百]{1,2}季)$`)

// 剧名里残留的集号/范围尾巴。频道标题常写成「长生录1-10集」「教师凌辱记第二季
// EP-2」「武侠幻想情色剧 全12」，清洗后这些尾巴会留在名字里，建出「长生录1 10」
// 这种重复剧。定名前统一剥掉：
//   - 尾部「1-10 / 1~10 / 1 至 10 / 1 10（集/话/期/回）」
//   - 尾部「全12（集）」
//   - 尾部「第3集 / EP2 / EP-2 / EP」
//   - 文件名开头的「第一集 / EP2」（只在文件名兜底时剥）
const nameEpisodeNumber = `[0-9零一二三四五六七八九十百千两]{1,4}`
const nameEpisodeRange = nameEpisodeNumber + `(?:\s*[-~－～—–至到]\s*|\s+)` + nameEpisodeNumber
const nameEpisodeUnit = `(?:合集|合辑|合輯|全集|[集话話期回])`

var (
	reNameRangeTail = regexp.MustCompile(`(?i)[\s._\-·]*(?:第\s*|EP\s*-?\s*)?` + nameEpisodeRange + `\s*` + nameEpisodeUnit + `?\s*$`)
	reNameFullTail  = regexp.MustCompile(`[\s._\-·]*全\s*` + nameEpisodeNumber + `\s*(?:集)?\s*$`)
	reNameEpTail    = regexp.MustCompile(`(?i)[\s._\-·]*(?:第\s*` + nameEpisodeNumber + `\s*` + nameEpisodeUnit + `|EP\s*-?\s*[0-9]{0,4})\s*$`)
	// 不带「第」的裸集号尾巴：文件名常写「长生录 32集.mp4」，清洗后剩「长生录 32集」
	reNameBareEpTail = regexp.MustCompile(`[\s._\-·]*[0-9]{1,4}\s*[集话期回話]\s*$`)
	// 分隔符后面的裸数字（「长生录 32」）：只对中文名生效，避免吃掉
	// 「Squid Game 2」这类英文续集序号
	reNameLooseNumTail = regexp.MustCompile(`[\s._\-·]+[0-9]{1,3}\s*$`)
	// 文件名常见的水印后缀（「滴滴代操 wm.mp4」→ 滴滴代操）
	reNameWmTail = regexp.MustCompile(`(?i)[\s._\-·]+(?:wm|watermark(?:ed)?|水印)\s*$`)
	// 水印/二次压制标记直接粘在名字后面的（「滴滴代操10水印」「艳母 EP4 processed」）
	reNameWatermarkTail = regexp.MustCompile(`(?i)[\s._\-·]*(?:带?水印切片|水印版?|processed|高清版?)\s*$`)
	reNameEpHead        = regexp.MustCompile(`^\s*(?:第\s*[0-9一二三四五六七八九十百千两]{1,4}\s*[集话期回話]|EP\s*-?\s*[0-9]{0,4})[\s._\-·]*`)
	// 紧贴通用词尾部的序号（`完整1`、`花絮2`），用于识别「垃圾词+集号」的名字。
	reTrailingNumOnly = regexp.MustCompile(`[0-9]{1,3}\s*$`)
	// AI 厂牌标记（「AI成人短剧」「AI短剧」「AI漫剧」）：只是频道标签，不是剧名的一部分，
	// 无论出现在名字开头（AI成人短剧儿媳觉醒…）还是结尾（…黄果原创最新都市AI成人短剧）
	// 都剥掉。
	reAIBrand = regexp.MustCompile(`(?i)ai\s*(?:成人短剧|成人|短剧|动漫|漫剧|生成)`)
	// reChannelBrand 是发布频道自己的厂牌/水印名，出现在剧名里没有意义。
	reChannelBrand = regexp.MustCompile(`(?i)(?:ygwp|cbdj|reelshort|jile\s*ai|9pm\s*成人剧厂牌|2048\s*原创(?:短剧)?|ai\s*大佬自制)[\s._\-·|@]*`)
	// 文件名末尾的营销标签（「…_家庭伦理连续剧_黑料718」）：与剧情无关，剥掉。
	reNamePromoTail = regexp.MustCompile(`(?i)[\s._\-·]*(?:家庭伦理连续剧|伦理连续剧|连续剧|黑料[0-9]{0,4}|美图视频消除|视频消除|完结撒花|荔枝短剧|野果短剧|黄瓜短剧|黄果短剧|原创最新都市|免费(?:在线)?观看|在线观看|在线看|免费|91成人(?:短剧)?|91视频|成人视频|短剧版)\s*$`)
	// 采集站文件名的分段尾巴：「狂飙｜AI短剧在线观看｜香蕉短剧」——竖线后面
	// 全是站点/频道说明，剧名在竖线前面。
	rePipeSegment = regexp.MustCompile(`\s*[|｜]\s*`)
	rePipePromo   = regexp.MustCompile(`(?i)短剧|在线观看|在线看|免费|剧场|视频`)
	// 只有集号的名字（「第一」「第二集」）：这是某一集，不是剧名。
	reEpisodeOnlyName = regexp.MustCompile(`^第\s*[0-9一二三四五六七八九十百千两]{1,4}\s*[集话期回話]?$`)
	// 名字中间夹着的集号（「唐仁杰探房 第2集 野果」「万圣夜的暧昧游戏 第1集 在线播放 - 黄果漫剧」）：
	// 集号及其后面的频道名/推广语都不属于剧名，从集号处截断。
	reInlineEpMarker = regexp.MustCompile(`(?i)[\s._\-·]*(?:第\s*(?:` + nameEpisodeRange + `|` + nameEpisodeNumber + `)\s*` + nameEpisodeUnit + `|[0-9]{1,4}\s*[集话話期回]|EP\s*-?\s*[0-9]{1,4})`)
	// 名字开头的裸序号（「2  骑手与妃子 第二集」「1 我与三个妹妹的同居史」）：
	// 是采集工具加的顺序号，后面紧跟中文且至少还有两个字时剥掉。
	reLeadIndex = regexp.MustCompile(`^\s*[0-9]{1,2}[.\s]\s*`)
)

// TrimNameNoise 去掉剧名前后的集号/范围残留；dropHead 用于文件名兜底
// （文件名常写成「第一集 水印.mp4」），正文标题不做开头剥离。
func TrimNameNoise(name string, dropHead bool) string {
	name = strings.TrimSpace(normalizeTitleWidth(name))
	// 厂牌标记可能出现在名字任意位置（…黄果原创最新都市AI成人短剧），先整体剥掉
	if b := strings.TrimSpace(reAIBrand.ReplaceAllString(name, " ")); b != name {
		name = b
	}
	// 集号出现在名字中间时，从集号处截断（后面的「野果短剧/在线播放」都是站点信息）。
	if loc := reInlineEpMarker.FindStringIndex(name); loc != nil && loc[0] > 0 {
		if head := strings.TrimSpace(name[:loc[0]]); utf8.RuneCountInString(head) >= 2 && containsHan(head) {
			name = head
		}
	}
	// 开头的采集序号：「2  骑手与妃子」→「骑手与妃子」。
	if loc := reLeadIndex.FindString(name); loc != "" {
		if rest := strings.TrimSpace(name[len(loc):]); containsHan(rest) && utf8.RuneCountInString(rest) >= 2 {
			name = rest
		}
	}
	for {
		n := reNameRangeTail.ReplaceAllString(name, "")
		n = reNameFullTail.ReplaceAllString(n, "")
		n = reNameEpTail.ReplaceAllString(n, "")
		n = reNameBareEpTail.ReplaceAllString(n, "")
		if containsHan(n) {
			n = reNameLooseNumTail.ReplaceAllString(n, "")
		}
		n = reNameWmTail.ReplaceAllString(n, "")
		n = reNamePromoTail.ReplaceAllString(n, "")
		// 水印/压制词直接粘着名字时也剥掉，但剥完至少留 2 个字，
		// 避免把「水印工具」这类业务词削成残字。
		if w := reNameWatermarkTail.ReplaceAllString(n, ""); w != n && utf8.RuneCountInString(strings.TrimSpace(w)) >= 2 {
			n = w
		}
		if dropHead {
			n = reNameEpHead.ReplaceAllString(n, "")
		}
		n = strings.TrimSpace(n)
		if n == name {
			break
		}
		name = n
	}
	return name
}

// isEpisodeishTitle 判断标题候选是不是以集号开头（说明它其实是「第N集+营销词」，
// 不是剧名）。例如「第3集 全系列完整版」。
func isEpisodeishTitle(title string) bool {
	return reLeadEpisode.MatchString(title)
}

// tagInBody 判断标签名是否也出现在正文（不含 # 号的普通文本）里，
// 用来验证「#标签」确实在描述这部剧，而不是搬运来的分类词。
func tagInBody(rawText, tag string) bool {
	body := reTag.ReplaceAllString(rawText, " ")
	return strings.Contains(Normalize(body), Normalize(tag))
}

// tagNameSuspicious 判断「完全靠 #标签 得到的剧名」是否不可信：
// 标签本身就是泛分类/服饰/场景词（#制服 / #原相机 / #漫剧）。这类频道把
// 真剧名写在文件名里，# 后面只是分类，按标签建剧会糊出一部假剧。
func tagNameSuspicious(name, _ string, _ string) bool {
	return IsGenreWord(name)
}

// looksLikeProse 判断标题候选是不是描述文案而不是剧名：太长，或者带句读。
func looksLikeProse(s string) bool {
	if utf8.RuneCountInString(s) > 14 {
		return true
	}
	return strings.ContainsAny(s, "，。！？；、")
}

// reTailEpisode 文件名末尾的集号尾巴：「琼明神女录7」「母子同欢 第一集」「xxx EP12」。
var reTailEpisode = regexp.MustCompile(`(?i)(?:[\s._\-·]*(?:第\s*[0-9一二三四五六七八九十百千两]{1,4}\s*[集话回期]|EP?\s*-?\s*[0-9]{1,4}|[0-9]{1,3}))\s*$`)

// nameFromFileName 从视频文件名里取出剧名，给「# 里全是分类词」的帖子兜底：
//
//	恶魔城堡.mp4      → 恶魔城堡
//	母子同欢 第一集.mp4 → 母子同欢
//	琼明神女录7.mp4   → 琼明神女录
func nameFromFileName(fileName string) string {
	name := strings.TrimSpace(TrimNameNoise(CleanFileName(fileName), true))
	for i := 0; i < 4 && name != ""; i++ {
		next := strings.TrimRight(reTailEpisode.ReplaceAllString(name, ""), " .-_.·")
		if next == name {
			break
		}
		name = next
	}
	if utf8.RuneCountInString(name) < 2 {
		return ""
	}
	// 纯编号/哈希文件名（2108190672973144237_video）不能当剧名
	digits, total := 0, 0
	for _, r := range name {
		if r == ' ' {
			continue
		}
		total++
		if r >= '0' && r <= '9' {
			digits++
		}
	}
	if total > 0 && digits*5 > total*3 {
		return ""
	}
	// 「预告 wm」「PV预告」「完整版」这类只有水印/预告词的文件名不是剧名
	if IsJunkName(name) || IsJunkShowName(name) {
		return ""
	}
	return name
}

// NameFromFileName 是 nameFromFileName 的导出版：重建时对「解析失败」的来源
// 用视频文件名兜底命名，需要和在线入库完全同一套清洗（剥厂牌前缀、集号、水印、
// 营销尾巴，再判垃圾名）。
func NameFromFileName(fileName string) string { return nameFromFileName(fileName) }

// isMarketingTitle 判断标题区取到的词是不是营销用语而非剧名。
// 这类标题（「新剧上线」「完整版」）没有剧名信息，应改用 #标签 定名。
func isMarketingTitle(title string) bool {
	n := Normalize(title)
	if n == "" {
		return true
	}
	for _, w := range []string{
		"新剧", "首播", "首更", "首发", "上线", "更新", "预览", "预告", "福利",
		"完整", "全集", "合集", "推荐", "精选", "独家", "速看", "必看", "热播",
		"完结", "内群", "会员", "置顶", "频道", "同步", "连载", "传媒", "影视", "影业",
		"工作室", "出品", "官方", "爆款", "新作", "上映",
	} {
		if strings.Contains(n, w) {
			return true
		}
	}
	return false
}

// pickShowTag 从正文里挑出最像剧名的 #标签：按出现顺序取第一个
// 既不是频道水印、也不是分类词的标签（#真人 #剧情 #熟女 这类泛词不能当剧名）。
func pickShowTag(text string) (string, bool) {
	t, _, ok := findShowTag(text)
	return t, ok
}

// findShowTag 返回标签名与 '#' 在原文里的字节位置。
// 第一轮只看「#标签」这种标准写法；都不可用时再退回整行——
// 中文标签常带标点（#妈妈，你可是我最爱的人），按字符类截断只剩「妈妈」，
// 而「妈妈」是分类词会被滤掉，所以再按整行取一次。
func findShowTag(text string) (string, int, bool) {
	for _, m := range reTag.FindAllStringSubmatchIndex(text, -1) {
		t := strings.TrimSpace(text[m[2]:m[3]])
		if t == "" || utf8.RuneCountInString(t) > 30 {
			continue
		}
		if isJunkTag(t) || isGenericTag(t) {
			continue
		}
		return t, m[0], true
	}
	for i := 0; i < len(text); i++ {
		if text[i] != '#' {
			continue
		}
		line := tagLine(text[i+1:])
		if line == "" || isJunkTag(line) || isGenericTag(line) {
			continue
		}
		if utf8.RuneCountInString(line) > 30 {
			continue
		}
		return line, i, true
	}
	return "", 0, false
}

const tagTrimSet = " \t\r\n，。、！？!?,.;:：；~～|｜·…—-—_（）()【】「」『』\"'“”‘’"

// tagLine 取 '#' 后面到行尾（或下一个 #、空白）的整段，去掉首尾标点。
func tagLine(s string) string {
	end := len(s)
	for i := 0; i < len(s); {
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == '#' || unicode.IsSpace(r) {
			end = i
			break
		}
		i += size
	}
	return strings.Trim(s[:end], tagTrimSet)
}

// titleBeforeTag 判断标题候选是不是紧挨在 #标签 前面的短前缀：
// 「阿舞 #韵母攻略 播放第二集」「西瓜视频~ #剧名 第1集」里的「阿舞」「西瓜视频」
// 是搬运号水印，不是剧名；这种紧邻关系说明标签才是本条帖子的剧名。
func titleBeforeTag(rawText, title, tag string) bool {
	if title == "" || tag == "" {
		return false
	}
	n := Normalize(title)
	if n == "" || utf8.RuneCountInString(n) > 12 {
		return false
	}
	pos := strings.Index(rawText, "#"+tag)
	if pos < 0 {
		return false
	}
	// 标签之前只剩标题开头这几个字（「阿舞」），标题候选里多出来的部分是
	// 标签之后的「播放第二集」被截断时带进来的尾巴，不算剧名。
	prefix := Normalize(rawText[:pos])
	pn := utf8.RuneCountInString(prefix)
	if pn < 2 || pn > 12 || !strings.HasPrefix(n, prefix) {
		return false
	}
	return utf8.RuneCountInString(n)-pn <= 6
}

// isGenericTag 过滤分类/题材类标签：它们描述内容类型而非某一部剧，
// 全库统计里每个都能挂到几十部剧上（#剧情 45 部、#真人 48 部）。
// genreWords 泛分类词：只有分类/标签含义，没有剧名信息。
// 频道常把「#露脸」「#大学生」「#颜值」这类分类当标题发，按它建剧只会把
// 一堆互不相干的视频糊成一部「剧」，列表里就会出现「露脸」「极品」这种条目。
var genreWords = map[string]bool{
	"真人": true, "剧情": true, "剧情向": true, "明星": true, "熟女": true, "绿帽": true,
	"国漫": true, "动漫": true, "动画": true, "禁忌": true, "调教": true, "反差": true,
	"人妻": true, "淫妻": true, "御姐": true, "出轨": true, "ntr": true, "擦边": true,
	"伦理": true, "百合": true, "女同": true, "奇幻": true, "师生": true, "乱伦": true,
	"穿越": true, "古风": true, "媚黑": true, "姐弟": true, "修仙": true, "古装": true,
	"巨乳": true, "车震": true, "嫂子": true, "魔幻": true, "母狗": true, "系统": true,
	"成人": true, "性转": true, "偷窥": true, "学姐": true, "学生": true, "偷情": true,
	"家庭伦理": true, "家庭禁忌": true, "母子": true, "母子乱伦": true, "兄妹乱伦": true,
	"姐弟乱伦": true, "3p": true, "4p": true, "群交": true, "轮奸": true,
	"中文字幕": true, "无码": true, "有码": true, "高清": true, "素人": true, "强奸": true,
	"迷奸": true, "白丝": true, "黑丝": true, "大长腿": true, "空姐": true, "护士": true,
	"校园": true, "都市": true, "悬疑": true, "甜宠": true, "复仇": true, "逆袭": true,
	"明星换脸": true, "ai换脸": true, "换脸": true, "解说": true, "合集": true,
	"全集": true, "预告": true, "切片": true, "姐姐": true, "妹妹": true, "妈妈": true,
	"母亲": true, "父亲": true, "爸爸": true, "哥哥": true, "弟弟": true, "岳母": true,
	"岳父": true, "婆婆": true, "公公": true, "小姨子": true, "姑妈": true, "表姐": true,
	"闺蜜": true, "老婆": true, "老公": true, "女友": true, "前女友": true, "女儿": true,
	"女仆": true, "房东": true, "邻居": true, "秘书": true, "老师": true, "医生": true,
	"学生妹": true, "校花": true, "警花": true, "总裁": true, "千金": true, "农女": true,
	"王妃": true, "皇后": true, "贵妃": true, "宫女": true,
	// 人设/题材类泛词：频道常把它们和真剧名混在同一行标签里
	// （「《刀剑少女在我家》#奇幻 #都市 #美少女 #剑灵」），按它们定名会
	// 建出「美少女」这种把互不相干的帖子糊在一起的假剧。
	"美少女": true, "少女": true, "美女": true, "女神": true, "足交": true,

	// 真人福利/自拍频道的分类词：这类频道不是短剧源，视频标题就是分类名
	"露脸": true, "颜值": true, "高颜值": true, "女优": true, "私房": true, "网红": true,
	"情侣": true, "多人": true, "换妻": true, "口交": true, "自慰": true, "黑人": true,
	"双飞": true, "后入": true, "露出": true, "约炮": true, "技师": true, "酒店": true,
	"探花": true, "抖音": true, "抖音风": true, "极品": true, "海角": true,
	"海角社区": true, "小宝寻花": true, "江南第一深情": true, "情侣泄密": true,
	"反差婊": true, "女高": true, "苹果原相机": true, "第一视角": true,
	// 频道把「服饰/场景/行为」当成专辑标签：#制服 #情趣内衣 #私拍 #原相机。
	// 这类词当剧名会把互不相干的视频糊成一部假剧，定名时直接跳过。
	"制服": true, "情趣内衣": true, "内衣": true, "丝袜": true, "高跟鞋": true,
	"写真": true, "模特": true, "泄密": true, "私拍": true, "原相机": true,
	"空降": true, "外围": true, "走光": true, "偷吃": true, "诱惑": true,
	"私拍泄密": true, "自拍泄密": true, "私房照": true,
	// 只剩「完整」「合集」这种残词的条目，多是群发的切片/合辑文件名，不是剧名。
	"完整": true, "完整版": true, "合集版": true,
	"大学生": true, "男大": true, "高中生": true, "中学生": true,
	"玉足": true, "足控": true, "美足": true, "丝足": true, "白袜": true, "肉丝": true,
	"白皙": true, "阿黑颜": true, "翘臀": true, "蜜桃臀": true, "肥臀": true,
	"足球宝贝": true, "dj": true, "上": true, "下": true, "中": true,
	"成人漫剧": true, "漫剧": true, "里番": true, "滤镜": true, "原创": true,
	"电影解说": true, "国产": true, "日韩": true, "欧美": true, "自拍": true,
	"偷拍": true, "街拍": true, "直播": true, "福利姬": true, "少妇": true, "女大": true,
	"首播": true, "首更": true, "首映": true, "预告片": true, "精彩片段": true,
	// 画幅/版本类说明词（「竖屏.mp4」「横屏」）：描述视频形式，不是剧名。
	"竖屏": true, "横屏": true, "竖版": true, "横版": true,
}

// isGenericTag 过滤无分类意义的标签：泛化词、AI 水印、厂牌名与文件名碎片
func isGenericTag(t string) bool { return genreWords[strings.ToLower(strings.TrimSpace(t))] }

// IsGenreWord 判断一个词是不是「题材/分类」泛词，而不是具体剧名。
//
// 定名、历史数据改名、标签清理共用同一张词表：像 #美少女 / #剑灵 这种
// 「题材 + 元素」混在同一行标签里的帖子，靠它把题材词剔掉、留下真正的剧名。
func IsGenreWord(s string) bool {
	return genreWords[strings.ToLower(strings.TrimSpace(s))]
}

// IsAdCaption 判断整条 caption 是不是「预告片 + 拉人进付费群」的引流帖。
// 命中明显的转化话术即认定：这类帖子没有剧名，视频也只是几秒的试看片段。
func IsAdCaption(text string) bool { return reAdCTA.MatchString(text) }

// reSocialPost 社交平台搬运标记：短剧频道会顺手转发推特 / OnlyFans 博主的
// 福利视频，文案以「推特极品…」「推特原创剧情…」开头。这类不是短剧。
var reSocialPost = regexp.MustCompile(`(?i)(推特|twitter|onlyfans|福利姬)`)

// IsNonDramaCaption 判断一条 caption 是「短剧频道里转发的非短剧内容」
// （社交平台博主视频），入库时整条丢弃。
func IsNonDramaCaption(text string) bool { return reSocialPost.MatchString(text) }

// IsJunkShowName 判断一个「剧名」其实不是短剧：只是泛分类词，或者带拉群
// 引流话术（剧名里出现「进群」「点此开通」「VPN」这类转化文案的，几乎都是
// 「预告切片 + 拉人进付费群」的帖子，不是一部剧）。入库前用它拦掉，
// 清理历史数据时也用同一套规则。
func IsJunkShowName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	if genreWords[strings.ToLower(name)] || genreWords[Normalize(name)] {
		return true
	}
	// 「正片」「花絮」「预告片」这类不是剧名的通用词（精确匹配，不误伤含这些字的真剧名）
	if junkExactNames[strings.ToLower(name)] || junkExactNames[Normalize(name)] {
		return true
	}
	// 「第一」「第二集」这类只有集号的名字不是剧名
	if reEpisodeOnlyName.MatchString(strings.TrimSpace(name)) {
		return true
	}
	// 「动态7 31 Ai」剥掉通用词后只剩数字/字母，也不是剧名
	for w := range junkExactNames {
		rest, ok := strings.CutPrefix(strings.ToLower(name), w)
		if !ok {
			continue
		}
		rest = strings.TrimSpace(rest)
		if rest == "" || !containsHan(rest) {
			return true
		}
	}
	// 通用词或分类词后面直接粘序号的（文件名 `完整1.mp4`、`偷窥4.mp4`），
	// 剥掉尾号再看原词；带分隔符的裸数字已被 TrimNameNoise 处理。
	if bare := reTrailingNumOnly.ReplaceAllString(name, ""); bare != name &&
		(junkExactNames[strings.ToLower(bare)] || genreWords[strings.ToLower(bare)]) {
		return true
	}
	// 「ygwp 竖屏」这类「作者/厂牌 + 画幅说明」的名字：空格后的尾词本身就是分类词
	if i := strings.LastIndex(name, " "); i > 0 {
		tail := strings.TrimSpace(name[i+1:])
		// 单字尾词（「风流御厨 下」的「下」）是正常续集写法，不按分类词处理
		if utf8.RuneCountInString(tail) >= 2 && (genreWords[tail] || genreWords[strings.ToLower(tail)]) {
			return true
		}
	}
	if reAdCTA.MatchString(name) {
		return true
	}
	// 剥掉广告尾巴后什么都不剩 → 整名都是广告
	return Normalize(reAdTail.ReplaceAllString(name, " ")) == ""
}

// nonDramaSingleKeywords 只有一集的条目里，名字带这些词的通常不是短剧：
// 演唱会/福利视频、众筹贴、修图工具（美图/视频消除）、宣发物料、片花、漫展/街拍/vlog
// 这类日常记录。判据放在名字上而不是「集数==1」，因为正常短剧只发一集的情况很多。
var nonDramaSingleKeywords = []string{
	"演唱会", "福利", "众筹", "美图", "视频消除", "宣发", "片花", "漫展", "街拍", "vlog", "录屏", "回放",
}

// IsNonDramaSingleName 判断「只有一集的条目」的名字是否明显不是短剧。
// 只给单集条目用：多集的剧名里出现「福利」这类词时，更可能是剧情标题，不该误杀。
func IsNonDramaSingleName(name string) bool {
	lower := strings.ToLower(name)
	for _, k := range nonDramaSingleKeywords {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

// isJunkTag 过滤无分类意义的标签：泛化词、AI 水印、厂牌名与文件名碎片
func isJunkTag(t string) bool {
	lower := strings.ToLower(t)
	// AI 相关（真人ai、ai原创、aigc、aiart…）与短剧泛化词
	if strings.Contains(lower, "ai") || strings.Contains(t, "短剧") {
		return true
	}
	// 厂牌/频道水印：黄豆、黄果、魔改系列，各种传媒工作室
	if strings.Contains(t, "黄豆") || strings.Contains(t, "黄果") ||
		strings.Contains(t, "魔改") || strings.Contains(t, "传媒") {
		return true
	}
	// 营销/状态词：原创声明、连载状态、内部群引流
	if strings.Contains(t, "原创") || strings.Contains(t, "连载") || strings.Contains(t, "内群") {
		return true
	}
	// 文件名碎片：watermarked 残留、_ep2 长尾、下划线开头
	if strings.Contains(lower, "watermark") || strings.Contains(lower, "_ep") ||
		strings.HasPrefix(t, "_") {
		return true
	}
	switch t {
	case "9pm", "black128x", "犬九", "joioio_犬九", "icicis", "pupuwaifu", "噗噗",
		"老虎菜", "kenny", "kennys548534", "糖心", "单体作品", "原档无水", "花少母带",
		"泰剧", "欧美版", "吃瓜", "写实", "落地", "asmr", "采耳", "pmv", "出差":
		return true
	}
	return false
}

// titleZone 截出「标题区」：去掉简介/剧情介绍等正文段，最多保留 400 字。
// 频道文案常把「目前正剧更新到第十六集」「第049期开发日志」写在正文里，
// 全文搜集号会把这些数字当成本条视频的集号，造成集数虚高。
var reVideoExt = regexp.MustCompile(`(?i)\.(mp4|mkv|mov|avi|wmv|ts|m4v|flv|webm)\s*$`)

func looksLikeFileName(text string) bool {
	return reVideoExt.MatchString(strings.TrimSpace(text))
}

func titleZone(text string) string {
	if loc := reSectionCut.FindStringIndex(text); loc != nil && loc[0] >= 6 {
		text = text[:loc[0]]
	}
	const maxRunes = 400
	if r := []rune(text); len(r) > maxRunes {
		text = string(r[:maxRunes])
	}
	return text
}

// badEpFollow 判断集号后面的字是不是「分钟/秒/期/季」这类非集数语义，
// 例如「EP72分02秒」「第2季」里的数字不是集号。
func badEpFollow(text string, end int) bool {
	if end <= 0 || end >= len(text) {
		return false
	}
	r, _ := utf8.DecodeRuneInString(text[end:])
	switch r {
	case '分', '秒', '时', '年', '月', '日', '期', '季', '折', '元', '天', '个', '人', '次', '楼', '层', '公':
		return true
	}
	return false
}

// reProgressBefore 匹配「更新至 / 连载至 / 已更」这类进度说明。
// 「EP3-4 内群连载更新至第12集」里的 12 是「目前更新到第几集」，不是这条
// 视频的集号；以前按正则优先级先匹配到「第12集」，整组相册都被记成第 12 集。
var reProgressBefore = regexp.MustCompile(`(?:更新至|更新到|已更新至|已更新到|连载至|连载到|更至|已更|更新)\s*$`)

// progressBefore 判断 idx 处的集号是不是「更新至第N集」这种进度说明。
func progressBefore(text string, idx int) bool {
	if idx <= 0 {
		return false
	}
	start := idx - 30
	if start < 0 {
		start = 0
	}
	prefix := text[start:idx]
	for len(prefix) > 0 && !utf8.RuneStart(prefix[0]) {
		prefix = prefix[1:]
	}
	return reProgressBefore.MatchString(prefix)
}

// epCandidate 一个集号候选：位置靠前的优先，进度说明只在没有其他候选时兜底。
type epCandidate struct {
	ep, end, idx int
	progress     bool
}

// extractEpisode 从文本中提取集号。
// allowLoose 用于纯文件名场景：允许「剧名3」「_05」这类裸数字；
// caption 场景关闭裸数字，避免把剧名/季数末尾的 2、日期、时长当成集号。
//
// 候选按「在文本里出现的位置」取最靠前的一个，而不是按正则类型排优先级：
// 频道文案常见「【剧名】EP3-4 内群连载更新至第12集」，按类型优先会把
// 第12集当成集号，整组相册的选集就全错了。
func extractEpisode(text string, allowLoose bool) (ep, epEnd, startIdx int, ok bool) {
	ep, epEnd, startIdx, ok, _ = extractEpisodeEx(text, allowLoose)
	return ep, epEnd, startIdx, ok
}

// extractEpisodeEx 同 extractEpisode，额外报告集号是否只来自「更新至第N集」
// 这类进度说明——进度不是本条视频的集号，调用方应优先用文件名/相册序号。
func extractEpisodeEx(text string, allowLoose bool) (ep, epEnd, startIdx int, ok, fromProgress bool) {
	var cands []epCandidate
	add := func(n, e, idx int) {
		if !validEp(n) || !validEp(e) || e < n {
			return
		}
		cands = append(cands, epCandidate{ep: n, end: e, idx: idx, progress: progressBefore(text, idx)})
	}
	for _, m := range reCNEpisode.FindAllStringSubmatchIndex(text, -1) {
		if n, good := chineseToInt(text[m[2]:m[3]]); good {
			end := n
			if m[4] > 0 {
				if e, good2 := chineseToInt(text[m[4]:m[5]]); good2 && validEp(e) && e >= n {
					end = e
				}
			}
			add(n, end, m[0])
		}
	}
	for _, m := range reEPRange.FindAllStringSubmatchIndex(text, -1) {
		n, _ := strconv.Atoi(text[m[2]:m[3]])
		e, _ := strconv.Atoi(text[m[4]:m[5]])
		add(n, e, m[0])
	}
	for _, m := range reNumRange.FindAllStringSubmatchIndex(text, -1) {
		n, _ := strconv.Atoi(text[m[2]:m[3]])
		e, _ := strconv.Atoi(text[m[4]:m[5]])
		// 用第一个数字的位置当起点，名字切割不会吃掉前一个字
		add(n, e, m[2])
	}
	for _, m := range reEP.FindAllStringSubmatchIndex(text, -1) {
		n, _ := strconv.Atoi(text[m[2]:m[3]])
		if !badEpFollow(text, m[3]) {
			add(n, n, m[0])
		}
	}
	for _, m := range reEShort.FindAllStringSubmatchIndex(text, -1) {
		n, _ := strconv.Atoi(text[m[2]:m[3]])
		if !badEpFollow(text, m[3]) {
			add(n, n, m[0])
		}
	}
	// 正常候选里取最靠前的；只有进度说明时才退回进度里的数字
	if best, found := earliestEp(cands, false); found {
		return best.ep, best.end, best.idx, true, false
	}
	if best, found := earliestEp(cands, true); found {
		return best.ep, best.end, best.idx, true, true
	}
	if !allowLoose {
		return 0, 0, -1, false, false
	}
	// 裸数字/汉字尾数兜底：4 位以上编号（0529 / 000853 / 0913 日期）
	// 通常是相册序号或时间，不应当作集号，会造成「总集数虚高」。
	if m := reAttached.FindStringSubmatchIndex(text); m != nil && digitRunLen(text, m[3]) < 4 {
		n, _ := strconv.Atoi(text[m[2]:m[3]])
		if validEp(n) {
			// 只截到集号数字之前（保留紧邻的汉字）
			return n, n, m[2], true, false
		}
	}
	if m := reBare.FindStringSubmatchIndex(text); m != nil && digitRunLen(text, m[3]) < 4 {
		n, _ := strconv.Atoi(text[m[2]:m[3]])
		if validEp(n) {
			return n, n, m[0], true, false
		}
	}
	return 0, 0, -1, false, false
}

// digitRunLen 返回以 end 结尾（不含）的连续数字长度。
// ≥4 位（0529 / 000853 / 0913）通常是编号、日期或时间，不应当作集号。
func digitRunLen(text string, end int) int {
	n := 0
	for i := end - 1; i >= 0; i-- {
		if text[i] < '0' || text[i] > '9' {
			break
		}
		n++
	}
	return n
}

// earliestEp 取最早出现的候选；onlyProgress 控制只从进度说明候选里挑。
func earliestEp(cands []epCandidate, onlyProgress bool) (epCandidate, bool) {
	var best epCandidate
	found := false
	for _, c := range cands {
		if c.progress != onlyProgress {
			continue
		}
		if !found || c.idx < best.idx {
			best, found = c, true
		}
	}
	return best, found
}

func validEp(n int) bool { return n >= 1 && n <= 999 }

func cleanName(s string) string {
	s = reBrackets.ReplaceAllString(s, " ")
	// 文件名分隔符归一为空格，保证 \b 词边界能匹配 _merge/_mp4 等
	s = strings.NewReplacer("_", " ", "-", " ", ".", " ").Replace(s)
	s = reNoise.ReplaceAllString(s, " ")
	s = reAdTail.ReplaceAllString(s, " ")
	s = stripEmojis(s)

	var b strings.Builder
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

func stripEmojis(s string) string {
	var b strings.Builder
	for _, r := range s {
		if isEmoji(r) {
			b.WriteRune(' ')
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func isEmoji(r rune) bool {
	return (r >= 0x1F000 && r <= 0x1FAFF) ||
		(r >= 0x2600 && r <= 0x27BF) ||
		(r >= 0x2B00 && r <= 0x2BFF) ||
		r == 0xFE0F || r == 0x200D || r == 0x2122
}

var cnNum = map[rune]int{
	'零': 0, '一': 1, '二': 2, '两': 2, '三': 3, '四': 4,
	'五': 5, '六': 6, '七': 7, '八': 8, '九': 9,
	'十': 10, '百': 100, '千': 1000,
}

// chineseToInt 支持阿拉伯数字与中文数字（十/百/千位）
func chineseToInt(s string) (int, bool) {
	if n, err := strconv.Atoi(s); err == nil {
		return n, true
	}
	total, unit := 0, 0
	for _, r := range s {
		v, ok := cnNum[r]
		if !ok {
			return 0, false
		}
		if r == '十' || r == '百' || r == '千' {
			if unit == 0 {
				unit = 1
			}
			total += unit * v
			unit = 0
		} else {
			unit = v
		}
	}
	total += unit
	if total == 0 {
		return 0, false
	}
	return total, true
}
