// 视频整理：从文件名 + 内嵌元数据推断片名/年份/季集号，生成规范名。
//
// 目标格式（对齐 Jellyfin / Plex / Emby 官方刮削规范）：
//
//	电影：片名 (年份) [分辨率] [来源].ext
//	剧集：剧名 (年份) - S01E02 - 标题.ext
//	目录：剧名 (年份)/Season 01/剧名 (年份) - S01E02 - 标题.ext
//
// 关键原则：
//   - 年份是最强的消歧符，务必提取（Heat (1995) vs Heat (2013)）
//   - 文件名里的画质/发布组标签放在年份之后（扫描器只解析前面的部分）
//   - 不认识的字符（, : " / \ | ? *）必须替换，Jellyfin 明确列为保留字符
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// mediaNamePlan 一次重命名的计划
type mediaNamePlan struct {
	OldPath string `json:"oldPath"`
	NewPath string `json:"newPath"`
	OldName string `json:"oldName"`
	NewName string `json:"newName"`
	Dir     string `json:"dir"`
	Title   string `json:"title"`
	Year    int    `json:"year"`
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
	Part    int    `json:"part,omitempty"`
	Res     string `json:"res"`
	Source  string `json:"source"`
	Reason  string `json:"reason"`
	Changed bool   `json:"changed"`
	// Skip：示例/预览文件，不整理（不算失败）
	Skip      bool   `json:"skip,omitempty"`
	SkipReason string `json:"skipReason,omitempty"`
	// Special：附加内容（extras），只移动不改名
	Special string `json:"special,omitempty"`
}

// ---------- 文件名解析 ----------

