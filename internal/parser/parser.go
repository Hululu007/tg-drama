package parser

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

type Parsed struct {
	ShowName   string
	NormName   string
	EpisodeNo  int
	EpEnd      int
	Tags       []string
	HasTitle   bool
	FromTag    bool
	Container  bool
	TagName    string
	TitleName  string
	ProgressEp bool
	Multi      []MultiEntry
}

type MultiEntry struct {
	Name string
	Ep   int
}

var reMultiTitle = regexp.MustCompile(`[《〈【]([^》〉】]{1,40})[》〉】]`)

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

	if len(uniq) < 3 {
		return nil
	}
	return uniq
}

type Parser struct{}

func New() *Parser { return &Parser{} }

var (
	reTag = regexp.MustCompile(`#([\p{L}\p{N}_]+)`)

	reCNEpisode = regexp.MustCompile(`第\s*([0-9]{1,4}|[零一二三四五六七八九十百千两]{1,4})\s*(?:[-~－～—–至到]\s*([0-9]{1,4}|[零一二三四五六七八九十百千两]{1,4}))?\s*(?:[集话話回]|合集|合辑|合輯|全集)`)

	reNumRange = regexp.MustCompile(`(?:^|[^0-9])0*([0-9]{1,3})\s*[-~－～—–至到]\s*0*([0-9]{1,3})\s*(?:[集话話回]|合集|合辑|合輯|全集)`)

	reEP = regexp.MustCompile(`(?i)\bEP[ _\-]?0*([0-9]{1,3})(?:[^0-9]|$)`)

	reEShort = regexp.MustCompile(`(?i)\bE[ _\-]?0*([0-9]{1,2})(?:[^0-9]|$)`)

	reEPRange = regexp.MustCompile(`(?i)\bEP?[ _]?0*([0-9]{1,3})\s*[-~－～]\s*(?:EP?[ _]?0*)?([0-9]{1,3})\b`)

	reBare = regexp.MustCompile(`(?:[\s_\-\.\[\(【】])0*([0-9]{1,3})(?:[\s_\-\.\]\)】]|$)`)

	reAttached = regexp.MustCompile(`\p{Han}(0*[1-9][0-9]{0,2})(?:[\s_\-\.\]\)】]|$)`)

	reBook = regexp.MustCompile(`[《〈]([^》〉]{1,60})[》〉]`)

	reQuotedTitle = regexp.MustCompile(`^\s*(?:[【\[(（][^】\])）]{0,40}[】\])）]|\s)*[『「]([^』」]{1,40})[』」]\s*(?:第\s*[0-9零一二三四五六七八九十百千两]{1,4}\s*(?:[-~－～—–至到]\s*(?:[0-9零一二三四五六七八九十百千两]{1,4}))?\s*(?:[集话話回]|合集|合辑|合輯)|\bEP?\s*-?\s*[0-9]{1,3}|[0-9]{1,4}\s*(?:全集|合集))`)

	reSquare = regexp.MustCompile(`【([^】]{1,60})】`)

	reBracketJunk = regexp.MustCompile(`(?i)会员|内群|内部|置顶|同步|更新|连载|已更|完整|预览|投喂|邀请|关注|投稿|合作|广告|粉丝|福利|频道|短剧|传媒|影视|影业|工作室|出品|官方|资源|剧场|爆款|新作|上映|[0-9]{1,3}\s*[集期话回]|\bEP?[ _]?0*[0-9]`)

	reSectionCut = regexp.MustCompile(`(?i)(剧情介绍|剧情简介|内容简介|剧集简介|剧集介绍|故事简介|剧情|简介|介绍|描述|标签|看点|主演|导演|类型)\s*[:：]`)

	reBrackets = regexp.MustCompile(`【[^】]*】|\[[^\]]*\]|「[^」]*」|（[^）]*）|\([^)]*\)`)

	reNoise = regexp.MustCompile(`(?i)(4k|2k|2160p|1080p|720p|480p|蓝光|超清|高清|标清|中字|字幕|完整版|未删减|无删减|抢先版|watermark\w*|\bhd\b|\bh264\b|\bh265\b|\bmp4\b|\bmkv\b|短剧视频(?:在线观看)?|在线观看|\bvert\b|\bmerge\b|md5\s*[0-9a-f]+|tg\s*[:：]?\s*@?\s*[a-z0-9_]{3,}|黄果(?:短剧)?|黄豆(?:短剧)?)`)

	reAdTail = regexp.MustCompile(`(?i)(?:群主推荐[^\s]{0,12}|超稳定\s*vpn|\bvpn\b|飞机号|内部群|会员群|官方群|交流群|内部片库|完整片库|预告频道|点此开通|点此自助|点按钮|机器人开通|立刻进群追更|自助入群|自助加入|自助开通|全网最低价|tg全域推广|官方客服|[0-9]+\s*秒进群)`)

	reAdCTA = regexp.MustCompile(`(?i)(?:进群|入群|加群|群里|会员群|内部群|内部片库|点此开通|点此自助|点按钮|开通后|机器人开通|自助开通|自助入群|立刻进群|秒进群|完整片库|预告频道|付费一次|按月|包月|全网最低价|官方客服|群主推荐|24\s*小时自助|\bvpn\b|飞机号|观看完整视频|完整视频观看|点此看全|点此观看|点此查看|看到结局|等预告|追更|tg搜|点击购买|[0-9]+(?:\.[0-9]+)?\s*元)`)
)

