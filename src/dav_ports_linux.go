//go:build linux

package main

// 本机正在监听的端口（用于自动发现 fnOS 的 WebDAV 网关地址）。
// 网关端口不固定（实测 15244 并未监听），只能从 /proc/net/tcp 里找。

import (
	"os"
	"sort"
)

func localListenPorts() []int {
	var all []int
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		if b, err := os.ReadFile(f); err == nil {
			all = append(all, parseNetTcp(string(b))...)
		}
	}
	seen := map[int]bool{}
	out := make([]int, 0, len(all))
	for _, p := range all {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Ints(out)
	if len(out) > 25 { // 逐个探测有成本，限制数量
		out = out[:25]
	}
	return out
}
