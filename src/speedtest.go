package main

// speedtest.go —— 网络测速
//
// 设计要点：
//   1. 不开新端口：端点全挂主 mux（/api/speedtest/*），经 fnOS 网关/隧道可达。
//   2. 大流量不出网关：下载/上传在「NAS 后端 ↔ 外网测速节点」之间进行，
//      浏览器只经网关回传结果 JSON（几 KB），不占中转带宽。
//   3. 节点三层来源（用户 2026-10-19 确认「预置+拉取」，2026-10-20 明确远程保留 ≤20）：
//        内置预置（实测可用节点兜底：教育网 2 个 + 测速网电信云测 1 个）
//        → 上游清单实时拉取（LibreSpeed 教育网 JSON + Ookla 三网/广电 txt
//          + speedtest.cn 节点库 CSV，来源 spiritLHLS/speedtest.cn-CN-ID 每日自动更新；
//          缓存 24h，可手动刷新）
//        → 用户自定义节点（speedtest_nodes.json 持久化）
//      远程节点合并去重（同机别名折叠）后自动探活，按用户策略裁剪至 ≤20：
//      归属地云测优先 → 四大运营商各保底 1 → 其余名额按延迟填充质量云测；
//      全部参与竞速，失败自动回退次优。
//   4. 探活升级：TCP 握手 + HTTP GET pingURL 校验（非 2xx/3xx 视为死节点）。
//      根治「TCP 通但后端已坏（如 HTTP 500）仍被选中 → 下载 0」的问题。
//   5. 阶段化进度 + 实时速率：前端 1s 轮询 /api/speedtest/status，
//      down/up 阶段状态里带瞬时速率与累计流量。

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------- 节点定义（LibreSpeed 协议：GET dlURL 下载，POST ulURL 上传） ----------

type stNode struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Group  string `json:"group"`            // 运营商/地区分组
	Server string `json:"-"`                // 基础地址，如 https://test.ustc.edu.cn
	Down   string `json:"-"`                // 下载路径，如 /backend/garbage.php
	Up     string `json:"-"`                // 上传路径
	Ping   string `json:"-"`                // 延迟探测路径
	Source string `json:"source,omitempty"` // builtin | remote | custom
	Alive  *bool  `json:"alive,omitempty"`  // 最近一次探活结果（列表展示用）
	// SizeParam 下载端点的计费参数名：""=用 ckSize（LibreSpeed 协议）；
	// "size"=运营商自建节点（部分本地电信/移动节点只认 size，传 ckSize 会
	// 返回 Content-Length: 0，导致下行恒为 0、只有上行速度）。
	SizeParam string `json:"-"`
}