func (p *Parser) Parse(caption, fileName string) (*Parsed, error) {
	text := strings.TrimSpace(caption)
	fromCaption := text != ""
	if !fromCaption {

		text = stripTrailingVideoExt(strings.TrimSpace(fileName))
	}
	text = normalizeTitleWidth(text)

	tags := extractTags(text)
	rawText := text

	text = reChannelBrand.ReplaceAllString(text, " ")
	text = reAIBrand.ReplaceAllString(text, " ")
	text = reTag.ReplaceAllString(text, " ")

	head := titleZone(text)

	ep, epEnd, idx, ok, epProgress := extractEpisodeEx(head, !fromCaption || looksLikeFileName(text))

	titleName := ""
	hasTitleContainer := false
	tagName := ""
	if fromCaption {
		tagName, _ = pickShowTag(rawText)
	}

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

	if bookIdx < 0 && squareIdx < 0 {
		if m := reQuotedTitle.FindStringSubmatchIndex(scan); m != nil {
			if cleaned := cleanName(scan[m[2]:m[3]]); cleaned != "" {
				bookName, bookIdx = cleaned, m[0]
			}
		}
	}
	switch {
	case bookIdx >= 0 && squareIdx >= 0:

		bn, sn, tn := Normalize(bookName), Normalize(squareName), Normalize(tagName)
		switch {
		case tn != "" && sn == tn:
			titleName, hasTitleContainer = squareName, true
		case tn != "" && bn == tn:
			titleName, hasTitleContainer = bookName, true
		default:

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

			titleName = cleanName(head)
		}
	}

	titleName = strings.TrimRight(titleName, " .-_.·")

	titleName = TrimNameNoise(titleName, !fromCaption)

	if isEpisodeishTitle(titleName) {
		titleName = ""
	}

	if fromCaption && !hasTitleContainer && tagName == "" &&
		len(reTag.FindAllString(rawText, -1)) > 0 && looksLikeProse(titleName) {
		if fb := nameFromFileName(fileName); fb != "" && !IsJunkShowName(fb) {
			titleName = fb
		} else {
			return &Parsed{Tags: tags}, errUnrecognized
		}
	}

	if !fromCaption && !hasTitleContainer {
		if fb := nameFromFileName(fileName); fb != "" {
			titleName = fb
		}
	}
	name, fromTag := chooseName(titleName, tagName, hasTitleContainer, rawText)

	name = TrimNameNoise(name, false)

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

	if fromCaption && !hasTitleContainer && utf8.RuneCountInString(name) > 30 {
		return &Parsed{Tags: tags}, errUnrecognized
	}

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

func Normalize(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			b.WriteRune(r)
		}
	}
	return b.String()
}

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

	if rePreviewOnlyName.MatchString(Normalize(lower)) {
		return true
	}
	if reDateName.MatchString(lower) {
		return true
	}
	if junkNames[lower] {
		return true
	}

	if containsHan(lower) {
		return false
	}
	return !wordlikeLatin(lower)
}

var (
	reJunkStemName = regexp.MustCompile("^(doc|vid|img|video|file|photo|image|temp|faststart|tweeload|telegram|preview|thumb|play|proxy|manifest|index|master|main|wm|dj|mp4|mov|tmp|e?p?[ _-]?[0-9]+)[\\s_-]*[0-9a-z]*$")
	rePureDigits   = regexp.MustCompile(`^[0-9]+$`)

	rePreviewOnlyName = regexp.MustCompile(`^(?:pv|wm|watermarked|video)*(?:预告片?|花絮|预览|片花)(?:pv|wm|watermarked|video)*$`)
	reHasDigit        = regexp.MustCompile(`[0-9]`)

	reDateName = regexp.MustCompile(`^(?:[0-9]{1,2}\s*月\s*[0-9]{1,2}\s*[日号]?|[0-9]{4}\s*[年./-]\s*[0-9]{1,2}\s*[月./-]\s*[0-9]{1,2}\s*[日号]?)$`)

	videoExtTokens = []string{"mp4", "mkv", "mov", "avi", "wmv", "flv", "webm", "m4v", "ts", "rmvb"}
)

