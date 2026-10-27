package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)
// ============ 进程 ============

type ProcEntry struct {
	PID      int     `json:"pid"`
	Name     string  `json:"name"`
	User     string  `json:"user"`
	State    string  `json:"state"`
	CPU      float64 `json:"cpuPercent"`
	MemKB    int64   `json:"memKB"`
	MemPct   float64 `json:"memPercent"`
	Threads  int     `json:"threads"`
	Cmdline  string  `json:"cmdline"`
	StartedAgo int64 `json:"startedSeconds"` // 进程存活秒数
	Kernel   bool   `json:"kernel,omitempty"` // 内核线程（无用户态 cmdline）
	App      string `json:"app,omitempty"`    // 所属 fnOS 应用名（能关联到时）
}

func (s *Server) handleProcesses(w http.ResponseWriter, r *http.Request) {
	users := loadPasswdUsers()
	memTotalKB := readMemTotalKB()

	// CPU%：两次采样取差值。采样只读 /proc/<pid>/stat（很轻），
	// 详情（comm/user/cmdline）只读一次，保证采样窗口接近设定值。
	sample1 := readProcStatSample()
	tA := time.Now()
	time.Sleep(250 * time.Millisecond)
	tB := time.Now()
	sample2 := readProcStatSample()
	window := tB.Sub(tA).Seconds()
	if window < 0.05 {
		window = 0.05
	}

	upSec := readUptimeSec()
	appOf := appNameByPID()

	var procs []ProcEntry
	for pid, cur := range sample2 {
		prev, has := sample1[pid]
		if !has {
			prev = cur // 采样窗口内新起的进程：CPU 记 0
		}
		procs = append(procs, buildProcEntry(pid, users, memTotalKB, cur, prev, upSec, window, appOf))
	}
	// 采样间隙消失的进程（短命进程）：仍列出来，CPU 记 0
	for pid, prev := range sample1 {
		if _, has := sample2[pid]; has {
			continue
		}
		procs = append(procs, buildProcEntry(pid, users, memTotalKB, prev, prev, upSec, window, appOf))
	}

	sort.Slice(procs, func(i, j int) bool { return procs[i].CPU > procs[j].CPU })

	ok(w, map[string]any{
		"processes":   procs,
		"memTotalKB":  memTotalKB,
		"processCount": len(procs),
		"cpuCount":     runtime.NumCPU(),
		"windowMs":     int(window * 1000),
	})
}

// procStat 只保留计算用到的 /proc/<pid>/stat 字段。
type procStat struct {
	utime, stime, starttime float64
	rss                     int64
	threads                 int
	state                   string
}

const procClockTicks = 100.0 // _SC_CLK_TCK 在 Linux 上恒为 100

// readProcStatSample 采样一次全部进程的 stat（不含 cmdline 等慢字段）。
func readProcStatSample() map[int]procStat {
	out := make(map[int]procStat, 256)
	pids, _ := filepath.Glob("/proc/[0-9]*")
	for _, pdir := range pids {
		pid, err := strconv.Atoi(filepath.Base(pdir))
		if err != nil {
			continue
		}
		st, err := os.ReadFile(filepath.Join(pdir, "stat"))
		if err != nil {
			continue
		}
		cs, ok := parseProcStat(string(st))
		if !ok {
			continue
		}
		out[pid] = cs
	}
	return out
}

// parseProcStat 解析 /proc/<pid>/stat。comm（第 2 字段）可能含空格与括号，
// 必须从最后一个 ')' 之后开始切字段——用它之后的字段下标做偏移。
func parseProcStat(s string) (procStat, bool) {
	rp := strings.LastIndex(s, ")")
	if rp < 0 || len(s) < rp+2 {
		return procStat{}, false
	}
	fields := strings.Fields(s[rp+2:])
	// fields[0] = state（原始第 3 字段）；第 22 个字段 = rss（原始第 24 字段）
	if len(fields) < 22 {
		return procStat{}, false
	}
	var cs procStat
	cs.state = fields[0]
	cs.utime, _ = strconv.ParseFloat(fields[11], 64)
	cs.stime, _ = strconv.ParseFloat(fields[12], 64)
	cs.threads, _ = strconv.Atoi(fields[17])
	cs.starttime, _ = strconv.ParseFloat(fields[19], 64)
	rssPages, _ := strconv.ParseInt(fields[21], 10, 64)
	cs.rss = rssPages * 4 // 4KB 页（getconf PAGESIZE 绝大多数为 4096）
	return cs, true
}