// dlURL 拼完整下载地址（带防缓存随机参数 + 计费参数）
func (n stNode) dlURL(seq int) string {
	sep := "?"
	if strings.Contains(n.Down, "?") {
		sep = "&"
	}
	param := n.SizeParam
	if param == "" {
		param = "ckSize"
	}
	return strings.TrimSuffix(n.Server, "/") + n.Down + sep + param + "=100&r=" + fmt.Sprintf("%d%d", time.Now().UnixNano(), seq)
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
// stcn-nj：2026-10-20 实测 /hello 200、/download 4s 拉 324MB；jsinfo 系其余节点（镇江/苏州等）已下线。
var stBuiltin = []stNode{
	{ID: "nuaa", Name: "南京航空航天大学", Group: "教育网", Server: "http://speed.nuaa.edu.cn", Down: "/backend/garbage.php", Up: "/backend/empty.php", Ping: "/backend/empty.php", Source: "builtin"},
	{ID: "seu", Name: "东南大学", Group: "教育网", Server: "https://xnfz.seu.edu.cn", Down: "/speed/garbage.php", Up: "/speed/empty.php", Ping: "/speed/empty.php", Source: "builtin"},
	{ID: "stcn-nj", Name: "南京电信云测", Group: "电信", Server: "http://5gnanjing.speedtest.jsinfo.net:8080", Down: "/download", Up: "/upload", Ping: "/hello", Source: "builtin"},
}

// 上游清单镜像模板（i-abc/Speedtest node 分支维护，三源互备：GitHub 原站 + 两个镜像）
var stMirrors = []string{
	"https://raw.githubusercontent.com/i-abc/speedtest/node/%s",
	"https://asset.bash.icu/https://raw.githubusercontent.com/i-abc/speedtest/node/%s",
	"https://cdn.jsdelivr.net/gh/i-abc/Speedtest@node/%s",
}

// 上游清单定义：LibreSpeed JSON（教育网）+ Ookla speedtest-cli 清单（运营商线路）
// + speedtest.cn 节点库 CSV（spiritLHLS/speedtest.cn-CN-ID，每日自动更新；
// CN.csv 为全量超集——2026-10-20 实测三网/广电子集 CSV 无独有 host——
// 只拉这一个文件，按 operator 列归组，不必再拉四个子集 CSV）。
// 运营商 txt 格式："speedtest-cli,电信 天津           ,-o tjrate.tjtele.com"，
// -o 后为 Ookla 服务器域名（默认端口 8080，标准路径 /speedtest/*）。
var stListSources = []struct {
	File    string   // 清单文件名
	Group   string   // 运营商/地区分组（cnid 按 operator 列取组，忽略此字段）
	Kind    string   // librespeed | ookla | cnid
	Mirrors []string // 镜像模板（nil → 默认 stMirrors）
}{
	{"china-education.json", "教育网", "librespeed", nil},
	{"china-telecom-m-thread-v4.txt", "电信", "ookla", nil},
	{"china-unicom-m-thread-v4.txt", "联通", "ookla", nil},
	{"china-mobile-m-thread-v4.txt", "移动", "ookla", nil},
	{"china-other-m-thread-v4.txt", "广电", "ookla", nil},
	{"CN.csv", "", "cnid", []string{
		"https://raw.githubusercontent.com/spiritLHLS/speedtest.cn-CN-ID/main/%s",
		"https://cdn.jsdelivr.net/gh/spiritLHLS/speedtest.cn-CN-ID@main/%s",
	}},
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
	phase    string // idle | ping | down | up | done | error
	progress int    // 当前阶段进度 0-100
	msg      string // 附加信息（当前节点等）
	server   string // 选定节点展示名
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

// refreshRemote 从上游拉取节点清单（教育网 JSON + 三网/广电 Ookla txt + speedtest.cn
// CN.csv，逐清单多镜像容错），随后探活裁剪：去重 → 轻量探活 → 按延迟排序 →
// 电信/联通/移动/广电各保底 1 个 → 裁剪至 ≤20 个写入 s.remote 并缓存。
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
		mirrors := ls.Mirrors
		if mirrors == nil {
			mirrors = stMirrors
		}
		// 单清单：多镜像依次尝试，拿到非空结果即止
		var body []byte
		var lastErr error
		for _, tpl := range mirrors {
			resp, err := client.Get(fmt.Sprintf(tpl, ls.File))
			if err != nil {
				lastErr = err
				continue
			}
			b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
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
			if lastErr != nil {
				errs = append(errs, ls.File+"："+lastErr.Error())
			} else {
				errs = append(errs, ls.File+"：无内容")
			}
			continue
		}
		// 解析为节点（按清单类型）
		var nodes []stNode
		switch ls.Kind {
		case "librespeed":
			nodes = stParseLibrespeed(body, ls.Group)
		case "cnid":
			nodes = stParseCnIDCsv(body)
		default:
			nodes = stParseOoklaTxt(body, ls.Group)
		}
		if len(nodes) == 0 {
			errs = append(errs, ls.File+"：无有效节点")
			continue
		}
		got = append(got, nodes...)
	}
	got = stDedupe(got)

	// 探活裁剪：远程候选全量探活 → 用户策略裁剪（归属地云测优先 → 四运营商
	// 各保底 1 → 云测按延迟填满）→ 总数 ≤20。原始清单动辄数百节点，直接进列表
	// 会拖慢竞速与页面渲染；探活只做 TCP+HTTP 轻量校验，20 并发约几秒完成。
	// 调用点均为异步，可给足预算。
	alive, lats := stProbeBatch(got)
	if len(alive) > 0 {
		alive = stTrimRemote(alive, lats, 20)
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.fetching = false
	if len(alive) > 0 {
		s.remote = alive
		s.remoteAt = time.Now()
		s.saveRemoteLocked()
		if len(errs) > 0 {
			s.fetchErr = fmt.Sprintf("部分清单拉取失败（%d/%d 成功）：%s", len(stListSources)-len(errs), len(stListSources), strings.Join(errs, "; "))
			// 有成功清单时失败清单只提示，不算整体错误——截断避免 toast 过长
			if len(s.fetchErr) > 120 {
				s.fetchErr = s.fetchErr[:120] + "…"
			}
		}
	} else if len(got) > 0 {
		// 清单拉到了但全军覆没（探活全死）：仍写入缓存供列表展示探活状态，
		// 下次 24h 刷新前用户至少能看到节点而不会以为列表为空
		s.remote = got
		s.remoteAt = time.Now()
		s.saveRemoteLocked()
		s.fetchErr = "清单已拉取但探活全部失败，可能是当前网络出口受限"
	} else {
		s.fetchErr = "上游清单拉取失败：" + strings.Join(errs, "; ")
	}
}

// stHostKey 节点去重键的 host 部分：host 归一化（去端口、小写）。Ookla 清单里
// 同一物理服务器常以「X」与「X.prod.hosts.ooklaserver.net」两个别名域名出现
// （后者是前者的 CNAME），折叠后缀避免同机条目占双份名额。
func stHostKey(server string) string {
	h := strings.ToLower(hostFromServer(server))
	if i := strings.LastIndexByte(h, ':'); i > 0 {
		h = h[:i] // 去端口
	}
	return strings.TrimSuffix(h, ".prod.hosts.ooklaserver.net")
}

// stDedupe 同 host:port+下载路径 去重（保留先出现的，来源优先级由调用方保证）。
// host 折叠 Ookla 别名后缀；保留路径维度——同一台服务器上 Ookla 标准路径与
// 云测专用路径（如 /download）是两个独立测速端点，都保留让探活/竞速决策，
// 只折叠「同机同路径」的真重复。
func stDedupe(list []stNode) []stNode {
	seen := map[string]bool{}
	out := make([]stNode, 0, len(list))
	for _, n := range list {
		key := stHostKey(n.Server) + "|" + n.Down
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, n)
	}
	return out
}

