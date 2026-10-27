package main

// 音乐下载：网易云曲库搜索 + 免费曲目直下到本地目录。
//
// 来源与限制（2026 实测）：
//   - 搜索：https://music.163.com/api/search/get/web（网页端公开接口，
//     普通搜索无需登录）；返回 fee 字段标识歌曲类型：
//     0=免费  8=低音质免费  1=VIP  4=购买专辑  16=需登录
//   - 下载：/song/media/outer/url?id= 外链接口，对免费曲返回 mp3，
//     对 VIP/版权曲 302 到 /404——据此判定「VIP 或无版权，无法直下」。
//
// 安全与可逆：
//   - 只写 msc 音乐文件到配置目录，重名走 uniqueFileSize 风格后缀不覆盖；
//   - 并发任务数限_dlMusConcMax（防止 NAS 带宽被机器占用）；
//   - 支持取消（ctx cancel），任务记录保留最近 40 条。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// DlMusicItem 一首搜索结果
type DlMusicItem struct {
	ID      int64  `json:"id"`
	Name    string `json:"name"`
	Artist  string `json:"artist"`
	Album   string `json:"album"`
	DurMS   int    `json:"durMs,omitempty"`
	Fee     int    `json:"fee"`
	CanDL   bool   `json:"canDL"`
	FeeText string `json:"feeText"`
}

// dlMusFeeText fee → 展示文案
func dlMusFeeText(fee int) (canDL bool, text string) {
	switch fee {
	case 0:
		return true, "免费"
	case 8:
		return true, "免费（低音质）"
	case 1:
		return false, "VIP 专享"
	case 4:
		return false, "购买专辑"
	case 16:
		return false, "需登录"
	default:
		return false, "受限"
	}
}

