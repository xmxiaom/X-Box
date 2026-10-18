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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
}

var syncStore struct {
	mu    sync.Mutex
	tasks []SyncTask
	state map[string]*SyncState
	busy  map[string]bool
}

const syncMaxFiles = 800000 // 单次扫描上限：百度网盘等挂载常见几十万文件

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
	defer syncStore.mu.Unlock()
	b, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(syncTasksPath(), b, 0o600)
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
	_ = os.WriteFile(syncIndexPath(id), b, 0o600)
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

// syncOnce 执行一次单向同步（源 → 目标）。
func syncOnce(task SyncTask) (SyncState, error) {
	start := time.Now()
	var st SyncState

	excludes := strings.Split(task.Exclude, ",")
	setSyncState(task.ID, func(x *SyncState) { x.Running = true; x.Phase = "scan"; x.Scanning = 0 })
	files, err := scanSource(task.Source, excludes, func(n int) {
		setSyncState(task.ID, func(x *SyncState) { x.Scanning = n })
	})
	if err != nil {
		return st, err
	}
	st.Scanned = len(files)

	// 阶段一：确定待处理清单（索引未命中或内容有变的文件）
	idx := loadSyncIndex(task.ID)
	newIdx := make(map[string]syncIdxEnt, len(files))
	srcSet := make(map[string]bool, len(files))
	var pending []srcEntry
	for _, f := range files {
		srcSet[f.rel] = true
		newIdx[f.rel] = syncIdxEnt{Size: f.size, Mtime: f.mtime.UnixNano()}
		// 索引命中且未变：仍确认目标文件存在（被删/丢失则进入复制阶段恢复）
		if e, ok := idx[f.rel]; ok && indexUnchanged(e, f) {
			if _, serr := os.Stat(filepath.Join(task.Target, f.rel)); serr == nil {
				continue
			}
			log.Printf("[同步] 目标端缺失 %s，将重新复制", f.rel)
		}
		pending = append(pending, f)
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

	saveSyncIndex(task.ID, newIdx)

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
	log.Printf("[同步] 任务 %s（%s）开始", task.ID, task.Name)
	st, err := syncOnce(task)
	if err != nil {
		st.LastErr = err.Error()
		st.LastOK = false
		st.LastAt = time.Now()
	}
	setSyncState(task.ID, func(s *SyncState) { *s = st })
	log.Printf("[同步] 任务 %s 结束：复制 %d · 删除 %d · 扫描 %d · %s",
		task.ID, st.Files, st.Deleted, st.Scanned, st.Duration)
}

// startSyncScheduler 每秒检查一次有没有到期的任务。
func (s *Server) startSyncScheduler() {
	go func() {
		last := map[string]time.Time{}
		for {
			for _, t := range loadSyncTasks() {
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
	tasks := loadSyncTasks()
	out := make([]map[string]any, 0, len(tasks))
	for _, t := range tasks {
		st := syncStateOf(t.ID)
		_, sErr := os.Stat(t.Source)
		_, tErr := os.Stat(t.Target)
		out = append(out, map[string]any{
			"id": t.ID, "name": t.Name, "source": t.Source, "target": t.Target,
			"mirror": t.Mirror, "interval": t.Interval, "exclude": t.Exclude,
			"realtime": t.Realtime, "watchSec": t.WatchSec,
			"enabled": t.Enabled,
			"state":   st,
			"sourceOk": sErr == nil, "targetOk": tErr == nil,
		})
	}
	ok(w, map[string]any{"tasks": out})
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

	tasks := loadSyncTasks()
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
	ok(w, map[string]any{"saved": t})
}

func (s *Server) handleSyncDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	tasks := loadSyncTasks()
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
	for _, t := range loadSyncTasks() {
		if t.ID == id {
			go runSyncTask(t)
			ok(w, map[string]any{"started": id})
			return
		}
	}
	fail(w, 404, "任务不存在")
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
func fmtBytesRate(bps float64) string {
	if bps >= 1024*1024 {
		return fmt.Sprintf("%.1f MB/s", bps/1024/1024)
	}
	return fmt.Sprintf("%.1f KB/s", bps/1024)
}
