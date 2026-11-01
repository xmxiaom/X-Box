package main

// 直接通过飞牛官方网盘网关（WebDAV）取目录清单与文件内容。
//
// 为什么：网盘挂载的目录缓存压在网关/挂载层，挂载目录可能长时间看不到网盘侧
// 新转存的文件；而网关本身是标准 WebDAV 服务（本机 127.0.0.1:15244），用新连接
// 去问它拿到的就是当前真实目录。于是同步的「发现」和「下载」都走网关，完全不经过
// 挂载目录，也就不受那层缓存影响，且不需要登录飞牛。
//
// 网关凭据来自系统给挂载用的配置 /etc/mountmgr/rclone/*.conf（用户 url/user/pass），
// 路径由 /proc/self/mountinfo 里该挂载的 source（<remote>:<path>）换算得到。

import (
	"encoding/xml"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const rcloneConfDir = "/etc/mountmgr/rclone"

type gwEndpoint struct {
	base string // 例如 http://localhost:15244
	root string // 例如 /cloud-storage/v1/dav
	user string
	pass string
}

type gwEnt struct {
	rel   string
	size  int64
	mtime time.Time
	isDir bool
}

// ---------- 配置解析 ----------

// parseINI 解析 rclone 的 ini 配置（只取需要的键）。纯函数，便于单测。
func parseINI(content string) map[string]map[string]string {
	out := map[string]map[string]string{}
	var cur string
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			cur = strings.TrimSpace(line[1 : len(line)-1])
			out[cur] = map[string]string{}
			continue
		}
		if cur == "" {
			continue
		}
		if i := strings.Index(line, "="); i > 0 {
			k := strings.TrimSpace(line[:i])
			v := strings.TrimSpace(line[i+1:])
			out[cur][k] = v
		}
	}
	return out
}

// gwEndpointFor 找到服务 srcPath 的网关地址与凭据。
// 返回 (网关, 相对挂载根的路径, 是否可用)
func gwEndpointFor(srcPath string) (gwEndpoint, string, bool) {
	mp, remote := mountSourceOf(srcPath)
	if mp == "" || remote == "" {
		return gwEndpoint{}, "", false
	}
	i := strings.Index(remote, ":")
	if i <= 0 {
		return gwEndpoint{}, "", false
	}
	name, root := remote[:i], remote[i+1:]
	if !strings.HasPrefix(root, "/") {
		root = "/" + root
	}
	cfg, ok := rcloneRemoteConf(name)
	if !ok || cfg["url"] == "" {
		return gwEndpoint{}, "", false
	}
	ep := gwEndpoint{
		base: strings.TrimRight(cfg["url"], "/"),
		root: strings.TrimRight(root, "/"),
		user: cfg["user"],
		pass: cfg["pass"],
	}
	rel, err := filepath.Rel(mp, srcPath)
	if err != nil || rel == "." {
		rel = ""
	}
	return ep, filepath.ToSlash(rel), true
}

// rcloneRemoteConf 从 /etc/mountmgr/rclone/*.conf 里找到指定 remote 的配置
func rcloneRemoteConf(name string) (map[string]string, bool) {
	files, err := os.ReadDir(rcloneConfDir)
	if err != nil {
		return nil, false
	}
	for _, f := range files {
		if f.IsDir() || !strings.HasSuffix(f.Name(), ".conf") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(rcloneConfDir, f.Name()))
		if err != nil {
			continue
		}
		sections := parseINI(string(b))
		for sec, kv := range sections {
			// rclone 里 remote 名与段名一致；有的版本带位置后缀，用前缀匹配
			if sec == name || strings.HasPrefix(sec, name) {
				return kv, true
			}
		}
	}
	return nil, false
}

// ---------- WebDAV ----------

func gwRequest(ep gwEndpoint, method, rel string, depth string) (*http.Response, error) {
	u := ep.base + ep.root
	if rel != "" {
		u += "/" + strings.TrimPrefix(rel, "/")
	}
	body := ""
	if method == "PROPFIND" {
		body = `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:getcontentlength/><D:getlastmodified/><D:resourcetype/></D:prop></D:propfind>`
	}
	req, err := http.NewRequest(method, u, strings.NewReader(body))
	if err != nil {
		return nil, err
	}
	if method == "PROPFIND" {
		req.Header.Set("Depth", depth)
		req.Header.Set("Content-Type", "application/xml")
	}
	if ep.user != "" {
		req.SetBasicAuth(ep.user, ep.pass)
	}
	cl := &http.Client{Timeout: 30 * time.Second}
	return cl.Do(req)
}

type davMultistatus struct {
	Responses []struct {
		Href  string `xml:"href"`
		Props []struct {
			Len   int64  `xml:"prop>getcontentlength"`
			Mtime string `xml:"prop>getlastmodified"`
			IsDir *struct{} `xml:"prop>resourcetype>collection"`
		} `xml:"propstat"`
	} `xml:"response"`
}