// stProbeBatch 并发探活（20 路并发，单节点 ~5s 上限），返回活节点切片及
// 与切片一一对应的延迟值（ms，供排序裁剪用）。
func stProbeBatch(list []stNode) ([]stNode, []float64) {
	if len(list) == 0 {
		return nil, nil
	}
	sem := make(chan struct{}, 20)
	var wg sync.WaitGroup
	results := make([]float64, len(list))
	for i := range list {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			results[i] = stProbeNode(ctx, list[i])
		}(i)
	}
	wg.Wait()
	out := make([]stNode, 0, len(list))
	lats := make([]float64, 0, len(list))
	for i := range list {
		if results[i] <= 0 {
			continue
		}
		n := list[i]
		v := true
		n.Alive = &v
		out = append(out, n)
		lats = append(lats, results[i])
	}
	return out, lats
}

// stCityOf 从节点名提取城市前缀：CN.csv 解析出的名称格式为「城市 sponsor」
// （如「南京 云测节点-CN-3」），取第一个空格前即城市名。非该格式的名称
// （Ookla「电信 天津」、LibreSpeed 校名）返回空——只对云测节点做归属地匹配。
func stCityOf(name string) string {
	name = strings.TrimSpace(name)
	if i := strings.IndexByte(name, ' '); i > 0 {
		return name[:i]
	}
	return ""
}

