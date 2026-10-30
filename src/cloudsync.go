package main

// 网盘同步：远程挂载（飞牛文件管理里的"挂载网盘"）→ 本地目录。
//
// 分工：挂载由飞牛自带文件管理负责（我们只看到普通目录）；本模块负责按
// 修改时间/大小比对差异并增量复制，行为对齐群晖 Cloud Sync 的"单向同步"。
//
// 实现要点：
//   - 纯标准库，不引入第三方同步库；用轮询（默认 60s）而非 inotify，
//     因为远程挂载（WebDAV/云盘 FUSE）的事件通知不可靠。
//   - 逐文件比对 size+mtime，只拷贝有差异的；目录按需创建。
//   - 可选"镜像删除"：源里已删除的文件，目标同步删除（默认关闭，防误删）。
//   - 安全：禁止源与目标互相包含（会无限递归）、禁止同步到挂载点自身之外。
//   - 单次运行有文件数上限，避免首次全量把设备拖死。

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strconv"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type SyncTask struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Source   string `json:"source"`  // 网盘挂载路径（绝对路径）
	Target   string `json:"target"`  // 本地目标目录（绝对路径）
	Mirror   bool   `json:"mirror"`  // 源删除时同步删除目标
	Interval int    `json:"interval"`  // 秒；0 = 仅手动
	Realtime bool   `json:"realtime"`  // 实时变化同步（轮询检测，间隔 WatchSec 秒）
	WatchSec int    `json:"watchSec"`  // 实时检测间隔秒数（默认 2）
	Exclude  string `json:"exclude"`   // 逗号分隔通配，如 *.tmp,.DS_Store
	Enabled  bool   `json:"enabled"`
}

type SyncState struct {
	Running  bool      `json:"running"`
	LastAt   time.Time `json:"lastAt"`
	LastOK   bool      `json:"lastOk"`
	LastErr  string    `json:"lastErr"`
	Files    int       `json:"files"`    // 上次同步复制的文件数
	Bytes    int64     `json:"bytes"`    // 上次同步复制的字节
	Deleted  int       `json:"deleted"`  // 上次删除的文件/空目录数
	Dirs     int       `json:"dirs"`     // 上次创建的目录数
	Scanned  int       `json:"scanned"`  // 上次扫描的文件数
	Total    int       `json:"total"`    // 本轮待处理文件数
	Done     int       `json:"done"`     // 已处理数（运行中实时更新）
	Speed    string    `json:"speed"`    // 运行中实时网速（如 12.3 MB/s）
	Phase      string    `json:"phase"`      // scan=扫描源目录 copy=复制 idle
	Scanning   int       `json:"scanning"`   // 扫描阶段已枚举的条目数
	TotalBytes int64     `json:"totalBytes"` // 本轮待复制总字节
	DoneBytes  int64     `json:"doneBytes"`  // 已复制字节（运行中实时更新）
	Duration string    `json:"duration"` // 上次耗时
	// RefreshHint 挂载目录刷新的诊断结果（如「已 SIGHUP pid=123」/「未找到 rclone 进程」/
	// 「SIGHUP 权限不足」）。挂在状态里是为了让用户不用看系统日志就能在面板上看到原因。
	RefreshHint string `json:"refreshHint"`
}

var syncStore struct {
	mu    sync.Mutex
	tasks []SyncTask
	state map[string]*SyncState
	busy  map[string]bool
}

const syncMaxFiles = 800000 // 单次扫描上限：百度网盘等挂载常见几十万文件

// syncRescanWait 本轮判定「无变化」后，等待多久再重扫一次以跨过远程挂载的目录缓存 TTL
const syncRescanWait = 1200 * time.Millisecond

func syncTasksPath() string {
	dir := os.Getenv("XBOX_DATA")
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "cloud-sync.json")
}

func loadSyncTasks() []SyncTask {
	syncStore.mu.Lock()
	defer syncStore.mu.Unlock()
	b, err := os.ReadFile(syncTasksPath())
	if err != nil {
		return nil
	}
	var out []SyncTask
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func saveSyncTasks(tasks []SyncTask) error {
	syncStore.mu.Lock()
	b, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		syncStore.mu.Unlock()
		return err
	}
	err = os.WriteFile(syncTasksPath(), b, 0o600)
	syncStore.mu.Unlock()
	if err == nil {
		setSyncTasksCache(tasks) // 落盘成功才同步内存，保证两者一致
	}
	return err
}

// 任务列表常驻内存：调度器每秒、面板每 1.5s 轮询都要读任务列表，
// 若每次都打开 cloud-sync.json 解析，等于持续的小文件随机读。只在保存时更新缓存。
var (
	syncTasksMu     sync.Mutex
	syncTasksMem    []SyncTask
	syncTasksLoaded bool
)

// cachedSyncTasks 取任务列表（优先内存；返回副本，调用方可自由修改）
func cachedSyncTasks() []SyncTask {
	syncTasksMu.Lock()
	if !syncTasksLoaded {
		syncTasksMem = loadSyncTasks()
		syncTasksLoaded = true
	}
	out := make([]SyncTask, len(syncTasksMem))
	copy(out, syncTasksMem)
	syncTasksMu.Unlock()
	return out
}

func setSyncTasksCache(tasks []SyncTask) {
	cp := make([]SyncTask, len(tasks))
	copy(cp, tasks)
	syncTasksMu.Lock()
	syncTasksMem = cp
	syncTasksLoaded = true
	syncTasksMu.Unlock()
}

func syncStateOf(id string) SyncState {
	syncStore.mu.Lock()
	defer syncStore.mu.Unlock()
	if syncStore.state == nil {
		syncStore.state = map[string]*SyncState{}
	}
	if st, ok := syncStore.state[id]; ok {
		return *st
	}
	return SyncState{}
}

