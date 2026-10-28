package main

// 短剧搜索聚合（免登录）。
//
// 生态现实（实测）：当前短剧源站（黑豆/短剧搜/rejusou/剧多多等）绝大多数只提供
// 夸克网盘分享链接，无磁力、无 m3u8 播放流、无 HTTP 直链；而夸克网盘无法免登录抓取
// （服务器直连 pan.quark.cn 返回 HTTP 000）。因此本搜索只做「聚合检索 + 展示网盘
// 分享链接」，下载动作交给用户在前端「复制链接 / 打开跳转」到网盘（需登录转存）。
//
// 源清单（公开接口/页面，无需登录）：
//   - 黑豆 hddj.cc   搜索页 /search.html?t= 真实按关键词过滤，命中每条再抓详情页
//                    /duanju/{id}.html 提取夸克网盘分享链接（主源，结果最干净）
//   - rejusou  API   https://rejusou.com/api/search?keyword= JSON 聚合源，但服务端
//                    关键词不过滤（total_result 恒为全库），需本端按标题子串自过滤
//
// 设计约束：
//   - 每源独立超时（dlSourceTimeout），整轮预算 dlSearchTotalBudget；单源失败静默，
//     前端展示各源状态行即可，绝不因单源失败整体报错。
//   - 结果按「来源+详情页 URL」去重；关键词拼接前统一 url.QueryEscape。

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// m3u8 采集源白名单（实测可达、免登录、返回 macCMS 结构，含 e报播地址）
var srtM3u8Endpoints = []string{
	"https://mdzyapi.com/api.php/provide/vod/",
}

// SrtLink 一条网盘分享链接
type SrtLink struct {
	Kind string `json:"kind"`           // 网盘类型：quark / baidu / aliyun / 139 / lanzou …
	Name string `json:"name,omitempty"` // 识别到的网盘名（如 夸克网盘）
	URL  string `json:"url"`            // 完整分享 URL（可打开/复制）
}

// SrtItem 一条短剧搜索结果
type SrtItem struct {
	Source    string    `json:"source"`              // 来源显示名（黑豆 / rejusou / m3u8）
	Title     string    `json:"title"`               // 剧名（去站点标记）
	Episodes  string    `json:"episodes,omitempty"`  // 集数文本（如 76集）
	Date      string    `json:"date,omitempty"`      // 更新/发布日（YYYY-MM-DD，可空）
	DetailURL string    `json:"detailUrl,omitempty"` // 站内详情页（可空）
	Links     []SrtLink `json:"links"`               // 网盘链接（≥1，仅网盘源有）
	Note      string    `json:"note,omitempty"`      // 附加说明

	// m3u8 直链（v1.3.58）：采集源返回的真实播放地址，前端据此提供「内置下载」
	Plays       []SrtPlay `json:"plays,omitempty"`       // 分集 m3u8（≥1）
	PlaySource  string    `json:"playSource,omitempty"`  // m3u8 采集源名（m3u8-<host>）
	PlayEpCount int       `json:"playEpCount,omitempty"` // 可分集数
}

// SrtPlay 一集 m3u8 播放地址
type SrtPlay struct {
	Name string `json:"name"` // 集名（如 第01集）
	URL  string `json:"url"`  // master m3u8 直链
}

// srtSource 短剧源（与 BT 搜索的 dlSource 并列，返回 SrtItem）
type srtSource struct {
	Name string
	Run  func(ctx context.Context, kw string) ([]SrtItem, error)
}

// srtSources 短剧聚合源清单
var srtSources = []srtSource{
	{Name: "黑豆", Run: srtSrcHddj},
	{Name: "rejusou", Run: srtSrcRejusou},
	{Name: "m3u8采集", Run: srtSrcM3u8},
}

