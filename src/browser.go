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
	"crypto/tls"
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
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
	browserTimeout   = 30 * time.Second
	browserGzipMin   = 1024 // 文本类响应超过该体积才压缩回传
)

// browserTransport 包级共享：连接复用（keep-alive）对代理性能是决定性的——
// 页面往往有几十个资源，每个都新建 TCP/TLS 连接会让点击链接明显变慢。
// 内网设备普遍自签证书，故跳过校验（与浏览器"继续访问"等价）。
var browserTransport = &http.Transport{
	TLSClientConfig:     &tls.Config{InsecureSkipVerify: true},
	Proxy:               nil,
	ForceAttemptHTTP2:   true,
	MaxIdleConns:        64,
	MaxIdleConnsPerHost: 8,
	IdleConnTimeout:     90 * time.Second,
}

// handleBrowserRoot GET /b（无子路径）：提示用法。
func (s *Server) handleBrowserRoot(w http.ResponseWriter, r *http.Request) {
	fail(w, 400, "用法：/b/http/<主机>/<路径> 或 /b/https/<主机>/<路径>")
}

// handleBrowserProxy GET/POST /b/{scheme}/{host}/{path}?{query}：同源网页代理。
func (s *Server) handleBrowserProxy(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.RequestURI() // 保留原始编码（含查询串）
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
	sub := "/"
	if len(parts) == 3 && parts[2] != "" {
		sub += parts[2]
	}
	target := scheme + "://" + host + sub
	tu, terr := url.Parse(target)
	if terr != nil || tu.Host == "" {
		fail(w, 400, "目标地址非法")
		return
	}

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

	var body io.Reader
	if r.Method == http.MethodPost && r.Body != nil {
		body = r.Body // POST 表单体原样转发（登录等场景）
	}
	// 每请求独立 cookiejar：本请求（含目标站 Set-Cookie）种下的会话随响应回写。
	jar, _ := cookiejar.New(nil)
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
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	req.Header.Set("Accept-Language", "zh-CN,zh;q=0.9,en;q=0.8")
	if ct := r.Header.Get("Content-Type"); ct != "" && r.Method == http.MethodPost {
		req.Header.Set("Content-Type", ct)
	}
	// 入向 Cookie 透传（浏览器按 /b/{scheme}/{host}/ 子树自动携带，天然按站点隔离）
	if ck := r.Header.Get("Cookie"); ck != "" {
		req.Header.Set("Cookie", ck)
	}
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
	if err != nil {
		rec.Err = browserErrText(err)
		browserFail(w, target, err)
		return
	}
	defer resp.Body.Close()

	// 剥离阻碍 iframe 内嵌的头（响应由面板同源提供，原站策略不适用于本响应）
	for _, h := range []string{"X-Frame-Options", "Content-Security-Policy", "Content-Security-Policy-Report-Only"} {
		resp.Header.Del(h)
	}
	// 跳转类响应头一律改写成代理路径后写给浏览器（否则浏览器直连目标 → 拒绝连接）
	for _, h := range []string{"Location", "Content-Location", "Refresh"} {
		v := resp.Header.Get(h)
		if v == "" {
			continue
		}
		if h == "Refresh" {
			w.Header().Set(h, browserRewriteRefreshValue(v, tu))
			continue
		}
		w.Header().Set(h, browserProxyURL(v, tu))
	}
	// 出向 Cookie 回写：显式 Path 限本站点代理子树（浏览器默认"当前请求目录"
	// 在无子路径时会抬到 /b/，导致跨站点串 cookie）
	cookiePath := "; Path=" + browserPathPrefix() + scheme + "/" + host + "/"
	for _, c := range jar.Cookies(tu) {
		w.Header().Add("Set-Cookie", c.Name+"="+c.Value+cookiePath)
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
		browserWriteStatus(w, resp.StatusCode)
		rec.Snippet = browserSnippet(data)
		browserSnapSave(target, ct, data) // 保存原始响应（重写前），供「查看源码」诊断
		lastSite.Store(scheme + "://" + host)
		browserWriteText(cw, w, r, browserRewriteHTML(string(data), tu, panelScheme, panelHost))
	case strings.Contains(low, "text/css"):
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, browserMaxText))
		if rerr != nil {
			rec.Err = "读取响应失败：" + rerr.Error()
			browserFail(w, target, rerr)
			return
		}
		w.Header().Set("Content-Type", browserKeepCharset(ct))
		browserWriteStatus(w, resp.StatusCode)
		rec.Snippet = browserSnippet(data)
		browserSnapSave(target, ct, data)
		browserWriteText(cw, w, r, []byte(browserRewriteCSS(string(data), tu)))
	case strings.Contains(low, "javascript") || strings.Contains(low, "ecmascript"):
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, browserMaxText))
		if rerr != nil {
			rec.Err = "读取响应失败：" + rerr.Error()
			browserFail(w, target, rerr)
			return
		}
		w.Header().Set("Content-Type", browserKeepCharset(ct))
		browserWriteStatus(w, resp.StatusCode)
		rec.Snippet = browserSnippet(data)
		browserSnapSave(target, ct, data)
		browserWriteText(cw, w, r, []byte(browserRewriteJS(string(data), panelScheme, panelHost)))
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
func browserWriteText(cw *countWriter, w http.ResponseWriter, r *http.Request, data []byte) {
	if len(data) >= browserGzipMin && strings.Contains(strings.ToLower(r.Header.Get("Accept-Encoding")), "gzip") {
		var buf bytes.Buffer
		gz := gzip.NewWriter(&buf)
		if _, err := gz.Write(data); err == nil && gz.Close() == nil && buf.Len() < len(data) {
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Set("Vary", "Accept-Encoding")
			_, _ = cw.Write(buf.Bytes())
			return
		}
	}
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
func browserPathPrefix() string { return "/b/" + browserVerSeg() + "/" }

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
func browserProxyURL(s string, cur *url.URL) string {
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
	if strings.HasPrefix(s, "/b/") {
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
	return browserURLPath(u)
}

// browserRewriteAttr 整体匹配形式：" name=value"（含引号/无引号）。
func browserRewriteAttr(m string, tu *url.URL) string {
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
		nv = browserRewriteSrcset(val, tu)
	} else {
		nv = browserProxyURL(val, tu)
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
func browserRewriteSrcset(v string, tu *url.URL) string {
	parts := strings.Split(v, ",")
	for i, p := range parts {
		m := browserReSrcsetPart.FindStringSubmatch(p)
		if m == nil {
			continue
		}
		parts[i] = m[1] + browserProxyURL(m[2], tu) + m[3]
	}
	return strings.Join(parts, ",")
}

// browserRewriteCSS 重写 CSS 里的绝对 url()。
func browserRewriteCSS(css string, tu *url.URL) string {
	return browserReCSSURL.ReplaceAllStringFunc(css, func(m string) string {
		inner := m[strings.Index(m, "(")+1 : strings.LastIndex(m, ")")]
		inner = strings.TrimSpace(inner)
		q := ""
		if len(inner) >= 2 && (inner[0] == '"' || inner[0] == '\'') && inner[len(inner)-1] == inner[0] {
			q = string(inner[0])
			inner = inner[1 : len(inner)-1]
		}
		nv := browserProxyURL(inner, tu)
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
func browserRewriteCSSOutsideScripts(doc string, tu *url.URL) string {
	locs := browserReScriptBlk.FindAllStringIndex(doc, -1)
	if locs == nil {
		return browserRewriteCSS(doc, tu)
	}
	var b strings.Builder
	b.Grow(len(doc) + 128)
	last := 0
	for _, loc := range locs {
		b.WriteString(browserRewriteCSS(doc[last:loc[0]], tu))
		b.WriteString(doc[loc[0]:loc[1]]) // <script> 块原样保留
		last = loc[1]
	}
	b.WriteString(browserRewriteCSS(doc[last:], tu))
	return b.String()
}

// browserRewriteRefreshValue 重写 Refresh 头/meta 里的 url=。
// 引号只在原文有引号时才补：meta 的 content 本身已被一层引号包着，
// 再补一层会变成 content="0; URL="cgi-bin/luci/"" —— 浏览器解析成"0 秒刷新到
// 空地址"，于是无限刷新自己（实测 192.168.1.50 的 LuCI 首页循环卡死）。
func browserRewriteRefreshValue(v string, tu *url.URL) string {
	return browserReRefreshU.ReplaceAllStringFunc(v, func(m string) string {
		sub := browserReRefreshU.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		q := sub[2]
		nv := browserProxyURL(sub[3], tu)
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

// handleOrphan GET/POST /（非根路径）：页面 JS 用面板 host 拼出的"面板根下
// 绝对 URL"（如 /cgi-bin/x）会落到这里 —— 307 重定向回最近站点的代理路径
// （307 保留方法与请求体，POST 登录表单也能继续）。
func (s *Server) handleOrphan(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" || r.URL.Path == "" {
		if r.Method == http.MethodGet || r.Method == http.MethodHead {
			s.handleIndex(w, r)
			return
		}
		fail(w, 404, "未找到")
		return
	}
	site, _ := lastSite.Load().(string)
	if site == "" {
		fail(w, 404, "未找到")
		return
	}
	dest := browserPathPrefix() + strings.Replace(site, "://", "/", 1) + r.URL.RequestURI()
	http.Redirect(w, r, dest, http.StatusTemporaryRedirect)
}

// browserRewriteHTML 全量重写 + 注入 base 与运行时脚本。
func browserRewriteHTML(doc string, tu *url.URL, panelScheme, panelHost string) []byte {
	doc = browserReAttr.ReplaceAllStringFunc(doc, func(m string) string { return browserRewriteAttr(m, tu) })
	doc = browserRewriteCSSOutsideScripts(doc, tu) // <style>/style 属性里的 url()，跳过 <script>
	doc = browserReRefresh.ReplaceAllStringFunc(doc, func(m string) string { return browserRewriteRefreshValue(m, tu) })
	doc = browserRewriteScriptBlocks(doc, panelScheme, panelHost) // JS 改写只作用于 <script> 块内部
	doc = browserReBase.ReplaceAllString(doc, "")
	doc = browserReCSPMeta.ReplaceAllString(doc, "")
	inject := `<base href="` + html.EscapeString(browserURLPath(tu)) + `">` + browserPatchScript(tu)
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
func browserPatchScript(tu *url.URL) string {
	// 目标值以字面量注入（ASCII 安全）；脚本内不出现 </script> 字面串
	hostLit := `"` + tu.Hostname() + `"`
	originLit := `"` + tu.Scheme + "://" + tu.Host + `"`
	s := `<script>(function(){try{` +
		// 脚本错误上报（最先安装）：页面 JS 崩溃（如跨源访问抛 SecurityError 导致
		// 登录表单等后续渲染中断）时，面板能直接看到错误位置
		`window.onerror=function(m,s,l){try{parent.postMessage({__xbox:1,type:"jserr",url:location.href,msg:String(m).slice(0,200),line:l,src:String(s||"")},"*");}catch(e){}return false;};` +
		`var P="` + browserPathPrefix() + `",H=` + hostLit + `,O=` + originLit + `;` +
		// px: 任意 URL → 代理路径（相对/非 http 原样）
		`function px(u){try{if(typeof u!=="string")return u;var s=u.replace(/^\s+|\s+$/g,"");if(!s)return u;` +
		`var l=s.toLowerCase();if(l.indexOf("javascript:")===0||l.indexOf("data:")===0||l.indexOf("mailto:")===0||l.indexOf("#")===0)return u;` +
		// 幂等：已经是代理路径（服务端已改写过的属性值）绝不能再套一层，
		// 否则 MutationObserver 每轮都会看到"属性又变了"→ 无限嵌套 → 卡死
		`if(s.indexOf("/b/")===0)return u;` +
		// 根相对（"/res/x.js"）：<base> 管不到，必须显式拼上目标站点根，
		// 否则落到面板根下 → 孤儿重定向 → 站点错位（百度地图 CSS/Worker 全丢）
		`if(s.charAt(0)==="/"&&s.charAt(1)!=="/"){var g=/^(https?):\/\/([\s\S]+)$/.exec(O);if(g)return P+g[1]+"/"+g[2]+s;}` +
		`if(s.indexOf("//")===0)s=location.protocol+s;` +
		`var m=/^(https?):\/\/([^\/]+)([\s\S]*)$/i.exec(s);if(!m)return u;` +
		`return P+m[1]+"/"+m[2]+((m[3]&&m[3].charAt(0)==="/")?m[3]:"/"+(m[3]||""));}catch(e){return u;}}` +
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
		// window.open / showModalDialog → 交由宿主新建标签
		`window.open=function(u){msg("newtab",u);return null;};` +
		`try{window.showModalDialog=function(u){msg("newtab",u);return null;};}catch(e){}` +
		// Worker：新建时把脚本地址改写为代理路径。new Worker("/res/newui/x.js")
		// 是根相对，不改写会落到面板根下（百度地图的 wasm worker 就这么丢的）。
		`try{var OW=window.Worker;if(OW){var NW=function(u,o){return new OW(px(u),o);};` +
		`NW.prototype=OW.prototype;window.Worker=NW;}}catch(e){}` +
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
		`var nv=(a==="srcset")?sx(v):px(v);if(nv!==v){try{n.setAttribute(a,nv);}catch(e){}}}}` +
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
		`var t=n.getAttribute("target");if(t&&t!=="_self"){try{n.setAttribute("target","_self");}catch(e2){}}` +
		`if(t==="_blank"||e.ctrlKey||e.metaKey||e.button===1){try{e.preventDefault();}catch(e3){}msg("newtab",n.getAttribute("href")||n.href);}}catch(e){}},true);` +
		// 表单 target 一律收回框架内
		`document.addEventListener("submit",function(e){try{var n=e.target;if(n&&n.nodeType===1&&n.tagName==="FORM"){var t=n.getAttribute("target");if(t&&t!=="_self"){n.setAttribute("target","_self");}}}catch(e){}},true);` +
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
