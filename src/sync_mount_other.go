//go:build !linux

package main

// 非 Linux 平台（开发/测试环境）无 /proc 与 syscall.SIGHUP 语义，直接返回不支持。
// 生产环境是 fnOS（Linux），实际刷新逻辑在 sync_mount_linux.go。

import "errors"

func flushRcloneVFS(mountPoint string) (int, error) {
	return 0, errors.New("仅支持 Linux")
}
