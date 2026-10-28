package main

// 系统网络信息：网卡、IP、MAC、网关、DNS、实时收发速率（供概览页与 DDNS 复用）。

import (
	"bufio"
	"context"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

type NetIface struct {
	Name      string   `json:"name"`
	MAC       string   `json:"mac"`
	MTU       int      `json:"mtu"`
	Up        bool     `json:"up"`
	IPv4      []string `json:"ipv4"`
	IPv6      []string `json:"ipv6"`
	RxBytes   int64    `json:"rxBytes"`
	TxBytes   int64    `json:"txBytes"`
	RxRate    int64    `json:"rxRate"` // B/s（两次概览请求差值）
	TxRate    int64    `json:"txRate"`
	IsDefault bool     `json:"isDefault"`
	IsWan     bool     `json:"isWan"` // 是否在默认路由上（WAN 口）
}

type NetInfo struct {
	Ifaces   []NetIface `json:"ifaces"`
	Gateway  string     `json:"gateway"`
	Gateway6 string     `json:"gateway6"`
	DNS      []string   `json:"dns"`
}

// netCounter 网卡累计字节快照（仪表盘每秒轮询用）
type netCounter struct {
	RxBytes int64 `json:"rx"`
	TxBytes int64 `json:"tx"`
}

// collectNetCounters 只读 /proc/net/dev 的累计字节数，不做差值计算。
// 速率由前端按**真实请求间隔**换算（浏览器端最清楚两次请求实际隔了多久，
// 且不会与 30s 的 overview 共用同一份采样状态互相干扰）。
func collectNetCounters() map[string]netCounter {
	dev, err := readProcNetDev()
	if err != nil {
		return map[string]netCounter{}
	}
	out := make(map[string]netCounter, len(dev))
	for name, v := range dev {
		out[name] = netCounter{RxBytes: v[0], TxBytes: v[1]}
	}
	return out
}

var netLastMu sync.Mutex
var netLastStat map[string][2]int64 // name -> [rx,tx]
var netLastAt time.Time

// collectNetInfo 静态信息（网卡/IP/网关/DNS/默认路由）短缓存：概览页每次刷新都调用，
// 但网卡结构、网关、DNS 是低频变化的系统信息，5s 内复用上次结果即可——
// 缓存命中能省去 spawn `ip route` 子进程与反复扫描 /proc（网盘同步繁忙时尤其明显）。
// 速率本身实时性要求高，这里只作为速率的备用来源（主实时速率走 /api/netrates），
// 5s 滞后无感知。
var netInfoCache NetInfo
var netInfoCacheAt time.Time
var netInfoCached bool

// collectNetInfo 收集当前网络信息（速率基于上次采集差值）。
func collectNetInfo() NetInfo {
	if netInfoCached && time.Since(netInfoCacheAt) < 5*time.Second {
		return netInfoCache
	}
	info := collectNetInfoLive()
	netInfoCache, netInfoCacheAt, netInfoCached = info, time.Now(), true
	return info
}

func collectNetInfoLive() NetInfo {
	info := NetInfo{}
	dev, err := readProcNetDev()
	if err != nil {
		dev = map[string][2]int64{}
	}

	// 默认路由网卡
	defIf4 := defaultRouteIface()
	defIf6 := defaultRouteIface6()
	info.Gateway = routeGateway(false)
	info.Gateway6 = routeGateway(true)
	info.DNS = readResolvConf()

	ifaces, err := net.Interfaces()
	if err == nil {
		for _, ifc := range ifaces {
			if ifc.Flags&net.FlagLoopback != 0 {
				continue
			}
			nf := NetIface{Name: ifc.Name, MTU: ifc.MTU, Up: ifc.Flags&net.FlagUp != 0}
			if mac := ifc.HardwareAddr.String(); mac != "" {
				nf.MAC = mac
			}
			addrs, _ := ifc.Addrs()
			for _, a := range addrs {
				ipnet, ok := a.(*net.IPNet)
				if !ok {
					continue
				}
				ip := ipnet.IP
				if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
					continue
				}
				if v4 := ip.To4(); v4 != nil {
					nf.IPv4 = append(nf.IPv4, v4.String())
				} else if v6 := ip.To16(); v6 != nil {
					nf.IPv6 = append(nf.IPv6, v6.String())
				}
			}
			if d, ok := dev[ifc.Name]; ok {
				nf.RxBytes, nf.TxBytes = d[0], d[1]
			}
			nf.IsDefault = (ifc.Name == defIf4 || ifc.Name == defIf6)
			nf.IsWan = nf.IsDefault
			info.Ifaces = append(info.Ifaces, nf)
		}
	}

	// 速率计算
	now := time.Now()
	netLastMu.Lock()
	if netLastStat != nil && !netLastAt.IsZero() && now.Sub(netLastAt) < 10*time.Second {
		dt := now.Sub(netLastAt).Seconds()
		for i := range info.Ifaces {
			n := info.Ifaces[i].Name
			if prev, ok := netLastStat[n]; ok {
				if d := info.Ifaces[i].RxBytes - prev[0]; d >= 0 {
					info.Ifaces[i].RxRate = int64(float64(d) / dt)
				}
				if d := info.Ifaces[i].TxBytes - prev[1]; d >= 0 {
					info.Ifaces[i].TxRate = int64(float64(d) / dt)
				}
			}
		}
	}
	cur := map[string][2]int64{}
	for _, n := range info.Ifaces {
		cur[n.Name] = [2]int64{n.RxBytes, n.TxBytes}
	}
	netLastStat, netLastAt = cur, now
	netLastMu.Unlock()

	return info
}