var (
	// 年份：19xx/20xx
	reYear = regexp.MustCompile(`(?:^|[^\d])((?:19|20)\d{2})(?:[^\d]|$)`)
	// 剧集：S01E02 / s1.e2 / 第01集
	reSxxExx = regexp.MustCompile(`(?i)S(\d{1,2})[\s._-]*E(\d{1,3})`)
	// 中文剧集：第01集 / 第01话 / 第01期（动漫/综艺）
	reCnEpisode = regexp.MustCompile(`第\s*(\d{1,3})\s*(?:集|话|話|期)`)
	reCnSeason  = regexp.MustCompile(`第\s*(\d{1,2})\s*季`)
	// 英文季：Season 1 / season.01
	reEnSeason = regexp.MustCompile(`(?i)season[\s._]*(\d{1,2})`)
	// EP 号：EP01 / ep.1
	reEpNum = regexp.MustCompile(`(?i)(?:^|[\s._\-])EP[\s._]*(\d{1,3})(?:$|[\s._\-])`)
	// 剧集：01x02
	reXxx = regexp.MustCompile(`(?i)(\d{1,2})x(\d{1,3})`)
	// 尾部裸集数：「剧名 - 01」「剧名.01」「剧名 01」「庆余年01」
	// 安全边界：①残留不能是纯数字（「24」是剧名，交给目录证据判定）；
	//          ②只认前导零（01-09）或三位数（100+）——「速度与激情 10」
	//            「敢死队 12」这类续集名是裸的 1-2 位，不能当集数
	reTrailingEp = regexp.MustCompile(`[\s._\-]?(\d{2,3})$`)
	// 多分割标记：CD1 / disc1 / part1 / pt1（一部电影拆多个文件）
	rePart = regexp.MustCompile(`(?i)(?:^|[\s._\-])(?:cd|dvd|disc|part|pt)[\s._\-]*(\d{1,2})(?:$|[\s._\-])`)
	// 纯数字文件名（01.mp4）：仅在目录名提供剧名时才敢当集数
	rePureNum = regexp.MustCompile(`^(\d{1,3})$`)
	// 常见分辨率数字（720/480 不带 p 时不能当集数）
	bareResNums = map[string]bool{"480": true, "540": true, "576": true, "720": true}
	// 分辨率标签
	reRes = regexp.MustCompile(`(?i)\b(2160p|1080[pi]|1080p|720[pi]|720p|480[pi]|480p|4k|uhd|8k)\b`)
	// 源码/发布组：半角 []() 与中文全角 【】（）都算
	reBracket = regexp.MustCompile(`[\[\(（【]([^\]\)）】]*)[\]\)）】]`)
	// 中文尾部标记：可连续出现多个（「国语 中字 无删减」「高清 正片」）
	// 只匹配标记本身、不消耗前后分隔符——否则链式标记（国语 中字）只能剥掉第一个
	// 只剥尾部的标记：「高清 世界」这种中部的是标题本身，不能动
	reCnTailMarkers = regexp.MustCompile(`(?:^|[\s._\-])(国语|双语|中字|外挂|内嵌字幕|无删减|完整版|高清|超清|正片|抢先版|免费观看|在线观看|独家|自制|影视)`)
	// 常见画质/编码关键词（保留为 source 候选）
	resKeywords = []string{"BluRay", "BDRip", "WEB-DL", "WEBRip", "WEB", "HDTV", "HDRip",
		"Remux", "DVDRip", "HDDVD", "CAM", "TS", "H265", "H264", "x264", "x265", "HEVC", "AVC"}
	// 画质/编码关键词（用于从标题里剥离；与 resKeywords 同源，避免两处不一致）
	reSourceKW = func() *regexp.Regexp {
		parts := make([]string, 0, len(resKeywords))
		for _, kw := range resKeywords {
			parts = append(parts, regexp.QuoteMeta(kw))
		}
		return regexp.MustCompile(`(?i)(?:^|[\s._\-\[\]])(` + strings.Join(parts, "|") + `)(?:$|[\s._\-\[\]])`)
	}()
	// 编码 + 发布组连写：x264-GROUP / H265-RARBG（整体去掉，否则标题会残留组名）
	reCodecGroup = regexp.MustCompile(`(?i)\b(?:x264|x265|h264|h265|hevc|avc)[-._][A-Za-z0-9]+`)
	// 标题尾部全大写短标记（多为发布组残留，如 " GRP"）；小写单词不受影响
	reUpperTail = regexp.MustCompile(`[\s._\-]+[A-Z0-9]{2,12}$`)
	// 示例/预览文件：直接跳过，不当正片改名
	reSampleFile = regexp.MustCompile(`(?i)(^|[\s._\-])(sample|samples|预览|样片|试看)([\s._\-]|$)`)
	// 附加内容（预告片/花絮/片头片尾/特典）：organize 时归入 extras/ 目录
	reExtrasFile = regexp.MustCompile(`(?i)(^|[\s._\-])(trailer|trailers|extra|extras|featurette|featurettes|behindthescenes|behind[\s._-]the[\s._-]scenes|interview|deleted|nced|ncop|特典|花絮|预告|幕后|采访|删减)([\s._\-]|$)`)
	// 附加内容短代码只认大写（SP/OP/ED/PV/CM），避免误伤 "Ed" 等人名
	reExtrasCodes = regexp.MustCompile(`(^|[\s._\-])(SP|PV|CM|OP|ED)([\s._\-]|$)`)
)

// mediaInfo 从文件名与元数据解析出的信息
type mediaInfo struct {
	Title        string
	Year         int
	Season       int
	Episode      int
	Part         int // 多分割编号（CD1 / part1）
	EpisodeTitle string
	Res          string
	Source       string
	IsSeries     bool
}

