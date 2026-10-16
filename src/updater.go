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
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	ghRepo      = "xmxiaom/X-Box"
	ghReleaseAPI = "https://api.github.com/repos/" + ghRepo + "/releases/latest"
)

// UpdateInfo 检查更新结果。
type UpdateInfo struct {
	Current    string `json:"current"`    // 本地版本
	Latest     string `json:"latest"`     // 远程最新版本（无 release 时为空）
	HasUpdate  bool   `json:"hasUpdate"`  // 是否有新版本
	Notes      string `json:"notes"`      // Release 说明
	AssetName  string `json:"assetName"`  // fpk 资源名
	DownloadURL string `json:"downloadUrl"` // fpk 下载地址（浏览器可下载 URL）
	PublishedAt string `json:"publishedAt"`
	Note       string `json:"note"`       // 无 release / 查询失败说明
	CheckedAt  string `json:"checkedAt"`
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

func checkUpdate() UpdateInfo {
	info := UpdateInfo{Current: version, CheckedAt: time.Now().Format("2006-01-02 15:04:05")}
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
		info.Note = fmt.Sprintf("GitHub 查询失败（HTTP %d）", resp.StatusCode)
		return info
	}
	var rel ghRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		info.Note = "GitHub 响应解析失败：" + err.Error()
		return info
	}
	info.Latest = strings.TrimPrefix(rel.TagName, "v")
	info.Notes = strings.TrimSpace(rel.Body)
	info.PublishedAt = rel.PublishedAt
	// 找 fpk 资源（优先 platform=all 单包）
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

// updateDownloadDir fpk 下载目录：优先 fnOS 应用数据目录（用户可通过文件管理访问）。
func updateDownloadDir() string {
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

// handleUpdateCheck GET /api/update/check（5 分钟缓存，避免频繁打 GitHub API）。
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

// handleUpdateDownload POST /api/update/download {url,name}：下载 fpk 到本地并校验。
func handleUpdateDownload(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL  string `json:"url"`
		Name string `json:"name"`
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
	dir := updateDownloadDir()
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
