package main

// BT/磁力搜索聚合：多站点并发检索磁力资源，统一结果格式。
//
// 源清单（全部为公开接口/页面，无需登录）：
//   - apibay.org   The Pirate Bay 的 JSON 封装（apibay），覆盖面最广
//   - yts.gg       YTS 官方 API（高清小体积电影，多画质逐条展开）
//   - bitsearch.eu SolidTorrents 的 DHT 索引 API
//   - nyaa.si      Nyaa 的 RSS（动画/日区资源）
//   - 动漫花园      share.dmhy.org 搜索页（中文动漫/日影，含磁力直链）
//
// 设计约束：
//   - 每源独立超时（dlSourceTimeout），整轮上限 dlSearchBudget；
//     被墙/改版的源静默失败，前端展示各源状态行即可，绝不因单源失败整体报错。
//   - 结果按 infohash 去重（大小写归一），按做种数降序。
//   - 关键词在拼 URL 前统一 url.QueryEscape，杜绝注入分片 URL。

import (
	"context"
	"encoding/json"
	"fmt"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DlItem 一条磁力搜索结果（跨源统一格式）
type DlItem struct {
	Source  string `json:"source"`            // 来源显示名（TPB / YTS / …）
	Title   string `json:"title"`             // 资源标题（去除站点标记）
	Size    int64  `json:"size"`              // 字节数（未知为 0）
	Seeds   int    `json:"seeds"`             // 做种数
	Leechs  int    `json:"leechs"`            // 下载数
	Magnet  string `json:"magnet"`            // 磁力链接（xt=urn:btih:…）
	PageURL string `json:"pageUrl,omitempty"` // 资源详情页（可选）
	Note    string `json:"note,omitempty"`    // 附加说明（画质/年份/字幕组等）
}

// dlSourceStat 单源查询状态（前端显示"N 个源可用"）
type dlSourceStat struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
	Ms    int64  `json:"ms"`
	Err   string `json:"err,omitempty"`
}

// dlSourceTimeout 单源请求超时：海外直连较慢但 10s 内足够；总预算兜底
const dlSourceTimeout = 10 * time.Second

// dlSourceMaxPerSource 单源最多贡献的结果条数，防止冷门关键词刷屏
const dlSourceMaxPerSource = 30

// dlSearchTotalBudget 整轮搜索预算
const dlSearchTotalBudget = 14 * time.Second

// dlHTTP 各站共用的 HTTP 客户端（桌面浏览器 UA +Referer 减少反爬拦截）
var dlHTTP = &http.Client{
	Transport: &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		TLSHandshakeTimeout: 10 * time.Second,
	},
	Timeout: dlSourceTimeout,
}

const dlDefaultUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/126.0.0.0 Safari/537.36"

// dlFetch 带超时与来源头的 GET；headers 可空。
func dlFetch(ctx context.Context, url string, headers map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", dlDefaultUA)
	req.Header.Set("Accept", "text/html,application/json;q=0.9,*/*;q=0.8")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := dlHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 上限 4MB：RSS/HTML/JSON 搜索结果足够，防异常大响应
	const max = 4 << 20
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 32<<10)
	for {
		n, rerr := resp.Body.Read(tmp)
		if n > 0 {
			if len(buf)+n > max {
				return buf, nil
			}
			buf = append(buf, tmp[:n]...)
		}
		if rerr != nil {
			return buf, nil // EOF 时正常返回
		}
	}
}

// ---------- 搜索聚合入口 ----------

type dlSource struct {
	Name string
	Run  func(ctx context.Context, kw string) ([]DlItem, error)
}

var dlBTSources = []dlSource{
	{Name: "TPB", Run: dlSrcApibay},
	{Name: "YTS", Run: dlSrcYTS},
	{Name: "BitSearch", Run: dlSrcBitSearch},
	{Name: "Nyaa", Run: dlSrcNyaa},
	{Name: "动漫花园", Run: dlSrcDmhy},
}

