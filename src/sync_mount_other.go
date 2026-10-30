//go:build !linux

package main

// 非 Linux 平台（开发/测试环境）无 /proc 与 syscall.SIGHUP 语义。
// 生产环境是 fnOS（Linux），实际刷新逻辑在 sync_mount_linux.go。

func flushRcloneVFS(mountPoint, fstype string) (int, string, error) {
	return 0, "非 Linux 平台不支持 SIGHUP 刷新", nil
}
