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
	Res     string `json:"res"`
	Source  string `json:"source"`
	Reason  string `json:"reason"`
	Changed bool   `json:"changed"`
}

// ---------- 文件名解析 ----------

var (
	// 年份：19xx/20xx
	reYear = regexp.MustCompile(`(?:^|[^\d])((?:19|20)\d{2})(?:[^\d]|$)`)
	// 剧集：S01E02 / s1.e2 / 第01集
	reSxxExx = regexp.MustCompile(`(?i)S(\d{1,2})[\s._-]*E(\d{1,3})`)
	// 中文剧集：第01集 / 第1季
	reCnEpisode = regexp.MustCompile(`第\s*(\d{1,3})\s*集`)
	reCnSeason  = regexp.MustCompile(`第\s*(\d{1,2})\s*季`)
	// 剧集：01x02
	reXxx = regexp.MustCompile(`(?i)(\d{1,2})x(\d{1,3})`)
	// 分辨率标签
	reRes = regexp.MustCompile(`(?i)\b(2160p|1080[pi]|1080p|720[pi]|720p|480[pi]|480p|4k|uhd|8k)\b`)
	// 源码/发布组：方括号、圆括号内的编码与来源
	reBracket = regexp.MustCompile(`[\[\(]([^\]\)]*)[\]\)]`)
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
)

// mediaInfo 从文件名与元数据解析出的信息
type mediaInfo struct {
	Title        string
	Year         int
	Season       int
	Episode      int
	EpisodeTitle string
	Res          string
	Source       string
	IsSeries     bool
}

// parseVideoName 从文件名解析视频信息
func parseVideoName(name string) mediaInfo {
	var info mediaInfo
	base := strings.TrimSuffix(name, filepath.Ext(name))
	orig := base

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
	} else if m := reXxx.FindStringSubmatch(base); m != nil {
		info.Season, _ = strconv.Atoi(m[1])
		info.Episode, _ = strconv.Atoi(m[2])
		info.IsSeries = true
		base = reXxx.ReplaceAllString(base, " ")
	}
	// 中文季
	if m := reCnSeason.FindStringSubmatch(base); m != nil {
		info.Season, _ = strconv.Atoi(m[1])
		info.IsSeries = true
		base = reCnSeason.ReplaceAllString(base, " ")
	}

	// 分辨率
	if m := reRes.FindStringSubmatch(base); m != nil {
		info.Res = normalizeRes(m[1])
		base = reRes.ReplaceAllString(base, " ")
	}

	// 方括号/圆括号内容：可能是年份、画质、发布组
	var bracketParts []string
	base = reBracket.ReplaceAllStringFunc(base, func(s string) string {
		inner := strings.Trim(s, "[]()")
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
	base = reCodecGroup.ReplaceAllString(base, " ")
	base = reSourceKW.ReplaceAllString(base, " ")
	base = reUpperTail.ReplaceAllString(base, " ")

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
}

// cleanTitle 清洗标题：去站点后缀、多余符号
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
	// 常见分隔符归一：把 . _ 换成空格（但保留已有空格结构）
	s = strings.ReplaceAll(s, ".", " ")
	s = strings.ReplaceAll(s, "_", " ")
	s = strings.ReplaceAll(s, "，", " ")
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
	plan := mediaNamePlan{OldPath: path, NewPath: path, OldName: oldName, NewName: oldName, Dir: dir}

	info := parseVideoName(oldName)

	// 内嵌元数据补充（内嵌标题更可信，但文件名里的年份/集号更结构化）
	if useMeta {
		if meta.Title != "" && (info.Title == "" || len(info.Title) < 2) {
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
	plan.Res = info.Res
	plan.Source = info.Source
	plan.NewName = newName

	newDir := dir
	if organizeDirs && info.IsSeries && info.Episode > 0 {
		seriesDir := filepath.Join(dir, buildSeriesDirName(info))
		season := info.Season
		if season <= 0 {
			season = 1
		}
		newDir = filepath.Join(seriesDir, seasonDirName(season))
	}
	newPath := uniquePath(path, filepath.Join(newDir, newName), nil)
	plan.NewPath = newPath
	plan.NewName = filepath.Base(newPath) // 唯一化后名字可能带序号，回填保持一致
	plan.Changed = newPath != path
	if plan.Changed {
		if plan.NewName != oldName {
			plan.Reason = "按规范重命名"
		} else {
			plan.Reason = "按规范整理目录"
		}
	}
	return plan
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