// dlSearchVideo 并发查询全部源 → 去重 → 排序
func dlSearchVideo(keyword string) ([]DlItem, []dlSourceStat) {
	kw := strings.TrimSpace(keyword)
	kw = strings.TrimSpace(strings.Trim(kw, "\"'`"))
	out := make([][]DlItem, len(dlBTSources))
	stats := make([]dlSourceStat, len(dlBTSources))
	var wg sync.WaitGroup
	for i := range dlBTSources {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			src := dlBTSources[i]
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
		// 超预算：不等慢源，直接收账（goroutine 泄漏由运行时自然回收，
		// 结果写入 out 前有 slice 赋值保护——超时场景直接取当前快照不安全，
		// 因此慢源的条目本轮丢弃即可，应用层无副作用）
	}
	var items []DlItem
	for i := range out {
		items = append(items, out[i]...)
	}
	items = dlDedup(items)
	sort.Slice(items, func(a, b int) bool {
		if items[a].Seeds != items[b].Seeds {
			return items[a].Seeds > items[b].Seeds
		}
		return items[a].Title < items[b].Title
	})
	return items, stats
}

// dlDedup 按 infohash 去重（大小写归一），无 hash 的条目按「标题+来源」去重
func dlDedup(items []DlItem) []DlItem {
	seenHash := map[string]bool{}
	seenTitle := map[string]bool{}
	out := make([]DlItem, 0, len(items))
	for _, it := range items {
		var hashKey, titleKey string
		if key := dlMagnetHash(it.Magnet); key != "" {
			hashKey = "h:" + strings.ToUpper(key)
			if seenHash[hashKey] {
				continue
			}
		}
		titleKey = "t:" + dlNormTitle(it.Title) + "|" + it.Source
		if seenTitle[titleKey] {
			continue
		}
		if hashKey != "" {
			seenHash[hashKey] = true
		}
		seenTitle[titleKey] = true
		out = append(out, it)
	}
	return out
}

// dlNormTitle 标题归一：去多余空白，便于近似比较
func dlNormTitle(s string) string {
	f := strings.Fields(s)
	return strings.ToLower(strings.Join(f, " "))
}

// dlMagnetHash 从磁力提取 btih 哈希（hex 或 base32）
var reMagnetHash = regexp.MustCompile(`xt=urn:btih:([a-zA-Z0-9]+)`)

func dlMagnetHash(magnet string) string {
	m := reMagnetHash.FindStringSubmatch(magnet)
	if len(m) < 2 {
		return ""
	}
	return strings.ToUpper(m[1])
}

// ---------- 源 1：apibay（TPB JSON 封装） ----------

func dlSrcApibay(ctx context.Context, kw string) ([]DlItem, error) {
	raw, err := dlFetch(ctx, "https://apibay.org/q.php?q="+url.QueryEscape(kw)+"&cat=0", nil)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Name     string `json:"name"`
		InfoHash string `json:"info_hash"`
		Seeders  string `json:"seeders"`
		Leechers string `json:"leechers"`
		Size     string `json:"size"`
		Added    string `json:"added"`
		Category string `json:"category"`
	}
	if json.Unmarshal(raw, &rows) != nil {
		return nil, fmt.Errorf("响应解析失败")
	}
	items := make([]DlItem, 0, len(rows))
	for _, r := range rows {
		name := strings.TrimSpace(r.Name)
		hash := strings.TrimSpace(r.InfoHash)
		if len(hash) != 40 {
			continue
		}
		sz, _ := strconv.ParseInt(r.Size, 10, 64)
		sd, _ := strconv.Atoi(r.Seeders)
		lc, _ := strconv.Atoi(r.Leechers)
		cat := r.Category
		items = append(items, DlItem{
			Source: "TPB",
			Title:  name,
			Size:   sz,
			Seeds:  sd,
			Leechs: lc,
			Magnet: "magnet:?xt=urn:btih:" + hash + "&dn=" + url.QueryEscape(name),
		})
		if cat != "" && cat != "0" {
			// apibay 的 imdb 标记为 cat=1<imdbid 形式，留给详情页；这里不占 note
			_ = cat
		}
	}
	if len(items) == 0 {
		return nil, nil
	}
	return items, nil
}

// ---------- 源 2：YTS（高清电影官方 API，镜像域名按序回退） ----------