func readProcNetDev() (map[string][2]int64, error) {
	f, err := os.Open("/proc/net/dev")
	if err != nil {
		return nil, err
	}
	defer f.Close()
	out := map[string][2]int64{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		colon := strings.Index(line, ":")
		if colon < 0 {
			continue
		}
		name := strings.TrimSpace(line[:colon])
		fields := strings.Fields(line[colon+1:])
		if len(fields) < 9 {
			continue
		}
		rx, _ := strconv.ParseInt(fields[0], 10, 64)
		tx, _ := strconv.ParseInt(fields[8], 10, 64)
		out[name] = [2]int64{rx, tx}
	}
	return out, nil
}

// routeGateway 取默认网关。优先 `ip route show default`（fnOS/Debian 一定有 iproute2，
// 且对 policy routing 兼容），失败再回退 /proc/net/route 解析。
func routeGateway(v6 bool) string {
	if g := routeGatewayViaIP(v6); g != "" {
		return g
	}
	if v6 {
		return routeGatewayIPv6()
	}
	return routeGatewayProc()
}

// routeGatewayViaIP 解析 `ip route show default` / `ip -6 route show default` 输出：
//   default via 192.168.31.1 dev eth0 ...
func routeGatewayViaIP(v6 bool) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	args := []string{"route", "show", "default"}
	if v6 {
		args = append([]string{"-6"}, args...)
	}
	out, err := exec.CommandContext(ctx, "ip", args...).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		for i, w := range f {
			if w == "via" && i+1 < len(f) {
				return f[i+1]
			}
		}
	}
	return ""
}

// routeGatewayProc 从 /proc/net/route 解析默认网关（IPv4）。
func routeGatewayProc() string {
	src, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer src.Close()
	sc := bufio.NewScanner(src)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		if f[1] != "00000000" { // 目标 0.0.0.0 = 默认路由
			continue
		}
		if f[3] != "00000000" { // 掩码
			continue
		}
		// gateway 是十六进制小端
		ip := hexLeToIPv4(f[2])
		if ip != "" {
			return ip
		}
	}
	return ""
}

func routeGatewayIPv6() string {
	src, err := os.Open("/proc/net/ipv6_route")
	if err != nil {
		return ""
	}
	defer src.Close()
	sc := bufio.NewScanner(src)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			continue
		}
		// 第一列为目标前缀（全 0 = 默认路由），第 5 列为下一跳网关
		if !isAllZero(f[0]) {
			continue
		}
		if f[4] != "00000000000000000000000000000000" {
			ip := expandIPv6(f[4])
			if ip != "" && !strings.HasPrefix(ip, "fe80") {
				return ip
			}
		}
	}
	return ""
}

func isAllZero(hexStr string) bool {
	for _, c := range hexStr {
		if c != '0' {
			return false
		}
	}
	return true
}

// hexLeToIPv4 解析 /proc/net/route 的网关字段：十六进制小端（如 0101A8C0 = 192.168.0.1），
// 必须把字节序反转后才得到点分十进制。
func hexLeToIPv4(h string) string {
	if len(h) != 8 {
		return ""
	}
	b := make([]byte, 4)
	for i := 0; i < 4; i++ {
		v, err := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
		if err != nil {
			return ""
		}
		b[3-i] = byte(v) // 小端 → 高位字节在字符串尾部，反转写入
	}
	return net.IP(b).String()
}

func expandIPv6(h string) string {
	if len(h) != 32 {
		return ""
	}
	raw := make([]byte, 16)
	for i := 0; i < 16; i++ {
		b, err := strconv.ParseUint(h[2*i:2*i+2], 16, 8)
		if err != nil {
			return ""
		}
		raw[i] = byte(b)
	}
	return net.IP(raw).String()
}

// defaultRouteIface 从 /proc/net/route 取默认路由网卡名。
func defaultRouteIface() string {
	src, err := os.Open("/proc/net/route")
	if err != nil {
		return ""
	}
	defer src.Close()
	sc := bufio.NewScanner(src)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 4 {
			continue
		}
		if f[1] == "00000000" && f[3] == "00000000" {
			return f[0]
		}
	}
	return ""
}

func defaultRouteIface6() string {
	src, err := os.Open("/proc/net/ipv6_route")
	if err != nil {
		return ""
	}
	defer src.Close()
	sc := bufio.NewScanner(src)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		// /proc/net/ipv6_route 每行 10 列：dst dstlen src srclen nexthop metric refcnt use flags device
		if len(f) < 10 {
			continue
		}
		if isAllZero(f[0]) && isAllZero(f[1]) {
			return f[9] // 第 10 列为出接口
		}
	}
	return ""
}

func readResolvConf() []string {
	b, err := os.ReadFile("/etc/resolv.conf")
	if err != nil {
		return nil
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		f := strings.Fields(line)
		if len(f) >= 2 && f[0] == "nameserver" {
			ns := f[1]
			// 去 IPv6 zone id（如 fe80::1%enp3s0-ovs → fe80::1），界面只展示地址本身
			if i := strings.Index(ns, "%"); i >= 0 {
				ns = ns[:i]
			}
			out = append(out, ns)
		}
	}
	return out
}