// srtSearch 并发查询全部源 → 去重 → 排序，返回条目列表与各源状态
func srtSearch(keyword string) ([]SrtItem, []dlSourceStat) {
	kw := strings.TrimSpace(keyword)
	kw = strings.TrimSpace(strings.Trim(kw, "\"'`"))
	out := make([][]SrtItem, len(srtSources))
	stats := make([]dlSourceStat, len(srtSources))
	var wg sync.WaitGroup
	for i := range srtSources {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			src := srtSources[i]
			ctx, cancel := context.WithTimeout(context.Background(), dlSourceTimeout)
			defer cancel()
			t0 := time.Now()
			items, err := src.Run(ctx, kw)
			stats[i] = dlSourceStat{Name: src.Name, Ms: time.Since(t0).Milliseconds()}
			if err != nil && len(items) == 0 {
				stats[i].Err = err.Error()
				return
			}
			if len(items) > dlSourceMaxPerSource {
				items = items[:dlSourceMaxPerSource]
			}
			out[i] = items
			stats[i].Count = len(items)
		}(i)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(dlSearchTotalBudget):
	}
	var items []SrtItem
	for i := range out {
		items = append(items, out[i]...)
	}
	items = srtDedup(items)
	sort.Slice(items, func(a, b int) bool {
		da, db := items[a].Date, items[b].Date
		if da != db {
			if da == "" {
				return false
			}
			if db == "" {
				return true
			}
			return da > db
		}
		return items[a].Title < items[b].Title
	})
	return items, stats
}

// srtDedup 按「来源+详情页URL」去重；无详情页的按「来源+标题」去重
func srtDedup(items []SrtItem) []SrtItem {
	seen := map[string]bool{}
	out := make([]SrtItem, 0, len(items))
	for _, it := range items {
		key := it.Source + "|"
		if it.DetailURL != "" {
			key += "d:" + it.DetailURL
		} else {
			key += "t:" + strings.ToLower(dlNormTitle(it.Title))
		}
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, it)
	}
	return out
}

// ---------- 源 1：黑豆短剧（hddj.cc） ----------

// srtSrcHddj 黑豆搜索：/search.html?t= 真实过滤 → 每条再并发抓详情页取网盘链接。
func srtSrcHddj(ctx context.Context, kw string) ([]SrtItem, error) {
	raw, err := dlFetch(ctx, "https://www.hddj.cc/search.html?t="+url.QueryEscape(kw), nil)
	if err != nil {
		return nil, err
	}
	entries := parseSrtHddjList(string(raw))
	if len(entries) == 0 {
		return nil, nil
	}
	items := make([]SrtItem, len(entries))
	var wg sync.WaitGroup
	for i, e := range entries {
		wg.Add(1)
		go func(i int, e srtHddjEntry) {
			defer wg.Done()
			e2, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()
			links := srtHddjDetailLinks(e2, e.detailURL)
			it := SrtItem{
				Source:    "黑豆",
				Title:     e.title,
				Episodes:  e.episodes,
				Date:      e.date,
				DetailURL: e.detailURL,
				Links:     links,
			}
			if len(links) == 0 {
				it.Note = "详情页暂未抓取到网盘链接，请点「详情」查看"
			}
			items[i] = it
		}(i, e)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
	}
	kept := items[:0]
	for _, it := range items {
		if it.Title != "" {
			kept = append(kept, it)
		}
	}
	return kept, nil
}

// srtHddjEntry 黑豆搜索结果页单条
type srtHddjEntry struct {
	title     string
	episodes  string
	date      string
	detailURL string
}

// 黑豆搜索页结构（实测）：
// <li><a href="/duanju/43.html"><span>枕边的恶魔</span>（76集）</a><span>2024-03-20</span></li>
var (
	srtHddjLiRe   = regexp.MustCompile(`(?s)<li>(.*?)</li>`)
	srtHddjARe    = regexp.MustCompile(`(?s)<a\s+href="([^"]+)"[^>]*>(.*?)</a>`)
	srtHddjSpanRe = regexp.MustCompile(`(?s)<span[^>]*>(.*?)</span>`)
	srtHddjEpRe   = regexp.MustCompile(`(?:（|\(|〔|【)?(\d{1,4}\s*集)`)

	// srtHddjDateRe 日期：<li> 内第 2 个 <span> 之后紧跟的 YYYY-MM-DD
	srtHddjDateRe = regexp.MustCompile(`(\d{4}-\d{2}-\d{2})`)

	// srtPanHrefRe 网盘分享链接（夸克/百度/阿里/移动云盘/蓝奏 等）
	srtPanHrefRe = regexp.MustCompile(`href="(https?://[^"]*(?:pan\.quark\.cn|pan\.baidu\.com|aliyundrive\.com|alipan\.com|cloud\.189\.cn|yun\.139\.com|lanzou[^/]*\.com?|quark\.cn)[^"]*)"`)
)

