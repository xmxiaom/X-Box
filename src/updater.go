package main

// 检查更新：对比本地版本与 GitHub Release，支持下载新版本 fpk 到本地，引导用户在
// 飞牛应用中心覆盖安装（不自动替换运行中的二进制——X-Box 服务自身正在运行，
// 自动安装有自杀风险；下载 + 手动覆盖安装是安全路径）。

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	ghRepo       = "xmxiaom/X-Box"
	ghReleaseAPI = "https://api.github.com/repos/" + ghRepo + "/releases/latest"
	// raw manifest：走 GitHub raw CDN，无 API 限流、无需认证，是版本检测的主路径
	ghRawManifest = "https://raw.githubusercontent.com/" + ghRepo + "/main/pkg/manifest"
)

// UpdateInfo 检查更新结果。
type UpdateInfo struct {
	Current     string `json:"current"`      // 本地版本
	Latest      string `json:"latest"`       // 远程最新版本（无 release 时为空）
	HasUpdate   bool   `json:"hasUpdate"`    // 是否有新版本
	Notes       string `json:"notes"`        // Release 说明
	AssetName   string `json:"assetName"`    // fpk 资源名
	DownloadURL string `json:"downloadUrl"`  // fpk 下载地址（浏览器可下载 URL）
	PublishedAt string `json:"publishedAt"`  // Release 发布时间
	Note        string `json:"note"`         // 说明（无 release / 降级提示等）
	CheckedAt   string `json:"checkedAt"`    // 检查时间
	Source      string `json:"source"`       // 版本来源：raw-manifest / release-api
}

var (
	updateCacheMu sync.Mutex
	updateCache   UpdateInfo
	updateCacheAt time.Time
)

// compareVer 语义化版本比较：a > b 返回 1，相等 0，a < b -1。非数字按 0。
func compareVer(a, b string) int {
	norm := func(s string) []int {
		s = strings.TrimPrefix(strings.TrimSpace(s), "v")
		var out []int
		for _, p := range strings.Split(s, ".") {
			n, _ := strconv.Atoi(strings.TrimSpace(p))
			out = append(out, n)
		}
		return out
	}
	va, vb := norm(a), norm(b)
	for i := 0; i < len(va) || i < len(vb); i++ {
		x, y := 0, 0
		if i < len(va) {
			x = va[i]
		}
		if i < len(vb) {
			y = vb[i]
		}
		if x != y {
			if x > y {
				return 1
			}
			return -1
		}
	}
	return 0
}

// ghRelease GitHub Release API 响应（只取需要的字段）。
type ghRelease struct {
	TagName     string `json:"tag_name"`
	Name        string `json:"name"`
	Body        string `json:"body"`
	PublishedAt string `json:"published_at"`
	Assets      []struct {
		ID                 int64  `json:"id"`
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	} `json:"assets"`
}

// ghAssetAPIURL 资产下载走 GitHub API 端点（Accept: application/octet-stream 返回二进制）。
// 不用 browser_download_url：它会 302 到 objects.githubusercontent.com，国内网络大量
// 不可达（更新下载 HTTP 0 的根因）；api.github.com 与版本检测同域，实测可达。
func ghAssetAPIURL(assetID int64) string {
	return fmt.Sprintf("https://api.github.com/repos/%s/releases/assets/%d", ghRepo, assetID)
}

// fmtGhTime GitHub 时间（RFC3339 UTC）→ 本地可读格式；解析失败原样返回。
func fmtGhTime(s string) string {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Local().Format("2006-01-02 15:04:05")
	}
	return s
}

// fetchRawVersion 从 raw manifest 读取远程版本号（无限流主路径）。
func fetchRawVersion() string {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(ghRawManifest)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	m := regexp.MustCompile(`(?m)^version\s*=\s*([0-9][0-9.]*)`).FindSubmatch(b)
	if len(m) < 2 {
		return ""
	}
	return string(m[1])
}

var updateInProgress int64 // atomic：检查更新进行中（同步引擎见此标记会让路）