// dlMusSearch 网易云网页搜索（type=1 歌曲），最多 20 条/页
func dlMusSearch(ctx context.Context, kw string, page int) ([]DlMusicItem, int, error) {
	offset := (page - 1) * 20
	if offset < 0 {
		offset = 0
	}
	api := "https://music.163.com/api/search/get/web?csrf_token=&scar_token=&s=" + url.QueryEscape(kw) +
		"&type=1&offset=" + strconv.Itoa(offset) + "&total=true&limit=20"
	req, err := http.NewRequestWithContext(ctx, "GET", api, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("User-Agent", dlDefaultUA)
	req.Header.Set("Referer", "https://music.163.com/")
	resp, err := dlHTTP.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil {
		return nil, 0, err
	}
	var doc struct {
		Code   int `json:"code"`
		Result struct {
			Songs []struct {
				ID       int64  `json:"id"`
				Name     string `json:"name"`
				Duration int    `json:"duration"`
				Fee      int    `json:"fee"`
				Artists  []struct {
					Name string `json:"name"`
				} `json:"artists"`
				Album struct {
					Name string `json:"name"`
				} `json:"album"`
			} `json:"songs"`
			SongCount int `json:"songCount"`
		} `json:"result"`
	}
	if json.Unmarshal(b, &doc) != nil {
		return nil, 0, fmt.Errorf("响应解析失败")
	}
	if doc.Code != 200 {
		return nil, 0, fmt.Errorf("接口返回 %d", doc.Code)
	}
	out := make([]DlMusicItem, 0, len(doc.Result.Songs))
	for _, s := range doc.Result.Songs {
		var artists []string
		for _, a := range s.Artists {
			if n := strings.TrimSpace(a.Name); n != "" {
				artists = append(artists, n)
			}
		}
		can, text := dlMusFeeText(s.Fee)
		out = append(out, DlMusicItem{
			ID:      s.ID,
			Name:    s.Name,
			Artist:  strings.Join(artists, "、"),
			Album:   s.Album.Name,
			DurMS:   s.Duration,
			Fee:     s.Fee,
			CanDL:   can,
			FeeText: text,
		})
	}
	return out, doc.Result.SongCount, nil
}

// ---------- 酷我有声源（r.s 老搜索 + antiserver 直链，免登录） ----------

// DlKwItem 酷我搜索结果
type DlKwItem struct {
	Rid    string `json:"rid"` // 例 MUSIC_288331715（antiserver 直链用）
	Name   string `json:"name"`
	Artist string `json:"artist"`
	Album  string `json:"album,omitempty"`
	DurMS  int    `json:"durMs,omitempty"`
	Format string `json:"format,omitempty"` // MINFO 首项（音质描述）
}

// dlKwRowRe 单条酷我行内字段（r.s 返回单引号伪 JSON，不能直接 Unmarshal）
var dlKwRowRe = map[string]*regexp.Regexp{
	"NAME":     regexp.MustCompile(`'NAME':'([^']*)'`),
	"SONGNAME": regexp.MustCompile(`'SONGNAME':'([^']*)'`),
	"ARTIST":   regexp.MustCompile(`'ARTIST':'([^']*)'`),
	"ALBUM":    regexp.MustCompile(`'ALBUMNAME':'([^']*)'`),
	"MUSICRID": regexp.MustCompile(`'MUSICRID':('(MUSIC_\d+)'|MUSIC_(\d+))`),
	"DURATION": regexp.MustCompile(`'DURATION':'?(\d+)`),
	"MINFO":    regexp.MustCompile(`'MINFO':'([^']*)'`),
}

// dlKwEscRe r.s 转义的 & ：反斜杠数量不定（1-4 个）+ u0026，一律归一为 &
var dlKwEscRe = regexp.MustCompile(`\\+u0026`)

// dlKwUnesc r.s 值清洗：\u0026（任意反斜杠数）→ &，&nbsp; → 空格，再压空白
func dlKwUnesc(s string) string {
	s = dlKwEscRe.ReplaceAllString(s, "&")
	s = strings.ReplaceAll(s, "&nbsp;", " ")
	s = strings.TrimSpace(s)
	return strings.Join(strings.Fields(s), " ")
}

// dlKwSearch 酷我 r.s 搜索（免 csrf 老 web 接口；此库存曲含大量免歌并带完整 MINFO）
func dlKwSearch(ctx context.Context, kw string, page int) ([]DlKwItem, error) {
	rn := 15
	pn := (page - 1) * rn
	if pn < 0 {
		pn = 0
	}
	api := "https://search.kuwo.cn/r.s?all=" + url.QueryEscape(kw) +
		"&ft=music&rformat=json&encoding=utf8&pn=" + strconv.Itoa(pn) + "&rn=" + strconv.Itoa(rn)
	req, err := http.NewRequestWithContext(ctx, "GET", api, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", dlDefaultUA)
	resp, err := dlHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if err != nil || len(b) == 0 {
		return nil, fmt.Errorf("响应为空")
	}
	raw := string(b)
	if !strings.Contains(raw, "abslist") {
		return nil, fmt.Errorf("搜索被酷我拒绝（无结果字段）")
	}
	return dlKwParseRows(raw), nil
}

// dlKwParseRows 从 r.s 单引号伪 JSON 里切出歌曲行并解析（独立出来便于测试）
func dlKwParseRows(raw string) []DlKwItem {
	// 行切分：以 '{'AARTIST 为锚（伪 JSON 单引号，值内不含此序列）
	rows := strings.Split(raw, "{'AARTIST'")
	out := make([]DlKwItem, 0, len(rows))
	for _, row := range rows[1:] {
		row = "{'AARTIST" + row
		name := ""
		if m := dlKwRowRe["SONGNAME"].FindStringSubmatch(row); m != nil {
			name = dlKwUnesc(m[1])
		} else if m := dlKwRowRe["NAME"].FindStringSubmatch(row); m != nil {
			name = dlKwUnesc(m[1])
		}
		if name == "" {
			continue
		}
		artist := ""
		if m := dlKwRowRe["ARTIST"].FindStringSubmatch(row); m != nil {
			artist = dlKwUnesc(m[1])
		}
		rid := ""
		m := dlKwRowRe["MUSICRID"].FindStringSubmatch(row)
		if m == nil {
			continue
		}
		rid = m[2]
		if rid == "" {
			rid = "MUSIC_" + m[3]
		}
		it := DlKwItem{Rid: rid, Name: name, Artist: artist}
		if mm := dlKwRowRe["ALBUM"].FindStringSubmatch(row); mm != nil {
			it.Album = dlKwUnesc(mm[1])
		}
		if mm := dlKwRowRe["DURATION"].FindStringSubmatch(row); mm != nil {
			it.DurMS, _ = strconv.Atoi(mm[1]) // 该库 DURATION 是秒
			it.DurMS *= 1000
		}
		if mm := dlKwRowRe["MINFO"].FindStringSubmatch(row); mm != nil {
			it.Format = dlKwUnesc(mm[1])
		}
		out = append(out, it)
	}
	return out
}

// dlKwResolveClient 测试可注入的 antiserver 客户端；nil 时现场构造（默认拒绝自动重定向）
var dlKwResolveClient *http.Client

// dlKwResolveLink antiserver 老直链接口：可能 302 → 真 mp3 地址，也可能直接回音频体。
// 返回可直接 GET 的最终地址。
func dlKwResolveLink(ctx context.Context, rid string) (string, error) {
	api := "http://antiserver.kuwo.cn/anti.s?type=convert_url&format=mp3&response=res&rid=" + rid
	req, err := http.NewRequestWithContext(ctx, "GET", api, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", dlDefaultUA)
	req.Header.Set("Referer", "https://kuwo.cn/")
	client := dlKwResolveClient
	if client == nil {
		client = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(r *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		}}
	}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	// 302：Location 即直链（302 目标在 kuwo 资源域上，属预期）
	if loc := resp.Header.Get("Location"); loc != "" {
		low := strings.ToLower(loc)
		if strings.HasSuffix(low, ".mp3") || strings.Contains(low, "kuwo.cn") {
			return loc, nil
		}
	}
	// 非 302：接口本身直接回音频体
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "audio/") || strings.Contains(ct, "octet-stream") {
		return api, nil
	}
	if s := resp.StatusCode; s != 200 {
		return "", fmt.Errorf("HTTP %d", s)
	}
	return "", fmt.Errorf("未返回可用直链")
}

// ---------- 下载任务 ----------

// DlMusicTask 单首歌曲下载任务
type DlMusicTask struct {
	ID     string    `json:"id"`
	Song   string    `json:"song"`
	Artist string    `json:"artist"`
	Source string    `json:"source,omitempty"` // netease / kuwo（空默认 netease）
	Path   string    `json:"path,omitempty"`
	Status string    `json:"status"` // running / paused / ok / error / cancel
	Bytes  int64     `json:"bytes"`
	Totals int64     `json:"total"`
	Err    string    `json:"err,omitempty"`
	At     time.Time `json:"at"`
	Dir    string    `json:"dir"`

	// 暂停/续传（v1.3.43）：暂停保留 .part 与进度，继续时用 HTTP Range 断点续传
	paused bool
	// 续传上下文（仅内存，不出 JSON）
	songID int64
	kwItem DlKwItem
	gen    int // 轮次代号：暂停/续传多次时丢弃过期轮次的收尾写入
}

var dlMusStore struct {
	mu    sync.Mutex
	tasks map[string]*DlMusicTask
	canc  map[string]context.CancelFunc
	sem   chan struct{}
	order []string
	once  sync.Once
}

func dlMusInit() {
	dlMusStore.once.Do(func() {
		dlMusStore.tasks = map[string]*DlMusicTask{}
		dlMusStore.canc = map[string]context.CancelFunc{}
		dlMusStore.sem = make(chan struct{}, 3) // 最多 3 并发下载
	})
}

// dlMusMusicDir 解析音乐下载目录：配置 → 缺省建议
func dlMusMusicDir(cfg DlConfig) string {
	if d := strings.TrimSpace(cfg.MusicDir); d != "" {
		return d
	}
	return dlDefaultDir("music")
}

// dlMusFileClean 文件名清洗（保留中英数与常用符号，其余空白下划线化）
func dlMusFileClean(name string) string {
	name = strings.TrimSpace(name)
	var b strings.Builder
	for _, r := range name {
		switch r {
		case '/', '\\', ':', '*', '?', '"', '<', '>', '|':
			b.WriteRune(' ')
		default:
			if r == 0x1b || r == '\n' || r == '\r' || r == '\t' {
				continue
			}
			b.WriteRune(r)
		}
	}
	out := strings.TrimSpace(b.String())
	out = strings.Join(strings.Fields(out), " ")
	if out == "" {
		out = "无标题"
	}
	// 限长（中文文件名超长会打不开）
	runes := []rune(out)
	if len(runes) > 80 {
		out = string(runes[:80])
	}
	return out
}

// dlMusUniquePath 目录下重名去重：a.mp3 → a (1).mp3 …
func dlMusUniquePath(dir, name string) string {
	p := filepath.Join(dir, name)
	if _, err := os.Stat(p); err == nil {
		ext := filepath.Ext(name)
		base := strings.TrimSuffix(name, ext)
		for i := 1; ; i++ {
			np := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, i, ext))
			if _, err := os.Stat(np); err != nil {
				return np
			}
			if i > 9999 {
				return filepath.Join(dir, fmt.Sprintf("%s-%d%s", base, time.Now().UnixNano(), ext))
			}
		}
	}
	return p
}

