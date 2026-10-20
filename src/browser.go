package main

// 浏览器模块：同源网页代理（路径式 + <base> 注入 + 全量 URL 重写 + JS 运行时接管）。
//
// 端点形态：/b/{scheme}/{host}/{path}?{query}，如
//   /b/http/192.168.1.1/            → http://192.168.1.1/
//   /b/https/example.com/a/b.html   → https://example.com/a/b.html
//
// 为什么必须"全量重写"而不只是注入 <base>：
//   面板通常经 fnOS 远程域名访问（https://xxx.fnos.net），用户的设备并不在
//   NAS 局域网内。页面里任何**绝对 URL**（<a href="http://192.168.1.1/x">、
//   JS 里写死的 fetch("http://192.168.1.1/cgi-bin/x")、location 跳转、302 的
//   Location 头）一旦原样落到用户浏览器，就会被拿去直连 192.168.1.1 —— 结果
//   就是"拒绝连接"/CORS 失败/页面残缺。所以必须把所有出口都改写成代理路径。
//
// 三层防线：
//   1) 服务端静态重写：HTML 属性（href/src/action/poster/srcset/formaction/
//      data-src…）、CSS url()、meta refresh、JS 里的 location.hostname|host|
//      origin|protocol|port（页面 JS 用它们拼绝对 URL，替换为真实目标值）；
//   2) 服务端注入运行时脚本：拦截 fetch / XHR.open / location.assign|replace /
//      window.open / 动态插入节点的资源属性，一律改写为代理路径；
//   3) <base> 兜底：覆盖 JS 动态发起的相对路径请求（无需改写任何业务逻辑）。
//
// 其余设计：
//   - 3xx 不跟随：重写 Location/Refresh 后交给浏览器重新发起代理请求（跨域跳转
//     http↔https、换主机都能正确工作，地址栏同步更新）；
//   - Cookie 双向透传，回写时显式 Path=/b/{scheme}/{host}/ —— 浏览器默认作用域
//     是"当前请求目录"，在无子路径时会被抬到 /b/ 造成跨站点串 cookie，必须显式；
//   - 保留响应原始 Content-Type（含 charset，GBK/GB2312 老页面不乱码）；
//   - 剥离 X-Frame-Options / CSP / CSP meta（解除内嵌限制）；
//   - 代理失败返回可读中文 HTML 错误页（区分拒绝连接/超时/DNS 失败），并提示
//     用终端在 NAS 侧自检——因为请求是 NAS 发出的，失败即"NAS 到目标不通"。
//
// 安全模型：与终端模块一致——面板本身仅 fnOS 账号体系可达（127.0.0.1 +
// 局域网私网 IPv4 + fnOS 远程访问），代理不做额外鉴权/白名单：它是用户主动
// 输入网址的通用浏览工具，内网设备（192.168.1.1 等）是合法目标。

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/tls"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 代理请求日志：环形缓冲保留最近若干条，供面板「诊断」查看（定位"页面打不开"
// 到底是代理没收到请求、目标连不上，还是页面内容本身的问题）。
type browserLogItem struct {
	Time    string `json:"time"`
	Method  string `json:"method"`
	Target  string `json:"target"`
	Status  int    `json:"status"`
	Bytes   int64  `json:"bytes"`
	CType   string `json:"ctype,omitempty"`   // 响应 Content-Type（定位"空白是否因类型/编码"）
	Snippet string `json:"snippet,omitempty"` // 文本响应开头 200 字节（定位页面实际内容）
	Err     string `json:"err,omitempty"`
}

var browserLog = struct {
	mu    sync.Mutex
	items []browserLogItem
}{}

func browserLogAdd(it browserLogItem) {
	browserLog.mu.Lock()
	browserLog.items = append(browserLog.items, it)
	if len(browserLog.items) > 60 {
		browserLog.items = browserLog.items[len(browserLog.items)-60:]
	}
	browserLog.mu.Unlock()
}

// handleBrowserLog GET /api/browser/log：最近 60 条代理请求记录。
func handleBrowserLog(w http.ResponseWriter, r *http.Request) {
	browserLog.mu.Lock()
	items := append([]browserLogItem(nil), browserLog.items...)
	browserLog.mu.Unlock()
	ok(w, map[string]any{"items": items})
}

// countWriter 统计实际写给浏览器的字节数（用于日志）。
type countWriter struct {
	w http.ResponseWriter
	n int64
}

func (c *countWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	c.n += int64(n)
	return n, err
}

// 响应源码快照：按目标 URL 键控保存最近的原始响应（重写前），供面板「查看源码」
// 定位"代理返回 200 但页面空白"——直接看目标到底返回了什么。
type browserSnap struct {
	Target string `json:"target"`
	CType  string `json:"ctype"`
	Time   string `json:"time"`
	Body   string `json:"body"`
}

var (
	browserSnapMu    sync.Mutex
	browserSnapStore = map[string]browserSnap{}
)

func browserSnapSave(target, ctype string, data []byte) {
	const maxSnaps = 16
	body := string(data)
	if len(body) > 64<<10 {
		body = body[:64<<10]
	}
	browserSnapMu.Lock()
	if len(browserSnapStore) >= maxSnaps { // 简单淘汰：满了全清（诊断用途足够）
		browserSnapStore = map[string]browserSnap{}
	}
	browserSnapStore[target] = browserSnap{
		Target: target,
		CType:  ctype,
		Time:   time.Now().Format("15:04:05"),
		Body:   body,
	}
	browserSnapMu.Unlock()
}

// handleBrowserSnap GET /api/browser/snap?target=<url>：查看某目标的原始响应源码。
func handleBrowserSnap(w http.ResponseWriter, r *http.Request) {
	t := r.URL.Query().Get("target")
	if t == "" {
		fail(w, 400, "缺少 target 参数")
		return
	}
	browserSnapMu.Lock()
	s, has := browserSnapStore[t]
	browserSnapMu.Unlock()
	if !has {
		fail(w, 404, "无该目标的响应记录（先访问一次）")
		return
	}
	ok(w, map[string]any{"target": s.Target, "ctype": s.CType, "time": s.Time, "body": s.Body})
}

const (
	browserMaxText   = 8 << 20  // 需文本重写（HTML/CSS/JS）的读取上限 8MB
	browserMaxStream = 64 << 20 // 其他内容流式透传上限 64MB
	browserMaxPost   = 32 << 20 // POST 请求体上限（读入内存以便带 Content-Length）
	browserTimeout   = 30 * time.Second
	browserGzipMin   = 1024 // 文本类响应超过该体积才压缩回传
)

// browserTransport 包级共享：连接复用（keep-alive）对代理性能是决定性的——
// 页面往往有几十个资源，每个都新建 TCP/TLS 连接会让点击链接明显变慢。
// 内网设备普遍自签证书，故跳过校验（与浏览器"继续访问"等价）。
var browserTLSConf = &tls.Config{
	InsecureSkipVerify: true,
	// 内网老设备（光猫/老路由器）可能只支持 TLS1.0/1.1，Go 默认最低 1.2
	// 会直接握手失败（表现为 502）。代理在可信内网，放宽到 1.0 换兼容性。
	MinVersion: tls.VersionTLS10,
}

var browserTransport = &http.Transport{
	TLSClientConfig:     browserTLSConf,
	Proxy:               nil,
	ForceAttemptHTTP2:   true,
	MaxIdleConns:        64,
	MaxIdleConnsPerHost: 8,
	MaxConnsPerHost:     16, // 单目标并发连接上限，防止把目标或本机连接表打满
	IdleConnTimeout:     90 * time.Second,
}

// browserTransport4 强制 IPv4 的备用传输：部分 NAS 分到公网 IPv6 但实际没有
// IPv6 出口，DNS 返回 AAAA 时连接会一直超时（表现为「手机端/外网访问 502」）。
// 主传输失败后用这条再试一次，能覆盖绝大多数此类环境。
var browserTransport4 = &http.Transport{
	TLSClientConfig:     browserTLSConf,
	Proxy:               nil,
	ForceAttemptHTTP2:   true,
	MaxIdleConns:        16,
	MaxIdleConnsPerHost: 8,
	MaxConnsPerHost:     16,
	IdleConnTimeout:     90 * time.Second,
	DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
		d := &net.Dialer{Timeout: 12 * time.Second, KeepAlive: 30 * time.Second}
		return d.DialContext(ctx, "tcp4", addr)
	},
}

// browserJars 按目标站点共享会话：host → cookiejar。
// 代理必须自己记住目标站的会话，不能完全依赖浏览器回传 Cookie——
// 管理类页面（光猫/路由器）丢一次 Cookie 就整体 404、菜单点不动。
var browserJars sync.Map

// browserJarFor 取（或创建）某站点的共享会话 jar。
func browserJarFor(host string) http.CookieJar {
	if v, ok := browserJars.Load(host); ok {
		return v.(http.CookieJar)
	}
	j, _ := cookiejar.New(nil)
	actual, _ := browserJars.LoadOrStore(host, j)
	return actual.(http.CookieJar)
}

// isMobileUA 判断是否为移动端浏览器标识。
func isMobileUA(ua string) bool {
	l := strings.ToLower(ua)
	for _, k := range []string{"mobile", "android", "iphone", "ipad", "ipod", "micromessenger", "harmonyos", "phone"} {
		if strings.Contains(l, k) {
			return true
		}
	}
	return false
}

// handleBrowserRoot GET /b（无子路径）：提示用法。
func (s *Server) handleBrowserRoot(w http.ResponseWriter, r *http.Request) {
	fail(w, 400, "用法：/b/http/<主机>/<路径> 或 /b/https/<主机>/<路径>")
}