var junkExactNames = map[string]bool{
	"正片": true, "花絮": true, "片花": true, "预告": true, "预告片": true,
	"完整": true, "完整版": true, "未删减": true, "未删减版": true, "动态": true,
}

var junkNames = map[string]bool{
	"icicis": true, "9pm": true, "black128x": true, "pupuwaifu": true,
	"joioio": true, "kennys548534": true, "kenny": true, "eroshort": true,
}

func compactLatin(s string) string {
	return strings.Map(func(r rune) rune {

		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r >= 0x80 {
			return r
		}
		return -1
	}, s)
}

var latinConnectors = map[string]bool{
	"the": true, "of": true, "and": true, "a": true, "an": true, "in": true,
	"on": true, "to": true, "my": true, "de": true, "la": true, "le": true,
}

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
			continue
		case reHasDigit.MatchString(t):
			return false
		case isLatinWord(t):
			words++
		default:
			return false
		}
	}
	return words > 0
}

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

func containsHan(s string) bool {
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func CleanFileName(name string) string {
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.ReplaceAll(name, "#", " ")
	name = stripTrailingVideoExt(name)

	name = reChannelBrand.ReplaceAllString(name, " ")
	name = reAIBrand.ReplaceAllString(name, " ")

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

	return stripTrailingVideoExt(name)
}

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

func chooseName(title, tag string, hasContainer bool, rawText string) (string, bool) {
	if tag == "" {
		return title, false
	}

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

	if rest, ok := strings.CutPrefix(t, g); ok && reTailSuffix.MatchString(rest) {
		return tag, true
	}

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

	if hasContainer {
		return title, false
	}

	if !hasContainer && titleBeforeTag(rawText, title, tag) {
		return tag, true
	}
	if tagInBody(rawText, tag) {
		return tag, true
	}
	return title, false
}

var reLeadEpisode = regexp.MustCompile(`^\s*(?:第\s*[0-9一二三四五六七八九十百千两]{1,4}\s*[集话回]|EP?\s*-?\s*[0-9]{1,4})`)

var reTailSuffix = regexp.MustCompile(`^(?:[0-9]{1,2}|第[0-9一二三四五六七八九十百]{1,2}季)$`)

const nameEpisodeNumber = `[0-9零一二三四五六七八九十百千两]{1,4}`
const nameEpisodeRange = nameEpisodeNumber + `(?:\s*[-~－～—–至到]\s*|\s+)` + nameEpisodeNumber
const nameEpisodeUnit = `(?:合集|合辑|合輯|全集|[集话話期回])`

var (
	reNameRangeTail = regexp.MustCompile(`(?i)[\s._\-·]*(?:第\s*|EP\s*-?\s*)?` + nameEpisodeRange + `\s*` + nameEpisodeUnit + `?\s*$`)
	reNameFullTail  = regexp.MustCompile(`[\s._\-·]*全\s*` + nameEpisodeNumber + `\s*(?:集)?\s*$`)
	reNameEpTail    = regexp.MustCompile(`(?i)[\s._\-·]*(?:第\s*` + nameEpisodeNumber + `\s*` + nameEpisodeUnit + `|EP\s*-?\s*[0-9]{0,4})\s*$`)

	reNameBareEpTail = regexp.MustCompile(`[\s._\-·]*[0-9]{1,4}\s*[集话期回話]\s*$`)

	reNameLooseNumTail = regexp.MustCompile(`[\s._\-·]+[0-9]{1,3}\s*$`)

	reNameWmTail = regexp.MustCompile(`(?i)[\s._\-·]+(?:wm|watermark(?:ed)?|水印)\s*$`)

	reNameWatermarkTail = regexp.MustCompile(`(?i)[\s._\-·]*(?:带?水印切片|水印版?|processed|高清版?)\s*$`)
	reNameEpHead        = regexp.MustCompile(`^\s*(?:第\s*[0-9一二三四五六七八九十百千两]{1,4}\s*[集话期回話]|EP\s*-?\s*[0-9]{0,4})[\s._\-·]*`)

	reTrailingNumOnly = regexp.MustCompile(`[0-9]{1,3}\s*$`)

	reAIBrand = regexp.MustCompile(`(?i)ai\s*(?:成人短剧|成人|短剧|动漫|漫剧|生成)`)

	reChannelBrand = regexp.MustCompile(`(?i)(?:ygwp|cbdj|reelshort|jile\s*ai|9pm\s*成人剧厂牌|2048\s*原创(?:短剧)?|ai\s*大佬自制)[\s._\-·|@]*`)

	reNamePromoTail = regexp.MustCompile(`(?i)[\s._\-·]*(?:家庭伦理连续剧|伦理连续剧|连续剧|黑料[0-9]{0,4}|美图视频消除|视频消除|完结撒花|荔枝短剧|野果短剧|黄瓜短剧|黄果短剧|原创最新都市|免费(?:在线)?观看|在线观看|在线看|免费|91成人(?:短剧)?|91视频|成人视频|短剧版)\s*$`)

	rePipeSegment = regexp.MustCompile(`\s*[|｜]\s*`)
	rePipePromo   = regexp.MustCompile(`(?i)短剧|在线观看|在线看|免费|剧场|视频`)

	reEpisodeOnlyName = regexp.MustCompile(`^第\s*[0-9一二三四五六七八九十百千两]{1,4}\s*[集话期回話]?$`)

	reInlineEpMarker = regexp.MustCompile(`(?i)[\s._\-·]*(?:第\s*(?:` + nameEpisodeRange + `|` + nameEpisodeNumber + `)\s*` + nameEpisodeUnit + `|[0-9]{1,4}\s*[集话話期回]|EP\s*-?\s*[0-9]{1,4})`)

	reLeadIndex = regexp.MustCompile(`^\s*[0-9]{1,2}[.\s]\s*`)
)

func TrimNameNoise(name string, dropHead bool) string {
	name = strings.TrimSpace(normalizeTitleWidth(name))

	if b := strings.TrimSpace(reAIBrand.ReplaceAllString(name, " ")); b != name {
		name = b
	}

	if loc := reInlineEpMarker.FindStringIndex(name); loc != nil && loc[0] > 0 {
		if head := strings.TrimSpace(name[:loc[0]]); utf8.RuneCountInString(head) >= 2 && containsHan(head) {
			name = head
		}
	}

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

func isEpisodeishTitle(title string) bool {
	return reLeadEpisode.MatchString(title)
}

func tagInBody(rawText, tag string) bool {
	body := reTag.ReplaceAllString(rawText, " ")
	return strings.Contains(Normalize(body), Normalize(tag))
}

func tagNameSuspicious(name, _ string, _ string) bool {
	return IsGenreWord(name)
}

func looksLikeProse(s string) bool {
	if utf8.RuneCountInString(s) > 14 {
		return true
	}
	return strings.ContainsAny(s, "，。！？；、")
}

var reTailEpisode = regexp.MustCompile(`(?i)(?:[\s._\-·]*(?:第\s*[0-9一二三四五六七八九十百千两]{1,4}\s*[集话回期]|EP?\s*-?\s*[0-9]{1,4}|[0-9]{1,3}))\s*$`)

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

	if IsJunkName(name) || IsJunkShowName(name) {
		return ""
	}
	return name
}

func NameFromFileName(fileName string) string { return nameFromFileName(fileName) }

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

func pickShowTag(text string) (string, bool) {
	t, _, ok := findShowTag(text)
	return t, ok
}

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

	prefix := Normalize(rawText[:pos])
	pn := utf8.RuneCountInString(prefix)
	if pn < 2 || pn > 12 || !strings.HasPrefix(n, prefix) {
		return false
	}
	return utf8.RuneCountInString(n)-pn <= 6
}

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

	"美少女": true, "少女": true, "美女": true, "女神": true, "足交": true,

	"露脸": true, "颜值": true, "高颜值": true, "女优": true, "私房": true, "网红": true,
	"情侣": true, "多人": true, "换妻": true, "口交": true, "自慰": true, "黑人": true,
	"双飞": true, "后入": true, "露出": true, "约炮": true, "技师": true, "酒店": true,
	"探花": true, "抖音": true, "抖音风": true, "极品": true, "海角": true,
	"海角社区": true, "小宝寻花": true, "江南第一深情": true, "情侣泄密": true,
	"反差婊": true, "女高": true, "苹果原相机": true, "第一视角": true,

	"制服": true, "情趣内衣": true, "内衣": true, "丝袜": true, "高跟鞋": true,
	"写真": true, "模特": true, "泄密": true, "私拍": true, "原相机": true,
	"空降": true, "外围": true, "走光": true, "偷吃": true, "诱惑": true,
	"私拍泄密": true, "自拍泄密": true, "私房照": true,

	"完整": true, "完整版": true, "合集版": true,
	"大学生": true, "男大": true, "高中生": true, "中学生": true,
	"玉足": true, "足控": true, "美足": true, "丝足": true, "白袜": true, "肉丝": true,
	"白皙": true, "阿黑颜": true, "翘臀": true, "蜜桃臀": true, "肥臀": true,
	"足球宝贝": true, "dj": true, "上": true, "下": true, "中": true,
	"成人漫剧": true, "漫剧": true, "里番": true, "滤镜": true, "原创": true,
	"电影解说": true, "国产": true, "日韩": true, "欧美": true, "自拍": true,
	"偷拍": true, "街拍": true, "直播": true, "福利姬": true, "少妇": true, "女大": true,
	"首播": true, "首更": true, "首映": true, "预告片": true, "精彩片段": true,

	"竖屏": true, "横屏": true, "竖版": true, "横版": true,
}

