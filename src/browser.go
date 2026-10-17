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
	CType   string `json:"ctype,omitempty"`  // 响应 Content-Type（定位"空白是否因类型/编码"）
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
	browserGzipMin   = 1024     // 文本类响应超过该体积才压缩回传
)

// browserTransport 包级共享：连接复用（keep-alive）对代理性能是决定性的——
// 页面往往有几十个资源，每个都新建 TCP/TLS 连接会让点击链接明显变慢。
// 内网设备普遍自签证书，故跳过校验（与浏览器"继续访问"等价）。
var browserTransport = &http.Transport{
	TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
	Proxy:             nil,
	ForceAttemptHTTP2: true,
	MaxIdleConns:      64,
	MaxIdleConnsPerHost: 8,
	IdleConnTimeout:   90 * time.Second,
}

// handleBrowserRoot GET /b（无子路径）：提示用法。
func (s *Server) handleBrowserRoot(w http.ResponseWriter, r *http.Request) {
	fail(w, 400, "用法：/b/http/<主机>/<路径> 或 /b/https/<主机>/<路径>")
}

// handleBrowserProxy GET/POST /b/{scheme}/{host}/{path}?{query}：同源网页代理。
func (s *Server) handleBrowserProxy(w http.ResponseWriter, r *http.Request) {
	raw := r.URL.RequestURI() // 保留原始编码（含查询串）
	rest := strings.TrimPrefix(raw, "/b/")
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
	cookiePath := "; Path=/b/" + scheme + "/" + host + "/"
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

// browserKeepCharset 保留原始 charset：原 Content-Type 已含 charset 原样返回；
// 缺失时仅补 text/html（不强制 utf-8，交由页面 meta/浏览器探测）。
func browserKeepCharset(ct string) string {
	if strings.Contains(strings.ToLower(ct), "charset") {
		return ct
	}
	return "text/html"
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
	browserReBase      = regexp.MustCompile(`(?i)<base\b[^>]*>`)
	browserReHead      = regexp.MustCompile(`(?i)<head[^>]*>`)
	browserReHTMLTag   = regexp.MustCompile(`(?i)<html[^>]*>`)
	browserReCSPMeta   = regexp.MustCompile(`(?i)<meta[^>]+http-equiv\s*=\s*("content-security-policy"|'content-security-policy'|content-security-policy)[^>]*>`)
	browserReAttr      = regexp.MustCompile(`(?i)\s(href|src|action|poster|srcset|formaction|data-src|data-href|data-url|data-original)\s*=\s*("[^"]*"|'[^']*'|[^\s>]+)`)
	browserReSrcsetPart = regexp.MustCompile(`^(\s*)([^\s,]+)(.*)$`)
	browserReCSSURL    = regexp.MustCompile(`(?i)url\(\s*('[^']*'|"[^"]*"|[^)]*?)\s*\)`)
	browserReRefresh   = regexp.MustCompile(`(?i)<meta[^>]*http-equiv\s*=\s*["']?refresh["']?[^>]*>`)
	browserReRefreshU  = regexp.MustCompile(`(?i)(url\s*=\s*)(['"]?)([^'">;]+)`)
	browserReJSHost    = regexp.MustCompile(`(?i)\b(location|document\.location|window\.location|top\.location|self\.location|parent\.location)\s*\.\s*(hostname|host|origin|protocol|port)\b`)
	browserReDocDomain = regexp.MustCompile(`(?i)\bdocument\s*\.\s*domain\s*=\s*["'][^"']*["']`)
	browserReTopLoc    = regexp.MustCompile(`(?i)\btop\s*\.\s*location\b`)
	// 页面 JS 的 location 赋值（location=/location.href=/top.location=/self.location=…）
	// ——这是"跳出代理"的头号出口：赋值触发真实导航，直连绝对 URL。
	// 统一改写为 self.location.href = __xbp(原表达式)，由运行时函数把绝对 URL
	// 转成代理路径，导航就落在代理内。注意：v1.2.50 曾简单改为 self.location
	// （不包装），结果赋值仍直连 → 逃逸 → 拉回 → 无限重载循环。
	// 字符类排除 '=' 以免误伤 == 比较；到分号结束覆盖单行赋值。
	browserReLocAssign = regexp.MustCompile(`(?i)\b(?:document\s*\.\s*location|window\s*\.\s*location|self\s*\.\s*location|top\s*\.\s*location|parent\s*\.\s*location|location)\s*(?:\.\s*href\s*)?\s*=\s*([^;={}\n]+)\s*;`)
)

// browserUnProxy 把面板形态的 URL（含 /b/{scheme}/{host}/...）还原为目标 URL，
// 用于 Referer 转发（目标站防盗链校验需要真实来源）。非代理 URL 原样返回。
func browserUnProxy(s string) string {
	i := strings.Index(s, "/b/")
	if i < 0 {
		return s
	}
	rest := s[i+3:]
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

// browserURLPath 代理路径形态的 URL（/b/{scheme}/{host}{path}?{query}）。
func browserURLPath(u *url.URL) string {
	p := u.EscapedPath()
	if p == "" || !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if u.RawQuery != "" {
		p += "?" + u.RawQuery
	}
	return "/b/" + u.Scheme + "/" + u.Host + p
}

// browserProxyURL 把绝对/协议相对 URL 转成代理路径；相对 URL 与非 http(s) 原样返回。
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
	if strings.HasPrefix(s, "//") { // 协议相对
		s = cur.Scheme + ":" + s
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
		if q == "" {
			q = "\""
		}
		return "url(" + q + nv + q + ")"
	})
}

