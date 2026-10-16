package main

// NAT 类型检测 + 公网 IP 查询（纯标准库 STUN 实现，RFC 3489 / 5389）。
// DDNS 的 IP 来源（auto/web）与概览页 NAT 体检共用本文件的探测能力。

import (
	"crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// ============ 公网 IP 查询 ============

// ipEchoSources 公网 IP 回显源（纯文本响应即 IP）。
var ipEchoSources = []string{
	"https://ipv4.getip.cc",
	"https://4.ident.me",
	"https://myexternalip.com/raw",
	"https://ifconfig.me/ip",
	"https://ipv4.ip.sb",
	"https://api.ipify.org",
}

var ipEchoSourcesV6 = []string{
	"https://api6.ipify.org",
	"https://ipv6.icanhazip.com",
	"https://ipv6.ip.sb",
	"https://6.ident.me",
}

// queryPublicIP 依次尝试回显源获取公网 IP；family=4/6。
func queryPublicIP(family int) (string, string, error) {
	srcs := ipEchoSources
	if family == 6 {
		srcs = ipEchoSourcesV6
	}
	client := &http.Client{Timeout: 4 * time.Second}
	var lastErr error
	for _, u := range srcs {
		ip, err := fetchEchoIP(client, u, family)
		if err == nil {
			return ip, u, nil
		}
		lastErr = err
	}
	return "", "", fmt.Errorf("全部 %d 个公网 IP 查询源均失败: %v", len(srcs), lastErr)
}

func fetchEchoIP(client *http.Client, url string, family int) (string, error) {
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 256))
	if err != nil {
		return "", err
	}
	ip := strings.TrimSpace(string(body))
	if net.ParseIP(ip) == nil {
		return "", errors.New("响应不是合法 IP: " + ip)
	}
	if family == 4 && !strings.Contains(ip, ".") {
		return "", errors.New("不是 IPv4")
	}
	if family == 6 && !strings.Contains(ip, ":") {
		return "", errors.New("不是 IPv6")
	}
	return ip, nil
}

// ============ STUN ============

const (
	stunCookie       uint32 = 0x2112A442
	attrMappedAddr          = 0x0001
	attrXORMappedAdr        = 0x0020
	attrChangeReq           = 0x0003
	attrOtherAddr           = 0x8020
)

var stunServers = []string{
	"stun.miwifi.com:3478",
	"stun.syncthing.net:3478",
	"stun.l.google.com:19302",
	"stun.qq.com:3478",
}

// stunResult 单次 binding 探测结果。
type stunResult struct {
	ip      string
	port    int
	otherIP string // OTHER-ADDRESS（若服务器提供）
	latency time.Duration
}

// stunBinding 向服务器发送 binding 请求，可选带 CHANGE-REQUEST flags。
func stunBinding(conn *net.UDPConn, server string, changeFlags uint32, timeout time.Duration) (*stunResult, error) {
	raddr, err := net.ResolveUDPAddr("udp", server)
	if err != nil {
		return nil, err
	}
	txid := make([]byte, 12)
	if _, err := rand.Read(txid); err != nil {
		return nil, err
	}
	req := make([]byte, 20)
	binary.BigEndian.PutUint16(req[0:2], 0x0001) // Binding Request
	binary.BigEndian.PutUint32(req[4:8], stunCookie)
	copy(req[8:20], txid)
	if changeFlags != 0 {
		cr := make([]byte, 4)
		binary.BigEndian.PutUint32(cr, changeFlags)
		req = append(req, packAttr(attrChangeReq, cr)...)
	}
	start := time.Now()
	if _, err := conn.WriteToUDP(req, raddr); err != nil {
		return nil, err
	}
	buf := make([]byte, 1500)
	_ = conn.SetReadDeadline(time.Now().Add(timeout))
	n, err := conn.Read(buf)
	if err != nil {
		return nil, err
	}
	lat := time.Since(start)
	if n < 20 {
		return nil, errors.New("响应过短")
	}
	if binary.BigEndian.Uint16(buf[0:2]) != 0x0101 { // Binding Success Response
		return nil, errors.New("非成功响应")
	}
	if string(buf[8:20]) != string(txid) {
		return nil, errors.New("事务 ID 不匹配")
	}
	res := &stunResult{latency: lat}
	// 遍历 attributes
	off := 20
	for off+4 <= n {
		atype := binary.BigEndian.Uint16(buf[off : off+2])
		alen := int(binary.BigEndian.Uint16(buf[off+2 : off+4]))
		if off+4+alen > n {
			break
		}
		val := buf[off+4 : off+4+alen]
		switch atype {
		case attrMappedAddr:
			if len(val) >= 8 && val[1] == 0x01 {
				res.port = int(binary.BigEndian.Uint16(val[2:4]))
				res.ip = net.IP(val[4:8]).String()
			}
		case attrXORMappedAdr:
			if len(val) >= 8 && val[1] == 0x01 {
				p := binary.BigEndian.Uint16(val[2:4]) ^ 0x2112
				ipb := make([]byte, 4)
				for i := 0; i < 4; i++ {
					ipb[i] = val[4+i] ^ byte(stunCookie>>(8*(3-i)))
				}
				res.port = int(p)
				res.ip = net.IP(ipb).String()
			}
		case attrOtherAddr:
			if len(val) >= 8 && val[1] == 0x01 {
				res.otherIP = net.IP(val[4:8]).String()
			}
		}
		off += 4 + ((alen + 3) / 4 * 4) // 4 字节对齐
	}
	if res.ip == "" {
		return nil, errors.New("响应无映射地址")
	}
	return res, nil
}