func setSyncState(id string, f func(*SyncState)) {
	syncStore.mu.Lock()
	defer syncStore.mu.Unlock()
	if syncStore.state == nil {
		syncStore.state = map[string]*SyncState{}
	}
	st, ok := syncStore.state[id]
	if !ok {
		st = &SyncState{}
		syncStore.state[id] = st
	}
	f(st)
}

// ---------- 同步引擎 ----------

type srcEntry struct {
	rel   string
	size  int64
	mtime time.Time
	isDir bool
}

// scanSource 收集源端文件清单（相对路径）。onProg 每 500 项回调一次（可为 nil）。
func scanSource(root string, excludes []string, onProg func(int)) ([]srcEntry, error) {
	var out []srcEntry
	root = filepath.Clean(root)
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 单个文件不可读不致命
		}
		if p == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		if syncExcluded(rel, info.Name(), excludes) {
			if info.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil // 跳过符号链接，避免指到挂载点之外
		}
		if info.IsDir() {
			// 目录也收集：空文件夹也要能同步过去
			out = append(out, srcEntry{rel: rel, size: 0, mtime: info.ModTime(), isDir: true})
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		out = append(out, srcEntry{rel: rel, size: info.Size(), mtime: info.ModTime()})
		if onProg != nil && len(out)%500 == 0 {
			syncYield() // 扫描同样让路
			onProg(len(out))
		}
		if len(out) > syncMaxFiles {
			return errors.New("源目录文件数超过上限（" + fmt.Sprint(syncMaxFiles) + "）。请在「排除规则」里排除不需要的子目录，或减少挂载范围")
		}
		return nil
	})
	return out, err
}

func syncExcluded(rel, name string, excludes []string) bool {
	for _, pat := range excludes {
		pat = strings.TrimSpace(pat)
		if pat == "" {
			continue
		}
		if ok, _ := filepath.Match(pat, name); ok {
			return true
		}
		if ok, _ := filepath.Match(pat, rel); ok {
			return true
		}
		if strings.HasPrefix(rel, strings.TrimSuffix(pat, "/")+"/") {
			return true
		}
	}
	return false
}

func sameFileInfo(src srcEntry, dstPath string) bool {
	st, err := os.Stat(dstPath)
	if err != nil {
		return false
	}
	if !st.Mode().IsRegular() {
		return false
	}
	if st.Size() != src.size {
		return false
	}
	// 远程挂载的时间精度可能不同，留 2 秒容差
	diff := st.ModTime().Sub(src.mtime)
	if diff < 0 {
		diff = -diff
	}
	return diff <= 2*time.Second
}

func copyFileWithMtime(src, dst string, mtime time.Time, onProg func(int64)) (int64, error) {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	in, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer in.Close()
	// 先写临时文件再改名：中途失败不会留下半个文件
	tmp := dst + ".xboxsync.tmp"
	out, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	// 分块复制，回调里上报已写字节（大文件内部也有实时进度）
	var written int64
	buf := make([]byte, 256*1024)
	var cerr error
	for {
		nn, er := in.Read(buf)
		if nn > 0 {
			on, ew := out.Write(buf[:nn])
			written += int64(on)
			if onProg != nil {
				onProg(written)
			}
			if ew != nil {
				cerr = ew
				break
			}
			if er != nil {
				if er != io.EOF {
					cerr = er
				}
				break
			}
			continue
		}
		if er != nil {
			if er != io.EOF {
				cerr = er
			}
			break
		}
	}
	serr := out.Close()
	if cerr != nil || serr != nil {
		_ = os.Remove(tmp)
		if cerr != nil {
			return written, cerr
		}
		return written, serr
	}
	_ = os.Chtimes(tmp, mtime, mtime)
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return written, err
	}
	return written, nil
}

// ---------------- 持久化索引（未变动的文件直接跳过，不做目标端比对） ----------------

type syncIdxEnt struct {
	Size  int64 `json:"s"`
	Mtime int64 `json:"m"` // UnixNano
}

func syncIndexPath(id string) string {
	dir := os.Getenv("XBOX_DATA")
	if dir == "" {
		dir = "."
	}
	d := filepath.Join(dir, "sync")
	_ = os.MkdirAll(d, 0o755)
	return filepath.Join(d, id+".idx")
}

func loadSyncIndex(id string) map[string]syncIdxEnt {
	b, err := os.ReadFile(syncIndexPath(id))
	if err != nil {
		return map[string]syncIdxEnt{}
	}
	out := map[string]syncIdxEnt{}
	if json.Unmarshal(b, &out) != nil {
		return map[string]syncIdxEnt{}
	}
	return out
}

func saveSyncIndex(id string, idx map[string]syncIdxEnt) {
	b, err := json.Marshal(idx)
	if err != nil {
		return
	}
	// 先写临时文件再改名：中途失败不会留下半个索引文件被下次误读
	p := syncIndexPath(id)
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, p)
}

// 索引常驻内存：轮询只读内存，避免每轮都读盘。
// 实时模式 1~2s 一轮，若每轮 loadSyncIndex 都读一次索引文件，虽然文件不大，
// 但持续的小随机读同样会扰动磁盘（机械盘尤其明显），放内存后轮询期间零磁盘 IO。
var (
	syncIdxMu  sync.Mutex
	syncIdxMem = map[string]map[string]syncIdxEnt{}
)

// cachedSyncIndex 取任务索引：命中内存直接返回，未加载过才读盘一次。
// 返回的 map 只读使用（planSync 只读它，新索引写 newIdx），并发安全。
func cachedSyncIndex(id string) map[string]syncIdxEnt {
	syncIdxMu.Lock()
	if m, ok := syncIdxMem[id]; ok {
		syncIdxMu.Unlock()
		return m
	}
	syncIdxMu.Unlock()
	m := loadSyncIndex(id)
	syncIdxMu.Lock()
	syncIdxMem[id] = m
	syncIdxMu.Unlock()
	return m
}