// parseVideoName 从文件名解析视频信息
func parseVideoName(name string) mediaInfo {
	var info mediaInfo
	name = fixMojibake(name)
	base := strings.TrimSuffix(name, filepath.Ext(name))
	orig := base
	// 重名序号先剥掉再解析（影片年份是 4 位，stripCollisionSuffix 只吃 1-3 位，不会误伤）
	base = stripCollisionSuffix(base)

	// 剧集号
	if m := reSxxExx.FindStringSubmatch(base); m != nil {
		info.Season, _ = strconv.Atoi(m[1])
		info.Episode, _ = strconv.Atoi(m[2])
		info.IsSeries = true
		base = reSxxExx.ReplaceAllString(base, " ")
	} else if m := reCnEpisode.FindStringSubmatch(base); m != nil {
		info.Episode, _ = strconv.Atoi(m[1])
		info.IsSeries = true
		base = reCnEpisode.ReplaceAllString(base, " ")
	} else if m := reEpNum.FindStringSubmatch(base); m != nil {
		info.Episode, _ = strconv.Atoi(m[1])
		info.IsSeries = true
		base = reEpNum.ReplaceAllString(base, " ")
	} else if m := reXxx.FindStringSubmatch(base); m != nil {
		info.Season, _ = strconv.Atoi(m[1])
		info.Episode, _ = strconv.Atoi(m[2])
		info.IsSeries = true
		base = reXxx.ReplaceAllString(base, " ")
	}
	// 季（中文「第1季」/英文 Season 1，含目录名）
	if m := reCnSeason.FindStringSubmatch(base); m != nil {
		info.Season, _ = strconv.Atoi(m[1])
		info.IsSeries = true
		base = reCnSeason.ReplaceAllString(base, " ")
	} else if m := reEnSeason.FindStringSubmatch(base); m != nil {
		info.Season, _ = strconv.Atoi(m[1])
		info.IsSeries = true
		base = reEnSeason.ReplaceAllString(base, " ")
	}

	// 多分割标记：CD1 / disc1 / part1（标题里剥掉，编号进 Part）
	if info.Part == 0 {
		if m := rePart.FindStringSubmatch(base); m != nil {
			if pt, err := strconv.Atoi(m[1]); err == nil && pt >= 1 && pt <= 20 {
				info.Part = pt
				base = rePart.ReplaceAllString(base, " ")
			}
		}
	}

	// 分辨率
	if m := reRes.FindStringSubmatch(base); m != nil {
		info.Res = normalizeRes(m[1])
		base = reRes.ReplaceAllString(base, " ")
	}

	// 方括号/圆括号内容：可能是年份、画质、发布组（半角与全角都认）
	var bracketParts []string
	base = reBracket.ReplaceAllStringFunc(base, func(s string) string {
		inner := strings.Trim(s, "[]()【】（）")
		bracketParts = append(bracketParts, inner)
		return " "
	})

	// 年份：优先从括号里找，其次从整体找
	for _, b := range bracketParts {
		if y := parseYear(b); y > 0 {
			info.Year = y
			break
		}
	}
	if info.Year == 0 {
		if m := reYear.FindStringSubmatch(base); m != nil {
			info.Year, _ = strconv.Atoi(m[1])
		}
	}
	// 去掉年份
	base = reYear.ReplaceAllString(base, " ")

	// 来源：括号里的画质/编码关键词，或整体里的
	for _, b := range bracketParts {
		for _, kw := range resKeywords {
			if strings.Contains(strings.ToLower(b), strings.ToLower(kw)) {
				info.Source = kw
				break
			}
		}
		if info.Source != "" {
			break
		}
	}
	if info.Source == "" {
		for _, kw := range resKeywords {
			if strings.Contains(strings.ToLower(base), strings.ToLower(kw)) {
				info.Source = kw
				break
			}
		}
	}

	// 剥离画质/编码关键词与发布组：标题里不该残留 "BluRay x264-GRP"
	// 关键词间的单个分隔符会被前一个匹配吃掉（"BluRay.x264" 只剥掉 BluRay），
	// 必须循环到稳定
	base = reCodecGroup.ReplaceAllString(base, " ")
	for i := 0; i < 5; i++ {
		t := reSourceKW.ReplaceAllString(base, " ")
		if t == base {
			break
		}
		base = t
	}

	// 尾部裸集数：「剧名 - 01」「剧名.01」「剧名 01」「庆余年01」
	// 必须在剥发布组尾巴之前做——否则 " - 01" 会被 reUpperTail 当发布组吃掉
	// 安全边界：①残留不能是纯数字（「24」是剧名，交给目录证据判定）；
	//          ②只认前导零（01-09）或三位数（100+）——「速度与激情 10」
	//            「敢死队 12」这类续集名是裸的 1-2 位，不能当集数
	if !info.IsSeries && !isDigitsOnly(strings.TrimSpace(base)) {
		if loc := reTrailingEp.FindStringSubmatchIndex(base); loc != nil {
			num := base[loc[2]:loc[3]]
			if !bareResNums[num] {
				if ep, err := strconv.Atoi(num); err == nil && (strings.HasPrefix(num, "0") || ep >= 100) {
					info.Episode = ep
					info.IsSeries = true
					base = strings.TrimSpace(base[:loc[0]])
				}
			}
		}
	}

	// 剥发布组尾巴：仅当文件名里存在发布标记（年份/分辨率/来源/括号内容）时才剥，
	// 否则 "Movie THE" 这种以大写单词正常结尾的标题会被误吃
	hasBracketRelease := false
	for _, b := range bracketParts {
		if len(strings.TrimSpace(b)) >= 2 {
			hasBracketRelease = true
			break
		}
	}
	if info.Year > 0 || info.Res != "" || info.Source != "" || hasBracketRelease {
		base = reUpperTail.ReplaceAllString(base, " ")
	}

	// 季号归一：是剧集且有集号、但季号未知时按第 1 季算
	// （Jellyfin/Emby 惯例；buildVideoName 与 seasonDirName 也都默认 1）
	if info.IsSeries && info.Episode > 0 && info.Season <= 0 {
		info.Season = 1
	}

	// 清理标题：多余分隔符、站点后缀
	title := cleanTitle(base)
	info.Title = title
	_ = orig
	return info
}

