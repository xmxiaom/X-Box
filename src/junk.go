package main

import (
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ============ 分类定义 ============

type JunkCategory struct {
	ID         string   `json:"id"`
	Name       string   `json:"name"`
	Desc       string   `json:"desc"`
	Paths      []string `json:"paths"`
	Safe       bool     `json:"safe"`       // true=安全 false=需注意
	MinAgeDays int      `json:"minAgeDays"` // 0 = 不限
	Unit       string   `json:"unit,omitempty"` // 计数单位：文件/目录（默认文件）
	// 扫描结果
	Available  bool       `json:"available"`
	Files      int64      `json:"files"`
	Bytes      int64      `json:"bytes"`
	OldestDays int        `json:"oldestDays"`
	SkipNote   string     `json:"skipNote,omitempty"`
	ListNote   string     `json:"listNote,omitempty"` // 明细截断说明（大目录时）
	FileList   []JunkFile `json:"fileList,omitempty"` // 候选文件明细，供前端勾选（最多 maxFileList 条）
}

// JunkFile 一条候选垃圾文件（前端可勾选后选择性清理）。
type JunkFile struct {
	Path    string `json:"path"`
	Size    int64  `json:"size"`
	AgeDays int    `json:"ageDays"`
	Dir     bool   `json:"dir,omitempty"` // 整目录删除项（已卸载应用残留）
}

type junkRule struct {
	id, name, desc string
	roots          []string
	match          func(rel, name string) bool
	minAge         time.Duration
	safe           bool
	dirMode        bool // true=按目录整棵删除（清理走 RemoveAll + 孤儿目录复核）
}

// 分类规则：路径白名单 + 匹配模式 + 最小年龄。
// 清理是破坏性操作，规则收敛比覆盖面重要。
var junkRules = []junkRule{
	{
		id: "apt-cache", name: "APT 软件包缓存",
		desc:  "已下载的 .deb 安装包缓存，删除后需要时可重新下载",
		roots: []string{"/var/cache/apt/archives"},
		match: func(rel, name string) bool { return strings.HasSuffix(name, ".deb") },
		minAge: 0, safe: true,
	},
	{
		id: "log-rotated", name: "轮转旧日志",
		desc:  "/var/log 下已轮转的旧日志（*.1/*.gz/*.old 等），当前活跃日志不受影响",
		roots: []string{"/var/log"},
		match: func(rel, name string) bool {
			if strings.Contains(rel, "/journal/") {
				return false
			}
			for _, ext := range []string{".gz", ".xz", ".bz2", ".old", ".1", ".2", ".3", ".4"} {
				if strings.HasSuffix(name, ext) {
					return true
				}
			}
			return false
		},
		minAge: 14 * 24 * time.Hour, safe: true,
	},
	{
		id: "journal", name: "systemd 日志归档",
		desc:  "journald 二进制日志，保留最近 500MB，其余按年龄清理（活跃主文件除外）",
		roots: []string{"/var/log/journal"},
		match: func(rel, name string) bool {
			return strings.HasSuffix(name, ".journal") && !strings.HasSuffix(name, "system.journal")
		},
		minAge: 3 * 24 * time.Hour, safe: true,
	},
	{
		id: "tmp", name: "系统临时文件",
		desc:  "/tmp 与 /var/tmp 中超过 7 天未修改的普通文件（自动跳过目录、符号链接与套接字）",
		roots: []string{"/tmp", "/var/tmp"},
		match: func(rel, name string) bool { return true },
		minAge: 7 * 24 * time.Hour, safe: true,
	},
	{
		id: "crash", name: "崩溃转储",
		desc:  "/var/crash 与 systemd coredump 留下的崩溃文件，排查完问题即可清理",
		roots: []string{"/var/crash", "/var/lib/systemd/coredump"},
		match: func(rel, name string) bool { return true },
		minAge: 24 * time.Hour, safe: true,
	},
	{
		id: "appdata-logs", name: "应用旧日志",
		desc:  "应用数据目录（@appdata/<应用>/logs，含飞牛系统应用 /usr/local/apps/@appdata）中超过 30 天的旧日志",
		roots: nil, // 运行时按 fnOS 实际路径动态展开
		match: func(rel, name string) bool { return true },
		minAge: 30 * 24 * time.Hour, safe: true,
	},
	{
		id: "appdata-tmp", name: "应用临时文件",
		desc:  "应用临时目录（@appdata/<应用>/tmp 与 /vol*/@apptemp/<应用>）中超过 7 天的临时文件",
		roots: nil,
		match: func(rel, name string) bool { return true },
		minAge: 7 * 24 * time.Hour, safe: true,
	},
	{
		id: "thumbnails", name: "缩略图缓存",
		desc:  "飞牛相册缩略图与人脸缓存（@appmeta/trim.photos/data/thumb、data/face）及应用 thumbnails 目录，全部可删，浏览时自动重新生成",
		roots: nil,
		match: func(rel, name string) bool { return true },
		minAge: 0, safe: false,
	},
	{
		id: "app-orphan", name: "已卸载应用残留",
		desc:  "已卸载应用留下的数据与临时目录（@appdata、@apptemp）里的整棵目录。飞牛卸载应用时通常保留数据目录，久了会越积越多；删除后该应用的设置与数据不可恢复",
		roots: nil, // 运行时按「无程序目录的应用」动态展开
		match: func(rel, name string) bool { return true },
		minAge: 0, safe: false, dirMode: true,
	},
}

// ruleRoots 动态展开分类的扫描根：appdata 三类按 fnOS 实际路径现查。
// fnOS 路径事实（社区验证）：
//   - 系统应用数据：/usr/local/apps/@appdata/<app>（trim.photos 相册 / trim.media 影视等）
//   - FPK 应用持久化数据：/vol*/@appdata/<app>
//   - FPK 应用临时文件：/vol*/@apptemp/<app>
//   - 相册缩略图与人脸缓存：/vol*/@appmeta/trim.photos/data/thumb 与 data/face（不在 @appdata！）
func ruleRoots(rule junkRule) []string {
	orphans := fnosOrphanAppDirs()
	// 已卸载应用残留：每个孤儿应用目录自身就是一个待删根，
	// 判定与进程页共用 scanFnosAppDirs（程序目录 + 悬空符号链接都算「已卸载」）
	if rule.id == "app-orphan" {
		var out []string
		for _, d := range orphans {
			// 目录内有独立挂载点时不提供清理：RemoveAll 会删到挂载进来的数据
			if hasMountUnder(d) {
				continue
			}
			out = append(out, d)
		}
		return out
	}
	// 已卸载应用的数据目录已归「已卸载应用残留」整棵处理，
	// 日志/临时/缩略图三类不再钻进去扫，否则同一批字节被重复计入「可释放合计」。
	underOrphan := func(p string) bool {
		for _, o := range orphans {
			if p == o || strings.HasPrefix(p, o+string(filepath.Separator)) {
				return true
			}
		}
		return false
	}

	// 缩略图分类：精确匹配可再生缓存子目录，绝不扫整棵 @appmeta（内有 photo.db 等数据库）
	if rule.id == "thumbnails" {
		var out []string
		patterns := []string{
			"/vol*/@appmeta/*/data/thumb", // 相册缩略图（列表/时间线预览）
			"/vol*/@appmeta/*/data/face",  // 人脸识别小图
		}
		for _, pat := range fnosDataPatterns {
			patterns = append(patterns, pat+"/thumbnails") // 应用常规缩略图目录
		}
		for _, pattern := range patterns {
			hits, _ := filepath.Glob(pattern)
			for _, v := range hits {
				if fi, err := os.Stat(v); err == nil && fi.IsDir() && !underOrphan(v) {
					out = append(out, v)
				}
			}
		}
		return out
	}

	var sub string
	switch rule.id {
	case "appdata-logs":
		sub = "logs"
	case "appdata-tmp":
		sub = "tmp"
	default:
		return rule.roots
	}
	var out []string
	appendDir := func(p string) {
		if underOrphan(p) {
			return
		}
		if fi, err := os.Stat(p); err == nil && fi.IsDir() {
			out = append(out, p)
		}
	}
	// 两种 appdata 布局下的同名子目录
	for _, pattern := range fnosDataPatterns {
		hits, _ := filepath.Glob(pattern + "/" + sub)
		for _, v := range hits {
			appendDir(v)
		}
	}
	// FPK 应用临时目录：/vol*/@apptemp/<app> 整棵纳入 tmp 规则
	if rule.id == "appdata-tmp" {
		for _, pattern := range fnosTempPatterns {
			hits, _ := filepath.Glob(pattern)
			for _, v := range hits {
				appendDir(v)
			}
		}
	}
	return out
}

// ============ 扫描 ============

func (s *Server) handleScan(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	now := time.Now()

	cats := make([]JunkCategory, 0, len(junkRules))
	candidates := map[string]map[string]int64{}
	var totalBytes, totalFiles int64

	for i := range junkRules {
		rule := junkRules[i]
		cat := JunkCategory{
			ID: rule.id, Name: rule.name, Desc: rule.desc,
			Safe: rule.safe, MinAgeDays: int(rule.minAge.Hours() / 24),
			Paths: ruleRoots(rule),
		}
		if rule.dirMode {
			cat.Unit = "目录"
		}
		if len(cat.Paths) == 0 {
			cat.SkipNote = "本机无相关目录"
			cats = append(cats, cat)
			continue
		}
		cat.Available = true

		switch {
		case rule.dirMode:
			scanAppOrphans(&cat, now, candidates)
		case rule.id == "journal":
			scanJournal(&cat, now)
		default:
			for _, root := range cat.Paths {
				walkJunk(root, rule, &cat, candidates)
			}
		}
		cats = append(cats, cat) // 有目录的分类也必须进结果（此前漏掉，导致扫出文件的分类全部丢失、列表只剩空目录项）

		totalBytes += cat.Bytes
		totalFiles += cat.Files
	}

	s.scanMu.Lock()
	s.scanTime = now
	s.candidate = candidates
	s.scanMu.Unlock()

	ok(w, map[string]any{
		"categories": cats,
		"totalBytes": totalBytes,
		"totalFiles": totalFiles,
		"scannedAt":  now.Format("2006-01-02 15:04:05"),
		"durationMs": time.Since(start).Milliseconds(),
	})
}

// maxFileList 每个分类返回给前端的文件明细上限：缩略图等大目录动辄数万文件，
// 全量返回会把 /api/scan 响应撑到几十 MB、前端渲染卡死。统计数与全清不受影响（后端全量候选）。
const maxFileList = 300

// walkJunk 遍历一个白名单根，累加满足条件的普通文件。
func walkJunk(root string, rule junkRule, cat *JunkCategory, out map[string]map[string]int64) {
	if _, err := os.Stat(root); err != nil {
		return
	}
	_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 读不了的直接跳过
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), "systemd-private-") {
				return fs.SkipDir
			}
			return nil
		}
		// 硬性门槛：只认普通文件；符号链接/socket/设备/管道一律跳过
		if d.Type()&^fs.ModePerm != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		age := time.Since(info.ModTime())
		if rule.minAge > 0 && age < rule.minAge {
			return nil
		}
		rel, _ := filepath.Rel(root, path)
		if !rule.match(rel, d.Name()) {
			return nil
		}
		cat.Files++
		cat.Bytes += info.Size()
		if dd := int(age.Hours() / 24); dd > cat.OldestDays {
			cat.OldestDays = dd
		}
		if len(cat.FileList) < maxFileList {
			cat.FileList = append(cat.FileList, JunkFile{
				Path:    path,
				Size:    info.Size(),
				AgeDays: int(age.Hours() / 24),
			})
			if cat.Files == int64(maxFileList) {
				cat.ListNote = "文件较多，仅列出前 " + strconv.Itoa(maxFileList) + " 个，全清不受影响"
			}
		}
		if out[rule.id] == nil {
			out[rule.id] = map[string]int64{}
		}
		out[rule.id][path] = info.Size()
		return nil
	})
}

