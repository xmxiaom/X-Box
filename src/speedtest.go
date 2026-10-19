package main

// speedtest.go —— 网络测速
//
// 设计要点：
//   1. 不开新端口：端点全挂主 mux（/api/speedtest/*），经 fnOS 网关/隧道可达。
//   2. 大流量不出网关：下载/上传在「NAS 后端 ↔ 外网测速节点」之间进行，
//      浏览器只经网关回传结果 JSON（几 KB），不占中转带宽。
//   3. 节点三层来源（用户 2026-10-19 确认「预置+拉取」）：
//        内置预置（实测可用教育网节点兜底）
//        → 上游 LibreSpeed 清单实时拉取（i-abc/Speedtest node 分支 china-education.json，
//          双源：GitHub raw + asset.bash.icu 镜像；缓存 24h，可手动刷新）
//        → 用户自定义节点（speedtest_nodes.json 持久化）
//      合并去重后全部参与竞速；节点可手动选择，也可自动选延迟最低。
//   4. 探活升级：TCP 握手 + HTTP GET pingURL 校验（非 2xx/3xx 视为死节点）。
//      根治「TCP 通但后端已坏（如 HTTP 500）仍被选中 → 下载 0」的问题。
//   5. 阶段化进度 + 实时速率：前端 1s 轮询 /api/speedtest/status，
//      down/up 阶段状态里带瞬时速率与累计流量。

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- 节点定义（LibreSpeed 协议：GET dlURL 下载，POST ulURL 上传） ----------

type stNode struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Group  string `json:"group"`       // 运营商/地区分组
	Server string `json:"-"`           // 基础地址，如 https://test.ustc.edu.cn
	Down   string `json:"-"`           // 下载路径，如 /backend/garbage.php
	Up     string `json:"-"`           // 上传路径
	Ping   string `json:"-"`           // 延迟探测路径
	Source string `json:"source,omitempty"` // builtin | remote | custom
	Alive  *bool  `json:"alive,omitempty"` // 最近一次探活结果（列表展示用）
}

// dlURL 拼完整下载地址（带防缓存随机参数）
func (n stNode) dlURL(seq int) string {
	sep := "?"
	if strings.Contains(n.Down, "?") {
		sep = "&"
	}
	return strings.TrimSuffix(n.Server, "/") + n.Down + sep + "ckSize=100&r=" + fmt.Sprintf("%d%d", time.Now().UnixNano(), seq)
}

// ulURL / pingURL 拼完整地址
func (n stNode) ulURL() string { return strings.TrimSuffix(n.Server, "/") + n.Up }
func (n stNode) pingURL() string {
	return strings.TrimSuffix(n.Server, "/") + n.Ping
}

// hostFromServer 从 Server 提取 host:port 供 TCP 探活。
// 无显式端口时必须按 scheme 补默认端口（http:80 / https:443），
// 否则 net.Dial 报 "missing port in address"，所有节点会被误判为不可达。
func hostFromServer(server string) string {
	u, err := url.Parse(server)
	if err != nil || u.Host == "" {
		return strings.TrimPrefix(strings.TrimPrefix(server, "https://"), "http://")
	}
	if u.Port() == "" {
		if u.Scheme == "https" {
			return u.Host + ":443"
		}
		return u.Host + ":80"
	}
	return u.Host
}

// 内置预置节点（2026-10 实测可用；作为上游拉取失败时的兜底）。
// neu/shu 曾在列但实测已下线，已移除；ustc/sjtu 按来源 IP 过滤（家宽 500/302）不入列。
// local：本机回环节点（NAS 内部自测）——打向启动时随机端口的专用回环监听，
// 量的是 NAS 本机协议栈基线（不含外网瓶颈），用于与外网测速对比判断宽带是否跑满。
// Server 在 initSpeedtest 里由 startLocalSpeedtest 填实际端口。
var stBuiltin = []stNode{
	{ID: "local", Name: "本机基线（回环）", Group: "本地", Down: "/speedtest-local/garbage", Up: "/speedtest-local/upload", Ping: "/speedtest-local/ping", Source: "builtin"},
	{ID: "nuaa", Name: "南京航空航天大学", Group: "教育网", Server: "http://speed.nuaa.edu.cn", Down: "/backend/garbage.php", Up: "/backend/empty.php", Ping: "/backend/empty.php", Source: "builtin"},
	{ID: "seu", Name: "东南大学", Group: "教育网", Server: "https://xnfz.seu.edu.cn", Down: "/speed/garbage.php", Up: "/speed/empty.php", Ping: "/speed/empty.php", Source: "builtin"},
}

// stLocalAddr 专用回环监听地址（127.0.0.1:随机端口），startLocalSpeedtest 赋值。
var stLocalAddr string

// startLocalSpeedtest 启动本机基线测速专用监听（仅回环，随机端口，进程内私有）。
// 生产面板只走 fnOS 网关 unix socket 不监听 TCP，回环节点必须有自己的 TCP 入口。
func startLocalSpeedtest() {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Printf("本机基线测速监听启动失败（本机基线节点不可用）: %v", err)
		return
	}
	stLocalAddr = ln.Addr().String()
	go func() {
		if err := http.Serve(ln, http.HandlerFunc(handleSpeedtestLocal)); err != nil {
			log.Printf("本机基线测速服务退出: %v", err)
		}
	}()
	for i := range stBuiltin {
		if stBuiltin[i].ID == "local" {
			stBuiltin[i].Server = "http://" + stLocalAddr
		}
	}
}

