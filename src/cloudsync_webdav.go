package main

// 直接按 WebDAV 协议访问平台的网盘接入服务，拿到远端真实目录并下载文件。
//
// 为什么要走这条路：挂载链路是「挂载服务 → 平台本机 WebDAV 接入 → 网盘」。
// 实测刷新挂载服务的目录缓存返回成功，但挂载目录里依旧看不到网盘侧刚转存的文件，
// 说明缓存压在平台这层接入服务里，靠挂载服务的手段改不了它。
//
// 而接入服务本身就是标准 WebDAV 服务（只监听本机），应用与它在同一台机器上，
// 可以按协议直接访问：PROPFIND 列目录、GET 取文件。这条路完全不经过挂载目录，
// 因此不受那层目录缓存影响，也不依赖挂载服务是否支持某些控制接口。
//
// 凭据取自平台为挂载准备的那份配置（应用以 root 运行可读），不需要用户登录。

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type davEP struct {
	base string // http://localhost:15244
	root string // /cloud-storage/v1/dav
	user string
	pass string
}

var (
	davEpMu    sync.Mutex
	davEpCache = map[string]davEP{} // key: 挂载源
)

// davEndpointFor 根据源目录找到它所属挂载对应的 WebDAV 接入地址与凭据
func davEndpointFor(srcPath string) (ep davEP, rel string, ok bool) {
	mp, remote, root := mountSourceOf(srcPath)
	if mp == "" || remote == "" || root == "" {
		return davEP{}, "", false
	}
	if !strings.HasPrefix(root, "/") {
		root = "/" + root
	}

	davEpMu.Lock()
	cached, has := davEpCache[remote]
	davEpMu.Unlock()
	if has {
		return cached, davRel(mp, srcPath), true
	}

	cfg := findRemoteConfig(remote)
	if cfg == nil {
		return davEP{}, "", false
	}
	ep = davEP{root: root, user: cfg["user"], pass: revealPass(cfg["pass"])}

	// 配置里写的地址未必是应用能直接连的那个（可能只给挂载进程内部用、
	// 也可能实际走的是本机 socket）。这里按候选顺序逐个探一次，用能通的。
	for _, base := range davCandidateBases(cfg, ep) {
		if davReachable(base, ep) {
			ep.base = base
			davEpMu.Lock()
			davEpCache[remote] = ep
			davEpMu.Unlock()
			return ep, davRel(mp, srcPath), true
		}
	}
	return davEP{}, "", false
}

// davCandidateBases 生成要尝试的接入地址：配置里写的优先，其次本机监听端口 / socket
func davCandidateBases(cfg map[string]string, ep davEP) []string {
	var out []string
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" {
			return
		}
		v = strings.TrimRight(v, "/")
		for _, e := range out {
			if e == v {
				return
			}
		}
		out = append(out, v)
	}

	if u := cfg["url"]; u != "" {
		add(u)
	}
	for _, k := range []string{"unix_socket", "unix-socket", "socket", "addr"} {
		if v := cfg[k]; v != "" {
			if strings.HasPrefix(v, "/") {
				add("unix://" + v)
			} else {
				add(v)
			}
		}
	}
	// 配置里的地址连不上（例如写的是挂载进程内部用的地址）时，
	// 退而扫描本机正在监听的端口与 socket，找出真正提供接入服务的那个。
	ports := localListenPorts()
	if len(ports) > 25 { // 逐个探测有成本
		ports = ports[:25]
	}
	for _, p := range ports {
		add(fmt.Sprintf("http://127.0.0.1:%d", p))
	}
	for _, sp := range localDavSockets() {
		add("unix://" + sp)
	}
	return out
}

