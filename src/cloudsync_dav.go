package main

// 绕过挂载层缓存：直连 fnOS 的 WebDAV 网关取目录清单与文件内容。
//
// 链路：rclone mount(fuse.rclone) → 本机 WebDAV 网关(127.0.0.1:15244) → 网盘 API。
// 实测给挂载进程发 SIGHUP 后，挂载目录仍看不到网盘侧新增文件，说明缓存压在网关
// （或挂载层根本不响应 SIGHUP）。这里换一条路：用标准 WebDAV 协议直接问网关
// 要目录清单，发现索引里没有的新文件，再经网关 GET 下载到目标目录——
// 全程不经过挂载目录，天然不受那层缓存影响。
//
// 网关不可达/路径对不上/清单与挂载一致时一律安静返回，绝不干扰正常同步。

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const davPropfindBody = `<?xml version="1.0" encoding="utf-8"?><D:propfind xmlns:D="DAV:"><D:prop><D:getcontentlength/><D:getlastmodified/><D:resourcetype/></D:prop></D:propfind>`

var (
	davRespRe = regexp.MustCompile(`(?is)<[\w.:-]*response[\s>].*?</[\w.:-]*response>`)
	davHrefRe = regexp.MustCompile(`(?is)<[\w.:-]*href[^>]*>([^<]+)<`)
	davLenRe  = regexp.MustCompile(`(?is)<[\w.:-]*getcontentlength[^>]*>\s*(\d+)`)
	davColRe  = regexp.MustCompile(`(?is)<[\w.:-]*collection[\s/>]`)
)

type davItem struct {
	name  string
	size  int64
	isDir bool
}

// davAddrs 候选网关地址：可用 XBOX_SYNC_DAV 覆盖，默认扫 fnOS 常用端口
func davAddrs() []string {
	var a []string
	if v := strings.TrimSpace(os.Getenv("XBOX_SYNC_DAV")); v != "" {
		a = append(a, strings.TrimRight(v, "/"))
	}
	return append(a, "http://127.0.0.1:15244", "http://127.0.0.1:15245", "http://127.0.0.1:15246")
}

func davEscaped(p string) string {
	u := &url.URL{Path: p}
	return u.EscapedPath()
}

// davList 对网关目录发 PROPFIND（Depth:1），返回子项（不含自身）
func davList(cl *http.Client, base, dir string) ([]davItem, bool) {
	req, err := http.NewRequest("PROPFIND", base+davEscaped(dir), strings.NewReader(davPropfindBody))
	if err != nil {
		return nil, false
	}
	req.Header.Set("Depth", "1")
	req.Header.Set("Content-Type", "application/xml")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 207 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return nil, false
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, false
	}
	self := strings.TrimRight(dir, "/")
	var out []davItem
	for _, blk := range davRespRe.FindAll(body, -1) {
		m := davHrefRe.FindSubmatch(blk)
		if m == nil {
			continue
		}
		raw := strings.TrimSpace(string(m[1]))
		item := davItem{}
		var decodeErr error
		if u, e := url.PathUnescape(raw); e == nil {
			raw = u
		} else {
			decodeErr = e
		}
		_ = decodeErr
		trimmed := strings.TrimRight(raw, "/")
		if trimmed == "" || trimmed == self {
			continue // 目录自身
		}
		item.name = path.Base(trimmed)
		item.isDir = davColRe.Match(blk) || strings.HasSuffix(string(m[1]), "/")
		if lm := davLenRe.FindSubmatch(blk); lm != nil {
			item.size, _ = strconv.ParseInt(string(lm[1]), 10, 64)
		}
		if item.name == "" || item.name == "." || item.name == ".." {
			continue
		}
		out = append(out, item)
	}
	return out, true
}

// davFindRoot 在网关根目录里找到与挂载点同名的那一层（挂载点路径与网关路径不一回事）
func davFindRoot(cl *http.Client, base, mountName string) (root string, seen []string, ok bool) {
	items, good := davList(cl, base, "/")
	if !good {
		return "", nil, false
	}
	for _, it := range items {
		seen = append(seen, it.name)
		if it.isDir && it.name == mountName {
			return "/" + mountName, seen, true
		}
	}
	return "", seen, false
}