// stIsYunCe 云测节点判定：speedtest.cn 节点库的 sponsor 均以「云测节点」开头
// （官方测速网服务器），是归属地优先与质量填充的目标集合。
func stIsYunCe(name string) bool {
	return strings.Contains(name, "云测节点")
}

// stTrimRemote 按用户策略（2026-10-20）裁剪至 cap 个，前提全部为探活存活节点：
//  1. 归属地云测优先：宽带归属地（省/市，取自线路识别 region）命中的云测节点
//     全部保留（如南京宽带优先保「南京 云测节点-CN-x」）；
//  2. 四大运营商（电信/联通/移动/广电）各保留延迟最优的 1 个作保底，
//     避免裁剪后某运营商节点全部缺席、竞速无从谈起；
//  3. 其余名额按延迟升序填充其它质量较好的云测节点。
//
// 名额不足时（存活数 < cap）全保留。
func stTrimRemote(list []stNode, lats []float64, cap int) []stNode {
	if len(list) != len(lats) || len(list) <= cap {
		return list
	}
	idx := make([]int, len(list))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return lats[idx[a]] < lats[idx[b]] })

	region := stIsp.snapshotRegion() // 「省 市」，如「江苏 南京」；未识别为空
	keep := map[int]bool{}
	usedHost := map[string]bool{} // 折叠别名后的机器维度，同机不重复入选
	markHost := func(i int) { usedHost[stHostKey(list[i].Server)] = true }
	hostUsed := func(i int) bool { return usedHost[stHostKey(list[i].Server)] }
	// 1) 归属地云测：region 含城市名（或城市名含 region 片段）即命中
	if region != "" {
		for _, i := range idx {
			n := list[i]
			if !stIsYunCe(n.Name) {
				continue
			}
			city := stCityOf(n.Name)
			if city != "" && !hostUsed(i) && (strings.Contains(region, city) || strings.Contains(city, region)) {
				keep[i] = true
				markHost(i)
			}
		}
	}
	// 2) 四大运营商各取延迟最快的一个保底
	bottom := map[string]bool{"电信": false, "联通": false, "移动": false, "广电": false}
	for _, i := range idx {
		g := list[i].Group
		if !bottom[g] && !hostUsed(i) {
			bottom[g] = true
			keep[i] = true
			markHost(i)
		}
	}
	// 3) 其余名额按延迟序填充云测节点（已在 keep 的跳过），再不够用其它节点补
	out := make([]stNode, 0, cap)
	for _, i := range idx {
		if len(out) >= cap {
			break
		}
		if keep[i] && stIsYunCe(list[i].Name) {
			markHost(i)
			out = append(out, list[i])
		}
	}
	// 3b) 其余云测按延迟序填充
	for _, i := range idx {
		if len(out) >= cap {
			break
		}
		if !keep[i] && !hostUsed(i) && stIsYunCe(list[i].Name) {
			keep[i] = true
			markHost(i)
			out = append(out, list[i])
		}
	}
	// 3c) 已入选的非云测（归属地云测外的保底等）按延迟序排入
	for _, i := range idx {
		if len(out) >= cap {
			break
		}
		if keep[i] && !stIsYunCe(list[i].Name) {
			out = append(out, list[i])
		}
	}
	// 3d) 名额仍不满时用其它存活节点补（同机去重）
	for _, i := range idx {
		if len(out) >= cap {
			break
		}
		if !keep[i] && !hostUsed(i) {
			keep[i] = true
			markHost(i)
			out = append(out, list[i])
		}
	}
	return out
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