func isGenericTag(t string) bool { return genreWords[strings.ToLower(strings.TrimSpace(t))] }

func IsGenreWord(s string) bool {
	return genreWords[strings.ToLower(strings.TrimSpace(s))]
}

func IsAdCaption(text string) bool { return reAdCTA.MatchString(text) }

var reSocialPost = regexp.MustCompile(`(?i)(推特|twitter|onlyfans|福利姬)`)

func IsNonDramaCaption(text string) bool { return reSocialPost.MatchString(text) }

func IsJunkShowName(name string) bool {
	name = strings.TrimSpace(name)
	if name == "" {
		return true
	}
	if genreWords[strings.ToLower(name)] || genreWords[Normalize(name)] {
		return true
	}

	if junkExactNames[strings.ToLower(name)] || junkExactNames[Normalize(name)] {
		return true
	}

	if reEpisodeOnlyName.MatchString(strings.TrimSpace(name)) {
		return true
	}

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

	if bare := reTrailingNumOnly.ReplaceAllString(name, ""); bare != name &&
		(junkExactNames[strings.ToLower(bare)] || genreWords[strings.ToLower(bare)]) {
		return true
	}

	if i := strings.LastIndex(name, " "); i > 0 {
		tail := strings.TrimSpace(name[i+1:])

		if utf8.RuneCountInString(tail) >= 2 && (genreWords[tail] || genreWords[strings.ToLower(tail)]) {
			return true
		}
	}
	if reAdCTA.MatchString(name) {
		return true
	}

	return Normalize(reAdTail.ReplaceAllString(name, " ")) == ""
}