// parseSrtHddjList 解析黑豆 /search.html 结果
func parseSrtHddjList(page string) []srtHddjEntry {
	var out []srtHddjEntry
	for _, li := range srtHddjLiRe.FindAllStringSubmatch(page, dlSourceMaxPerSource) {
		body := li[1]
		am := srtHddjARe.FindStringSubmatch(body)
		if len(am) < 3 {
			continue
		}
		href := am[1]
		if !strings.HasPrefix(href, "/duanju/") {
			continue
		}
		title := ""
		if sm := srtHddjSpanRe.FindStringSubmatch(am[2]); len(sm) >= 2 {
			title = html.UnescapeString(strings.TrimSpace(sm[1]))
		}
		if title == "" {
			title = html.UnescapeString(stripSrtTags(am[2]))
		}
		if title == "" {
			continue
		}
		ep := ""
		if em := srtHddjEpRe.FindStringSubmatch(body); len(em) >= 2 {
			ep = em[1]
		}
		date := ""
		if dm := srtHddjDateRe.FindStringSubmatch(body); len(dm) >= 2 {
			date = strings.TrimSpace(dm[1])
		}
		out = append(out, srtHddjEntry{
			title:     title,
			episodes:  ep,
			date:      date,
			detailURL: "https://www.hddj.cc" + href,
		})
		if len(out) >= dlSourceMaxPerSource {
			break
		}
	}
	return out
}

// srtHddjDetailLinks 抓黑豆详情页，提取网盘分享链接
func srtHddjDetailLinks(ctx context.Context, detailURL string) []SrtLink {
	raw, err := dlFetch(ctx, detailURL, nil)
	if err != nil {
		return nil
	}
	page := string(raw)
	seen := map[string]bool{}
	var links []SrtLink
	for _, m := range srtPanHrefRe.FindAllStringSubmatch(page, 12) {
		u := strings.TrimSpace(m[1])
		if u == "" || seen[u] {
			continue
		}
		seen[u] = true
		kind, name := srtClassifyPan(strings.ToLower(u))
		links = append(links, SrtLink{Kind: kind, Name: name, URL: u})
	}
	return links
}

// srtClassifyPan 根据 URL 判断网盘类型与展示名
func srtClassifyPan(lu string) (kind, name string) {
	switch {
	case strings.Contains(lu, "pan.quark.cn"):
		return "quark", "夸克网盘"
	case strings.Contains(lu, "pan.baidu.com"):
		return "baidu", "百度网盘"
	case strings.Contains(lu, "aliyundrive.com"), strings.Contains(lu, "alipan.com"):
		return "aliyun", "阿里云盘"
	case strings.Contains(lu, "cloud.189.cn"), strings.Contains(lu, "yun.139.com"):
		return "139", "中国移动云盘"
	case strings.Contains(lu, "lanzou"):
		return "lanzou", "蓝奏云"
	default:
		return "other", "网盘"
	}
}

// stripSrtTags 去 HTML 标签
func stripSrtTags(s string) string {
	return strings.TrimSpace(reTags.ReplaceAllString(s, ""))
}

// ---------- 源 2：rejusou API（不过滤，需本端自过滤） ----------

// srtSrcRejusou rejusou JSON 聚合源。该接口关键词【不过滤】（返回全库热门，
// total_result 恒为全库量），故服务端按标题含关键词自过滤，再尽量收敛网盘链接。
func srtSrcRejusou(ctx context.Context, kw string) ([]SrtItem, error) {
	raw, err := dlFetch(ctx, "https://rejusou.com/api/search?keyword="+url.QueryEscape(kw), nil)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Code int `json:"code"`
		Data struct {
			Items []struct {
				Title string `json:"title"`
				URL   string `json:"url"`
				Times string `json:"times"`
			} `json:"items"`
		} `json:"data"`
	}
	if json.Unmarshal(raw, &doc) != nil {
		return nil, fmt.Errorf("响应解析失败")
	}
	kwLower := strings.ToLower(kw)
	var items []SrtItem
	for _, it := range doc.Data.Items {
		t := strings.TrimSpace(it.Title)
		if t == "" {
			continue
		}
		// 服务端自过滤：标题必须含关键词（CJK 用子串，英文/数字用小写匹配提升精度）
		if !srtTitleContains(t, kw, kwLower) {
			continue
		}
		ep := ""
		if em := srtHddjEpRe.FindStringSubmatch(t); len(em) >= 2 {
			ep = em[1]
		}
		date := strings.TrimSpace(it.Times)
		links := []SrtLink{}
		if u := strings.TrimSpace(it.URL); u != "" {
			kind, name := srtClassifyPan(strings.ToLower(u))
			links = append(links, SrtLink{Kind: kind, Name: name, URL: u})
		}
		note := ""
		if !srtHasCJK(t) {
			note = "可能非中文短剧，建议核对"
		}
		items = append(items, SrtItem{
			Source:   "rejusou",
			Title:    t,
			Episodes: ep,
			Date:     date,
			Links:    links,
			Note:     note,
		})
		if len(items) >= dlSourceMaxPerSource {
			break
		}
	}
	return items, nil
}

