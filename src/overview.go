package main

import (
	"bufio"
	"context"
	"net/http"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"
)

// execCommand 带超时执行外部命令，返回 stdout。
func execCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	return cmd.Output()
}

// ============ 系统概览 ============

type DiskInfo struct {
	Mount    string `json:"mount"`
	Device   string `json:"device"`
	FsType   string `json:"fsType"`
	TotalKB  int64  `json:"totalKB"`
	FreeKB   int64  `json:"freeKB"`
	AvailKB  int64  `json:"availKB"`
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	ov := map[string]any{}

	ov["version"] = version
	if h, err := os.Hostname(); err == nil {
		ov["hostname"] = h
	}
	ov["platform"] = runtime.GOOS + "/" + runtime.GOARCH
	ov["goVersion"] = runtime.Version()

	if b, err := os.ReadFile("/proc/sys/kernel/osrelease"); err == nil {
		ov["kernel"] = strings.TrimSpace(string(b))
	}
	pretty := readOsRelease()
	if pretty != "" {
		ov["os"] = pretty
	}
	if b, err := os.ReadFile("/proc/cpuinfo"); err == nil {
		model := ""
		cores := 0
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "model name"); ok {
				if idx := strings.Index(v, ":"); idx >= 0 && model == "" {
					model = strings.TrimSpace(v[idx+1:])
				}
			}
			if strings.HasPrefix(line, "processor") {
				cores++
			}
		}
		ov["cpuModel"] = model
		ov["cpuCores"] = cores
	}

	if b, err := os.ReadFile("/proc/uptime"); err == nil {
		fs := strings.Fields(string(b))
		if len(fs) > 0 {
			if up, err := strconv.ParseFloat(fs[0], 64); err == nil {
				ov["uptimeSeconds"] = int64(up)
			}
		}
	}
	if b, err := os.ReadFile("/proc/loadavg"); err == nil {
		fs := strings.Fields(string(b))
		if len(fs) >= 3 {
			ov["load1"], _ = strconv.ParseFloat(fs[0], 64)
			ov["load5"], _ = strconv.ParseFloat(fs[1], 64)
			ov["load15"], _ = strconv.ParseFloat(fs[2], 64)
		}
	}

	// 内存
	var memTotalKB, memAvailKB int64
	if f, err := os.Open("/proc/meminfo"); err == nil {
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "MemTotal:"); ok {
				memTotalKB, _ = strconv.ParseInt(strings.Fields(v)[0], 10, 64)
			}
			if v, ok := strings.CutPrefix(sc.Text(), "MemAvailable:"); ok {
				memAvailKB, _ = strconv.ParseInt(strings.Fields(v)[0], 10, 64)
			}
		}
		f.Close()
	}
	ov["memTotalKB"] = memTotalKB
	ov["memAvailKB"] = memAvailKB

	// 磁盘：/ 与 /vol*（fnOS 存储空间挂载点）
	ov["disks"] = listDisks()

	// 硬件温度（CPU / 硬盘，传感器缺失时为 0，前端显示「—」）
	ov["temps"] = collectTempsCached()

	// 端口概况（供概览页直接展示）
	portEntries := &[]PortEntry{}
	uidMap := loadPasswdUsers()
	inodeProc := buildInodeProcMap()
	for _, src := range []struct {
		file, proto string
	}{
		{"/proc/net/tcp", "tcp"}, {"/proc/net/tcp6", "tcp6"},
		{"/proc/net/udp", "udp"}, {"/proc/net/udp6", "udp6"},
	} {
		parseProcNet(src.file, src.proto, uidMap, inodeProc, portEntries)
	}
	ov["listeningPorts"] = countState(*portEntries, "LISTEN")
	ov["publicPorts"] = countPublic(*portEntries)

	// 网络信息（网卡 / 网关 / DNS / 实时速率）
	ov["net"] = collectNetInfo()

	// 飞牛系统版本（系统信息卡展示）
	ov["fnosVersion"] = readFnOSVersion()

	ok(w, ov)
}

// readFnOSVersion 读取 fnOS 系统版本。
// 真机实证：fnOS 主版本文件是 /usr/trim/etc/version（内容如 1.2.0800）；/etc/fnos/version 不一定存在。
// 回退从 os-release 提取时必须要求含点号的版本（x.y/z），否则会把 Debian 12 的
// PRETTY_NAME（"Debian GNU/Linux 12"）误提取为 "12"。
func readFnOSVersion() string {
	for _, p := range []string{
		"/usr/trim/etc/version",
		"/etc/fnos/version",
		"/etc/fnos/release",
		"/etc/fnos/VERSION",
		"/etc/fnos_version",
		"/var/trim/version",
	} {
		v := strings.TrimSpace(readSmall(p))
		if v == "" {
			continue
		}
		// key=value 形式（如 version=1.2.0800）取等号后的值
		if _, val, ok := strings.Cut(v, "="); ok && strings.TrimSpace(val) != "" {
			return strings.Trim(strings.TrimSpace(val), `"`)
		}
		return v
	}
	// 回退：os-release 里含点号的版本号（拒绝纯数字，避开 Debian 12）
	if pretty := readOsRelease(); pretty != "" {
		if m := regexp.MustCompile(`\d+\.\d+(\.\d+)?`).FindString(pretty); m != "" {
			return m
		}
	}
	return ""
}

// listDisks 从 /proc/mounts 取真实块设备挂载点（排除虚拟 fs），statfs 求用量。
func listDisks() []DiskInfo {
	var out []DiskInfo
	seen := map[string]bool{}

	f, err := os.Open("/proc/mounts")
	if err != nil {
		return out
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) < 4 {
			continue
		}
		dev, mount, fsType := fields[0], fields[1], fields[2]
		// 只关心真实磁盘文件系统
		switch fsType {
		case "ext2", "ext3", "ext4", "xfs", "btrfs", "f2fs", "ntfs", "exfat", "vfat":
		default:
			continue
		}
		// 只看 / 与 /vol*（fnOS 存储空间），排除系统内部杂项挂载
		if mount != "/" && !strings.HasPrefix(mount, "/vol") {
			continue
		}
		// 同一设备重复挂载取第一个
		key := dev + "@" + mount
		if seen[key] {
			continue
		}
		seen[key] = true

		var st syscall.Statfs_t
		if err := syscall.Statfs(mount, &st); err != nil {
			continue
		}
		bsize := int64(st.Bsize)
		d := DiskInfo{
			Mount: mount, Device: dev, FsType: fsType,
			TotalKB: int64(st.Blocks) * bsize / 1024,
			FreeKB:  int64(st.Bfree) * bsize / 1024,
			AvailKB: int64(st.Bavail) * bsize / 1024,
		}
		out = append(out, d)
	}
	// 根分区排最前，其余按挂载点
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j].Mount < out[j-1].Mount; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

func readOsRelease() string {
	b, err := os.ReadFile("/etc/os-release")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
			return strings.Trim(v, `"`)
		}
	}
	return ""
}