// dlMusReq 一次下载请求（source=netease 用 SongID；source=kuwo 用 Kw.Rid）
type dlMusReq struct {
	Source string    `json:"source"` // netease / kuwo
	SongID int64     `json:"id"`
	Kw     DlKwItem  `json:"-"`
	Name   string    `json:"name"`
	Artist string    `json:"artist"`
}

// dlMusStart 创建下载任务
func dlMusStart(cfg DlConfig, req dlMusReq) (*DlMusicTask, error) {
	dlMusInit()
	dir := dlMusMusicDir(cfg)
	if dir == "" {
		return nil, fmt.Errorf("尚未设置音乐下载目录")
	}
	if _, safe := safeDownloadDir(dir); !safe {
		return nil, fmt.Errorf("下载目录不可用：%s", dir)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("创建下载目录失败：%s", err.Error())
	}
	id := fmt.Sprintf("m%d%d", time.Now().UnixNano(), req.SongID)
	fname := dlMusFileClean(req.Artist+" - "+req.Name) + ".mp3"
	path := dlMusUniquePath(dir, fname)
	st := &DlMusicTask{
		ID:     id,
		Song:   req.Name,
		Artist: req.Artist,
		Source: req.Source,
		Path:   path,
		Status: "pending", // 交给 dlMusLaunch 置 running（若直接置 running，launch 会误判「已在跑」而不启动）
		Dir:    dir,
		At:     time.Now(),
		songID: req.SongID,
		kwItem: req.Kw,
	}
	dlMusStore.mu.Lock()
	dlMusStore.tasks[id] = st
	dlMusStore.mu.Unlock()
	dlMusLaunch(st)
	return st, nil
}