// normalizeRes 归一化分辨率标签
func normalizeRes(s string) string {
	s = strings.ToUpper(s)
	switch s {
	case "UHD", "4K":
		return "4K"
	case "8K":
		return "8K"
	case "2160P", "2160I":
		return "2160p"
	case "1080P", "1080I":
		return "1080p"
	case "720P", "720I":
		return "720p"
	case "480P", "480I":
		return "480p"
	}
	return s
}

// 常见的资源站/下载站后缀（片名里出现这些通常是站点名，需剥离）
var siteSuffixes = []string{
	"-高清影视", "高清影视", "在线观看", "电影天堂", "天天影视", "量子资源",
	"OK资源", "OK动漫", "樱花动漫", "贪婪", "人人影视", "人人", "百度云",
	"种子", "磁力", "Telegram", "夸克", "阿里云盘", "115", "迅雷",
	"蓝光", "超清", "在线", "免费观看", "完整版", "抢先版", "TS", "TC",
	"4K影视", "HD影院", "MP4影视", "影视大全", "影视工厂", "酷云影视",
	"低端影视", "爱美剧", "天天美剧", "抖音电影", "番茄电影", "电影盒子",
}

// dotAbbrs 标题里需要保留点的缩写（Mr. Robot / Dr. Death / St. Elsewhere）
var dotAbbrs = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "dr": true, "prof": true, "st": true,
	"sr": true, "jr": true, "vs": true, "etc": true, "inc": true, "ltd": true,
	"co": true, "no": true, "vol": true, "de": true, "la": true, "le": true,
	"al": true, "el": true, "u.s": true, "u.k": true,
}

func isASCIILetter(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z')
}

func isASCIIUpper(r rune) bool { return r >= 'A' && r <= 'Z' }

// smartDots 智能处理标题里的点：分隔符场景（The.Matrix）换成空格，
// 但保护缩写与名缩写里的点（Mr. Robot / S.W.A.T. / U.F.O. / 3.5）
func smartDots(s string) string {
	runes := []rune(s)
	var sb strings.Builder
	for i, r := range runes {
		if r != '.' {
			sb.WriteRune(r)
			continue
		}
		var prev, next rune
		if i > 0 {
			prev = runes[i-1]
		}
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		keep := false
		switch {
		case prev >= '0' && prev <= '9' && next >= '0' && next <= '9':
			keep = true // 3.5 / 4.0 小数
		case isSingleLetterAt(runes, i-1) && (isASCIIUpper(next) || next == 0 || next == ' ' || next == '-' || next == '_' || next == '.'):
			// S.W.A.T. / U.F.O.：前面是单独一个字母（不是多字母单词的一部分）
			keep = true
		default:
			// Mr. / Dr. / St. 等已知缩写：保留点并补一个空格（Mr.Robot → Mr. Robot）
			j := i - 1
			for j >= 0 && isASCIILetter(runes[j]) {
				j--
			}
			if dotAbbrs[strings.ToLower(string(runes[j+1:i]))] {
				keep = true
				if next != ' ' && next != '.' && next != 0 {
					sb.WriteString(". ")
					continue
				}
			}
		}
		if keep {
			sb.WriteRune('.')
		} else {
			sb.WriteByte(' ')
		}
	}
	return sb.String()
}