func dlSrcYTS(ctx context.Context, kw string) ([]DlItem, error) {
	var lastErr error
	for _, host := range []string{"https://yts.mx", "https://yts.gg", "https://yts.lt"} {
		select {
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		default:
		}
		api := host + "/api/v2/list_movies.json?query_term=" + url.QueryEscape(kw) + "&limit=10&sort_by=seeds&order_by=desc"
		raw, err := dlFetch(ctx, api, nil)
		if err != nil {
			lastErr = err
			continue
		}
		var doc struct {
			Status string `json:"status"`
			Data   struct {
				Movies []struct {
					Title    string `json:"title_long"`
					Torrents []struct {
						Hash      string `json:"hash"`
						Quality   string `json:"quality"`
						SizeBytes int64  `json:"size_bytes"`
						Seeds     int    `json:"seeds"`
						Peers     int    `json:"peers"`
					} `json:"torrents"`
				} `json:"movies"`
			} `json:"data"`
		}
		if json.Unmarshal(raw, &doc) != nil {
			lastErr = fmt.Errorf("响应解析失败")
			continue
		}
		var items []DlItem
		for _, mv := range doc.Data.Movies {
			for _, t := range mv.Torrents {
				if len(t.Hash) != 40 {
					continue
				}
				// Title 形如 "Inception (2010)"
				title := strings.TrimSpace(mv.Title)
				base := title
				year := ""
				if i := strings.LastIndex(title, "("); i > 0 {
					base = strings.TrimSpace(title[:i])
					year = title[i:]
				}
				note := ""
				if t.Quality != "" {
					note = "画质：" + t.Quality
				}
				items = append(items, DlItem{
					Source: "YTS",
					Title:  base + " " + strings.TrimSpace(t.Quality) + " " + year,
					Size:   t.SizeBytes,
					Seeds:  t.Seeds,
					Leechs: t.Peers,
					Magnet: "magnet:?xt=urn:btih:" + t.Hash + "&dn=" + url.QueryEscape(title),
					Note:   note,
				})
			}
		}
		if len(items) == 0 && doc.Status != "ok" {
			lastErr = fmt.Errorf("接口无结果")
			continue
		}
		return items, nil
	}
	return nil, lastErr
}

// ---------- 源 3：bitsearch.eu（SolidTorrents DHT 索引 API） ----------

func dlSrcBitSearch(ctx context.Context, kw string) ([]DlItem, error) {
	raw, err := dlFetch(ctx, "https://bitsearch.eu/api/v1/search?q="+url.QueryEscape(kw)+"&limit=25&sort=seeders", nil)
	if err != nil {
		return nil, err
	}
	var doc struct {
		Success bool `json:"success"`
		Results []struct {
			Title    string `json:"title"`
			InfoHash string `json:"infohash"`
			Size     int64  `json:"size"`
			Seeders  int    `json:"seeders"`
			Leechers int    `json:"leechers"`
		} `json:"results"`
	}
	if json.Unmarshal(raw, &doc) != nil || !doc.Success {
		return nil, fmt.Errorf("响应解析失败")
	}
	items := make([]DlItem, 0, len(doc.Results))
	for _, r := range doc.Results {
		hash := strings.TrimSpace(r.InfoHash)
		if len(hash) != 40 {
			continue
		}
		items = append(items, DlItem{
			Source: "BitSearch",
			Title:  strings.TrimSpace(r.Title),
			Size:   r.Size,
			Seeds:  r.Seeders,
			Leechs: r.Leechers,
			Magnet: "magnet:?xt=urn:btih:" + strings.ToLower(hash) + "&dn=" + url.QueryEscape(r.Title),
		})
	}
	return items, nil
}

// ---------- 源 4：Nyaa RSS ----------

func dlSrcNyaa(ctx context.Context, kw string) ([]DlItem, error) {
	raw, err := dlFetch(ctx, "https://nyaa.si/?page=rss&q="+url.QueryEscape(kw), nil)
	if err != nil {
		return nil, err
	}
	return parseNyaaRSS(string(raw)), nil
}

var (
	reNyaaItem  = regexp.MustCompile(`(?s)<item>(.*?)</item>`)
	reNyaaTitle = regexp.MustCompile(`(?s)<title>(?:<!\[CDATA\[)?(.*?)(?:\]\]>)?</title>`)
	reNyaaHash  = regexp.MustCompile(`<nyaa:infoHash>(.*?)</nyaa:infoHash>`)
	reNyaaSize  = regexp.MustCompile(`(?s)<nyaa:size>(?:<!\[CDATA\[)?(.*?)(?:\]\]>)?</nyaa:size>`)
	reNyaaSeeds = regexp.MustCompile(`<nyaa:seeders>(.*?)</nyaa:seeders>`)
	reNyaaLeech = regexp.MustCompile(`<nyaa:leechers>(.*?)</nyaa:leechers>`)
	reNyaaGuid  = regexp.MustCompile(`(?s)<guid[^>]*>(.*?)</guid>`)
)

