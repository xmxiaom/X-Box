package main

// 让平台自身的挂载服务重新向网盘拉取目录（等价于文件管理里点一次刷新）。
//
// 为什么必须要它：网盘挂载链路是「挂载服务(rclone) → 平台本机网盘接入(WebDAV) → 网盘」。
// 目录缓存压在中间那层接入服务里，只让挂载服务重列目录，拿到的仍是接入服务缓存下来的
// 旧列表——这就是「到点了却看不到新转存文件」的原因。能真正让它重拉的只有它自己的刷新动作。
//
// 凭据从哪来：应用页面本身是经平台网关访问的，网关转发时会带上当前会话的鉴权信息。
// 应用直接复用这一份（不要求用户再登录、也不保存任何账号密码）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	fnosMu       sync.Mutex
	fnosDeadAt   time.Time // 连续不可用时冷却，避免每轮都打接口
	fnosLastNote string

	gwAuthMu sync.RWMutex
	gwAuth   string   // 网关透传的鉴权头（如 "trim <token>"），空表示没拿到
	gwSeen   []string // 见过的鉴权类请求头名（诊断用，不含内容）
)

// captureGatewayAuth 记录平台网关转发请求时携带的鉴权信息。
// 只在自身还没拿到时记录一次，开销可以忽略。
func captureGatewayAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gwAuthMu.Lock()
		if gwAuth == "" {
			if v := gatewayTokenFrom(r); v != "" {
				gwAuth = v
			}
			for name := range r.Header {
				ln := strings.ToLower(name)
				if strings.Contains(ln, "token") || strings.Contains(ln, "auth") || ln == "cookie" {
					if !hasStr(gwSeen, name) {
						gwSeen = append(gwSeen, name)
					}
				}
			}
		}
		gwAuthMu.Unlock()
		next.ServeHTTP(w, r)
	})
}

// gatewayTokenFrom 从请求里取出可用的鉴权头
func gatewayTokenFrom(r *http.Request) string {
	for _, name := range []string{"Authorization", "X-Trim-Token", "X-Auth-Token", "X-Access-Token", "X-Token"} {
		v := strings.TrimSpace(r.Header.Get(name))
		if v == "" {
			continue
		}
		low := strings.ToLower(v)
		if strings.HasPrefix(low, "trim ") || strings.HasPrefix(low, "bearer ") {
			return v
		}
		if len(v) > 20 && !strings.Contains(v, " ") {
			return "trim " + v // 裸令牌
		}
	}
	if c, err := r.Cookie("token"); err == nil && c.Value != "" {
		return "trim " + c.Value
	}
	return ""
}

func hasStr(list []string, s string) bool {
	for _, v := range list {
		if v == s {
			return true
		}
	}
	return false
}

// fnosLocalAddrs 平台服务的候选地址：从本机实际监听的端口里取，不预设固定端口。
// 端口会随版本/部署方式变化，写死某个端口在换环境后必然失效。
// 可用 XBOX_FNOS_ADDR 直接指定（跳过探测）。
func fnosLocalAddrs() []string {
	if v := strings.TrimSpace(os.Getenv("XBOX_FNOS_ADDR")); v != "" {
		return []string{strings.TrimRight(v, "/")}
	}
	ports := localListenPorts()
	if len(ports) > 25 { // 逐个探测有成本
		ports = ports[:25]
	}
	out := make([]string, 0, len(ports))
	for _, p := range ports {
		out = append(out, fmt.Sprintf("http://127.0.0.1:%d", p))
	}
	return out
}

// fnosLocalCall 在本机调平台接口，返回原始响应体。
func fnosLocalCall(base, method string, params map[string]any, auth string, timeout time.Duration) (string, error) {
	if params == nil {
		params = map[string]any{}
	}
	payload, err := json.Marshal(map[string]any{"method": method, "params": params})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequest("POST", base+"/cgi", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	cl := &http.Client{Timeout: timeout}
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return string(raw), nil
}

// refreshViaPlatform 请求平台刷新挂载目录，返回是否成功与一句诊断。
//
// 开销控制：单次超时 3 秒，明确被拒就立刻换下一个候选，最坏几秒内出结论。
// 失败后冷却 10 分钟再试，不会拖慢同步。
func refreshViaPlatform() (bool, string) {
	fnosMu.Lock()
	defer fnosMu.Unlock()
	if time.Now().Before(fnosDeadAt) {
		return false, fnosLastNote
	}

	gwAuthMu.RLock()
	auth := gwAuth
	seen := append([]string(nil), gwSeen...)
	gwAuthMu.RUnlock()

	// 参数名各版本不一致：先试空参数，只有「不是鉴权拒绝」时才换参数再试。
	variants := []map[string]any{{}, {"path": "/"}, {"force": true}}
	var last string
	for _, base := range fnosLocalAddrs() {
		for i, p := range variants {
			body, err := fnosLocalCall(base, "appcgi.mountmgr.cache.refresh", p, auth, 3*time.Second)
			if err != nil {
				last = firstN(err.Error(), 80)
				break // 地址不通，换下一个地址
			}
			low := strings.ToLower(body)
			if strings.Contains(low, "invalid token") || strings.Contains(low, "unauthorized") {
				if auth == "" {
					last = "平台接口需要鉴权，网关未透传凭据"
					if len(seen) > 0 {
						last += "（仅见请求头：" + strings.Join(seen, ",") + "）"
					}
				} else {
					last = "平台接口拒绝了透传凭据"
				}
				break
			}
			if strings.Contains(low, `"code":0`) || strings.Contains(low, `"code": 0`) {
				fnosLastNote = "平台已刷新"
				return true, fnosLastNote
			}
			last = firstN(strings.TrimSpace(body), 80)
			if i == 0 && !strings.Contains(low, "param") && !strings.Contains(low, "invalid") {
				break // 与参数无关，换参数也没用
			}
		}
	}
	if last == "" {
		last = "本机平台服务不可达"
	}
	fnosLastNote = "平台刷新不可用：" + last
	fnosDeadAt = time.Now().Add(10 * time.Minute)
	return false, fnosLastNote
}

func firstN(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
