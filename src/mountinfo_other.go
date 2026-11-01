//go:build !linux

package main

// 非 Linux 环境没有 /proc/self/mountinfo
func mountSourceOf(p string) (string, string) { return "", "" }