// dlMusLaunch 拉起一轮下载（占并发槽）；已在跑或已终态则忽略。
// 暂停后继续走同一入口 → 复用 .part 与已下载字节，实现断点续传。
func dlMusLaunch(st *DlMusicTask) {
	dlMusInit()
	dlMusStore.mu.Lock()
	if st.Status == "running" {
		dlMusStore.mu.Unlock()
		return
	}
	st.gen++
	gen := st.gen
	ctx, cancel := context.WithCancel(context.Background())
	dlMusStore.canc[st.ID] = cancel
	dlMusStore.tasks[st.ID] = st
	dlMusStore.mu.Unlock()

	st.paused = false
	st.Status = "running"
	st.Err = ""

	go func() {
		defer func() { recover() }()
		select {
		case dlMusStore.sem <- struct{}{}:
			defer func() { <-dlMusStore.sem }()
		case <-ctx.Done():
			return
		}
		if st.Source == "kuwo" {
			dlMusDoDownloadKuwo(ctx, st, st.kwItem, gen)
		} else {
			dlMusDoDownload(ctx, st, st.songID, gen)
		}
	}()
}

// dlMusPartOffset 已下载到 .part 的字节数（续传起点）
func dlMusPartOffset(st *DlMusicTask) int64 {
	fi, err := os.Stat(st.Path + ".part")
	if err != nil || fi.Size() <= 0 {
		return 0
	}
	return fi.Size()
}

