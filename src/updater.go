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
		Name               string `json:"name"`
		BrowserDownloadURL string `json:"browser_download_url"`
		Size               int64  `json:"size"`
	} `json:"assets"`
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

func checkUpdate() UpdateInfo {
	info := UpdateInfo{Current: version, CheckedAt: time.Now().Format("2006-01-02 15:04:05")}

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
			info.PublishedAt = rel.PublishedAt
			for _, a := range rel.Assets {
				if strings.HasSuffix(a.Name, ".fpk") {
					info.AssetName = a.Name
					info.DownloadURL = a.BrowserDownloadURL
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
	info.PublishedAt = rel.PublishedAt
	for _, a := range rel.Assets {
		if strings.HasSuffix(a.Name, ".fpk") {
			info.AssetName = a.Name
			info.DownloadURL = a.BrowserDownloadURL
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
	for _, bad := range []string{"/", "/etc", "/usr", "/bin", "/sbin", "/boot", "/sys", "/proc", "/dev", "/lib", "/lib64", "/var/lib"} {
		if clean == bad || strings.HasPrefix(clean, bad+"/") {
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

// handleUpdateDownload POST /api/update/download {url,name,dir}：下载 fpk 到指定目录并校验。
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
	// 只允许 GitHub 官方域名下载（防 SSRF）
	if !strings.HasPrefix(req.URL, "https://github.com/"+ghRepo+"/releases/download/") {
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
		d, ok := safeDownloadDir(dir)
		if !ok {
			fail(w, 400, "保存目录无效（必须已存在且不能是系统目录）")
			return
		}
		dir = d
	} else {
		dir = updateDownloadDir()
	}
	dst := filepath.Join(dir, name)

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(req.URL)
	if err != nil {
		fail(w, 500, "下载失败："+err.Error())
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		fail(w, 500, fmt.Sprintf("下载失败（HTTP %d）", resp.StatusCode))
		return
	}
	f, err := os.Create(dst)
	if err != nil {
		fail(w, 500, "无法创建文件："+err.Error())
		return
	}
	n, err := io.Copy(f, io.LimitReader(resp.Body, 512<<20)) // 512MB 上限
	f.Close()
	if err != nil || n < 1024 {
		_ = os.Remove(dst)
		fail(w, 500, "下载不完整")
		return
	}
	// 校验：fpk 本质是 zip（文件头 PK\x03\x04）
	if !isZipFile(dst) {
		_ = os.Remove(dst)
		fail(w, 500, "下载的文件不是有效的 fpk（非 zip 格式）")
		return
	}
	ok(w, map[string]any{
		"message": "已下载到 " + dst,
		"path":    dst,
		"bytes":   n,
		"hint":    "请到飞牛 应用中心 → 手动安装 选择该文件完成覆盖安装（数据与配置保留）",
	})
}

func isZipFile(p string) bool {
	f, err := os.Open(p)
	if err != nil {
		return false
	}
	defer f.Close()
	magic := make([]byte, 4)
	if _, err := io.ReadFull(f, magic); err != nil {
		return false
	}
	return magic[0] == 'P' && magic[1] == 'K'
}