func checkUpdate() UpdateInfo {
	info := UpdateInfo{Current: version, CheckedAt: time.Now().Format("2006-01-02 15:04:05")}
	// 检查更新需要外网带宽（GitHub），与同步争抢；进行中标记让同步引擎让路
	atomic.StoreInt64(&updateInProgress, 1)
	defer atomic.StoreInt64(&updateInProgress, 0)

	// 1) 主路径：raw manifest 版本（无 API 限流）
	latest := fetchRawVersion()
	if latest != "" {
		info.Latest = latest
		info.Source = "raw-manifest"
		info.HasUpdate = compareVer(latest, version) > 0
		if !info.HasUpdate {
			info.Note = "已是最新版本"
		}
	} else {
		// 2) 回退：releases/latest API（可能触发匿名限流 60 次/小时）
		info = checkUpdateViaAPI(info)
	}

	// 3) 有新版本时补充 Release 详情（说明/发布时间）；API 失败不影响判定，
	//    下载 URL 按约定规则拼出（release 页面本身不限流）
	if info.HasUpdate && info.Latest != "" {
		if rel, ok := fetchLatestRelease(); ok {
			info.Notes = strings.TrimSpace(rel.Body)
			info.PublishedAt = fmtGhTime(rel.PublishedAt)
			for _, a := range rel.Assets {
				if strings.HasSuffix(a.Name, ".fpk") {
					info.AssetName = a.Name
					info.DownloadURL = ghAssetAPIURL(a.ID) // API 资产端点（穿透 objects.githubusercontent.com 不可达）
					break
				}
			}
		} else {
			info.Note = "未能获取 Release 详情（可能触发 GitHub 匿名 API 限流），下载地址按版本号推断"
		}
		if info.DownloadURL == "" {
			info.AssetName = "X-Box.fpk"
			info.DownloadURL = fmt.Sprintf("https://github.com/%s/releases/download/v%s/X-Box.fpk", ghRepo, info.Latest)
		}
	}
	return info
}

// checkUpdateViaAPI releases/latest API 路径（raw 失败时的回退）。
func checkUpdateViaAPI(info UpdateInfo) UpdateInfo {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(ghReleaseAPI)
	if err != nil {
		info.Note = "无法连接 GitHub（网络或 DNS 异常）：" + err.Error()
		return info
	}
	defer resp.Body.Close()
	if resp.StatusCode == 404 {
		info.Note = "GitHub 仓库尚未发布任何 Release（维护者发版后即可检测更新）"
		return info
	}
	if resp.StatusCode != 200 {
		info.Note = fmt.Sprintf("GitHub 查询失败（HTTP %d，匿名 API 每小时限 60 次，可稍后再试）", resp.StatusCode)
		return info
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		info.Note = "GitHub 响应解析失败：" + err.Error()
		return info
	}
	info.Source = "release-api"
	info.Latest = strings.TrimPrefix(rel.TagName, "v")
	info.Notes = strings.TrimSpace(rel.Body)
	info.PublishedAt = fmtGhTime(rel.PublishedAt)
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, ".fpk") {
			info.AssetName = a.Name
			info.DownloadURL = ghAssetAPIURL(a.ID) // API 资产端点（穿透 objects.githubusercontent.com 不可达）
			break
		}
	}
	if info.Latest == "" {
		info.Note = "Release 无有效版本号"
		return info
	}
	info.HasUpdate = compareVer(info.Latest, version) > 0
	if !info.HasUpdate {
		info.Note = "已是最新版本"
	}
	return info
}

// fetchLatestRelease 取最新 Release 详情（限流/失败返回 ok=false，不影响主判定）。
func fetchLatestRelease() (ghRelease, bool) {
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(ghReleaseAPI)
	if err != nil || resp.StatusCode != 200 {
		if resp != nil {
			resp.Body.Close()
		}
		return ghRelease{}, false
	}
	defer resp.Body.Close()
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return ghRelease{}, false
	}
	return rel, true
}

// updateDownloadDir fpk 默认下载目录：优先应用自身数据目录（XBOX_DATA，fnOS pkgvar，
// 文件管理可直接找到），回退 @appdata 常规位置，最后 /tmp。
func updateDownloadDir() string {
	if d := os.Getenv("XBOX_DATA"); d != "" {
		dir := filepath.Join(d, "updates")
		_ = os.MkdirAll(dir, 0755)
		return dir
	}
	for _, base := range []string{"/vol1/@appdata/X-Box", "/vol2/@appdata/X-Box"} {
		dir := filepath.Join(base, "updates")
		if fi, err := os.Stat(base); err == nil && fi.IsDir() {
			_ = os.MkdirAll(dir, 0755)
			return dir
		}
	}
	dir := "/tmp/X-Box-updates"
	_ = os.MkdirAll(dir, 0755)
	return dir
}