// buildProcEntry 组装单条进程信息。window 为两次采样的真实间隔秒数——
// 早期实现固定按 25 个 tick（=250ms）换算，采样耗时会让窗口变长从而导致 CPU% 系统性高估。
func buildProcEntry(pid int, users map[int]string, memTotalKB int64, cur, prev procStat, upSec, window float64, appOf map[int]string) ProcEntry {
	delta := (cur.utime + cur.stime) - (prev.utime + prev.stime)
	if delta < 0 {
		delta = 0
	}
	// delta(ticks) / procClockTicks = 占用 CPU 秒数；除以窗口秒数即百分比
	cpu := delta / procClockTicks / window * 100.0
	if cpu < 0 {
		cpu = 0
	}
	memPct := 0.0
	if memTotalKB > 0 {
		memPct = float64(cur.rss) / float64(memTotalKB) * 100
	}
	// 进程已运行秒数 = 系统运行秒数 - 进程启动时刻（starttime 单位为 100Hz 时钟滴答）
	startedAgo := int64(upSec - cur.starttime/procClockTicks)
	if startedAgo < 0 {
		startedAgo = 0
	}
	pdir := "/proc/" + strconv.Itoa(pid)
	name, user, cmdline := readProcDetail(pdir)
	return ProcEntry{
		PID: pid, Name: name, User: user, State: cur.state,
		CPU: cpu, MemKB: cur.rss, MemPct: memPct, Threads: cur.threads,
		Cmdline: cmdline, StartedAgo: startedAgo,
		// 内核线程没有用户态命令行
		Kernel: cmdline == "",
		App:    appOf[pid],
	}
}

// readUptimeSec 系统运行秒数（用于计算进程已运行时长）。
func readUptimeSec() float64 {
	b, err := os.ReadFile("/proc/uptime")
	if err != nil {
		return 0
	}
	fs := strings.Fields(string(b))
	if len(fs) == 0 {
		return 0
	}
	v, _ := strconv.ParseFloat(fs[0], 64)
	return v
}

