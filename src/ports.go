package main

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// ============ 数据结构 ============

type PortEntry struct {
	Proto    string `json:"proto"`    // tcp / tcp6 / udp / udp6
	Local    string `json:"local"`    // 监听地址:端口
	Remote   string `json:"remote"`   // 对端（无连接为空）
	State    string `json:"state"`    // LISTEN / ESTABLISHED / …
	PID      int    `json:"pid"`      // 0 = 未知（内核态或无权限）
	Process  string `json:"process"`  // 进程名
	User     string `json:"user"`     // 属主（socket uid 对应的用户名）
	Exposure string `json:"exposure"` // public / lan / local
}

// /proc/net/*/st 字段（十六进制）
var tcpStates = map[string]string{
	"01": "ESTABLISHED", "02": "SYN_SENT", "03": "SYN_RECV",
	"04": "FIN_WAIT1", "05": "FIN_WAIT2", "06": "TIME_WAIT",
	"07": "CLOSE", "08": "CLOSE_WAIT", "09": "LAST_ACK",
	"0A": "LISTEN", "0B": "CLOSING",
}

func (s *Server) handlePorts(w http.ResponseWriter, r *http.Request) {
	uidMap := loadPasswdUsers()
	inodeProc := buildInodeProcMap()

	var entries []PortEntry
	for _, src := range []struct {
		file, proto string
	}{
		{"/proc/net/tcp", "tcp"}, {"/proc/net/tcp6", "tcp6"},
		{"/proc/net/udp", "udp"}, {"/proc/net/udp6", "udp6"},
	} {
		parseProcNet(src.file, src.proto, uidMap, inodeProc, &entries)
	}

	sort.Slice(entries, func(i, j int) bool {
		if (entries[i].State == "LISTEN") != (entries[j].State == "LISTEN") {
			return entries[i].State == "LISTEN"
		}
		return entries[i].Local < entries[j].Local
	})

	ok(w, map[string]any{
		"ports":     entries,
		"listeners": countState(entries, "LISTEN"),
		"public":    countPublic(entries),
	})
}

func countState(entries []PortEntry, state string) int {
	n := 0
	for _, e := range entries {
		if e.State == state {
			n++
		}
	}
	return n
}

func countPublic(entries []PortEntry) int {
	n := 0
	for _, e := range entries {
		if e.State == "LISTEN" && e.Exposure == "public" {
			n++
		}
	}
	return n
}

// parseProcNet 解析一份 /proc/net/xxx。
// 行格式： sl local_address rem_address st tx:rx tr:when retrnsmt uid timeout inode ...
func parseProcNet(file, proto string, uidMap map[int]string, inodeProc map[string]procInfo, out *[]PortEntry) {
	f, err := os.Open(file)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64*1024), 256*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		if lineNo == 1 {
			continue // 表头
		}
		fields := strings.Fields(sc.Text())
		if len(fields) < 10 {
			continue
		}
		local := parseHexSockAddr(fields[1])
		remote := parseHexSockAddr(fields[2])
		if local == "" {
			continue
		}
		state := "—"
		if proto == "tcp" || proto == "tcp6" {
			if v, has := tcpStates[strings.ToUpper(fields[3])]; has {
				state = v
			}
		} else {
			state = "UNCONN"
		}
		uid, _ := strconv.Atoi(fields[7])
		inode := fields[9]

		e := PortEntry{
			Proto:   proto,
			Local:   local,
			Remote:  remote,
			State:   state,
			User:    uidMap[uid],
			Process: "-",
		}
		if pi, has := inodeProc[inode]; has {
			e.PID = pi.pid
			e.Process = pi.comm
		}
		e.Exposure = classifyExposure(local)
		*out = append(*out, e)
	}
}

// parseHexSockAddr "0100007F:1F90" → "127.0.0.1:8080"
// IPv4 部分 4 字节按小端存储；IPv6 为 4 组小端 32 位。
func parseHexSockAddr(s string) string {
	colon := strings.LastIndex(s, ":")
	if colon < 0 {
		return ""
	}
	ipHex, portHex := s[:colon], s[colon+1:]
	port, err := strconv.ParseUint(portHex, 16, 16)
	if err != nil {
		return ""
	}
	raw, err := hex.DecodeString(ipHex)
	if err != nil {
		return ""
	}
	var ip net.IP
	switch len(raw) {
	case 4: // IPv4 小端
		ip = net.IPv4(raw[3], raw[2], raw[1], raw[0])
	case 16: // IPv6：每 4 字节一组小端
		b := make([]byte, 16)
		for g := 0; g < 4; g++ {
			for i := 0; i < 4; i++ {
				b[g*4+i] = raw[g*4+3-i]
			}
		}
		ip = net.IP(b)
	default:
		return ""
	}
	return net.JoinHostPort(ip.String(), strconv.Itoa(int(port)))
}

// classifyExposure 按监听地址分级暴露面。
func classifyExposure(local string) string {
	host, _, err := net.SplitHostPort(local)
	if err != nil {
		return "local"
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return "lan"
	}
	if ip.IsUnspecified() {
		return "public" // 0.0.0.0 / ::
	}
	if ip.IsLoopback() {
		return "local"
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() {
		return "lan"
	}
	return "public"
}

// ============ 进程映射 ============

type procInfo struct {
	pid  int
	comm string
}

// buildInodeProcMap 扫描 /proc/[pid]/fd/*，建立 socket:[inode] → 进程 的映射。
// /proc/net/* 里的 inode 与 fd 链接目标一一对应。
func buildInodeProcMap() map[string]procInfo {
	out := map[string]procInfo{}
	pids, _ := filepath.Glob("/proc/[0-9]*")
	for _, pdir := range pids {
		pidStr := filepath.Base(pdir)
		pid, err := strconv.Atoi(pidStr)
		if err != nil {
			continue
		}
		comm := readComm(pdir)
		fds, err := os.ReadDir(filepath.Join(pdir, "fd"))
		if err != nil {
			continue // 无权限（不该发生：root 运行）
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(pdir, "fd", fd.Name()))
			if err != nil {
				continue
			}
			if inode, ok := strings.CutPrefix(link, "socket:["); ok {
				inode = strings.TrimSuffix(inode, "]")
				if _, exists := out[inode]; !exists {
					out[inode] = procInfo{pid: pid, comm: comm}
				}
			}
		}
	}
	return out
}

func readComm(pdir string) string {
	b, err := os.ReadFile(filepath.Join(pdir, "comm"))
	if err != nil {
		return "?"
	}
	return strings.TrimSpace(string(b))
}

// loadPasswdUsers uid → 用户名（/etc/passwd 一次性解析）。
func loadPasswdUsers() map[int]string {
	out := map[int]string{0: "root"}
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return out
	}
	for _, line := range strings.Split(string(b), "\n") {
		parts := strings.Split(line, ":")
		if len(parts) < 3 {
			continue
		}
		if uid, err := strconv.Atoi(parts[2]); err == nil {
			out[uid] = parts[0]
		}
	}
	return out
}

var (
	passwdOnce  sync.Once
	passwdCache map[int]string
)

// cachedPasswdUsers 进程明细逐个读 Uid 时用缓存版，避免反复解析 /etc/passwd。
func cachedPasswdUsers() map[int]string {
	passwdOnce.Do(func() { passwdCache = loadPasswdUsers() })
	return passwdCache
}

// formatPort 用于前端展示（预留）
var _ = fmt.Sprintf
