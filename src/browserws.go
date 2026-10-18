package main

// WebSocket 转发。
//
// 为什么需要：不少管理面板把主通道放在常驻 WebSocket 上（飞牛桌面就是
// ws://<面板>/websocket?type=main）。不转发的话，页面能渲染出框架，但会一直
// 弹"连接已断开"。这里只做 TCP 层透传（握手 + 双向字节流），不解析帧。

import (
	"bufio"
	"crypto/tls"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// browserIsWSUpgrade 判断是否 WebSocket 升级请求。
func browserIsWSUpgrade(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// browserProxyWS 把 WebSocket 连接转发到目标站。
func (s *Server) browserProxyWS(w http.ResponseWriter, r *http.Request, target string, tu *url.URL) {
	addr := tu.Host
	if tu.Port() == "" {
		if tu.Scheme == "https" {
			addr = net.JoinHostPort(tu.Hostname(), "443")
		} else {
			addr = net.JoinHostPort(tu.Hostname(), "80")
		}
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	var (
		conn net.Conn
		err  error
	)
	if tu.Scheme == "https" {
		conn, err = tls.DialWithDialer(d, "tcp", addr, &tls.Config{InsecureSkipVerify: true})
	} else {
		conn, err = d.Dial("tcp", addr)
	}
	if err != nil {
		fail(w, 502, "WebSocket 连接目标失败："+browserErrText(err))
		return
	}
	defer conn.Close()

	// 用 origin-form 重放握手（r.Write 走 r.URL.RequestURI()）。
	// 注意 Path 必须换成目标侧路径：r.URL 里是面板路径 /b/{ver}/{scheme}/{host}/...
	// 只改 Scheme/Host 不改 Path，目标站会收到带代理前缀的路径 → 404。
	req := r.Clone(r.Context())
	req.URL.Scheme = tu.Scheme
	req.URL.Host = tu.Host
	req.URL.Path = tu.Path
	req.URL.RawPath = tu.RawPath
	req.URL.RawQuery = tu.RawQuery
	req.Host = tu.Host
	req.RequestURI = ""
	req.Body = http.NoBody
	req.ContentLength = 0
	req.Header.Del("Accept-Encoding")
	if err := req.Write(conn); err != nil {
		fail(w, 502, "WebSocket 握手写入失败："+err.Error())
		return
	}

	br := bufio.NewReader(conn)
	resp, rerr := http.ReadResponse(br, req)
	if rerr != nil {
		fail(w, 502, "WebSocket 握手失败："+browserErrText(rerr))
		return
	}
	// 未升级（例如 401/403/404）：按普通 HTTP 响应回传，便于页面自行处理
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	cconn, _, herr := http.NewResponseController(w).Hijack()
	if herr != nil {
		fail(w, 502, "WebSocket 无法接管连接："+herr.Error())
		return
	}
	defer cconn.Close()

	var sb strings.Builder
	sb.WriteString("HTTP/1.1 101 Switching Protocols\r\n")
	_ = resp.Header.Write(&sb)
	sb.WriteString("\r\n")
	if _, werr := cconn.Write([]byte(sb.String())); werr != nil {
		return
	}

	// 双向透传；任一侧结束即收尾。br 里可能已有目标先发来的字节，必须一起读。
	done := make(chan struct{}, 2)
	go func() { _, _ = io.Copy(conn, cconn); done <- struct{}{} }()
	go func() { _, _ = io.Copy(cconn, io.MultiReader(br, conn)); done <- struct{}{} }()
	<-done
}
