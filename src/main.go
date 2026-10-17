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
// 注意：面板默认监听 :8899（所有接口，含 IPv6），以便通过 DDNS 域名从外部访问；已内置登录保护
// （多账户 + 10 分钟空闲超时 + 防暴力破解）。对外开放仍意味着可达者经登录后可操作 NAS，
// 请在路由器/光猫仅放行可信来源，并登录后及时修改默认账户密码。
package main

import (
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"sync"
	"time"
)

//go:embed web/index.html
var indexHTML []byte

const (
	version    = "1.2.33"
	// 监听所有接口（含 IPv6）：允许通过 DDNS 域名（如 your-domain.example.com:8899）从外部访问面板。
	// 已配合 auth.go 的登录保护（多账户 + 10 分钟空闲超时）；请仅在路由器/光猫放行可信来源。
	listenAddr = ":8899"
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

	// 初始化 DDNS、反向代理子系统（注册联动、后台同步）
	initDDNS(dataDir)
	initProxy(dataDir)
	initAuth(dataDir)

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
	mux.HandleFunc("GET /api/proxy", srv.handleProxyList)
	mux.HandleFunc("POST /api/proxy/save", srv.handleProxySave)
	mux.HandleFunc("POST /api/proxy/del", srv.handleProxyDel)
	mux.HandleFunc("GET /api/nvme-sleep", handleNVMeSleepList)
	mux.HandleFunc("POST /api/nvme-sleep", handleNVMeSleepSet)
	mux.HandleFunc("GET /api/update/check", handleUpdateCheck)
	mux.HandleFunc("POST /api/update/download", handleUpdateDownload)
	mux.HandleFunc("GET /api/update/dirs", handleUpdateDirs)
	mux.HandleFunc("GET /api/update/file", handleUpdateFile)
	mux.HandleFunc("POST /api/login", srv.handleLogin)
	mux.HandleFunc("POST /api/logout", srv.handleLogout)
	mux.HandleFunc("GET /api/auth/status", srv.handleAuthStatus)
	mux.HandleFunc("POST /api/auth/changepass", srv.handleAuthChangePass)
	mux.HandleFunc("POST /api/auth/adduser", srv.handleAuthAddUser)
	mux.HandleFunc("POST /api/auth/deluser", srv.handleAuthDelUser)
	mux.HandleFunc("GET /api/auth/listusers", srv.handleAuthListUsers)
	mux.HandleFunc("POST /api/auth/rename", srv.handleAuthRename)
	mux.HandleFunc("GET /", srv.handleRoot)

	log.Printf("X-Box v%s 启动，监听 %s（已启用登录保护，面板对外需登录）", version, listenAddr)
	if err := http.ListenAndServe(listenAddr, authMiddleware(mux)); err != nil {
		log.Fatalf("HTTP 服务退出: %v", err)
	}
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
	for k, val := range v.(map[string]any) {
		m[k] = val
	}
	writeJSON(w, 200, m)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 注入版本标识到登录卡片：便于确认客户端实际加载的构建版本（iframe 缓存/未重启排障）
	page := bytes.Replace(indexHTML, []byte("<!--XBOX_VER-->"), []byte("v"+version), 1)
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