// handleBrowserProxy GET/POST /b/{scheme}/{host}/{path}?{query}：同源网页代理。
func (s *Server) handleBrowserProxy(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.RequestURI() // 保留原始编码（含查询串）

	// 防护 1：地址长度上限。改写若陷入自激，URL 会一轮比一轮长，先挡住。
	if len(raw) > 4096 {
		fail(w, 400, "代理地址过长（疑似循环），已阻止")
		return
	}
	// 防护 2：同一 URL 里出现多段 /b/vNNN/ 说明发生了代理嵌套，直接拒绝
	if browserProxyDepth(raw) > 1 {
		// 先尝试修复：页面 JS 把"面板地址"与相对路径二次拼接，会产出
		// /b/vNNN/https/host/A/ + b/vNNN/https/host + B 这种重复前缀地址
		// （实测百度地图移动版：/mobile/webapp/ + b/vNNN/https/host + index/index）。
		// 折叠重复前缀后 307 到正确地址，避免整页报"嵌套循环"。
		if fixed, ok := browserCollapseDupPrefix(raw); ok {
			http.Redirect(w, r, fixed, http.StatusTemporaryRedirect)
			return
		}
		fail(w, 400, "检测到代理嵌套循环，已阻止")
		return
	}
	// 防护 3：普通请求的并发上限。页面若自激，浏览器会瞬间发大量请求，
	// 限流保证 NAS 不会被拖垮（表现为整机卡死、只能重启）。
	// WebSocket 是长连接，不占用普通请求的并发额度，否则几十条 WS 会把闸门占满
	if !browserIsWSUpgrade(r) {
		if !browserAcquireSlot() {
			fail(w, 429, "代理并发请求过多，请稍后再试")
			return
		}
		defer browserReleaseSlot()
	}

	// 防护 4：同一路径的页面导航限流。前端路由互相跳转（登录页↔首页）时，
	// 浏览器会以极高频率重载同一文档；只限 HTML 导航，不影响静态资源。
	// key 用路径部分：循环跳转时查询串会变化，用完整 URL 会漏判
	navKey := raw
	if qi := strings.IndexByte(navKey, '?'); qi > 0 {
		navKey = navKey[:qi]
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/html") &&
		!browserNavAllowed(navKey) {
		fail(w, 429, "该页面在代理下反复跳转，已暂停加载；请改用站点自身入口")
		return
	}

	rest := browserStripVerSeg(strings.TrimPrefix(raw, "/b/"))
	parts := strings.SplitN(rest, "/", 3)
	scheme := parts[0]
	if scheme != "http" && scheme != "https" {
		fail(w, 400, "仅支持 http/https 代理（/b/http/<主机>/... 或 /b/https/<主机>/...）")
		return
	}
	if len(parts) < 2 || parts[1] == "" {
		fail(w, 400, "缺少目标主机")
		return
	}
	host := parts[1]
	// 禁止代理面板自身：/b/https/面板域名/... 会打回面板 → 面板 307 回 /b/...
	// —— 实测每秒几十条 307 的自环（worker_wasm_ncq5j1.js），并污染 lastSite，
	// 连带把别的站点的孤儿路径也带偏。
	if browserIsPanelHost(host, r) {
		fail(w, 400, "不能通过代理访问面板自身（会造成自环）")
		return
	}
	// 本机的 80/443 就是 fnOS 网关入口：代理它 → 网关 → 又回到本应用，必然成环
	if browserIsLocalGateway(host) {
		fail(w, 400, "不能代理本机的网关端口（会形成自环）")
		return
	}
	sub := "/"
	if len(parts) == 3 && parts[2] != "" {
		sub += parts[2]
	}
	// 兜底纠正：目标路径若以面板前缀开头（appBase），说明某个相对路径被误当成
	// 站点路径拼接（实测会产出 目标域名/app/X-Box/ 这种畸形地址 → 整页 404）。
	// 剥掉前缀后 307 到正确地址。
	if appBase != "" && strings.HasPrefix(sub, appBase+"/") {
		fixed := appBase + "/b/" + strings.TrimPrefix(raw, "/b/")
		if i := strings.Index(fixed, appBase+"/b/"); i >= 0 {
			if j := strings.Index(fixed[i+len(appBase)+3:], appBase+"/"); j >= 0 {
				cut := i + len(appBase) + 3 + j
				fixed = fixed[:cut] + strings.TrimPrefix(fixed[cut:], appBase)
				if r.URL.RawQuery != "" {
					fixed += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, fixed, http.StatusTemporaryRedirect)
				return
			}
		}
	}
	target := scheme + "://" + host + sub
	tu, terr := url.Parse(target)
	if terr != nil || tu.Host == "" {
		fail(w, 400, "目标地址非法")
		return
	}
	s.proxyCore(w, r, target, tu, false)
}

// proxyCore 代理主体：取回目标响应、改写并回写。
// direct=true 为直通模式（独立端口、路径不加前缀），供 pathname 路由的 SPA 使用。
func (s *Server) proxyCore(w http.ResponseWriter, r *http.Request, target string, tu *url.URL, direct bool) {
	scheme, host := tu.Scheme, tu.Host
	// 请求日志（诊断用）：无论成功失败都记录，供面板「诊断」面板查看
	rec := &browserLogItem{Time: time.Now().Format("15:04:05"), Method: r.Method, Target: target, Status: 502}
	cw := &countWriter{w: w}
	defer func() { rec.Bytes = cw.n; browserLogAdd(*rec) }()

	// 面板自身的 scheme/host：JS 里的 location.host 系列替换为它，让页面 JS 拼出的
	// 绝对 URL 落到面板根下，再由 handleOrphan 兜底回代理（fnOS 中转/反代会改写
	// Host，优先取 X-Forwarded-Proto 还原真实协议）
	panelScheme := "http"
	if r.TLS != nil {
		panelScheme = "https"
	}
	if fp := r.Header.Get("X-Forwarded-Proto"); fp != "" {
		panelScheme = fp
	}
	panelHost := r.Host
	if panelHost == "" {
		panelHost = tu.Host
	}

	// POST 请求体必须先读入内存再转发：直接用 r.Body（io.Reader）时 Go 无法
	// 得知长度，会改用 chunked 传输编码；路由器/光猫这类嵌入式 httpd 普遍不支持
	// chunked，收到就断连 → 代理侧报 EOF（实测点"登录"提交表单即 502）。
	// bytes.Reader 让 net/http 自动带上 Content-Length，与浏览器直连一致。
	var body io.Reader
	if r.Method == http.MethodPost && r.Body != nil {
		data, rerr := io.ReadAll(io.LimitReader(r.Body, browserMaxPost))
		if rerr != nil {
			rec.Err = "读取请求体失败：" + rerr.Error()
			fail(w, 400, "读取请求体失败")
			return
		}
		if len(data) > 0 {
			body = bytes.NewReader(data)
		}
	}
	// WebSocket 升级：常驻主通道（管理面板/桌面类应用），不转发会一直提示"连接已断开"
	if browserIsWSUpgrade(r) {
		s.browserProxyWS(w, r, target, tu)
		return
	}
	// 按站点共享 cookiejar：会话必须在服务端留存一份。
	// 只用「每请求独立 jar + 依赖浏览器回传」时，只要有一次请求浏览器没带上
	// Cookie（路径不匹配、框架页 302、第三方站点 Set-Cookie 被浏览器丢弃等），
	// 设备立刻判定未登录：所有 .gch 返回 404、点菜单不刷新、登录提示密码错误。
	// 共享 jar 让代理自己维持会话，浏览器 Cookie 仍会合并进来（多端登录场景）。
	jar := browserJarFor(host)
	if ck := r.Header.Get("Cookie"); ck != "" {
		var cs []*http.Cookie
		for _, part := range strings.Split(ck, ";") {
			part = strings.TrimSpace(part)
			if k, v, ok := strings.Cut(part, "="); ok {
				cs = append(cs, &http.Cookie{Name: strings.TrimSpace(k), Value: strings.TrimSpace(v)})
			}
		}
		if len(cs) > 0 {
			jar.SetCookies(tu, cs)
		}
	}
	client := &http.Client{
		Timeout: browserTimeout,
		Jar:     jar,
		// 3xx 不跟随：重写 Location 后交给浏览器重新发起代理请求，
		// 跨域/换协议/换主机跳转都能正确工作（跟随会导致最终 URL 与地址栏不一致）。
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
		// 必须用包级共享 Transport：v1.2.46 曾在此处每请求 new 一个 Transport，
		// 导致每次代理请求都重新 TCP/TLS 握手（页面每个资源一次），点击链接明显变慢。
		Transport: browserTransport,
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		rec.Err = "构造请求失败：" + err.Error()
		browserFail(w, target, err)
		return
	}
	// UA 策略：移动端一律改用桌面标识。
	// 原因：移动版站点在代理下层出不穷——按 UA 302 到 m.* 域名、接口路径不同、
	// 资源域走 http、页面脚本用 location.host 拼地址等，都是 502/白屏/点不动的来源；
	// 桌面版页面结构稳定，与电脑端表现一致。设备管理页（光猫等）用桌面 UA 也会返回
	// 完整界面而不是简化版。
	// 例外：XBOX_MOBILE_UA=keep 可保留原 UA（排查移动版专用问题时用）。
	const desktopUA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36"
	ua := r.Header.Get("User-Agent")
	if ua == "" || (isMobileUA(ua) && os.Getenv("XBOX_MOBILE_UA") != "keep") {
		ua = desktopUA
	}
	req.Header.Set("User-Agent", ua)
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	if ct := r.Header.Get("Content-Type"); ct != "" && r.Method == http.MethodPost {
		req.Header.Set("Content-Type", ct)
	}
	// Cookie 由 client 的共享 jar 统一注入（浏览器的 Cookie 已合并进 jar），
	// 这里不能再手工设 Cookie 头，否则同名 Cookie 会重复发送、部分设备直接判为无效会话。
	// Basic/Digest 认证透传（路由器管理页常见；浏览器弹框输入后由 iframe 请求带上）
	if au := r.Header.Get("Authorization"); au != "" {
		req.Header.Set("Authorization", au)
	}
	// Referer：浏览器发的是面板 /b/... 路径，还原成目标 URL 再转发。
	// 图片/地图瓦片常有防盗链校验，Referer 不对直接 403。
	if rf := r.Header.Get("Referer"); rf != "" {
		req.Header.Set("Referer", browserUnProxy(rf))
	}
	// Accept 头转发：浏览器对 <img>/<script>/<link> 发的是 image/*、*/* 等，
	// 用它替代固定值才能正确做内容协商（WebP/AVIF 等图片格式依赖此头）
	if ac := r.Header.Get("Accept"); ac != "" {
		req.Header.Set("Accept", ac)
	}
	// 断点续传/媒体拖动：Range 请求头透传（视频、大图渐进加载依赖）
	for _, h := range []string{"Range", "If-Range"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	// 条件请求头透传：命中目标站 304 时浏览器直接用自己的缓存（配合上面的缓存头）
	for _, h := range []string{"If-None-Match", "If-Modified-Since"} {
		if v := r.Header.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	resp, err := client.Do(req)
	// 瞬时失败重试：第一次原样重试（移动网络/DNS 抖动），仍失败再强制 IPv4
	// （NAS 分到 IPv6 但无出口时，AAAA 首连会一直超时 → 用户看到 502）。
	if err != nil && (r.Method == http.MethodGet || r.Method == http.MethodHead) {
		if req2, e2 := http.NewRequestWithContext(r.Context(), r.Method, target, nil); e2 == nil {
			req2.Header = req.Header.Clone()
			if r2, err2 := client.Do(req2); err2 == nil {
				resp, err = r2, nil
			} else {
				// 第二跳：IPv4 强制
				c4 := &http.Client{
					Timeout:       browserTimeout,
					Jar:           jar,
					CheckRedirect: client.CheckRedirect,
					Transport:     browserTransport4,
				}
				if req3, e3 := http.NewRequestWithContext(r.Context(), r.Method, target, nil); e3 == nil {
					req3.Header = req.Header.Clone()
					if r3, err3 := c4.Do(req3); err3 == nil {
						resp, err = r3, nil
					}
				}
			}
		}
	}
	if err != nil {
		rec.Err = browserErrText(err)
		browserFail(w, target, err)
		return
	}
	defer resp.Body.Close()

	// 剥离阻碍 iframe 内嵌的头（响应由面板同源提供，原站策略不适用于本响应）。
	// Strict-Transport-Security 必须剥：https 站点发它会让浏览器把面板域名也
	// 强制升级 https 并记住（跨站点串味）；Cross-Origin-* 系列会让代理页的
	// 子资源被浏览器按 CORP/COEP 规则拦掉（表现为样式/图片丢失）。
	for _, h := range []string{"X-Frame-Options", "Content-Security-Policy", "Content-Security-Policy-Report-Only",
		"Strict-Transport-Security", "Cross-Origin-Opener-Policy", "Cross-Origin-Embedder-Policy", "Cross-Origin-Resource-Policy"} {
		resp.Header.Del(h)
	}
	// 跳转类响应头一律改写成代理路径后写给浏览器（否则浏览器直连目标 → 拒绝连接）
	for _, h := range []string{"Location", "Content-Location", "Refresh"} {
		v := resp.Header.Get(h)
		if v == "" {
			continue
		}
		if h == "Refresh" {
			w.Header().Set(h, browserRewriteRefreshValue(v, tu, direct))
			continue
		}
		w.Header().Set(h, browserProxyURL(v, tu, direct))
	}
	// 出向 Cookie 回写：显式 Path 限本站点代理子树（浏览器默认"当前请求目录"
	// 在无子路径时会抬到 /b/，导致跨站点串 cookie）
	cookiePathV := "; Path=" + browserPathPrefix() + scheme + "/" + host + "/" // 带版本段（base 注入的页面）
	// 无版本段（入口地址）：必须带 appBase 前缀。网关模式下面板挂在 /app/X-Box 下，
	// 写死 /b/... 的 Path 与真实请求路径 /app/X-Box/b/... 不匹配，浏览器不会回传
	// Cookie → 设备判定未登录 → 子页面 404、点击菜单无反应。
	cookiePathN := "; Path=" + appBase + "/b/" + scheme + "/" + host + "/"
	if direct {
		cookiePathV = "; Path=/"
		cookiePathN = ""
	}
	if os.Getenv("XBOX_DEBUG_COOKIE") == "1" {
		log.Printf("[cookie] %s respSet=%d jarHit=%d", target, len(resp.Cookies()), len(jar.Cookies(tu)))
	}
	// 浏览器已带有的同值 Cookie 不再重复下发：站点一次下发十几个 Cookie 时，
	// 「每个写两份（带/不带版本段）」会让响应头成倍膨胀，越过网关（nginx）的
	// 响应头缓冲上限后上游直接 502（表现为整页打不开，刷新无效）。
	have := map[string]string{}
	for _, part := range strings.Split(r.Header.Get("Cookie"), ";") {
		part = strings.TrimSpace(part)
		if k, v, ok := strings.Cut(part, "="); ok {
			have[strings.TrimSpace(k)] = strings.TrimSpace(v)
		}
	}
	// 只有入口地址（路径不含版本段）才需要补一份"无版本段 Path"
	needNPath := cookiePathN != "" && cookiePathN != cookiePathV &&
		!browserReVerSeg.MatchString(r.URL.Path)
	// 只回写**本次响应**新产生的 Cookie（resp.Cookies()），不回写 jar 里的历史累积。
	// 会话本身已由服务端共享 jar 维持，浏览器侧只需要同步最新值；
	// 若把 jar 全量回写，站点 cookie 越积越多，响应头会撑爆网关缓冲（整页 502）。
	const cookieBudget = 3072
	used := 0
	for _, c := range resp.Cookies() {
		if v, ok := have[c.Name]; ok && v == c.Value && !needNPath {
			continue
		}
		sz := len(c.Name) + len(c.Value) + len(cookiePathV)
		if used+sz > cookieBudget {
			continue
		}
		used += sz
		w.Header().Add("Set-Cookie", c.Name+"="+c.Value+cookiePathV)
		// 入口页面地址不带版本段，Cookie 若只写带版本段的 Path，登录等表单提交
		// （发往无版本段路径）时浏览器不会携带——会话丢失导致光猫提示"密码错误"。
		if needNPath && used+sz <= cookieBudget {
			used += sz
			w.Header().Add("Set-Cookie", c.Name+"="+c.Value+cookiePathN)
		}
	}
	// 媒体/下载相关：断点续传与文件名（视频拖动、大文件下载依赖）
	for _, h := range []string{"Accept-Ranges", "Content-Range", "Content-Disposition"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	// 缓存头双向透传：让浏览器缓存代理后的资源并支持条件请求（304），
	// 二次点击同一站点时直接命中本地缓存，不再回源到 NAS，显著提速。
	for _, h := range []string{"Cache-Control", "ETag", "Last-Modified", "Expires"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	// 认证挑战透传：让浏览器能弹出账号密码框（401 场景）
	for _, h := range []string{"WWW-Authenticate", "Proxy-Authenticate"} {
		if v := resp.Header.Get(h); v != "" {
			w.Header().Set(h, v)
		}
	}
	rec.Status = resp.StatusCode

	// 304/204 无响应体：必须原样返回，绝不重写也不写 body。
	// v1.2.53 修复：此前对 304 也走了 HTML 分支，把注入脚本写进了"无 body"的
	// 304 响应，浏览器收到带 body 的 304 会反复重新加载（实测 192.168.1.50
	// 每秒几十次 304 循环，页面永远刷不出来）。
	if resp.StatusCode == http.StatusNotModified || resp.StatusCode == http.StatusNoContent {
		browserWriteStatus(w, resp.StatusCode)
		return
	}

	ct := resp.Header.Get("Content-Type")
	low := strings.ToLower(ct)
	rec.CType = ct
	switch {
	case strings.Contains(low, "text/html") || strings.Contains(low, "application/xhtml"):
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, browserMaxText))
		if rerr != nil {
			rec.Err = "读取响应失败：" + rerr.Error()
			browserFail(w, target, rerr)
			return
		}
		w.Header().Set("Content-Type", browserKeepCharset(ct)) // 保留原 charset（GBK 等不乱码）
		rec.Snippet = browserSnippet(data)
		browserSnapSave(target, ct, data) // 保存原始响应（重写前），供「查看源码」诊断
		lastSite.Store(scheme + "://" + host)
		browserWriteText(cw, w, r, browserRewriteHTML(string(data), tu, panelScheme, panelHost, direct), resp.StatusCode)
	case strings.Contains(low, "text/css"):
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, browserMaxText))
		if rerr != nil {
			rec.Err = "读取响应失败：" + rerr.Error()
			browserFail(w, target, rerr)
			return
		}
		w.Header().Set("Content-Type", browserKeepCharset(ct))
		rec.Snippet = browserSnippet(data)
		browserSnapSave(target, ct, data)
		browserWriteText(cw, w, r, []byte(browserRewriteCSS(string(data), tu, direct)), resp.StatusCode)
	case strings.Contains(low, "javascript") || strings.Contains(low, "ecmascript"):
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, browserMaxText))
		if rerr != nil {
			rec.Err = "读取响应失败：" + rerr.Error()
			browserFail(w, target, rerr)
			return
		}
		w.Header().Set("Content-Type", browserKeepCharset(ct))
		rec.Snippet = browserSnippet(data)
		browserSnapSave(target, ct, data)
		browserWriteText(cw, w, r, []byte(browserRewriteJS(string(data), panelScheme, panelHost)), resp.StatusCode)
	default:
		// 其余类型：流式透传（图片/字体/媒体/二进制）
		if ct != "" {
			w.Header().Set("Content-Type", ct)
		}
		if resp.ContentLength >= 0 && resp.ContentLength <= browserMaxStream {
			w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
		}
		browserWriteStatus(w, resp.StatusCode)
		if resp.StatusCode != 204 && resp.StatusCode != 304 {
			_, _ = io.Copy(cw, io.LimitReader(resp.Body, browserMaxStream))
		}
	}
}