// gwList 列出网关某目录的直接子项
func gwList(ep gwEndpoint, rel string) ([]gwEnt, bool) {
	resp, err := gwRequest(ep, "PROPFIND", rel, "1")
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 207 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, false
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, false
	}
	var ms davMultistatus
	if xml.Unmarshal(raw, &ms) != nil {
		return nil, false
	}
	self := "/" + strings.Trim(ep.root+"/"+strings.TrimPrefix(rel, "/"), "/")
	var out []gwEnt
	for _, r := range ms.Responses {
		href := r.Href
		if u, e := decodePath(href); e == nil {
			href = u
		}
		href = strings.TrimRight(href, "/")
		if href == "" || href == self || href == ep.root {
			continue
		}
		name := path.Base(href)
		if name == "" || name == "." || name == ".." {
			continue
		}
		e := gwEnt{rel: name}
		for _, p := range r.Props {
			if p.IsDir != nil {
				e.isDir = true
			}
			if p.Len > 0 {
				e.size = p.Len
			}
			if p.Mtime != "" {
				if t, err := http.ParseTime(p.Mtime); err == nil {
					e.mtime = t
				}
			}
		}
		out = append(out, e)
	}
	return out, true
}

func decodePath(s string) (string, error) {
	// 只解码 %xx，不做完整 URL 解析，避免把 scheme/host 混进来
	return url.PathUnescape(s)
}

// gwWalk 递归列出网关目录（限制条目数，避免大目录把一轮拖死）
func gwWalk(ep gwEndpoint, rel, prefix string, excludes []string, out *[]gwEnt, budget *int) {
	if *budget <= 0 {
		return
	}
	*budget--
	items, ok := gwList(ep, rel)
	if !ok {
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].rel < items[j].rel })
	for _, it := range items {
		childRel := it.rel
		if prefix != "" {
			childRel = prefix + "/" + it.rel
		}
		if syncExcluded(childRel, it.rel, excludes) {
			continue
		}
		e := it
		e.rel = childRel
		*out = append(*out, e)
		if it.isDir {
			sub := strings.TrimPrefix(strings.TrimPrefix(rel+"/"+it.rel, "/"), "/")
			gwWalk(ep, sub, childRel, excludes, out, budget)
		}
	}
}

// gwDownload 经网关下载文件（先写临时文件再改名）
func gwDownload(ep gwEndpoint, rel, dst string) (int64, error) {
	resp, err := gwRequest(ep, "GET", rel, "")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
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
	if e := f.Close(); e != nil && cerr == nil {
		cerr = e
	}
	if cerr != nil {
		_ = os.Remove(tmp)
		return n, cerr
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return n, err
	}
	return n, nil
}

// gatewaySync 经网关补收「挂载目录看不到」的新文件：发现 + 下载 + 记索引。
// 返回本次补收的文件数。网关不可用时返回 0，不影响正常同步。
func gatewaySync(task SyncTask, excludes []string, idx map[string]syncIdxEnt, newIdx map[string]syncIdxEnt, st *SyncState, gen int) int {
	ep, relRoot, ok := gwEndpointFor(task.Source)
	if !ok {
		gwNoteOnce("locate", "[同步] %s 无法定位网盘网关（挂载源或 /etc/mountmgr/rclone 配置不可读），本轮按挂载目录扫描", task.ID)
		return 0
	}
	budget := 1500
	var all []gwEnt
	gwWalk(ep, relRoot, "", excludes, &all, &budget)

	var fresh []gwEnt
	var total int64
	for _, e := range all {
		if e.isDir {
			continue
		}
		if _, known := idx[e.rel]; known {
			continue
		}
		fresh = append(fresh, e)
		total += e.size
	}
	if len(fresh) == 0 {
		return 0
	}
	log.Printf("[同步] %s 网关直读发现 %d 个挂载目录看不到的新文件（共 %d 字节），开始经网关下载", task.ID, len(fresh), total)
	setSyncState(task.ID, func(x *SyncState) {
		x.Phase = "copy"
		x.Total = len(fresh)
		x.TotalBytes = total
		x.Done, x.DoneBytes = 0, 0
	})
	var doneBytes int64
	for i, e := range fresh {
		if syncGenAborted(task.ID, gen) {
			return st.Files
		}
		davRel := strings.TrimPrefix(relRoot+"/"+e.rel, "/")
		n, err := gwDownload(ep, davRel, filepath.Join(task.Target, e.rel))
		if err != nil {
			st.LastErr = "网关下载失败 " + e.rel + "：" + err.Error()
			log.Printf("[同步] %s 网关下载 %s 失败: %v", task.ID, e.rel, err)
		} else {
			st.Files++
			st.Bytes += n
			if fi, serr := os.Stat(filepath.Join(task.Target, e.rel)); serr == nil {
				newIdx[e.rel] = syncIdxEnt{Size: fi.Size(), Mtime: fi.ModTime().UnixNano()}
			}
		}
		doneBytes += n
		done := i + 1
		setSyncState(task.ID, func(x *SyncState) { x.Done = done; x.DoneBytes = doneBytes })
	}
	return len(fresh)
}

// gwNoteOnce 同一类消息 5 分钟只记一次，避免实时模式刷日志
var (
	gwNoteMu sync.Mutex
	gwNoteAt = map[string]time.Time{}
)

func gwNoteOnce(key, format string, args ...any) {
	gwNoteMu.Lock()
	defer gwNoteMu.Unlock()
	if time.Since(gwNoteAt[key]) < 5*time.Minute {
		return
	}
	gwNoteAt[key] = time.Now()
	log.Printf(format, args...)
}