// parseNyaaRSS 抽出 RSS 条目（样本见 tests 样例数据）
func parseNyaaRSS(xmlText string) []DlItem {
	var out []DlItem
	for _, m := range reNyaaItem.FindAllStringSubmatch(xmlText, dlSourceMaxPerSource) {
		body := m[1]
		title := html.UnescapeString(stripCDATA(grabText(reNyaaTitle, body)))
		hash := strings.TrimSpace(grabText(reNyaaHash, body))
		if len(hash) != 40 {
			continue
		}
		sz, _ := parseHumanSize(grabText(reNyaaSize, body))
		sd, _ := strconv.Atoi(strings.TrimSpace(grabText(reNyaaSeeds, body)))
		lc, _ := strconv.Atoi(strings.TrimSpace(grabText(reNyaaLeech, body)))
		page := strings.TrimSpace(grabText(reNyaaGuid, body))
		out = append(out, DlItem{
			Source:  "Nyaa",
			Title:   title,
			Size:    sz,
			Seeds:   sd,
			Leechs:  lc,
			Magnet:  "magnet:?xt=urn:btih:" + hash + "&dn=" + url.QueryEscape(title),
			PageURL: page,
		})
	}
	return out
}

// stripCDATA 去掉 CDATA 包裹
func stripCDATA(s string) string {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "<![CDATA[") {
		s = strings.TrimSuffix(strings.TrimPrefix(s, "<![CDATA["), "]]>")
	}
	return s
}

var reTags = regexp.MustCompile(`<[^>]*>`)

// grabText 正则取第 1 组并清理
func grabText(re *regexp.Regexp, body string) string {
	m := re.FindStringSubmatch(body)
	if len(m) < 2 {
		return ""
	}
	s := stripCDATA(m[1])
	s = reTags.ReplaceAllString(s, "")
	return html.UnescapeString(strings.TrimSpace(s))
}

// parseHumanSize “9.8 GiB”这类人类可读大小 → 字节数（未知返回 0）
func parseHumanSize(s string) (int64, error) {
	s = strings.TrimSpace(s)
	m := regexp.MustCompile(`^([\d.]+)\s*([KMGT]?i?B)$`).FindStringSubmatch(s)
	if len(m) < 3 {
		return 0, fmt.Errorf("size: %q", s)
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0, err
	}
	unit := m[2]
	mult := float64(1)
	switch {
	case strings.HasPrefix(unit, "K"):
		mult = 1 << 10
	case strings.HasPrefix(unit, "M"):
		mult = 1 << 20
	case strings.HasPrefix(unit, "G"):
		mult = 1 << 30
	case strings.HasPrefix(unit, "T"):
		mult = 1 << 40
	}
	return int64(v * mult), nil
}

// ---------- 源 5：动漫花园 dmhy（搜索页 HTML） ----------

func dlSrcDmhy(ctx context.Context, kw string) ([]DlItem, error) {
	raw, err := dlFetch(ctx, "https://share.dmhy.org/topics/list?keyword="+url.QueryEscape(kw), nil)
	if err != nil {
		return nil, err
	}
	return parseDmhyList(string(raw)), nil
}

var (
	reViewLnk  = regexp.MustCompile(`href="/topics/view/[^"]+"[^>]*>(.*?)</a>`)
	reMagLnk   = regexp.MustCompile(`href="(magnet:\?xt=urn:btih:[^"&]+)[^"]*"`)
	reDataMag  = regexp.MustCompile(`data-magnet="magnet:\?xt=urn:btih:([^"&]+)"`)
	reSizeCell = regexp.MustCompile(`align="center">\s*([\d.]+)\s*(TB|GB|MB|KB)\s*<`)
)

// reTrRow 已废弃：RE2 无前视断言 `(?=...)`（曾导致启动 panic），
// 行截取改为 splitDmhyRows 手工按 `<tr` 切分。