// browserWriteStatus 转发原始状态码（200 无需显式写）。
func browserWriteStatus(w http.ResponseWriter, code int) {
	if code != http.StatusOK {
		w.WriteHeader(code)
	}
}

// browserKeepCharset 原样保留目标的 Content-Type。
// 绝不能"没有 charset 就改成 text/html"：那会把 application/javascript 变成
// text/html，而 <script type="module"> 对 MIME 严格校验（HTML 规范要求
// JavaScript MIME），整个模块图直接被拒 —— 实测 fnOS 前端（Vite/ESM）全白，
// 报 "Expected a JavaScript-or-Wasm module script but ... text/html"。
// charset 缺失也不补：HTTP 头优先级高于 <meta>，擅自补 utf-8 会让 GBK 页面乱码。
func browserKeepCharset(ct string) string {
	if ct == "" {
		return "text/html"
	}
	return ct
}

// browserWriteText 文本类响应回写：客户端支持 gzip 且压缩确有收益时压缩回传。
// 远程访问经 fnOS 域名中转、带宽常是瓶颈，HTML/CSS/JS 压缩后通常只剩原文的
// 1/4~1/3，点击链接的等待时间明显缩短。
func browserWriteText(cw *countWriter, w http.ResponseWriter, r *http.Request, data []byte, status int) {
	if len(data) >= browserGzipMin && strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip") {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write(data); err == nil && gz.Close() == nil && buf.Len() < len(data) {
			// 编码头必须在写状态码之前设置：非 200 响应（如未登录的 403）若先
			// WriteHeader，之后再设置的头会被丢弃，浏览器拿到 gzip 字节却不知道
			// 要解压，整页显示为乱码。
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Vary", "Accept-Encoding")
			browserWriteStatus(w, status)
			_, _ = cw.Write(buf.Bytes())
			return
		}
	}
	browserWriteStatus(w, status)
	_, _ = cw.Write(data)
}