// storeSyncIndex 更新内存索引，persist 为 true 时才落盘。
func storeSyncIndex(id string, idx map[string]syncIdxEnt, persist bool) {
	syncIdxMu.Lock()
	syncIdxMem[id] = idx
	syncIdxMu.Unlock()
	if persist {
		saveSyncIndex(id, idx)
	}
}

// dropSyncIndexCache 任务被删除时清掉内存索引，避免残留占用
func dropSyncIndexCache(id string) {
	syncIdxMu.Lock()
	delete(syncIdxMem, id)
	syncIdxMu.Unlock()
}

// indexUnchanged 判断相对上次索引是否未变（2 秒容差，与目标端比对一致）。
func indexUnchanged(e syncIdxEnt, f srcEntry) bool {
	if f.isDir {
		return true // 目录：只比对"目标是否存在"，mtime 不参与（会随内容变化）
	}
	if e.Size != f.size {
		return false
	}
	d := e.Mtime - f.mtime.UnixNano()
	if d < 0 {
		d = -d
	}
	return d <= int64(2*time.Second)
}

// syncProc 一条进程信息（从 /proc 采集，或测试里伪造）
type syncProc struct {
	pid  int
	args []string
	exe  string // 可执行文件名（小写，取 /proc/<pid>/exe 的 basename）
}

// decideFlushTargets 从进程表里挑出「可以安全 SIGHUP」的 rclone 挂载进程。
// 纯函数：开发机（Windows）也能单测，覆盖 fnOS 的真实命令行形态。
//
// 判定必须严格——SIGHUP 对非 rclone 进程是终止信号，误判会让网盘挂载掉线。
// 规则：命令行必须精确含该挂载点，且满足其一：
//   - 出现 rclone 专属参数（--dir-cache-time / --vfs-cache-mode / --poll-interval / --vfs-cache-max-age）
//   - 可执行名含 rclone
//   - 挂载类型本身是 fuse.rclone 且可执行名含 webdav（fnOS 把 rclone 改名成 webdav）
//
// 返回待发信号的 pid 列表与诊断说明（诊断会显示在面板上）。
func decideFlushTargets(procs []syncProc, mountPoint, fstype string) (pids []int, hint string) {
	isFuseRclone := strings.Contains(strings.ToLower(fstype), "rclone")
	matched := 0
	var seen []string
	for _, p := range procs {
		if p.pid <= 1 || !hasArg(p.args, mountPoint) {
			continue
		}
		matched++
		if len(seen) < 3 {
			seen = append(seen, fmt.Sprintf("pid%d(%s): %s", p.pid, p.exe, strings.Join(p.args, " ")))
		}
		if !isRcloneCmdline(p.args, mountPoint) {
			nameOK := strings.Contains(p.exe, "rclone") ||
				(isFuseRclone && strings.Contains(p.exe, "webdav"))
			if !nameOK {
				continue
			}
		}
		pids = append(pids, p.pid)
	}
	switch {
	case len(pids) > 0:
		return pids, ""
	case !isFuseRclone:
		// 不是 fuse.rclone（本地盘/其他文件系统）：本来就没有 VFS 目录缓存问题
		return nil, "非 rclone 挂载（fstype=" + fstype + "），无 SIGHUP 可用"
	case matched > 0:
		return nil, "找到服务该挂载点的进程但无法确认是 rclone，已跳过（" + strings.Join(seen, " | ") + "）"
	default:
		return nil, "未找到服务 " + mountPoint + " 的挂载进程"
	}
}

func hasArg(args []string, want string) bool {
	if want == "" {
		return false
	}
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

// isRcloneCmdline 判断一组命令行参数是否为服务 mountPoint 的 rclone 挂载进程。
// 纯函数（跨平台共用，便于单测）。
//
// 必须严格：SIGHUP 对非 rclone 进程是「终止」而不是「刷缓存」，误判会导致网盘
// 挂载掉线。因此除「含挂载点参数」外，还要求出现 rclone 专属特征（可执行名含
// rclone，或带 --dir-cache-time / --vfs-cache-mode / --poll-interval 这类 rclone
// 独有参数）。fnOS 把 rclone 重命名成 /usr/trim/bin/webdav，但其挂载命令行仍带
// 这些参数，可据此识别。
func isRcloneCmdline(args []string, mountPoint string) bool {
	hasMount := false
	strong := false
	for _, a := range args {
		low := strings.ToLower(a)
		if mountPoint != "" && a == mountPoint {
			hasMount = true
		}
		if strings.Contains(low, "rclone") {
			strong = true
			continue
		}
		switch {
		case strings.HasPrefix(low, "--dir-cache-time"),
			strings.HasPrefix(low, "--vfs-cache-mode"),
			strings.HasPrefix(low, "--poll-interval"),
			strings.HasPrefix(low, "--vfs-cache-max-age"):
			strong = true
		}
	}
	return strong && hasMount
}

// planSync 依据源清单与上次索引，算出本轮待复制项、新索引与源文件集合。
// verifyTarget 为 false 时跳过「索引命中项在目标端是否存在」的校验：
// 该校验要对每个文件做一次 stat，实时模式高频轮询下会把 dentry 缓存挤出去、
// 退化成真实随机读，长期对磁盘不友好。跳过不影响正确性——目标端被删的文件
// 会在下一次校验轮（由 shouldVerifyTarget 控制）重新复制恢复。
func planSync(task SyncTask, idx map[string]syncIdxEnt, files []srcEntry, verifyTarget bool) (pending []srcEntry, newIdx map[string]syncIdxEnt, srcSet map[string]bool) {
	newIdx = make(map[string]syncIdxEnt, len(files))
	srcSet = make(map[string]bool, len(files))
	for _, f := range files {
		srcSet[f.rel] = true
		newIdx[f.rel] = syncIdxEnt{Size: f.size, Mtime: f.mtime.UnixNano()}
		if e, ok := idx[f.rel]; ok && indexUnchanged(e, f) {
			if !verifyTarget {
				continue
			}
			if _, serr := os.Stat(filepath.Join(task.Target, f.rel)); serr == nil {
				continue
			}
			log.Printf("[同步] 目标端缺失 %s，将重新复制", f.rel)
		}
		pending = append(pending, f)
	}
	return pending, newIdx, srcSet
}

// shouldVerifyTarget 决定本轮是否做目标端存在性校验。
// 普通间隔模式每轮都校验；实时模式每 12 轮（约 24s）校验一次，兼顾正确性与磁盘压力。
func shouldVerifyTarget(task SyncTask) bool {
	if !task.Realtime {
		return true
	}
	syncVerifyMu.Lock()
	defer syncVerifyMu.Unlock()
	syncVerifyCnt[task.ID]++
	return syncVerifyCnt[task.ID]%12 == 1 // 首轮也校验
}

// mountInfoOf 找到包含 p 的最具体挂载点，返回挂载点、文件系统类型、设备名。
func mountInfoOf(p string) (mountPoint, fstype, device string) {
	b, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return "", "", ""
	}
	return parseMountInfo(string(b), p)
}

