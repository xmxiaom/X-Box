package main

// 媒体管理：按目录批量整理视频/音频的文件名与元数据。
//
// 分工与定位：
//   - 视频整理：读文件名 + 内嵌元数据（MP4/MKV）→ 规范命名，剧集自动建
//     「剧名 (年份)/Season 01」目录，命名对齐 Jellyfin/Plex 识别规范。
//   - 音频整理：读标签（ID3/Vorbis/MP4）→ 字段纠错（歌名/歌手/专辑错位纠正）
//     → 可选在线补全（MusicBrainz/iTunes）→ 规范命名「歌手 - 歌名」或
//     归档到「歌手/专辑」→ 可选回写标签。
//
// 安全与可逆性：
//   - 只改动用户显式选择的目录内的媒体文件，不做删除。
//   - 默认「预览」模式先出计划（旧名 → 新名 + 纠错明细），确认后才执行。
//   - 目标重名自动加 (1)(2) 后缀，绝无覆盖。
//   - 目录校验复用系统敏感目录黑名单，拒绝对 /etc /usr 等位置操作。

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// ---------- 数据结构 ----------

// MediaTask 一条媒体整理任务（视频或音频）
type MediaTask struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Kind      string `json:"kind"`      // video / audio
	Dir       string `json:"dir"`       // 目标目录（绝对路径）
	Recursive bool   `json:"recursive"` // 递归子目录
	UseMeta   bool   `json:"useMeta"`   // 视频：用内嵌元数据补标题/年份/分辨率
	Fix       bool   `json:"fix"`       // 音频：字段纠错
	Online    bool   `json:"online"`    // 音频：联网匹配元数据
	WriteTag  bool   `json:"writeTag"`  // 音频：回写标签
	Organize  bool   `json:"organize"`  // 归档到 歌手/专辑 或 剧集/Season
	Template  string `json:"template"`  // 音频命名模板
	Exclude   string `json:"exclude"`   // 逗号分隔通配
	Enabled   bool   `json:"enabled"`
}

// mediaOptions 单次整理的行为开关（由任务派生）
type mediaOptions struct {
	Kind     string
	UseMeta  bool
	Fix      bool
	Online   bool
	WriteTag bool
	Organize bool
	Template string
	// ArtistSecond：文件名里歌手在右侧（「歌名 - 歌手」）
	ArtistSecond bool
	// RoleGuessed：角色来自模板顺序推断，目录内没有重复证据
	RoleGuessed bool
	// Root：任务的根目录。归档一律以它为基准，
	// 不能用"文件当前所在目录"，否则递归整理会一层层套娃
	Root string
}

// MediaState 任务运行状态（供前端轮询）
type MediaState struct {
	Running  bool      `json:"running"`
	Phase    string    `json:"phase"` // scan / plan / apply / done
	Scanned  int       `json:"scanned"`
	Total    int       `json:"total"` // 待处理媒体文件数
	Done     int       `json:"done"`
	Renamed  int       `json:"renamed"`
	Tagged   int       `json:"tagged"`
	Skipped  int       `json:"skipped"`
	Failed   int       `json:"failed"`
	Unsupported int    `json:"unsupported"` // 该格式不支持写标签（不算失败）
	Stopped     bool   `json:"stopped"`     // 用户手动停止（不算错误）
	Current  string    `json:"current"`
	LastAt   time.Time `json:"lastAt"`
	LastOK   bool      `json:"lastOk"`
	LastErr  string    `json:"lastErr"`
	Duration string    `json:"duration"`
	Logs     []string  `json:"logs"`
}

