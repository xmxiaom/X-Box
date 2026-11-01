package main

// 刷新网盘挂载目录缓存：调用 rclone 的 rc 接口 vfs/forget。
//
// 现场实测（fnOS 1.2.x）：
//   - 网盘挂载由 rclone rcd 守护进程负责，控制口是 unix socket
//     /var/run/rclone/rcd_<uid>.sock（--rc-no-auth，root 专属 0600）
//   - 存在多个挂载时，vfs/forget 必须带 "fs" 参数，否则报
//     "more than one VFS active - need \"fs\" parameter"
//   - 带 fs + dir 调用会返回 {"forgotten":["目录名"]}，即该目录缓存已丢弃
//
// 因此刷新 = 查 mount/listmounts 找到源目录所属挂载的 Fs，再按 Fs（+ 挂载内相对路径）
// 调 vfs/forget。按挂载点匹配，天然支持任意网盘（夸克/百度/115/WebDAV 等）。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	rcloneMu     sync.Mutex
	rcloneLastAt time.Time
	rcloneLastOK bool
	rcloneNote   string
)

// rcloneSockets 列出所有 rclone rc socket（不同用户各有一个）
func rcloneSockets() []string {
	paths, _ := filepath.Glob("/var/run/rclone/rcd_*.sock")
	sort.Strings(paths)
	return paths
}

func rcloneRC(sock, endpoint string, payload map[string]any) (map[string]any, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			d := net.Dialer{Timeout: 3 * time.Second}
			return d.DialContext(ctx, "unix", sock)
		},
	}
	cl := &http.Client{Timeout: 10 * time.Second, Transport: tr}
	req, err := http.NewRequest("POST", "http://unix/"+endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := cl.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("响应无法解析：%s", strings.TrimSpace(string(raw)))
	}
	if e, ok := out["error"].(string); ok && e != "" {
		return out, fmt.Errorf("%s", e)
	}
	return out, nil
}

type rcMount struct {
	Fs         string
	MountPoint string
}

// pickMount 选出包含 srcPath 的最具体挂载（纯函数，便于单测）
func pickMount(mounts []rcMount, srcPath string) (rcMount, string, bool) {
	clean := path.Clean(srcPath)
	best := rcMount{}
	found := false
	for _, m := range mounts {
		mp := path.Clean(m.MountPoint)
		if mp == "" || m.Fs == "" {
			continue
		}
		if clean == mp || strings.HasPrefix(clean, strings.TrimSuffix(mp, "/")+"/") {
			if !found || len(mp) > len(path.Clean(best.MountPoint)) {
				best, found = m, true
			}
		}
	}
	if !found {
		return rcMount{}, "", false
	}
	rel := strings.TrimPrefix(clean, path.Clean(best.MountPoint))
	rel = "/" + strings.TrimPrefix(rel, "/")
	return best, rel, true
}

// refreshMountCache 丢弃源目录在挂载内的目录缓存。
// 3 秒节流：实时模式 1~2 秒一轮，不必每轮都让远端重新列目录。
func refreshMountCache(srcPath string) bool {
	rcloneMu.Lock()
	defer rcloneMu.Unlock()
	if time.Since(rcloneLastAt) < 10*time.Second {
		return rcloneLastOK
	}
	rcloneLastAt = time.Now()

	socks := rcloneSockets()
	if len(socks) == 0 {
		noteOnce("sock", "[同步] 未找到 rclone 控制 socket（/var/run/rclone/rcd_*.sock），跳过挂载缓存刷新")
		rcloneLastOK = false
		return false
	}

	var lastErr error
	for _, sock := range socks {
		out, err := rcloneRC(sock, "mount/listmounts", nil)
		if err != nil {
			lastErr = err
			continue
		}
		raw, _ := out["mountPoints"].([]any)
		var mounts []rcMount
		for _, it := range raw {
			m, ok := it.(map[string]any)
			if !ok {
				continue
			}
			fs, _ := m["Fs"].(string)
			mp, _ := m["MountPoint"].(string)
			mounts = append(mounts, rcMount{Fs: fs, MountPoint: mp})
		}
		m, rel, ok := pickMount(mounts, srcPath)
		if !ok {
			continue
		}
		// 实测：vfs/refresh 带 fs+dir 会重新列该目录（返回 {"result":{"/xxx":"OK"}}），
		// 之后新转存的文件/目录立刻可见；不带 dir 或只传 fs 都无效。
		// 只刷同步源目录这一层，不做 recursive，避免大目录反复扫描费硬盘。
		// 每次都对「同步目录及其以下」做整树刷新：rclone 只是重新列目录、
		// 更新内存里的目录缓存，不写磁盘；子目录里的新增文件也能被同步到。
		// _async 让 rclone 在后台刷新，避免大目录树阻塞本轮扫描；
		// 每轮都发，缓存持续保持新鲜，下一轮扫描即能看到变化。
		params := map[string]any{"fs": m.Fs, "recursive": "true", "_async": "true"}
		// 实测：dir 为空/"/" 会被当成不存在的路径（"file does not exist"），
		// 源目录正好是挂载根时必须整个省略 dir。
		if rel != "/" {
			params["dir"] = rel
		}
		if _, err := rcloneRC(sock, "vfs/refresh", params); err != nil {
			lastErr = err
			continue
		}
		log.Printf("[同步] 已刷新挂载目录树（%s，dir=%s）", m.MountPoint, rel)
		rcloneNote = ""
		rcloneLastOK = true
		return true
	}
	if lastErr != nil {
		if rcloneNote != lastErr.Error() {
			log.Printf("[同步] 挂载缓存刷新失败：%v", lastErr)
			rcloneNote = lastErr.Error()
		}
	} else {
		noteOnce("nomatch", "[同步] 源目录 %s 不在任何 rclone 挂载内，跳过缓存刷新", srcPath)
	}
	rcloneLastOK = false
	return false
}

var (
	noteMu sync.Mutex
	noteAt = map[string]time.Time{}
)

func noteOnce(key, format string, args ...any) {
	noteMu.Lock()
	defer noteMu.Unlock()
	if time.Since(noteAt[key]) < 5*time.Minute {
		return
	}
	noteAt[key] = time.Now()
	log.Printf(format, args...)
}

// 保持 os 导入被使用（socket 存在性判断用）
var _ = os.Stat