// safeDownloadDir 用户自定义目录校验：必须已存在、是目录、且不得是系统敏感目录
// （防通过下载接口向任意位置写文件）。
func safeDownloadDir(dir string) (string, bool) {
	if dir == "" {
		return "", false
	}
	clean := filepath.Clean(dir)
	if !filepath.IsAbs(clean) {
		return "", false
	}
	// 系统敏感目录黑名单（这些位置一律不接受）
	badSys := []string{"/", "/etc", "/usr", "/bin", "/sbin", "/boot", "/sys", "/proc", "/dev", "/lib", "/lib64", "/var/lib"}
	lower := strings.ToLower(clean)
	for _, bad := range badSys {
		if clean == bad || strings.HasPrefix(clean, bad+"/") {
			return "", false
		}
	}
	// Windows 系统盘敏感目录（运行环境是 Linux fnOS，跑 Windows 单测时也守住底线；
	// 注意不含 C:\USERS：用户目录下本就是合法的下载位置）
	for _, bad := range []string{"C:\\WINDOWS", "C:\\PROGRAM FILES", "C:\\PROGRAM FILES (X86)", "C:\\PROGRAMDATA"} {
		if lower == strings.ToLower(bad) || strings.HasPrefix(lower, strings.ToLower(bad)+"\\") {
			return "", false
		}
	}
	fi, err := os.Stat(clean)
	if err != nil || !fi.IsDir() {
		return "", false
	}
	return clean, true
}

// updateDirCandidates 可选保存目录（供前端下拉；仅列真实存在的）。
func updateDirCandidates() []string {
	var out []string
	for _, d := range []string{
		"/vol1/@appdata/X-Box/updates", "/vol2/@appdata/X-Box/updates",
		"/vol1", "/vol2", "/tmp",
	} {
		if fi, err := os.Stat(d); err == nil && fi.IsDir() {
			out = append(out, d)
		}
	}
	if len(out) == 0 {
		out = append(out, updateDownloadDir())
	}
	return out
}

// handleUpdateCheck GET /api/update/check（5 分钟缓存，避免频繁打 GitHub）。
func handleUpdateCheck(w http.ResponseWriter, r *http.Request) {
	updateCacheMu.Lock()
	if !updateCacheAt.IsZero() && time.Since(updateCacheAt) < 5*time.Minute {
		info := updateCache
		updateCacheMu.Unlock()
		ok(w, map[string]any{"update": info, "cached": true})
		return
	}
	updateCacheMu.Unlock()

	info := checkUpdate()

	updateCacheMu.Lock()
	updateCache, updateCacheAt = info, time.Now()
	updateCacheMu.Unlock()
	ok(w, map[string]any{"update": info})
}

// handleUpdateDirs GET /api/update/dirs：可选保存目录列表（NAS 存储目录）。
func handleUpdateDirs(w http.ResponseWriter, r *http.Request) {
	ok(w, map[string]any{"dirs": updateDirCandidates()})
}

