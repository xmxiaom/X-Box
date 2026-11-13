//go:build linux

package main

// 从 /proc/self/mountinfo 找源目录所属挂载点，并拆出它的「挂载源」。
// 网盘挂载的源形如 1000-1-abc:/cloud-storage/v1/dav ——
// 冒号前是接入服务的账号标识，冒号后是接入服务里的根路径。
// 据此可以直接按 WebDAV 协议访问该接入服务，而不必经过挂载目录。

import (
	"os"
	"path/filepath"
	"strings"
)

func mountSourceOf(srcPath string) (mountPoint, remote, root string) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", "", ""
	}
	clean := filepath.Clean(srcPath)
	best := -1
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		mp := unescape8(f[4])
		if clean != mp && !strings.HasPrefix(clean, strings.TrimSuffix(mp, "/")+"/") {
			continue
		}
		if len(mp) <= best {
			continue
		}
		// "-" 之后依次是 fstype、source、super options
		src := ""
		for i := 6; i < len(f)-1; i++ {
			if f[i] == "-" {
				if i+2 < len(f) {
					src = f[i+2]
				}
				break
			}
		}
		colon := strings.Index(src, ":")
		if colon <= 0 || colon == len(src)-1 {
			continue // 本地挂载没有这种「标识:路径」形态
		}
		mountPoint, remote, root, best = mp, src[:colon], src[colon+1:], len(mp)
	}
	return mountPoint, remote, root
}
