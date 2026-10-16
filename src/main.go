// X-Box — 飞牛 fnOS 非 Docker 系统清理与安全体检工具
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
// 注意：面板无登录（应用户要求），监听 0.0.0.0，局域网内任何主机可访问，
// 请勿将 8899 端口暴露到公网。
package main

import (
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
	version    = "1.2.9"
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
	mux.HandleFunc("GET /", srv.handleRoot)

	log.Printf("X-Box v%s 启动，监听 %s（无登录，请勿暴露公网）", version, listenAddr)
	if err := http.ListenAndServe(listenAddr, mux); err != nil {
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
	_, _ = w.Write(indexHTML)
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