// davReachable 探一下某个地址能不能提供接入服务（207/401/403 都算能连上）
func davReachable(base string, ep davEP) bool {
	probe := davEP{base: base, root: ep.root, user: ep.user, pass: ep.pass}
	cl := davHTTPClient(probe, 1200*time.Millisecond)
	req, err := http.NewRequest("PROPFIND", davURL(probe, ""), strings.NewReader(davPropfindBody))
	if err != nil {
		return false
	}
	req.Header.Set("Depth", "0")
	if probe.user != "" {
		req.SetBasicAuth(probe.user, probe.pass)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	return resp.StatusCode == 207 || resp.StatusCode == 401 || resp.StatusCode == 403 || resp.StatusCode == 405
}

// davRel 源目录相对挂载根的路径（"" 表示挂载根）
func davRel(mountPoint, srcPath string) string {
	r := strings.TrimPrefix(path.Clean(srcPath), path.Clean(mountPoint))
	r = strings.TrimPrefix(r, "/")
	if r == "." {
		r = ""
	}
	return r
}

// findRemoteConfig 在平台为挂载准备的配置里查某个挂载的 url/user/pass
func findRemoteConfig(remote string) map[string]string {
	files, _ := filepath.Glob("/etc/mountmgr/rclone/*.conf")
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		if cfg := parseIniSection(string(b), remote); cfg != nil {
			return cfg
		}
	}
	return nil
}