// browserSnippet 取文本响应开头 200 字节做日志摘要（控制字符转空格，便于诊断
// "代理返回 200 但页面空白"——直接看页面实际返回了什么）。
func browserSnippet(data []byte) string {
	const n = 200
	if len(data) > n {
		data = data[:n]
	}
	s := strings.Map(func(r rune) rune {
		if r < 32 && r != '\n' && r != '\t' {
			return ' '
		}
		return r
	}, string(data))
	return strings.TrimSpace(s)
}

var (
	browserReBase       = regexp.MustCompile(`(?i)<base\b[^>]*>`)
	browserReHead       = regexp.MustCompile(`(?i)<head[^>]*>`)
	browserReHTMLTag    = regexp.MustCompile(`(?i)<html[^>]*>`)
	browserReCSPMeta    = regexp.MustCompile(`(?i)<meta[^>]+http-equiv\s*=\s*("content-security-policy"|'content-security-policy'|content-security-policy)[^>]*>`)
	browserReAttr       = regexp.MustCompile(`(?i)\s(href|src|action|poster|srcset|formaction|data-src|data-href|data-url|data-original)\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	browserReSrcsetPart = regexp.MustCompile(`^(\s*)([^\s,]+)(.*)$`)
	browserReCSSURL     = regexp.MustCompile(`(?i)url\(\s*('[^']*'|"[^"]*"|[^)]*?)\s*\)`)
	browserReRefresh    = regexp.MustCompile(`(?i)<meta[^>]*http-equiv\s*=\s*["']?refresh["']?[^>]*>`)
	browserReRefreshU   = regexp.MustCompile(`(?i)(url\s*=\s*)(['"]?)([^'">;]+)`)
	// JS 改写已改为词法级（jslex.go）：先用 token 切分再按邻接关系替换，
	// 不再用正则去猜字符串/注释边界。这里只保留定位 <script> 块的正则。
	browserReScriptBlk = regexp.MustCompile(`(?is)(<script\b[^>]*>)(.*?)(</script>)`)
)

// browserVerSeg 代理路径的版本段（/b/v1254/{scheme}/{host}/...）。
// 为什么必须有：改写结果随版本变化，而目标站的静态资源常带
// Cache-Control: max-age=31536000, immutable —— 路径不变的话，升级后浏览器
// 仍然用旧版本改写出来的那份（v1.2.54 修复了百度大包的语法错误，用户却还在
// 报同样的行号，就是命中了旧缓存）。把版本写进路径 = 每次升级自动弃用旧缓存。
func browserVerSeg() string { return "v" + strings.ReplaceAll(version, ".", "") }

// browserPathPrefix 代理路径前缀，如 "/b/v1254/"。
func browserPathPrefix() string { return appBase + "/b/" + browserVerSeg() + "/" }

// browserStripVerSeg 去掉路径里的版本段（"v1254/https/host/x" → "https/host/x"），
// 供解析入口统一使用；不带版本段的旧路径原样返回（兼容面板里写死的 /b/...）。
func browserStripVerSeg(rest string) string {
	i := strings.IndexByte(rest, '/')
	if i > 1 && rest[0] == 'v' {
		digits := true
		for k := 1; k < i; k++ {
			if rest[k] < '0' || rest[k] > '9' {
				digits = false
				break
			}
		}
		if digits {
			return rest[i+1:]
		}
	}
	return rest
}

// browserUnProxy 把面板形态的 URL（含 /b/{ver}/{scheme}/{host}/...）还原为目标 URL，
// 用于 Referer 转发（目标站防盗链校验需要真实来源）。非代理 URL 原样返回。
func browserUnProxy(s string) string {
	i := strings.Index(s, "/b/")
	if i < 0 {
		return s
	}
	rest := browserStripVerSeg(s[i+3:])
	j := strings.Index(rest, "/")
	if j < 0 {
		return s
	}
	scheme := rest[:j]
	tail := rest[j:]
	if scheme != "http" && scheme != "https" {
		return s
	}
	return scheme + "://" + strings.TrimPrefix(tail, "/")
}

// browserIsPanelHost 目标主机是否就是面板自己（含反代/远程访问 Host）。
// 用于拦住"代理面板自身"的自环：面板对未知路径会 307 回 /b/...，套一层代理
// 就变成无限重定向（实测百度地图页每秒钟几十条 307）。
func browserIsPanelHost(host string, r *http.Request) bool {
	norm := func(s string) string {
		s = strings.ToLower(strings.TrimSpace(s))
		if i := strings.LastIndex(s, ":"); i > 0 {
			port := s[i+1:]
			if port == "80" || port == "443" {
				s = s[:i]
			}
		}
		return s
	}
	h := norm(host)
	for _, cand := range []string{r.Host, r.Header.Get("X-Forwarded-Host"), r.Header.Get("Host")} {
		if cand == "" {
			continue
		}
		c := norm(strings.Split(cand, ",")[0])
		if c != "" && h == c {
			return true
		}
	}
	return false
}

// browserURLPath 代理路径形态的 URL（/b/{scheme}/{host}{path}?{query}）。
func browserURLPath(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" || !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return browserPathPrefix() + u.Scheme + "/" + u.Host + p
}

// browserProxyURL 把绝对/协议相对/根相对 URL 转成代理路径；非 http(s)、
// 相对路径原样返回。
// 根相对（"/wolfman/x.css"）必须在这里就改写：它不经过 <base>（以 "/" 开头的
// URL 一律从站点根解析），落到面板根下只能靠孤儿重定向兜底，而孤儿重定向依赖
// "最近访问的站点"，多站点/自代理时会把路径指错（百度地图的 CSS 就是这么丢的）。
func browserProxyURL(s string, cur *url.URL, direct bool) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return s
	}
	low := strings.ToLower(s)
	for _, p := range []string{"javascript:", "mailto:", "tel:", "data:", "blob:", "about:", "vbscript:", "#"} {
		if strings.HasPrefix(low, p) {
			return s
		}
	}
	// 幂等：已经是代理路径（本次改写过的结果）绝不能再套一层。
	// v1.2.55 的根相对改写引入过死循环：属性值被改成 /b/v1255/...（根相对形式），
	// 运行时的原型 setter / MutationObserver 再拿它调一次改写 → /b/…/b/… 无限增长
	// → 每轮属性都变、观察器再触发 → 浏览器卡死。
	if strings.HasPrefix(s, browserPathPrefix()) || strings.HasPrefix(s, "/b/") ||
		(appBase != "" && strings.HasPrefix(s, appBase+"/b/")) {
		return s
	}
	// 幂等补：目标站页面若把"当前地址"存起来再拼绝对 URL，响应里会出现
	// "面板域名绝对 URL"形态（http(s)://面板host/b/vNNN/https/目标/...）。
	// 它不以 /b/ 开头，上面拦不住；再包一层就 depth=2 → 400。这里按代理
	// 签名识别，命中即原样返回（绝对 URL 由浏览器按面板路由解析，天然带
	// appBase 前缀，不需要在这里截取）。
	if i := strings.Index(s, "/b/"); i > 0 {
		if strings.HasPrefix(s[:i], "http://") || strings.HasPrefix(s[:i], "https://") {
			if browserReAbsProxy.MatchString(s) {
				return s
			}
		}
	}
	// 直通模式（独立端口、路径不加前缀）：同站绝对 URL 压成根相对，
	// 根相对原样，跨站绝对 URL 保持原样（由浏览器直连）。
	if direct {
		if strings.HasPrefix(s, "//") {
			s = cur.Scheme + ":" + s
		}
		if du, derr := url.Parse(s); derr == nil && du.Host != "" {
			if !strings.EqualFold(du.Host, cur.Host) {
				return s
			}
			dp := du.EscapedPath()
			if dp == "" {
				dp = "/"
			}
			if du.RawQuery != "" {
				dp += "?" + du.RawQuery
			}
			return dp
		}
		return s
	}
	if strings.HasPrefix(s, "//") { // 协议相对
		s = cur.Scheme + ":" + s
	} else if strings.HasPrefix(s, "/") && cur.Host != "" { // 根相对
		return browserPathPrefix() + cur.Scheme + "/" + cur.Host + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return s
	}
	out := browserURLPath(u)
	// 登录类跳转常把"当前地址"编码进 redirect_uri 等查询参数。若当前地址已经是
	// 代理路径，目标站会把它当作站点内路径再回跳一次，于是每跳一次多套一层
	// （实测某管理面板的 /login?redirect_uri=... 无限嵌套）。
	// 这里把查询参数里的自身代理前缀剥掉，回跳参数恢复成站点内路径。
	pres := []string{
		browserPathPrefix() + cur.Scheme + "/" + cur.Host,
		"/b/" + cur.Scheme + "/" + cur.Host,
	}
	out = browserStripPrefixInQuery(out, pres)
	// 硬兜底：路径本身已有一段代理标记，查询串里再出现一段就是嵌套。
	// 此时宁可丢弃查询串（回跳参数失效），也不能让 URL 无限增长。
	if browserProxyDepth(out) > 1 {
		if i := strings.IndexByte(out, '?'); i > 0 {
			out = out[:i]
		}
	}
	return out
}

// browserStripPrefixInQuery 只处理 ? 之后的查询串，避免误伤路径本身。
func browserStripPrefixInQuery(u string, pres []string) string {
	i := strings.IndexByte(u, '?')
	if i < 0 {
		return u
	}
	head, q := u[:i], u[i:]
	for _, pre := range pres {
		if pre == "" {
			continue
		}
		q = strings.ReplaceAll(q, url.QueryEscape(pre+"/"), "%2F")
		q = strings.ReplaceAll(q, url.QueryEscape(pre), "")
		q = strings.ReplaceAll(q, pre+"/", "/")
		q = strings.ReplaceAll(q, pre, "")
	}
	return head + q
}

// browserRewriteAttr 整体匹配形式：" name=value"（含引号/无引号）。
func browserRewriteAttr(m string, tu *url.URL, direct bool) string {
	eq := strings.Index(m, "=")
	if eq < 0 {
		return m
	}
	left := m[:eq]
	val := strings.TrimSpace(m[eq+1:])
	q := ""
	if len(val) >= 2 && (val[0] == '"' || val[0] == '\'') && val[len(val)-1] == val[0] {
		q = string(val[0])
		val = val[1 : len(val)-1]
	}
	name := strings.ToLower(strings.TrimSpace(left))
	var nv string
	if name == "srcset" {
		nv = browserRewriteSrcset(val, tu, direct)
	} else {
		nv = browserProxyURL(val, tu, direct)
	}
	if nv == val {
		return m
	}
	if q == "" {
		q = "\""
	}
	return left + "=" + q + nv + q
}

