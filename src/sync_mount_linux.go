//go:build linux

package main

// 网盘同步：刷新远程挂载的 VFS 目录缓存（Linux 实现）。
//
// fnOS「文件管理 → 远程挂载」底层是 rclone FUSE（二进制常被重命名为
// /usr/trim/bin/webdav），其 VFS 层缓存目录列表（--dir-cache-time 默认 5 分钟）。
// 网盘侧新增/修改的文件在缓存过期前对挂载目录不可见——这是「同步到点却扫不到
// 新文件」的根因。rclone 官方解法：向进程发 SIGHUP 立即丢弃全部目录缓存
// （https://rclone.org/commands/rclone_serve_nfs/ 的 VFS Directory Cache 一节），
// 无需开启 rc 服务，也无需 root 之外的特殊权限（向自己用户的进程发信号即可）。

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

// flushRcloneVFS 找到服务 mountPoint 的 rclone 进程并发 SIGHUP。
// 匹配规则：进程 cmdline 中同时出现 rclone/webdav 字样与挂载点路径
// （mount 命令必然包含挂载点参数）。fnOS 可能有多个网盘挂载共用一个进程，
// SIGHUP 会刷新该进程的全部缓存，副作用仅是多拉几次目录列表，可接受。
func flushRcloneVFS(mountPoint string) (int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0, err
	}
	signalled := 0
	var lastErr error
	for _, e := range entries {
		pid, cerr := strconv.Atoi(e.Name())
		if cerr != nil || pid <= 1 || pid == os.Getpid() {
			continue
		}
		b, cerr := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if cerr != nil {
			continue // 权限不足或进程已退出，跳过
		}
		args := strings.Split(string(bytes.TrimRight(b, "\x00")), "\x00")
		if len(args) == 0 || !isRcloneCmdline(args, mountPoint) {
			continue
		}
		if serr := syscall.Kill(pid, syscall.SIGHUP); serr != nil {
			lastErr = fmt.Errorf("SIGHUP pid=%d: %v", pid, serr)
			continue
		}
		signalled++
		log.Printf("[同步] 已向 rclone 进程 %d 发送 SIGHUP 刷新挂载缓存（%s）", pid, mountPoint)
	}
	if signalled > 0 {
		return signalled, nil
	}
	if lastErr != nil {
		return 0, lastErr
	}
	return 0, fmt.Errorf("未找到服务 %s 的 rclone 进程", mountPoint)
}

// isRcloneCmdline 的实现见 cloudsync.go（纯函数跨平台共用，便于单测）。