// srtTitleContains 判断标题是否含关键词：CJK 用子串，否则小写匹配提升精度
func srtTitleContains(title, kw, kwLower string) bool {
	if srtHasCJK(kw) {
		return strings.Contains(title, kw)
	}
	return strings.Contains(strings.ToLower(title), kwLower)
}

// srtHasCJK 判断是否含中日韩字符
func srtHasCJK(s string) bool { return dlHasCJK(s) }
// ---------- 源 3：m3u8 采集源（macCMS 接口，免登录返回真实 HLS 播放地址） ----------

// srtM3u8Vod macCMS 采集接口的单条影片字段（ac=detail / ac=videolist 通用）
type srtM3u8Vod struct {
	VodID    int    `json:"vod_id"`
	VodName  string `json:"vod_name"`
	TypeName string `json:"type_name,omitempty"`
	Remarks  string `json:"vod_remarks"`
	Pubdate  string `json:"vod_pubdate"`
	PlayFrom string `json:"vod_play_from"`
	PlayURL  string `json:"vod_play_url"`
	DownURL  string `json:"vod_down_url"`
}

// srtM3u8Detail 采集详情：id → 完整分集 m3u8。返回空切片表示该剧无可下载分集。
func srtM3u8Detail(ctx context.Context, base string, id int) srtM3u8Vod {
	u := base + "?ac=videolist&ids=" + strconv.Itoa(id)
	raw, err := dlFetch(ctx, u, map[string]string{"Referer": base, "Accept": "application/json"})
	if err != nil {
		return srtM3u8Vod{}
	}
	var doc struct {
		List []srtM3u8Vod `json:"list"`
	}
	if json.Unmarshal(raw, &doc) != nil || len(doc.List) == 0 {
		return srtM3u8Vod{}
	}
	return doc.List[0]
}

// srtM3u8ParsePlayURL 把 macCMS 的 vod_play_url（"集名$url#集名$url"）解析为分集列表。
// - 形如 "第01集$https://…/index.m3u8#第02集$…"
// - 也有 "第01集$m3u8:https://…" 前缀形式（key^name$url），这里只认标准 mac 播放格式。
func srtM3u8ParsePlayURL(playFrom, playURL string) []SrtPlay {
	raw := strings.TrimSpace(playURL)
	if raw == "" || raw == "-" || raw == "$" {
		return nil
	}
	// macCMS 有时 play_url 前带 "源名$" 前缀（如 "modum3u8$第01集$https…"），
	// 或分集格式为 "第xx集$url"。先按 $ 拆出「集名#url」整体串，再对每集切分。
	var plays []SrtPlay
	for _, seg := range strings.Split(raw, "#") {
		seg = strings.Trim(seg, " \t")
		if seg == "" {
			continue
		}
		// 每段形如：名称$URL
		// 兼容 "key^url"（苹果 TV 协议之外罕见）以及前缀 m3u8: 直链
		s := seg
		// 去掉 leading "玩法名$"（第一次 $ 前若不含 :// 且后有 $ 视为玩法名）
		name, u := srtM3u8SplitSeg(s)
		if u == "" {
			continue
		}
		if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
			// 兼容 "m3u8:https://…" / "mp4:https://…" 前缀
			if i := strings.Index(u, ":"); i > 0 {
				u = u[i+1:]
			}
			if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
				continue
			}
		}
		plays = append(plays, SrtPlay{Name: strings.TrimSpace(name), URL: strings.TrimSpace(u)})
	}
	return plays
}