// dlMusEndRound 轮次收尾：仅当代号匹配（未被新轮次顶替）时写回并清 cancel
func dlMusEndRound(st *DlMusicTask, gen int) {
	dlMusStore.mu.Lock()
	if st.gen == gen {
		delete(dlMusStore.canc, st.ID)
		dlMusStore.tasks[st.ID] = st
	}
	dlMusStore.mu.Unlock()
}

// dlMusDoDownload 执行下载：outer url → 302 跟踪（/404 即 VIP/无版权）→ 流式写文件
func dlMusDoDownload(ctx context.Context, st *DlMusicTask, songID int64, gen int) {
	defer dlMusRecoverPanic(st)
	defer dlMusEndRound(st, gen)
	finalize := func(status, errMsg string) {
		st.Status = status
		st.Err = errMsg
		st.At = time.Now()
	}
	url1 := "https://music.163.com/song/media/outer/url?id=" + strconv.FormatInt(songID, 10) + ".mp3"
	req, err := http.NewRequestWithContext(ctx, "GET", url1, nil)
	if err != nil {
		finalize("error", err.Error())
		return
	}
	req.Header.Set("User-Agent", dlDefaultUA)
	req.Header.Set("Referer", "https://music.163.com/")
	off := dlMusPartOffset(st)
	if off > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-")
	}
	// 不自动跟随：识别 302→/404（VIP/无版权）的明确信号；Transport borrow 自
	// dlHTTP（可注入打桩），重定向规则本任务自带
	client := &http.Client{
		Timeout:   10 * time.Minute,
		Transport: dlHTTPTransport(),
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if strings.Contains(r.URL.Path, "/404") {
				return fmt.Errorf(dlMusErrVIP404)
			}
			if len(via) >= 5 {
				return context.Cause(ctx)
			}
			return nil
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		if strings.Contains(err.Error(), dlMusErrVIP404) {
			finalize("error", "VIP/无版权曲目，客户端直连无法下载（会员需在官方客户端完成）")
		} else if ctx.Err() != nil {
			dlMusInterrupted(st, finalize)
		} else {
			finalize("error", "下载失败："+err.Error())
		}
		return
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		resp.Body.Close()
		finalize("error", fmt.Sprintf("HTTP %d", resp.StatusCode))
		return
	}
	if ct := resp.Header.Get("Content-Type"); strings.Contains(ct, "text/html") {
		resp.Body.Close()
		finalize("error", "服务端返回 HTML（非音频），跳过")
		return
	}
	// 206 说明续传被接受；200 说明服务端忽略 Range → 从头覆盖，起点归零
	if resp.StatusCode == 200 && off > 0 {
		off = 0
	}
	dlMusStreamBody(ctx, st, resp, finalize, off)
}

// dlMusDoDownloadKuwo 酷我下载：antiserver 解析直链 → 流式落盘（同样支持 Range 续传）
func dlMusDoDownloadKuwo(ctx context.Context, st *DlMusicTask, it DlKwItem, gen int) {
	defer dlMusRecoverPanic(st)
	defer dlMusEndRound(st, gen)
	finalize := func(status, errMsg string) {
		st.Status = status
		st.Err = errMsg
		st.At = time.Now()
	}
	link, err := dlKwResolveLink(ctx, it.Rid)
	if err != nil {
		if ctx.Err() != nil {
			dlMusInterrupted(st, finalize)
			return
		}
		finalize("error", "酷我直链解析失败："+err.Error())
		return
	}
	req, err := http.NewRequestWithContext(ctx, "GET", link, nil)
	if err != nil {
		finalize("error", err.Error())
		return
	}
	req.Header.Set("User-Agent", dlDefaultUA)
	req.Header.Set("Referer", "https://kuwo.cn/")
	off := dlMusPartOffset(st)
	if off > 0 {
		req.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-")
	}
	resp, err := dlHTTP.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			dlMusInterrupted(st, finalize)
		} else {
			finalize("error", "下载失败："+err.Error())
		}
		return
	}
	if resp.StatusCode != 200 && resp.StatusCode != 206 {
		resp.Body.Close()
		finalize("error", fmt.Sprintf("HTTP %d", resp.StatusCode))
		return
	}
	if resp.StatusCode == 200 && off > 0 {
		off = 0
	}
	dlMusStreamBody(ctx, st, resp, finalize, off)
}

