//go:build linux

package main

// 网盘同步：刷新远程挂载的 VFS 目录缓存（Linux 实现）。
//
// fnOS「文件管理 → 远程挂载」底层是 rclone FUSE（二进制常被重命名为
// /usr/trim/bin/webdav），其 VFS 层缓存目录列表（--dir-cache-time）。网盘侧
// 新增/修改的文件在缓存过期前对挂载目录不可见——这是「同步到点却扫不到新文件」
// 的根因。rclone 官方解法：向进程发 SIGHUP 立即丢弃全部目录缓存
// （rclone 文档 VFS Directory Cache 一节），无需开启 rc 服务。
//
// 判定逻辑在 cloudsync.go 的 decideFlushTargets（纯函数，跨平台单测覆盖），
// 本文件只负责「读 /proc」和「发信号」两个系统动作。

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// listProcs 采集当前所有进程的 pid / 命令行 / 可执行名。
func listProcs() ([]syncProc, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	out := make([]syncProc, 0, 256)
	for _, e := range entries {
		pid, cerr := strconv.Atoi(e.Name())
		if cerr != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		b, cerr := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if cerr != nil {
			continue // 权限不足或进程已退出
		}
		args := strings.Split(string(bytes.TrimRight(b, "\x00")), "\x00")
		if len(args) == 0 || args[0] == "" {
			continue
		}
		exe := ""
		if link, lerr := os.Readlink(filepath.Join("/proc", e.Name(), "exe")); lerr == nil {
			exe = strings.ToLower(filepath.Base(link))
		}
		out = append(out, syncProc{pid: pid, args: args, exe: exe})
	}
	return out, nil
}

// flushRcloneVFS 找到服务 mountPoint 的 rclone 进程并发 SIGHUP。
// 返回 (命中进程数, 诊断说明, 错误)；诊断说明写进同步状态给面板显示。
func flushRcloneVFS(mountPoint, fstype string) (int, string, error) {
	procs, err := listProcs()
	if err != nil {
		return 0, "读 /proc 失败: " + err.Error(), err
	}
	pids, hint := decideFlushTargets(procs, mountPoint, fstype)
	if len(pids) == 0 {
		return 0, hint, nil
	}
	signalled := 0
	var permErr, lastErr error
	for _, pid := range pids {
		if serr := syscall.Kill(pid, syscall.SIGHUP); serr != nil {
			lastErr = fmt.Errorf("SIGHUP pid=%d: %v", pid, serr)
			if serr == syscall.EPERM || serr == syscall.EACCES {
				permErr = serr
			}
			continue
		}
		signalled++
		log.Printf("[同步] 已向 rclone 进程 %d 发送 SIGHUP 刷新挂载缓存（%s）", pid, mountPoint)
	}
	switch {
	case signalled > 0:
		return signalled, fmt.Sprintf("已 SIGHUP 刷新 %d 个进程", signalled), nil
	case permErr != nil:
		return 0, "SIGHUP 权限不足（应用非 root，给挂载进程发信号被拒绝）", permErr
	default:
		return 0, "SIGHUP 失败: " + lastErr.Error(), lastErr
	}
}
