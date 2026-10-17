package main

// 浏览器模块：同源网页代理（轻量级内嵌浏览）。
//
// 为什么端点是顶级路径 /b 而不是 /api/*：fnOS 远程客户端 iframe 环境下，
// 中转只拦 /api/*（挂起/跨域重定向），普通页面导航路径畅通（隧道通道即依赖
// 此特性）。面板内 iframe 直接以页面导航方式加载 /b?u=<目标网址>。
//
// 同源代理解决两类直连 iframe 必挂的问题：
//   1. 目标站点 X-Frame-Options / CSP frame-ancestors 禁止内嵌；
//   2. 面板经远程域名是 https，直连 http 站点（如 192.168.1.1 路由器）被
//      混合内容策略拦截。
//
// 实现要点：
//   - 服务端转发 GET/POST，跟随重定向，30s 超时，读取上限保护；
//   - HTML：重写 a[href]/form[action]/src/poster/srcset 为 /b?u=<绝对地址>，
//     后续导航与子资源都经本代理；非 HTML/CSS 按原 Content-Type 流式透传；
//   - CSS：重写 url(...) 与 @import（相对地址按该 CSS 自身 URL 解析）；
//   - 剥离 X-Frame-Options / CSP 等阻碍内嵌的响应头；不转发 Cookie。
//
// 局限（轻量级定位，有意不做）：页面 JS 动态加载的资源与 fetch/XHR 不经代理；
// meta refresh 不改写；不维护目标站点会话（需登录的站点请用「新窗口打开」
// 直连或系统浏览器）。
//
// 安全模型：与终端模块一致——面板本身仅 fnOS 账号体系可达（127.0.0.1 +
// 局域网私网 IPv4 + fnOS 远程访问），代理不做额外鉴权/白名单：它是用户主动
// 输入网址的通用浏览工具，内网设备（192.168.1.1 等）是合法目标。

import (
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var browserClient = &http.Client{
	Timeout: 30 * time.Second,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 8 {
			return http.ErrUseLastResponse
		}
		return nil
	},
}

const (
	browserMaxRewrite = 8 << 20  // HTML/CSS 重写读取上限 8MB（超出截断透传）
	browserMaxStream  = 64 << 20 // 其他内容透传上限 64MB
)

// handleBrowserProxy GET/POST /b?u=<目标网址>：同源网页代理。
// u 之外的查询参数透传给目标（支持 GET 表单：/b?u=X&k=v → 目标 /?k=v）。
func (s *Server) handleBrowserProxy(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	raw := strings.TrimSpace(q.Get("u"))
	if raw == "" {
		fail(w, 400, "缺少网址参数 u")
		return
	}
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		raw = "http://" + raw // 允许省略协议（与面板地址栏行为一致）
	}
	target, err := url.Parse(raw)
	if err != nil || target.Host == "" {
		fail(w, 400, "网址格式错误")
		return
	}
	// GET 表单字段透传（u 之外的查询参数拼到目标 URL）
	tgt := *target
	if len(q) > 1 {
		tq := tgt.Query()
		for k, vs := range q {
			if k == "u" {
				continue
			}
			for _, v := range vs {
				tq.Add(k, v)
			}
		}
		tgt.RawQuery = tq.Encode()
	}

	var body io.Reader
	if r.Method == http.MethodPost && r.Body != nil {
		body = r.Body // POST 表单体原样转发（登录等场景）
	}
	req, err := http.NewRequestWithContext(r.Context(), r.Method, tgt.String(), body)
	if err != nil {
		fail(w, 502, "构造请求失败")
		return
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0 Safari/537.36")
	req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8")
	if ct := r.Header.Get("Content-Type"); ct != "" && r.Method == http.MethodPost {
		req.Header.Set("Content-Type", ct)
	}
	resp, err := browserClient.Do(req)
	if err != nil {
		fail(w, 502, "访问失败："+err.Error())
		return
	}
	defer resp.Body.Close()

	// 剥离阻碍 iframe 内嵌的头（响应由面板同源提供，原站策略不适用于本响应）
	for _, h := range []string{"X-Frame-Options", "Content-Security-Policy", "Content-Security-Policy-Report-Only"} {
		resp.Header.Del(h)
	}

	ct := resp.Header.Get("Content-Type")
	finalURL := resp.Request.URL // 重定向后的最终地址（相对链接解析基准）

	if strings.Contains(ct, "text/html") || strings.Contains(ct, "application/xhtml") {
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, browserMaxRewrite))
		if rerr != nil {
			fail(w, 502, "读取响应失败："+rerr.Error())
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write(browserRewriteHTML(string(data), finalURL))
		return
	}
	if strings.Contains(ct, "text/css") {
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, browserMaxRewrite))
		if rerr != nil {
			fail(w, 502, "读取响应失败："+rerr.Error())
			return
		}
		w.Header().Set("Content-Type", ct)
		_, _ = w.Write([]byte(browserRewriteCSS(string(data), finalURL)))
		return
	}
	// 其余类型：流式透传（图片/JS/JSON 等）
	w.Header().Set("Content-Type", ct)
	if resp.ContentLength >= 0 && resp.ContentLength <= browserMaxStream {
		w.Header().Set("Content-Length", strconv.FormatInt(resp.ContentLength, 10))
	}
	_, _ = io.Copy(w, io.LimitReader(resp.Body, browserMaxStream))
}