// dlMusInterrupted 中断归因：用户暂停（保留 .part 续传）还是取消（清理 .part）
func dlMusInterrupted(st *DlMusicTask, finalize func(string, string)) {
	if st.paused {
		finalize("paused", "")
		return
	}
	os.Remove(st.Path + ".part")
	finalize("cancel", "已取消")
}

// dlMusFinalize 终态统一写回
func dlMusFinalize(st *DlMusicTask, status, errMsg string) {
	st.Status = status
	st.Err = errMsg
	st.At = time.Now()
}

// dlHTTPTransport 返回 dlHTTP 的 Transport（测试可换 dlHTTP 注入）
func dlHTTPTransport() http.RoundTripper {
	if dlHTTP == nil {
		return http.DefaultTransport
	}
	return dlHTTP.Transport
}

// dlMusRecoverPanic 下载 goroutine 兜底恢复：任何 panic 只标记任务失败，
// 不许把整个进程炸掉（之前 CheckRedirect nil body panic 就是前车之鉴）。
func dlMusRecoverPanic(st *DlMusicTask) {
	if r := recover(); r != nil {
		dlMusStore.mu.Lock()
		st.Status = "error"
		st.Err = "下载器内部错误：" + fmt.Sprint(r) + "\n" + string(debug.Stack())
		st.At = time.Now()
		dlMusStore.tasks[st.ID] = st
		dlMusStore.mu.Unlock()
	}
}

// dlMusStreamBody 音频体流式落盘（mp3 头校验 / 空体拦截 / 取消 / 暂停续传）
// off>0 表示续传：.part 以追加模式打开，跳过 mp3 头校验（头部已校验过）。
func dlMusStreamBody(ctx context.Context, st *DlMusicTask, resp *http.Response, finalize func(string, string), off int64) {
	defer resp.Body.Close()
	// 简单格式保险：Content-Length 立刻为 0 / 或内容不是 mp3 头，判定失真
	if resp.ContentLength == 0 && off == 0 {
		finalize("error", "响应为空（可能 VIP/无版权，请换一首试试）")
		return
	}
	if off == 0 {
		st.Totals = resp.ContentLength
	}
	st.Bytes = off
	tmp := st.Path + ".part"
	flags := os.O_CREATE | os.O_WRONLY
	if off > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(tmp, flags, 0o644)
	if err != nil {
		finalize("error", "创建文件失败："+err.Error())
		return
	}
	head := make([]byte, 0, 16)
	buf := make([]byte, 32<<10)
	firstChunk := off == 0 // 续传时不再校验头
	for {
		select {
		case <-ctx.Done():
			f.Close()
			// 暂停保留 .part 供续传；取消才清理
			if st.paused {
				finalize("paused", "")
			} else {
				os.Remove(tmp)
				finalize("cancel", "已取消")
			}
			return
		default:
		}
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			if firstChunk {
				head = append(head, buf[:min(n, 4)]...)
				firstChunk = false
				// mp3 起始：ID3 头，或 0xFF 开头的 MPEG 帧同步；两者都不是
				// 说明响应体不是音频（多半是 VIP 提示 HTML，前面 CT 判断没拦住）
				isMP3 := (head[0] == 'I' && head[1] == 'D' && head[2] == '3') ||
					head[0] == 0xFF || head[0] == 0xFE
				if !isMP3 {
					finalize("error", "返回内容不是音频（可能 VIP/无版权）")
					f.Close()
					return
				}
			}
			if _, werr := f.Write(buf[:n]); werr != nil {
				f.Close()
				finalize("error", "写文件失败："+werr.Error())
				return
			}
			st.Bytes += int64(n)
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			// interrupted = ctx canceled 的包装
			if ctx.Err() != nil {
				f.Close()
				if st.paused {
					finalize("paused", "")
				} else {
					os.Remove(tmp)
					finalize("cancel", "已取消")
				}
				return
			}
			f.Close()
			finalize("error", "传输中断："+rerr.Error())
			return
		}
	}
	f.Close()
	if st.Bytes == 0 {
		os.Remove(tmp)
		finalize("error", "传输为空（可能 VIP/无版权）")
		return
	}
	if err := os.Rename(tmp, st.Path); err != nil {
		finalize("error", "落盘失败："+err.Error())
		return
	}
	finalize("ok", "")
}