// browserRewriteRefreshValue 重写 Refresh 头里的 url=。
func browserRewriteRefreshValue(v string, tu *url.URL) string {
	return browserReRefreshU.ReplaceAllStringFunc(v, func(m string) string {
		sub := browserReRefreshU.FindStringSubmatch(m)
		if sub == nil {
			return m
		}
		q := sub[2]
		nv := browserProxyURL(sub[3], tu)
		if q == "" {
			q = "\""
		}
		return sub[1] + q + nv + q
	})
}

// browserStringMask 标记 js 中处于字符串字面量/模板串/注释内的字节。
// 字符串里的 "location.host" 等不是代码（eval 参数、拼接的代码文本很常见），
// 一旦被替换就会产生 SyntaxError（实测 music.taihe.com："Unexpected string"），
// 所以 JS 改写必须只作用于代码区。
func browserStringMask(js string) []bool {
	m := make([]bool, len(js))
	inStr := byte(0)
	i, n := 0, len(js)
	for i < n {
		c := js[i]
		if inStr == 0 {
			switch c {
			case '\'', '"', '`':
				inStr = c
				m[i] = true
				i++
			case '/':
				if i+1 < n && js[i+1] == '/' { // 行注释
					for i < n && js[i] != '\n' {
						m[i] = true
						i++
					}
				} else if i+1 < n && js[i+1] == '*' { // 块注释
					stop := n
					if end := strings.Index(js[i+2:], "*/"); end >= 0 {
						stop = i + 2 + end + 2
					}
					if stop > n {
						stop = n
					}
					for k := i; k < stop; k++ {
						m[k] = true
					}
					i = stop
				} else {
					i++
				}
			default:
				i++
			}
		} else {
			m[i] = true
			if c == '\\' && i+1 < n { // 转义字符
				m[i+1] = true
				i += 2
				continue
			}
			if c == inStr {
				inStr = 0
			}
			i++
		}
	}
	return m
}