// srtM3u8SplitSeg 拆一集 "名称$url"。处理前导玩法名（如 "modum3u8$第01集$https…"）。
// 取最后一个 $ 分隔 URL。若没有 $ 则整段视为 URL。
func srtM3u8SplitSeg(seg string) (name, url string) {
	// 规则：最后一个 $ 之后若以 http 开头 → 左侧是名称（可能含玩法名）；否则按普通切分
	i := strings.LastIndex(seg, "$")
	if i < 0 {
		// 无 $：整段算 name，无 url
		if strings.HasPrefix(seg, "http") {
			return "", seg
		}
		return seg, ""
	}
	u := seg[i+1:]
	name = seg[:i]
	// 名称里可能还有一层玩法名 "源名$集名"，取最后一层作集名
	if j := strings.LastIndex(name, "$"); j >= 0 {
		name = name[j+1:]
	}
	return strings.TrimSpace(name), strings.TrimSpace(u)
}

// srtM3u8EpText 剧名旁的集数文案：优先用 remarks（如 "更新至xx期"），否则用分集数
func srtM3u8EpText(remarks string, epCount int) string {
	remarks = strings.TrimSpace(remarks)
	if remarks != "" && remarks != "-" {
		return remarks
	}
	if epCount > 0 {
		return fmt.Sprintf("%d集", epCount)
	}
	return ""
}

// srtM3u8Host 取采集接口域名（用于 Source 标注）
func srtM3u8Host(base string) string {
	u, err := url.Parse(base)
	if err != nil {
		return base
	}
	return u.Host
}

// srtSrcM3u8 短剧 m3u8 采集源。调用 macCMS 采集接口 ac=detail&wd={kw} 搜索短剧，
// 命中的每条再调 ac=videolist&ids= 取完整分集的 m3u8 播放地址（vod_play_url）。
// 结果带 Plays 字段，前端据此提供「内置下载」。
func srtSrcM3u8(ctx context.Context, kw string) ([]SrtItem, error) {
	var out []SrtItem
	seen := map[int]bool{}
	for _, base := range srtM3u8Endpoints {
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		default:
		}
		searchURL := base + "?ac=detail&wd=" + url.QueryEscape(kw)
		raw, err := dlFetch(ctx, searchURL, map[string]string{"Referer": base, "Accept": "application/json"})
		if err != nil {
			continue // 单源失败静默
		}
		var doc struct {
			List []srtM3u8Vod `json:"list"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			continue
		}
		for _, it := range doc.List {
			if it.VodID == 0 || seen[it.VodID] {
				continue
			}
			hit := srtM3u8Hit(it, kw, base)
			if !hit {
				continue
			}
			seen[it.VodID] = true
			// 详情取完整分集 m3u8
			detail := srtM3u8Detail(ctx, base, it.VodID)
			if detail.VodID == 0 {
				detail = it
			}
			plays := srtM3u8ParsePlayURL(detail.PlayFrom, detail.PlayURL)
			var links []SrtLink
			if d := strings.TrimSpace(detail.DownURL); d != "" && strings.HasPrefix(d, "http") {
				links = append(links, SrtLink{Kind: "m3u8", Name: "直链", URL: d})
			}
			title := strings.TrimSpace(detail.VodName)
			if title == "" {
				title = strings.TrimSpace(it.VodName)
			}
			if title == "" {
				continue
			}
			note := ""
			if len(plays) == 0 {
				note = "采集源未返回可下载分集（可能仅网盘）"
			}
			out = append(out, SrtItem{
				Source:      "m3u8采集",
				Title:       title,
				Episodes:    srtM3u8EpText(detail.Remarks, len(plays)),
				Date:        strings.TrimSpace(detail.Pubdate),
				Note:        note,
				Links:       links,
				Plays:       plays,
				PlaySource:  "m3u8-" + srtM3u8Host(base),
				PlayEpCount: len(plays),
			})
			if len(out) >= dlSourceMaxPerSource {
				return out, nil
			}
		}
	}
	return out, nil
}

// srtM3u8Hit 粗筛：标题含关键词或关键词含糊时命中；短剧站搜索已过滤，这里兜底
func srtM3u8Hit(it srtM3u8Vod, kw, base string) bool {
	t := strings.ToLower(it.VodName)
	k := strings.ToLower(strings.TrimSpace(kw))
	if t != "" && strings.Contains(t, k) {
		return true
	}
	// 采集接口对中文关键词不过滤时可能返回全库，此时只保留标题含关键词的
	return srtHasCJK(kw) && strings.Contains(it.VodName, kw)
}
