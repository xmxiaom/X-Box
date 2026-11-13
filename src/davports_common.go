package main

// /proc/net/tcp 的解析（纯函数，跨平台编译便于单测）

import (
	"strconv"
	"strings"
)

// parseNetTcp 从 /proc/net/tcp{,6} 内容里取 LISTEN 端口（十六进制、状态 0A）
func parseNetTcp(content string) []int {
	var out []int
	seen := map[int]bool{}
	for i, line := range strings.Split(content, "\n") {
		if i == 0 || strings.TrimSpace(line) == "" {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 || f[3] != "0A" {
			continue
		}
		addr := f[1]
		ci := strings.LastIndex(addr, ":")
		if ci < 0 {
			continue
		}
		p, err := strconv.ParseInt(addr[ci+1:], 16, 32)
		if err != nil || p <= 0 || p > 65535 || seen[int(p)] {
			continue
		}
		seen[int(p)] = true
		out = append(out, int(p))
	}
	return out
}