// handleUpdateFile GET /api/update/file?path=...：把已下载的 fpk 经本服务回传给浏览器
// （"下载到电脑"的可靠路径——iframe/APP WebView 会拦截 window.open 外链，
// 同源 attachment 响应则可正常触发本机下载）。
// 安全：只允许回传应用 updates 目录内的 .fpk 文件，防任意文件读取。
func handleUpdateFile(w http.ResponseWriter, r *http.Request) {
	reqPath := r.URL.Query().Get("path")
	if reqPath == "" {
		fail(w, 400, "缺少 path 参数")
		return
	}
	clean := filepath.Clean(reqPath)
	if !strings.HasSuffix(clean, ".fpk") || !filepath.IsAbs(clean) {
		fail(w, 400, "仅支持回传 fpk 文件")
		return
	}
	// 必须落在应用 updates 目录内
	allowed := false
	for _, base := range []string{updateDownloadDir(), "/vol1/@appdata/X-Box/updates", "/vol2/@appdata/X-Box/updates", "/tmp/X-Box-updates"} {
		if base == "" {
			continue
		}
		b := filepath.Clean(base)
		if clean == b {
			continue
		}
		if strings.HasPrefix(clean, b+string(filepath.Separator)) {
			allowed = true
			break
		}
	}
	if !allowed {
		fail(w, 403, "该路径不在允许的更新目录内")
		return
	}
	fi, err := os.Stat(clean)
	if err != nil || fi.IsDir() {
		fail(w, 404, "文件不存在")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", `attachment; filename="`+filepath.Base(clean)+`"`)
	w.Header().Set("Content-Length", strconv.FormatInt(fi.Size(), 10))
	http.ServeFile(w, r, clean)
}

// ---- 异步下载任务（同一时间仅一个）----
// 客户端传输通道有 8-10s 超时（fnOS 中转环境下必须短超时防挂死），而 fpk 下载
// 常耗时数十秒以上——同步下载必然「HTTP 0/TIMEOUT」（v1.2.17 之后下载全挂的
// 根因）。改为后台任务 + 轮询：POST 立即返回，GET /api/update/status 查进度。
var (
	dlMu    sync.Mutex
	dlRun   bool
	dlDone  bool
	dlErr   string
	dlPath  string
	dlName  string
	dlStart time.Time // dlRun 置 true 的时刻（看门狗：running 超时强制复位，防单例锁残留）
	dlPW    *progressWriter // 下载进度计数器（运行中非 nil）
)

// progressWriter 下载字节数计数（读取需持 dlMu）。
type progressWriter struct{ n int64 }

func (p *progressWriter) Write(b []byte) (int, error) {
	p.n += int64(len(b))
	return len(b), nil
}

// handleUpdateStatus GET /api/update/status：查询后台下载任务进度/结果。
func handleUpdateStatus(w http.ResponseWriter, r *http.Request) {
	dlMu.Lock()
	// 看门狗：dlRun 理论上应在成功（updFinishPart）或失败（setFail）后复位，
	// 但后台 goroutine 若因未知原因卡死/异常，单例锁会永远 true → 之后所有下载都 409。
	// 这里若 running 超过更新下载整体时限 + 5min 缓冲仍未结束，视为异常残留，强制复位并报错。
	if dlRun && time.Since(dlStart) > updDlDeadline+5*time.Minute {
		dlRun, dlDone, dlErr = false, true, "下载任务异常卡死（超过时限无结果），已自动清理，请重新尝试"
		dlPW = nil
	}
	st := map[string]any{"running": dlRun, "done": dlDone, "bytes": int64(0), "path": "", "name": ""}
	if dlPW != nil {
		st["bytes"] = dlPW.n
	}
	if dlErr != "" {
		st["error"] = dlErr
	}
	if dlPath != "" {
		st["path"] = dlPath
	}
	if dlName != "" {
		st["name"] = dlName
	}
	dlMu.Unlock()
	ok(w, st)
}

// handleUpdateDownload POST /api/update/download {url,name,dir}：校验后启动后台
// 下载任务并立即返回（进度经 /api/update/status 轮询）。
// dir 为空时用默认目录；自定义目录必须已存在且非系统敏感目录。
func handleUpdateDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL  string `json:"url"`
		Name string `json:"name"`
		Dir  string `json:"dir"` // 可选：自定义保存目录（NAS 存储目录）
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.URL == "" {
		fail(w, 400, "参数错误")
		return
	}
	// 只允许 GitHub 官方下载地址（防 SSRF）：
	//   1) api.github.com 资产端点（首选，穿透 objects.githubusercontent.com 不可达）
	//   2) github.com/.../releases/download/ 直链（兜底）
	apiAssetPrefix := "https://api.github.com/repos/" + ghRepo + "/releases/assets/"
	dlPrefix := "https://github.com/" + ghRepo + "/releases/download/"
	isAPI := strings.HasPrefix(req.URL, apiAssetPrefix) && strings.TrimPrefix(req.URL, apiAssetPrefix) != "" && isAllDigits(strings.TrimPrefix(req.URL, apiAssetPrefix))
	if !isAPI && !strings.HasPrefix(req.URL, dlPrefix) {
		fail(w, 400, "仅支持从 GitHub Release 下载")
		return
	}
	name := filepath.Base(req.Name)
	if name == "" || name == "." || !strings.HasSuffix(name, ".fpk") {
		fail(w, 400, "非法文件名")
		return
	}
	dir := req.Dir
	if dir != "" {
		d, okd := safeDownloadDir(dir)
		if !okd {
			fail(w, 400, "保存目录无效（必须已存在且不能是系统目录）")
			return
		}
		dir = d
	} else {
		dir = updateDownloadDir()
	}
	dst := filepath.Join(dir, name)

	dlMu.Lock()
	if dlRun {
		dlMu.Unlock()
		fail(w, 409, "已有下载任务进行中，请稍候（可稍后刷新状态）")
		return
	}
	dlRun, dlDone, dlErr, dlPath, dlName, dlPW = true, false, "", "", name, &progressWriter{}
	dlStart = time.Now()
	dlMu.Unlock()
	go runDownload(req.URL, isAPI, dst)
	ok(w, map[string]any{"started": true, "message": "已开始后台下载 " + name + "（可关闭此页，下载在 NAS 上继续）"})
}

