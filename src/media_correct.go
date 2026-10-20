// 媒体字段纠错引擎
//
// 现实里音乐/影视文件的标签经常是乱的：
//   - 歌曲名被写进"歌手"框，歌手名写进"歌名"框（下载站/转码工具常见）
//   - 专辑名被写进"歌名"框
//   - 标题框里装的是文件名而不是歌名
//   - 年份框里装的是别的数字
//
// 纠错思路（离线启发式，不依赖网络）：
//  1. 从文件名解析出"候选角色"（歌手/歌名/专辑/年份/集号）
//  2. 拿候选值去比对现有标签字段，找出"值对不上角色"的字段
//  3. 按启发式规则判断错位类型并重新分配
//  4. 输出纠正后的字段 + 置信度，供用户预览确认
//
// 在线匹配（MusicBrainz）拿到权威元数据后，会再覆盖这里的结果。
package main

import (
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// mediaField 标签字段的通用表示
type mediaField struct {
	Key   string `json:"key"`   // 逻辑名：title/artist/album/year/track/genre
	Value string `json:"value"` // 当前值
	Raw   string `json:"raw"`   // 原始来源说明
}

// correctedMeta 纠错结果
type correctedMeta struct {
	Meta       MediaMeta  `json:"meta"`
	Confidence float64    `json:"confidence"` // 0~1 整体置信度
	Fixes      []mediaFix `json:"fixes"`      // 纠正明细
	Warnings   []string   `json:"warnings"`
}

// mediaFix 一条纠正记录
type mediaFix struct {
	Field string `json:"field"`
	From  string `json:"from"`
	To    string `json:"to"`
	Why   string `json:"why"`
}

// ---------- 文件名候选解析 ----------

// mediaCandidates 从文件名解析出的角色候选
type mediaCandidates struct {
	Artist    string
	Title     string
	Album     string
	Year      int
	Track     int
	HasArtist bool
	HasTitle  bool
}

var (
	// "歌手 - 歌名"（最常见）
	reArtistDashTitle = regexp.MustCompile(`^(.+?)\s*-\s*(.+)$`)
	// "歌手 - 专辑 - 歌名"
	reArtistDashAlbumDashTitle = regexp.MustCompile(`^(.+?)\s*-\s*(.+?)\s*-\s*(.+)$`)
	// "01 歌名" / "01. 歌名" / "01-歌名"
	reTrackPrefix = regexp.MustCompile(`^(\d{1,3})[\s._-]+(.+)$`)
	// "歌手 _ 歌名"
	reArtistUnderscore = regexp.MustCompile(`^(.+?)\s*_\s*(.+)$`)
)

// parseAudioCandidates 从音频文件名解析角色候选
// parseAudioCandidates 从文件名解析角色候选。
// artistSecond=true 表示库里文件名是「歌名 - 歌手」顺序（由目录内重复度推断）。
func parseAudioCandidates(filename string, artistSecond bool) mediaCandidates {
	var c mediaCandidates
	base := strings.TrimSuffix(filename, extOf(filename))
	// 去掉站点后缀噪声
	base = strings.TrimSpace(base)

	// 优先"歌手 - 专辑 - 歌名"（反序库里不套用，容易误判）
	if m := reArtistDashAlbumDashTitle.FindStringSubmatch(base); m != nil && !artistSecond {
		c.Artist = strings.TrimSpace(m[1])
		c.Album = strings.TrimSpace(m[2])
		c.Title = strings.TrimSpace(m[3])
		c.HasArtist, c.HasTitle = true, true
		return c
	}
	// "歌手 - 歌名" / 反序时 "歌名 - 歌手"
	if m := reArtistDashTitle.FindStringSubmatch(base); m != nil {
		l, r := strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		a, t := l, r
		if artistSecond {
			a, t = r, l // 右侧才是歌手
		}
		// 命中常用歌手名时以它为准（比目录统计更直接）
		if lk, rk := isKnownArtist(l), isKnownArtist(r); lk != rk {
			if rk {
				a, t = r, l
			} else {
				a, t = l, r
			}
		}
		if looksLikeArtist(a) && looksLikeTitle(t) {
			c.Artist, c.Title = a, t
			c.HasArtist, c.HasTitle = true, true
			return c
		}
	}
	// "歌手 _ 歌名"（同样支持反序）
	if m := reArtistUnderscore.FindStringSubmatch(base); m != nil {
		a, t := strings.TrimSpace(m[1]), strings.TrimSpace(m[2])
		if artistSecond {
			a, t = t, a
		}
		if looksLikeArtist(a) && looksLikeTitle(t) {
			c.Artist, c.Title = a, t
			c.HasArtist, c.HasTitle = true, true
			return c
		}
	}
	// "01 歌名"
	if m := reTrackPrefix.FindStringSubmatch(base); m != nil {
		n, err := strconv.Atoi(m[1])
		if err == nil && n > 0 && n < 999 {
			c.Track = n
			c.Title = strings.TrimSpace(m[2])
			c.HasTitle = true
			return c
		}
	}
	// 兜底：整个文件名当歌名
	c.Title = base
	c.HasTitle = true
	return c
}

// extOf 取扩展名（含点）
func extOf(p string) string {
	i := strings.LastIndexByte(p, '.')
	if i < 0 {
		return ""
	}
	return p[i:]
}

// looksLikeArtist 判断字符串是否像歌手/艺术家名
// 特征：不含明显的数字年份、不含站点后缀词、长度合理、不是纯数字
func looksLikeArtist(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" || len(s) > 60 {
		return false
	}
	// 含 4 位年份 → 更像片名而非歌手
	if reYear.MatchString(s) {
		return false
	}
	// 含常见站点/技术词 → 不像歌手
	lower := strings.ToLower(s)
	for _, bad := range []string{"高清", "影视", "在线", "资源", "下载", "torrent", "www.", "http",
		"1080", "720", "4k", "x264", "x265", "rar", "zip", "mkv", "mp4", "flac", "ape"} {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	// 纯数字或含大量数字 → 不像歌手
	digits := 0
	for _, r := range s {
		if unicode.IsDigit(r) {
			digits++
		}
	}
	if digits > len(s)/3 {
		return false
	}
	return true
}

// looksLikeTitle 判断字符串是否像歌名/片名
func looksLikeTitle(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	// 歌名可以很短
	return len(s) <= 120
}

// looksLikeAlbum 判断字符串是否像专辑名
// 专辑名一般不含常见站点后缀、不含"第X首"等
func looksLikeAlbum(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	lower := strings.ToLower(s)
	for _, bad := range []string{"www.", "http", "在线", "下载", "高清"} {
		if strings.Contains(lower, bad) {
			return false
		}
	}
	return true
}

// dirArtistHint 用所在目录名做外部证据判断歌手是谁。
// 专辑目录通常以歌手或专辑命名：若某个上级目录名恰好等于「歌名框」里的值，
// 说明歌名/歌手两个字段互换了（目录名才是歌手）；等于歌手框则标签没问题。
func dirArtistHint(path string, m MediaMeta) (string, bool) {
	title := normalizeForCompare(m.Title)
	artist := normalizeForCompare(m.Artist)
	if title == "" || artist == "" || title == artist {
		return "", false
	}
	dir := filepath.Dir(path)
	titleDepth, artistDepth := -1, -1
	titleName := ""
	for i := 0; i < 4; i++ {
		base := filepath.Base(dir)
		if base == "" || base == "." || base == string(filepath.Separator) {
			break
		}
		nb := normalizeForCompare(base)
		if nb == title {
			titleDepth, titleName = i, base
		}
		if nb == artist {
			artistDepth = i
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	// 目录层级越高越像「歌手目录」（歌手在上、专辑在下）：
	// 只有上层命中歌名框、且没有更上层的歌手目录时，才认定两个字段互换
	if titleDepth >= 0 && titleDepth > artistDepth {
		return titleName, true
	}
	return "", false
}

// ---------- 相似度 ----------

// normalizeForCompare 归一化用于比较：小写、去符号、去空格
func normalizeForCompare(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var sb strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r), unicode.IsDigit(r):
			sb.WriteRune(r)
		default:
			// 跳过标点与空格
		}
	}
	return sb.String()
}