// browserProxyURL 把 ref（相对或绝对）解析为经本代理的 URL；不应代理的
// （javascript:/data:/mailto:/tel:/blob:/about:/纯锚点/非 http(s) 目标）返回 false。
func browserProxyURL(base *url.URL, ref string) (string, bool) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", false
	}
	low := strings.ToLower(ref)
	for _, p := range []string{"javascript:", "data:", "mailto:", "tel:", "blob:", "about:", "#"} {
		if strings.HasPrefix(low, p) {
			return "", false
		}
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "", false
	}
	abs := base.ResolveReference(u)
	if abs.Scheme != "http" && abs.Scheme != "https" || abs.Host == "" {
		return "", false
	}
	return "/b?u=" + url.QueryEscape(abs.String()), true
}

// HTML 属性重写：prefix + 引号值（三种引号形态捕获其一）。
var (
	browserReHref  = regexp.MustCompile(`(?i)(<a\b[^>]*?\bhref\s*=\s*)("([^"]*)"|'([^']*)'|([^\s">]+))`)
	browserReForm  = regexp.MustCompile(`(?i)(<form\b[^>]*?\baction\s*=\s*)("([^"]*)"|'([^']*)'|([^\s">]+))`)
	browserReSrc   = regexp.MustCompile(`(?i)(\b(?:src|poster)\s*=\s*)("([^"]*)"|'([^']*)'|([^\s">]+))`)
	browserReSrcst = regexp.MustCompile(`(?i)(\bsrcset\s*=\s*)("([^"]*)"|'([^']*)'|([^\s">]+))`)
	browserReCSSU  = regexp.MustCompile(`(?i)url\(\s*('([^']*)'|"([^"]*)"|([^)'"]+))\s*\)`)
	browserReImp   = regexp.MustCompile(`(?i)@import\s+("([^"]*)"|'([^']*)')`)
)

// browserAttrVal 取属性值（捕获组 3/4/5 之一）。
func browserAttrVal(sub []string) string {
	for _, g := range []int{3, 4, 5} {
		if sub[g] != "" {
			return sub[g]
		}
	}
	return ""
}

// browserRewriteAttr 按正则重写属性值（统一改为双引号包裹的代理 URL）。
func browserRewriteAttr(doc string, re *regexp.Regexp, base *url.URL) string {
	return re.ReplaceAllStringFunc(doc, func(m string) string {
		sub := re.FindStringSubmatch(m)
		pv, ok2 := browserProxyURL(base, html.UnescapeString(browserAttrVal(sub)))
		if !ok2 {
			return m
		}
		return sub[1] + `"` + pv + `"`
	})
}

// browserRewriteSrcset 重写 srcset（逗号分隔多候选，仅替换每段的 URL 部分）。
func browserRewriteSrcset(doc string, re *regexp.Regexp, base *url.URL) string {
	return re.ReplaceAllStringFunc(doc, func(m string) string {
		sub := re.FindStringSubmatch(m)
		val := html.UnescapeString(browserAttrVal(sub))
		parts := strings.Split(val, ",")
		out := make([]string, 0, len(parts))
		for _, p := range parts {
			fields := strings.Fields(strings.TrimSpace(p))
			if len(fields) == 0 {
				continue
			}
			if pv, ok2 := browserProxyURL(base, fields[0]); ok2 {
				fields[0] = pv
			}
			out = append(out, strings.Join(fields, " "))
		}
		if len(out) == 0 {
			return m
		}
		return sub[1] + `"` + html.EscapeString(strings.Join(out, ", ")) + `"`
	})
}

// browserRewriteCSS 重写 CSS 中的 url(...) 与 @import（相对地址按该 CSS 自身 URL 解析）。
func browserRewriteCSS(css string, base *url.URL) string {
	css = browserReCSSU.ReplaceAllStringFunc(css, func(m string) string {
		sub := browserReCSSU.FindStringSubmatch(m)
		val := sub[2]
		if val == "" {
			val = sub[3]
		}
		if val == "" {
			val = sub[4]
		}
		if pv, ok2 := browserProxyURL(base, val); ok2 {
			return `url("` + pv + `")`
		}
		return m
	})
	css = browserReImp.ReplaceAllStringFunc(css, func(m string) string {
		sub := browserReImp.FindStringSubmatch(m)
		val := sub[2]
		if val == "" {
			val = sub[3]
		}
		if pv, ok2 := browserProxyURL(base, val); ok2 {
			return `@import "` + pv + `"`
		}
		return m
	})
	return css
}

// browserRewriteHTML 重写 HTML 文档中的导航与资源引用。
func browserRewriteHTML(doc string, base *url.URL) []byte {
	doc = browserRewriteAttr(doc, browserReHref, base)  // 链接 → 代理（保持站内浏览）
	doc = browserRewriteAttr(doc, browserReForm, base)  // 表单 → 代理（GET 字段/POST 体均透传）
	doc = browserRewriteSrcset(doc, browserReSrcst, base)
	doc = browserRewriteAttr(doc, browserReSrc, base) // 图片/脚本/iframe 等资源 → 代理（规避混合内容）
	return []byte(doc)
}
