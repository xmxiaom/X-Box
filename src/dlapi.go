package main

// 下载板块 HTTP 接口（/api/dl/* 与 /api/dlmusic/*）。
//
// 路由：
//   GET  /api/dl/search?q=        磁力聚合搜索（多源并发）
//   GET  /api/dl/config           配置（视频/音乐目录）
//   POST /api/dl/config           保存配置
//   POST /api/dl/start            提交磁力到内置 BT 引擎（kind=video/music）
//   GET  /api/dl/tasks            统一任务列表（BT 视频/音乐 + 直链音乐）
//   POST /api/dl/remove?id=       移除一个终态任务
//   POST /api/dl/cancel?id=       停止任务（BT/音乐通用，终态）
//   POST /api/dl/pause?id=        暂停一个任务（保留断点，可续传）
//   POST /api/dl/resume?id=       继续一个暂停任务（断点续传）
//   POST /api/dl/pauseall         全部暂停
//   POST /api/dl/resumeall        全部继续
//   POST /api/dl/cancelall        全部停止
//   POST /api/dl/cleardone        清除已完成任务
//   POST /api/dl/clearall         清空全部任务（运行中先停止）
//   GET  /api/dlmusic/search      网易云+酷我+BT 并行搜索
//   POST /api/dlmusic/start       音乐直链下载入队
//   GET  /api/dlmusic/tasks       音乐任务列表（分页兼容旧前端）
//   POST /api/dlmusic/cancel      取消一个音乐任务

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	dlSearchMaxKB  = 128 // 搜索关键词上限字符
	dlSearchLimits = 60  // 聚合结果上限条数
)

// handleDlSearch GET /api/dl/search?q=
func (s *Server) handleDlSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len([]rune(q)) > dlSearchMaxKB {
		fail(w, 400, "请输入搜索关键词（1-64 字）")
		return
	}
	items, stats := dlSearchVideo(q)
	if len(items) > dlSearchLimits {
		items = items[:dlSearchLimits]
	}
	ok(w, map[string]any{"items": items, "sources": stats})
}

// handleDlConfigGet GET /api/dl/config
func (s *Server) handleDlConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := loadDlConfig()
	ok(w, map[string]any{
		"videoDir": cfg.VideoDir,
		"musicDir": cfg.MusicDir,
		"maxTasks": cfg.MaxTasks,
		"maxConns": cfg.MaxConns,
		"pipeline": cfg.Pipeline,
		"limits": map[string]int{
			"maxTasksHi": btMaxTasks,
			"maxConnsHi": btMaxConnsCap,
			"pipelineHi": btPipeCap,
		},
		"suggest": map[string]string{
			"video": dlDefaultDir("video"),
			"music": dlDefaultDir("music"),
		},
	})
}

// handleDlConfigSave POST /api/dl/config
func (s *Server) handleDlConfigSave(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		fail(w, 400, "读取请求失败")
		return
	}
	var in map[string]any
	if json.Unmarshal(body, &in) != nil {
		fail(w, 400, "配置格式错误")
		return
	}
	cfg := loadDlConfig()
	if vd, has := in["videoDir"]; has {
		if vd == nil {
			vd = ""
		}
		vs := strings.TrimSpace(toStr(vd))
		if vs != "" {
			if _, safe := safeDownloadDir(vs); !safe {
				fail(w, 400, "视频下载目录不可用："+vs)
				return
			}
		}
		cfg.VideoDir = vs
	}
	if md, has := in["musicDir"]; has {
		if md == nil {
			md = ""
		}
		ms := strings.TrimSpace(toStr(md))
		if ms != "" {
			if _, safe := safeDownloadDir(ms); !safe {
				fail(w, 400, "音乐下载目录不可用："+ms)
				return
			}
		}
		cfg.MusicDir = ms
	}
	// 并发/线程（0 或越界回落默认；缺省字段不动原值）
	if v, has := in["maxTasks"]; has {
		cfg.MaxTasks = toInt(v)
	}
	if v, has := in["maxConns"]; has {
		cfg.MaxConns = toInt(v)
	}
	if v, has := in["pipeline"]; has {
		cfg.Pipeline = toInt(v)
	}
	if err := saveDlConfig(cfg); err != nil {
		fail(w, 500, "配置写入失败："+err.Error())
		return
	}
	cfg = loadDlConfig()
	btApplyTune(cfg) // 立即生效（下一次入队也读同一份）
	ok(w, map[string]any{
		"videoDir": cfg.VideoDir,
		"musicDir": cfg.MusicDir,
		"maxTasks": cfg.MaxTasks,
		"maxConns": cfg.MaxConns,
		"pipeline": cfg.Pipeline,
		"limits": map[string]int{
			"maxTasksHi": btMaxTasks,
			"maxConnsHi": btMaxConnsCap,
			"pipelineHi": btPipeCap,
		},
		"suggest": map[string]string{
			"video": dlDefaultDir("video"),
			"music": dlDefaultDir("music"),
		},
	})
}

