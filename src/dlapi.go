package main

// 下载板块 HTTP 接口（/api/dl/* 与 /api/dlmusic/*）。
//
// 路由：
//   GET  /api/dl/search?q=       磁力聚合搜索（多源并发）
//   GET  /api/dl/config          配置（目录 + 三种下载器连接信息）
//   POST /api/dl/config          保存配置
//   POST /api/dl/probe           立即探测各下载器
//   POST /api/dl/start           提交磁力到下载器
//   GET  /api/dl/tasks           提交历史（会话级）
//   GET  /api/dlmusic/search?q=&page=  网易云搜索
//   POST /api/dlmusic/start      下载一首（免费曲直下到音乐目录）
//   GET  /api/dlmusic/tasks      音乐下载任务/进度
//   POST /api/dlmusic/cancel     取消一个音乐下载任务

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

// dlCfgView 配置给前端看时隐藏密码（只报是否已配置）
func dlCfgView(cfg DlConfig, hidePass bool) map[string]any {
	clients := map[string]any{}
	one := func(kind string, c DlClientCfg) map[string]any {
		v := map[string]any{"base": c.Base, "user": c.User, "secret": c.Secret}
		if hidePass {
			v["pass"] = boolStrMask(c.Pass)
		} else {
			v["pass"] = c.Pass
		}
		return v
	}
	clients["qbittorrent"] = one("qbittorrent", cfg.Clients.QBittorrent)
	clients["transmission"] = one("transmission", cfg.Clients.Transmission)
	clients["aria2"] = one("aria2", cfg.Clients.Aria2)
	return map[string]any{
		"videoDir": cfg.VideoDir,
		"musicDir": cfg.MusicDir,
		"clients":  clients,
		"suggest": map[string]string{
			"video": dlDefaultDir("video"),
			"music": dlDefaultDir("music"),
		},
	}
}

// boolStrMask 密码掩码（未配置为空串）
func boolStrMask(s string) string {
	if s == "" {
		return ""
	}
	return "已设置"
}

// handleDlConfigGet GET /api/dl/config
func (s *Server) handleDlConfigGet(w http.ResponseWriter, r *http.Request) {
	cfg := loadDlConfig()
	ok(w, dlCfgView(cfg, false))
}

// handleDlConfigSave POST /api/dl/config —— 应用成组校验后落盘
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
	if cl, has := in["clients"]; has {
		if m, isMap := cl.(map[string]any); isMap {
			pick := func(kind string) (DlClientCfg, bool) {
				raw, ok0 := m[kind]
				if !ok0 || raw == nil {
					return DlClientCfg{}, false
				}
				sub, ok1 := raw.(map[string]any)
				if !ok1 {
					return DlClientCfg{}, false
				}
				// 前端提交的密码是掩码值「已设置」：保留旧密码
				pass := toStr(sub["pass"])
				if pass == "" || pass == "已设置" {
					pass = ""
				}
				if pass == "" {
					switch kind {
					case "qbittorrent":
						pass = cfg.Clients.QBittorrent.Pass
					case "transmission":
						pass = cfg.Clients.Transmission.Pass
					case "aria2":
						pass = cfg.Clients.Aria2.Secret
					}
				}
				out := DlClientCfg{
					Base:   strings.TrimSpace(toStr(sub["base"])),
					User:   strings.TrimSpace(toStr(sub["user"])),
					Secret: strings.TrimSpace(toStr(sub["secret"])),
				}
				switch kind {
				case "qbittorrent":
					out.Pass = pass
				case "transmission":
					out.Pass = pass
				case "aria2":
					out.Secret = pass
				}
				return out, true
			}
			if v, ok2 := pick("qbittorrent"); ok2 {
				cfg.Clients.QBittorrent = v
			}
			if v, ok2 := pick("transmission"); ok2 {
				cfg.Clients.Transmission = v
			}
			if v, ok2 := pick("aria2"); ok2 {
				cfg.Clients.Aria2 = v
			}
		}
	}
	if err := saveDlConfig(cfg); err != nil {
		fail(w, 500, "配置写入失败："+err.Error())
		return
	}
	ok(w, dlCfgView(cfg, true))
}

// toStr json any → 字符串
func toStr(v any) string {
	if s, isStr := v.(string); isStr {
		return s
	}
	return ""
}

// handleDlProbe POST /api/dl/probe
func (s *Server) handleDlProbe(w http.ResponseWriter, r *http.Request) {
	cfg := loadDlConfig()
	ok(w, map[string]any{"results": dlProbeAll(cfg)})
}

// handleDlStart POST /api/dl/start {magnet, title}
func (s *Server) handleDlStart(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if err != nil {
		fail(w, 400, "读取请求失败")
		return
	}
	var in struct {
		Magnet string `json:"magnet"`
		Title  string `json:"title"`
	}
	if json.Unmarshal(body, &in) != nil || strings.TrimSpace(in.Magnet) == "" {
		fail(w, 400, "缺少磁力链接")
		return
	}
	if !strings.HasPrefix(in.Magnet, "magnet:?xt=urn:btih:") {
		fail(w, 400, "不是有效的磁力链接")
		return
	}
	if len(in.Magnet) > 240<<10 {
		fail(w, 400, "磁力链接过长")
		return
	}
	cfg := loadDlConfig()
	dir := strings.TrimSpace(cfg.VideoDir)
	if dir == "" {
		dir = dlDefaultDir("video")
	}
	kind, err := dlSubmit(cfg, in.Magnet, dir)
	if err != nil {
		dlHistPush(dlHistEntry{At: time.Now(), Title: in.Title, OK: false, Msg: err.Error()})
		fail(w, 500, err.Error())
		return
	}
	msg := "已提交到 " + kind
	if dir != "" {
		msg += "，目标目录 " + dir
	} else {
		msg += "（保存目录跟随下载器默认设置）"
	}
	dlHistPush(dlHistEntry{At: time.Now(), Title: in.Title, Client: kind, OK: true, Msg: msg})
	ok(w, map[string]any{"client": kind, "msg": msg, "dir": dir})
}

// handleDlTasks GET /api/dl/tasks
func (s *Server) handleDlTasks(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"tasks": dlHistList()})
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

// handleDlMusicTasks GET /api/dlmusic/tasks
func (s *Server) handleDlMusicTasks(w http.ResponseWriter, r *http.Request) {
	tasks := dlMusTaskList()
	running := false
	for _, t := range tasks {
		if t.Status == "running" {
			running = true
			break
		}
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