// scanAppOrphans 已卸载应用残留：每个孤儿目录按整棵统计，一条明细 = 一个目录。
// 不做年龄门槛（卸载残留多久都该清），但要求目录内有内容。
func scanAppOrphans(cat *JunkCategory, now time.Time, out map[string]map[string]int64) {
	for _, root := range cat.Paths {
		if fi, err := os.Lstat(root); err != nil || !fi.IsDir() {
			continue // 符号链接或已消失：不提供删除
		}
		var bytes, files int64
		var newest time.Time
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			files++
			bytes += info.Size()
			if info.ModTime().After(newest) {
				newest = info.ModTime()
			}
			return nil
		})
		if bytes == 0 && files == 0 {
			continue // 空目录交给系统清理，不占列表
		}
		cat.Files++
		cat.Bytes += bytes
		age := int(now.Sub(newest).Hours() / 24)
		if age > cat.OldestDays {
			cat.OldestDays = age
		}
		if len(cat.FileList) < maxFileList {
			cat.FileList = append(cat.FileList, JunkFile{
				Path:    root,
				Size:    bytes,
				AgeDays: age,
				Dir:     true,
			})
		}
		if out[cat.ID] == nil {
			out[cat.ID] = map[string]int64{}
		}
		out[cat.ID][root] = bytes
	}
}