// handleSpeedtestLocal 本机回环测速端点（监听本身只绑 127.0.0.1，天然不对外）。
// /speedtest-local/ping     — 延迟探测（固定小文本）
// /speedtest-local/garbage  — 无限下载流（读侧按测速窗口自行停止）
// /speedtest-local/upload   — 上传接收（读丢弃）
func handleSpeedtestLocal(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	switch r.URL.Path {
	case "/speedtest-local/ping":
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, "pong")
	case "/speedtest-local/garbage":
		// 无限流：stDownload 的 12s 超时窗口到期后 phaseCtx 取消，读侧断开，本循环随之退出
		w.Header().Set("Content-Type", "application/octet-stream")
		block := make([]byte, 256*1024)
		for i := range block {
			block[i] = byte(i % 251)
		}
		flusher, _ := w.(http.Flusher)
		for r.Context().Err() == nil {
			if _, err := w.Write(block); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
	case "/speedtest-local/upload":
		_, _ = io.Copy(io.Discard, r.Body)
	default:
		fail(w, 404, "未知端点")
	}
}

// 上游清单镜像模板（i-abc/Speedtest node 分支维护，三源互备：GitHub 原站 + 两个镜像）
var stMirrors = []string{
	"https://raw.githubusercontent.com/i-abc/speedtest/node/%s",
	"https://asset.bash.icu/https://raw.githubusercontent.com/i-abc/speedtest/node/%s",
	"https://cdn.jsdelivr.net/gh/i-abc/Speedtest@node/%s",
}

// 上游清单定义：LibreSpeed JSON（教育网）+ Ookla speedtest-cli 清单（运营商线路）。
// 运营商 txt 格式："speedtest-cli,电信 天津           ,-o tjrate.tjtele.com"，
// -o 后为 Ookla 服务器域名（默认端口 8080，标准路径 /speedtest/*）。
var stListSources = []struct {
	File  string // node 分支文件名
	Group string // 运营商/地区分组
	Kind  string // librespeed | ookla
}{
	{"china-education.json", "教育网", "librespeed"},
	{"china-telecom-m-thread-v4.txt", "电信", "ookla"},
	{"china-unicom-m-thread-v4.txt", "联通", "ookla"},
	{"china-mobile-m-thread-v4.txt", "移动", "ookla"},
	{"china-other-m-thread-v4.txt", "广电", "ookla"},
}

// LibreSpeed JSON 单条格式（librespeed/speedtest servers.json 标准）
type lsServer struct {
	ID      any    `json:"id"`
	Name    string `json:"name"`
	Server  string `json:"server"`
	DlURL   string `json:"dlURL"`
	UlURL   string `json:"ulURL"`
	PingURL string `json:"pingURL"`
}

// ---------- 运行状态 ----------

type stHistoryItem struct {
	At       string  `json:"at"`
	Server   string  `json:"server"`
	PingMs   float64 `json:"ping"`
	JitterMs float64 `json:"jitter"`
	LossPct  float64 `json:"loss"`
	DownMbps float64 `json:"down"`
	UpMbps   float64 `json:"up"`
}

type stState struct {
	mu       sync.Mutex
	running  bool
	cancel   context.CancelFunc
	phase    string  // idle | ping | down | up | done | error
	progress int     // 当前阶段进度 0-100
	msg      string  // 附加信息（当前节点等）
	server   string  // 选定节点展示名
	pingMs   float64
	jitterMs float64
	lossPct  float64
	downMbps float64
	upMbps   float64
	liveMbps float64 // 实时瞬时速率（down/up 阶段，前端实时显示）
	downMB   float64 // 下载阶段累计流量 MB
	upMB     float64 // 上传阶段累计流量 MB
	errMsg   string
	history  []stHistoryItem
	histPath string // 持久化文件（initSpeedtest 赋值）
}

type stNodesState struct {
	mu        sync.Mutex
	custom    []stNode // 用户自定义
	remote    []stNode // 上游拉取
	remoteAt  time.Time
	fetching  bool
	fetchErr  string
	nodesPath string
	cachePath string
}

var st = &stState{phase: "idle", history: []stHistoryItem{}}
var stN = &stNodesState{}

func initSpeedtest(dataDir string) {
	st.histPath = filepath.Join(dataDir, "speedtest_history.json")
	if b, err := os.ReadFile(st.histPath); err == nil {
		var h []stHistoryItem
		if json.Unmarshal(b, &h) == nil && h != nil {
			st.history = h
		}
	}
	stN.nodesPath = filepath.Join(dataDir, "speedtest_nodes.json")
	stN.cachePath = filepath.Join(dataDir, "speedtest_remote.json")
	stN.loadCustom()
	stN.loadRemoteCache()
	startLocalSpeedtest() // 本机基线回环节点：起专用回环监听并回填 local 节点 Server
	// 后台异步刷新上游清单（不阻塞启动）
	go stN.refreshRemote(false)
}

func (s *stNodesState) loadCustom() {
	if b, err := os.ReadFile(s.nodesPath); err == nil {
		var list []stNode
		if json.Unmarshal(b, &list) == nil {
			for i := range list {
				if list[i].ID == "" {
					list[i].ID = "cu-" + fmt.Sprintf("%d", time.Now().UnixNano()%100000) + fmt.Sprintf("%d", i)
				}
				if list[i].Source == "" {
					list[i].Source = "custom"
				}
			}
			s.custom = list
		}
	}
}

