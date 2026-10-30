//go:build !linux

package main

// 非 Linux（开发/测试）环境没有 /proc/net/tcp，不做端口发现。
func localListenPorts() []int { return nil }