// hasMountUnder 目录自身或内部是否存在挂载点。
// 判据来自 /proc/self/mountinfo（第五列是挂载点）。读不到（非 Linux）时返回 false。
func hasMountUnder(dir string) bool {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	clean := filepath.Clean(dir)
	prefix := clean + "/"
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		mp := strings.ReplaceAll(f[4], `\040`, " ")
		if mp == clean || strings.HasPrefix(mp, prefix) {
			return true
		}
	}
	return false
}

// isFnosAppDirPath 路径是否形如 .../@appdata/<名> 或 .../@apptemp/<名>。
// 整目录删除的最后一道形状闸：只允许删应用数据/临时目录的最外层一级。
func isFnosAppDirPath(p string) bool {
	clean := filepath.Clean(p)
	parent := filepath.Dir(clean)
	switch filepath.Base(parent) {
	case "@appdata", "@apptemp":
	default:
		return false
	}
	name := filepath.Base(clean)
	return name != "" && name != "." && name != ".." && !strings.HasPrefix(name, ".")
}

// scanJournal journal 特殊规则：按修改时间从新到旧累计，保留最近 500MB。
func scanJournal(cat *JunkCategory, now time.Time) {
	const keepBytes = 500 << 20
	type jf struct {
		path string
		size int64
		mod  time.Time
	}
	var files []jf
	for _, root := range cat.Paths {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			if d.Type()&^fs.ModePerm != 0 || !strings.HasSuffix(d.Name(), ".journal") {
				return nil
			}
			info, err := d.Info()
			if err != nil {
				return nil
			}
			files = append(files, jf{path, info.Size(), info.ModTime()})
			return nil
		})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].mod.After(files[j].mod) })
	var kept int64
	for _, f := range files {
		age := now.Sub(f.mod)
		if strings.HasSuffix(f.path, "system.journal") {
			continue // 活跃主文件不动
		}
		if kept+f.size <= keepBytes {
			kept += f.size
			continue
		}
		if age < 3*24*time.Hour {
			continue // 太新的不动，即便超预算
		}
		cat.Files++
		cat.Bytes += f.size
		if d := int(age.Hours() / 24); d > cat.OldestDays {
			cat.OldestDays = d
		}
		if len(cat.FileList) < maxFileList {
			cat.FileList = append(cat.FileList, JunkFile{
				Path:    f.path,
				Size:    f.size,
				AgeDays: int(age.Hours() / 24),
			})
		}
	}
}