// runDownload 执行后台下载（结果写回全局状态）。
// v1.3.49 起改用与音乐下载器同源的断点续传方案（.part + HTTP Range + 自动重试）：
// 此前是一次性 GET（10min 总超时、无续传），GitHub 大文件传输途中任何一次抖动
// 都整单作废——这就是「更新下载不稳定」的根因。现在：网络中断自动从 .part 续传，
// 30s 无进展判定空转主动重试，全部字节到齐才落正名。
// v1.3.51 起加多源轮换：api.github.com 资产端点 302 后真实字节仍来自
// objects.githubusercontent.com（国内大量不可达，「下载老是报错」的根子），
// 候选链 = S3 签名直链 → 公共加速镜像 × N + github 直链 → 原始 URL 兜底，
// 每轮换一个源；同一 Release 资产字节一致，断点续传跨源续接。
var downloadInProgress int64 // atomic：后台下载 fpk 进行中（同步引擎让路）

const (
	updDlMaxAttempts = 18               // 单次任务最多尝试轮数（每轮失败都从 .part 续；多源后轮数加多）
	updDlDeadline    = 30 * time.Minute // 整体时限（防死循环占后台）
	updDlIdleWatch   = 30 * time.Second // 读空转看门狗：30s 一个字节都没来就断开重试
	updDlMaxSize     = 512 << 20        // fpk 大小上限
)

// updMirrors 公共 GitHub 加速镜像（仅代理 github.com 直链；个别失效会被源轮换自然跳过）。
func updMirrors() []string {
	return []string{"https://ghfast.top/", "https://gh-proxy.com/", "https://ghproxy.net/"}
}

// updCandidates 构建下载候选源（按优先级去重）。
func updCandidates(rawURL string, isAPI bool) []string {
	ghDirect := "" // github.com/.../releases/download/... 直链（镜像可代理的形态）
	if isAPI {
		// 资产元数据（默认 Accept 即 JSON）：拿 browser_download_url 推导直链
		cli := &http.Client{Timeout: 15 * time.Second}
		if resp, err := cli.Get(rawURL); err == nil {
			var meta struct {
				BrowserDownloadURL string `json:"browser_download_url"`
			}
			if json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&meta) == nil {
				if u := meta.BrowserDownloadURL; strings.HasPrefix(u, "https://github.com/"+ghRepo+"/releases/download/") {
					ghDirect = u
				}
			}
			resp.Body.Close()
		}
	} else {
		ghDirect = rawURL
	}
	var out []string
	// S3 签名直链：资产端点不跟随 302 拿 Location——绕开 api.github.com 每小时 60 次匿名限流，
	// 且后续续传轮直接打 S3（Range 可用）
	if isAPI {
		cli := &http.Client{Timeout: 15 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}}
		if req, err := http.NewRequest("GET", rawURL, nil); err == nil {
			req.Header.Set("Accept", "application/octet-stream")
			if resp, err := cli.Do(req); err == nil {
				if resp.StatusCode >= 300 && resp.StatusCode < 400 {
					if loc := resp.Header.Get("Location"); loc != "" {
						out = append(out, loc)
					}
				}
				resp.Body.Close()
			}
		}
	}
	if ghDirect != "" {
		for _, m := range updMirrors() {
			out = append(out, m+ghDirect)
		}
		out = append(out, ghDirect)
	}
	out = append(out, rawURL)
	seen := map[string]bool{}
	uniq := out[:0]
	for _, u := range out {
		if u != "" && !seen[u] {
			seen[u] = true
			uniq = append(uniq, u)
		}
	}
	return uniq
}

