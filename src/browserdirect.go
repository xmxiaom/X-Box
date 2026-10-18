package main

// 直通模式（独立端口、路径不加前缀）。
//
// 为什么需要：路径式代理把页面放在 /b/{ver}/{scheme}/{host}/ 下，而不少前后端
// 分离的应用用 window.location.pathname 做路由（react-router / vue-router，
// basename 固定 "/"），前缀会让路由表全部不匹配，页面只能渲染应用自己的 404。
// 这类站点必须让它的 pathname 保持原样，也就是"根路径直通"。
//
// 站点选择：浏览器 Cookie `xbox_site`（值形如 http://host:port，URL 编码）。
// 同一时刻只服务一个站点——直通端口上所有路径都属于它；多站点并行仍走
// 路径式代理（8899）。该端口仅局域网可达，远程访问经 fnOS 域名时用不上。

import (
	"net"
	"net/http"
	"net/url"
)

const directPort = "8900"

// directAddrs 直通端口监听地址：回环 + 局域网私网 IPv4（与主面板一致）。
func directAddrs() []string {
	addrs := []string{"127.0.0.1:" + directPort}
	if ip := lanIPv4(); ip != "" {
		addrs = append(addrs, net.JoinHostPort(ip, directPort))
	}
	return addrs
}

// directSite 从 Cookie 取当前直通站点（"http://host:port"）。
func directSite(r *http.Request) string {
	c, err := r.Cookie("xbox_site")
	if err != nil || c.Value == "" {
		return ""
	}
	v, uerr := url.QueryUnescape(c.Value)
	if uerr != nil {
		v = c.Value
	}
	u, perr := url.Parse(v)
	if perr != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return ""
	}
	return u.Scheme + "://" + u.Host
}

// handleDirectProxy 直通模式入口：路径原样转发到 Cookie 指定的站点。
func (s *Server) handleDirectProxy(w http.ResponseWriter, r *http.Request) {
	site := directSite(r)
	if site == "" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(directHelpPage))
		return
	}
	u, _ := url.Parse(site)
	if browserIsPanelHost(u.Host, r) {
		fail(w, 400, "不能通过直通模式访问面板自身")
		return
	}
	target := site + r.URL.RequestURI()
	tu, terr := url.Parse(target)
	if terr != nil || tu.Host == "" {
		fail(w, 400, "目标地址非法")
		return
	}
	s.proxyCore(w, r, target, tu, true)
}

const directHelpPage = `<!doctype html><html lang="zh-CN"><head><meta charset="utf-8">
<title>直通模式</title><style>
body{font:14px/1.8 system-ui,-apple-system,"Microsoft YaHei",sans-serif;color:#374151;background:#fff;margin:0;padding:28px}
.box{max-width:620px;margin:0 auto;border:1px solid #E5E7EB;border-radius:10px;padding:20px 22px;background:#FAFAFA}
h2{margin:0 0 10px;font-size:16px;color:#111827}
code{background:#F3F4F6;padding:2px 6px;border-radius:4px;font-family:ui-monospace,Consolas,monospace}
p{margin:8px 0}
</style></head><body><div class="box">
<h2>直通模式</h2>
<p>此端口用于打开"按路径路由"的单页应用（如设备自带的桌面/管理界面）。这类页面在
普通代理路径下会路由失配，只能以根路径运行。</p>
<p>用法：在浏览器的「地址栏」里打开目标站点后，点工具栏的「桌面模式」按钮，
或在控制台设置 Cookie：</p>
<p><code>document.cookie='xbox_site='+encodeURIComponent('http://目标主机:端口')+';path=/'</code></p>
<p>注意：直通端口只在一台设备上生效（同一时刻只服务一个站点），且仅局域网可达。</p>
</div></body></html>`