func packAttr(atype uint16, data []byte) []byte {
	out := make([]byte, 4+len(data))
	binary.BigEndian.PutUint16(out[0:2], atype)
	binary.BigEndian.PutUint16(out[2:4], uint16(len(data)))
	copy(out[4:], data)
	return out
}

// NatResult NAT 检测结论。
type NatResult struct {
	Type         string `json:"type"`         // 类型代码
	TypeText     string `json:"typeText"`     // 中文可读结论
	NATLevel     int    `json:"natLevel"`     // NAT 等级 1–4（1 最宽松，4 最差），0=未知
	NATLevelText string `json:"natLevelText"` // 等级可读文案，如 "NAT2 · 中度开放（受限圆锥）"
	Optimize     string `json:"optimize"`     // 优化建议（非 NAT1 时按类型给出）
	PublicIP     string `json:"publicIP"`     // STUN 映射出的公网 IP
	PublicPort   int    `json:"publicPort"`   // 映射端口
	CGNAT        bool   `json:"cgnat"`        // 是否运营商级 NAT
	P2PNote      string `json:"p2pNote"`      // P2P 打洞建议
	Detail       string `json:"detail"`       // 探测过程说明
	TestedAt     string `json:"testedAt"`
}

// natLevel 把探测类型映射为玩家/用户熟悉的 NAT1–NAT4 等级。
// NAT1 = 公网或全圆锥（最宽松）；NAT2 = 受限/端口受限圆锥（中度开放）；
// NAT3 = 对称型（严格）；NAT4 = CGNAT 或 UDP 阻断（最差）。
func natLevel(t string) (int, string) {
	switch t {
	case "fullcone":
		return 1, "NAT1 · 开放型（全圆锥 / 等效公网）"
	case "restricted":
		return 2, "NAT2 · 中度开放（受限圆锥）"
	case "portrestricted":
		return 2, "NAT2 · 中度开放（端口受限圆锥）"
	case "stable":
		return 2, "NAT2 · 映射稳定（圆锥类）"
	case "symmetric":
		return 3, "NAT3 · 严格（对称型）"
	case "cgnat":
		return 4, "NAT4 · 极差（运营商级 NAT）"
	case "blocked":
		return 4, "NAT4 · 极差（UDP 出站被阻断）"
	default:
		return 0, "NAT? · 未能判定"
	}
}

// natOptimize 按检测结果给出可落地的优化建议（NAT1 无需建议，返回空）。
func natOptimize(t string) string {
	switch t {
	case "restricted":
		return "已达中度开放，日常 P2P（下载、远程访问）基本可直连。若需要外网入连服务（WebDAV、BT 等），建议在路由器开启 UPnP 自动映射，或在飞牛「设置 → 网络 → 端口转发」固定映射端口，避免映射变化导致断连。"
	case "portrestricted":
		return "端口受限圆锥：大部分 P2P 应用可用。对需要入连的服务建议做固定端口映射（路由器端口转发，或将本机设为 DMZ 主机），并优先使用飞牛自带 FN Connect 中继作为兜底通道。"
	case "stable":
		return "映射稳定，可直接使用端口转发。若个别 P2P 应用偶发打洞失败，优先改用 FN Connect / Tailscale 等中继方案，无需调整路由器。"
	case "symmetric":
		return "对称型 NAT 下 P2P 打洞困难：BT、远程访问等建议改用中继方案（FN Connect、Tailscale、ZeroTier）。根本解决需在路由器开启 DMZ 主机，或将光猫改为桥接模式由路由器拨号，以获取 NAT1/NAT2。"
	case "cgnat":
		return "运营商级 NAT（100.64.x.x）导致外网无法入连：根本解决是联系运营商开通公网 IP；过渡期可用 FN Connect / Tailscale 中继，或通过已下发的 IPv6 地址做 DDNS（解析记录类型选 AAAA）。"
	case "blocked":
		return "UDP 出站被阻断：请检查路由器防火墙 / 家长控制 / 安全策略是否禁用了 UDP 出站，或改用光猫桥接由路由器拨号。STUN 不通也会影响 DDNS 的 auto/web 取公网 IP，可暂时将 IP 来源改为 DNS 解析或自定义地址。"
	default:
		return ""
	}
}