// isSingleLetterAt runes[j] 是否是单独一个字母（前一个字符不是字母）
func isSingleLetterAt(runes []rune, j int) bool {
	if j < 0 || !isASCIILetter(runes[j]) {
		return false
	}
	return j == 0 || !isASCIILetter(runes[j-1])
}

// endsWithSepOnly s[start:] 是否只剩分隔符（或为空）——用于判断标记在尾部
func endsWithSepOnly(s string, start int) bool {
	for _, r := range s[start:] {
		switch r {
		case ' ', '\t', '.', '_', '-', '–', '—', '·':
		default:
			return false
		}
	}
	return true
}

// cleanTitle 清洗标题：去站点后缀、中文尾部标记、多余符号
func cleanTitle(s string) string {
	s = strings.TrimSpace(s)
	// 反复剥离站点后缀
	for changed := true; changed; {
		changed = false
		for _, suf := range siteSuffixes {
			if strings.HasSuffix(s, suf) && len(s) > len(suf)+1 {
				s = strings.TrimSpace(strings.TrimSuffix(s, suf))
				changed = true
			}
		}
	}
	// 反复剥离中文尾部标记（「国语 中字 无删减」这类连续出现的）
	for changed := true; changed; {
		changed = false
		locs := reCnTailMarkers.FindAllStringSubmatchIndex(s, -1)
		for i := len(locs) - 1; i >= 0; i-- {
			loc := locs[i]
			// 标记后只剩分隔符或直接结尾，才算尾部标记
			if !endsWithSepOnly(s, loc[1]) {
				continue
			}
			t := strings.TrimSpace(s[:loc[0]])
			if t == s || len([]rune(t)) < 2 {
				continue
			}
			s = t
			changed = true
			break
		}
	}
	// 常见分隔符归一：点按智能规则处理，_ 与中文逗号换空格
	s = smartDots(s)
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "，", " ")
	s = strings.ReplaceAll(s, "。", " ")
	// 合并多余空格
	s = strings.Join(strings.Fields(s), " ")
	// 去掉首尾常见修饰
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "-–—·|")
	s = strings.TrimSpace(s)
	return s
}

// sanitizeName 清理文件名中的非法字符（Jellyfin 保留字符）
// , : " / \ | ? * 以及控制字符
func sanitizeName(s string) string {
	repl := map[rune]string{
		',': " ", ':': " -", '"': "", '/': " ", '\\': " ", '|': " ", '?': "", '*': "",
		'<': " ", '>': " ", '：': " -", '，': " ", '。': ".", '？': "", '！': "",
		'“': "", '”': "", '‘': "", '’': "", '《': "", '》': "",
	}
	var sb strings.Builder
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			continue
		}
		if rep, ok := repl[r]; ok {
			sb.WriteString(rep)
			continue
		}
		sb.WriteRune(r)
	}
	out := strings.Join(strings.Fields(sb.String()), " ")
	// Windows 保留名
	upper := strings.ToUpper(out)
	for _, bad := range []string{"CON", "PRN", "AUX", "NUL", "COM1", "COM2", "COM3", "COM4",
		"COM5", "COM6", "COM7", "COM8", "COM9", "LPT1", "LPT2", "LPT3", "LPT4", "LPT5", "LPT6", "LPT7", "LPT8", "LPT9"} {
		if upper == bad {
			return "_" + out
		}
	}
	// 按字符（而非字节）截断，避免把多字节中文切成非法 UTF-8
	if len([]rune(out)) > 120 {
		out = strings.TrimSpace(string([]rune(out)[:120]))
	}
	return out
}

