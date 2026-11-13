//go:build !linux

package main

// 非 Linux 环境没有 /proc/net/tcp
func localListenPorts() []int { return nil }