// detectNAT 完整检测并附带 NAT1–NAT4 等级与优化建议。
func detectNAT() NatResult {
	res := detectNATRaw()
	res.NATLevel, res.NATLevelText = natLevel(res.Type)
	res.Optimize = natOptimize(res.Type)
	return res
}

// detectNATRaw 完整 RFC3489 风格检测：换服务器对比 + CHANGE-REQUEST 判定。
func detectNATRaw() NatResult {
	res := NatResult{Type: "unknown", TypeText: "检测失败", TestedAt: time.Now().Format("2006-01-02 15:04:05")}

	// 本地 UDP socket（固定端口发全部请求）
	laddr := &net.UDPAddr{IP: net.IPv4zero, Port: 0}
	conn, err := net.ListenUDP("udp", laddr)
	if err != nil {
		res.Detail = "无法创建 UDP socket: " + err.Error()
		return res
	}
	defer conn.Close()

	// 1. 找第一个可用的 primary server
	var primary string
	var sa *stunResult
	for _, srv := range stunServers {
		r, err := stunBinding(conn, srv, 0, 1500*time.Millisecond)
		if err == nil {
			primary, sa = srv, r
			break
		}
	}
	if sa == nil {
		res.Type = "blocked"
		res.TypeText = "UDP 阻断或 STUN 不可达"
		res.Detail = "所有 STUN 服务器均无响应，UDP 出站可能被防火墙拦截"
		return res
	}
	res.PublicIP, res.PublicPort = sa.ip, sa.port

	// CGNAT：映射地址落在 100.64.0.0/10（运营商级 NAT）
	if isCGNAT(sa.ip) {
		res.CGNAT = true
		res.Type = "cgnat"
		res.TypeText = "CGNAT（运营商级 NAT）"
		res.Detail = fmt.Sprintf("映射地址 %s 属于 100.64.0.0/10 运营商保留段，处于多层 NAT 之后，外网无法直接入连", sa.ip)
		res.P2PNote = "P2P 打洞成功率低，建议依赖中继服务（如 FN Connect）或联系运营商开通公网 IP"
		return res
	}

	// 2. 换 secondary server：映射 IP/端口变化 → 对称型
	for _, srv := range stunServers {
		if srv == primary {
			continue
		}
		r, err := stunBinding(conn, srv, 0, 1500*time.Millisecond)
		if err != nil {
			continue
		}
		if r.ip != sa.ip || r.port != sa.port {
			res.Type = "symmetric"
			res.TypeText = "对称型 NAT"
			res.Detail = fmt.Sprintf("%s 与 %s 返回的映射地址不同（%s:%d vs %s:%d），典型的对称型 NAT", primary, srv, sa.ip, sa.port, r.ip, r.port)
			res.P2PNote = "对称型 NAT 下 P2P 打洞困难，需中继"
			return res
		}
		break // 一个 secondary 足够
	}

	// 3. CHANGE-REQUEST 到映射地址：区分圆锥型各子类
	target := net.JoinHostPort(sa.ip, fmt.Sprint(sa.port))
	r2, err2 := stunBinding(conn, target, 0x00000002, 1500*time.Millisecond) // change IP + port
	if err2 == nil {
		_ = r2
		res.Type = "fullcone"
		res.TypeText = "开放型（Full Cone）NAT"
		res.Detail = fmt.Sprintf("映射地址 %s:%d 对任意外部目的地址均放行入站，NAT 类型最宽松", sa.ip, sa.port)
		res.P2PNote = "任何 P2P 应用均可直连，端口映射最稳定"
		return res
	}
	r3, err3 := stunBinding(conn, target, 0x00000001, 1500*time.Millisecond) // change port only
	if err3 == nil {
		_ = r3
		res.Type = "restricted"
		res.TypeText = "受限圆锥型（Restricted Cone）NAT"
		res.Detail = fmt.Sprintf("映射地址 %s:%d 对本机曾通信过的外部地址放行入站", sa.ip, sa.port)
		res.P2PNote = "P2P 打洞成功率高（一端先发包即可）"
		return res
	}
	// change-request 均无响应：可能是端口受限圆锥，也可能是服务器不支持该扩展
	if sa.otherIP != "" {
		// 服务器声明了对端地址（说明支持 CHANGE-REQUEST 机制）→ 端口受限圆锥
		res.Type = "portrestricted"
		res.TypeText = "端口受限圆锥型（Port Restricted Cone）NAT"
		res.Detail = fmt.Sprintf("映射地址 %s:%d 仅对本机曾通信过的 外部IP+端口 放行", sa.ip, sa.port)
		res.P2PNote = "P2P 打洞需双方同时发包（生日悖论），成功率中等"
		return res
	}
	res.Type = "stable"
	res.TypeText = "稳定映射型（圆锥类）NAT"
	res.Detail = fmt.Sprintf("映射地址 %s:%d 在不同服务器间保持一致（非对称型），公网 IP:%s", sa.ip, sa.port, sa.ip)
	res.P2PNote = "映射稳定，端口转发与 P2P 均可用"
	return res
}