// buildVideoName 生成规范文件名
// 电影：片名 (年份) [分辨率] [来源].ext
// 剧集：剧名 (年份) - S01E02 - 标题.ext
func buildVideoName(info mediaInfo, ext string, tmpl string) string {
	ext = strings.ToLower(ext)
	title := sanitizeName(info.Title)
	if title == "" {
		title = "未命名"
	}
	var sb strings.Builder
	sb.WriteString(title)
	if info.Year > 0 {
		sb.WriteString(" (")
		sb.WriteString(strconv.Itoa(info.Year))
		sb.WriteString(")")
	}
	// 多分割（一部电影拆多个文件）：Jellyfin 惯例「片名 (年份) - cd1」
	if info.Part > 0 {
		sb.WriteString(fmt.Sprintf(" - cd%d", info.Part))
	}
	if info.IsSeries && info.Episode > 0 {
		season := info.Season
		if season <= 0 {
			season = 1
		}
		sb.WriteString(fmt.Sprintf(" - S%02dE%02d", season, info.Episode))
		if info.EpisodeTitle != "" {
			sb.WriteString(" - ")
			sb.WriteString(sanitizeName(info.EpisodeTitle))
		}
	} else {
		if info.Res != "" {
			sb.WriteString(" [")
			sb.WriteString(info.Res)
			sb.WriteString("]")
		}
		if info.Source != "" {
			sb.WriteString(" [")
			sb.WriteString(sanitizeName(info.Source))
			sb.WriteString("]")
		}
	}
	sb.WriteString(ext)
	return sb.String()
}

// buildSeriesDirName 剧集目录名：剧名 (年份)
func buildSeriesDirName(info mediaInfo) string {
	title := sanitizeName(info.Title)
	if title == "" {
		title = "未命名剧集"
	}
	if info.Year > 0 {
		return title + " (" + strconv.Itoa(info.Year) + ")"
	}
	return title
}

// seasonDirName 季度目录名：Season 01
func seasonDirName(season int) string {
	if season <= 0 {
		season = 1
	}
	return fmt.Sprintf("Season %02d", season)
}