// browserReplaceCode 只在代码区（非字符串/注释）做正则替换：
// 匹配起点落在字符串/注释内的一律跳过（保守——漏替换好过语法错误）。
// browserReplaceCode 只在代码区（非字符串/注释）做正则替换：
// 匹配起点落在字符串/注释内的一律跳过（保守——漏替换好过语法错误）。
// repl 的 tail 参数是匹配后紧随的文本，供调用方判断上下文（如是否在赋值左侧）。
func browserReplaceCode(js string, re *regexp.Regexp, repl func(m []string, tail string) string) string {
	locs := re.FindAllStringSubmatchIndex(js, -1)
	if locs == nil {
		return js
	}
	mask := browserStringMask(js)
	var b strings.Builder
	b.Grow(len(js) + 256)
	last := 0
	for _, loc := range locs {
		if loc[0] < last { // 与上一匹配重叠
			continue
		}
		if mask[loc[0]] { // 起点在字符串/注释内：原样保留
			continue
		}
		b.WriteString(js[last:loc[0]])
		sub := make([]string, 0, len(loc)/2)
		for g := 0; g*2 < len(loc); g++ {
			if loc[g*2] < 0 {
				sub = append(sub, "")
			} else {
				sub = append(sub, js[loc[g*2]:loc[g*2+1]])
			}
		}
		tail := js[loc[1]:]
		if len(tail) > 8 {
			tail = tail[:8]
		}
		b.WriteString(repl(sub, tail))
		last = loc[1]
	}
	b.WriteString(js[last:])
	return b.String()
}

// browserRewriteJS 替换页面 JS（含内联）里的 location.* 为面板值 + 中和必崩写法。
// protocol/port 为**面板**的对应值。这些值在代理页面下本来就是面板域名；页面
// JS 拿它们拼 URL 时得到"面板根下的路径"（如 https://面板/cgi-bin/x），由
// handleOrphan 兜底重定向回最近站点的代理路径。此前替换为真实目标值反而引导
// JS 拼出 http://192.168.1.1/... 直连 URL——https 面板下被混合内容拦截 → 空白。
func browserRewriteJS(js string, panelScheme, panelHost string) string {
	vals := map[string]string{
		"hostname": `"` + panelHostname(panelHost) + `"`,
		"host":     `"` + panelHost + `"`,
		"origin":   `"` + panelScheme + "://" + panelHost + `"`,
		"protocol": `"` + panelScheme + `:"`,
		"port":     `"` + panelPort(panelHost) + `"`,
	}
	// 只改代码区：字符串/注释里的 location.* 一旦替换就是 SyntaxError
	// （实测 music.taihe.com、image.baidu.com 均因此整页脚本报废）
	js = browserReplaceCode(js, browserReJSHost, func(m []string, browserTail string) string {
		text := m[0]
		i := strings.LastIndex(text, ".")
		if i < 0 {
			return text
		}
		// 赋值左侧（location.host = x）不能换成字符串字面量，否则语法错误：
		// 原样保留（代理页面下就是一次合法的 location 赋值导航）
		if strings.HasPrefix(strings.TrimLeft(browserTail, " \t\r\n"), "=") &&
			!strings.HasPrefix(strings.TrimLeft(browserTail, " \t\r\n"), "==") {
			return text
		}
		key := strings.ToLower(text[i+1:])
		if v, has := vals[key]; has {
			return v
		}
		return text
	})
	return browserDefusePageJS(js)
}