// browserRewriteSrcset "a.png 1x, b.png 2x" 逐项重写首字段。
func browserRewriteSrcset(v string, tu *url.URL, direct bool) string {
	parts := strings.Split(v, ",")
	for i, p := range parts {
		m := browserReSrcsetPart.FindStringSubmatch(p)
		if m == nil {
			continue
		}
		parts[i] = m[1] + browserProxyURL(m[2], tu, direct) + m[3]
	}
	return strings.Join(parts, ",")
}

// browserRewriteCSS 重写 CSS 里的绝对 url()。
func browserRewriteCSS(css string, tu *url.URL, direct bool) string {
	return browserReCSSURL.ReplaceAllStringFunc(css, func(m string) string {
		inner := m[strings.Index(m, "(")+1 : strings.LastIndex(m, ")")]
		inner = strings.TrimSpace(inner)
		q := ""
		if len(inner) >= 2 && (inner[0] == '"' || inner[0] == '\'') && inner[len(inner)-1] == inner[0] {
			q = string(inner[0])
			inner = inner[1 : len(inner)-1]
		}
		nv := browserProxyURL(inner, tu, direct)
		if nv == inner {
			return m
		}
		// 引号只按原样保留，绝不补：url() 可能出现在 style 属性（外层已被
		// 引号包着）或 JS 字符串里，补引号会提前闭合外层 → 语法错误
		// （实测光猫页 style.background="url(/x.gif) no-repeat" 整段脚本报废）
		if q == "" {
			return "url(" + nv + ")"
		}
		return "url(" + q + nv + q + ")"
	})
}

// browserRewriteCSSOutsideScripts 只改写 <script> 之外的 CSS url()。
// 页面 JS 字符串里常出现 "url(/x.gif) no-repeat" 这类赋值，一并改写会破坏
// JS 字符串（v1.2.55 起的实测故障：整段脚本 SyntaxError）。
func browserRewriteCSSOutsideScripts(doc string, tu *url.URL, direct bool) string {
	locs := browserReScriptBlk.FindAllStringIndex(doc, -1)
	if locs == nil {
		return browserRewriteCSS(doc, tu, direct)
	}
	var b strings.Builder
	b.Grow(len(doc) + 128)
	last := 0
	for _, loc := range locs {
		b.WriteString(browserRewriteCSS(doc[last:loc[0]], tu, direct))
		b.WriteString(doc[loc[0]:loc[1]]) // <script> 块原样保留
		last = loc[1]
	}
	b.WriteString(browserRewriteCSS(doc[last:], tu, direct))
	return b.String()
}

// browserRewriteRefreshValue 重写 Refresh 头/meta 里的 url=。
// 引号只在原文有引号时才补：meta 的 content 本身已被一层引号包着，
// 再补一层会变成 content="0; URL="cgi-bin/luci/"" —— 浏览器解析成"0 秒刷新到
// 空地址"，于是无限刷新自己（实测 192.168.1.50 的 LuCI 首页循环卡死）。
func browserRewriteRefreshValue(v string, tu *url.URL, direct bool) string {
	return browserReRefreshU.ReplaceAllStringFunc(v, func(m string) string {
		sub := browserReRefreshU.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		q := sub[2]
		nv := browserProxyURL(sub[3], tu, direct)
		if q == "" {
			return sub[1] + nv
		}
		return sub[1] + q + nv + q
	})
}

// browserRewriteJS 页面 JS 改写入口：交给词法级引擎（见 jslex.go）。
// 只做四件事：location.host 系列取值替换为面板值、location 赋值改走代理、
// top.location 收成 self、document.domain 赋值中和。
func browserRewriteJS(js string, panelScheme, panelHost string) string {
	return browserRewriteJSLex(js, panelScheme, panelHost)
}

// browserRewriteScriptBlocks 只对 <script>…</script> 块内部做 JS 改写。
// 不能对整个 HTML 文本跑 JS 改写：属性值/可见文本里的 "location.host" 会被
// 破坏，且字符串掩码按 JS 语法扫描 HTML 会失准、误替换产生 SyntaxError
// （实测 www.baidu.com 报 3 处 "Unexpected string"）。
func browserRewriteScriptBlocks(doc, panelScheme, panelHost string) string {
	return browserReScriptBlk.ReplaceAllStringFunc(doc, func(m string) string {
		sub := browserReScriptBlk.FindStringSubmatch(m)
		if sub == nil || sub[2] == "" {
			return m
		}
		return sub[1] + browserRewriteJS(sub[2], panelScheme, panelHost) + sub[3]
	})
}
func panelHostname(host string) string {
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		return host[:i]
	}
	return host
}

func panelPort(host string) string {
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		return host[i+1:]
	}
	return ""
}

// lastSite 最近一次经代理成功加载主文档的站点（"http://192.168.1.1" 形态），
// 供孤儿路径兜底重定向使用。多标签同时开不同站点时可能归错站点——单站点场景
// （路由器管理页）不受影响。
var lastSite atomic.Value // string

// browserReProxySite 从任意地址里提取代理路径中的站点段（scheme/host）。
var browserReProxySite = regexp.MustCompile(`/b/(?:v\d+/)?(https?)/([^/?#]+)`)

// browserReVerSeg 路径里是否带代理版本段（/b/v1315/…）。
var browserReVerSeg = regexp.MustCompile(`(?i)/b/v\d+/`)

// browserCollapseDupPrefix 折叠同一站点的重复代理前缀。
// 输入形如 /b/v1312/https/map.baidu.com/mobile/webapp/b/v1312/https/map.baidu.comindex/index&x=1
// （页面 JS 二次拼接产物），返回 /b/v1312/https/map.baidu.com/mobile/webapp/index&x=1。
// 只处理所有前缀都指向同一站点（scheme/host 相同）的情形——跨站嵌套是真的嵌套，
// 仍交给上面的 400 拦截。
func browserCollapseDupPrefix(raw string) (string, bool) {
	first := browserReProxySite.FindStringSubmatchIndex(raw)
	if first == nil || first[0] != 0 {
		return "", false
	}
	schemeA, hostA := raw[first[2]:first[3]], raw[first[4]:first[5]]
	prefix := raw[:first[1]] // /b/v1312/https/map.baidu.com
	out := prefix
	rest := raw[first[1]:]
	for {
		loc := browserReProxySite.FindStringSubmatchIndex(rest)
		if loc == nil {
			out += rest
			break
		}
		schemeB, hostB := rest[loc[2]:loc[3]], rest[loc[4]:loc[5]]
		if !strings.EqualFold(schemeA, schemeB) {
			return "", false
		}
		// 二次拼接常把路径首段粘在 host 后面（.../map.baidu.comindex/index），
		// 此时 hostB 以 hostA 开头、多出的尾巴其实是路径的一部分：折叠前缀但保留尾巴。
		tail := ""
		if strings.EqualFold(hostB, hostA) {
			// 完全同站
		} else if len(hostB) > len(hostA) && strings.EqualFold(hostB[:len(hostA)], hostA) {
			tail = hostB[len(hostA):]
		} else {
			// 跨站嵌套：不折叠
			return "", false
		}
		head := rest[:loc[0]]
		if tail != "" && !strings.HasSuffix(head, "/") {
			tail = "/" + tail // host 粘连的路径首段要补回分隔斜杠
		}
		out += head + tail
		rest = rest[loc[1]:]
	}
	// 修复后必须真的不含多段前缀；且必须仍是一个合法的代理路径（带 scheme/host）
	if out == raw || browserProxyDepth(out) > 1 {
		return "", false
	}
	if browserReProxySite.FindStringIndex(out) == nil {
		return "", false
	}
	return out, true
}



// browserReAbsProxy 代理路径签名：/b/ + 可选版本段 + scheme + host 齐全。
// 正常站点 URL 几乎不可能出现该签名；能构造出它的只有本代理链路。
var browserReAbsProxy = regexp.MustCompile(`(?i)/b/(?:v\d+/)?https?/[^/?#]+`)

// handleOrphan GET/POST /（非根路径）：页面 JS 用面板 host 拼出的"面板根下
// 绝对 URL"（如 /cgi-bin/x）会落到这里 —— 307 重定向回最近站点的代理路径
// （307 保留方法与请求体，POST 登录表单也能继续）。
// 站点判定：优先从 Referer（代理页地址，/b/ 段里带着真实站点）提取——孤儿
// 请求总由某个代理页内的 JS 发出，它的站点就是归属站点；Referer 缺失才回退
// 全局 lastSite。此前只用 lastSite，多标签同时开不同站点时会把请求重定向到
// 错误站点（实测：同时开地图与管理页，管理页子菜单全部 404 或串到别的站）。
func (s *Server) handleOrphan(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" || r.URL.Path == "" {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			s.handleIndex(w, r)
			return
		}
		fail(w, 404, "未找到")
		return
	}
	site := ""
	if ref := r.Referer(); ref != "" {
		if m := browserReProxySite.FindStringSubmatch(ref); m != nil {
			site = m[1] + "://" + m[2]
		}
	}
	if site == "" {
		site, _ = lastSite.Load().(string)
	}
	if site == "" {
		fail(w, 404, "未找到")
		return
	}
	dest := browserPathPrefix() + strings.Replace(site, "://", "/", 1) + r.URL.RequestURI()
	http.Redirect(w, r, dest, http.StatusTemporaryRedirect)
}

// browserRewriteHTML 全量重写 + 注入 base 与运行时脚本。
func browserRewriteHTML(doc string, tu *url.URL, panelScheme, panelHost string, direct bool) []byte {
	doc = browserReAttr.ReplaceAllStringFunc(doc, func(m string) string { return browserRewriteAttr(m, tu, direct) })
	doc = browserRewriteCSSOutsideScripts(doc, tu, direct) // <style>/style 属性里的 url()，跳过 <script>
	doc = browserReRefresh.ReplaceAllStringFunc(doc, func(m string) string { return browserRewriteRefreshValue(m, tu, direct) })
	doc = browserRewriteScriptBlocks(doc, panelScheme, panelHost) // JS 改写只作用于 <script> 块内部
	doc = browserReBase.ReplaceAllString(doc, "")
	doc = browserReCSPMeta.ReplaceAllString(doc, "")
	basePath := browserURLPath(tu)
	if direct {
		basePath = "/"
	}
	inject := `<base href="` + html.EscapeString(basePath) + `">` + browserPatchScript(tu, direct)
	if m := browserReHead.FindStringIndex(doc); m != nil {
		pos := m[1]
		return []byte(doc[:pos] + inject + doc[pos:])
	}
	if m := browserReHTMLTag.FindStringIndex(doc); m != nil {
		pos := m[1]
		return []byte(doc[:pos] + inject + doc[pos:])
	}
	return []byte(inject + doc)
}