// planVideoRename 为单个视频文件生成重命名计划
// meta 为内嵌元数据（可为零值）
func planVideoRename(path string, meta MediaMeta, useMeta bool, organizeDirs bool) mediaNamePlan {
	dir := filepath.Dir(path)
	oldName := filepath.Base(path)
	ext := filepath.Ext(path)
	base := stripCollisionSuffix(strings.TrimSuffix(oldName, ext))
	plan := mediaNamePlan{OldPath: path, NewPath: path, OldName: oldName, NewName: oldName, Dir: dir}

	// 附加内容：示例/预览文件直接跳过；预告/花絮归档时移入 extras/
	switch classifyVideoSpecial(base) {
	case "sample":
		plan.Skip = true
		plan.SkipReason = "示例/预览文件，不整理"
		return plan
	case "extras":
		plan.Special = "extras"
	}

	info := parseVideoName(oldName)

	// 目录名线索：中文下载的常见布局「庆余年 (2019)/01.mp4」
	// 「庆余年/Season 01/庆余年 - 01.mkv」光看文件名解析不出片名，
	// 目录名（当前目录 + 上一级）是唯一的片名/年份来源
	hint := dirVideoHint(path)
	if hint.Title != "" && (info.Title == "" || len([]rune(info.Title)) < 2 || isDigitsOnly(info.Title)) {
		info.Title = hint.Title
	}
	if hint.Year > 0 && info.Year == 0 {
		info.Year = hint.Year
	}
	if hint.Season > 0 && info.Season == 0 {
		info.Season = hint.Season
	}
	// 纯数字文件名（01.mp4）：有目录剧名证据时才当集数——
	// 没有证据时「24」是剧名，不能误判成第 24 集
	if !info.IsSeries && hint.Title != "" {
		if m := rePureNum.FindStringSubmatch(strings.TrimSpace(base)); m != nil {
			if ep, err := strconv.Atoi(m[1]); err == nil && ep >= 1 && ep <= 999 {
				info.Episode = ep
				info.IsSeries = true
			}
		}
	}
	// 季号归一：目录证据补出的集数同样按第 1 季算
	if info.IsSeries && info.Episode > 0 && info.Season <= 0 {
		info.Season = 1
	}

	// 内嵌元数据补充（内嵌标题更可信，但文件名里的年份/集号更结构化）
	if useMeta {
		if meta.Title != "" && (info.Title == "" || len(info.Title) < 2 || isDigitsOnly(info.Title)) {
			info.Title = cleanTitle(meta.Title)
		}
		if meta.Year > 0 && info.Year == 0 {
			info.Year = meta.Year
		}
		if info.Res == "" && meta.Resolution() != "" {
			info.Res = meta.Resolution()
		}
	}

	newName := buildVideoName(info, ext, "")
	plan.Title = info.Title
	plan.Year = info.Year
	plan.Season = info.Season
	plan.Episode = info.Episode
	plan.Part = info.Part
	plan.Res = info.Res
	plan.Source = info.Source
	plan.NewName = newName

	// 目标目录
	dirBase := filepath.Base(dir)
	newDir := dir
	switch {
	case plan.Special == "extras":
		// 附加内容只移动、不改名（Jellyfin 对 extras 保留原名）
		newName = oldName
		if organizeDirs && dirBase != extrasDirName {
			host := dir
			if info.IsSeries && info.Episode > 0 {
				host = seriesRootDir(dir, info, hint)
			} else if dirBase != movieDirName {
				host = filepath.Join(dir, movieDirName)
			}
			newDir = filepath.Join(host, extrasDirName)
			plan.Reason = "附加内容归入 extras 目录"
		}
	case organizeDirs && info.IsSeries && info.Episode > 0:
		newDir = seriesEpisodeDir(dir, info, hint)
	case organizeDirs && !info.IsSeries:
		// 电影归档到「电影」目录（已在其中则不重复嵌套）
		if dirBase != movieDirName {
			newDir = filepath.Join(dir, movieDirName)
		}
	}

	newPath := uniquePath(path, filepath.Join(newDir, newName), nil)
	plan.NewPath = newPath
	plan.NewName = filepath.Base(newPath) // 唯一化后名字可能带序号，回填保持一致
	plan.Changed = newPath != path
	if plan.Changed && plan.Reason == "" {
		if plan.NewName != oldName {
			plan.Reason = "按规范重命名"
		} else {
			plan.Reason = "按规范整理目录"
		}
	}
	return plan
}

// movieDirName 电影归档目录名
const movieDirName = "电影"

// extrasDirName 附加内容（预告/花絮）目录名
const extrasDirName = "extras"

// genericDirNames 通用目录名：从这些目录名里提取不出片名
var genericDirNames = map[string]bool{
	"movies": true, "movie": true, "film": true, "films": true, "cinema": true,
	"video": true, "videos": true, "media": true, "medias": true,
	"tv": true, "tvshow": true, "tvshows": true, "series": true, "show": true, "shows": true,
	"download": true, "downloads": true, "temp": true, "tmp": true, "cache": true,
	"new": true, "incoming": true, "upload": true, "share": true, "shares": true,
	"storage": true, "data": true, "nas": true, "disk": true, "volume": true,
	"电影": true, "电影院": true, "影片": true, "电视剧": true, "剧集": true,
	"连续剧": true, "美剧": true, "韩剧": true, "日剧": true, "国产剧": true,
	"综艺": true, "动漫": true, "动画": true, "纪录片": true, "视频": true,
	"影视": true, "媒体": true, "下载": true, "缓存": true, "最新": true,
	"合集": true, "收藏": true, "收藏夹": true, "音乐": true, "mv": true,
	"specials": true, "extra": true, "extras": true, "trailer": true, "trailers": true,
	"sample": true, "samples": true, "bdmv": true, "certificate": true,
	"video_ts": true, "audio_ts": true, "字幕": true, "字幕组": true,
}