func runDownload(rawURL string, isAPI bool, dst string) {
	atomic.StoreInt64(&downloadInProgress, 1)
	defer atomic.StoreInt64(&downloadInProgress, 0)
	setFail := func(msg string) {
		dlMu.Lock()
		dlRun, dlDone, dlErr = false, true, msg
		dlMu.Unlock()
	}
	// 兜底：任何 panic 都不能让 dlRun 卡 true——否则之后的所有下载都返回 409「进行中」，
	// 表现正是「完全停止应用重开才能正常下载」（后台 goroutine panic 后单例锁永不复位）。
	defer func() {
		if r := recover(); r != nil {
			setFail("下载内部异常：" + fmt.Sprint(r))
		}
	}()
	setStage := func(bytes int64) {
		dlMu.Lock()
		if dlPW != nil {
			dlPW.n = bytes
		}
		dlMu.Unlock()
	}
	part := dst + ".part"
	deadline := time.Now().Add(updDlDeadline)
	client := &http.Client{Timeout: 0} // 不设总超时：慢但在动就算活着，空转由看门狗管
	cands := updCandidates(rawURL, isAPI)
	var whys []string // 各源失败原因聚合（写进最终错误，用户可自查）
	for attempt := 1; attempt <= updDlMaxAttempts && time.Now().Before(deadline); attempt++ {
		// 续传起点：.part 现有字节数（换源不断点：同一资产字节一致，Range 按字节偏移）
		var off int64
		if fi, err := os.Stat(part); err == nil {
			off = fi.Size()
			if off > updDlMaxSize {
				os.Remove(part) // 脏数据，从头来
				off = 0
			}
		}
		setStage(off)
		src := cands[(attempt-1)%len(cands)] // 每轮换一个源：单个源抽风时其余源接着下
		httpReq, err := http.NewRequest("GET", src, nil)
		if err != nil {
			setFail("下载失败：" + err.Error())
			return
		}
		if isAPI && src == rawURL {
			httpReq.Header.Set("Accept", "application/octet-stream") // API 资产端点：带此头返回二进制，否则 JSON
		}
		if off > 0 {
			httpReq.Header.Set("Range", "bytes="+strconv.FormatInt(off, 10)+"-")
		}
		resp, err := client.Do(httpReq)
		if err != nil {
			whys = append(whys, src[:minI(60, len(src))]+"："+err.Error())
			time.Sleep(3 * time.Second) // 网络抖动：稍候换源从 .part 继续
			continue
		}
		// 416：.part 已到（或超过）文件总长——多半是上轮恰好下完但没来得及收尾
		if resp.StatusCode == 416 {
			resp.Body.Close()
			if updFinishPart(part, dst, setFail) {
				return
			}
			os.Remove(part) // 不是完整 fpk，重来
			continue
		}
		if resp.StatusCode != 200 && resp.StatusCode != 206 {
			resp.Body.Close()
			whys = append(whys, fmt.Sprintf("%s：HTTP %d", src[:minI(60, len(src))], resp.StatusCode))
			time.Sleep(2 * time.Second)
			continue // 该源不可用/限流：换下一源（不再整单立即失败）
		}
		// 200 = 服务端忽略 Range 从头给 → 覆盖 .part 起点归零；206 = 续传被接受
		base := off
		if resp.StatusCode == 200 {
			base = 0
		}
		var expect int64 = -1
		if resp.ContentLength > 0 {
			expect = base + resp.ContentLength
		}
		n, idle, cerr := updCopyWithWatch(resp.Body, part, base, setStage)
		resp.Body.Close()
		got := base + n
		complete := cerr == nil && (got == expect || (expect < 0 && cerr == nil && !idle && n > 0))
		if complete {
			dlMu.Lock()
			dlPath = dst
			dlMu.Unlock()
			updFinishPart(part, dst, setFail) // 校验+落正名（内部已置终态）
			return
		}
		// 未完整：空转/中断——保留 .part 下一轮换源续传；短退避防打死服务器
		if cerr != nil {
			whys = append(whys, src[:minI(60, len(src))]+"："+cerr.Error())
		} else if idle {
			whys = append(whys, src[:minI(60, len(src))]+"：30s 无数据")
		}
		backoff := time.Duration(attempt*2) * time.Second
		if backoff > 12*time.Second {
			backoff = 12 * time.Second
		}
		time.Sleep(backoff)
	}
	reason := ""
	if len(whys) > 0 {
		// 只保留最后 4 条，避免刷屏
		if len(whys) > 4 {
			whys = whys[len(whys)-4:]
		}
		reason = "近期失败原因：" + strings.Join(whys, "；")
	}
	setFail("下载未完成：网络多次中断（已轮换源重试 " + strconv.Itoa(updDlMaxAttempts) + " 轮）。" + reason + " 进度已保留，点「重试」会按原设置接着下。")
}

