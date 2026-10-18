// X-Box — 飞牛 fnOS 系统清理与安全体检工具
//
// 单二进制 + 内嵌 Web UI，纯标准库实现。
// 功能：垃圾扫描/安全清理、开放端口审计、进程/服务/应用一览。
//
// 安全设计（清理相关，硬约束）：
//   1. 仅允许白名单根目录下的文件（/tmp /var/tmp /var/log /var/cache/apt
//      /var/lib/apt/lists /var/crash /var/lib/systemd/coredump 及
//      /vol*/@appdata/<app>/{logs,tmp,thumbnails}）；
//   2. 只删普通文件：跳过符号链接、socket、设备、目录，绝不递归删目录树；
//   3. 每个分类有最小文件年龄阈值，扫描与清理两阶段都各自复核。
//
// 访问控制（面板暴露面）：面板仅监听 127.0.0.1:8899（本机回环，不绑定任何
// 对外网卡）——局域网/公网无法直连面板；唯一入口是 fnOS 自身的远程访问体系
// （fnos.net 域名/PC 客户端/手机 App，均需飞牛账号登录），由 fnOS 代验身份后
// 从本机回环转发进来。请勿为了外网直连而修改此绑定。
package main

import (
	"bytes"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

const (
	version = "1.2.77"
	// 面板监听策略：回环 + 首个局域网私网 IPv4（运行时探测）。
	// 不绑定 0.0.0.0/通配地址——避免对公网/所有网卡（含 docker 桥)暴露；
	// 局域网私网 IP 保证 fnOS 桌面门户（经 NAS 地址嵌入 iframe）与本机访问可达；
	// 外网访问仍只能经 fnOS 远程访问体系（需飞牛账号）。请勿改为 ":8899" 通配绑定。
	listenPort = "8899"
)

type Server struct {
	dataDir string

	// 最近一次扫描的候选清单（清理时逐条复核后使用）
	scanMu    sync.Mutex
	scanTime  time.Time
	candidate map[string]map[string]int64 // category -> path -> size
}

func main() {
	dataDir := os.Getenv("XBOX_DATA")
	if dataDir == "" {
		dataDir = "."
	}
	_ = os.MkdirAll(dataDir, 0o755)

	srv := &Server{dataDir: dataDir, candidate: map[string]map[string]int64{}}

	// 初始化 DDNS、终端子系统
	initDDNS(dataDir)
	initTerminal()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/overview", srv.handleOverview)
	mux.HandleFunc("GET /api/temps", handleTemps)
	mux.HandleFunc("GET /api/scan", srv.handleScan)
	mux.HandleFunc("POST /api/clean", srv.handleClean)
	mux.HandleFunc("GET /api/ports", srv.handlePorts)
	mux.HandleFunc("GET /api/processes", srv.handleProcesses)
	mux.HandleFunc("POST /api/proc/kill", srv.handleProcKill)
	mux.HandleFunc("GET /api/services", srv.handleServices)
	mux.HandleFunc("GET /api/autostart", srv.handleAutostart)
	mux.HandleFunc("GET /api/nat", srv.handleNAT)
	mux.HandleFunc("GET /api/ddns", srv.handleDDNSList)
	mux.HandleFunc("POST /api/ddns/provider/save", srv.handleDDNSProviderSave)
	mux.HandleFunc("POST /api/ddns/provider/del", srv.handleDDNSProviderDel)
	mux.HandleFunc("POST /api/ddns/provider/test", srv.handleDDNSProviderTest)
	mux.HandleFunc("POST /api/ddns/record/save", srv.handleDDNSRecordSave)
	mux.HandleFunc("POST /api/ddns/record/del", srv.handleDDNSRecordDel)
	mux.HandleFunc("POST /api/ddns/sync", srv.handleDDNSSync)
	mux.HandleFunc("GET /api/terminal/info", srv.handleTerminalInfo)
	mux.HandleFunc("POST /api/terminal/exec", srv.handleTerminalExec)
	mux.HandleFunc("POST /api/terminal/close", srv.handleTerminalClose)
	mux.HandleFunc("GET /api/nvme-sleep", handleNVMeSleepList)
	mux.HandleFunc("POST /api/nvme-sleep", handleNVMeSleepSet)
	mux.HandleFunc("GET /api/update/check", handleUpdateCheck)
	mux.HandleFunc("POST /api/update/download", handleUpdateDownload)
	mux.HandleFunc("GET /api/update/dirs", handleUpdateDirs)
	mux.HandleFunc("GET /api/update/status", handleUpdateStatus)
	mux.HandleFunc("GET /api/update/file", handleUpdateFile)
	// 通道探测端点：登录态由 fnOS 网关校验，这里只回报已认证，
	// 让前端不需要靠 404 判定链路可用
	mux.HandleFunc("GET /api/auth/status", func(w http.ResponseWriter, r *http.Request) {
		ok(w, map[string]any{"authed": true})
	})
	mux.HandleFunc("GET /api/browser/log", handleBrowserLog)
	mux.HandleFunc("GET /api/browser/snap", handleBrowserSnap)
	// 浏览器代理走顶级路径 /b（fnOS 中转只拦 /api/*，页面导航路径畅通）
	mux.HandleFunc("GET /b", srv.handleBrowserRoot)
	mux.HandleFunc("GET /b/", srv.handleBrowserProxy)
	mux.HandleFunc("POST /b/", srv.handleBrowserProxy)
	// 根路径：/ 由面板渲染；其余"孤儿路径"（页面 JS 用面板 host 拼出的绝对 URL）
	// 兜底重定向回最近代理站点——这是 location.host 替换方案的配套落点
	mux.HandleFunc("GET /", srv.handleOrphan)
	mux.HandleFunc("POST /", srv.handleOrphan)

	initAppBase()
	cleanupLegacyReverseProxy() // 反向代理功能已移除，清掉早期版本留下的实例
	handler := formBridge(tunnelBridge(mux))
	if appBase != "" {
		// 网关入口：先剥前缀，再交给正常路由
		inner := handler
		handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			stripAppBase(r)
			inner.ServeHTTP(w, r)
		})
	}
	// fnOS 统一网关入口：监听 app.sock，网关校验登录态后转发进来
	gatewayUp := false
	if sock := strings.TrimSpace(os.Getenv("XBOX_SOCKET")); sock != "" {
		_ = os.Remove(sock)
		if ln, err := net.Listen("unix", sock); err != nil {
			log.Printf("网关 socket 监听失败 %s: %v", sock, err)
		} else {
			gatewayUp = true
			_ = os.Chmod(sock, 0o770)
			go func(ln net.Listener) {
				if err := http.Serve(ln, handler); err != nil {
					log.Printf("网关 socket 服务退出: %v", err)
				}
			}(ln)
			log.Printf("网关入口已就绪：%s -> %s", sock, appBase)
		}
	}
	addrs := listenAddrs()
	listened := 0
	for _, a := range addrs {
		ln, err := net.Listen("tcp", a)
		if err != nil {
			log.Printf("监听 %s 失败: %v", a, err)
			continue
		}
		listened++
		go func(a string, ln net.Listener) {
			if err := http.Serve(ln, handler); err != nil {
				log.Printf("HTTP 服务(%s)退出: %v", a, err)
			}
		}(a, ln)
	}
	if listened == 0 && !gatewayUp {
		log.Fatalf("服务无可用的监听入口（网关 socket 与 TCP 均失败）")
	}
	log.Printf("X-Box v%s 启动，监听 %v（回环+局域网私网，不对公网暴露；外部经 fnOS 远程访问体系）", version, addrs)
	select {} // 主协程驻留：服务在监听协程中运行
}