// reDirSeasonOnly 纯季目录名：S01 / s1（parseVideoName 会把 "S01" 当标题，需排除）
var reDirSeasonOnly = regexp.MustCompile(`(?i)^s\d{1,2}$`)

// reDirGenericPat 通用目录名模式：vol1 / disk2 / media3…（数字编号的根目录）
var reDirGenericPat = regexp.MustCompile(`(?i)^(vol|volume|disk|part|media|user|home|mnt|data|share|folder)\d*$`)

// dirVideoHint 从文件所在目录（当前目录 + 上一级）提取片名/年份/季线索。
// 「Season 01」这类目录只提供季号；通用目录（电影/视频/下载…）不提供片名。
func dirVideoHint(path string) mediaInfo {
	var hint mediaInfo
	dir := filepath.Dir(path)
	for i := 0; i < 2; i++ {
		base := strings.TrimSpace(fixMojibake(filepath.Base(dir)))
		if base == "" || base == "." || base == string(filepath.Separator) {
			break
		}
		low := strings.ToLower(base)
		if !genericDirNames[low] && !reDirSeasonOnly.MatchString(base) && !reDirGenericPat.MatchString(base) && !rePureNum.MatchString(base) && len([]rune(base)) >= 2 {
			if v := parseVideoName(base); v.Title != "" && len([]rune(v.Title)) >= 2 && !isDigitsOnly(v.Title) {
				if hint.Title == "" {
					hint.Title = v.Title
				}
				if hint.Year == 0 && v.Year > 0 {
					hint.Year = v.Year
				}
				if hint.Season == 0 && v.Season > 0 {
					hint.Season = v.Season
				}
			} else if v.Season > 0 && hint.Season == 0 {
				hint.Season = v.Season
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return hint
}

// classifyVideoSpecial 识别附加内容："" 正片 / "sample" 示例预览 / "extras" 预告花絮
func classifyVideoSpecial(base string) string {
	b := " " + strings.TrimSpace(base) + " "
	if reSampleFile.MatchString(b) {
		return "sample"
	}
	if reExtrasFile.MatchString(b) || reExtrasCodes.MatchString(b) {
		return "extras"
	}
	return ""
}

// seriesRootDir 剧集根目录「剧名 (年份)」：已就位（或目录名解析结果一致）则不重复嵌套
func seriesRootDir(dir string, info mediaInfo, hint mediaInfo) string {
	root := buildSeriesDirName(info)
	if filepath.Base(dir) == root {
		return dir
	}
	// 目录名解析出的片名/年份与目标一致（如 "Breaking.Bad.2008"）→ 就地升级，不套娃
	if hint.Title != "" && hint.Year > 0 && info.Year > 0 &&
		normalizeForCompare(hint.Title) == normalizeForCompare(info.Title) && hint.Year == info.Year {
		return dir
	}
	return filepath.Join(dir, root)
}

// seriesEpisodeDir 剧集季目录「剧名 (年份)/Season 01」：已在 Season 目录则不重复嵌套
func seriesEpisodeDir(dir string, info mediaInfo, hint mediaInfo) string {
	season := info.Season
	if season <= 0 {
		season = 1
	}
	if filepath.Base(dir) == seasonDirName(season) {
		return dir
	}
	return filepath.Join(seriesRootDir(dir, info, hint), seasonDirName(season))
}

// uniquePath 避免目标重名：已存在则加 (1)、(2) 后缀。
// origin 是源文件路径——目标等于自身时不算冲突（否则已规范的文件会被改成 "xxx (1)"）。
func uniquePath(origin, path string, reserved map[string]bool) string {
	if path == origin {
		return path
	}
	if !reserved[path] {
		if _, err := os.Stat(path); err != nil {
			return path
		}
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; i < 1000; i++ {
		cand := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if cand == origin {
			return cand
		}
		if !reserved[cand] {
			if _, err := os.Stat(cand); err != nil {
				return cand
			}
		}
	}
	return path
}
