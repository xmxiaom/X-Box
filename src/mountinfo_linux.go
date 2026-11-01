//go:build linux

package main

import (
	"os"
	"path"
	"strings"
)

// mountSourceOf 返回包含 p 的最具体挂载点，以及它的挂载源（mountinfo 里 "-" 之后的第二列）。
// 对飞牛网盘挂载，源形如 `1000-1-abc:/cloud-storage/v1/dav`，据此可定位网关地址与根路径。
func mountSourceOf(p string) (mountPoint, source string) {
	b, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		return "", ""
	}
	clean := path.Clean(p)
	bestLen := -1
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) < 5 {
			continue
		}
		mp := unescapeMountField(f[4])
		if clean != mp && !strings.HasPrefix(clean, strings.TrimSuffix(mp, "/")+"/") {
			continue
		}
		if len(mp) <= bestLen {
			continue
		}
		src := ""
		for i := 6; i < len(f)-1; i++ {
			if f[i] == "-" {
				if i+2 < len(f) {
					src = f[i+2]
				}
				break
			}
		}
		mountPoint, source, bestLen = mp, src, len(mp)
	}
	return mountPoint, source
}

// unescapeMountField mountinfo 里空格等字符转义为 \040 形式
func unescapeMountField(s string) string {
	r := strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)
	return path.Clean(r.Replace(s))
}
