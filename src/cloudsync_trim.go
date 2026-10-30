package main

// 调用 fnOS 自己的「刷新挂载缓存」接口，等价于点文件管理里的刷新按钮。
//
// 从前端 JS 逆向得到：传输统一走 POST /cgi，请求体 {method, params}，
// 鉴权头 Authorization: trim <token>；刷新方法名 appcgi.mountmgr.cache.refresh。
// 这个 token 是 fnOS 网页登录后存在浏览器里的，应用拿不到，所以需要用户
// 在同步页粘贴一次（保存在 XBOX_DATA/trim-token，权限 0600）。
//
// 这是唯一能真正让 fnOS 网关重新向网盘拉目录的动作——SIGHUP 与 readdir 都被
// 网关缓存挡住（实测无效）。

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var trimMu sync.Mutex

func trimTokenPath() string {
	dir := os.Getenv("XBOX_DATA")
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "trim-token")
}

func loadTrimToken() string {
	b, err := os.ReadFile(trimTokenPath())
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

func saveTrimToken(tok string) error {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		if err := os.Remove(trimTokenPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	return os.WriteFile(trimTokenPath(), []byte(tok), 0o600)
}

func trimAddr() string {
	if v := strings.TrimSpace(os.Getenv("XBOX_TRIM_ADDR")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:8000"
}

// trimRefreshMount 请求 fnOS 刷新指定挂载路径的目录缓存。
// 返回 (是否成功, 说明)。说明会显示在面板上，便于判断是令牌问题还是参数问题。
func trimRefreshMount(p string) (bool, string) {
	tok := loadTrimToken()
	if tok == "" {
		return false, "未配置 fnOS 令牌"
	}
	trimMu.Lock()
	defer trimMu.Unlock()

	// 参数形态未知，按最可能到最宽松依次尝试，成功即止
	bodies := []string{
		fmt.Sprintf(`{"method":"appcgi.mountmgr.cache.refresh","params":{"path":%q}}`, p),
		fmt.Sprintf(`{"method":"appcgi.mountmgr.cache.refresh","params":{"paths":[%q]}}`, p),
		`{"method":"appcgi.mountmgr.cache.refresh","params":{}}`,
	}
	cl := &http.Client{Timeout: 8 * time.Second}
	lastMsg := ""
	for i, body := range bodies {
		req, err := http.NewRequest("POST", trimAddr()+"/cgi", strings.NewReader(body))
		if err != nil {
			continue
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "trim "+tok)
		resp, err := cl.Do(req)
		if err != nil {
			return false, "连接 fnOS 接口失败：" + err.Error()
		}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 8192))
		_ = resp.Body.Close()
		text := strings.TrimSpace(string(b))
		low := strings.ToLower(text)
		if resp.StatusCode == 200 && !strings.Contains(low, "invalid token") &&
			!strings.Contains(low, `"error"`) && !strings.Contains(low, `"code":-`) {
			if i > 0 {
				return true, fmt.Sprintf("已用 fnOS 令牌刷新挂载（参数形态 %d）", i+1)
			}
			return true, "已用 fnOS 令牌刷新挂载"
		}
		lastMsg = text
		if strings.Contains(low, "invalid token") {
			return false, "fnOS 令牌无效或已过期，请重新粘贴"
		}
	}
	if len(lastMsg) > 120 {
		lastMsg = lastMsg[:120]
	}
	return false, "fnOS 刷新接口返回异常：" + lastMsg
}
