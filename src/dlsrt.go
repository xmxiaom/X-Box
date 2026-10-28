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
	"strings"
	"sync"
	"time"
)

// SrtLink 一条网盘分享链接
type SrtLink struct {
	Kind string `json:"kind"`           // 网盘类型：quark / baidu / aliyun / 139 / lanzou …
	Name string `json:"name,omitempty"` // 识别到的网盘名（如 夸克网盘）
	URL  string `json:"url"`            // 完整分享 URL（可打开/复制）
}

// SrtItem 一条短剧搜索结果
type SrtItem struct {
	Source    string    `json:"source"`              // 来源显示名（黑豆 / rejusou）
	Title     string    `json:"title"`               // 剧名（去站点标记）
	Episodes  string    `json:"episodes,omitempty"`  // 集数文本（如 76集）
	Date      string    `json:"date,omitempty"`      // 更新/发布日（YYYY-MM-DD，可空）
	DetailURL string    `json:"detailUrl,omitempty"` // 站内详情页（可空）
	Links     []SrtLink `json:"links"`               // 网盘链接（≥1）
	Note      string    `json:"note,omitempty"`      // 附加说明
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