// mediaPlanItem 统一的重命名计划条目（视频/音频共用输出格式）
type mediaPlanItem struct {
	OldPath    string     `json:"oldPath"`
	NewPath    string     `json:"newPath"`
	OldName    string     `json:"oldName"`
	NewName    string     `json:"newName"`
	Title      string     `json:"title"`
	Artist     string     `json:"artist,omitempty"`
	Album      string     `json:"album,omitempty"`
	Year       int        `json:"year,omitempty"`
	Track      int        `json:"track,omitempty"`
	Season     int        `json:"season,omitempty"`
	Episode    int        `json:"episode,omitempty"`
	Res        string     `json:"res,omitempty"`
	Source     string     `json:"source,omitempty"`
	Reason     string     `json:"reason,omitempty"`
	Changed    bool       `json:"changed"`
	Fixes      []mediaFix `json:"fixes,omitempty"`
	Confidence float64    `json:"confidence,omitempty"`
	Warn       string     `json:"warn,omitempty"`
	Error      string     `json:"error,omitempty"`
	RoleGuessed bool      `json:"roleGuessed,omitempty"`
}

var mediaStore struct {
	mu     sync.Mutex
	tasks  []MediaTask
	state  map[string]*MediaState
	busy   map[string]bool
	cancel map[string]chan struct{}
}

func mediaTasksPath() string {
	dir := os.Getenv("XBOX_DATA")
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "media.json")
}

func loadMediaTasks() []MediaTask {
	mediaStore.mu.Lock()
	defer mediaStore.mu.Unlock()
	b, err := os.ReadFile(mediaTasksPath())
	if err != nil {
		return nil
	}
	var out []MediaTask
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	// 默认模板已改为「歌名 - 歌手」：仍停留在旧默认值的任务跟随新默认，
	// 用户自定义过的模板原样保留
	for i := range out {
		tpl := strings.TrimSpace(out[i].Template)
		// 去掉空格后比较，兼容 "{artist}-{title}" 这类写法
		flat := strings.ReplaceAll(tpl, " ", "")
		if tpl == "" || flat == "{artist}-{title}" {
			out[i].Template = "{title} - {artist}"
		}
	}
	return out
}

func saveMediaTasks(tasks []MediaTask) error {
	mediaStore.mu.Lock()
	defer mediaStore.mu.Unlock()
	b, err := json.MarshalIndent(tasks, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(mediaTasksPath(), b, 0o600)
}

func mediaStateOf(id string) MediaState {
	mediaStore.mu.Lock()
	defer mediaStore.mu.Unlock()
	if mediaStore.state == nil {
		mediaStore.state = map[string]*MediaState{}
	}
	if st, ok := mediaStore.state[id]; ok {
		cp := *st
		cp.Logs = append([]string(nil), st.Logs...)
		return cp
	}
	return MediaState{}
}

func setMediaState(id string, f func(*MediaState)) {
	mediaStore.mu.Lock()
	defer mediaStore.mu.Unlock()
	if mediaStore.state == nil {
		mediaStore.state = map[string]*MediaState{}
	}
	st, ok := mediaStore.state[id]
	if !ok {
		st = &MediaState{}
		mediaStore.state[id] = st
	}
	f(st)
}

// mediaLog 追加一行日志（保留最近 80 条，避免内存无限增长）
func mediaLog(id, line string) {
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	setMediaState(id, func(st *MediaState) {
		st.Logs = append(st.Logs, line)
		if len(st.Logs) > 80 {
			st.Logs = st.Logs[len(st.Logs)-80:]
		}
	})
}

// ---------- 文件枚举 ----------

// collectMediaFiles 枚举目录下的媒体文件（按 kind 过滤扩展名）
func collectMediaFiles(root, kind string, recursive bool, excludes []string, onScan func(int)) ([]string, error) {
	var out []string
	root = filepath.Clean(root)
	err := filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // 单个条目不可读不致命
		}
		if p == root {
			return nil
		}
		rel, rerr := filepath.Rel(root, p)
		if rerr != nil {
			return nil
		}
		if info.IsDir() {
			if syncExcluded(rel, info.Name(), excludes) {
				return filepath.SkipDir
			}
			if !recursive {
				return filepath.SkipDir
			}
			return nil
		}
		if !info.Mode().IsRegular() {
			return nil
		}
		if syncExcluded(rel, info.Name(), excludes) {
			return nil
		}
		isMatch := IsVideoExt(p)
		if kind == "audio" {
			isMatch = IsAudioExt(p)
		}
		if !isMatch {
			return nil
		}
		out = append(out, p)
		if onScan != nil && len(out)%200 == 0 {
			onScan(len(out))
		}
		return nil
	})
	return out, err
}