// similarity 两个字符串的相似度 0~1
// 采用"包含 + 编辑距离"组合
func similarity(a, b string) float64 {
	na, nb := normalizeForCompare(a), normalizeForCompare(b)
	if na == "" || nb == "" {
		return 0
	}
	if na == nb {
		return 1
	}
	if strings.Contains(na, nb) || strings.Contains(nb, na) {
		// 长度差越大惩罚越重
		shorter, longer := len(na), len(nb)
		if shorter > longer {
			shorter, longer = longer, shorter
		}
		return 0.7 + 0.25*float64(shorter)/float64(longer)
	}
	// 编辑距离
	d := levenshtein(na, nb)
	maxLen := len(na)
	if len(nb) > maxLen {
		maxLen = len(nb)
	}
	if maxLen == 0 {
		return 0
	}
	return 1 - float64(d)/float64(maxLen)
}

// levenshtein 编辑距离
func levenshtein(a, b string) int {
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			m := prev[j] + 1
			if cur[j-1]+1 < m {
				m = cur[j-1] + 1
			}
			if prev[j-1]+cost < m {
				m = prev[j-1] + cost
			}
			cur[j] = m
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

// ---------- 音频字段纠错 ----------

// correctAudioTags 对音频文件的标签做字段纠错
// meta 为读到的原始标签，filename 为文件名
func correctAudioTags(meta MediaMeta, filename string, artistSecond bool, dirPath string) correctedMeta {
	res := correctedMeta{Meta: meta, Confidence: 0.5}
	cand := parseAudioCandidates(filename, artistSecond)

	// 收集现有字段值
	origTitle, origArtist, origAlbum := meta.Title, meta.Artist, meta.Album

	// 情形 1：歌名与歌手完全相同（下载站常见：所有字段填同一个）
	if origTitle != "" && origTitle == origArtist {
		// 文件名能解析出歌手 → 歌手框装的是歌名
		if cand.HasArtist && similarity(cand.Artist, origTitle) < 0.6 && looksLikeArtist(cand.Artist) {
			res.Meta.Artist = cand.Artist
			res.Meta.Title = origTitle
			res.Fixes = append(res.Fixes, mediaFix{
				Field: "artist", From: origArtist, To: cand.Artist,
				Why: "歌名与歌手框内容相同，按文件名中的歌手纠正",
			})
			res.Confidence = 0.8
		}
	}

	// 情形 2：文件名里的歌手/歌名顺序与标签相反。
	// 这种情况本地无法判定谁错了——可能标签错位，也可能文件名被改反了
	// （例如整批文件是「歌名 - 歌手」，标签却是对的）。
	// 标签是文件自带信息，回写又不可逆，所以这里**只告警不改写**；
	// 真正需要靠外部知识判断的错位，交给「联网匹配」用权威库定案。
	if origArtist != "" && origTitle != "" && cand.HasArtist && cand.HasTitle {
		simAT := similarity(origArtist, origTitle)
		reversed := similarity(cand.Artist, origTitle) > 0.7 && similarity(cand.Title, origArtist) > 0.7
		sameOrder := similarity(cand.Artist, origArtist) > 0.7 && similarity(cand.Title, origTitle) > 0.7
		if reversed && !sameOrder {
			res.Warnings = append(res.Warnings, "文件名里歌手/歌名顺序与标签相反，已按标签为准（标签若有误可用联网匹配纠正）")
		} else if simAT > 0.9 {
			res.Warnings = append(res.Warnings, "歌名与歌手内容几乎相同，可能未正确填写")
		}
	}

	// 情形 3：专辑框与歌名相同
	// 同名专辑（单曲/EP/同名专辑）非常常见，清空反而会把文件挪错目录，
	// 所以只提示不改写。只有专辑框明显是整条文件名时才清空。
	if origAlbum != "" && origTitle != "" {
		if similarity(origAlbum, origTitle) > 0.85 && filepath.Ext(origAlbum) != "" {
			res.Meta.Album = ""
			res.Fixes = append(res.Fixes, mediaFix{
				Field: "album", From: origAlbum, To: "",
				Why: "专辑框里是整条文件名，不是专辑名，已清空",
			})
			res.Confidence = 0.75
		} else if similarity(origAlbum, origTitle) > 0.85 {
			res.Warnings = append(res.Warnings, "专辑名与歌名相同（同名专辑），保持原样")
		}
	}

	// 情形 4：标题框装的是文件名而非歌名
	if origTitle != "" && cand.HasTitle {
		if similarity(origTitle, filename) > 0.9 && similarity(origTitle, cand.Title) < 0.6 {
			res.Meta.Title = cand.Title
			res.Fixes = append(res.Fixes, mediaFix{
				Field: "title", From: origTitle, To: cand.Title,
				Why: "标题框内容是原始文件名，改用文件名解析出的歌名",
			})
			res.Confidence = 0.7
		}
	}

	// 情形 5：歌手框装的是歌名但没有歌名框（歌名缺失）
	if res.Meta.Title == "" && cand.HasTitle {
		res.Meta.Title = cand.Title
		res.Fixes = append(res.Fixes, mediaFix{
			Field: "title", From: "", To: cand.Title,
			Why: "歌名缺失，按文件名补全",
		})
		res.Confidence = 0.6
	}
	// 情形 6：歌手缺失但文件名有
	if res.Meta.Artist == "" && cand.HasArtist {
		res.Meta.Artist = cand.Artist
		res.Fixes = append(res.Fixes, mediaFix{
			Field: "artist", From: "", To: cand.Artist,
			Why: "歌手缺失，按文件名补全",
		})
		res.Confidence = 0.6
	}

	// 情形 10：歌名框里是常用歌手名、歌手框里不是 → 两个字段写反了
	if res.Meta.Title != "" && res.Meta.Artist != "" && res.Meta.Title != res.Meta.Artist {
		if isKnownArtist(res.Meta.Title) && !isKnownArtist(res.Meta.Artist) {
			oldTitle, oldArtist := res.Meta.Title, res.Meta.Artist
			res.Meta.Title, res.Meta.Artist = oldArtist, oldTitle
			res.Fixes = append(res.Fixes, mediaFix{
				Field: "title", From: oldTitle, To: res.Meta.Title,
				Why: "歌名框里是常用歌手名，与歌手框内容互换",
			})
			res.Confidence = 0.88
		}
	}

	// 情形 9：所在文件夹名是歌手 → 歌名/歌手框内容互换
	// 这是本地唯一可靠的"外部证据"（专辑目录名通常就是歌手名）
	if dirPath != "" {
		if name, ok := dirArtistHint(dirPath, res.Meta); ok {
			oldTitle, oldArtist := res.Meta.Title, res.Meta.Artist
			res.Meta.Title, res.Meta.Artist = oldArtist, oldTitle
			res.Fixes = append(res.Fixes, mediaFix{
				Field: "title", From: oldTitle, To: res.Meta.Title,
				Why: "所在文件夹「" + name + "」是歌手名，歌名与歌手框内容互换",
			})
			res.Confidence = 0.9
		}
	}

	// 情形 7：年份非法
	if res.Meta.Year < 1900 || res.Meta.Year > 2100 {
		res.Meta.Year = 0
	}
	// 情形 8：音轨号非法
	if res.Meta.Track < 0 || res.Meta.Track > 9999 {
		res.Meta.Track = 0
	}

	// 文件名里的音轨号优先（专辑内序号）
	if cand.Track > 0 && res.Meta.Track != cand.Track {
		res.Fixes = append(res.Fixes, mediaFix{
			Field: "track", From: strconv.Itoa(res.Meta.Track), To: strconv.Itoa(cand.Track),
			Why: "按文件名中的序号纠正音轨号",
		})
		res.Meta.Track = cand.Track
	}

	if len(res.Fixes) == 0 {
		res.Confidence = 0.9 // 无需纠正，说明标签本身自洽
	}
	return res
}

// ---------- 视频字段纠错 ----------

// correctVideoTags 对视频文件的元数据做字段纠错
func correctVideoTags(meta MediaMeta, filename string) correctedMeta {
	res := correctedMeta{Meta: meta, Confidence: 0.6}
	info := parseVideoName(filename)
	base := strings.TrimSuffix(filename, extOf(filename))

	// 情形 1：标题框装的是原始文件名
	if meta.Title != "" && similarity(meta.Title, base) > 0.85 && info.Title != "" && similarity(meta.Title, info.Title) < 0.6 {
		res.Meta.Title = info.Title
		res.Fixes = append(res.Fixes, mediaFix{
			Field: "title", From: meta.Title, To: info.Title,
			Why: "标题框内容是原始文件名，改用文件名解析出的片名",
		})
		res.Confidence = 0.75
	} else if meta.Title == "" && info.Title != "" {
		res.Meta.Title = info.Title
		res.Fixes = append(res.Fixes, mediaFix{
			Field: "title", From: "", To: info.Title,
			Why: "标题缺失，按文件名补全",
		})
		res.Confidence = 0.7
	}

	// 情形 2：年份与文件名不一致（文件名更可信，扫描器也认文件名）
	if meta.Year > 0 && info.Year > 0 && meta.Year != info.Year {
		res.Fixes = append(res.Fixes, mediaFix{
			Field: "year", From: strconv.Itoa(meta.Year), To: strconv.Itoa(info.Year),
			Why: "内嵌年份与文件名不一致，按文件名纠正（媒体库以文件名为准）",
		})
		res.Meta.Year = info.Year
		res.Confidence = 0.8
	} else if meta.Year == 0 && info.Year > 0 {
		res.Meta.Year = info.Year
		res.Fixes = append(res.Fixes, mediaFix{
			Field: "year", From: "", To: strconv.Itoa(info.Year),
			Why: "年份缺失，按文件名补全",
		})
		res.Confidence = 0.75
	}

	// 情形 3：标题里塞了年份（媒体库会把年份当片名的一部分，导致匹配失败）
	if res.Meta.Title != "" {
		cleaned := strings.TrimSpace(reYear.ReplaceAllString(res.Meta.Title, " "))
		cleaned = strings.Join(strings.Fields(cleaned), " ")
		if cleaned != res.Meta.Title && cleaned != "" {
			res.Fixes = append(res.Fixes, mediaFix{
				Field: "title", From: res.Meta.Title, To: cleaned,
				Why: "标题中混入了年份，剥离后更易被媒体库匹配",
			})
			res.Meta.Title = cleaned
			res.Confidence = 0.8
		}
	}

	// 情形 4：标题里带画质标签（1080p 等会干扰刮削）
	if res.Meta.Title != "" {
		cleaned := reRes.ReplaceAllString(res.Meta.Title, " ")
		cleaned = strings.Join(strings.Fields(cleaned), " ")
		if cleaned != res.Meta.Title && cleaned != "" {
			res.Fixes = append(res.Fixes, mediaFix{
				Field: "title", From: res.Meta.Title, To: cleaned,
				Why: "标题中混入了画质标签，剥离后更易被媒体库匹配",
			})
			res.Meta.Title = cleaned
			res.Confidence = 0.8
		}
	}

	// 情形 5：年份非法
	if res.Meta.Year < 1900 || res.Meta.Year > 2100 {
		res.Meta.Year = 0
	}

	if len(res.Fixes) == 0 {
		res.Confidence = 0.9
	}
	return res
}

// mergeCorrected 把在线匹配到的权威元数据合并进纠错结果
// online 为空时只返回纠错结果；置信度取两者较高值
func mergeCorrected(c correctedMeta, online MediaMeta, onlineOK bool) correctedMeta {
	if !onlineOK {
		return c
	}
	// 在线结果优先，但保留本地有而在线没有的（如时长）
	if online.Title != "" && online.Title != c.Meta.Title {
		c.Fixes = append(c.Fixes, mediaFix{
			Field: "title", From: c.Meta.Title, To: online.Title,
			Why: "在线元数据库匹配结果（更权威）",
		})
		c.Meta.Title = online.Title
	}
	if online.Artist != "" && online.Artist != c.Meta.Artist {
		c.Fixes = append(c.Fixes, mediaFix{
			Field: "artist", From: c.Meta.Artist, To: online.Artist,
			Why: "在线元数据库匹配结果（更权威）",
		})
		c.Meta.Artist = online.Artist
	}
	if online.Album != "" && online.Album != c.Meta.Album {
		c.Fixes = append(c.Fixes, mediaFix{
			Field: "album", From: c.Meta.Album, To: online.Album,
			Why: "在线元数据库匹配结果（更权威）",
		})
		c.Meta.Album = online.Album
	}
	if online.Year > 0 && online.Year != c.Meta.Year {
		c.Fixes = append(c.Fixes, mediaFix{
			Field: "year", From: strconv.Itoa(c.Meta.Year), To: strconv.Itoa(online.Year),
			Why: "在线元数据库匹配结果（更权威）",
		})
		c.Meta.Year = online.Year
	}
	if online.Track > 0 && online.Track != c.Meta.Track {
		c.Fixes = append(c.Fixes, mediaFix{
			Field: "track", From: strconv.Itoa(c.Meta.Track), To: strconv.Itoa(online.Track),
			Why: "在线元数据库匹配结果（更权威）",
		})
		c.Meta.Track = online.Track
	}
	if online.TrackTotal > 0 {
		c.Meta.TrackTotal = online.TrackTotal
	}
	if online.Genre != "" {
		c.Meta.Genre = online.Genre
	}
	if online.Duration > 0 && c.Meta.Duration <= 0 {
		c.Meta.Duration = online.Duration
	}
	c.Confidence = 0.95
	return c
}
