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
	version = "1.3.83"
	// 面板监听策略：生产形态只监听 fnOS 统一网关 unix socket（XBOX_SOCKET），
	// 登录态由网关注入；不监听任何 TCP 端口。仅 XBOX_DEV=1 时开 127.0.0.1 回环
	// 端口供本地开发/自动化测试。请勿改为 ":8899" 通配绑定。
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
	// 开发专用：元数据抓取诊断模式（与引擎同函数同进程环境，用于对照外部 diag）
	if p := os.Getenv("XBOX_META_DIAG"); p != "" {
		btMetaDiagMain(p)
		return
	}
	dataDir := os.Getenv("XBOX_DATA")
	if dataDir == "" {
		dataDir = "."
	}
	_ = os.MkdirAll(dataDir, 0o755)

	srv := &Server{dataDir: dataDir, candidate: map[string]map[string]int64{}}

	// 初始化 DDNS、终端、测速、NAT 子系统
	initDDNS(dataDir)
	initTerminal()
	initSpeedtest(dataDir)
	initNAT(dataDir)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/overview", srv.handleOverview)
	mux.HandleFunc("GET /api/temps", handleTemps)
	mux.HandleFunc("GET /api/netrates", handleNetRates)
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
	mux.HandleFunc("POST /api/terminal/start", srv.handleTerminalStart)
	mux.HandleFunc("POST /api/terminal/write", srv.handleTerminalWrite)
	mux.HandleFunc("POST /api/terminal/stop", srv.handleTerminalStop)
	mux.HandleFunc("GET /api/nvme-sleep", handleNVMeSleepList)
	mux.HandleFunc("POST /api/nvme-sleep", handleNVMeSleepSet)
	mux.HandleFunc("GET /api/config/export", srv.handleConfigExport)
	mux.HandleFunc("POST /api/config/import", srv.handleConfigImport)
	mux.HandleFunc("GET /api/update/check", handleUpdateCheck)
	mux.HandleFunc("POST /api/update/download", handleUpdateDownload)
	mux.HandleFunc("GET /api/update/dirs", handleUpdateDirs)
	mux.HandleFunc("GET /api/update/status", handleUpdateStatus)
	mux.HandleFunc("GET /api/update/file", handleUpdateFile)
	// 通道探测端点：登录态由 fnOS 网关校验，这里只回报已认证，
	// 让前端不需要靠 404 判定链路可用
	// 网盘同步：远程挂载 → 本地目录的增量同步
	mux.HandleFunc("GET /api/sync/list", srv.handleSyncList)
	mux.HandleFunc("POST /api/sync/save", srv.handleSyncSave)
	mux.HandleFunc("POST /api/sync/delete", srv.handleSyncDelete)
	mux.HandleFunc("POST /api/sync/run", srv.handleSyncRun)
	mux.HandleFunc("POST /api/sync/pause", srv.handleSyncPause)
	mux.HandleFunc("GET /api/sync/dirs", srv.handleSyncDirs)
	// 媒体管理：按目录批量整理视频/音频文件名与元数据
	mux.HandleFunc("GET /api/media/list", srv.handleMediaList)
	mux.HandleFunc("POST /api/media/save", srv.handleMediaSave)
	mux.HandleFunc("POST /api/media/delete", srv.handleMediaDelete)
	mux.HandleFunc("POST /api/media/run", srv.handleMediaRun)
	mux.HandleFunc("POST /api/media/stop", srv.handleMediaStop)
	mux.HandleFunc("GET /api/media/status", srv.handleMediaStatus)
	mux.HandleFunc("GET /api/media/dirs", srv.handleMediaDirs)
	// 视频下载：BT/磁力聚合搜索 + 内置 BT 引擎（v1.3.39 起不再外接下载器）
	mux.HandleFunc("GET /api/dl/search", srv.handleDlSearch)          // 视频磁力聚合（并入短剧网盘源）
	mux.HandleFunc("GET /api/dl/search/last", srv.handleDlSearchLast) // 搜索结果留存（v1.3.49）
	mux.HandleFunc("GET /api/dl/config", srv.handleDlConfigGet)
	mux.HandleFunc("POST /api/dl/config", srv.handleDlConfigSave)
	mux.HandleFunc("POST /api/dl/start", srv.handleDlStart)
	mux.HandleFunc("POST /api/dl/m3u8/start", srv.handleDlM3u8Start) // 短剧 m3u8 内置下载（v1.3.58）
	mux.HandleFunc("GET /api/dl/tasks", srv.handleDlTasks)
	mux.HandleFunc("POST /api/dl/remove", srv.handleDlRemove)
	mux.HandleFunc("POST /api/dl/cancel", srv.handleDlCancel)
	// 下载任务批量控制（v1.3.43）：暂停/继续/全部停止/清除
	mux.HandleFunc("POST /api/dl/pause", srv.handleDlPause)
	mux.HandleFunc("POST /api/dl/resume", srv.handleDlResume)
	mux.HandleFunc("POST /api/dl/pauseall", srv.handleDlPauseAll)
	mux.HandleFunc("POST /api/dl/resumeall", srv.handleDlResumeAll)
	mux.HandleFunc("POST /api/dl/cancelall", srv.handleDlCancelAll)
	mux.HandleFunc("POST /api/dl/cleardone", srv.handleDlClearDone)
	mux.HandleFunc("POST /api/dl/clearall", srv.handleDlClearAll)
	// 音乐下载：网易云搜索 + 免费曲直下
	mux.HandleFunc("GET /api/dlmusic/search", srv.handleDlMusicSearch)
	mux.HandleFunc("POST /api/dlmusic/start", srv.handleDlMusicStart)
	mux.HandleFunc("GET /api/dlmusic/tasks", srv.handleDlMusicTasks)
	mux.HandleFunc("POST /api/dlmusic/cancel", srv.handleDlMusicCancel)
	mux.HandleFunc("GET /api/speedtest/status", handleSpeedtestStatus)
	mux.HandleFunc("POST /api/speedtest/start", handleSpeedtestStart)
	mux.HandleFunc("POST /api/speedtest/stop", handleSpeedtestStop)
	mux.HandleFunc("POST /api/speedtest/clear", handleSpeedtestClear)
	mux.HandleFunc("GET /api/speedtest/nodes", handleSpeedtestNodes)
	mux.HandleFunc("POST /api/speedtest/nodes/refresh", handleSpeedtestNodesRefresh)
	mux.HandleFunc("POST /api/speedtest/nodes/save", handleSpeedtestNodesSave)
	mux.HandleFunc("POST /api/speedtest/nodes/delete", handleSpeedtestNodesDelete)
	mux.HandleFunc("GET /api/speedtest/isp", handleSpeedtestISP)
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
	btInit()                 // 内置 BT 引擎（DHT + 元数据抓取 + 下载到配置目录）
	dlRestoreTasks()         // 恢复上次退出/升级前的下载任务并自动续传（v1.3.52）
	dlPersistHandleExit()    // SIGTERM/SIGINT 时先把下载任务快照落盘再退出
	startPTYIdleReaper()     // 终端 PTY 空闲会话回收
	srv.startSyncScheduler() // 网盘同步定时任务
	cleanupLegacyReverseProxy() // 反向代理功能已移除，清掉早期版本留下的实例
	handler := formBridge(tunnelBridge(panelActivity(mux)))
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
	if gatewayUp {
		log.Printf("X-Box v%s 启动，网关入口：%s", version, os.Getenv("XBOX_SOCKET"))
	} else {
		log.Printf("X-Box v%s 启动，本地调试入口：%v", version, addrs)
	}
	go warmAutostart() // 后台预热开机自启扫描（1s+），用户点进该板块时缓存已就绪=秒开
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
	// 所有 API 响应一律不缓存：隧道 URL 对同一接口恒定不变（如轮询 /api/sync/list），
	// 一旦被浏览器或网关缓存就会永远返回首个快照，表现为进度条/实时数据不刷新
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
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
	// 面板 HTML 禁止缓存：升级后浏览器/手机 WebView 会沿用旧页面（表现为
	// "电脑端已修复、手机端没有"），且旧页面里的 base 前缀可能与新网关不匹配。
	// 面板本身只有几十 KB，每次取最新最稳妥；静态资源（图标等）仍可缓存。
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Expires", "0")
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