// toInt json any → int（数字/字符串都认，非法返回 0）
func toInt(v any) int {
	switch x := v.(type) {
	case float64:
		return int(x)
	case int:
		return x
	case string:
		if n, err := strconv.Atoi(strings.TrimSpace(x)); err == nil {
			return n
		}
	}
	return 0
}

// toStr json any → 字符串
func toStr(v any) string {
	if s, isStr := v.(string); isStr {
		return s
	}
	return ""
}

// handleDlStart POST /api/dl/start {magnet, title, kind}
// kind=video（默认）用 videoDir；kind=music 用 musicDir（音乐 BT 兜底）。
func (s *Server) handleDlStart(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if err != nil {
		fail(w, 400, "读取请求失败")
		return
	}
	var in struct {
		Magnet string `json:"magnet"`
		Title  string `json:"title"`
		Kind   string `json:"kind"`
	}
	if json.Unmarshal(body, &in) != nil || strings.TrimSpace(in.Magnet) == "" {
		fail(w, 400, "缺少磁力链接")
		return
	}
	if !strings.HasPrefix(in.Magnet, "magnet:?xt=urn:btih:") {
		fail(w, 400, "不是有效的磁力链接")
		return
	}
	if len(in.Magnet) > 4<<20 {
		fail(w, 400, "磁力链接过长")
		return
	}
	cfg := loadDlConfig()
	kind := in.Kind
	if kind != "music" {
		kind = "video"
	}
	dir := strings.TrimSpace(cfg.VideoDir)
	defMsg := ""
	if kind == "music" {
		dir = strings.TrimSpace(cfg.MusicDir)
		if dir == "" {
			dir = dlDefaultDir("music")
		}
		if dir == "" {
			fail(w, 400, "请先在「音乐下载」的设置里选择音乐目录")
			return
		}
	} else {
		if dir == "" {
			dir = dlDefaultDir("video")
			defMsg = "（未设置目录，使用缺省 " + dir + "）"
		}
		if dir == "" {
			fail(w, 400, "请先在「设置」里选择视频下载目录")
			return
		}
	}
	t, err := dlStartBT(in.Magnet, dir, in.Title)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	msg := "已加入下载列表"
	if defMsg != "" {
		msg += defMsg
	}
	ok(w, map[string]any{"taskId": t.ID, "msg": msg, "dir": dir})
}

// handleDlTasks GET /api/dl/tasks —— 统一任务列表
func (s *Server) handleDlTasks(w http.ResponseWriter, r *http.Request) {
	tasks := dlUnifiedTasks()
	running := false
	for _, t := range tasks {
		switch t.Status {
		case "running", "fetching", "downloading", "pending":
			running = true
		}
	}
	ok(w, map[string]any{"tasks": tasks, "running": running})
}

// handleDlRemove POST /api/dl/remove?id=
func (s *Server) handleDlRemove(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		fail(w, 400, "缺少任务 id")
		return
	}
	if strings.HasPrefix(id, "bt") {
		btRemove(id)
	} else {
		dlMusRemove(id)
	}
	ok(w, nil)
}

// handleDlCancel POST /api/dl/cancel?id= —— 统一停止（BT / 音乐）
func (s *Server) handleDlCancel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		fail(w, 400, "缺少任务 id")
		return
	}
	if strings.HasPrefix(id, "bt") {
		btCancel(id)
	} else {
		dlMusCancel(id)
	}
	ok(w, nil)
}

// handleDlPause POST /api/dl/pause?id= —— 暂停（保留断点）
func (s *Server) handleDlPause(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		fail(w, 400, "缺少任务 id")
		return
	}
	if strings.HasPrefix(id, "bt") {
		if !btPause(id) {
			fail(w, 400, "该任务当前不可暂停")
			return
		}
	} else if !dlMusPause(id) {
		fail(w, 400, "该任务当前不可暂停")
		return
	}
	ok(w, nil)
}