// ============ 清理 ============

func (s *Server) handleClean(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Category string   `json:"category"`
		Paths    []string `json:"paths"` // 空=清理该分类全部候选文件
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.Category == "" {
		fail(w, 400, "参数错误")
		return
	}

	s.scanMu.Lock()
	all := s.candidate[req.Category]
	scanAge := time.Since(s.scanTime)
	// 锁内快照：并发清理同分类时避免对同一 map 同时遍历与 delete（Go 会 panic）
	snap := make(map[string]int64, len(all))
	for p, sz := range all {
		snap[p] = sz
	}
	s.scanMu.Unlock()

	if all == nil {
		fail(w, 400, "该分类没有可清理项（或尚未扫描），请先执行扫描")
		return
	}
	if scanAge > 30*time.Minute {
		fail(w, 400, "扫描结果已超过 30 分钟，请重新扫描后再清理")
		return
	}

	// 找回该分类规则，清理时逐条复核年龄
	var rule *junkRule
	for i := range junkRules {
		if junkRules[i].id == req.Category {
			rule = &junkRules[i]
			break
		}
	}
	if rule == nil {
		fail(w, 400, "未知分类")
		return
	}

	// 选定路径集合（空=全选）
	selected := map[string]bool{}
	if len(req.Paths) > 0 {
		for _, p := range req.Paths {
			selected[p] = true
		}
	}

	// 「已卸载应用残留」按目录整棵删除：删除前重新判定一次孤儿集合——
	// 若用户刚好重装了该应用（程序目录回来了），这里就不该再删它的数据。
	if rule.dirMode {
		freed, dirs, failures := cleanOrphanAppDirs(snap, selected)
		s.scanMu.Lock()
		if s.candidate[req.Category] != nil {
			for path := range snap {
				if len(selected) > 0 && !selected[path] {
					continue
				}
				delete(s.candidate[req.Category], path)
			}
			if len(s.candidate[req.Category]) == 0 {
				delete(s.candidate, req.Category)
			}
		}
		s.scanMu.Unlock()
		ok(w, map[string]any{
			"freedBytes": freed, "files": dirs, "failures": failures,
			"freedText": fmtBytes(freed),
		})
		return
	}

	var freed, files int64
	var failures []string
	for path, size := range snap {
		if len(selected) > 0 && !selected[path] {
			continue
		}
		// 复核 1：白名单前缀（最后一道硬闸）
		if !underWhitelist(path, rule) {
			failures = append(failures, path+": 路径不在白名单内（拒绝）")
			continue
		}
		// 复核 2：必须是普通文件（Lstat 不跟随符号链接）
		info, err := os.Lstat(path)
		if err != nil {
			continue // 已不存在，视为已清理
		}
		if !info.Mode().IsRegular() {
			failures = append(failures, path+": 非普通文件（拒绝）")
			continue
		}
		// 复核 3：年龄阈值
		if rule.minAge > 0 && time.Since(info.ModTime()) < rule.minAge {
			failures = append(failures, path+": 文件年龄不足阈值（拒绝）")
			continue
		}
		if err := os.Remove(path); err != nil {
			failures = append(failures, path+": "+err.Error())
			continue
		}
		freed += size
		files++
	}

	// 清完把已处理路径从候选里移除；若该分类已空，则删除分类键
	s.scanMu.Lock()
	if s.candidate[req.Category] != nil {
		for path := range snap {
			if len(selected) > 0 && !selected[path] {
				continue
			}
			delete(s.candidate[req.Category], path)
		}
		if len(s.candidate[req.Category]) == 0 {
			delete(s.candidate, req.Category)
		}
	}
	s.scanMu.Unlock()

	ok(w, map[string]any{
		"freedBytes": freed,
		"files":      files,
		"failures":   failures,
		"freedText":  fmtBytes(freed),
	})
}

