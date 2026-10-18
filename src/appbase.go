package main

// fnOS 统一网关接入。
//
// 官方契约：应用监听 ${TRIM_APPDEST}/app.sock（unix socket），网关先校验飞牛账号
// 登录态，再把 /app/{appname}/... 的请求转发进来，并附带 X-Trim-Userid 等身份头。
// 这样应用不占 TCP 端口、不暴露到局域网，外部入口（官方域名 / 自有域名 / IPv6）
// 全部由网关统一承接。
//
// 代价是：应用收到的路径都带前缀，服务端要剥掉它再路由，同时**所有对外生成的
// 绝对路径都要补回前缀**（否则浏览器请求的地址网关不认识）。

import (
	"net/http"
	"os"
	"strings"
)

// appBase 网关前缀，例如 "/app/X-Box"；为空表示非网关模式（局域网直连）。
var appBase string

// initAppBase 从环境变量读取网关前缀（由启动脚本按 TRIM_APPNAME 注入）。
func initAppBase() {
	v := strings.TrimSpace(os.Getenv("XBOX_BASE"))
	if v == "" {
		return
	}
	if !strings.HasPrefix(v, "/") {
		v = "/" + v
	}
	appBase = strings.TrimSuffix(v, "/")
}

// stripAppBase 剥掉网关前缀，把 /app/X-Box/xxx 还原成 /xxx。
// 只处理精确前缀，避免误伤名字相近的路径。
func stripAppBase(r *http.Request) {
	if appBase == "" {
		return
	}
	p := r.URL.Path
	if p == appBase {
		r.URL.Path = "/"
		r.URL.RawPath = ""
		return
	}
	if strings.HasPrefix(p, appBase+"/") {
		r.URL.Path = strings.TrimPrefix(p, appBase)
		r.URL.RawPath = ""
	}
}