// browserPatchScript 运行时接管脚本（注入点越靠前越好：必须在页面脚本执行前）。
// 作用：把 JS 动态发起的绝对 URL 一律改写为同源代理路径，并把新窗口/新标签请求
// 通过 postMessage 交给面板宿主（新建面板标签页），杜绝跳出到外部浏览器或直连目标。
func browserPatchScript(tu *url.URL, direct bool) string {
	directFlag := "0"
	if direct {
		directFlag = "1"
	}
	// 目标值以字面量注入（ASCII 安全）；脚本内不出现 </script> 字面串
	hostLit := `"` + tu.Hostname() + `"`
	originLit := `"` + tu.Scheme + "://" + tu.Host + `"`
	s := `<script>(function(){try{` +
		// 脚本错误上报（最先安装）：页面 JS 崩溃（如跨源访问抛 SecurityError 导致
		// 登录表单等后续渲染中断）时，面板能直接看到错误位置
		// 导航风暴自保护：登录页互相跳转这类循环不触发"逃逸"判断，
		// 靠页面自身的加载频次拦住（sessionStorage 按路径计数，10 秒窗口）。
		`try{var nk="_xbv_"+location.pathname,nt=Date.now(),na=[];` +
		`try{na=JSON.parse(sessionStorage.getItem(nk)||"[]")||[];}catch(ee){}` +
		`na=na.filter(function(t){return nt-t<10000;});na.push(nt);` +
		`try{sessionStorage.setItem(nk,JSON.stringify(na));}catch(ee){}` +
		`if(na.length>8){document.documentElement.innerHTML='<body style="font:14px system-ui,sans-serif;padding:24px;color:#666">此页面在代理下反复跳转，已停止加载。请改用站点自身的入口打开。</body>';return;}}catch(ee){}` +
		`window.onerror=function(m,s,l){try{parent.postMessage({__xbox:1,type:"jserr",url:location.href,msg:String(m).slice(0,200),line:l,src:String(s||"")},"*");}catch(e){}return false;};` +
		// 表单 target="_top" 接管：光猫等设备的菜单表单提交到最顶层框架，
		// 在 fnOS 桌面的 iframe 沙箱里会被静默拦截（点击菜单"没反应"）。
		// 劫持 HTMLFormElement.prototype.submit（程序化提交不触发 submit 事件），
		// 并在捕获阶段兜底处理事件提交——一律去掉 _top，让提交发生在当前代理页内。
		`try{var fsub=HTMLFormElement.prototype.submit;` +
		`HTMLFormElement.prototype.submit=function(){try{if(String(this.target).toLowerCase()==="_top"){this.removeAttribute("target");}}catch(e){}return fsub.apply(this,arguments);};` +
		`document.addEventListener("submit",function(ev){try{var f=ev.target;if(f&&f.tagName==="FORM"&&String(f.target).toLowerCase()==="_top"){f.removeAttribute("target");}}catch(e){}},true);` +
		`}catch(e){}` +
		`var B="` + appBase + `",P="` + browserPathPrefix() + `",H=` + hostLit + `,O=` + originLit + `,M=` + directFlag + `;` +
		// px: 任意 URL → 代理路径（相对/非 http 原样）
		// spfx：把地址里"已编码的自身代理前缀"剥掉（登录回跳参数常见），
		// 避免每次跳转都把上一次的代理路径再套一层。
		`function spfx(out){try{var qm=out.indexOf("?");if(qm<0)return out;` +
		`var head=out.slice(0,qm),q=out.slice(qm);` +
		`var g=/^(https?):\/\/([\s\S]+)$/.exec(O);if(!g)return out;` +
		`var pres=[P+g[1]+"/"+g[2],P.replace(/v\d+\//,"")+g[1]+"/"+g[2]];` +
		`for(var i=0;i<pres.length;i++){var pv=pres[i];` +
		// 查询参数里的回跳地址可能是 encodeURIComponent 编码（%2F...），
		// 也可能是 encodeURI 未编码（/b/...），两种都要剥
		`q=q.split(encodeURIComponent(pv+"/")).join("%2F").split(pv+"/").join("/")` +
		`.split(encodeURIComponent(pv)).join("").split(pv).join("");}` +
		`var res=head+q;` +
		// 硬兜底：查询串里再出现一段代理标记就整体丢弃，阻止无限嵌套
		`if((res.match(/\/b\/v\d+\//g)||[]).length>1){var qi=res.indexOf("?");if(qi>0)res=res.slice(0,qi);}` +
		`return res;}catch(e){return out;}}` +
		`function px(u){try{if(typeof u!=="string")return u;var s=u.replace(/^\s+|\s+$/g,"");if(!s)return u;` +
		`var l=s.toLowerCase();if(l.indexOf("javascript:")===0||l.indexOf("data:")===0||l.indexOf("mailto:")===0||l.indexOf("#")===0)return u;` +
		// 保险：地址异常长或已含多段代理路径，说明改写出现了自激，直接放弃改写
		`if(s.length>2048||s.split("/b/").length>3)return u;` +
		// 去重：页面 JS 二次拼接会产出含两段代理前缀的畸形地址（实测百度地图
		// 移动版：/b/vNNN/https/host/mobile/webapp/b/vNNN/https/hostindex/index），
		// 保留第一段前缀、删掉后续重复前缀；否则服务端判定嵌套循环 → 400。
		`var sg=s.match(/\/b\/(?:v\d+\/)?https?\/[^\/?#]+|b\/(?:v\d+\/)?https?\/[^\/?#]+/g)||[];` +
		`if(sg.length>1&&s.indexOf(sg[0])===0){` +
		`s=sg[0]+s.slice(sg[0].length).replace(/\/b\/(?:v\d+\/)?https?\/[^\/?#]+|b\/(?:v\d+\/)?https?\/[^\/?#]+/g,"");}` +
		// 幂等：已经是代理路径（服务端已改写过的属性值）绝不能再套一层，
		// 否则 MutationObserver 每轮都会看到"属性又变了"→ 无限嵌套 → 卡死
		`if(s.indexOf(B+"\/b\/")===0)return u;` +
		// 面板自身路径（appBase 开头、但不是 /b/ 代理路径）：原样返回，绝不能当
		// 目标站路径去包装，否则会拼出 目标域名/app/X-Box/… 这种畸形地址
		// （实测：站点里残留的面板相对路径被改写后整页 404）。
		`if(B&&s.indexOf(B+"\/")===0)return u;` +
		// 幂等补：页面 JS 读已改写属性（a.href）时浏览器会解析成"面板域名绝对
		// URL"（https://面板/b/vNNN/https/目标/...），上面只拦相对前缀拦不住，
		// 走通用分支会再包一层 → 服务端 depth 检测 400 → 按钮点击无反应。
		// 命中面板 host + /b/ 路径时截取 /b/ 起的部分原样返回。
		`var am=/^(https?):\/\/([^\/]+)(\/b\/(?:v\d+\/)?(?:https?)\/[^\s]*)$/i.exec(s);` +
		`if(am&&am[2].toLowerCase()===location.host.toLowerCase())return am[3];` +
		// 幂等补 2：网关模式下面板绝对 URL 形如 https://域名/app/X-Box/b/vNNN/...，
		// host 与 /b/ 之间夹着 appBase 前缀，上面的正则匹配不上。这里改用前缀
		// 判定：面板协议+host 开头、且 /b/ 起是合法代理签名时，截取 /b/ 起的
		// 部分并补回 appBase（B），避免丢前缀落到面板根 → 404。
		`var k=s.indexOf("/b/");if(k>=0){var seg=s.substring(k);var ok=k===0;` +
		`if(!ok){var pp=(location.protocol+"//"+location.host+"/").toLowerCase();` +
		`ok=s.substring(0,k).toLowerCase().indexOf(pp)===0;}` +
		`if(ok&&/^\/b\/(?:v\d+\/)?https?\/[^\/?#]/i.test(seg))return B+seg;}` +
		`if(M){if(s.charAt(0)==="/"&&s.charAt(1)!=="/")return s;` +
		`if(s.indexOf("//")===0)s=location.protocol+s;` +
		`var d=/^(https?):\/\/([^\/]+)([\s\S]*)$/i.exec(s);if(!d)return u;` +
		`if(d[2].toLowerCase()!==H.toLowerCase())return u;` +
		`return (d[3]&&d[3].charAt(0)==="/")?d[3]:"/"+(d[3]||"");}` +
		// 根相对（"/res/x.js"）：<base> 管不到，必须显式拼上目标站点根，
		// 否则落到面板根下 → 孤儿重定向 → 站点错位（百度地图 CSS/Worker 全丢）
		`if(s.charAt(0)==="/"&&s.charAt(1)!=="/"){var g=/^(https?):\/\/([\s\S]+)$/.exec(O);if(g)return P+g[1]+"/"+g[2]+s;}` +
		`if(s.indexOf("//")===0)s=location.protocol+s;` +
		// 面板 host 绝对地址：页面 JS 用 location.host 拼接口/资源地址（该值已被
		// 替换成面板 host），如 http://面板/?qt=xxx。它的本意是"当前站点"，若直接
		// 走通用分支会变成"代理面板自身"→ 服务端 400（实测百度地图接口全部失败）。
		// 这里先还原成当前目标站的绝对地址，再交给通用分支包代理路径。
		`var hp=/^(https?):\/\/([^\/]+)([\s\S]*)$/i.exec(s);` +
		`if(hp&&hp[2].toLowerCase()===location.host.toLowerCase()){` +
		`var g2=/^(https?):\/\/([\s\S]+)$/.exec(O);` +
		`if(g2)s=g2[1]+"://"+g2[2]+((hp[3]&&hp[3].charAt(0)==="/")?hp[3]:"/"+(hp[3]||""));}` +
		// 兜底：页面 JS 在真实目标域上下文里二次拼接，产出「外域 host + 面板前缀」
		// 的畸形绝对 URL（实测形态：https://map.baidu.com/app/X-Box/…）。这类地址
		// 不带 /b/ 段时服务端管不到（请求逃逸到 fnOS 网关 → 品牌 404）。这里剥掉
		// 面板前缀：剥后若带代理签名（可能无前导斜杠，如 b/vNNN/https/…）则当
		// 代理路径直接返回（补回 B）；否则还原成目标绝对地址交给通用分支重新包装。
		`var mm=/^(https?):\/\/([^\/]+)([\s\S]*)$/i.exec(s);if(mm&&B){var rp=mm[3]||"";` +
		`if(rp.indexOf(B+"/")===0){rp=rp.slice((B+"/").length);` +
		`var r1="/"+rp.replace(/^\/+/,"");` +
		`var kk=r1.indexOf("/b/");if(kk>=0&&/^\/b\/(?:v\d+\/)?https?\/[^\/?#]/i.test(r1.substring(kk)))return B+r1.substring(kk);` +
		`s=mm[1]+"://"+mm[2]+(r1.charAt(0)==="/"?r1:"/"+r1);}}` +
		`var m=/^(https?):\/\/([^\/]+)([\s\S]*)$/i.exec(s);if(!m)return u;` +
		`var out=P+m[1]+"/"+m[2]+((m[3]&&m[3].charAt(0)==="/")?m[3]:"/"+(m[3]||""));` +
		// 防自嵌套：登录类跳转常把当前地址编码进 redirect_uri 等参数，
		// 若不剥掉其中的代理前缀，每跳一次就多套一层（URL 越来越长 → 死循环）
		`out=spfx(out);` +
		`return out;}catch(e){return u;}}` +
		// un: 代理路径 → 原始绝对 URL
		`function un(p){try{var m=/^\/b\/(?:v\d+\/)?(https?)\/([^\/]+)([\s\S]*)$/.exec(String(p));if(m)return m[1]+"://"+m[2]+(m[3]||"/");}catch(e){}return String(p);}` +
		// norm: 任意形式（相对/代理路径/绝对/面板绝对）→ 原始目标绝对 URL（供宿主新标签用）
		`function norm(u){var s=String(u==null?"":u).replace(/^\s+|\s+$/g,"");if(!s)return "";` +
		`var i=s.indexOf("/b/");if(i>=0)return un(s.substring(i));` +
		`if(s.indexOf("//")===0)s=location.protocol+s;` +
		`if(/^https?:\/\//i.test(s)){var j=s.indexOf("/b/");return j>=0?un(s.substring(j)):s;}` +
		`try{var x=new URL(s,(document.baseURI||location.href)).href,k=x.indexOf("/b/");return k>=0?un(x.substring(k)):x;}catch(e){}return s;}` +
		`function msg(t,u){try{parent.postMessage({__xbox:1,type:t,url:norm(u)},"*");}catch(e){}}` +
		// 暴露给页面 JS：词法级改写把页面里的 location 赋值改写成
		// __xbp(...) 调用，使"跳转"也走代理，而不是跳出代理直连目标
		`try{window.__xbp=px;}catch(e){}` +
		// fetch
		`var of=window.fetch;if(of){window.fetch=function(a,b){try{if(typeof a==="string"){a=px(a);}else if(a&&typeof a.url==="string"){a.url=px(a.url);}}catch(e){}return of.call(window,a,b);};}` +
		// XMLHttpRequest
		`var XP=window.XMLHttpRequest&&window.XMLHttpRequest.prototype;` +
		`if(XP&&XP.open){var oo=XP.open;XP.open=function(){try{if(typeof arguments[1]==="string"){arguments[1]=px(arguments[1]);}}catch(e){}return oo.apply(this,arguments);};}` +
		// location.assign / replace
		`try{var LP=Object.getPrototypeOf(window.location)||null;` +
		`if(LP&&LP.assign){var la=LP.assign;LP.assign=function(u){try{if(typeof u==="string")u=px(u);}catch(e){}return la.call(this,u);};}` +
		`if(LP&&LP.replace){var lr=LP.replace;LP.replace=function(u){try{if(typeof u==="string")u=px(u);}catch(e){}return lr.call(this,u);};}}catch(e){}` +
		// window.open → 宿主新建标签；但带「命名框架」目标且该框架确实存在时放行：
		// 管理类页面（光猫等）常用 window.open(url,"mainFrame") 切换子页面，
		// 一律改成新标签会导致点击后主区域毫无反应。目标不存在时仍走新标签，
		// 避免页面随意弹窗。
		`var nowo=window.open;window.open=function(u,t){try{var nu=px(u);` +
		`if(t&&typeof t==="string"&&t.charAt(0)!=="_"&&t!==""){var tf=null;` +
		`try{tf=window[t];}catch(ee){}` +
		`if(tf&&typeof tf==="object"&&tf.location){return nowo.call(window,nu,t);}}` +
		`}catch(e){}msg("newtab",u);return null;};` +
		`try{window.showModalDialog=function(u){msg("newtab",u);return null;};}catch(e){}` +
		// Worker：新建时把脚本地址改写为代理路径。new Worker("/res/newui/x.js")
		// 是根相对，不改写会落到面板根下（百度地图的 wasm worker 就这么丢的）。
		`try{var OW=window.Worker;if(OW){var NW=function(u,o){return new OW(px(u),o);};` +
		`NW.prototype=OW.prototype;window.Worker=NW;}}catch(e){}` +
		// WebSocket：页面用面板 host 拼 ws 地址（location.host 已被替换为面板）时
		// 改写为代理路径；否则连到面板根 → 307 → 前端常驻提示"连接已断开"。
		`function wsz(u){try{var s=String(u),m=/^(wss?):\/\/([^\/]+)([\s\S]*)$/i.exec(s);if(!m)return s;` +
		`var o=/^(https?):\/\/([\s\S]+)$/.exec(O);if(!o)return s;` +
		`var h=m[2].toLowerCase();` +
		`if(M){if(h===location.host.toLowerCase())return s;` +
		`if(h===o[2].toLowerCase()){var pm=location.protocol==="https:"?"wss:":"ws:";` +
		`return pm+"//"+location.host+(m[3]||"/");}return s;}` +
		`if(h!==location.host.toLowerCase()&&h!==o[2].toLowerCase())return s;` +
		`var pr=location.protocol==="https:"?"wss:":"ws:";` +
		`return pr+"//"+location.host+P+o[1]+"/"+o[2]+(m[3]||"/");}catch(e){return u;}}` +
		`try{var OWS=window.WebSocket;if(OWS){var NWS=function(u,p){var s=u;try{s=wsz(u);}catch(e){}` +
		`return p===undefined?new OWS(s):new OWS(s,p);};NWS.prototype=OWS.prototype;` +
		`NWS.CONNECTING=0;NWS.OPEN=1;NWS.CLOSING=2;NWS.CLOSED=3;window.WebSocket=NWS;}}catch(e){}` +
		// 前端路由：pushState('/login') 会把 iframe 地址改成站点根（逃出代理），
		// 触发"跳转→拦截→重载"的循环。这里把根相对路径改成代理路径。
		`try{var oh=history.pushState,orp=history.replaceState;` +
		`var psc=0,pst=Date.now();` +
		`function hpx(u){try{if(M||typeof u!=="string"||!u)return u;` +
		// 频率闸门：前端路由互相跳转时不产生网络请求，服务端限流拦不住，
		// 只能在这里数。10 秒内超过 30 次就冻结页面，避免持续占用 CPU。
		`var pn=Date.now();if(pn-pst>10000){pst=pn;psc=0;}psc++;` +
		`if(psc>30){try{if(!window.__xbStopped){window.__xbStopped=1;` +
		`document.documentElement.innerHTML='<body style="font:14px system-ui,sans-serif;padding:24px;color:#666">该站点在代理下反复跳转，已停止加载。请改用站点自身的入口打开。</body>';}}catch(e){}` +
		`return location.href;}` +

		// 幂等：页面 JS 用 location.pathname 拼前端路由（地图类 SPA 常见）时，
		// 传进来的已经是代理路径（/b/… 或 appBase+/b/…），再包一层就会变成
		// /b/vNNN/https/host/app/X-Box/b/https/host/… → 整页 404。
		`if(u.indexOf(B+"/b/")===0||/^\/b\/(?:v\d+\/)?https?\/[^\/?#]/i.test(u))return u;` +
		`if(u.charAt(0)==="/"&&u.charAt(1)!=="/"){var g=/^(https?):\/\/([\s\S]+)$/.exec(O);` +
		`if(g)return spfx(P+g[1]+"/"+g[2]+u);}return u;}catch(e){return u;}}` +
		`history.pushState=function(s,t,u){return oh.call(history,s,t,hpx(u));};` +
		`history.replaceState=function(s,t,u){return orp.call(history,s,t,hpx(u));};}catch(e){}` +
		// 原型级 setter 拦截：地图瓦片/懒加载图片大量使用 new Image()、
		// createElement('img') 且**不插入 DOM**（MutationObserver 观察不到），
		// 只能通过原型上的 src/href setter 兜住，否则这类资源直连目标必然失败。
		`var PS=[["HTMLImageElement","src"],["HTMLScriptElement","src"],["HTMLLinkElement","href"],` +
		`["HTMLSourceElement","src"],["HTMLVideoElement","src"],["HTMLAudioElement","src"],` +
		`["HTMLIFrameElement","src"],["HTMLInputElement","src"],["HTMLEmbedElement","src"],["HTMLTrackElement","src"]];` +
		`for(var pi=0;pi<PS.length;pi++){try{(function(nm,at){var P=window[nm];if(!P||!P.prototype)return;` +
		`var d=Object.getOwnPropertyDescriptor(P.prototype,at);if(!d||!d.set)return;` +
		`Object.defineProperty(P.prototype,at,{set:function(v){try{d.set.call(this,px(v));}catch(e){d.set.call(this,v);}},` +
		`get:d.get,configurable:true});})(PS[pi][0],PS[pi][1]);}catch(e){}}` +
		// 动态节点属性修正
		`var ATTR=["href","src","action","poster","srcset","formaction","data-src","data-href"];` +
		`function sx(v){var ps=String(v).split(","),o=[];for(var i=0;i<ps.length;i++){var t=ps[i].replace(/^\s+|\s+$/g,"");var sp=t.split(/\s+/);if(sp.length){sp[0]=px(sp[0]);o.push(sp.join(" "));}else{o.push(t);}}return o.join(",");}` +
		`function fixEl(n){if(!n||n.nodeType!==1||!n.getAttribute)return;` +
		// style 属性里的绝对 url() 也要改写（JS 设置的背景图/装饰图走这里）
		`try{if(n.hasAttribute("style")){var sv=n.getAttribute("style");` +
		`var s2=sv.replace(/url\(\s*(['"]?)([^)'"]+)\1\s*\)/g,function(m,q,u){return "url("+q+px(u)+q+")";});` +
		`if(s2!==sv){n.setAttribute("style",s2);}}}catch(e){}` +
		`for(var i=0;i<ATTR.length;i++){var a=ATTR[i];if(!n.hasAttribute(a))continue;var v=n.getAttribute(a);if(!v)continue;` +
		`var nv=(a==="srcset")?sx(v):px(v);if(nv!==v&&nv.length<1500){try{n.setAttribute(a,nv);}catch(e){}}}}` +
		`function scan(root){if(!root||!root.querySelectorAll)return;try{var ns=root.querySelectorAll("a[href],img[src],script[src],link[href],form[action],iframe[src],frame[src],source[src],video[poster],area[href],input[formaction],button[formaction],embed[src],object[data],track[src]");` +
		`for(var i=0;i<ns.length;i++){fixEl(ns[i]);}}catch(e){}}` +
		`try{new MutationObserver(function(ms){for(var i=0;i<ms.length;i++){var m=ms[i];` +
		`if(m.type==="attributes"){fixEl(m.target);}else if(m.addedNodes){for(var j=0;j<m.addedNodes.length;j++){fixEl(m.addedNodes[j]);scan(m.addedNodes[j]);}}}}` +
		`).observe(document.documentElement||document,{childList:true,subtree:true,attributes:true,attributeFilter:ATTR.concat(["style"])});}catch(e){}` +
		`try{scan(document);}catch(e){}` +
		`document.addEventListener("DOMContentLoaded",function(){try{scan(document);}catch(e){}},false);` +
		`window.addEventListener("load",function(){try{scan(document);}catch(e){}},false);` +
		// 链接点击：_blank / Ctrl / 中键 → 宿主新建标签；其余留在当前框架
		`document.addEventListener("click",function(e){try{var n=e.target;` +
		`while(n&&n.nodeType!==1&&n.parentNode)n=n.parentNode;` +
		`while(n&&n.nodeType===1&&n.tagName!=="A"&&n!==document.documentElement)n=n.parentNode;` +
		`if(!n||n.nodeType!==1||n.tagName!=="A")return;` +
		`var t=n.getAttribute("target"),tl=(t||"").toLowerCase();` +
		// 只收敛会跳出代理的目标；命名框架（mainFrame 等）必须保留，
		// 否则管理类页面点菜单链接会「原地不动、内容不变」。
		`if(tl==="_top"||tl==="_parent"){try{n.setAttribute("target","_self");}catch(e2){}}` +
		`if(tl==="_blank"||e.ctrlKey||e.metaKey||e.button===1){try{e.preventDefault();}catch(e3){}msg("newtab",n.getAttribute("href")||n.href);}}catch(e){}},true);` +
		// 表单 target：同上，只把 _top/_parent/_blank 收回框架内
		`document.addEventListener("submit",function(e){try{var n=e.target;if(n&&n.nodeType===1&&n.tagName==="FORM"){var t=(n.getAttribute("target")||"").toLowerCase();if(t==="_top"||t==="_parent"||t==="_blank"){n.setAttribute("target","_self");}}}catch(e){}},true);` +
		`}catch(e){}})();</script>`
	return s
}