// dlMusErrVIP404 自定义错误串（网易云外链对 VIP 曲的 /404 重定向）
const dlMusErrVIP404 = "__MSIC_VIP404__"

// dlMusCancel 取消任务（终态，清理 .part）
func dlMusCancel(id string) {
	dlMusInit()
	dlMusStore.mu.Lock()
	c, ok := dlMusStore.canc[id]
	if ok {
		delete(dlMusStore.canc, id)
	}
	if t := dlMusStore.tasks[id]; t != nil {
		t.paused = false
		if t.Status == "running" || t.Status == "paused" {
			t.Status = "cancel"
			t.Err = "已取消"
			t.At = time.Now()
		}
	}
	dlMusStore.mu.Unlock()
	if ok {
		c()
	}
}

// dlMusPause 暂停音乐任务（保留 .part，可续传）。返回 false 表示不可暂停。
func dlMusPause(id string) bool {
	dlMusInit()
	dlMusStore.mu.Lock()
	t := dlMusStore.tasks[id]
	if t == nil || t.Status != "running" {
		dlMusStore.mu.Unlock()
		return false
	}
	t.paused = true
	t.Status = "paused"
	t.Err = ""
	c := dlMusStore.canc[id]
	dlMusStore.mu.Unlock()
	if c != nil {
		c() // 打断本轮；.part 保留
	}
	return true
}

// dlMusResume 继续音乐任务：从 .part 续传（HTTP Range）
func dlMusResume(id string) bool {
	dlMusInit()
	dlMusStore.mu.Lock()
	t := dlMusStore.tasks[id]
	if t == nil || t.Status != "paused" {
		dlMusStore.mu.Unlock()
		return false
	}
	dlMusStore.mu.Unlock()
	dlMusLaunch(t)
	return true
}

// dlMusIsRunning 单任务是否运行中（批量判定用）
func dlMusIsRunning(id string) bool {
	dlMusInit()
	dlMusStore.mu.Lock()
	defer dlMusStore.mu.Unlock()
	t := dlMusStore.tasks[id]
	return t != nil && t.Status == "running"
}

// dlMusTaskList 任务列表（按时间倒序，截掉 40 条以前）
func dlMusTaskList() []DlMusicTask {
	dlMusInit()
	dlMusStore.mu.Lock()
	defer dlMusStore.mu.Unlock()
	out := make([]DlMusicTask, 0, len(dlMusStore.tasks))
	for _, t := range dlMusStore.tasks {
		cp := *t
		out = append(out, cp)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].At.After(out[b].At) })
	if len(out) > 40 {
		out = out[:40]
	}
	return out
}

// dlMusRunning 是否还有运行中的任务（前端轮询判断）
func dlMusRunning() bool {
	dlMusInit()
	dlMusStore.mu.Lock()
	defer dlMusStore.mu.Unlock()
	for _, t := range dlMusStore.tasks {
		if t.Status == "running" {
			return true
		}
	}
	return false
}
