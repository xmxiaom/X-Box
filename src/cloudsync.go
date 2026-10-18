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
	Interval int    `json:"interval"` // 秒；0 = 仅手动
	Exclude  string `json:"exclude"` // 逗号分隔通配，如 *.tmp,.DS_Store
	Enabled  bool   `json:"enabled"`
}

type SyncState struct {
	Running  bool      `json:"running"`
	LastAt   time.Time `json:"lastAt"`
	LastOK   bool      `json:"lastOk"`
	LastErr  string    `json:"lastErr"`
	Files    int       `json:"files"`    // 上次同步复制的文件数
	Bytes    int64     `json:"bytes"`    // 上次同步复制的字节
	Deleted  int       `json:"deleted"`  // 上次删除的文件数
	Scanned  int       `json:"scanned"`  // 上次扫描的文件数
	Duration string    `json:"duration"` // 上次耗时
}

var syncStore struct {
	mu    sync.Mutex
	tasks []SyncTask
	state map[string]*SyncState
	busy  map[string]bool
}

const syncMaxFiles = 200000 // 单次扫描上限，防首次全量拖死

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
}

// scanSource 收集源端文件清单（相对路径）。
func scanSource(root string, excludes []string) ([]srcEntry, error) {
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
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		out = append(out, srcEntry{rel: rel, size: info.Size(), mtime: info.ModTime()})
		if len(out) > syncMaxFiles {
			return errors.New("文件数超过上限（" + fmt.Sprint(syncMaxFiles) + "），已中止扫描")
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

func copyFileWithMtime(src, dst string, mtime time.Time) (int64, error) {
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
	n, cerr := io.Copy(out, in)
	serr := out.Close()
	if cerr != nil || serr != nil {
		_ = os.Remove(tmp)
		if cerr != nil {
			return n, cerr
		}
		return n, serr
	}
	_ = os.Chtimes(tmp, mtime, mtime)
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return n, err
	}
	return n, nil
}

// syncOnce 执行一次单向同步（源 → 目标）。
func syncOnce(task SyncTask) (SyncState, error) {
	start := time.Now()
	var st SyncState

	excludes := strings.Split(task.Exclude, ",")
	files, err := scanSource(task.Source, excludes)
	if err != nil {
		return st, err
	}
	st.Scanned = len(files)

	srcSet := make(map[string]bool, len(files))
	for _, f := range files {
		srcSet[f.rel] = true
		dst := filepath.Join(task.Target, f.rel)
		if sameFileInfo(f, dst) {
			continue
		}
		n, cerr := copyFileWithMtime(filepath.Join(task.Source, f.rel), dst, f.mtime)
		if cerr != nil {
			st.LastErr = "复制失败 " + f.rel + "：" + cerr.Error()
			log.Printf("[同步] %s %v", task.ID, cerr)
			continue
		}
		st.Files++
		st.Bytes += n
	}

	// 镜像删除：目标多出来的文件删掉（源里已不存在）
	if task.Mirror {
		_ = filepath.Walk(task.Target, func(p string, info os.FileInfo, werr error) error {
			if werr != nil || info.IsDir() || p == task.Target {
				return nil
			}
			rel, rerr := filepath.Rel(task.Target, p)
			if rerr != nil || srcSet[rel] {
				return nil
			}
			if syncExcluded(rel, info.Name(), excludes) {
				return nil
			}
			if err := os.Remove(p); err == nil {
				st.Deleted++
			}
			return nil
		})
	}

	st.Running = false
	st.LastAt = time.Now()
	st.LastOK = st.LastErr == ""
	st.Duration = time.Since(start).Round(time.Millisecond).String()
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
}

// startSyncScheduler 每秒检查一次有没有到期的任务。
func (s *Server) startSyncScheduler() {
	go func() {
		last := map[string]time.Time{}
		for {
			for _, t := range loadSyncTasks() {
				if !t.Enabled || t.Interval <= 0 {
					continue
				}
				if d := time.Since(last[t.ID]); d >= time.Duration(t.Interval)*time.Second {
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
	ok(w, map[string]any{"deleted": id})
}

func (s *Server) handleSyncRun(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
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