// browserErrText 把代理失败原因翻译成可读中文（日志与错误页共用）。
func browserErrText(err error) string {
	if ue, ok := err.(*url.Error); ok && ue.Err != nil {
		e := strings.ToLower(ue.Err.Error())
		switch {
		case strings.Contains(e, "connection refused"):
			return "目标拒绝连接（端口未开放，或服务未启动）"
		case strings.Contains(e, "no such host"), strings.Contains(e, "lookup"):
			return "域名解析失败（DNS 无法解析该主机名）"
		case strings.Contains(e, "i/o timeout"), strings.Contains(e, "deadline exceeded"), strings.Contains(e, "timeout"):
			return "连接超时（目标不可达，或被防火墙丢弃）"
		case strings.Contains(e, "network is unreachable"), strings.Contains(e, "no route to host"):
			return "网络不可达（路由不通）"
		case strings.Contains(e, "connection reset"):
			return "连接被目标重置"
		case strings.Contains(e, "certificate"):
			return "TLS 证书校验失败（已跳过校验仍失败，多为协议不匹配）"
		default:
			return ue.Err.Error()
		}
	}
	return err.Error()
}

// browserFail 代理失败：返回可读的中文 HTML 错误页（请求由 NAS 发出，
// 失败意味着 NAS→目标不通，不是用户电脑的问题），并给出终端自检命令。
func browserFail(w http.ResponseWriter, target string, err error) {
	reason := browserErrText(err)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(502)
	page := `<!doctype html><html><head><meta charset="utf-8"><title>无法访问该地址</title><style>` +
		`body{font:14px/1.8 system-ui,-apple-system,"Microsoft YaHei",sans-serif;padding:28px;color:#374151;background:#fff;margin:0}` +
		`.box{max-width:680px;margin:0 auto;border:1px solid #FDCDC5;background:#FFF6F4;border-radius:10px;padding:20px 22px}` +
		`h2{margin:0 0 12px;font-size:16px;color:#B42318}` +
		`code{background:#F3F4F6;padding:2px 6px;border-radius:4px;word-break:break-all;font-family:ui-monospace,Consolas,monospace}` +
		`ol{margin:10px 0 0 20px;padding:0}li{margin:6px 0}.tip{margin-top:14px;font-size:13px;color:#667085}</style></head><body><div class="box">` +
		`<h2>无法访问目标地址</h2>` +
		`<div>目标：<code>` + html.EscapeString(target) + `</code></div>` +
		`<div>原因：` + html.EscapeString(reason) + `</div>` +
		`<div>时间：` + time.Now().Format("2006-01-02 15:04:05") + `（NAS 本地时间）</div>` +
		`<div class="tip">说明：页面由 <b>NAS 上的 X-Box 代为访问</b>（你的设备并不直连目标），所以这里失败说明 <b>NAS 到目标不通</b>，与你电脑能否访问无关。</div>` +
		`<ol><li>确认目标已开机、端口正确（路由器管理页若非 80 端口，请写成 <code>192.168.1.1:8080</code>）；</li>` +
		`<li>确认目标未限制只允许特定 IP / MAC 访问；</li>` +
		`<li>到「终端」执行 <code>curl -v --max-time 8 ` + html.EscapeString(target) + `</code> 看 NAS 侧的真实报错；也可 <code>ping -c 3 ` + html.EscapeString(tu_host(target)) + `</code> 看是否可达。</li></ol>` +
		`<div class="tip" style="margin-top:16px">` +
		`<button onclick="location.reload()" style="font:14px system-ui;padding:7px 16px;border:1px solid #B42318;color:#B42318;background:#fff;border-radius:6px;cursor:pointer">重试</button>` +
		`<span style="margin-left:10px">若只是网络抖动，点重试通常即可恢复。</span></div>` +
		`</div></body></html>`
	_, _ = io.WriteString(w, page)
}