func (s *stNodesState) saveCustomLocked() {
	b, err := json.MarshalIndent(s.custom, "", "  ")
	if err == nil {
		_ = os.WriteFile(s.nodesPath, b, 0o644)
	}
}

func (s *stNodesState) loadRemoteCache() {
	if b, err := os.ReadFile(s.cachePath); err == nil {
		var wrapper struct {
			FetchedAt string   `json:"fetched_at"`
			Nodes     []stNode `json:"nodes"`
		}
		if json.Unmarshal(b, &wrapper) == nil && len(wrapper.Nodes) > 0 {
			s.remote = wrapper.Nodes
			if t, e := time.Parse(time.RFC3339, wrapper.FetchedAt); e == nil {
				s.remoteAt = t
			}
		}
	}
}

func (s *stNodesState) saveRemoteLocked() {
	wrapper := struct {
		FetchedAt string   `json:"fetched_at"`
		Nodes     []stNode `json:"nodes"`
	}{FetchedAt: s.remoteAt.Format(time.RFC3339), Nodes: s.remote}
	b, err := json.MarshalIndent(wrapper, "", "  ")
	if err == nil {
		_ = os.WriteFile(s.cachePath, b, 0o644)
	}
}

// isFetching 线程安全读取 fetching 标志（handler 构造响应用）
func (s *stNodesState) isFetching() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fetching
}

// refreshRemote 从上游拉取节点清单（教育网 JSON + 三网/广电 Ookla txt，逐清单三镜像容错）。
// force=true 由用户手动触发；false 时缓存未满 24h 直接跳过。
func (s *stNodesState) refreshRemote(force bool) {
	s.mu.Lock()
	if s.fetching {
		s.mu.Unlock()
		return
	}
	if !force && time.Since(s.remoteAt) < 24*time.Hour && len(s.remote) > 0 {
		s.mu.Unlock()
		return
	}
	s.fetching = true
	s.fetchErr = ""
	s.mu.Unlock()

	client := &http.Client{Timeout: 15 * time.Second}
	var got []stNode
	var errs []string
	for _, ls := range stListSources {
		// 单清单：三镜像依次尝试，拿到非空结果即止
		var body []byte
		var lastErr error
		for _, tpl := range stMirrors {
			resp, err := client.Get(fmt.Sprintf(tpl, ls.File))
			if err != nil {
				lastErr = err
				continue
			}
			b, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			resp.Body.Close()
			if err != nil {
				lastErr = err
				continue
			}
			if resp.StatusCode != 200 {
				lastErr = fmt.Errorf("HTTP %d", resp.StatusCode)
				continue
			}
			body = b
			break
		}
		if len(body) == 0 {
			errs = append(errs, ls.File+"："+lastErr.Error())
			continue
		}
		// 解析为节点（按清单类型）
		var nodes []stNode
		if ls.Kind == "librespeed" {
			nodes = stParseLibrespeed(body, ls.Group)
		} else {
			nodes = stParseOoklaTxt(body, ls.Group)
		}
		if len(nodes) == 0 {
			errs = append(errs, ls.File+"：无有效节点")
			continue
		}
		got = append(got, nodes...)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetching = false
	if len(got) > 0 {
		s.remote = got
		s.remoteAt = time.Now()
		s.saveRemoteLocked()
		if len(errs) > 0 {
			s.fetchErr = fmt.Sprintf("部分清单拉取失败（%d/%d 成功）：%s", len(stListSources)-len(errs), len(stListSources), strings.Join(errs, "; "))
			if len(s.remote) > 0 {
				// 有成功清单时失败清单只提示，不算整体错误——截断避免 toast 过长
				if len(s.fetchErr) > 120 {
					s.fetchErr = s.fetchErr[:120] + "…"
				}
			} 
		}
	} else {
		s.fetchErr = "上游清单拉取失败：" + strings.Join(errs, "; ")
	}
}

// stParseLibrespeed 解析 LibreSpeed servers.json 标准格式（教育网清单）。
func stParseLibrespeed(body []byte, group string) []stNode {
	var list []lsServer
	if json.Unmarshal(body, &list) != nil {
		return nil
	}
	var out []stNode
	for _, it := range list {
		name := strings.TrimSpace(it.Name)
		server := strings.TrimSpace(it.Server)
		// 过滤：v6 节点（家用宽带基本无 v6 出口）、空地址
		if server == "" || it.DlURL == "" || strings.Contains(name, "v6") {
			continue
		}
		out = append(out, stNode{
			ID:     "r-" + fmt.Sprintf("%x", fnv32(server)),
			Name:   name,
			Group:  group,
			Server: server,
			Down:   it.DlURL,
			Up:     it.UlURL,
			Ping:   it.PingURL,
			Source: "remote",
		})
	}
	return out
}

// stParseOoklaTxt 解析 i-abc speedtest-cli 清单行："speedtest-cli,电信 天津 ,-o 域名"。
// Ookla 服务器默认端口 8080，标准路径 /speedtest/backend/garbage.php（下载）、
// /speedtest/upload.php（上传）、/speedtest/latency.txt（延迟）。
func stParseOoklaTxt(body []byte, group string) []stNode {
	var out []stNode
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		parts := strings.Split(line, ",")
		if len(parts) < 3 || strings.TrimSpace(parts[0]) != "speedtest-cli" {
			continue
		}
		name := strings.Join(parts[1:len(parts)-1], ",") // 名称本身可能含逗号，取中间段
		name = strings.TrimSpace(name)
		opt := strings.TrimSpace(parts[len(parts)-1])
		if !strings.HasPrefix(opt, "-o ") {
			continue
		}
		host := strings.TrimSpace(strings.TrimPrefix(opt, "-o "))
		if host == "" || name == "" {
			continue
		}
		// 域名可能自带端口；补默认 Ookla 端口 8080
		if !strings.Contains(host, ":") {
			host += ":8080"
		}
		server := "http://" + host
		// 过滤 v6 专项节点
		if strings.Contains(name, "v6") {
			continue
		}
		out = append(out, stNode{
			ID:     "isp-" + fmt.Sprintf("%x", fnv32(server)),
			Name:   name,
			Group:  group,
			Server: server,
			Down:   "/speedtest/backend/garbage.php",
			Up:     "/speedtest/upload.php",
			Ping:   "/speedtest/latency.txt",
			Source: "remote",
		})
	}
	return out
}