// stParseCnIDCsv 解析 speedtest.cn 节点库 CSV（spiritLHLS/speedtest.cn-CN-ID，每日更新）。
// 23 列（0 基）：1=active 5=host 6=country_code 8=city 10=operator 15=sponsor
// 19=pingUrl 20=downloadUrl 21=uploadUrl——URL 三列为完整地址（含 scheme+端口+路径），
// 需拆成 base+path。过滤：表头、active!=1、非 CN、IPv6 节点、URL 缺失行。
func stParseCnIDCsv(body []byte) []stNode {
	rd := csv.NewReader(bytes.NewReader(body))
	rd.FieldsPerRecord = -1 // 行字段数容错（个别行 websocketUrl 列可能缺省）
	rd.TrimLeadingSpace = true
	rows, err := rd.ReadAll()
	if err != nil && len(rows) == 0 {
		return nil
	}
	groupOf := func(op string) string {
		for _, g := range []string{"电信", "联通", "移动", "广电"} {
			if strings.Contains(op, g) {
				return g
			}
		}
		if strings.Contains(op, "教育") {
			return "教育网"
		}
		return op
	}
	splitURL := func(raw string) (base, path string) {
		raw = strings.TrimSpace(raw)
		i := strings.Index(raw, "://")
		if i < 0 {
			return "", raw
		}
		slash := strings.Index(raw[i+3:], "/")
		if slash < 0 {
			return raw, "/"
		}
		return raw[:i+3+slash], raw[i+3+slash:]
	}
	var out []stNode
	for _, rec := range rows {
		if len(rec) < 22 {
			continue
		}
		if strings.TrimSpace(rec[0]) == "id" { // 表头
			continue
		}
		if strings.TrimSpace(rec[1]) != "1" || strings.TrimSpace(rec[6]) != "CN" {
			continue
		}
		host := strings.TrimSpace(rec[5])
		pingURL := strings.TrimSpace(rec[19])
		downURL := strings.TrimSpace(rec[20])
		upURL := strings.TrimSpace(rec[21])
		if host == "" || downURL == "" || upURL == "" || pingURL == "" {
			continue
		}
		// IPv6 字面量：host 含 [ 或冒号数 >1（host 列本身可含 ":端口"，单个冒号正常）
		if strings.Contains(host, "[") || strings.Count(host, ":") > 1 {
			continue
		}
		srv, dPath := splitURL(downURL)
		_, uPath := splitURL(upURL)
		_, pPath := splitURL(pingURL)
		if srv == "" {
			continue
		}
		op := strings.TrimSpace(rec[10])
		city := strings.TrimSpace(rec[8])
		sponsor := strings.TrimSpace(rec[15])
		if sponsor == "" {
			sponsor = host
		}
		name := strings.TrimSpace(city + " " + sponsor)
		// 协议分型：speedtest.cn 公共云测（sponsor 以「云测节点」开头）用
		// LibreSpeed 协议（ckSize）；运营商自建节点（如「上海电信」「南京移动-5G」
		// 的本地站点）只认 size 参数，传 ckSize 会返回 Content-Length:0，
		// 表现为下行恒 0、只有上行速度。
		sizeParam := ""
		if !strings.Contains(sponsor, "云测节点") {
			sizeParam = "size"
		}
		out = append(out, stNode{
			ID:        "cnid-" + fmt.Sprintf("%x", fnv32(srv+dPath)),
			Name:      name,
			Group:     groupOf(op),
			Server:    srv,
			Down:      dPath,
			Up:        uPath,
			Ping:      pPath,
			Source:    "remote",
			SizeParam: sizeParam,
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

	// 候选序列：同运营商优先（probes 已按延迟升序，同 ISP 组排前），其余按延迟
	// 跟随。跨网测速点常屏蔽异网访问；而同网节点也可能探活通过但下载不可用，
	// 因此下载失败时沿候选序列自动回退次优节点重测（最多尝试 3 个）。
	cands := make([]stProbe, 0, len(probes))
	inCands := map[string]bool{}
	if myISP := ispNormalize(stIsp.snapshotISP()); myISP != "" && myISP != "未知" {
		for _, p := range probes {
			if ispNormalize(p.node.Group) == myISP {
				cands = append(cands, p)
				inCands[p.node.ID] = true
			}
		}
	}
	for _, p := range probes {
		if !inCands[p.node.ID] {
			cands = append(cands, p)
			inCands[p.node.ID] = true
		}
	}
	maxTry := len(cands)
	if maxTry > 3 {
		maxTry = 3
	}

	// 探活结果缓存给节点列表展示
	stN.rememberAlive(probes, nodes)

	client := &http.Client{
		Timeout:   0,
		Transport: &http.Transport{DisableCompression: true, MaxIdleConnsPerHost: 6},
	}

	for attempt := 0; attempt < maxTry; attempt++ {
		best := cands[attempt]
		fb := ""
		if attempt > 0 {
			fb = "回退重测 · "
		}

		// ---- 阶段 1：延迟/抖动/丢包（对当前节点连续 TCP 握手 12 次） ----
		s.setPhase("ping", 0, fb+"测试延迟（"+best.node.Name+"）")
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

		// ---- 阶段 2：下载（多连接 8s，取后 60% 平均速率）；失败自动回退次优节点 ----
		s.setPhase("down", 0, fb+"下载测速（"+best.node.Name+"）")
		downMbps, downMB, derr := stDownload(ctx, client, best.node, s)
		if derr != nil {
			if ctx.Err() != nil {
				s.fail(ctx, "已中止")
				return
			}
			if attempt+1 < maxTry {
				continue // 沿候选序列回退到下一个节点重测
			}
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
		s.setPhase("up", 0, fb+"上传测速（"+best.node.Name+"）")
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
		break
	}

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
	// 下载端点有效性校验：部分节点 ping 正常、HTTP 200，但下载端点返回
	// Content-Length: 0（服务器没挂载测速文件，如某些地方电信的 /download）。
	// 这类节点只能测上行、下行恒为 0，若放任入选会表现为"只有上行速度"。
	// 用 HEAD 读响应头判定，开销约 40ms，远小于后续真实测速。
	if n.Down != "" {
		if cl, ok := stDownloadContentLen(n); ok && cl == 0 {
			return 0
		}
	}
	return float64(total.Microseconds()) / 1000.0 / float64(tries-fail)
}

// stDownloadContentLen 对下载端点发 HEAD，取 Content-Length。
// ok=false 表示拿不到该头（节点不支持 HEAD 或无此响应头）——此时不做判定，
// 交由真实测速阶段处理，避免误杀可用节点。
func stDownloadContentLen(n stNode) (int64, bool) {
	// 必须用 dlURL（含计费参数）而不是裸路径：运营商自建节点不传 size 时
	// 会返回 Content-Length:0，用裸路径探活会把可用节点误杀。
	u := n.dlURL(0)
	req, err := http.NewRequest(http.MethodHead, u, nil)
	if err != nil {
		return 0, false
	}
	cl := &http.Client{
		Timeout: 2000 * time.Millisecond,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
	if resp.StatusCode >= 400 {
		return 0, false
	}
	v := resp.Header.Get("Content-Length")
	if v == "" {
		return 0, false
	}
	size, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		return 0, false
	}
	return size, true
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
	prevBytes, prevAt := int64(0), start
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
			// 瞬时速率：本采样窗口内增量（与下载阶段同口径，供前端「↑ 当前 X Mbps」）
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
			st.upMB = r1(float64(s) / 1e6)
			st.liveMbps = r1(inst)
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
	mu        sync.Mutex
	ip        string // 请求实际走的出口地址（v4 或 v6）
	ip4       string // IPv4 出口地址（无 v4 出口/探测失败为空）
	ip6       string // IPv6 出口地址（无 v6 出口/探测失败为空）
	isp       string // 联通/电信/移动/广电/教育网/其他…
	region    string // 省 市
	checkedAt time.Time
	checking  bool
}

var stIsp = &stISP{}

// snapshotISP 并发安全读取当前识别到的运营商名（测速竞速同 ISP 优先判断用）。
func (d *stISP) snapshotISP() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.isp
}

// snapshotRegion 并发安全读取当前识别到的归属地「省 市」（节点归属地优先裁剪用）。
func (d *stISP) snapshotRegion() string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.region
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
		"ip": stIsp.ip, "ip4": stIsp.ip4, "ip6": stIsp.ip6, "isp": stIsp.isp, "region": stIsp.region,
		"checked_at": func() string {
			if stIsp.checkedAt.IsZero() {
				return ""
			}
			return stIsp.checkedAt.Format("2006-01-02 15:04:05")
		}(),
	})
}