// tu_host 取目标地址的主机部分（用于错误页提示的 ping 目标），失败返回原串。
func tu_host(target string) string {
	if u, err := url.Parse(target); err == nil && u.Hostname() != "" {
		return u.Hostname()
	}
	return target
}

// browserProxyDepth 统计 URL 里代理路径段的数量：正常为 1，嵌套会递增。
func browserProxyDepth(raw string) int {
	n := 0
	for i := 0; i+3 < len(raw); i++ {
		if raw[i] != '/' || raw[i+1] != 'b' || raw[i+2] != '/' || raw[i+3] != 'v' {
			continue
		}
		j := i + 4
		digits := 0
		for j < len(raw) && raw[j] >= '0' && raw[j] <= '9' {
			j++
			digits++
		}
		if digits > 0 && j < len(raw) && raw[j] == '/' {
			n++
			i = j
		}
	}
	return n
}

// proxySlots 普通代理请求的并发闸门（WebSocket 长连接不占用）。
var proxySlots = make(chan struct{}, 24)

func browserAcquireSlot() bool {
	select {
	case proxySlots <- struct{}{}:
		return true
	default:
		return false
	}
}

func browserReleaseSlot() { <-proxySlots }

// browserSplitHostPort 拆分主机与端口（IPv6 方括号安全）。
func browserSplitHostPort(host string) (string, string) {
	h := strings.TrimSpace(host)
	if strings.HasPrefix(h, "[") {
		if i := strings.Index(h, "]"); i > 0 {
			addr := h[1:i]
			if i+1 < len(h) && h[i+1] == ':' {
				return addr, h[i+2:]
			}
			return addr, ""
		}
	}
	if i := strings.LastIndex(h, ":"); i > 0 && !strings.Contains(h[i:], "]") {
		return h[:i], h[i+1:]
	}
	return h, ""
}

// browserIsLocalAddr 判断主机是否属于本机（回环或本机网卡地址）。
func browserIsLocalAddr(h string) bool {
	h = strings.ToLower(strings.Trim(h, "[]"))
	if h == "" {
		return false
	}
	if h == "localhost" || h == "0.0.0.0" || h == "::" {
		return true
	}
	ip := net.ParseIP(h)
	if ip == nil {
		return false
	}
	if ip.IsLoopback() || ip.IsUnspecified() {
		return true
	}
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return false
	}
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok && ipn.IP.Equal(ip) {
			return true
		}
	}
	return false
}

// browserIsLocalGateway 本机的 80/443（含无端口）就是 fnOS 网关入口，
// 代理它必然形成"应用→网关→应用"的环。
func browserIsLocalGateway(host string) bool {
	h, port := browserSplitHostPort(host)
	if !browserIsLocalAddr(h) {
		return false
	}
	return port == "" || port == "80" || port == "443"
}

// browserNavAllowed 同一路径（含查询串）每秒最多 5 次 HTML 导航。
var navRate = struct {
	mu sync.Mutex
	m  map[string]*navHit
}{m: map[string]*navHit{}}

type navHit struct {
	ts []time.Time
}

func browserNavAllowed(key string) bool {
	navRate.mu.Lock()
	defer navRate.mu.Unlock()
	now := time.Now()
	if len(navRate.m) > 512 { // 简单防膨胀
		navRate.m = map[string]*navHit{}
	}
	h := navRate.m[key]
	if h == nil {
		h = &navHit{}
		navRate.m[key] = h
	}
	// 滑动窗口：只保留 10 秒内的加载时间点
	keep := h.ts[:0]
	for _, t := range h.ts {
		if now.Sub(t) < 10*time.Second {
			keep = append(keep, t)
		}
	}
	h.ts = append(keep, now)
	return len(h.ts) <= 8
}