// handleProcKill 结束指定进程（SIGTERM）。init（PID 1）与 X-Box 自身禁止结束。
func (s *Server) handleProcKill(w http.ResponseWriter, r *http.Request) {
	var req struct {
		PID int `json:"pid"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.PID <= 0 {
		fail(w, 400, "参数错误")
		return
	}
	if req.PID == 1 || req.PID == os.Getpid() {
		fail(w, 400, "系统关键进程禁止结束")
		return
	}
	proc, err := os.FindProcess(req.PID)
	if err != nil {
		fail(w, 400, "进程不存在")
		return
	}
	if err := proc.Signal(syscall.Signal(0)); err != nil {
		fail(w, 400, "进程不存在或无权操作")
		return
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		fail(w, 500, "发送结束信号失败: "+err.Error())
		return
	}
	ok(w, map[string]any{"message": fmt.Sprintf("已向 PID %d 发送结束信号（SIGTERM）", req.PID)})
}

func readProcDetail(pdir string) (name, user, cmdline string) {
	name = readComm(pdir)
	if st, err := os.ReadFile(filepath.Join(pdir, "status")); err == nil {
		for _, line := range strings.Split(string(st), "\n") {
			if v, ok := strings.CutPrefix(line, "Uid:"); ok {
				fs := strings.Fields(v)
				if len(fs) > 0 {
					if uid, err := strconv.Atoi(fs[0]); err == nil {
						user = cachedPasswdUsers()[uid]
					}
				}
				break
			}
		}
	}
	if b, err := os.ReadFile(filepath.Join(pdir, "cmdline")); err == nil && len(b) > 0 {
		cmdline = strings.ReplaceAll(string(b), "\x00", " ")
		cmdline = strings.TrimSpace(cmdline)
		if len(cmdline) > 220 {
			cmdline = cmdline[:220] + "…"
		}
	}
	if user == "" {
		user = "-"
	}
	return
}

func readMemTotalKB() int64 {
	f, err := os.Open("/proc/meminfo")
	if err != nil {
		return 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
			fields := strings.Fields(v)
			if len(fields) >= 1 {
				n, _ := strconv.ParseInt(fields[0], 10, 64)
				return n
			}
		}
	}
	return 0
}

// ============ 系统服务 ============

type ServiceEntry struct {
	Unit  string `json:"unit"`
	Load  string `json:"load"`
	Active string `json:"active"`
	Sub   string `json:"sub"`
	Desc  string `json:"desc"`
}

func (s *Server) handleServices(w http.ResponseWriter, r *http.Request) {
	var services []ServiceEntry
	var fnosApps []FnosApp
	var dockerContainers []DockerContainer

	// systemd 服务
	services = listSystemdServices()

	// fnOS 应用（程序/数据目录合并 + 进程判活）
	fnosApps = listFnosAppsCached()

	// Docker（存在才展示）
	dockerContainers = listDockerContainers()

	ok(w, map[string]any{
		"services":   services,
		"apps":       fnosApps,
		"docker":     dockerContainers,
		"hasDocker":  dockerContainers != nil,
	})
}

func listSystemdServices() []ServiceEntry {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	outBytes, err := execCommand(ctx, "systemctl", "list-units", "--type=service", "--all", "--no-pager", "--no-legend")
	if err != nil {
		return nil
	}
	var out []ServiceEntry
	for _, line := range strings.Split(string(outBytes), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// systemctl 输出为多空格对齐的表格，按单空格 SplitN 会把空串切进字段、
		// 导致 Active/Sub 错位（如 Active 拿到 "loaded"）。改用空白折叠切列，
		// 描述为第 4 列之后的所有 token 重新拼接（描述内的连续空格折叠为单个，可接受）。
		f := strings.Fields(line)
		// 异常状态单元行首带 "●" 标记，去掉后列位才对齐，否则 Unit 名变成 "●"
		if len(f) > 0 && f[0] == "●" {
			f = f[1:]
		}
		if len(f) < 4 {
			continue
		}
		desc := ""
		if len(f) > 4 {
			desc = strings.Join(f[4:], " ")
		}
		out = append(out, ServiceEntry{
			Unit: f[0], Load: f[1], Active: f[2], Sub: f[3], Desc: desc,
		})
	}
	// 活跃的排前面
	sort.SliceStable(out, func(i, j int) bool {
		return (out[i].Active == "active") && out[j].Active != "active"
	})
	return out
}

// FnosApp fnOS 应用：程序/数据目录 + 是否有进程在运行
type FnosApp struct {
	Name    string `json:"name"`
	Path    string `json:"path"`
	Running bool   `json:"running"`
	PIDs    []int  `json:"pids"`
}

// fnosAppCache 缓存应用清单：进程页每 3s 拉一次，扫描 /proc 读 cmdline/cwd/exe 不便宜，
// 5s 内的重复调用直接复用（服务页与进程页共用一份）。
var fnosAppCache struct {
	mu   sync.Mutex
	at   time.Time
	apps []FnosApp
}

const fnosAppCacheTTL = 5 * time.Second

func listFnosAppsCached() []FnosApp {
	fnosAppCache.mu.Lock()
	defer fnosAppCache.mu.Unlock()
	if fnosAppCache.apps != nil && time.Since(fnosAppCache.at) < fnosAppCacheTTL {
		return fnosAppCache.apps
	}
	apps := listFnosApps()
	fnosAppCache.at = time.Now()
	fnosAppCache.apps = apps
	return apps
}

// appNameByPID 建立 PID → fnOS 应用名映射，
// 让进程列表能标出「这是哪个应用的进程」，避免用户对着守护进程的 S 状态猜它是不是在跑。
func appNameByPID() map[int]string {
	apps := listFnosAppsCached()
	out := make(map[int]string, 64)
	for _, a := range apps {
		for _, pid := range a.PIDs {
			if _, exists := out[pid]; !exists {
				out[pid] = a.Name
			}
		}
	}
	return out
}

// listFnosApps 汇总 fnOS 应用清单与运行状态。
// fnOS 的 fpk 应用分两处存放，二者无包含关系：
//   - 程序目录：/usr/local/apps/<名>/（/var/apps/<名> 多为其符号链接）
//   - 数据目录：/vol*/@appdata/<名>/ 或 /usr/local/apps/@appdata/<名>/
//
// 旧实现只扫数据目录、且只按「cmdline 含 /@appdata/<名>/」判活，
// 导致装在程序目录的应用整条缺失、运行中的应用被误判为未运行。
// 现合并两类目录建清单，判活改为三重匹配：
// cmdline 命中路径别名 / cwd 落在应用目录下 / exe 落在应用目录下。
func listFnosApps() []FnosApp {
	type appInfo struct {
		name    string   // 首次出现的原始名
		progDir string   // 程序目录
		dataDir string   // 数据目录
		markers []string // 判活用路径前缀（均带尾斜杠）
		running bool
		pids    []int
	}
	apps := map[string]*appInfo{}
	get := func(name string) *appInfo {
		k := strings.ToLower(name)
		a, ok := apps[k]
		if !ok {
			a = &appInfo{name: name}
			apps[k] = a
		}
		return a
	}
	add := func(dir string, prog bool) {
		name := filepath.Base(dir)
		if name == "" || strings.HasPrefix(name, ".") || strings.HasPrefix(name, "@") {
			return // @appdata 等非应用目录
		}
		a := get(name)
		if prog {
			if a.progDir == "" {
				a.progDir = dir
			}
		} else if a.dataDir == "" {
			a.dataDir = dir
		}
	}

	// 程序目录（/usr/local/apps 优先；/var/apps 符号链接按名去重）
	if dirs, err := filepath.Glob("/usr/local/apps/*"); err == nil {
		for _, d := range dirs {
			add(d, true)
		}
	}
	if dirs, err := filepath.Glob("/var/apps/*"); err == nil {
		for _, d := range dirs {
			add(d, true)
		}
	}
	// 数据目录
	if dirs, err := filepath.Glob("/vol*/@appdata/*"); err == nil {
		for _, d := range dirs {
			add(d, false)
		}
	}
	if dirs, err := filepath.Glob("/usr/local/apps/@appdata/*"); err == nil {
		for _, d := range dirs {
			add(d, false)
		}
	}

	// 每个应用的判活标记：程序目录 + /var/apps 别名 + 数据目录
	for _, a := range apps {
		if a.progDir != "" {
			a.markers = append(a.markers, a.progDir+"/", "/var/apps/"+a.name+"/")
		}
		if a.dataDir != "" {
			a.markers = append(a.markers, a.dataDir+"/")
		}
	}

	// 扫描 /proc，按标记判活
	pids, _ := filepath.Glob("/proc/[0-9]*")
	for _, pdir := range pids {
		pid, _ := strconv.Atoi(filepath.Base(pdir))
		cmdline := ""
		if b, err := os.ReadFile(filepath.Join(pdir, "cmdline")); err == nil {
			cmdline = strings.ReplaceAll(string(b), "\x00", " ")
		}
		cwd, _ := os.Readlink(filepath.Join(pdir, "cwd"))
		exe, _ := os.Readlink(filepath.Join(pdir, "exe"))
		for _, a := range apps {
			hit := false
			for _, m := range a.markers {
				dir := strings.TrimSuffix(m, "/")
				if strings.Contains(cmdline, m) ||
					(cwd != "" && (cwd == dir || strings.HasPrefix(cwd, m))) ||
					(exe != "" && strings.HasPrefix(exe, m)) {
					hit = true
					break
				}
			}
			if hit {
				a.running = true
				a.pids = append(a.pids, pid)
			}
		}
	}

	out := make([]FnosApp, 0, len(apps))
	for _, a := range apps {
		app := FnosApp{Name: a.name, Running: a.running}
		if a.dataDir != "" {
			app.Path = a.dataDir // 有数据目录时优先展示数据目录（沿用旧展示）
		} else {
			app.Path = a.progDir
		}
		app.PIDs = a.pids
		if len(app.PIDs) > 1 {
			sort.Ints(app.PIDs)
		}
		out = append(out, app)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Running != out[j].Running {
			return out[i].Running
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// DockerContainer Docker 容器（仅展示；X-Box 自身不做 Docker 管理）
type DockerContainer struct {
	Names  string `json:"names"`
	Image  string `json:"image"`
	Status string `json:"status"`
	Ports  string `json:"ports"`
}

func listDockerContainers() []DockerContainer {
	if _, err := os.Stat("/usr/bin/docker"); err != nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	outBytes, err := execCommand(ctx, "/usr/bin/docker", "ps", "--format", "{{.Names}}\t{{.Image}}\t{{.Status}}\t{{.Ports}}")
	if err != nil {
		return nil
	}
	var out []DockerContainer
	for _, line := range strings.Split(string(outBytes), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Split(line, "\t")
		c := DockerContainer{}
		if len(f) > 0 {
			c.Names = f[0]
		}
		if len(f) > 1 {
			c.Image = f[1]
		}
		if len(f) > 2 {
			c.Status = f[2]
		}
		if len(f) > 3 {
			c.Ports = f[3]
		}
		out = append(out, c)
	}
	return out
}