// listenAddrs 监听地址列表：127.0.0.1 + 首个局域网私网 IPv4（探测不到则仅回环）。
func listenAddrs() []string {
	// 生产形态只走 fnOS 统一网关（unix socket，网关校验登录态），不监听任何 TCP 端口。
	// 仅本地开发/自动化测试用 XBOX_DEV=1 临时开回环端口。
	if os.Getenv("XBOX_DEV") != "1" {
		return nil
	}
	return []string{"127.0.0.1:" + listenPort}
}

// lanIPv4 取首个局域网私网 IPv4（10.x / 172.16-31.x / 192.168.x），跳过回环/链路本地/非私网。
func lanIPv4() string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return ""
	}
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrList, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrList {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			ip4 := ip.To4()
			if ip4 == nil || ip4.IsLoopback() || ip4.IsLinkLocalUnicast() || !ip4.IsPrivate() {
				continue
			}
			return ip4.String()
		}
	}
	return ""
}

// tunnelBridge：fnOS 远程访问中转会拦截 /api/ 前缀（重定向到跨域门户，导致页面内
// XHR 被 CORS 拦、fetch/iframe 挂起），而 / 路径畅通。前端把所有 API 请求编码进
// /?__x=<base64url(JSON{m,p,b})> 隧道参数，此处还原成真实请求再交给 mux。
// （面板已无登录态，sid 字段仅为兼容旧版前端而保留忽略。）
func tunnelBridge(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/" {
			if t := r.URL.Query().Get("__x"); t != "" {
				var tp struct {
					M   string `json:"m"`
					P   string `json:"p"`
					B   string `json:"b"`
					Sid string `json:"sid"`
				}
				if raw, err := base64.URLEncoding.WithPadding(base64.NoPadding).DecodeString(t); err == nil && json.Unmarshal(raw, &tp) == nil && tp.P != "" {
					if u, perr := url.Parse(tp.P); perr == nil && strings.HasPrefix(u.Path, "/api/") {
						r2 := r.Clone(r.Context())
						r2.Method = tp.M
						if r2.Method == "" {
							r2.Method = "GET"
						}
						r2.URL.Path = u.Path
						r2.URL.RawQuery = u.RawQuery
						r2.Body = io.NopCloser(strings.NewReader(tp.B))
						r2.ContentLength = int64(len(tp.B))
						r2.Header.Set("Content-Type", "application/json")
						if tp.Sid != "" {
							r2.Header.Set("Authorization", "Bearer "+tp.Sid)
						}
						next.ServeHTTP(w, r2)
						return
					}
				}
			}
		}
		next.ServeHTTP(w, r)
	})
}

