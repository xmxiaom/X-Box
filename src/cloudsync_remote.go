package main

// 绕过挂载目录缓存，直接向挂载服务要远端文件清单并完成复制。
//
// 背景：同步靠「读源目录」发现文件，而源目录是网盘挂载，它的内容受挂载服务
// 的目录缓存支配。缓存未过期时，网盘侧刚转存的文件在挂载目录里根本看不到，
// 于是同步永远是「无变化」。
//
// 这里改用挂载服务的文件层接口：直接列远端目录、直接让服务把文件复制到目标。
// 这条路径不经过挂载目录的目录缓存，因此看到的就是远端真实内容；复制也由服务
// 自己完成，不需要先让文件出现在挂载目录里。

import (
	"fmt"
	"log"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// 挂载服务是精简版，未必保留全部控制接口。这里探测一次并缓存结果，
// 避免把「不支持」误判成「没变化」。
var (
	rcCapMu sync.Mutex
	rcCap   = map[string]bool{} // "sock|endpoint" -> 是否支持
	rcCapAt time.Time
)

func rcloneSupports(sock, endpoint string) bool {
	rcCapMu.Lock()
	defer rcCapMu.Unlock()
	key := sock + "|" + endpoint
	if v, ok := rcCap[key]; ok && time.Since(rcCapAt) < 10*time.Minute {
		return v
	}
	out, err := rcloneRCTimeout(sock, "rc/list", nil, 8*time.Second)
	if err != nil {
		return false // 探测失败不缓存，下次再试
	}
	cmds, _ := out["cmds"].([]any)
	if len(cmds) == 0 {
		return false
	}
	rcCap = map[string]bool{}
	for _, it := range cmds {
		if m, ok := it.(map[string]any); ok {
			if p, _ := m["Path"].(string); p != "" {
				rcCap[sock+"|"+p] = true
			}
		}
	}
	rcCapAt = time.Now()
	return rcCap[key]
}

// 远端补收的节流（每任务 60 秒最多列一次远端目录）
var (
	syncRemoteMu sync.Mutex
	syncRemoteAt = map[string]time.Time{}
)

// rcFile 远端文件条目（相对同步源目录的路径）
type rcFile struct {
	Rel  string
	Size int64
	Mod  time.Time
}

// remoteMountOf 找到源目录所属挂载：返回控制地址、远端根（Fs）、挂载内相对路径
func remoteMountOf(srcPath string) (sock, fs, rel string, ok bool) {
	clean := path.Clean(srcPath)
	for _, sk := range rcloneSockets() {
		out, err := rcloneRC(sk, "mount/listmounts", nil)
		if err != nil {
			continue
		}
		raw, _ := out["mountPoints"].([]any)
		var mounts []rcMount
		for _, it := range raw {
			m, mok := it.(map[string]any)
			if !mok {
				continue
			}
			f, _ := m["Fs"].(string)
			mp, _ := m["MountPoint"].(string)
			mounts = append(mounts, rcMount{Fs: f, MountPoint: mp})
		}
		if m, r, mok := pickMount(mounts, clean); mok {
			return sk, m.Fs, r, true
		}
	}
	return "", "", "", false
}

// listRemote 递归列出远端目录的文件（只用文件层，不碰挂载目录）
func listRemote(sock, fs, rel string) ([]rcFile, error) {
	params := map[string]any{
		"fs":  fs,
		"opt": map[string]any{"recurse": true, "filesOnly": true},
	}
	if rel != "/" && rel != "" {
		params["remote"] = rel
	}
	out, err := rcloneRCTimeout(sock, "operations/list", params, 120*time.Second)
	if err != nil {
		return nil, err
	}
	raw, _ := out["list"].([]any)
	files := make([]rcFile, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			continue
		}
		if isDir, _ := m["IsDir"].(bool); isDir {
			continue
		}
		p, _ := m["Path"].(string)
		if p == "" {
			continue
		}
		size, _ := m["Size"].(float64)
		var mod time.Time
		if s, _ := m["ModTime"].(string); s != "" {
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				mod = t
			}
		}
		files = append(files, rcFile{Rel: strings.TrimPrefix(p, "/"), Size: int64(size), Mod: mod})
	}
	return files, nil
}

// copyRemoteFile 让挂载服务把远端文件直接复制到本地目标路径
func copyRemoteFile(sock, fs, rel, destPath string) error {
	srcRemote := rel
	if !strings.HasPrefix(srcRemote, "/") {
		srcRemote = "/" + srcRemote
	}
	_, err := rcloneRCTimeout(sock, "operations/copyfile", map[string]any{
		"srcFs":     fs,
		"srcRemote": srcRemote,
		"dstFs":     filepath.Dir(destPath),
		"dstRemote": filepath.Base(destPath),
	}, 30*time.Minute)
	return err
}

// fillMissingFromRemote 把「远端有、挂载目录看不到」的文件直接复制过来。
// have 是本次挂载目录扫描到的相对路径集合；返回补收数量、字节数与一句诊断。
func fillMissingFromRemote(task SyncTask, excludes []string, have map[string]struct{},
	idx, newIdx map[string]syncIdxEnt, st *SyncState, isAborted func() bool) (int, int64, string) {

	// 列远端要遍历整棵目录，开销随文件数增长。高频检测时没必要每轮都做，
	// 60 秒一次足够（远端的目录缓存周期本身比这更长）。
	syncRemoteMu.Lock()
	if time.Since(syncRemoteAt[task.ID]) < 60*time.Second {
		syncRemoteMu.Unlock()
		return 0, 0, ""
	}
	syncRemoteAt[task.ID] = time.Now()
	syncRemoteMu.Unlock()

	sock, fs, rel, ok := remoteMountOf(task.Source)
	if !ok {
		return 0, 0, "✗未匹配"
	}
	files, err := listRemote(sock, fs, rel)
	if err != nil {
		// 精简版挂载服务可能没有这个接口，错误原文直接给出便于判断
		return 0, 0, "✗" + shortErr(err)
	}
	if len(files) == 0 {
		return 0, 0, "✗空目录"
	}

	var missing []rcFile
	for _, f := range files {
		if _, seen := have[f.Rel]; seen {
			continue
		}
		if syncExcluded(f.Rel, path.Base(f.Rel), excludes) {
			continue
		}
		missing = append(missing, f)
		if len(missing) >= 200 { // 单轮上限，避免异常时一次性搬运过多
			break
		}
	}
	if len(missing) == 0 {
		// 挂载目录与远端一致：说明这一轮不是缓存挡着
		return 0, 0, fmt.Sprintf("一致%d", len(files))
	}

	var n int
	var bytes int64
	for _, f := range missing {
		if isAborted() {
			break
		}
		dst := filepath.Join(task.Target, filepath.FromSlash(f.Rel))
		if err := copyRemoteFile(sock, fs, path.Join(rel, f.Rel), dst); err != nil {
			continue
		}
		n++
		bytes += f.Size
		newIdx[f.Rel] = syncIdxEnt{Size: f.Size, Mtime: f.Mod.UnixNano()}
		st.Files++
		st.Bytes += f.Size
		st.Done++
	}
	if n > 0 {
		log.Printf("[同步] %s 挂载目录看不到的 %d 个文件已直接复制（%d 字节）", task.ID, n, bytes)
	}
	return n, bytes, fmt.Sprintf("补收%d", n)
}