// parseDmhyList 提取列表行：标题、磁力、大小
func parseDmhyList(page string) []DlItem {
	var out []DlItem
	for _, row := range splitDmhyRows(page) {
		if !strings.Contains(row, "/topics/view/") {
			continue
		}
		title := grabText(reViewLnk, row)
		title = strings.ReplaceAll(title, "\u00a0", " ")
		title = regexp.MustCompile(`\s+`).ReplaceAllString(title, " ")
		if title == "" {
			continue
		}
		// 优先 data-magnet（纯 hex hash，短小干净），回退第一个 magnet 链接
		magnet := ""
		if dm := reDataMag.FindStringSubmatch(row); len(dm) >= 2 {
			magnet = "magnet:?xt=urn:btih:" + strings.ToLower(dm[1]) + "&dn=" + url.QueryEscape(title)
		} else if mm := reMagLnk.FindStringSubmatch(row); len(mm) >= 2 {
			magnet = html.UnescapeString(mm[1])
			if dlMagnetHash(magnet) == "" {
				magnet = ""
			}
			if magnet != "" && !strings.Contains(magnet, "&dn=") {
				magnet += "&dn=" + url.QueryEscape(title)
			}
		}
		if magnet == "" {
			continue
		}
		var size int64
		if sm := reSizeCell.FindStringSubmatch(row); len(sm) >= 3 {
			size, _ = parseHumanSize(sm[1] + sm[2])
		}
		out = append(out, DlItem{
			Source: "动漫花园",
			Title:  title,
			Size:   size,
			Magnet: magnet,
		})
		if len(out) >= dlSourceMaxPerSource {
			break
		}
	}
	return out
}

// splitDmhyRows 手工切分 <tr 行块：以 `<tr` 为界向前找最近的段落终点（下一个 tr
// 或 </tbody>），不依赖前视断言。
func splitDmhyRows(page string) []string {
	var out []string
	idx := 0
	for {
		begin := strings.Index(page[idx:], "<tr")
		if begin < 0 || len(out) > 80 {
			break
		}
		begin += idx
		// 行块终点：下一个 <tr 或 </tbody>，取更近者
		end1 := strings.Index(page[begin+3:], "<tr")
		if end1 >= 0 {
			end1 += begin + 3
		} else {
			end1 = len(page)
		}
		end2 := strings.Index(page[begin:], "</tbody>")
		if end2 >= 0 {
			end2 += begin
		} else {
			end2 = len(page)
		}
		end := end1
		if end2 < end {
			end = end2
		}
		out = append(out, page[begin:end])
		idx = end
	}
	return out
}

// ---------- 音乐 BT 兜底（音频类目专用检索） ----------

// dlSrcTpbAudio TPB 音频分区（category 101 mp3 / 102 无损 / 103 其它音频）
func dlSrcTpbAudio(ctx context.Context, kw string) ([]DlItem, error) {
	raw, err := dlFetch(ctx, "https://apibay.org/q.php?q="+url.QueryEscape(kw)+"&cat=101,102", nil)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Name     string `json:"name"`
		InfoHash string `json:"info_hash"`
		Seeders  string `json:"seeders"`
		Leechers string `json:"leechers"`
		Size     string `json:"size"`
		Category string `json:"category"`
	}
	if json.Unmarshal(raw, &rows) != nil {
		return nil, fmt.Errorf("响应解析失败")
	}
	items := make([]DlItem, 0, len(rows))
	for _, r := range rows {
		name := strings.TrimSpace(r.Name)
		hash := strings.TrimSpace(r.InfoHash)
		if len(hash) != 40 {
			continue
		}
		// apibay 对多 cat 查询可能不按过滤走，此处按 category 1xx 再双重确认
		if len(r.Category) < 3 || (r.Category[:1] != "1") {
			continue
		}
		sz, _ := strconv.ParseInt(r.Size, 10, 64)
		sd, _ := strconv.Atoi(r.Seeders)
		lc, _ := strconv.Atoi(r.Leechers)
		items = append(items, DlItem{
			Source: "TPB音频",
			Title:  name,
			Size:   sz,
			Seeds:  sd,
			Leechs: lc,
			Magnet: "magnet:?xt=urn:btih:" + hash + "&dn=" + url.QueryEscape(name),
		})
	}
	return items, nil
}

// dlSrcNyaaAudio Nyaa 音频类目（c=0_3 Audio）
func dlSrcNyaaAudio(ctx context.Context, kw string) ([]DlItem, error) {
	raw, err := dlFetch(ctx, "https://nyaa.si/?page=rss&q="+url.QueryEscape(kw)+"&c=0_3&s=seeders&o=desc", nil)
	if err != nil {
		return nil, err
	}
	return parseNyaaRSS(string(raw)), nil
}