// all 返回合并后的节点列表：custom > remote > builtin（自定义排最前），同 Server 去重。
func (s *stNodesState) all() []stNode {
	s.mu.Lock()
	defer s.mu.Unlock()
	merged := make([]stNode, 0, len(s.custom)+len(s.remote)+len(stBuiltin))
	seen := map[string]bool{}
	add := func(list []stNode) {
		for _, n := range list {
			key := strings.ToLower(hostFromServer(n.Server)) + "|" + n.Down
			if seen[key] {
				continue
			}
			seen[key] = true
			merged = append(merged, n)
		}
	}
	add(s.custom)
	add(s.remote)
	add(stBuiltin)
	return merged
}

// find 按 ID 找节点（自定义 → 上游 → 内置）。
func (s *stNodesState) find(id string) (stNode, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.custom {
		if n.ID == id {
			return n, true
		}
	}
	for _, n := range s.remote {
		if n.ID == id {
			return n, true
		}
	}
	for _, n := range stBuiltin {
		if n.ID == id {
			return n, true
		}
	}
	return stNode{}, false
}

// ---------- HTTP handlers ----------

// GET /api/speedtest/status — 状态轮询 + 历史记录
func handleSpeedtestStatus(w http.ResponseWriter, r *http.Request) {
	ok(w, st.snapshot())
}

// GET /api/speedtest/nodes — 合并节点列表 + 上游同步状态
func handleSpeedtestNodes(w http.ResponseWriter, r *http.Request) {
	stN.mu.Lock()
	fetching, fetchErr, remoteAt := stN.fetching, stN.fetchErr, stN.remoteAt
	stN.mu.Unlock()
	ok(w, map[string]any{
		"nodes":     stN.all(),
		"fetching":  fetching,
		"fetch_err": fetchErr,
		"remote_at": func() string {
			if remoteAt.IsZero() {
				return ""
			}
			return remoteAt.Format("2006-01-02 15:04:05")
		}(),
	})
}

// POST /api/speedtest/nodes/refresh — 手动刷新上游清单（异步触发，立即返回当前列表+fetching，
// 前端轮询 /api/speedtest/nodes 直到 fetching=false；不能返回空 nodes，否则前端会把列表清空）
func handleSpeedtestNodesRefresh(w http.ResponseWriter, r *http.Request) {
	go stN.refreshRemote(true)
	fetching := stN.isFetching()
	ok(w, map[string]any{"nodes": stN.all(), "fetching": fetching})
}