// davWalk 递归列出网关目录（限制深度/条目，避免网盘大目录把一轮拖死）
func davWalk(cl *http.Client, base, dir, prefix string, excludes []string, out *[]srcEntry, budget *int) {
	if *budget <= 0 {
		return
	}
	*budget--
	items, ok := davList(cl, base, dir)
	if !ok {
		return
	}
	for _, it := range items {
		rel := it.name
		if prefix != "" {
			rel = prefix + "/" + it.name
		}
		if syncExcluded(rel, it.name, excludes) {
			continue
		}
		e := srcEntry{rel: rel, size: it.size, isDir: it.isDir}
		*out = append(*out, e)
		if it.isDir {
			davWalk(cl, base, dir+"/"+it.name, rel, excludes, out, budget)
		}
	}
}

// davGetTo 经网关下载单个文件（先写临时文件再改名，绝不留下半成品）
func davGetTo(cl *http.Client, base, davPath, dst string) (int64, error) {
	resp, err := cl.Get(base + davEscaped(davPath))
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

// davFillMissing 经网关补收「挂载目录看不到」的新文件。
// 返回 (新下载数量, 诊断说明)。数量为 0 时说明里解释原因（网关不可用/路径对不上/确实无新增）。
func davFillMissing(task SyncTask, excludes []string, idx map[string]syncIdxEnt, newIdx map[string]syncIdxEnt, st *SyncState, gen int) (int, string) {
	mp, fstype, _ := mountInfoOf(task.Source)
	if mp == "" || !strings.Contains(strings.ToLower(fstype), "rclone") {
		return 0, ""
	}
	cl := &http.Client{Timeout: 10 * time.Minute}
	mountName := filepath.Base(mp)
	note := ""
	for _, base := range davAddrs() {
		root, seen, ok := davFindRoot(cl, base, mountName)
		if !ok {
			if note == "" {
				if len(seen) > 0 {
					if len(seen) > 6 {
						seen = seen[:6]
					}
					note = "网关" + base[len("http://"):] + "根目录无「" + mountName + "」，实有：" + strings.Join(seen, ",")
				} else {
					note = "网关 " + base[len("http://"):] + " 不可达或非 WebDAV"
				}
			}
			continue
		}
		sub := ""
		if r, err := filepath.Rel(mp, task.Source); err == nil && r != "." && r != "" {
			sub = "/" + strings.TrimPrefix(filepath.ToSlash(r), "/")
		}
		davDir := root + sub

		budget := 1200
		var all []srcEntry
		davWalk(cl, base, davDir, "", excludes, &all, &budget)

		var fresh []srcEntry
		var totalBytes int64
		for _, f := range all {
			if f.isDir {
				continue
			}
			if _, known := idx[f.rel]; known {
				continue // 索引里已有：交给常规扫描处理，不重复下载
			}
			fresh = append(fresh, f)
			totalBytes += f.size
		}
		if len(fresh) == 0 {
			return 0, "网关可见，无新增文件"
		}
		log.Printf("[同步] %s 网关直读发现 %d 个挂载目录看不到的新文件，开始经网关下载", task.ID, len(fresh))
		setSyncState(task.ID, func(x *SyncState) {
			x.Phase = "copy"
			x.Total = len(fresh)
			x.TotalBytes = totalBytes
			x.Done = 0
			x.DoneBytes = 0
			x.RefreshHint = fmt.Sprintf("网关直读：发现 %d 个新文件", len(fresh))
		})
		var doneBytes int64
		for i, f := range fresh {
			if syncGenAborted(task.ID, gen) { // 用户暂停：立即停下
				return st.Files, "网关直读已中止"
			}
			n, err := davGetTo(cl, base, davDir+"/"+f.rel, filepath.Join(task.Target, f.rel))
			doneBytes += n
			done := i + 1
			if err != nil {
				st.LastErr = "网关下载失败 " + f.rel + "：" + err.Error()
				log.Printf("[同步] %s 网关下载 %s 失败: %v", task.ID, f.rel, err)
			} else {
				st.Files++
				st.Bytes += n
				// 记进索引，否则下一轮又会被当成「索引里没有的新文件」重复下载
				if fi, serr := os.Stat(filepath.Join(task.Target, f.rel)); serr == nil {
					newIdx[f.rel] = syncIdxEnt{Size: fi.Size(), Mtime: fi.ModTime().UnixNano()}
				}
			}
			setSyncState(task.ID, func(x *SyncState) { x.Done = done; x.DoneBytes = doneBytes })
		}
		return len(fresh), fmt.Sprintf("网关直读：已补收 %d 个新文件", len(fresh))
	}
	return 0, note
}
