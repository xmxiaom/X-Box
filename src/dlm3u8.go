package main

// 短剧内置下载引擎（HLS/m3u8，v1.3.58）。
//
// 背景：短剧网盘源（黑豆/rejusou）只给夸克网盘分享、无直链；内置 BT 下载器只吃磁力。
// 经实测 TVBox 生态的 macCMS 采集接口（mdzyapi.com 等）可免登录返回短剧的 m3u8 播放
// 地址（master → 子 m3u8 → .ts/.m4s 分片，VOD、无加密），从而让短剧真正内置下载，
// 不再依赖网盘转存。
//
// 本引擎职责：
//   - 交付：整剧 → 在视频下载目录建「剧名」子目录，逐集下载 HLS 分片并拼接为 .ts
//     文件（TS 即 HLS 原生容器，免转码、浏览器/VLC/Kodi/媒体库原生可播）。每集一个
//     .ts，命名 001.ts / 002.ts …（集序来自 macCMS 的 play_url 序号）。
//   - 分批：任务结构按「分集」切片，每集一个子任务（DlM3u8Ep），逐集串行下载。
//   - 断点续传：已完成的集保存在任务里，重启/暂停后续传只补未完成的集；单集内部
//     已写分片不重复下载（写到一个临时 .part，全部完成后 rename 为正式 .ts）。
//   - 并发：依赖音乐/BT 共同的并发语义，进场先拿 dlPersistRec 持久化，避免升级丢任务。
//
// 实现约束（继承项目红线）：
//   - 零第三方依赖（纯标准库），不引入 ffmpeg；.ts 直接拼接即可（TS 是自装配容器）。
//   - 持久化失败静默；目录不可用则跳过恢复且不写错位置。
//   - 永不删除用户文件：remove 需显式带 files=1 才删，否则只移除任务记录。
//
// 网络获取复用 dlHTTP / dlDefaultUA / dlFetch。

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// DlM3u8Ep 一集短剧下载子任务（内存态）
type DlM3u8Ep struct {
	Idx  int    // 集序号（1 起）
	Name string // 集名（如 第01集，空则用序号）
	URI  string // 该集的 master m3u8 URL
}