// audioRole 文件名中「歌手」所在的一侧
type audioRole struct {
	ArtistSecond bool
	Guessed      bool
}

// detectRolesByDir 按目录判断文件名里歌手在左还是在右。
// 依据：同一目录（通常是一张专辑）里歌手名会反复出现，歌名基本不重复。
// 左侧重复多 → 歌手在左；右侧重复多 → 歌手在右（「歌名 - 歌手」）。
// 证据不足时按模板顺序推断并标记 Guessed——这类文件只改名、不回写标签。
func detectRolesByDir(files []string, tmpl string) map[string]audioRole {
	tmplTitleFirst := true
	if strings.TrimSpace(tmpl) != "" {
		ti := strings.Index(tmpl, "{title}")
		ai := strings.Index(tmpl, "{artist}")
		if ti < 0 {
			tmplTitleFirst = false
		} else if ai >= 0 && ai < ti {
			tmplTitleFirst = false
		}
	}
	left := map[string]map[string]int{}
	right := map[string]map[string]int{}
	knownLeft := map[string]int{}  // 左侧命中常用歌手名的文件数
	knownRight := map[string]int{} // 右侧命中常用歌手名的文件数
	inc := func(m map[string]map[string]int, dir, tok string) {
		if m[dir] == nil {
			m[dir] = map[string]int{}
		}
		m[dir][tok]++
	}
	for _, p := range files {
		dir := filepath.Dir(p)
		base := fixMojibake(strings.TrimSuffix(filepath.Base(p), filepath.Ext(p)))
		i := strings.Index(base, " - ")
		if i <= 0 {
			continue
		}
		l := strings.TrimSpace(base[:i])
		r := strings.TrimSpace(base[i+3:])
		if l == "" || r == "" {
			continue
		}
		if !isDigitsOnly(l) {
			inc(left, dir, strings.ToLower(l))
		}
		if !isDigitsOnly(r) {
			inc(right, dir, strings.ToLower(r))
		}
		if lk, rk := isKnownArtist(l), isKnownArtist(r); lk != rk {
			if rk {
				knownRight[dir]++
			} else {
				knownLeft[dir]++
			}
		}
	}
	out := map[string]audioRole{}
	seen := map[string]bool{}
	for _, m := range []map[string]map[string]int{left, right} {
		for dir := range m {
			if seen[dir] {
				continue
			}
			seen[dir] = true
			out[dir] = classifyRole(maxRepeat(left[dir]), maxRepeat(right[dir]), knownLeft[dir], knownRight[dir], tmplTitleFirst)
		}
	}
	return out
}

func classifyRole(maxLeft, maxRight, knownLeft, knownRight int, tmplTitleFirst bool) audioRole {
	// 常用歌手名命中是最直接的证据，优先于重复度统计
	if knownLeft != knownRight {
		return audioRole{ArtistSecond: knownRight > knownLeft}
	}
	switch {
	case maxRight >= 3 && maxRight > maxLeft*2:
		return audioRole{ArtistSecond: true}
	case maxLeft >= 3 && maxLeft > maxRight*2:
		return audioRole{ArtistSecond: false}
	}
	return audioRole{ArtistSecond: tmplTitleFirst, Guessed: true}
}

func maxRepeat(m map[string]int) int {
	best := 0
	for _, v := range m {
		if v > best {
			best = v
		}
	}
	return best
}