// detect 双栈识别：分别强制 IPv4 / IPv6 出口各探测一次（请求走哪条栈就返回哪个 IP），
// 缺栈或该栈探测失败时对应字段留空不报错。ISP/归属地主源取「能成功的那一栈」的结果。
func (d *stISP) detect() {
	// 并发探测双栈
	type probeRes struct {
		ip, isp, region string
		ok              bool
	}
	var v4, v6 probeRes
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); v4 = stDetectOne(true) }()
	go func() { defer wg.Done(); v6 = stDetectOne(false) }()
	wg.Wait()

	d.mu.Lock()
	defer d.mu.Unlock()
	d.checkedAt = time.Now()
	// 出口地址分栈落盘
	d.ip4, d.ip6 = v4.ip, v6.ip
	// 主展示 IP：优先探测成功的那一栈（与旧行为一致——请求走哪条算哪条）
	if v4.ok {
		d.ip, d.isp, d.region = v4.ip, v4.isp, v4.region
	} else if v6.ok {
		d.ip, d.isp, d.region = v6.ip, v6.isp, v6.region
	}
	// v4 失败但 v6 成功时补齐 ISP 信息；反之亦然（任一栈拿到 ISP 即用）
	if d.isp == "" {
		if v4.ok || v6.ok {
			r := v4
			if !r.ok {
				r = v6
			}
			d.isp, d.region = r.isp, r.region
		}
		if d.isp == "" {
			d.isp = "未知"
		}
	}
	// 双源皆败：保留旧 IP 字段（若有），只更新时间戳，等下次触发重试
}

