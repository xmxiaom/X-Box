package main

// 让 fnOS 自己的挂载服务重新向网盘拉取目录（等价于文件管理里点一次刷新）。
//
// 为什么需要它：网盘挂载链路是「rclone mount → fnOS 本机 WebDAV 接入 → 网盘」。
// 目录缓存压在中间那层接入服务里，所以只让 rclone 重新列目录（vfs/refresh）
// 拿到的仍是接入服务缓存下来的旧列表——这就是「到点了却看不到新转存文件」的原因。
// 能真正让接入服务重拉的，只有它自己的刷新动作。
//
// 做法：在设备本机直接请求平台的服务接口，不涉及任何账号登录。
// 本机调用成功即说明目录已被强制刷新；不可用时返回原因，交给上层退回其它方式。

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

var (
	fnosMu       sync.Mutex
	fnosDeadAt   time.Time // 连续不可用时，隔一段时间再试，避免每轮都打接口
	fnosLastNote string
)

// fnosLocalAddrs 本机平台服务的候选地址。端口不固定（不同版本/配置不同），
// 可用 XBOX_FNOS_ADDR 覆盖。
func fnosLocalAddrs() []string {
	if v := strings.TrimSpace(os.Getenv("XBOX_FNOS_ADDR")); v != "" {
		return []string{strings.TrimRight(v, "/")}
	}
	return []string{
		"http://127.0.0.1:8000",
		"http://127.0.0.1:5666",
		"http://127.0.0.1",
	}
}

// fnosLocalCall 在本机调平台服务接口，返回原始响应体。
func fnosLocalCall(base, method string, params map[string]any, timeout time.Duration) (string, error) {
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
// 只在设备本机生效；调用方在失败时应退回挂载服务自身的刷新。
//
// 注意控制开销：三个候选地址各最多试一次（先探空参数，明确被拒就不再纠缠），
// 单次超时 3 秒，最坏几秒内出结论，不会拖慢同步。
func refreshViaPlatform() (bool, string) {
	fnosMu.Lock()
	defer fnosMu.Unlock()
	if time.Now().Before(fnosDeadAt) {
		return false, fnosLastNote // 冷却中，直接复用上次结论
	}

	// 参数名各版本不一致：先试空参数，只有「不是鉴权拒绝」时才继续换参数重试。
	variants := []map[string]any{{}, {"path": "/"}, {"force": true}}
	var last string
	for _, base := range fnosLocalAddrs() {
		denied := false
		for i, p := range variants {
			body, err := fnosLocalCall(base, "appcgi.mountmgr.cache.refresh", p, 3*time.Second)
			if err != nil {
				last = firstN(err.Error(), 80)
				break // 地址不通，换下一个地址
			}
			low := strings.ToLower(body)
			if strings.Contains(low, "invalid token") || strings.Contains(low, "unauthorized") {
				last = "平台接口要求登录凭证"
				denied = true
				break
			}
			if strings.Contains(low, "\"code\":0") || strings.Contains(low, "\"code\": 0") {
				fnosLastNote = "平台已刷新"
				return true, fnosLastNote
			}
			last = firstN(strings.TrimSpace(body), 80)
			if i == 0 && !strings.Contains(low, "param") && !strings.Contains(low, "invalid") {
				break // 不是参数问题，换参数也没用
			}
		}
		if !denied && last != "" {
			continue
		}
	}
	if last == "" {
		last = "本机平台服务不可达"
	}
	fnosLastNote = "平台刷新不可用：" + last
	fnosDeadAt = time.Now().Add(10 * time.Minute) // 冷却 10 分钟后再试
	return false, fnosLastNote
}

func firstN(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}