func minI(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// updCopyWithWatch 把响应体追加进 .part（base 起点前已存在）。
// 返回 (本轮字节数, 是否空转终止, 错误)。空转 = idleWatch 内一个字节都没收到。
func updCopyWithWatch(body io.Reader, part string, base int64, setStage func(int64)) (int64, bool, error) {
	flags := os.O_CREATE | os.O_WRONLY
	if base > 0 {
		flags |= os.O_APPEND
	} else {
		flags |= os.O_TRUNC
	}
	f, err := os.OpenFile(part, flags, 0o644)
	if err != nil {
		return 0, false, err
	}
	defer f.Close()
	var n int64
	buf := make([]byte, 256<<10) // 256KB 缓冲：减少小块写系统调用（64KB 在 SD 卡/NAS 上尾段写放大明显）
	last := time.Now()
	lastStage := time.Now()
	for {
		nw, rerr := body.Read(buf)
		if nw > 0 {
			if _, werr := f.Write(buf[:nw]); werr != nil {
				return n, false, werr
			}
			n += int64(nw)
			last = time.Now()
			// 进度刷新节流：≥512KB 或 ≥1s 才更新一次，避免每 256KB 抢全局锁
			// 跟 handleUpdateStatus 轮询争锁（高频 setStage 在高速盘上产生锁抖动）。
			if n >= 512<<10 || time.Since(lastStage) > time.Second {
				setStage(base + n)
				lastStage = time.Now()
			}
			if base+n > updDlMaxSize {
				return n, false, fmt.Errorf("超出大小上限")
			}
		}
		if rerr == io.EOF {
			setStage(base + n) // 终值铺满（末段可能不足 512KB/1s 未刷新）
			return n, false, nil
		}
		if rerr != nil {
			setStage(base + n)
			return n, false, rerr
		}
		if time.Since(last) > updDlIdleWatch {
			setStage(base + n)
			return n, true, nil // 空转：主动断开，外层续传
		}
	}
}

// updFinishPart 校验 .part 是有效 fpk 后落正名。返回 true 表示流程终结（成功或已报错）。
func updFinishPart(part, dst string, setFail func(string)) bool {
	if !isFpkFile(part) {
		setFail("下载的文件不是有效的 fpk（非 zip/gzip 格式）")
		os.Remove(part)
		return true
	}
	if err := os.Rename(part, dst); err != nil {
		// 目标可能已存在（上轮残留）：覆盖一次
		os.Remove(dst)
		if err2 := os.Rename(part, dst); err2 != nil {
			setFail("保存文件失败：" + err2.Error())
			return true
		}
	}
	dlMu.Lock()
	dlRun, dlDone, dlErr = false, true, ""
	dlMu.Unlock()
	return true
}

// isAllDigits 判断字符串是否全为数字（资产 ID 校验）。
func isAllDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// isFpkFile 校验 fpk 文件：fnpack 打出的 fpk 是 **gzip** 容器（1F 8B），
// 少数版本为 zip（PK\x03\x04）——两者都接受（曾只认 zip 头，把正常 fpk 误判为无效文件删除）。
func isFpkFile(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return false
	}
	// ZIP: PK\x03\x04
	if magic[0] == 'P' && magic[1] == 'K' && magic[2] == 0x03 && magic[3] == 0x04 {
		return true
	}
	// GZIP: 1F 8B
	if magic[0] == 0x1F && magic[1] == 0x8B {
		return true
	}
	return false
}