// stDetectOne 单栈探测：v4=true 强制 IPv4 出口，false 强制 IPv6。
// 依次尝试 ipip.net（主源，结构化 ISP/归属地）→ ip-api.com（备源，字段直给）。
func stDetectOne(v4 bool) (res struct {
	ip, isp, region string
	ok              bool
}) {
	network := "tcp6"
	if v4 {
		network = "tcp4"
	}
	// 自定义 Transport：DialContext 固定 network，强制该栈出口
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
			return (&net.Dialer{Timeout: 6 * time.Second}).DialContext(ctx, network, addr)
		},
	}
	client := &http.Client{Timeout: 8 * time.Second, Transport: tr}

	// 主源：ipip.net（location 数组：国家/省/市/区县/ISP）
	if req, err := http.NewRequest("GET", "https://myip.ipip.net/json", nil); err == nil {
		if resp, err := client.Do(req); err == nil {
			var j struct {
				Ret  string `json:"ret"`
				Data struct {
					IP       string   `json:"ip"`
					Location []string `json:"location"`
				} `json:"data"`
			}
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			if json.Unmarshal(b, &j) == nil && j.Ret == "ok" && j.Data.IP != "" {
				res.ip = j.Data.IP
				loc := j.Data.Location
				if len(loc) >= 3 {
					res.region = strings.TrimSpace(loc[1]) + " " + strings.TrimSpace(loc[2])
				} else if len(loc) >= 2 {
					res.region = strings.TrimSpace(loc[1])
				}
				if len(loc) >= 5 {
					res.isp = strings.TrimSpace(loc[4])
				}
				if res.isp == "" {
					res.isp = "未知"
				}
				res.ok = true
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
			res.ip = j.Query
			res.isp = strings.TrimSpace(j.ISP)
			if res.isp == "" {
				res.isp = "未知"
			}
			res.region = strings.TrimSpace(j.RegionName) + " " + strings.TrimSpace(j.City)
			res.ok = true
		}
	}
	return
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
