//go:build !linux

package main

// 非 Linux（开发/测试）环境没有平台挂载服务
func restartMountService(mountPoint string) (bool, string) {
	return false, "当前系统不支持重建挂载"
}

func mountPointOfSource(srcPath string) string { return "" }