// browserDefusePageJS 中和老页面两类"必崩"写法（光猫/路由器页面极常见）：
//  1. document.domain = "192.168.1.1" —— 老跨 frame 技巧，现代 Chrome 直接抛
//     "Failed to set document.domain"，整个 <script> 块中断，登录表单等后续
//     渲染全部消失；中和为无害注释。
//  2. top.location（读 href 比较或赋值跳转）—— top 是 fnOS 客户端顶层窗口
//     （跨源），读属性抛 SecurityError，赋值被 sandbox 静默阻止；统一改写为
//     self.location，同源可读不抛，且"想让顶层跳转"在代理场景下恰好应该
//     落回当前框架。
func browserDefusePageJS(js string) string {
	// 顺序：先把赋值改成"经代理"的导航（含 top.location 赋值），再中和剩余的
	// top.location 读取（跨源读抛 SecurityError），最后中和 document.domain 赋值。
	// 全部只在代码区生效（字符串/注释内跳过）。
	js = browserReplaceCode(js, browserReLocAssign, func(m []string, tail string) string {
		return `self.location.href=(window.__xbp?__xbp(` + m[1] + `):(` + m[1] + `));`
	})
	js = browserReplaceCode(js, browserReTopLoc, func(m []string, tail string) string {
		return "self.location"
	})
	js = browserReplaceCode(js, browserReDocDomain, func(m []string, tail string) string {
		return "/*xb*/"
	})
	return js
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
	dest := "/b/" + strings.Replace(site, "://", "/", 1) + r.URL.RequestURI()
	http.Redirect(w, r, dest, http.StatusTemporaryRedirect)
}

// browserRewriteHTML 全量重写 + 注入 base 与运行时脚本。
func browserRewriteHTML(doc string, tu *url.URL, panelScheme, panelHost string) []byte {
	doc = browserReAttr.ReplaceAllStringFunc(doc, func(m string) string { return browserRewriteAttr(m, tu) })
	doc = browserRewriteCSS(doc, tu) // <style> 块与 style 属性内的绝对 url()
	doc = browserReRefresh.ReplaceAllStringFunc(doc, func(m string) string { return browserRewriteRefreshValue(m, tu) })
	doc = browserRewriteJS(doc, panelScheme, panelHost) // 内联 <script> 与事件属性里的 location.host/hostname 等
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
		`window.onerror=function(m,s,l){try{parent.postMessage({__xbox:1,type:"jserr",url:location.href,msg:String(m).slice(0,200),line:l},"*");}catch(e){}return false;};` +
		`var P="/b/",H=` + hostLit + `,O=` + originLit + `;` +
		// px: 任意 URL → 代理路径（相对/非 http 原样）
		`function px(u){try{if(typeof u!=="string")return u;var s=u.replace(/^\s+|\s+$/g,"");if(!s)return u;` +
		`var l=s.toLowerCase();if(l.indexOf("javascript:")===0||l.indexOf("data:")===0||l.indexOf("mailto:")===0||l.indexOf("#")===0)return u;` +
		`if(s.indexOf("//")===0)s=location.protocol+s;` +
		`var m=/^(https?):\/\/([^\/]+)([\s\S]*)$/i.exec(s);if(!m)return u;` +
		`return P+m[1]+"/"+m[2]+((m[3]&&m[3].charAt(0)==="/")?m[3]:"/"+(m[3]||""));}catch(e){return u;}}` +
		// un: 代理路径 → 原始绝对 URL
		`function un(p){try{var m=/^\/b\/(https?)\/([^\/]+)([\s\S]*)$/.exec(String(p));if(m)return m[1]+"://"+m[2]+(m[3]||"/");}catch(e){}return String(p);}` +
		// norm: 任意形式（相对/代理路径/绝对/面板绝对）→ 原始目标绝对 URL（供宿主新标签用）
		`function norm(u){var s=String(u==null?"":u).replace(/^\s+|\s+$/g,"");if(!s)return "";` +
		`var i=s.indexOf("/b/");if(i>=0)return un(s.substring(i));` +
		`if(s.indexOf("//")===0)s=location.protocol+s;` +
		`if(/^https?:\/\//i.test(s)){var j=s.indexOf("/b/");return j>=0?un(s.substring(j)):s;}` +
		`try{var x=new URL(s,(document.baseURI||location.href)).href,k=x.indexOf("/b/");return k>=0?un(x.substring(k)):x;}catch(e){}return s;}` +
		`function msg(t,u){try{parent.postMessage({__xbox:1,type:t,url:norm(u)},"*");}catch(e){}}` +
		// 暴露给页面 JS：browserDefusePageJS 把页面里的 location 赋值改写成
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