var nonDramaSingleKeywords = []string{
	"演唱会", "福利", "众筹", "美图", "视频消除", "宣发", "片花", "漫展", "街拍", "vlog", "录屏", "回放",
}

func IsNonDramaSingleName(name string) bool {
	lower := strings.ToLower(name)
	for _, k := range nonDramaSingleKeywords {
		if strings.Contains(lower, k) {
			return true
		}
	}
	return false
}

func isJunkTag(t string) bool {
	lower := strings.ToLower(t)

	if strings.Contains(lower, "ai") || strings.Contains(t, "短剧") {
		return true
	}

	if strings.Contains(t, "黄豆") || strings.Contains(t, "黄果") ||
		strings.Contains(t, "魔改") || strings.Contains(t, "传媒") {
		return true
	}

	if strings.Contains(t, "原创") || strings.Contains(t, "连载") || strings.Contains(t, "内群") {
		return true
	}

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

var reProgressBefore = regexp.MustCompile(`(?:更新至|更新到|已更新至|已更新到|连载至|连载到|更至|已更|更新)\s*$`)

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

type epCandidate struct {
	ep, end, idx int
	progress     bool
}

func extractEpisode(text string, allowLoose bool) (ep, epEnd, startIdx int, ok bool) {
	ep, epEnd, startIdx, ok, _ = extractEpisodeEx(text, allowLoose)
	return ep, epEnd, startIdx, ok
}

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

	if best, found := earliestEp(cands, false); found {
		return best.ep, best.end, best.idx, true, false
	}
	if best, found := earliestEp(cands, true); found {
		return best.ep, best.end, best.idx, true, true
	}
	if !allowLoose {
		return 0, 0, -1, false, false
	}

	if m := reAttached.FindStringSubmatchIndex(text); m != nil && digitRunLen(text, m[3]) < 4 {
		n, _ := strconv.Atoi(text[m[2]:m[3]])
		if validEp(n) {

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