// parseIniSection 从 ini 内容里取指定 section 的键值
func parseIniSection(content, section string) map[string]string {
	out := map[string]string{}
	in := false
	for _, line := range strings.Split(content, "\n") {
		t := strings.TrimSpace(line)
		if t == "" || strings.HasPrefix(t, "#") || strings.HasPrefix(t, ";") {
			continue
		}
		if strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]") {
			in = strings.TrimSpace(t[1:len(t)-1]) == section
			continue
		}
		if !in {
			continue
		}
		if i := strings.Index(t, "="); i > 0 {
			k := strings.TrimSpace(t[:i])
			v := strings.TrimSpace(t[i+1:])
			out[strings.ToLower(k)] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// revealPass 配置里的密码是加密串，用平台自带的挂载组件解密；
// 解不开就按明文用（有些部署直接写明文）。
func revealPass(v string) string {
	if v == "" {
		return ""
	}
	for _, bin := range []string{"/usr/bin/rclone", "/usr/local/bin/rclone", "rclone", "/usr/trim/bin/rclone"} {
		ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		out, err := exec.CommandContext(ctx, bin, "reveal", v).Output()
		cancel()
		if err == nil {
			return strings.TrimSpace(string(out))
		}
	}
	return v
}

// ---------- PROPFIND ----------

const davPropfindBody = `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:"><D:prop>
<D:resourcetype/><D:getcontentlength/><D:getlastmodified/>
</D:prop></D:propfind>`

type davItem struct {
	rel   string
	size  int64
	isDir bool
	mod   time.Time
}

type davMultistatus struct {
	Responses []struct {
		Href     string `xml:"href"`
		Propstat []struct {
			Prop struct {
				ContentLength string `xml:"getcontentlength"`
				LastModified  string `xml:"getlastmodified"`
				ResourceType  struct {
					Collection *struct{} `xml:"collection"`
				} `xml:"resourcetype"`
			} `xml:"prop"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// davHTTPClient 按接入地址类型返回客户端：要么普通 HTTP，要么走本机 socket
func davHTTPClient(ep davEP, timeout time.Duration) *http.Client {
	if strings.HasPrefix(ep.base, "unix://") {
		sock := strings.TrimPrefix(ep.base, "unix://")
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 3 * time.Second}
				return d.DialContext(ctx, "unix", sock)
			},
		}
		return &http.Client{Timeout: timeout, Transport: tr}
	}
	return &http.Client{Timeout: timeout}
}

// davURL 拼接入服务上的请求地址。
// 平台配置里的 url 有时已经把根路径写进去了，这里避免重复拼接。
func davURL(ep davEP, rel string) string {
	base := strings.TrimRight(ep.base, "/")
	if strings.HasPrefix(base, "unix://") {
		base = "http://unix" // 走 socket 时只用作 HTTP 主机名
	}
	root := "/" + strings.Trim(strings.TrimSpace(ep.root), "/")
	if root != "/" && !strings.HasSuffix(base, root) {
		base += root
	}
	if rel == "" {
		return base + "/" // 目录请求带尾斜杠，避免被重定向
	}
	return base + "/" + davEscape(rel)
}

// davRelOf 把响应里的 href 规整成「相对接入根」的路径。
// 不同实现对 href 的写法不一致（有的带根路径前缀、有的只给服务器绝对路径），
// 这里统一去掉可能的根前缀，只保留可比较的部分。
func davRelOf(href, root string) string {
	p := href
	if u, err := url.Parse(href); err == nil && u.Path != "" {
		p = u.Path
	}
	if d, err := url.PathUnescape(p); err == nil {
		p = d
	}
	p = strings.TrimSuffix(p, "/")
	if r := "/" + strings.Trim(strings.TrimSpace(root), "/"); r != "/" && strings.HasPrefix(p, r) {
		p = strings.TrimPrefix(p, r)
	}
	return strings.Trim(p, "/")
}

// davListOnce 列一个目录的直接子项（Depth: 1）。
// 按协议，Depth 1 的响应是「自身 + 直接子项」，所以只按名字取子项即可，
// 不依赖各实现对 href 前缀的不同写法。
func davListOnce(cl *http.Client, ep davEP, rel string) ([]davItem, int, error) {
	target := davURL(ep, rel)
	req, err := http.NewRequest("PROPFIND", target, strings.NewReader(davPropfindBody))
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml; charset=utf-8")
	// 关键：明确要求不要用缓存副本，尽量让接入服务回源
	req.Header.Set("Cache-Control", "no-cache, no-store, max-age=0")
	req.Header.Set("Pragma", "no-cache")
	if ep.user != "" {
		req.SetBasicAuth(ep.user, ep.pass)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode != 207 {
		return nil, resp.StatusCode, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var ms davMultistatus
	if err := xml.Unmarshal(body, &ms); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("响应无法解析")
	}
	self := strings.Trim(strings.TrimSpace(rel), "/")
	var out []davItem
	for _, r := range ms.Responses {
		p := davRelOf(r.Href, ep.root)
		if p == self {
			continue // 自身
		}
		name := path.Base(p)
		if name == "" || name == "." || name == "/" {
			continue
		}
		it := davItem{rel: name}
		if strings.HasSuffix(r.Href, "/") {
			it.isDir = true // 有些实现靠尾斜杠表示目录
		}
		for _, ps := range r.Propstat {
			if ps.Prop.ResourceType.Collection != nil {
				it.isDir = true
			}
			if ps.Prop.ContentLength != "" {
				fmt.Sscanf(ps.Prop.ContentLength, "%d", &it.size)
			}
			if t, perr := http.ParseTime(ps.Prop.LastModified); perr == nil {
				it.mod = t
			}
		}
		out = append(out, it)
	}
	return out, resp.StatusCode, nil
}

// davEscape 逐段转义路径（保留分隔符）
func davEscape(p string) string {
	segs := strings.Split(path.Clean(p), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// davWalk 递归收集文件（限制条数与深度，避免异常时无限遍历）。
// 返回失败的子目录数，便于判断「列出得比本地少」是漏读还是确实没有。
func davWalk(cl *http.Client, ep davEP, base, prefix string, depth int, out *[]davItem, limit int) int {
	if depth > 8 || len(*out) >= limit {
		return 0
	}
	items, _, err := davListOnce(cl, ep, base)
	if err != nil {
		return 1
	}
	fails := 0
	for _, it := range items {
		rel := it.rel
		if prefix != "" {
			rel = prefix + "/" + it.rel
		}
		if it.isDir {
			fails += davWalk(cl, ep, path.Join(base, it.rel), rel, depth+1, out, limit)
			continue
		}
		it.rel = rel
		*out = append(*out, it)
		if len(*out) >= limit {
			break
		}
	}
	return fails
}

// davDownload 经接入服务下载到本地（先写临时文件再改名，避免半截文件）
func davDownload(cl *http.Client, ep davEP, rel, dst string) (int64, error) {
	target := ep.base + ep.root + "/" + davEscape(rel)
	req, err := http.NewRequest("GET", target, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Cache-Control", "no-cache, no-store, max-age=0")
	req.Header.Set("Pragma", "no-cache")
	if ep.user != "" {
		req.SetBasicAuth(ep.user, ep.pass)
	}
	resp, err := cl.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return 0, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return 0, err
	}
	tmp := dst + ".xboxsync.tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return 0, err
	}
	n, cerr := io.Copy(f, resp.Body)
	serr := f.Close()
	if cerr != nil || serr != nil {
		_ = os.Remove(tmp)
		if cerr != nil {
			return 0, cerr
		}
		return 0, serr
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return 0, err
	}
	return n, nil
}

// davFill 直连接入服务：列出远端文件，把本地缺失的下载过来。
// 返回补收数量、字节数、结论；ok=false 表示这条路径不可用（地址/凭据取不到）。
func davFill(task SyncTask, excludes []string, have map[string]struct{},
	idx, newIdx map[string]syncIdxEnt, st *SyncState, isAborted func() bool) (int, int64, string, bool) {

	ep, rel, ok := davEndpointFor(task.Source)
	if !ok {
		return 0, 0, "✗无地址", false
	}
	return davFillWith(task, ep, rel, excludes, have, idx, newIdx, st, isAborted)
}

// davFillWith 已确定接入地址与相对路径后的实际补收逻辑（拆出来便于单测）
func davFillWith(task SyncTask, ep davEP, rel string, excludes []string, have map[string]struct{},
	idx, newIdx map[string]syncIdxEnt, st *SyncState, isAborted func() bool) (int, int64, string, bool) {

	clProbe := davHTTPClient(ep, 20*time.Second)
	clGet := davHTTPClient(ep, 30*time.Minute)

	// 先探一次：拿到状态码与首层条目，空目录时能说清是「返回 207 但没内容」
	// 还是「路径不对被拒」，避免只给一句没信息量的「空目录」。
	first, status, ferr := davListOnce(clProbe, ep, rel)
	if ferr != nil {
		log.Printf("[同步] %s 直连接入服务失败（%s）：%v", task.ID, davURL(ep, rel), ferr)
		return 0, 0, "✗" + shortErr(ferr), true
	}
	if len(first) == 0 {
		log.Printf("[同步] %s 直连返回空目录（HTTP %d，%s）", task.ID, status, davURL(ep, rel))
		return 0, 0, fmt.Sprintf("✗空(%d)", status), true
	}

	var files []davItem
	davFails := 0
	for _, it := range first {
		if it.isDir {
			davFails += davWalk(clProbe, ep, path.Join(rel, it.rel), it.rel, 1, &files, 5000)
			continue
		}
		files = append(files, it)
	}

	var missing []davItem
	for _, f := range files {
		if _, seen := have[f.rel]; seen {
			continue
		}
		if syncExcluded(f.rel, path.Base(f.rel), excludes) {
			continue
		}
		missing = append(missing, f)
		if len(missing) >= 200 {
			break
		}
	}
	if len(missing) == 0 {
		note := fmt.Sprintf("%d/%d", len(have), len(files))
		if davFails > 0 {
			note += fmt.Sprintf(" 错%d", davFails)
		}
		return 0, 0, note, true
	}

	var n int
	var bytes int64
	for _, f := range missing {
		if isAborted() {
			break
		}
		dst := filepath.Join(task.Target, filepath.FromSlash(f.rel))
		cnt, err := davDownload(clGet, ep, path.Join(rel, f.rel), dst)
		if err != nil {
			continue
		}
		n++
		bytes += cnt
		newIdx[f.rel] = syncIdxEnt{Size: cnt, Mtime: f.mod.UnixNano()}
		st.Files++
		st.Bytes += cnt
		if f.mod.IsZero() == false {
			_ = os.Chtimes(dst, time.Now(), f.mod)
		}
	}
	log.Printf("[同步] %s 直连接入服务：挂载看到 %d 项，远端 %d 项，补收 %d 个", task.ID, len(have), len(files), n)
	note := fmt.Sprintf("+%d", n)
	if davFails > 0 {
		note += fmt.Sprintf(" 错%d", davFails)
	}
	return n, bytes, note, true
}

// localDavSockets 本机可能提供接入服务的 socket（名字里带 dav/cloud/trim 的）
func localDavSockets() []string {
	var out []string
	seen := map[string]bool{}
	dirs := []string{"/run", "/var/run", "/run/trim", "/var/run/trim", "/run/fnos", "/tmp"}
	for _, d := range dirs {
		entries, err := os.ReadDir(d)
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() {
				continue
			}
			n := strings.ToLower(e.Name())
			if !strings.Contains(n, "dav") && !strings.Contains(n, "cloud") && !strings.Contains(n, "trim") {
				continue
			}
			p := filepath.Join(d, e.Name())
			if seen[p] {
				continue
			}
			if fi, err := os.Lstat(p); err == nil && fi.Mode()&os.ModeSocket != 0 {
				seen[p] = true
				out = append(out, p)
			}
		}
	}
	sort.Strings(out)
	return out
}
