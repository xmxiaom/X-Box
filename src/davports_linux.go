//go:build linux

package main

// 本机正在监听的端口：平台接入服务的端口不固定（配在挂载配置里的地址
// 也未必是应用能直接连的那个），只能从 /proc/net/tcp 里找。

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
	return out
}