// parseMountInfo 从 /proc/mounts 文本里找出包含 path 的最具体挂载点。
// 注意：/proc/mounts 里的路径恒为 Unix 风格，必须用 path 包（分隔符固定 "/"）
// 而非 filepath——后者在 Windows 上会把 "/a/b" 变成 "\a\b" 导致匹配失败。
// 抽成纯函数便于单测（开发机无 /proc/mounts）。
func parseMountInfo(content, path2 string) (mountPoint, fstype, device string) {
	clean := path.Clean(path2)
	bestLen := -1
	unesc := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\134`, `\`)
	for _, line := range strings.Split(content, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		mp := path.Clean(unesc.Replace(f[1]))
		if clean == mp || strings.HasPrefix(clean, strings.TrimSuffix(mp, "/")+"/") {
			if len(mp) > bestLen {
				bestLen, mountPoint, fstype, device = len(mp), mp, f[2], f[0]
			}
		}
	}
	return
}

// rcloneWarnMu / rcloneWarnAt 限频记录「rclone rc 不可用」的告警，避免每轮扫描都刷日志
var (
	rcloneWarnMu sync.Mutex
	rcloneWarnAt = map[string]time.Time{}
)

// syncIdleMu / syncIdle 实时模式连续「无变化」轮次计数：
// 用于把深度刷新重扫降到每 30 轮一次，兼顾实时性与性能
var (
	syncIdleMu sync.Mutex
	syncIdle   = map[string]int{}
)

// syncVerifyMu / syncVerifyCnt 实时模式目标端校验轮次计数（每 12 轮校验一次）
var (
	syncVerifyMu  sync.Mutex
	syncVerifyCnt = map[string]int{}
)

// refreshRemoteMount 让远程挂载 daemon 丢弃目录项缓存。
// 关键：像 rclone mount 这类 daemon 自己缓存目录列表（--dir-cache-time 默认 5 分钟），
// 用户态 open+readdir 只会命中它的缓存、拿不到网盘侧的新文件——这也是「实时检测间隔
// 到了却看不到新文件」的根因。必须走 daemon 的控制接口才能真正刷新：rclone 提供
// rc 服务（--rc，默认端口 5572）的 vfs/refresh。返回 true 表示已真正刷新。
func refreshRemoteMount(p string) bool {
	mp, fstype, dev := mountInfoOf(p)
	if mp == "" {
		return false
	}
	low := strings.ToLower(fstype + " " + dev)
	if !strings.Contains(low, "rclone") {
		return false // 非 rclone 挂载（davfs/内核 NFS 等）没有通用刷新接口
	}
	rel := "/"
	if clean := path.Clean(p); clean != mp {
		rel = strings.TrimPrefix(clean, strings.TrimSuffix(mp, "/"))
		if rel == "" {
			rel = "/"
		} else if !strings.HasPrefix(rel, "/") {
			rel = "/" + rel
		}
	}
	payload, _ := json.Marshal(map[string]any{"dir": rel, "recursive": true})
	cl := &http.Client{Timeout: 4 * time.Second}
	for _, addr := range []string{"127.0.0.1:5572", "127.0.0.1:5573", "127.0.0.1:5574"} {
		req, err := http.NewRequest("POST", "http://"+addr+"/vfs/refresh", bytes.NewReader(payload))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := cl.Do(req)
		if err != nil {
			continue
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode == 200 {
			log.Printf("[同步] 已通过 rclone rc(%s) 刷新挂载目录 %s", addr, rel)
			return true
		}
	}
	// rc 不可用：限频提示一次，便于排查「刷不到新文件」的原因
	rcloneWarnMu.Lock()
	if time.Since(rcloneWarnAt[mp]) > 5*time.Minute {
		rcloneWarnAt[mp] = time.Now()
		log.Printf("[同步] 检测到 rclone 挂载 %s，但其 rc 服务不可访问（未启动 --rc，默认端口 5572），"+
			"无法强制刷新目录缓存；网盘新文件可能延迟到 rclone 的 --poll-interval（默认 1 分钟）后才可见", mp)
	}
	rcloneWarnMu.Unlock()
	return false
}

// warmupSource 刷新远程挂载的目录项，确保接下来的扫描能看到网盘侧的最新内容。
// 优先级：
//  1. SIGHUP 刷新 rclone VFS 缓存（官方解法，立即生效，fnOS 远程挂载即此架构）
//  2. rclone rc vfs/refresh（需挂载进程带 --rc，部分场景可用）
//  3. open + readdir 预热兜底（对内核 dentry 缓存有效，对 daemon 自身缓存无效）
// 只预热根目录及有限个一级子目录（扫描本身会全量遍历，此处只求打破缓存、控制开销）。
func warmupSource(root string) string {
	mp, fstype, dev := mountInfoOf(root)
	failHint := ""
	if mp != "" {
		if n, hint, err := flushRcloneVFS(mp, fstype); err == nil && n > 0 {
			time.Sleep(300 * time.Millisecond) // 给 rclone 重新拉取目录列表的时间
			return hint
		} else if hint != "" {
			failHint = hint
		}
	} else {
		failHint = "路径不在任何挂载点内"
	}
	if refreshRemoteMount(root) {
		return "rc vfs/refresh 已刷新" // rc vfs/refresh 已真正刷新缓存
	}
	_ = dev
	const maxDirs = 32
	dirs := []string{root}
	if d, err := os.Open(root); err == nil {
		names, _ := d.Readdirnames(512) // 限制读取条目数，取到前若干项即可
		_ = d.Close()
		for _, n := range names {
			if len(dirs) > maxDirs {
				break
			}
			p := filepath.Join(root, n)
			if fi, serr := os.Stat(p); serr == nil && fi.IsDir() && fi.Mode()&os.ModeSymlink == 0 {
				dirs = append(dirs, p)
			}
		}
	}
	for i := 0; i < 2; i++ {
		for _, d := range dirs {
			if h, err := os.Open(d); err == nil {
				_, _ = h.Readdirnames(64) // 走一遍 readdir 即可触发刷新
				_ = h.Close()
			}
		}
		if i == 0 && len(dirs) > 1 {
			time.Sleep(150 * time.Millisecond) // 给网络文件系统一点刷新时间
		}
	}
	if failHint != "" {
		return "readdir 兜底（" + failHint + "）"
	}
	return "readdir 兜底"
}

// syncOnce 执行一次单向同步（源 → 目标）。
func syncOnce(task SyncTask) (SyncState, error) {
	start := time.Now()
	var st SyncState

	excludes := strings.Split(task.Exclude, ",")
	setSyncState(task.ID, func(x *SyncState) { x.Running = true; x.Phase = "scan"; x.Scanning = 0 })
	// 扫描前打破挂载目录缓存（网盘侧刚变更的文件否则读不到）；同时把刷新结论记进状态，
	// 便于用户在面板上直接看到「为什么没刷到新文件」，不必翻系统日志。
	rh := warmupSource(task.Source)
	st.RefreshHint = rh // 同时写进本地副本：runSyncTask 最后会用 st 整体覆盖共享状态
	setSyncState(task.ID, func(x *SyncState) { x.RefreshHint = rh })
	files, err := scanSource(task.Source, excludes, func(n int) {
		setSyncState(task.ID, func(x *SyncState) { x.Scanning = n })
	})
	if err != nil {
		return st, err
	}
	st.Scanned = len(files)

	// 阶段一：确定待处理清单（索引未命中或内容有变的文件）
	idx := cachedSyncIndex(task.ID) // 只读内存，轮询期间不读盘
	verify := shouldVerifyTarget(task)
	pending, newIdx, srcSet := planSync(task, idx, files, verify)

	// 挂载缓存兜底：本轮判定「无变化」时，可能只是远程挂载的目录项缓存还没过期
	// （网盘侧已新增/修改，readdir 仍返回旧结果）。刷新挂载目录后重扫：
	//   - 普通模式：递进两次（1.2s、3s），跨越秒级 TTL
	//   - 实时模式：本身 1~2s 一轮，若每轮都重扫会让网盘扫描翻倍、明显拖慢；
	//     改为每 30 轮（约 60s）做一次深度刷新重扫，覆盖挂载 daemon 的缓存/poll 周期
	doRescan, waits := false, []time.Duration{syncRescanWait, 3 * time.Second}
	if task.Realtime {
		syncIdleMu.Lock()
		syncIdle[task.ID]++
		doRescan = syncIdle[task.ID]%30 == 0
		syncIdleMu.Unlock()
		waits = []time.Duration{syncRescanWait}
	} else {
		syncIdleMu.Lock()
		syncIdle[task.ID] = 0
		syncIdleMu.Unlock()
		doRescan = true
	}
	if len(pending) == 0 && len(idx) > 0 && doRescan {
		for _, wait := range waits {
			time.Sleep(wait)
			rh2 := warmupSource(task.Source) // 再次刷新挂载目录
			setSyncState(task.ID, func(x *SyncState) { x.RefreshHint = rh2 })
			files2, err2 := scanSource(task.Source, excludes, nil)
			if err2 != nil || len(files2) == 0 {
				continue
			}
			p2, n2, s2 := planSync(task, idx, files2, false)
			if len(p2) == 0 {
				continue
			}
			log.Printf("[同步] %s 刷新挂载目录后重扫，取到 %d 项待复制", task.ID, len(p2))
			files, pending, newIdx, srcSet = files2, p2, n2, s2
			st.Scanned = len(files)
			setSyncState(task.ID, func(x *SyncState) { x.Scanning = len(files) })
			break
		}
	} else if len(pending) > 0 {
		syncIdleMu.Lock()
		syncIdle[task.ID] = 0
		syncIdleMu.Unlock()
	}
	st.Total = len(pending)
	var totalBytes int64
	for _, f := range pending {
		totalBytes += f.size
	}
	st.TotalBytes = totalBytes
	setSyncState(task.ID, func(x *SyncState) {
		x.Total = st.Total; x.Done = 0; x.Running = true; x.Phase = "copy"
		x.TotalBytes = totalBytes; x.DoneBytes = 0
	})

	// 阶段二：逐个复制，实时上报进度与网速
	copyStart := time.Now()
	var doneBytes int64
	for i, f := range pending {
		dst := filepath.Join(task.Target, f.rel)
		if f.isDir {
			// 目录：不存在则创建（含 mtime 不比对的语义，见 indexUnchanged）
			if _, serr := os.Stat(dst); serr != nil {
				if merr := os.MkdirAll(dst, 0o755); merr == nil {
					st.Dirs++
				}
			}
			done := i + 1
			setSyncState(task.ID, func(x *SyncState) { x.Done = done })
			continue
		}
		if sameFileInfo(f, dst) {
			doneBytes += f.size
			done := i + 1
			setSyncState(task.ID, func(x *SyncState) { x.Done = done; x.DoneBytes = doneBytes })
			continue
		}
		localBase := doneBytes
		n, cerr := copyFileWithMtime(filepath.Join(task.Source, f.rel), dst, f.mtime, func(w int64) {
			syncYield() // 动态优先级：面板忙/系统负载高时让路
			setSyncState(task.ID, func(x *SyncState) {
				x.DoneBytes = localBase + w
				// 网速：按已复制字节 / 复制耗时（剔除前 300ms 抖动窗口）
				if el := time.Since(copyStart); el > 300*time.Millisecond && x.DoneBytes > 0 {
					x.Speed = fmtBytesRate(float64(x.DoneBytes) / el.Seconds())
				}
			})
		})
		doneBytes += n
		done := i + 1
		if cerr != nil {
			st.LastErr = "复制失败 " + f.rel + "：" + cerr.Error()
			log.Printf("[同步] %s %v", task.ID, cerr)
		} else {
			st.Files++
			st.Bytes += n
		}
		setSyncState(task.ID, func(x *SyncState) { x.Done = done; x.DoneBytes = doneBytes })
	}

	// 镜像删除（带安全阀）：
	//   1. 首轮（索引为空）绝不删除——远程挂载按需枚举，首扫可能只看到网盘
	//      的一小部分，此时镜像会把目标端文件误删光；
	//   2. 删除项（文件+空目录）超过 max(100, 已复制数×2) 立即中止；
	//   3. 目录按深度降序删除，只有空目录才会真正被移除。
	if task.Mirror && len(idx) > 0 {
		delLimit := st.Files * 2
		if delLimit < 100 {
			delLimit = 100
		}
		aborted := false
		del := func(p, rel string) {
			if aborted {
				return
			}
			if srcSet[rel] || syncExcluded(rel, filepath.Base(rel), excludes) {
				return
			}
			if err := os.Remove(p); err == nil {
				st.Deleted++
				if st.Deleted > delLimit {
					aborted = true
					st.LastErr = fmt.Sprintf("镜像删除中止：本轮删除数(%d)异常超过上限(%d)，已保留剩余文件。若确需删除请再次同步", st.Deleted, delLimit)
				}
			}
		}
		// 文件
		_ = filepath.Walk(task.Target, func(p string, info os.FileInfo, werr error) error {
			if werr != nil || info.IsDir() || p == task.Target {
				return nil
			}
			rel, rerr := filepath.Rel(task.Target, p)
			if rerr != nil {
				return nil
			}
			del(p, rel)
			return nil
		})
		// 空目录（深度降序：子目录先于父目录尝试）
		var dirs []string
		_ = filepath.Walk(task.Target, func(p string, info os.FileInfo, werr error) error {
			if werr == nil && info.IsDir() && p != task.Target {
				dirs = append(dirs, p)
			}
			return nil
		})
		sort.Sort(sort.Reverse(sort.StringSlice(dirs)))
		for _, d := range dirs {
			if aborted {
				break
			}
			rel, rerr := filepath.Rel(task.Target, d)
			if rerr != nil {
				continue
			}
			del(d, rel)
		}
	}

	// 索引只在真正变化时落盘：实时模式 1~2s 一轮，若每轮无条件重写索引文件，
	// 等于每秒刷盘一次（索引大小随文件数增长，可达数百 KB~数 MB），长期是明显的
	// 写放大、影响 SSD 寿命。无待复制项且文件数未变时索引必然等价——
	// indexUnchanged 已逐个校验过 size+mtime，此时只更新内存、跳过写盘是安全无损的。
	if len(pending) > 0 || len(newIdx) != len(idx) {
		storeSyncIndex(task.ID, newIdx, true)
	}

	st.Running = false
	st.LastAt = time.Now()
	st.LastOK = st.LastErr == ""
	st.Duration = time.Since(start).Round(time.Millisecond).String()
	if st.DoneBytes < st.TotalBytes {
		st.DoneBytes = st.TotalBytes
	}
	// 完成后记录平均速度（运行中已由复制循环实时更新瞬时速度）
	if el := time.Since(start).Seconds(); el > 0 && st.Bytes > 0 {
		st.Speed = fmtBytesRate(float64(st.Bytes) / el)
	}
	return st, nil
}

// runSyncTask 带互斥地跑一次，并记录状态。
func runSyncTask(task SyncTask) {
	syncStore.mu.Lock()
	if syncStore.busy == nil {
		syncStore.busy = map[string]bool{}
	}
	if syncStore.busy[task.ID] {
		syncStore.mu.Unlock()
		return
	}
	syncStore.busy[task.ID] = true
	syncStore.mu.Unlock()
	defer func() {
		syncStore.mu.Lock()
		delete(syncStore.busy, task.ID)
		syncStore.mu.Unlock()
	}()

	setSyncState(task.ID, func(s *SyncState) { s.Running = true; s.LastErr = "" })
	st, err := syncOnce(task)
	if err != nil {
		st.LastErr = err.Error()
		st.LastOK = false
		st.LastAt = time.Now()
	}
	setSyncState(task.ID, func(s *SyncState) { *s = st })
	// 仅在有实际动作或出错时记日志：实时模式 1~2s 一轮，若每轮都写「开始/结束」
	// 两条日志，等于持续向日志文件写入（fnOS 会落盘），属于无谓的写放大。
	if st.Files > 0 || st.Deleted > 0 || st.LastErr != "" {
		log.Printf("[同步] 任务 %s 结束：复制 %d · 删除 %d · 扫描 %d · %s",
			task.ID, st.Files, st.Deleted, st.Scanned, st.Duration)
	}
}

// startSyncScheduler 每秒检查一次有没有到期的任务。
func (s *Server) startSyncScheduler() {
	go func() {
		last := map[string]time.Time{}
		for {
			for _, t := range cachedSyncTasks() {
				if !t.Enabled {
					continue
				}
				iv := 2 * time.Second
				if !t.Realtime {
					if t.Interval <= 0 {
						continue
					}
					iv = time.Duration(t.Interval) * time.Second
				} else if t.WatchSec >= 1 {
					iv = time.Duration(t.WatchSec) * time.Second
				}
				if d := time.Since(last[t.ID]); d >= iv {
					// 上一轮还在跑（远程挂载扫描慢，常见）时**不推进触发时间**，
					// 否则每轮都撞上忙碌、每轮都被跳过，任务就永远不再同步。
					// 这里先看一眼是否忙碌：忙则本轮跳过，但保留原触发时间，
					// 扫描一结束立即补跑。
					syncStore.mu.Lock()
					busy := syncStore.busy[t.ID]
					syncStore.mu.Unlock()
					if busy {
						continue
					}
					last[t.ID] = time.Now()
					go runSyncTask(t)
				}
			}
			time.Sleep(time.Second)
		}
	}()
}

// ---------- HTTP 接口 ----------

func (s *Server) handleSyncList(w http.ResponseWriter, r *http.Request) {
	tasks := cachedSyncTasks()
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		st := syncStateOf(t.ID)
		sOk, tOk := cachedPathReachable(t)
		out = append(out, map[string]any{
			"id": t.ID, "name": t.Name, "source": t.Source, "target": t.Target,
			"mirror": t.Mirror, "interval": t.Interval, "exclude": t.Exclude,
			"realtime": t.Realtime, "watchSec": t.WatchSec,
			"enabled": t.Enabled,
			"state":   st,
			"sourceOk": sOk, "targetOk": tOk,
		})
	}
	ok(w, map[string]any{"tasks": out})
}

// 源/目标可达性缓存：面板每 1.5s 轮询都会查一次，而源是网盘远程挂载时
// 一次 stat 可能要几百毫秒甚至更久，每次都实时探测会把面板拖卡。
// 缓存 20 秒，既能让用户及时发现挂载掉线，又不至于高频骚扰远程挂载。
var (
	syncReachMu  sync.Mutex
	syncReachMem = map[string]syncReach{}
)

type syncReach struct {
	at       time.Time
	sourceOk bool
	targetOk bool
}

func cachedPathReachable(t SyncTask) (sourceOk, targetOk bool) {
	key := t.ID + "|" + t.Source + "|" + t.Target
	syncReachMu.Lock()
	if c, ok := syncReachMem[key]; ok && time.Since(c.at) < 20*time.Second {
		syncReachMu.Unlock()
		return c.sourceOk, c.targetOk
	}
	syncReachMu.Unlock()

	_, sErr := os.Stat(t.Source)
	_, tErr := os.Stat(t.Target)
	sourceOk, targetOk = sErr == nil, tErr == nil
	syncReachMu.Lock()
	syncReachMem[key] = syncReach{at: time.Now(), sourceOk: sourceOk, targetOk: targetOk}
	syncReachMu.Unlock()
	return sourceOk, targetOk
}

func (s *Server) handleSyncSave(w http.ResponseWriter, r *http.Request) {
	var t SyncTask
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		fail(w, 400, "参数解析失败")
		return
	}
	t.ID = strings.TrimSpace(t.ID)
	t.Name = strings.TrimSpace(t.Name)
	t.Source = filepath.Clean(strings.TrimSpace(t.Source))
	t.Target = filepath.Clean(strings.TrimSpace(t.Target))
	t.Exclude = strings.TrimSpace(t.Exclude)
	if t.ID == "" {
		t.ID = fmt.Sprintf("s%d", time.Now().UnixNano())
	}
	if t.Name == "" {
		t.Name = filepath.Base(t.Target)
	}
	if !filepath.IsAbs(t.Source) || !filepath.IsAbs(t.Target) {
		fail(w, 400, "源与目标都必须是绝对路径")
		return
	}
	if t.Source == t.Target {
		fail(w, 400, "源与目标不能是同一个目录")
		return
	}
	// 互相包含会无限递归（源是目标的子目录或反之）
	if strings.HasPrefix(t.Target, t.Source+string(os.PathSeparator)) ||
		strings.HasPrefix(t.Source, t.Target+string(os.PathSeparator)) {
		fail(w, 400, "源与目标不能互相包含")
		return
	}
	if fi, err := os.Stat(t.Source); err != nil || !fi.IsDir() {
		fail(w, 400, "源目录不存在或不是目录："+t.Source)
		return
	}
	if t.Interval < 0 {
		t.Interval = 0
	}
	if t.WatchSec < 1 || t.WatchSec > 3600 {
		t.WatchSec = 2
	}

	tasks := cachedSyncTasks()
	found := false
	for i := range tasks {
		if tasks[i].ID == t.ID {
			tasks[i] = t
			found = true
		}
	}
	if !found {
		tasks = append(tasks, t)
	}
	if err := saveSyncTasks(tasks); err != nil {
		fail(w, 500, "保存失败："+err.Error())
		return
	}
	// 源/目标可能被改过，丢掉内存索引让下一轮从磁盘重新加载，避免用旧缓存比对
	dropSyncIndexCache(t.ID)
	ok(w, map[string]any{"saved": t})
}

func (s *Server) handleSyncDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	tasks := cachedSyncTasks()
	kept := make([]SyncTask, 0, len(tasks))
	found := false
	for _, t := range tasks {
		if t.ID == id {
			found = true
			continue
		}
		kept = append(kept, t)
	}
	if !found {
		fail(w, 404, "任务不存在")
		return
	}
	if err := saveSyncTasks(kept); err != nil {
		fail(w, 500, "保存失败："+err.Error())
		return
	}
	syncStore.mu.Lock()
	delete(syncStore.state, id)
	syncStore.mu.Unlock()
	dropSyncIndexCache(id) // 同时清掉内存索引，避免残留
	_ = os.Remove(syncIndexPath(id))
	ok(w, map[string]any{"deleted": id})
}

func (s *Server) handleSyncRun(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	syncStore.mu.Lock()
	busy := syncStore.busy[id]
	syncStore.mu.Unlock()
	if busy {
		fail(w, 409, "上一轮同步仍在进行中（大网盘扫描可能需要几分钟），请等它完成后再试")
		return
	}
	for _, t := range cachedSyncTasks() {
		if t.ID == id {
			go runSyncTask(t)
			ok(w, map[string]any{"started": id})
			return
		}
	}
	fail(w, 404, "任务不存在")
}

// handleSyncPause 切换任务的暂停/继续（Enabled 的便捷切换，配置与索引保留）。
func (s *Server) handleSyncPause(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	on := r.URL.Query().Get("on") == "1"
	tasks := cachedSyncTasks()
	found := false
	for i := range tasks {
		if tasks[i].ID == id {
			tasks[i].Enabled = on
			found = true
			break
		}
	}
	if !found {
		fail(w, 404, "任务不存在")
		return
	}
	if err := saveSyncTasks(tasks); err != nil {
		fail(w, 500, "保存失败："+err.Error())
		return
	}
	ok(w, map[string]any{"id": id, "paused": !on})
}

// handleSyncDirs 简单的目录候选（便于前端提示当前卷）
func (s *Server) handleSyncDirs(w http.ResponseWriter, r *http.Request) {
	out := []string{}
	if ents, err := os.ReadDir("/vol1"); err == nil {
		for _, e := range ents {
			if e.IsDir() && !strings.HasPrefix(e.Name(), "@") {
				out = append(out, filepath.Join("/vol1", e.Name()))
			}
			if len(out) >= 20 {
				break
			}
		}
	}
	ok(w, map[string]any{"dirs": out})
}

// fmtBytesRate 速度固定用 KB/s 或 MB/s 显示（用户偏好，不用 B/s / GB/s）。
// ---------------- 动态优先级：面板忙/系统负载高时，同步自动让路 ----------------

var panelLastReq int64 // 最近一次面板请求的 UnixNano（atomic）

// panelActivity HTTP 入口包装：为每个面板请求打活跃度时间戳。
func panelActivity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.StoreInt64(&panelLastReq, time.Now().UnixNano())
		next.ServeHTTP(w, r)
	})
}

// systemLoadHigh 读取 /proc/loadavg，1 分钟负载超过核数×0.8 视为系统忙。
func systemLoadHigh() bool {
	b, err := os.ReadFile("/proc/loadavg")
	if err != nil {
		return false
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return false
	}
	l1, perr := strconv.ParseFloat(f[0], 64)
	if perr != nil {
		return false
	}
	return l1 > float64(runtime.NumCPU())*0.8
}

var syncLastYield int64 // 上次让路时间（atomic，限制让路频率）

// syncYield 动态优先级：面板 2 秒内有请求，或系统负载偏高时，
// 同步让出 120ms。空闲时立即返回（全速）。至少间隔 120ms 才让一次。
func syncYield() {
	now := time.Now().UnixNano()
	last := atomic.LoadInt64(&syncLastYield)
	if now-last < int64(120*time.Millisecond) {
		return
	}
	// 检查更新 / 后台下载 fpk 进行中：都需要外网带宽，同步优先让路
	if atomic.LoadInt64(&updateInProgress) == 1 || atomic.LoadInt64(&downloadInProgress) == 1 {
		atomic.StoreInt64(&syncLastYield, now)
		time.Sleep(150 * time.Millisecond)
		return
	}
	// 面板忙让路窗口必须短于前端同步页轮询间隔（1.5s），
	// 否则每次轮询都把窗口续满，同步会一直让路、速度被压到极低
	panelBusy := now-atomic.LoadInt64(&panelLastReq) < int64(800*time.Millisecond)
	if panelBusy {
		atomic.StoreInt64(&syncLastYield, now)
		time.Sleep(120 * time.Millisecond)
		return
	}
	if systemLoadHigh() {
		atomic.StoreInt64(&syncLastYield, now)
		time.Sleep(80 * time.Millisecond)
	}
}

func fmtBytesRate(bps float64) string {
	if bps >= 1024*1024 {
		return fmt.Sprintf("%.1f MB/s", bps/1024/1024)
	}
	return fmt.Sprintf("%.1f KB/s", bps/1024)
}