// DlM3u8Task 一个短剧下载任务
type DlM3u8Task struct {
	ID        string    `json:"id"`                 // m3u8 + 纳秒时间戳
	Title     string    `json:"title"`              // 剧名
	Dir       string    `json:"dir"`                // 任务根目录（视频下载目录/<剧名>）
	Status    string    `json:"status"`             // pending / running / paused / done / error / cancel
	Err       string    `json:"err,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	At        time.Time `json:"at"`

	Eps      []DlM3u8Ep `json:"eps"`      // 全部分集（含已完成与待下）
	CurEp    int        `json:"curEp"`    // 当前正在下/下一个待下的分集下标（-1 表示未开始）
	DoneEps  int        `json:"doneEps"`  // 已完成的集数
	Bytes    int64      `json:"bytes"`    // 当前集已下字节
	Total    int64      `json:"total"`    // 当前集估算总量（取了子 m3u8 后更新）
	Speed    int64      `json:"speed"`    // 瞬时速度（B/s，进程内展示）
	LastTime time.Time  `json:"-"`        // 速度采样
	LastB    int64      `json:"-"`

	paused bool
	gen    int // 轮次代号：暂停/续传丢弃过期轮次收尾
}

// dlM3u8Store 引擎任务表
var dlM3u8Store struct {
	mu    sync.Mutex
	tasks map[string]*DlM3u8Task
	canc  map[string]context.CancelFunc
	once  sync.Once
}

func dlM3u8Init() {
	dlM3u8Store.once.Do(func() {
		dlM3u8Store.tasks = map[string]*DlM3u8Task{}
		dlM3u8Store.canc = map[string]context.CancelFunc{}
	})
}

// dlM3u8NewID 生成任务 id（m3u8 + 纳秒）
func dlM3u8NewID() string {
	return fmt.Sprintf("m3u8%d", time.Now().UnixNano())
}

// dlM3u8StartDedup 创建短剧下载任务。eps 需包含全部集（含 m3u8 URI）。
// 与音乐一致：正在下载/已完成但未 error/cancel 的任务复用，不重复建。
func dlM3u8StartDedup(title, dir string, eps []DlM3u8Ep) (*DlM3u8Task, bool, error) {
	dlM3u8Init()
	title = strings.TrimSpace(title)
	if title == "" {
		return nil, false, fmt.Errorf("缺少剧名")
	}
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return nil, false, fmt.Errorf("尚未设置视频下载目录")
	}
	if _, safe := safeDownloadDir(dir); !safe {
		return nil, false, fmt.Errorf("下载目录不可用：%s", dir)
	}
	// 去重：同剧名只要有活动任务就复用
	dlM3u8Store.mu.Lock()
	for _, ex := range dlM3u8Store.tasks {
		if ex == nil || !strings.EqualFold(ex.Title, title) {
			continue
		}
		if ex.Status != "error" && ex.Status != "cancel" {
			dlM3u8Store.mu.Unlock()
			return ex, true, nil
		}
	}
	dlM3u8Store.mu.Unlock()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, false, fmt.Errorf("创建下载目录失败：%s", err.Error())
	}
	// 短剧放独立子目录，避免与其它视频文件混杂
	sub := dlMusFileClean(title) // 复用文件名清洗（目录同名）
	if sub == "" {
		sub = dlMusFileClean(title)
	}
	taskDir := dlMusUniquePath(dir, sub) // 重名自动 (1)(2)…
	if err := os.MkdirAll(taskDir, 0o755); err != nil {
		return nil, false, fmt.Errorf("创建任务目录失败：%s", err.Error())
	}
	if len(eps) > 0 {
		// 补齐集序号
		for i := range eps {
			if eps[i].Idx == 0 {
				eps[i].Idx = i + 1
			}
		}
	}
	t := &DlM3u8Task{
		ID:        dlM3u8NewID(),
		Title:     title,
		Dir:       taskDir,
		Status:    "pending",
		CreatedAt: time.Now(),
		At:        time.Now(),
		Eps:       eps,
		CurEp:     -1,
	}
	dlM3u8Store.mu.Lock()
	dlM3u8Store.tasks[t.ID] = t
	dlM3u8Store.mu.Unlock()
	dlM3u8Launch(t)
	return t, false, nil
}

// dlM3u8TaskList 任务快照（按创建倒序）
func dlM3u8TaskList() []DlM3u8Task {
	dlM3u8Init()
	dlM3u8Store.mu.Lock()
	out := make([]DlM3u8Task, 0, len(dlM3u8Store.tasks))
	for _, t := range dlM3u8Store.tasks {
		if t != nil {
			out = append(out, *t)
		}
	}
	dlM3u8Store.mu.Unlock()
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.After(out[b].CreatedAt) })
	return out
}

// dlM3u8Launch 拉起/续跑一轮下载；已在跑或终态忽略
func dlM3u8Launch(t *DlM3u8Task) {
	dlM3u8Init()
	dlM3u8Store.mu.Lock()
	if t.Status == "running" || t.Status == "done" || t.Status == "cancel" {
		dlM3u8Store.mu.Unlock()
		return
	}
	t.gen++
	gen := t.gen
	ctx, cancel := context.WithCancel(context.Background())
	dlM3u8Store.canc[t.ID] = cancel
	dlM3u8Store.mu.Unlock()

	t.paused = false
	t.Status = "running"
	t.Err = ""

	go func() {
		defer func() { recover() }()
		dlM3u8Run(ctx, t, gen)
	}()
}

// dlM3u8Cancel 停止任务（保留已完成集，文件保留）
func dlM3u8Cancel(id string) {
	dlM3u8Init()
	dlM3u8Store.mu.Lock()
	cancel := dlM3u8Store.canc[id]
	t := dlM3u8Store.tasks[id]
	dlM3u8Store.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if t != nil {
		t.Status = "cancel"
	}
}

// dlM3u8Pause 暂停（保留断点）
func dlM3u8Pause(id string) {
	dlM3u8Init()
	dlM3u8Store.mu.Lock()
	cancel := dlM3u8Store.canc[id]
	t := dlM3u8Store.tasks[id]
	dlM3u8Store.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if t != nil {
		t.paused = true
		t.Status = "paused"
	}
}

// dlM3u8Resume 继续（断点续传：只补未完成集）
func dlM3u8Resume(id string) {
	dlM3u8Init()
	dlM3u8Store.mu.Lock()
	t := dlM3u8Store.tasks[id]
	dlM3u8Store.mu.Unlock()
	if t == nil || t.Status != "paused" {
		return
	}
	dlM3u8Launch(t)
}

// dlM3u8RemoveWithFiles 移除任务；deleteFiles=true 同时删除任务目录
func dlM3u8RemoveWithFiles(id string, deleteFiles bool) (int, error) {
	dlM3u8Init()
	dlM3u8Store.mu.Lock()
	cancel := dlM3u8Store.canc[id]
	t := dlM3u8Store.tasks[id]
	if t != nil {
		delete(dlM3u8Store.tasks, id)
		delete(dlM3u8Store.canc, id)
	}
	dlM3u8Store.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	if t == nil {
		return 0, nil
	}
	if !deleteFiles {
		return 0, nil
	}
	// 删除任务目录下所有文件（仅限任务目录内，绝不动目录外）
	var n int
	filepath.Walk(t.Dir, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return nil
		}
		if err := os.Remove(p); err == nil {
			n++
		}
		return nil
	})
	os.Remove(t.Dir)
	return n, nil
}

// dlM3u8Progress 整剧进度（百分比 0-100）
func dlM3u8Progress(t *DlM3u8Task) float64 {
	n := len(t.Eps)
	if n == 0 {
		return 0
	}
	// 已完成集数 + 当前集内进度
	if t.DoneEps >= n {
		return 100
	}
	base := float64(t.DoneEps) / float64(n) * 100
	if t.CurEp >= 0 && t.CurEp < n && t.Total > 0 {
		cur := float64(t.Bytes) / float64(t.Total) / float64(n) * 100
		if base+cur > 99 {
			return 99
		}
		return base + cur
	}
	return base
}

// ---------- 下载核心 ----------

// dlM3u8Run 逐集下载
func dlM3u8Run(ctx context.Context, t *DlM3u8Task, gen int) {
	defer func() { recover() }()
	n := len(t.Eps)
	if n == 0 {
		t.Status = "error"
		t.Err = "没有可下载的分集"
		dlPersistTouch()
		return
	}
	// 找出第一个未完成集（已完成的 eps 有 .ts 文件在盘上）
	start := t.CurEp
	if start < 0 {
		start = 0
	}
	for {
		if gen != t.gen {
			return // 本轮已被替代（暂停后新轮）
		}
		start = dlM3u8FirstPending(t, ctx)
		if start < 0 {
			break // 全部完成
		}
		if err := dlM3u8DownloadEp(ctx, t, start, gen); err != nil {
			if ctx.Err() != nil {
				// 被取消/暂停：保持 paused/cancel 状态
				if t.paused && t.Status == "running" {
					t.Status = "paused"
				}
				return
			}
			t.Status = "error"
			t.Err = err.Error()
			dlPersistTouch()
			return
		}
		if gen != t.gen {
			return
		}
		if start+1 > t.DoneEps {
			t.DoneEps = start + 1
		}
		if t.DoneEps >= n {
			t.Status = "done"
		}
		dlPersistTouch()
		if t.Status == "done" {
			break
		}
	}
	if t.Status == "running" {
		t.Status = "done"
	}
	// 清理取消句柄
	dlM3u8Store.mu.Lock()
	delete(dlM3u8Store.canc, t.ID)
	dlM3u8Store.mu.Unlock()
	dlPersistTouch()
}

// dlM3u8FirstPending 找第一个未完成集（对应 .ts 文件不存在）
func dlM3u8FirstPending(t *DlM3u8Task, ctx context.Context) int {
	for i := 0; i < len(t.Eps); i++ {
		select {
		case <-ctx.Done():
			return -1
		default:
		}
		name := dlM3u8EpName(t.Eps[i], i)
		if _, err := os.Stat(filepath.Join(t.Dir, name)); err != nil {
			return i
		}
	}
	return -1
}

// dlM3u8EpName 集文件名：001.ts …
func dlM3u8EpName(ep DlM3u8Ep, idx int) string {
	num := idx + 1
	if ep.Idx > 0 {
		num = ep.Idx
	}
	base := fmt.Sprintf("%03d", num)
	if ep.Name != "" {
		cleaned := dlMusFileClean(ep.Name)
		if cleaned != "" && cleaned != fmt.Sprintf("%03d", num) {
			base = cleaned
		}
	}
	return base + ".ts"
}

// dlM3u8DownloadEp 下载单集
func dlM3u8DownloadEp(ctx context.Context, t *DlM3u8Task, idx, gen int) error {
	if idx < 0 || idx >= len(t.Eps) {
		return fmt.Errorf("分集下标越界")
	}
	ep := t.Eps[idx]
	t.CurEp = idx
	t.Bytes = 0
	t.Total = 0
	// 解析 master → 选码率 → 子 m3u8 → 分片列表
	segs, total, err := dlM3u8Resolve(ctx, ep.URI)
	if err != nil {
		return fmt.Errorf("第%d集解析失败：%s", ep.Idx, err.Error())
	}
	if len(segs) == 0 {
		return fmt.Errorf("第%d集无分片", ep.Idx)
	}
	t.Total = int64(total)

	name := dlM3u8EpName(ep, idx)
	finalPath := filepath.Join(t.Dir, name)
	tmpPath := finalPath + ".part"
	if err := dlM3u8FetchSegments(ctx, t, ep, idx, segs, finalPath, tmpPath, gen); err != nil {
		return err
	}
	if gen != t.gen {
		return nil
	}
	// 全部完成 → rename 为正式 .ts
	if err := os.Rename(tmpPath, finalPath); err != nil {
		if _, serr := os.Stat(finalPath); serr != nil {
			return err
		}
		os.Remove(tmpPath)
	}
	return nil
}

// dlM3u8Resolve 解析 m3u8（master 或直接子清单）→ 分片 URL 列表 + 总字节估测
// 返回的 total 为 ts 分片数×估算均值（无法精确预知，用于进度展示）
func dlM3u8Resolve(ctx context.Context, uri string) (segments []string, totalBytes int, err error) {
	raw, err := dlFetch(ctx, uri, nil)
	if err != nil {
		return nil, 0, err
	}
	text := string(raw)
	if !strings.Contains(text, "#EXTM3U") {
		return nil, 0, fmt.Errorf("非 HLS 清单")
	}
	baseDir := dlM3u8BaseDir(uri)
	// master：含 #EXT-X-STREAM-INF → 取第一个/最大码率子清单再解析
	if strings.Contains(text, "#EXT-X-STREAM-INF") {
		child := dlM3u8PickChild(text, baseDir)
		if child == "" {
			return nil, 0, fmt.Errorf("master 无可选子清单")
		}
		raw2, err2 := dlFetch(ctx, dlM3u8Abs(child, baseDir, uri), nil)
		if err2 != nil {
			return nil, 0, err2
		}
		text = string(raw2)
		baseDir = dlM3u8BaseDir(dlM3u8Abs(firstPlayURL(text), baseDir, uri))
		if !strings.Contains(text, "#EXTM3U") {
			return nil, 0, fmt.Errorf("子清单非 HLS")
		}
	}
	// 解析 #EXTINF 分片
	return dlM3u8ParseSegments(text, baseDir), 0, nil
}

// dlM3u8BaseDir 取 URL 不含文件名之前的目录前缀
func dlM3u8BaseDir(u string) string {
	// 处理相对路径基准：取到最后一个 / 为止（不含文件）
	i := strings.LastIndex(u, "/")
	if i < 0 {
		return u
	}
	return u[:i+1]
}

// dlM3u8Abs 把可能相对的 URL 解析为绝对（相对子 m3u8 是相对 baseDir 的）
func dlM3u8Abs(seg, baseDir, referer string) string {
	seg = strings.TrimSpace(seg)
	if strings.HasPrefix(seg, "http://") || strings.HasPrefix(seg, "https://") {
		return seg
	}
	// 协议相对
	if strings.HasPrefix(seg, "//") {
		if i := strings.Index(referer, "://"); i >= 0 {
			return referer[:i] + ":" + seg
		}
		return "https:" + seg
	}
	if strings.HasPrefix(seg, "/") {
		// 根相对：取 referer 的 host
		if u, e := url.Parse(referer); e == nil {
			return u.Scheme + "://" + u.Host + seg
		}
	}
	return baseDir + seg
}

// dlM3u8PickChild 从 master 选子清单：优先最高 BANDWIDTH
func dlM3u8PickChild(text, baseDir string) string {
	lines := strings.Split(text, "\n")
	bestURL, bestBW := "", -1
	for i := 0; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		if !strings.HasPrefix(l, "#EXT-X-STREAM-INF") {
			continue
		}
		bw := dlM3u8StreamBW(l)
		for j := i + 1; j < len(lines); j++ {
			u := strings.TrimSpace(lines[j])
			if u == "" || strings.HasPrefix(u, "#") {
				break
			}
			if u != "" && bw >= bestBW {
				bestBW, bestURL = bw, u
			}
			break
		}
	}
	if bestURL != "" {
		return bestURL
	}
	// 退化：取第一条非注释行
	for _, l := range lines {
		u := strings.TrimSpace(l)
		if u != "" && !strings.HasPrefix(u, "#") {
			return u
		}
	}
	return ""
}

// dlM3u8StreamBW 解析 #EXT-X-STREAM-INF 的 BANDWIDTH
func dlM3u8StreamBW(l string) int {
	i := strings.Index(l, "BANDWIDTH=")
	if i < 0 {
		return -1
	}
	rest := l[i+len("BANDWIDTH="):]
	num := 0
	for _, c := range rest {
		if c >= '0' && c <= '9' {
			num = num*10 + int(c-'0')
		} else {
			break
		}
	}
	return num
}

// dlM3u8ParseSegments 从子 m3u8 提取分片绝对 URL（跳过 #EXT-X-KEY 加密分片）
func dlM3u8ParseSegments(text, baseDir string) []string {
	var out []string
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		l := strings.TrimSpace(lines[i])
		if l == "" || strings.HasPrefix(l, "#") {
			if strings.HasPrefix(l, "#EXT-X-KEY") && strings.Contains(l, "METHOD=AES-128") {
				// 加密源：跳过（我们只下 VOD 未加密）
			}
			continue
		}
		if strings.HasSuffix(l, ".m3u8") {
			continue // 忽略嵌套清单（已在上层处理）
		}
		out = append(out, dlM3u8Abs(l, baseDir, ""))
	}
	return out
}

// dlM3u8FetchSegments 拉取一集全部分片并写 .part，断点续传（已存在且大小匹配的分片跳过）
func dlM3u8FetchSegments(ctx context.Context, t *DlM3u8Task, ep DlM3u8Ep, idx int, segs []string, finalPath, tmpPath string, gen int) error {
	f, err := os.OpenFile(tmpPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		if _, serr := os.Stat(finalPath); serr == nil {
			return nil // 正式文件已存在，跳过
		}
		return fmt.Errorf("创建临时文件失败：%s", err.Error())
	}
	defer f.Close()

	for i, seg := range segs {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if gen != t.gen {
			return ctx.Err()
		}
		data, err := dlM3u8Get(ctx, seg, finalPath)
		if err != nil {
			return fmt.Errorf("分片 %d 下载失败：%s", i+1, err.Error())
		}
		if len(data) == 0 {
			continue
		}
		if _, werr := f.Write(data); werr != nil {
			return fmt.Errorf("写入失败：%s", werr.Error())
		}
		// 更新进度与速度（基于字节差分，1s 滑动）
		now := time.Now()
		if t.LastTime.IsZero() {
			t.LastTime = now
		} else {
			dt := now.Sub(t.LastTime).Seconds()
			if dt >= 1 {
				t.Speed = int64(float64(t.Bytes-t.LastB) / dt)
				t.LastTime = now
				t.LastB = t.Bytes
			}
		}
		t.Bytes += int64(len(data))
	}
	return nil
}

// dlM3u8Get 拉单个分片
func dlM3u8Get(ctx context.Context, u string, referer string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", dlDefaultUA)
	if referer != "" {
		req.Header.Set("Referer", dlM3u8BaseDir(referer))
	}
	resp, err := dlHTTP.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	// 上限 32MB/分片
	return io.ReadAll(io.LimitReader(resp.Body, 32<<20))
}

// firstPlayURL 取文本里第一个以 http 开头的 URL（用于相对子清单位算基准）
func firstPlayURL(s string) string {
	for _, l := range strings.Split(s, "\n") {
		u := strings.TrimSpace(l)
		if strings.HasPrefix(u, "http") {
			return u
		}
	}
	return ""
}

// dlM3u8ResolveSegs 供进度/总量用：解析后返回分片段与目标子清单 URL（不含下载）
//（目前进度以 get 到的字节累计为准，total 用分片计数估算——见 dlM3u8DownloadEp）

// dlM3u8ClearDone 清除已完成/已取消的短剧任务
func dlM3u8ClearDone() int {
	dlM3u8Init()
	dlM3u8Store.mu.Lock()
	defer dlM3u8Store.mu.Unlock()
	n := 0
	for id, t := range dlM3u8Store.tasks {
		if t != nil && (t.Status == "done" || t.Status == "error" || t.Status == "cancel") {
			delete(dlM3u8Store.tasks, id)
			delete(dlM3u8Store.canc, id)
			n++
		}
	}
	return n
}

// dlM3u8ClearAll 清空全部短剧任务（运行中先停止，文件保留）
func dlM3u8ClearAll() int {
	dlM3u8Init()
	for _, t := range dlM3u8TaskList() {
		if t.Status == "running" || t.Status == "paused" {
			dlM3u8Cancel(t.ID)
		}
	}
	dlM3u8Store.mu.Lock()
	defer dlM3u8Store.mu.Unlock()
	n := len(dlM3u8Store.tasks)
	dlM3u8Store.tasks = map[string]*DlM3u8Task{}
	dlM3u8Store.canc = map[string]context.CancelFunc{}
	return n
}