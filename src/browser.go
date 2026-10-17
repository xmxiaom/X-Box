package main

// 浏览器模块：同源网页代理（路径式 + <base> 注入）。
//
// 端点形态：/b/{scheme}/{host}/{path}?{query}，如
//   /b/http/192.168.1.1/            → http://192.168.1.1/
//   /b/https/example.com/a/b.html   → https://example.com/a/b.html
//
// 为什么是路径式 + base 而不是 /b?u=<url> + 属性重写：面板内 iframe 的文档
// URL 在 /b/{scheme}/{host}/ 之下，注入 <base href="当前代理路径"> 后，浏览器
// 对**一切相对 URL**——包括 JS 动态发起的 fetch/XHR、location 跳转、动态创建
// 的 img/script——都按 base 解析回代理路径。零 JS 改写即可让路由器管理页
// （192.168.1.1 等）这类大量使用相对路径 CGI 的页面正常工作（?u= 方案只重写
// 静态属性，JS 动态请求全部 404，是"页面显示不正常"的根因）。
//
// 其余设计：
//   - Cookie 双向透传（每请求独立 cookiejar 支持重定向链内种下的会话；回写给
//     浏览器的 Cookie 默认作用域限 /b/{scheme}/{host}/ 子树，天然按站点隔离），
//     路由器等需登录的站点可用；
//   - 保留响应原始 Content-Type（含 charset，GBK/GB2312 老页面不乱码）；
//   - 剥离响应头 X-Frame-Options / CSP 与 HTML 内 CSP meta（解除内嵌限制）；
//   - 非 HTML 一律流式透传（CSS 相对 url() 按 CSS 自身的代理 URL 解析，天然正确）；
//   - 顶级路径 /b（fnOS 远程中转只拦 /api/*，页面导航路径畅通）。
//
// 局限（轻量级定位，有意不做）：页面内写死绝对 http:// 的资源引用不经代理；
// 不改写 meta refresh。
//
// 安全模型：与终端模块一致——面板本身仅 fnOS 账号体系可达（127.0.0.1 +
// 局域网私网 IPv4 + fnOS 远程访问），代理不做额外鉴权/白名单：它是用户主动
// 输入网址的通用浏览工具，内网设备（192.168.1.1 等）是合法目标。

import (
	"html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	browserMaxHTML   = 8 << 20  // HTML 注入 base 的读取上限 8MB
	browserMaxStream = 64 << 20 // 其他内容透传上限 64MB
)

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

	var body io.Reader
	if r.Method == http.MethodPost && r.Body != nil {
		body = r.Body // POST 表单体原样转发（登录等场景）
	}
	// 每请求独立 cookiejar：重定向链内（如登录 302）种下的会话 Cookie 会随
	// 后续跳转请求自动携带；完成后回写给浏览器。
	jar, _ := cookiejar.New(nil)
	client := &http.Client{
		Timeout: 30 * time.Second,
		Jar:     jar,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 8 {
				return http.ErrUseLastResponse
			}
			return nil
		},
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, body)
	if err != nil {
		fail(w, 502, "构造请求失败")
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	if ct := r.Header.Get("Content-Type"); ct != "" && r.Method == http.MethodPost {
		req.Header.Set("Content-Type", ct)
	}
	// 入向 Cookie 透传（浏览器按 /b/{host} 子树自动携带，天然按站点隔离）
	if ck := r.Header.Get("Cookie"); ck != "" {
		req.Header.Set("Cookie", ck)
	}
	resp, err := client.Do(req)
	if err != nil {
		fail(w, 502, "访问失败："+err.Error())
		return
	}
	defer resp.Body.Close()

	// 剥离阻碍 iframe 内嵌的头（响应由面板同源提供，原站策略不适用于本响应）
	for _, h := range []string{"X-Frame-Options", "Content-Security-Policy", "Content-Security-Policy-Report-Only"} {
		resp.Header.Del(h)
	}
	// 出向 Cookie 回写（jar 只存 name=value；不加属性 → 浏览器默认作用域为
	// 当前代理路径目录 /b/{scheme}/{host}/，即只在该站点的代理请求中回传）
	finalURL := resp.Request.URL
	for _, c := range jar.Cookies(finalURL) {
		w.Header().Add("Set-Cookie", c.Name+"="+c.Value)
	}

	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml") {
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, browserMaxHTML))
		if rerr != nil {
			fail(w, 502, "读取响应失败："+rerr.Error())
			return
		}
		w.Header().Set("Content-Type", browserHTMLCT(ct)) // 保留原 charset（GBK 等不乱码）
		_, _ = w.Write(browserInjectBase(string(data), finalURL))
		return
	}
	// 其余类型：流式透传（CSS 相对 url() 按 CSS 自身代理 URL 解析，天然正确）
	if ct != "" {
		w.Header().Set("Content-Type", ct)
	}
	if resp.ContentLength >= 0 && resp.ContentLength <= browserMaxStream {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	_, _ = io.Copy(w, io.LimitReader(resp.Body, browserMaxStream))
}

// browserHTMLCT 保留原始 charset：原 Content-Type 已含 charset 原样返回；
// 缺失时仅补 text/html（不强制 utf-8，交由页面 meta/浏览器探测）。
func browserHTMLCT(ct string) string {
	if strings.Contains(strings.ToLower(ct), "charset") {
		return ct
	}
	return "text/html"
}

var (
	browserReBase    = regexp.MustCompile(`(?i)<base\b[^>]*>`)
	browserReHead    = regexp.MustCompile(`(?i)<head[^>]*>`)
	browserReHTMLTag = regexp.MustCompile(`(?i)<html[^>]*>`)
	browserReCSPMeta = regexp.MustCompile(`(?i)<meta[^>]+http-equiv\s*=\s*("content-security-policy"|'content-security-policy'|content-security-policy)[^>]*>`)
)

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

// browserInjectBase 去掉原有 <base> 与 CSP meta，注入指向当前代理路径的
// <base>（注入点：<head> 后 > <html> 后 > 文档最前）。base 值为 ASCII，任何
// 字符集下注入均安全。
func browserInjectBase(doc string, finalURL *url.URL) []byte {
	doc = browserReBase.ReplaceAllString(doc, "")
	doc = browserReCSPMeta.ReplaceAllString(doc, "")
	tag := `<base href="` + html.EscapeString(browserURLPath(finalURL)) + `">`
	if m := browserReHead.FindStringIndex(doc); m != nil {
		pos := m[1]
		return []byte(doc[:pos] + tag + doc[pos:])
	}
	if m := browserReHTMLTag.FindStringIndex(doc); m != nil {
		pos := m[1]
		return []byte(doc[:pos] + tag + doc[pos:])
	}
	return []byte(tag + doc)
}