// formBridge 表单桥接：fnOS 客户端 iframe 内 fetch/XHR 子资源请求不可用，
// 前端改走「隐藏 iframe 导航通道」——POST 经表单提交，载荷放在 _body 字段。
// 此处把 application/x-www-form-urlencoded 的 _body 还原成 JSON 请求体，
// 下游 handler 零改动。
func formBridge(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// 代理请求的表单体必须原样转发，不能被中间件解析掉：ParseForm 会
		// 消费 r.Body，转发时就没有请求体了（实测光猫点"登录"提交后目标侧
		// 只收到空体 → 直接断连，代理报 EOF）
		if strings.HasPrefix(r.URL.Path, "/b/") {
			next.ServeHTTP(w, r)
			return
		}
		if r.Method == http.MethodPost && strings.HasPrefix(r.Header.Get("Content-Type"), "application/x-www-form-urlencoded") {
			_ = r.ParseForm()
			if b := r.PostFormValue("_body"); b != "" {
				r.Body = io.NopCloser(strings.NewReader(b))
				r.Header.Set("Content-Type", "application/json")
				r.ContentLength = int64(len(b))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]any{"ok": false, "error": msg})
}

func ok(w http.ResponseWriter, v any) {
	m := map[string]any{"ok": true}
	switch mm := v.(type) {
	case map[string]any:
		for k, val := range mm {
			m[k] = val
		}
	case nil:
		// 无数据：只回 {ok:true}
	default:
		// 非 map 结构以 data 承载，避免类型断言 panic（曾有接口因此直接断连）
		m["data"] = v
	}
	writeJSON(w, 200, m)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	// 网关可能以 /app/<appname> 或 /app/<appname>/index.html 形式转发
	if r.URL.Path != "/" && r.URL.Path != "/index.html" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 注入版本标识到登录卡片：便于确认客户端实际加载的构建版本（iframe 缓存/未重启排障）
	page := bytes.Replace(indexHTML, []byte("<!--XBOX_VER-->"), []byte("v"+version), 1)
	// 网关模式下前端需要知道前缀，才能拼出网关能识别的绝对路径
	page = bytes.Replace(page, []byte("__XBOX_BASE__"), []byte(appBase), 1)
	// 注入 <base>：无论网关以 /app/X-Box 还是 /app/X-Box/index.html 转发，
	// 页面里的相对请求都会落到应用根目录下
	baseTag := []byte(`<base href="` + appBase + `/">`)
	page = bytes.Replace(page, []byte("<head>"), append([]byte("<head>"), baseTag...), 1)
	_, _ = w.Write(page)
}

// fmtBytes 人类可读容量。
func fmtBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.2f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}
