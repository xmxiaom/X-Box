//go:build linux

package main

// 重建远程挂载：让平台重新向网盘拉一遍目录。
//
// 为什么需要它：目录缓存压在平台自己的网盘接入层里。刷新挂载进程的目录缓存
// （vfs/refresh）能成功，但拿到的仍是接入层缓存下来的旧列表——实测就是这样：
// 刷新返回成功，挂载目录里依旧看不到网盘侧刚转存的文件。
// 接入层没有对外开放的免凭据刷新入口，唯一确定有效的动作是让它重新挂载：
// 重新挂载必然重新拉目录，缓存随之消失。
//
// 这条路径代价是重挂期间挂载点会短暂不可用（通常几秒），所以调用方必须
// 做冷却控制，且只在确实需要时才触发。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// restartMountService 重启平台的挂载服务，并等待挂载点恢复可读。
func restartMountService(mountPoint string) (bool, string) {
	candidates := [][]string{
		{"systemctl", "restart", "mountmgr.service"},
		{"systemctl", "restart", "mountmgr"},
		{"/etc/init.d/mountmgr", "restart"},
		{"service", "mountmgr", "restart"},
	}
	var lastOut string
	for _, c := range candidates {
		if _, err := os.Stat(c[0]); err != nil && !strings.HasPrefix(c[0], "systemctl") && c[0] != "service" {
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		cmd := exec.CommandContext(ctx, c[0], c[1:]...)
		out, err := cmd.CombinedOutput()
		cancel()
		lastOut = firstN(strings.TrimSpace(string(out)), 80)
		if err != nil {
			continue
		}
		if waitMountReady(mountPoint, 45*time.Second) {
			return true, "已重建挂载"
		}
		return true, "已重建挂载（挂载点尚未就绪）"
	}
	if lastOut == "" {
		lastOut = "未找到挂载服务"
	}
	return false, "重建挂载失败：" + lastOut
}

// waitMountReady 轮询等待挂载点重新可读
func waitMountReady(mountPoint string, timeout time.Duration) bool {
	if mountPoint == "" {
		return false
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if isMountPresent(mountPoint) {
			if d, err := os.Open(mountPoint); err == nil {
				_, rerr := d.Readdirnames(1)
				_ = d.Close()
				if rerr == nil {
					return true
				}
			}
		}
		time.Sleep(700 * time.Millisecond)
	}
	return false
}

// isMountPresent 判断挂载点当前是否在挂载表里
func isMountPresent(mountPoint string) bool {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return false
	}
	want := filepath.Clean(mountPoint)
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		if strings.Contains(f[4], want) {
			return true
		}
	}
	return false
}

// mountPointOfSource 返回源目录所属的挂载点（用于重建后判断是否就绪）
func mountPointOfSource(srcPath string) string {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return ""
	}
	clean := filepath.Clean(srcPath)
	best := ""
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		mp := unescape8(f[4])
		if clean == mp || strings.HasPrefix(clean, strings.TrimSuffix(mp, "/")+"/") {
			if len(mp) > len(best) {
				best = mp
			}
		}
	}
	return best
}

// unescape8 mountinfo 里的八进制转义（空格等）
func unescape8(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+3 < len(s) {
			if v, err := strconv.ParseInt(s[i+1:i+4], 8, 16); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}
	return b.String()
}