// POST /api/speedtest/nodes/save — 新增/更新自定义节点
func handleSpeedtestNodesSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID     string `json:"id"`
		Name   string `json:"name"`
		Server string `json:"server"`
		Down   string `json:"down"`
		Up     string `json:"up"`
		Ping   string `json:"ping"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 64<<10)).Decode(&req); err != nil {
		fail(w, 400, "参数解析失败")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Server = strings.TrimRight(strings.TrimSpace(req.Server), "/")
	if req.Name == "" || req.Server == "" {
		fail(w, 400, "名称与节点地址不能为空")
		return
	}
	if !strings.HasPrefix(req.Server, "http://") && !strings.HasPrefix(req.Server, "https://") {
		fail(w, 400, "节点地址需以 http:// 或 https:// 开头")
		return
	}
	def := func(v, d string) string {
		v = strings.TrimSpace(v)
		if v == "" {
			return d
		}
		if !strings.HasPrefix(v, "/") {
			return "/" + v
		}
		return v
	}
	node := stNode{
		ID:     strings.TrimSpace(req.ID),
		Name:   req.Name,
		Group:  "自定义",
		Server: req.Server,
		Down:   def(req.Down, "/backend/garbage.php"),
		Up:     def(req.Up, "/backend/empty.php"),
		Ping:   def(req.Ping, "/backend/empty.php"),
		Source: "custom",
	}
	stN.mu.Lock()
	if node.ID == "" {
		node.ID = "cu-" + fmt.Sprintf("%x", fnv32(node.Server+node.Name+fmt.Sprint(time.Now().UnixNano())))
		stN.custom = append(stN.custom, node)
	} else {
		found := false
		for i := range stN.custom {
			if stN.custom[i].ID == node.ID {
				stN.custom[i] = node
				found = true
				break
			}
		}
		if !found {
			// 指定了不存在 ID → 追加
			node.ID = "cu-" + fmt.Sprintf("%x", fnv32(node.Server+node.Name+fmt.Sprint(time.Now().UnixNano())))
			stN.custom = append(stN.custom, node)
		}
	}
	stN.saveCustomLocked()
	stN.mu.Unlock()
	ok(w, map[string]any{"nodes": stN.all()})
}

// POST /api/speedtest/nodes/delete — 删除自定义节点
func handleSpeedtestNodesDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req); err != nil || req.ID == "" {
		fail(w, 400, "参数错误")
		return
	}
	stN.mu.Lock()
	kept := stN.custom[:0]
	for _, n := range stN.custom {
		if n.ID != req.ID {
			kept = append(kept, n)
		}
	}
	stN.custom = kept
	stN.saveCustomLocked()
	stN.mu.Unlock()
	ok(w, map[string]any{"nodes": stN.all()})
}

// POST /api/speedtest/start — 启动测速（body 可带 {"node_id":"xx"} 指定节点；运行中直接返回）
func handleSpeedtestStart(w http.ResponseWriter, r *http.Request) {
	var req struct {
		NodeID string `json:"node_id"`
	}
	// body 可为空（自动模式）
	_ = json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&req)

	st.mu.Lock()
	if st.running {
		st.mu.Unlock()
		ok(w, st.snapshot())
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	st.running = true
	st.cancel = cancel
	st.phase = "ping"
	st.progress = 0
	st.msg = "探测测速节点…"
	st.server = ""
	st.pingMs, st.jitterMs, st.lossPct = 0, 0, 0
	st.downMbps, st.upMbps, st.downMB, st.upMB, st.liveMbps = 0, 0, 0, 0, 0
	st.errMsg = ""
	st.mu.Unlock()

	go st.run(ctx, strings.TrimSpace(req.NodeID))
	ok(w, st.snapshot())
}

// POST /api/speedtest/stop — 中止测速
func handleSpeedtestStop(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	if st.running && st.cancel != nil {
		st.cancel()
	}
	st.mu.Unlock()
	ok(w, st.snapshot())
}

// POST /api/speedtest/clear — 清空历史记录
func handleSpeedtestClear(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	st.history = []stHistoryItem{}
	st.saveHistLocked()
	st.mu.Unlock()
	ok(w, st.snapshot())
}

func (s *stState) snapshot() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return map[string]any{
		"running":  s.running,
		"phase":    s.phase,
		"progress": s.progress,
		"msg":      s.msg,
		"server":   s.server,
		"ping":     s.pingMs,
		"jitter":   s.jitterMs,
		"loss":     s.lossPct,
		"down":     s.downMbps,
		"up":       s.upMbps,
		"live":     s.liveMbps,
		"down_mb":  s.downMB,
		"up_mb":    s.upMB,
		"error":    s.errMsg,
		"history":  s.history,
	}
}

// ---------- 测速主流程 ----------

func (s *stState) setPhase(phase string, progress int, msg string) {
	s.mu.Lock()
	s.phase, s.progress, s.msg = phase, progress, msg
	s.mu.Unlock()
}

func (s *stState) fail(ctx context.Context, msg string) {
	s.mu.Lock()
	s.running = false
	s.phase = "error"
	s.errMsg = msg
	s.mu.Unlock()
	_ = ctx
}

func (s *stState) run(ctx context.Context, wantID string) {
	defer func() {
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()

	nodes := stN.all()
	if len(nodes) == 0 {
		s.fail(ctx, "没有可用测速节点")
		return
	}

	// 指定节点：只测它（但探活失败直接报错）
	if wantID != "" {
		if n, found := stN.find(wantID); found {
			nodes = []stNode{n}
		} else {
			s.fail(ctx, "指定的测速节点不存在")
			return
		}
	}

	// ---- 阶段 1：并发探活（TCP + HTTP 双校验），选延迟最低的活节点 ----
	if stIsp.snapshotISP() == "" {
		go stIsp.detect() // 线路未识别时异步补识别，探活窗口内通常能就绪
	}
	probes := make([]stProbe, 0, len(nodes))
	var pmu sync.Mutex
	var wg sync.WaitGroup
	for _, n := range nodes {
		wg.Add(1)
		go func(n stNode) {
			defer wg.Done()
			ms := stProbeNode(ctx, n)
			pmu.Lock()
			if ms > 0 {
				probes = append(probes, stProbe{n, ms})
			}
			alive := ms > 0
			n.Alive = &alive
			pmu.Unlock()
		}(n)
	}
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(8 * time.Second):
	case <-ctx.Done():
		s.fail(ctx, "已中止")
		return
	}
	if len(probes) == 0 {
		s.fail(ctx, "所有测速节点均不可达，请检查 NAS 外网连接，或刷新节点列表")
		return
	}
	sort.Slice(probes, func(i, j int) bool { return probes[i].ms < probes[j].ms })
	best := probes[0]
	// 同运营商优先竞速：probes 已按延迟升序，取首个与本机 ISP 同组的活节点；
	// 同组无活节点（或线路未知）则保持全局最优兜底。跨网测速点常屏蔽异网
	// 访问，强行选延迟低的异网站点会导致下载/上传阶段整体失败。
	if myISP := ispNormalize(stIsp.snapshotISP()); myISP != "" && myISP != "未知" {
		for _, p := range probes {
			if ispNormalize(p.node.Group) == myISP {
				best = p
				break
			}
		}
	}

	// 探活结果缓存给节点列表展示
	stN.rememberAlive(probes, nodes)

	// 延迟/抖动/丢包：对选定节点连续 TCP 握手 12 次
	s.setPhase("ping", 0, "测试延迟（"+best.node.Name+"）")
	var lat []float64
	lostCnt := 0
	const rounds = 12
	for i := 0; i < rounds; i++ {
		if ctx.Err() != nil {
			s.fail(ctx, "已中止")
			return
		}
		t0 := time.Now()
		c, err := net.DialTimeout("tcp", hostFromServer(best.node.Server), 2*time.Second)
		if err != nil {
			lostCnt++
		} else {
			c.Close()
			lat = append(lat, float64(time.Since(t0).Microseconds())/1000.0)
		}
		s.mu.Lock()
		s.progress = (i + 1) * 100 / rounds
		s.mu.Unlock()
		time.Sleep(60 * time.Millisecond)
	}
	var pingMs, jitterMs float64
	if len(lat) > 0 {
		var sum float64
		for _, v := range lat {
			sum += v
		}
		pingMs = sum / float64(len(lat))
		var jd float64
		for i := 1; i < len(lat); i++ {
			jd += absf(lat[i] - lat[i-1])
		}
		jitterMs = jd / float64(len(lat)-1)
	}
	lossPct := float64(lostCnt) * 100 / float64(rounds)
	s.mu.Lock()
	s.server = best.node.Name
	s.pingMs, s.jitterMs, s.lossPct = r1(pingMs), r1(jitterMs), r1(lossPct)
	s.mu.Unlock()

	client := &http.Client{
		Timeout:   0,
		Transport: &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 6},
	}

	// ---- 阶段 2：下载（多连接 8s，取后 60% 平均速率） ----
	s.setPhase("down", 0, "下载测速（"+best.node.Name+"）")
	downMbps, downMB, derr := stDownload(ctx, client, best.node, s)
	if derr != nil && ctx.Err() == nil {
		s.fail(ctx, "下载测速失败："+derr.Error())
		return
	}
	if ctx.Err() != nil {
		s.fail(ctx, "已中止")
		return
	}
	s.mu.Lock()
	s.downMbps, s.downMB, s.liveMbps = r1(downMbps), r1(downMB), 0
	s.mu.Unlock()

	// ---- 阶段 3：上传（多连接 8s） ----
	s.setPhase("up", 0, "上传测速（"+best.node.Name+"）")
	upMbps, upMB, uerr := stUpload(ctx, client, best.node, s)
	if uerr != nil && ctx.Err() == nil {
		s.fail(ctx, "上传测速失败："+uerr.Error())
		return
	}
	if ctx.Err() != nil {
		s.fail(ctx, "已中止")
		return
	}
	s.mu.Lock()
	s.upMbps, s.upMB, s.liveMbps = r1(upMbps), r1(upMB), 0
	s.mu.Unlock()

	// ---- 完成：写历史 ----
	s.mu.Lock()
	s.phase = "done"
	s.progress = 100
	s.msg = "测速完成"
	item := stHistoryItem{
		At:       time.Now().Format("2006-01-02 15:04:05"),
		Server:   s.server,
		PingMs:   s.pingMs,
		JitterMs: s.jitterMs,
		LossPct:  s.lossPct,
		DownMbps: s.downMbps,
		UpMbps:   s.upMbps,
	}
	s.history = append([]stHistoryItem{item}, s.history...)
	if len(s.history) > 30 {
		s.history = s.history[:30]
	}
	s.saveHistLocked()
	s.mu.Unlock()
}

// stProbe 单节点探活结果。
type stProbe struct {
	node stNode
	ms   float64
}

// rememberAlive 把最近一次探活结果记到 remote/custom 列表（供前端标注死节点）。
func (s *stNodesState) rememberAlive(probes []stProbe, all []stNode) {
	aliveMap := map[string]bool{}
	for _, p := range probes {
		aliveMap[p.node.ID] = true
	}
	s.mu.Lock()
	for i := range s.custom {
		if a, has := aliveMap[s.custom[i].ID]; has {
			s.custom[i].Alive = &a
		}
	}
	for i := range s.remote {
		if a, has := aliveMap[s.remote[i].ID]; has {
			s.remote[i].Alive = &a
		}
	}
	s.mu.Unlock()
}

func (s *stState) saveHistLocked() {
	if s.histPath == "" {
		return
	}
	b, err := json.Marshal(s.history)
	if err == nil {
		_ = os.WriteFile(s.histPath, b, 0o644)
	}
}

// ---------- 探活与速率测算 ----------

// stProbeNode：TCP 握手 3 次平均延迟 + HTTP GET pingURL 校验。
// 只有 HTTP 可达（2xx/3xx）才返回正值延迟，否则返回 0（视为死节点）。
func stProbeNode(ctx context.Context, n stNode) float64 {
	host := hostFromServer(n.Server)
	var total time.Duration
	fail := 0
	const tries = 3
	for i := 0; i < tries; i++ {
		t0 := time.Now()
		c, err := net.DialTimeout("tcp", host, 1500*time.Millisecond)
		if err != nil {
			fail++
			continue
		}
		c.Close()
		total += time.Since(t0)
	}
	if fail == tries {
		return 0
	}
	// HTTP 校验：HEAD pingURL，1.5s 超时。HEAD 不被支持时退 GET（少数后端只认 GET）。
	pingClient := &http.Client{
		Timeout: 1500 * time.Millisecond,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse // 3xx 也算活（部分节点 302 到 www）
		},
	}
	try := func(method string) bool {
		req, err := http.NewRequestWithContext(ctx, method, n.pingURL(), nil)
		if err != nil {
			return false
		}
		resp, err := pingClient.Do(req)
		if err != nil {
			return false
		}
		io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
		resp.Body.Close()
		return resp.StatusCode < 400
	}
	if !try("HEAD") && !try("GET") {
		return 0
	}
	return float64(total.Microseconds()) / 1000.0 / float64(tries-fail)
}

// 下载：8 连接并发拉流 8 秒，丢弃前 2 秒（TCP 爬坡），后 6 秒平均速率。
// 采样回调实时更新 state（进度 / 累计流量 / 瞬时速率）。
func stDownload(ctx context.Context, client *http.Client, node stNode, s *stState) (mbps, totalMB float64, err error) {
	phaseCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var mu sync.Mutex
	var bytesSum int64
	var lastErr error
	var wg sync.WaitGroup
	const conns = 8
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func(seq int) {
			defer wg.Done()
			req, e := http.NewRequestWithContext(phaseCtx, "GET", node.dlURL(seq), nil)
			if e != nil {
				return
			}
			req.Header.Set("Cache-Control", "no-cache")
			resp, e := client.Do(req)
			if e != nil {
				mu.Lock()
				lastErr = e
				mu.Unlock()
				return
			}
			defer resp.Body.Close()
			buf := make([]byte, 128*1024)
			for {
				nn, e := resp.Body.Read(buf)
				if nn > 0 {
					mu.Lock()
					bytesSum += int64(nn)
					mu.Unlock()
				}
				if e != nil {
					break
				}
			}
		}(i)
	}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()
	start := time.Now()
	samples := map[int64]int64{} // 秒偏移 → 累计字节
	prevBytes, prevAt := int64(0), start
	for {
		select {
		case <-waitDone:
			goto done
		case <-phaseCtx.Done():
			goto done
		case <-tick.C:
			mu.Lock()
			s := bytesSum
			mu.Unlock()
			off := int64(time.Since(start).Seconds())
			samples[off] = s
			// 瞬时速率：本采样窗口内增量
			dt := time.Since(prevAt).Seconds()
			inst := 0.0
			if dt > 0 {
				inst = float64(s-prevBytes) * 8 / dt / 1e6
			}
			prevBytes, prevAt = s, time.Now()
			p := int(time.Since(start).Seconds() * 100 / 8)
			if p > 99 {
				p = 99
			}
			st.mu.Lock()
			st.progress = p
			st.downMB = r1(float64(s) / 1e6)
			st.liveMbps = r1(inst)
			st.mu.Unlock()
		}
	}
done:
	if bytesSum == 0 && lastErr != nil {
		return 0, 0, lastErr
	}
	elapsed := time.Since(start).Seconds()
	if elapsed < 1 {
		elapsed = 1
	}
	totalMB = float64(bytesSum) / 1e6
	useFrom := int64(2)
	base := int64(0)
	if v, has := samples[useFrom]; has {
		base = v
		if span := time.Since(start).Seconds() - float64(useFrom); span >= 2 {
			mbps = float64(bytesSum-base) * 8 / span / 1e6
			return mbps, totalMB, nil
		}
	}
	mbps = float64(bytesSum) * 8 / elapsed / 1e6
	return mbps, totalMB, nil
}

// 上传：8 连接并发 POST 随机块 8 秒
func stUpload(ctx context.Context, client *http.Client, node stNode, s *stState) (mbps, totalMB float64, err error) {
	phaseCtx, cancel := context.WithTimeout(ctx, 12*time.Second)
	defer cancel()
	var mu sync.Mutex
	var bytesSum int64
	var lastErr error
	var wg sync.WaitGroup
	const conns = 8
	const chunk = 256 * 1024
	block := make([]byte, chunk)
	for i := range block {
		block[i] = byte(i % 251)
	}
	for i := 0; i < conns; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				if phaseCtx.Err() != nil {
					return
				}
				req, e := http.NewRequestWithContext(phaseCtx, "POST", node.ulURL(), nil)
				if e != nil {
					mu.Lock()
					lastErr = e
					mu.Unlock()
					return
				}
				req.ContentLength = int64(chunk)
				req.Header.Set("Content-Type", "application/octet-stream")
				var sent int64
				req.Body = io.NopCloser(&countReader{r: newLoopReader(block), n: &sent, limit: int64(chunk)})
				resp, e := client.Do(req)
				if e != nil {
					mu.Lock()
					lastErr = e
					bytesSum += sent
					mu.Unlock()
					return
				}
				io.Copy(io.Discard, resp.Body)
				resp.Body.Close()
				mu.Lock()
				bytesSum += sent
				mu.Unlock()
			}
		}()
	}
	waitDone := make(chan struct{})
	go func() { wg.Wait(); close(waitDone) }()
	start := time.Now()
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
loop:
	for {
		select {
		case <-waitDone:
			break loop
		case <-phaseCtx.Done():
			break loop
		case <-tick.C:
			mu.Lock()
			s := bytesSum
			mu.Unlock()
			p := int(time.Since(start).Seconds() * 100 / 8)
			if p > 99 {
				p = 99
			}
			st.mu.Lock()
			st.progress = p
			st.upMB = r1(float64(s) / 1e6)
			st.mu.Unlock()
		}
	}
	mu.Lock()
	bs := bytesSum
	le := lastErr
	mu.Unlock()
	if bs == 0 && le != nil {
		return 0, 0, le
	}
	elapsed := time.Since(start).Seconds()
	if elapsed < 1 {
		elapsed = 1
	}
	totalMB = float64(bs) / 1e6
	mbps = float64(bs) * 8 / elapsed / 1e6
	return mbps, totalMB, nil
}

// loopReader：无限循环输出 block 内容；countReader 包装计数，达到 limit 报 EOF
type loopReader struct {
	b   []byte
	pos int
}

func newLoopReader(b []byte) *loopReader { return &loopReader{b: b} }

func (l *loopReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	n := 0
	for n < len(p) {
		c := copy(p[n:], l.b[l.pos:])
		l.pos = (l.pos + c) % len(l.b)
		n += c
	}
	return n, nil
}

type countReader struct {
	r     *loopReader
	n     *int64
	limit int64
}

func (c *countReader) Read(p []byte) (int, error) {
	if *c.n >= c.limit {
		return 0, io.EOF
	}
	nn, err := c.r.Read(p)
	*c.n += int64(nn)
	return nn, err
}

// ---------- ISP 线路识别 ----------

// stISP 运营商线路识别结果（24h 缓存，手动可强刷）。
type stISP struct {
	mu       sync.Mutex
	ip       string
	isp      string // 联通/电信/移动/广电/教育网/其他…
	region   string // 省 市
	checkedAt time.Time
	checking bool
}

var stIsp = &stISP{}

// snapshotISP 并发安全读取当前识别到的运营商名（测速竞速同 ISP 优先判断用）。
func (d *stISP) snapshotISP() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.isp
}

// handleSpeedtestISP GET /api/speedtest/isp — 线路识别（带缓存；?refresh=1 强刷）。
func handleSpeedtestISP(w http.ResponseWriter, r *http.Request) {
	refresh := r.URL.Query().Get("refresh") == "1"
	stIsp.mu.Lock()
	if refresh || stIsp.isp == "" || time.Since(stIsp.checkedAt) > 24*time.Hour {
		if !stIsp.checking {
			stIsp.checking = true
			stIsp.mu.Unlock()
			stIsp.detect()
			stIsp.mu.Lock()
			stIsp.checking = false
		}
	}
	stIsp.mu.Unlock()
	stIsp.mu.Lock()
	defer stIsp.mu.Unlock()
	ok(w, map[string]any{
		"ip": stIsp.ip, "isp": stIsp.isp, "region": stIsp.region,
		"checked_at": func() string {
			if stIsp.checkedAt.IsZero() {
				return ""
			}
			return stIsp.checkedAt.Format("2006-01-02 15:04:05")
		}(),
	})
}

// detect 依次尝试多源识别（ipip.net 主源结构化最好，ip-api.com 备源字段直给 ISP）。
func (d *stISP) detect() {
	client := &http.Client{Timeout: 6 * time.Second}
	// 主源：ipip.net（location 数组：国家/省/市/区县/ISP）
	if req, err := http.NewRequest("GET", "https://myip.ipip.net/json", nil); err == nil {
		if resp, err := client.Do(req); err == nil {
			var j struct {
				Ret string `json:"ret"`
				Data struct {
					IP       string   `json:"ip"`
					Location []string `json:"location"`
				} `json:"data"`
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if json.Unmarshal(b, &j) == nil && j.Ret == "ok" && j.Data.IP != "" {
				isp, region := "", ""
				loc := j.Data.Location
				if len(loc) >= 3 {
					region = strings.TrimSpace(loc[1]) + " " + strings.TrimSpace(loc[2])
				} else if len(loc) >= 2 {
					region = strings.TrimSpace(loc[1])
				}
				if len(loc) >= 5 {
					isp = strings.TrimSpace(loc[4])
				}
				if isp == "" {
					isp = "未知"
				}
				d.mu.Lock()
				d.ip, d.isp, d.region, d.checkedAt = j.Data.IP, isp, region, time.Now()
				d.mu.Unlock()
				return
			}
		}
	}
	// 备源：ip-api.com（ISP 字段直给；国内直连可用，免费限频 45 req/min）
	if resp, err := client.Get("http://ip-api.com/json/?lang=zh-CN&fields=status,regionName,city,isp,query"); err == nil {
		var j struct {
			Status     string `json:"status"`
			RegionName string `json:"regionName"`
			City       string `json:"city"`
			ISP        string `json:"isp"`
			Query      string `json:"query"`
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if json.Unmarshal(b, &j) == nil && j.Status == "success" && j.Query != "" {
			isp := strings.TrimSpace(j.ISP)
			if isp == "" {
				isp = "未知"
			}
			d.mu.Lock()
			d.ip, d.isp, d.region, d.checkedAt = j.Query, isp, strings.TrimSpace(j.RegionName)+" "+strings.TrimSpace(j.City), time.Now()
			d.mu.Unlock()
			return
		}
	}
	// 双源皆败：保留旧结果（若有），只更新时间戳外不覆盖，等下次触发重试
}

// ispNormalize 归一 ISP 名到节点分组口径（电信/联通/移动/广电），
// 用于竞速时同运营商节点优先。匹配不上返回原词（教育网等）。
func ispNormalize(isp string) string {
	for _, g := range []string{"电信", "联通", "移动", "广电"} {
		if strings.Contains(isp, g) {
			return g
		}
	}
	// ipip.net 对教育网常给「教育网/中国教育网」；ip-api 给 CERNET
	if strings.Contains(isp, "教育") || strings.Contains(strings.ToUpper(isp), "CERNET") {
		return "教育网"
	}
	return isp
}

// ---------- 工具 ----------

func fnv32(s string) uint32 {
	var h uint32 = 2166136261
	for i := 0; i < len(s); i++ {
		h ^= uint32(s[i])
		h *= 16777619
	}
	return h
}

func absf(v float64) float64 {
	if v < 0 {
		return -v
	}
	return v
}

func r1(v float64) float64 {
	return float64(int(v*10+0.5)) / 10
}
