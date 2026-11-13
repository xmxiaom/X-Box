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

// 刷新结论按「源目录」分别缓存：同一台机器上可能挂着多个网盘，
// 共用一个全局节流会让后一个任务直接拿到前一个任务的结论（显示成别人的路径），
// 既误导判断、也真的跳过了它自己的刷新。
var (
	rcloneMu   sync.Mutex
	rcloneSeen = map[string]*rcRefresh{} // key: 源目录
	rclonePrevNote string
)

type rcRefresh struct {
	at   time.Time
	ok   bool
	note string
}

// rcloneSockets 列出所有 rclone rc socket（不同用户各有一个）
func rcloneSockets() []string {
	if v := strings.TrimSpace(os.Getenv("XBOX_RCLONE_RC")); v != "" {
		return []string{v} // 测试或特殊部署可直接指定地址
	}
	paths, _ := filepath.Glob("/var/run/rclone/rcd_*.sock")
	sort.Strings(paths)
	return paths
}

func rcloneRC(sock, endpoint string, payload map[string]any) (map[string]any, error) {
	return rcloneRCTimeout(sock, endpoint, payload, 10*time.Second)
}

// rcloneRCTimeout 同 rcloneRC，但可指定超时（整树刷新可能要几十秒）
func rcloneRCTimeout(sock, endpoint string, payload map[string]any, timeout time.Duration) (map[string]any, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	var (
		cl  *http.Client
		url string
	)
	if strings.HasPrefix(sock, "http://") || strings.HasPrefix(sock, "https://") {
		cl = &http.Client{Timeout: timeout}
		url = strings.TrimRight(sock, "/") + "/" + endpoint
	} else {
		tr := &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: 3 * time.Second}
				return d.DialContext(ctx, "unix", sock)
			},
		}
		cl = &http.Client{Timeout: timeout, Transport: tr}
		url = "http://unix/" + endpoint
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
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

// refreshMountCache 让挂载服务重新列一次同步源目录（含子目录），返回是否成功与一句诊断。
//
// 为什么必须同步等待：vfs/refresh 是「现在去列一遍目录并更新缓存」。若带 _async，
// 它只是排个后台任务就返回，紧接着的扫描读到的仍是旧缓存——表面刷新成功、实际没生效。
// 所以这里不传 _async，等它列完再让调用方扫描（纯网络+内存操作，不写磁盘）。
func refreshMountCache(srcPath string) (bool, string) {
	rcloneMu.Lock()
	defer rcloneMu.Unlock()
	key := path.Clean(srcPath)
	st := rcloneSeen[key]
	if st != nil && time.Since(st.at) < 10*time.Second {
		return st.ok, st.note // 同一源目录的节流窗口内复用上次结论
	}
	if st == nil {
		st = &rcRefresh{}
		rcloneSeen[key] = st
	}
	st.at = time.Now()

	// 缓存有两层：平台接入服务一层、挂载服务(VFS)一层。两层都要破，
	// 只破其中一层，扫描看到的仍是旧目录列表。
	platOK, platNote := refreshViaPlatform()
	plat, platSuffix := "", ""
	if platOK {
		plat = "平台已刷新 · "
	} else if platNote != "" {
		// 平台刷新失败的原因必须一并显示：缓存很可能就压在平台那层，
		// 只写「已重列」会让人误以为刷新已经做到位。
		platSuffix = "（" + platNote + "）"
	}

	socks := rcloneSockets()
	if len(socks) == 0 {
		st.note = plat + "未找到挂载服务控制接口"
		if !platOK {
			st.note += "（" + platNote + "）"
		}
		st.ok = platOK
		noteOnce("sock", "[同步] %s", st.note)
		return platOK, st.note
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
		// 实测语义：vfs/refresh 带 fs 会重新列该目录（返回 {"result":{"/xx":"OK"}}），
		// 之后新转存的文件/目录立刻可见；不带 dir 或只传 fs 都不生效。
		// recursive 让子目录一并重列（子目录里的新文件也要能同步到）。
		params := map[string]any{"fs": m.Fs, "recursive": "true"}
		// 实测：dir 传空串或 "/" 会被当成不存在的路径（file does not exist），
		// 源目录正好是挂载根时必须整个省略 dir。
		if rel != "/" {
			params["dir"] = rel
		}
		if _, err := rcloneRCTimeout(sock, "vfs/refresh", params, 120*time.Second); err != nil {
			lastErr = err
			continue
		}
		st.note = plat + "已重列 " + m.MountPoint + rel + platSuffix
		st.ok = true
		log.Printf("[同步] %s", st.note)
		return true, st.note
	}
	if lastErr != nil {
		st.note = plat + "重列失败：" + lastErr.Error()
	} else {
		// 一个挂载都没匹配上：把现有挂载点列出来，便于一眼看出路径对不对
		st.note = plat + "源目录不在网盘挂载内（现有：" + mountListHint() + "）"
	}
	if !platOK {
		st.note += "（" + platNote + "）"
	}
	st.ok = false
	if st.note != rclonePrevNote {
		log.Printf("[同步] %s", st.note)
		rclonePrevNote = st.note
	}
	return false, st.note
}

// mountListHint 汇总所有 socket 上的挂载点，供诊断文案使用
func mountListHint() string {
	var out []string
	for _, sock := range rcloneSockets() {
		res, err := rcloneRC(sock, "mount/listmounts", nil)
		if err != nil {
			continue
		}
		raw, _ := res["mountPoints"].([]any)
		for _, it := range raw {
			if m, ok := it.(map[string]any); ok {
				if mp, _ := m["MountPoint"].(string); mp != "" {
					out = append(out, mp)
				}
			}
		}
	}
	if len(out) == 0 {
		return "无"
	}
	if len(out) > 3 {
		out = append(out[:3], "…")
	}
	return strings.Join(out, "，")
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
