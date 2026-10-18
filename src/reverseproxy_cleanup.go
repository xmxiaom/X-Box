package main

// 一次性清理：早期版本曾用"独立 nginx 实例"实现反向代理，功能已移除。
// 升级后那个实例可能还在跑（占着端口），这里在启动时把它停掉并清掉配置目录。

import (
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func cleanupLegacyReverseProxy() {
	dir := os.Getenv("XBOX_DATA")
	if dir == "" {
		return
	}
	legacy := filepath.Join(dir, "nginx")
	b, err := os.ReadFile(filepath.Join(legacy, "logs", "nginx.pid"))
	if err == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(b))); perr == nil && pid > 0 {
			// 只对确属本目录的进程发信号，避免误伤系统 nginx
			if cb, cerr := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/cmdline"); cerr == nil &&
				strings.Contains(string(cb), legacy) {
				if p, e := os.FindProcess(pid); e == nil {
					_ = p.Signal(syscall.SIGTERM)
					log.Printf("[清理] 已停止历史反代实例 pid=%d", pid)
					time.Sleep(300 * time.Millisecond)
				}
			}
		}
	}
	if _, err := os.Stat(legacy); err == nil {
		_ = os.RemoveAll(legacy)
		log.Printf("[清理] 已移除历史反代配置目录 %s", legacy)
	}
}