// handleDlResume POST /api/dl/resume?id= —— 继续（断点续传）
func (s *Server) handleDlResume(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		fail(w, 400, "缺少任务 id")
		return
	}
	if strings.HasPrefix(id, "bt") {
		if !btResume(id) {
			fail(w, 400, "该任务当前不可继续")
			return
		}
	} else if !dlMusResume(id) {
		fail(w, 400, "该任务当前不可继续")
		return
	}
	ok(w, nil)
}

// handleDlPauseAll POST /api/dl/pauseall
func (s *Server) handleDlPauseAll(w http.ResponseWriter, r *http.Request) {
	n := btPauseAll()
	for _, t := range dlUnifiedTasks() {
		if t.Type == "music" && t.Status == "running" {
			if dlMusPause(t.ID) {
				n++
			}
		}
	}
	ok(w, map[string]any{"count": n})
}

// handleDlResumeAll POST /api/dl/resumeall
func (s *Server) handleDlResumeAll(w http.ResponseWriter, r *http.Request) {
	n := btResumeAll()
	for _, t := range dlUnifiedTasks() {
		if t.Type == "music" && t.Status == "paused" {
			if dlMusResume(t.ID) {
				n++
			}
		}
	}
	ok(w, map[string]any{"count": n})
}

// handleDlCancelAll POST /api/dl/cancelall —— 全部停止
func (s *Server) handleDlCancelAll(w http.ResponseWriter, r *http.Request) {
	n := btCancelAllTasks()
	for _, t := range dlUnifiedTasks() {
		if t.Type == "music" && (t.Status == "running" || t.Status == "paused") {
			dlMusCancel(t.ID)
			n++
		}
	}
	ok(w, map[string]any{"count": n})
}

// handleDlClearDone POST /api/dl/cleardone —— 清除已完成
func (s *Server) handleDlClearDone(w http.ResponseWriter, r *http.Request) {
	n := btClearDone()
	n += dlMusClearDone()
	ok(w, map[string]any{"count": n})
}

// handleDlClearAll POST /api/dl/clearall —— 清空全部（运行中先停）
func (s *Server) handleDlClearAll(w http.ResponseWriter, r *http.Request) {
	n := btClearAll()
	n += dlMusClearAll()
	ok(w, map[string]any{"count": n})
}

// ---------- 音乐 ----------

// handleDlMusicSearch GET /api/dlmusic/search?q=&page=
func (s *Server) handleDlMusicSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" || len([]rune(q)) > dlSearchMaxKB {
		fail(w, 400, "请输入搜索关键词（1-64 字）")
		return
	}
	page, _ := parseUint(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	items, count, err := dlMusSearch(r.Context(), q, page)
	if err != nil {
		fail(w, 502, "搜索失败："+err.Error())
		return
	}
	// 酷我有声源（免登录直链库）与 BT 兜底并行不阻塞
	var kwItems []DlKwItem
	var btItems []DlItem
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		defer func() { recover() }()
		ctx2, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		list, err := dlKwSearch(ctx2, q, page)
		if err == nil {
			kwItems = list
		}
	}()
	go func() {
		defer wg.Done()
		defer func() { recover() }()
		if page == 1 {
			btItems = dlMusicBTSearch(r.Context(), q)
		}
	}()
	wg.Wait()
	cfg := loadDlConfig()
	ok(w, map[string]any{
		"items": items,
		"count": count,
		"page":  page,
		"dir":   dlMusMusicDir(cfg),
		"kw":    kwItems,
		"bt":    btItems,
	})
}

// dlMusicBTSearch 音乐 BT 兜底搜索：TPB 音频区 + Nyaa 音频类目
func dlMusicBTSearch(ctx context.Context, kw string) []DlItem {
	var out []DlItem
	var mu sync.Mutex
	var wg sync.WaitGroup
	run := func(f func(context.Context, string) ([]DlItem, error)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer func() { recover() }()
			ctx2, cancel := context.WithTimeout(ctx, 8*time.Second)
			defer cancel()
			items, err := f(ctx2, kw)
			if err != nil || len(items) == 0 {
				return
			}
			mu.Lock()
			out = append(out, items...)
			mu.Unlock()
		}()
	}
	run(dlSrcTpbAudio)
	run(dlSrcNyaaAudio)
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(9 * time.Second):
	}
	out = dlDedup(out)
	sort.Slice(out, func(a, b int) bool { return out[a].Seeds > out[b].Seeds })
	if len(out) > 20 {
		out = out[:20]
	}
	return out
}