// isCGNAT 判断是否 100.64.0.0/10。
func isCGNAT(ipStr string) bool {
	ip := net.ParseIP(ipStr).To4()
	if ip == nil {
		return false
	}
	return ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127
}

// ============ 网卡枚举（DDNS interface 源 / 概览共用） ============

// listNICs 返回非回环网卡名与 IPv4 地址（排除链路本地）。
func listNICs() []struct {
	Name string
	IPv4 string
	IPv6 string
} {
	var out []struct {
		Name string
		IPv4 string
		IPv6 string
	}
	ifaces, err := net.Interfaces()
	if err != nil {
		return out
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagLoopback != 0 || ifc.Flags&net.FlagUp == 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		var v4, v6 string
		for _, a := range addrs {
			ipnet, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			ip := ipnet.IP
			if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
				continue
			}
			if v4 == "" && ip.To4() != nil {
				v4 = ip.String()
			}
			if v6 == "" && ip.To4() == nil {
				v6 = ip.String()
			}
		}
		if v4 != "" || v6 != "" {
			out = append(out, struct {
				Name string
				IPv4 string
				IPv6 string
			}{ifc.Name, v4, v6})
		}
	}
	return out
}

// nicIPv4 取指定网卡（或自动挑选）的 IPv4；自动挑选优先公网段、私网垫底。
func nicIPv4(name string) (string, error) {
	nic := listNICs()
	for _, n := range nic {
		if n.Name == name && n.IPv4 != "" {
			return n.IPv4, nil
		}
	}
	if name == "" || name == "auto" {
		var private string
		for _, n := range nic {
			if n.IPv4 == "" {
				continue
			}
			ip := net.ParseIP(n.IPv4).To4()
			if ip == nil {
				continue
			}
			if ip[0] == 10 || (ip[0] == 172 && ip[1] >= 16 && ip[1] <= 31) || (ip[0] == 192 && ip[1] == 168) || (ip[0] == 100 && ip[1] >= 64 && ip[1] <= 127) {
				if private == "" {
					private = n.IPv4
				}
				continue
			}
			return n.IPv4, nil // 公网段优先
		}
		if private != "" {
			return private, nil
		}
	}
	return "", errors.New("未找到可用网卡地址")
}

var (
	publicIPv4Cache   string
	publicIPv4CacheAt time.Time
	publicIPv4Mu      sync.Mutex
)

// publicIPv4Cached 带 60s 缓存的公网 IPv4 查询（DDNS/概览轮询避免重复外联）。
func publicIPv4Cached() (string, error) {
	publicIPv4Mu.Lock()
	defer publicIPv4Mu.Unlock()
	if publicIPv4Cache != "" && time.Since(publicIPv4CacheAt) < 60*time.Second {
		return publicIPv4Cache, nil
	}
	ip, _, err := queryPublicIP(4)
	if err != nil {
		if publicIPv4Cache != "" {
			return publicIPv4Cache, nil // 陈旧缓存好过没有
		}
		return "", err
	}
	publicIPv4Cache, publicIPv4CacheAt = ip, time.Now()
	return ip, nil
}

// nat 探测结果缓存：完整探测要连发多个 STUN 请求（最多可阻塞 10 秒+），
// 高频调用（页面定时刷新、用户连点重测）一律沿用 5 分钟内的缓存结果，仅 force 才重测。
var (
	natCacheMu  sync.Mutex
	natCacheRes NatResult
	natCacheAt  time.Time
)

// handleNAT 触发一次 STUN 探测并返回结论；?force=1 强制重测，否则沿用 5 分钟缓存。
func (s *Server) handleNAT(w http.ResponseWriter, r *http.Request) {
	force := r.URL.Query().Get("force") == "1"
	natCacheMu.Lock()
	if !force && natCacheAt.Add(5*time.Minute).After(time.Now()) {
		res := natCacheRes
		natCacheMu.Unlock()
		ok(w, map[string]any{"nat": res})
		return
	}
	natCacheMu.Unlock()

	res := detectNAT()

	natCacheMu.Lock()
	natCacheRes, natCacheAt = res, time.Now()
	natCacheMu.Unlock()
	ok(w, map[string]any{"nat": res})
}