func isDigitsOnly(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func parseMediaExcludes(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []string
	for _, part := range strings.Split(s, ",") {
		if p := strings.TrimSpace(part); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ---------- 计划与执行 ----------

// buildMediaPlan 生成一个文件的整理计划
func buildMediaPlan(path string, opts mediaOptions, reserved map[string]bool, role audioRole) (mediaPlanItem, MediaMeta, []byte) {
	if opts.Kind == "audio" {
		opts.ArtistSecond = role.ArtistSecond
		opts.RoleGuessed = role.Guessed
		ap, meta, cover := planAudioRename(path, opts, reserved)
		return mediaPlanItem{
			OldPath: ap.OldPath, NewPath: ap.NewPath,
			OldName: ap.OldName, NewName: ap.NewName,
			Title: ap.Title, Artist: ap.Artist, Album: ap.Album,
			Year: ap.Year, Track: ap.Track,
			Reason: ap.Reason, Changed: ap.Changed,
			Fixes: ap.Fixes, Confidence: ap.Confidence, Warn: ap.Warn,
			RoleGuessed: ap.RoleGuessed,
		}, meta, cover
	}
	meta, _ := ReadMediaMeta(path)
	vp := planVideoRename(path, meta, opts.UseMeta, opts.Organize)
	// 同批次内再避让一次：planVideoRename 只能看到磁盘现状，
	// 本批前几个文件尚未落盘时可能撞名
	if np := uniquePath(path, vp.NewPath, reserved); np != vp.NewPath {
		vp.NewPath = np
		vp.NewName = filepath.Base(np)
		vp.Changed = np != path
	}
	return mediaPlanItem{
		OldPath: vp.OldPath, NewPath: vp.NewPath,
		OldName: vp.OldName, NewName: vp.NewName,
		Title: vp.Title, Year: vp.Year,
		Season: vp.Season, Episode: vp.Episode,
		Res: vp.Res, Source: vp.Source,
		Reason: vp.Reason, Changed: vp.Changed,
	}, meta, nil
}

// previewMediaTask 只出计划不动文件（dry run）
func previewMediaTask(t MediaTask, limit int) (map[string]any, error) {
	opts := mediaOptions{
		Kind: t.Kind, UseMeta: t.UseMeta, Fix: t.Fix, Online: t.Online,
		WriteTag: t.WriteTag, Organize: t.Organize, Template: t.Template,
		Root: t.Dir,
	}
	excludes := parseMediaExcludes(t.Exclude)
	files, err := collectMediaFiles(t.Dir, t.Kind, t.Recursive, excludes, nil)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 300
	}
	// 在线匹配有 1 req/s 节流，预览时只对前若干条联网，避免用户等几分钟
	onlineCap := 8
	roles := detectRolesByDir(files, t.Template)
	reserved := map[string]bool{}
	items := []mediaPlanItem{}
	changed, fixed, warnN, onlineN, guessedN := 0, 0, 0, 0, 0
	for _, p := range files {
		o := opts
		if o.Online && onlineN >= onlineCap {
			o.Online = false // 超出部分只做本地纠错，保证预览尽快返回
		}
		if o.Online {
			onlineN++
		}
		it, _, _ := buildMediaPlan(p, o, reserved, roles[filepath.Dir(p)])
		if it.RoleGuessed {
			guessedN++
		}
		if it.Changed {
			changed++
			reserved[it.NewPath] = true
		} else if len(it.Fixes) > 0 {
			fixed++ // 名字已规范、只有字段被纠正：这类同样要在预览里可见
		}
		if it.Warn != "" {
			warnN++
		}
		if (it.Changed || len(it.Fixes) > 0) && len(items) < limit {
			items = append(items, it)
		}
	}
	var notes []string
	if opts.Online && len(files) > onlineCap {
		notes = append(notes, fmt.Sprintf("联网匹配在预览阶段仅覆盖前 %d 个文件；实际执行时会全部联网补全（每首约 1 秒）", onlineCap))
	}
	if guessedN > 0 {
		notes = append(notes, fmt.Sprintf("其中 %d 个文件没有标签、目录里也无法判断歌手在左还是右，已按模板顺序命名，且不会写入标签", guessedN))
	}
	note := strings.Join(notes, "；")
	return map[string]any{
		"total": len(files), "changed": changed, "fixed": fixed, "warn": warnN,
		"roleGuessed": guessedN,
		"items": items, "truncated": changed+fixed > len(items),
		"onlineCapped": opts.Online && len(files) > onlineCap,
		"note":         note,
	}, nil
}

// runMediaTask 执行整理（异步调用）。返回结果计数，错误仅表示整体失败。
func runMediaTask(t MediaTask) {
	id := t.ID
	start := time.Now()
	opts := mediaOptions{
		Kind: t.Kind, UseMeta: t.UseMeta, Fix: t.Fix, Online: t.Online,
		WriteTag: t.WriteTag, Organize: t.Organize, Template: t.Template,
		Root: t.Dir,
	}
	excludes := parseMediaExcludes(t.Exclude)

	setMediaState(id, func(st *MediaState) {
		*st = MediaState{Running: true, Phase: "scan", LastAt: start, Logs: []string{}}
	})
	mediaLog(id, fmt.Sprintf("开始扫描：%s", t.Dir))

	var cancel chan struct{}
	mediaStore.mu.Lock()
	if mediaStore.cancel == nil {
		mediaStore.cancel = map[string]chan struct{}{}
	}
	cancel = make(chan struct{})
	mediaStore.cancel[id] = cancel
	mediaStore.mu.Unlock()

	files, err := collectMediaFiles(t.Dir, t.Kind, t.Recursive, excludes, func(n int) {
		setMediaState(id, func(st *MediaState) { st.Scanned = n })
	})
	if err != nil {
		finishMediaTask(id, start, false, "扫描目录失败："+err.Error())
		return
	}
	setMediaState(id, func(st *MediaState) {
		st.Phase = "apply"
		st.Scanned = len(files)
		st.Total = len(files)
	})
	mediaLog(id, fmt.Sprintf("发现 %d 个待整理文件", len(files)))

	roles := detectRolesByDir(files, t.Template)
	reserved := map[string]bool{}
	// 标签写入失败按原因聚合：同一原因只打前 3 条，末尾给汇总，避免刷屏
	tagFailByReason := map[string]int{}
	tagFailOrder := []string{}
	unsupportedSeen := map[string]int{}
	for i, p := range files {
		select {
		case <-cancel:
			mediaLog(id, "已手动停止（已处理的不回滚）")
			finishMediaStopped(id, start)
			return
		default:
		}
		setMediaState(id, func(st *MediaState) { st.Current = filepath.Base(p) })

		it, meta, cover := buildMediaPlan(p, opts, reserved, roles[filepath.Dir(p)])
		if it.Error != "" {
			setMediaState(id, func(st *MediaState) { st.Done = i + 1; st.Failed++ })
			continue
		}

		// 标签回写（在改名之前完成，写入用临时文件+原子改名）
		// 仅在"名字要变"或"纠错改动了字段"时写，避免整库无谓重写
		tagDone, tagErr := false, ""
		// RoleGuessed：歌手/歌名位置是猜的，宁可不写标签，也不把错的信息写进文件
		unsupported := false
		if opts.WriteTag && opts.Kind == "audio" && (it.Changed || len(it.Fixes) > 0) && !it.RoleGuessed {
			if err := writeMediaTags(it.OldPath, meta, cover); err != nil {
				if errors.Is(err, errTagUnsupported) {
					// 格式本身不支持（ape/dsf 等）：不是错误，单独计数
					unsupported = true
					if unsupportedSeen[err.Error()] == 0 {
						unsupportedSeen[err.Error()] = 1
						mediaLog(id, "· "+err.Error()+"（这类文件只改名，不改标签）")
					}
					unsupportedSeen[err.Error()]++
					goto tagDone
				}
				tagErr = err.Error()
				tagFailByReason[tagErr]++
				if tagFailByReason[tagErr] <= 3 {
					mediaLog(id, fmt.Sprintf("! 标签写入失败 %s：%s", it.OldName, tagErr))
				}
				seen := false
				for _, r := range tagFailOrder {
					if r == tagErr {
						seen = true
					}
				}
				if !seen {
					tagFailOrder = append(tagFailOrder, tagErr)
				}
			} else {
				tagDone = true
			}
		}
	tagDone:

		moved := false
		if it.Changed {
			if err := moveMediaFile(it.OldPath, it.NewPath); err != nil {
				mediaLog(id, fmt.Sprintf("! 移动失败 %s：%v", it.OldName, err))
				setMediaState(id, func(st *MediaState) { st.Done = i + 1; st.Failed++ })
				continue
			}
			moved = true
			reserved[it.NewPath] = true
		}

		switch {
		case moved && it.NewName != it.OldName:
			mediaLog(id, fmt.Sprintf("✓ %s → %s", it.OldName, it.NewName))
		case moved:
			// 只挪目录、名字没变：日志里写出相对目录，避免看起来"改了个寂寞"
			mediaLog(id, fmt.Sprintf("✓ 归档 %s → %s", it.OldName, filepath.Join(filepath.Base(filepath.Dir(it.NewPath)), it.NewName)))
		case tagDone:
			mediaLog(id, fmt.Sprintf("✓ 标签已修正 %s", it.OldName))
		}
		setMediaState(id, func(st *MediaState) {
			st.Done = i + 1
			if moved {
				st.Renamed++
			}
			if tagDone {
				st.Tagged++
			}
			switch {
			case tagErr != "":
				st.Failed++ // 标签没写进去不算"跳过"，否则用户以为无事发生
			case unsupported:
				st.Unsupported++
			case !moved && !tagDone:
				st.Skipped++
			}
		})

		// 联网模式下每首之间有节流（MusicBrainz 1 req/s），避免被封
		if opts.Online && i+1 < len(files) {
			time.Sleep(120 * time.Millisecond)
		}
	}

	// 失败原因汇总：每个原因一行，失败数 > 0 时任务标记为"有错误"
	summary := ""
	for _, r := range tagFailOrder {
		if n := tagFailByReason[r]; n > 0 {
			line := fmt.Sprintf("标签写入失败 %d 个：%s", n, r)
			mediaLog(id, "! "+line)
			if summary == "" {
				summary = line
			}
		}
	}
	st := mediaStateOf(id)
	finishMediaTask(id, start, st.Failed == 0, summary)
	mediaLog(id, fmt.Sprintf("完成：重命名 %d，写标签 %d，跳过 %d，不支持写标签 %d，失败 %d",
		st.Renamed, st.Tagged, st.Skipped, st.Unsupported, st.Failed))
}

// finishMediaStopped 手动停止：不算错误，状态标记为「已停止」
func finishMediaStopped(id string, start time.Time) {
	mediaStore.mu.Lock()
	if mediaStore.cancel != nil {
		delete(mediaStore.cancel, id)
	}
	if mediaStore.busy != nil {
		delete(mediaStore.busy, id)
	}
	mediaStore.mu.Unlock()
	setMediaState(id, func(st *MediaState) {
		st.Running = false
		st.Phase = "done"
		st.Current = ""
		st.Stopped = true
		st.LastAt = time.Now()
		st.LastOK = true
		st.LastErr = ""
		st.Duration = time.Since(start).Round(time.Second).String()
	})
}

// finishMediaTask 收尾：清运行标记、更新统计
func finishMediaTask(id string, start time.Time, okFlag bool, errMsg string) {
	mediaStore.mu.Lock()
	if mediaStore.cancel != nil {
		delete(mediaStore.cancel, id)
	}
	mediaStore.mu.Unlock()
	setMediaState(id, func(st *MediaState) {
		st.Running = false
		st.Phase = "done"
		st.Current = ""
		st.LastAt = time.Now()
		st.LastOK = okFlag
		st.LastErr = errMsg
		st.Duration = time.Since(start).Round(time.Second).String()
	})
	mediaStore.mu.Lock()
	if mediaStore.busy != nil {
		delete(mediaStore.busy, id)
	}
	mediaStore.mu.Unlock()
}

// ---------- HTTP 接口 ----------

// handleMediaList GET /api/media/list → 任务与运行状态
func (s *Server) handleMediaList(w http.ResponseWriter, r *http.Request) {
	tasks := loadMediaTasks()
	states := map[string]MediaState{}
	for _, t := range tasks {
		states[t.ID] = mediaStateOf(t.ID)
	}
	ok(w, map[string]any{"tasks": tasks, "states": states})
}

// handleMediaSave POST /api/media/save → 新增/更新任务
func (s *Server) handleMediaSave(w http.ResponseWriter, r *http.Request) {
	var t MediaTask
	if err := json.NewDecoder(r.Body).Decode(&t); err != nil {
		fail(w, 400, "参数解析失败")
		return
	}
	t.ID = strings.TrimSpace(t.ID)
	t.Name = strings.TrimSpace(t.Name)
	t.Kind = strings.ToLower(strings.TrimSpace(t.Kind))
	t.Dir = filepath.Clean(strings.TrimSpace(t.Dir))
	t.Template = strings.TrimSpace(t.Template)
	t.Exclude = strings.TrimSpace(t.Exclude)

	if t.Kind != "video" && t.Kind != "audio" {
		fail(w, 400, "类型必须是 video 或 audio")
		return
	}
	dir, safe := safeDownloadDir(t.Dir)
	if !safe {
		fail(w, 400, "目录无效或属于系统敏感路径："+t.Dir)
		return
	}
	t.Dir = dir
	if t.ID == "" {
		t.ID = fmt.Sprintf("m%d", time.Now().UnixNano())
	}
	if t.Name == "" {
		label := "视频"
		if t.Kind == "audio" {
			label = "音频"
		}
		t.Name = filepath.Base(t.Dir) + " · " + label
	}
	if t.Template == "" && t.Kind == "audio" {
		t.Template = "{title} - {artist}"
	}

	tasks := loadMediaTasks()
	found := false
	for i := range tasks {
		if tasks[i].ID == t.ID {
			tasks[i] = t
			found = true
			break
		}
	}
	if !found {
		tasks = append(tasks, t)
		// 新任务不该继承任何历史状态（否则可能一加进来就显示"有错误"）
		mediaStore.mu.Lock()
		if mediaStore.state != nil {
			delete(mediaStore.state, t.ID)
		}
		mediaStore.mu.Unlock()
	}
	sort.Slice(tasks, func(i, j int) bool {
		if tasks[i].Kind != tasks[j].Kind {
			return tasks[i].Kind < tasks[j].Kind
		}
		return tasks[i].Name < tasks[j].Name
	})
	if err := saveMediaTasks(tasks); err != nil {
		fail(w, 500, "保存失败："+err.Error())
		return
	}
	ok(w, map[string]any{"id": t.ID, "task": t})
}

// handleMediaDelete POST /api/media/delete → 删除任务（不动文件）
func (s *Server) handleMediaDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || strings.TrimSpace(req.ID) == "" {
		fail(w, 400, "缺少任务 id")
		return
	}
	id := strings.TrimSpace(req.ID)
	mediaStore.mu.Lock()
	busy := mediaStore.busy[id]
	mediaStore.mu.Unlock()
	if busy {
		fail(w, 409, "任务正在运行，请先停止再删除")
		return
	}
	tasks := loadMediaTasks()
	out := tasks[:0]
	found := false
	for _, t := range tasks {
		if t.ID == id {
			found = true
			continue
		}
		out = append(out, t)
	}
	if !found {
		fail(w, 404, "任务不存在")
		return
	}
	if err := saveMediaTasks(out); err != nil {
		fail(w, 500, "删除失败："+err.Error())
		return
	}
	ok(w, map[string]any{"deleted": id})
}

// handleMediaRun POST /api/media/run?id=&dry=1 → 预览（dry）或执行
func (s *Server) handleMediaRun(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	dry := r.URL.Query().Get("dry") == "1"
	var task MediaTask
	found := false
	for _, t := range loadMediaTasks() {
		if t.ID == id {
			task = t
			found = true
			break
		}
	}
	if !found {
		fail(w, 404, "任务不存在")
		return
	}

	if dry {
		res, err := previewMediaTask(task, 300)
		if err != nil {
			fail(w, 500, "扫描失败："+err.Error())
			return
		}
		ok(w, res)
		return
	}

	mediaStore.mu.Lock()
	if mediaStore.busy == nil {
		mediaStore.busy = map[string]bool{}
	}
	if mediaStore.busy[id] {
		mediaStore.mu.Unlock()
		fail(w, 409, "该任务正在运行中，请等它完成")
		return
	}
	mediaStore.busy[id] = true
	mediaStore.mu.Unlock()

	go runMediaTask(task)
	ok(w, map[string]any{"started": id})
}

// handleMediaStop POST /api/media/stop?id= → 请求停止
func (s *Server) handleMediaStop(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	mediaStore.mu.Lock()
	ch := mediaStore.cancel[id]
	if ch != nil {
		delete(mediaStore.cancel, id) // 先摘掉再关闭，避免重复点击导致 close 已关闭通道
	}
	mediaStore.mu.Unlock()
	if ch == nil {
		fail(w, 404, "任务未在运行")
		return
	}
	close(ch)
	ok(w, map[string]any{"stopping": id})
}

// handleMediaStatus GET /api/media/status?id= → 运行状态（不传 id 返回全部）
func (s *Server) handleMediaStatus(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id != "" {
		ok(w, mediaStateOf(id))
		return
	}
	states := map[string]MediaState{}
	for _, t := range loadMediaTasks() {
		states[t.ID] = mediaStateOf(t.ID)
	}
	ok(w, map[string]any{"states": states})
}

// mediaRoots 可浏览的根目录（存在才列）
func mediaRoots() []string {
	out := []string{}
	for _, d := range []string{"/vol1", "/vol2", "/vol3", "/vol4", "/vol5", "/home"} {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		out = append(out, "/")
	}
	return out
}

// handleMediaDirs GET /api/media/dirs?path= → 目录浏览（面包屑选择器）
func (s *Server) handleMediaDirs(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimSpace(r.URL.Query().Get("path"))
	roots := mediaRoots()

	type dirEnt struct {
		Name  string `json:"name"`
		Path  string `json:"path"`
		Media int    `json:"media"` // 直属媒体文件数（不递归，仅作提示）
	}
	dirs := []dirEnt{}

	if path == "" {
		for _, rt := range roots {
			dirs = append(dirs, dirEnt{Name: rt, Path: rt, Media: countMediaDirect(rt)})
		}
		ok(w, map[string]any{"path": "", "parent": "", "roots": roots, "dirs": dirs})
		return
	}

	clean := filepath.Clean(path)
	if _, safe := safeDownloadDir(clean); !safe {
		// 根目录本身可能落在黑名单外但仍需可浏览；safeDownloadDir 已校验存在性
		fail(w, 400, "目录不可访问："+path)
		return
	}
	ents, err := os.ReadDir(clean)
	if err != nil {
		fail(w, 400, "读取目录失败："+err.Error())
		return
	}
	for _, e := range ents {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, ".") || strings.HasPrefix(name, "@") {
			continue // 隐藏目录与飞牛内部目录
		}
		full := filepath.Join(clean, name)
		dirs = append(dirs, dirEnt{Name: name, Path: full, Media: countMediaDirect(full)})
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Name < dirs[j].Name })

	parent := filepath.Dir(clean)
	if parent == clean {
		parent = ""
	}
	ok(w, map[string]any{"path": clean, "parent": parent, "roots": roots, "dirs": dirs})
}

// countMediaDirect 统计目录内直属媒体文件数（不递归，供选择器提示）
func countMediaDirect(dir string) int {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		if IsVideoExt(e.Name()) || IsAudioExt(e.Name()) {
			n++
		}
	}
	return n
}

// applyMediaImport 导入媒体任务（整体替换；运行中的任务不受影响）
func applyMediaImport(tasks []MediaTask) error {
	now := time.Now().UnixNano()
	for i := range tasks {
		if tasks[i].ID == "" {
			tasks[i].ID = fmt.Sprintf("m%d-%d", now%1000000, i)
		}
		if tasks[i].Kind != "video" && tasks[i].Kind != "audio" {
			tasks[i].Kind = "video"
		}
		if tasks[i].Template == "" && tasks[i].Kind == "audio" {
			tasks[i].Template = "{title} - {artist}"
		}
	}
	if err := saveMediaTasks(tasks); err != nil {
		return err
	}
	return nil
}