// parseUint 正整数
func parseUint(s string) (int, error) {
	s = strings.TrimSpace(s)
	v, err := strconv.Atoi(s)
	if err != nil || v < 0 {
		return 0, err
	}
	return v, nil
}

// handleDlMusicStart POST /api/dlmusic/start
func (s *Server) handleDlMusicStart(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if err != nil {
		fail(w, 400, "读取请求失败")
		return
	}
	var in struct {
		Source string `json:"source"` // 空默认 netease；"kuwo" 走酷我直链
		ID     int64  `json:"id"`
		Rid    string `json:"rid"` // 酷我 MUSIC_xxx
		Name   string `json:"name"`
		Artist string `json:"artist"`
	}
	if json.Unmarshal(body, &in) != nil || strings.TrimSpace(in.Name) == "" {
		fail(w, 400, "缺少歌曲信息")
		return
	}
	var req dlMusReq
	switch in.Source {
	case "kuwo":
		rid := strings.TrimSpace(in.Rid)
		if !strings.HasPrefix(rid, "MUSIC_") || strings.TrimPrefix(rid, "MUSIC_") == "" {
			fail(w, 400, "缺少酷我曲目标识")
			return
		}
		req = dlMusReq{Source: "kuwo", Kw: DlKwItem{Rid: rid, Name: in.Name, Artist: in.Artist}, Name: in.Name, Artist: in.Artist}
	default:
		if in.ID <= 0 {
			fail(w, 400, "缺少歌曲信息")
			return
		}
		req = dlMusReq{Source: "netease", SongID: in.ID, Name: in.Name, Artist: in.Artist}
	}
	cfg := loadDlConfig()
	st, err := dlMusStart(cfg, req)
	if err != nil {
		fail(w, 500, err.Error())
		return
	}
	ok(w, map[string]any{"task": st})
}

// handleDlMusicTasks GET /api/dlmusic/tasks（数据源同一视图，music 过滤）
func (s *Server) handleDlMusicTasks(w http.ResponseWriter, r *http.Request) {
	var tasks []DlTaskView
	running := false
	for _, t := range dlUnifiedTasks() {
		if t.Type != "music" {
			continue
		}
		switch t.Status {
		case "running", "downloading":
			running = true
		}
		tasks = append(tasks, t)
	}
	ok(w, map[string]any{"tasks": tasks, "running": running})
}

// handleDlMusicCancel POST /api/dlmusic/cancel?id=
func (s *Server) handleDlMusicCancel(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		fail(w, 400, "缺少任务 id")
		return
	}
	dlMusCancel(id)
	ok(w, nil)
}

// dlMusRemove 移除音乐终态任务（与 DlTaskView 状态对齐）
func dlMusRemove(id string) {
	dlMusInit()
	dlMusStore.mu.Lock()
	t, ok0 := dlMusStore.tasks[id]
	dlMusStore.mu.Unlock()
	if ok0 && t.Status != "running" && t.Status != "paused" {
		dlMusStore.mu.Lock()
		delete(dlMusStore.tasks, id)
		dlMusStore.mu.Unlock()
	}
}

// dlMusClearDone 清除音乐已完成任务
func dlMusClearDone() int {
	dlMusInit()
	dlMusStore.mu.Lock()
	defer dlMusStore.mu.Unlock()
	n := 0
	for id, t := range dlMusStore.tasks {
		if t.Status == "ok" {
			delete(dlMusStore.tasks, id)
			delete(dlMusStore.canc, id)
			n++
		}
	}
	return n
}

// dlMusClearAll 清空音乐任务（运行中/暂停中先取消）
func dlMusClearAll() int {
	dlMusInit()
	for _, t := range dlMusTaskList() {
		if t.Status == "running" || t.Status == "paused" {
			dlMusCancel(t.ID)
		}
	}
	dlMusStore.mu.Lock()
	defer dlMusStore.mu.Unlock()
	n := len(dlMusStore.tasks)
	dlMusStore.tasks = map[string]*DlMusicTask{}
	dlMusStore.canc = map[string]context.CancelFunc{}
	dlMusStore.order = nil
	return n
}