// cleanOrphanAppDirs 整目录删除已卸载应用残留。
// 三重复核：①候选路径必须仍在「当前孤儿集合」里（重装过就不删）；
// ②路径形状必须是 @appdata/@apptemp 的直接子级；③必须是非符号链接的真实目录。
func cleanOrphanAppDirs(snap map[string]int64, selected map[string]bool) (freed, dirs int64, failures []string) {
	current := map[string]bool{}
	for _, d := range fnosOrphanAppDirs() {
		current[filepath.Clean(d)] = true
	}
	for path, size := range snap {
		if len(selected) > 0 && !selected[path] {
			continue
		}
		clean := filepath.Clean(path)
		if !current[clean] {
			failures = append(failures, path+": 应用已重新安装或目录已不存在（拒绝）")
			continue
		}
		if !isFnosAppDirPath(clean) {
			failures = append(failures, path+": 路径形状不符（拒绝）")
			continue
		}
		if hasMountUnder(clean) {
			failures = append(failures, path+": 目录内有挂载点（拒绝，避免删到挂载数据）")
			continue
		}
		fi, err := os.Lstat(clean)
		if err != nil {
			continue
		}
		if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
			failures = append(failures, path+": 不是真实目录（拒绝）")
			continue
		}
		if err := os.RemoveAll(clean); err != nil {
			failures = append(failures, path+": "+err.Error())
			continue
		}
		freed += size
		dirs++
	}
	return freed, dirs, failures
}

// underWhitelist 清理前的最后一道硬闸：路径必须精确落在该分类白名单根之下。
func underWhitelist(path string, rule *junkRule) bool {
	clean := filepath.Clean(path)
	for _, root := range ruleRoots(*rule) {
		rootClean := filepath.Clean(root)
		if clean == rootClean {
			return false // 根目录本身不可删
		}
		if strings.HasPrefix(clean, rootClean+string(filepath.Separator)) {
			return true
		}
	}
	return false
}
