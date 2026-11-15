package main

// 平台访问令牌：用于调用平台自己的「刷新挂载目录缓存」接口。
//
// 为什么需要它：网盘挂载的目录列表缓存在平台接入层里，应用侧所有手段
// （刷新挂载进程缓存、直连列目录）拿到的都是缓存那份；只有平台自己的刷新
// 动作能让它重新向网盘拉取。该接口需要平台签发的访问令牌。
//
// 令牌只保存在本机应用数据目录（0600），只用于本机接口调用，不外发、不写日志。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

var (
	fnosTokMu  sync.RWMutex
	fnosTok    string
	fnosTokOK  bool   // 上次验证是否通过
	fnosTokMsg string // 上次验证结论
	// 平台刷新接口的参数形式各版本不同，验证时逐个试，成功一次就记住
	fnosRefreshParam = map[string]any{}
)

func fnosTokenPath() string {
	return filepath.Join(dataDirPath(), "fnos-token")
}

// dataDirPath 应用数据目录（与 cloud-sync.json 等同处）
func dataDirPath() string {
	if v := strings.TrimSpace(os.Getenv("XBOX_DATA")); v != "" {
		return v
	}
	return "."
}

func loadFnosToken() string {
	fnosTokMu.RLock()
	if fnosTok != "" || fnosTokOK || fnosTokMsg != "" {
		v := fnosTok
		fnosTokMu.RUnlock()
		return v
	}
	fnosTokMu.RUnlock()

	b, err := os.ReadFile(fnosTokenPath())
	if err != nil {
		return ""
	}
	tok := strings.TrimSpace(string(b))
	fnosTokMu.Lock()
	fnosTok = tok
	fnosTokMu.Unlock()
	return tok
}

func saveFnosToken(tok string) error {
	tok = strings.TrimSpace(tok)
	if err := os.MkdirAll(dataDirPath(), 0o755); err != nil {
		return err
	}
	p := fnosTokenPath()
	if tok == "" {
		_ = os.Remove(p)
		fnosTokMu.Lock()
		fnosTok, fnosTokOK, fnosTokMsg = "", false, ""
		fnosTokMu.Unlock()
		return nil
	}
	if err := os.WriteFile(p, []byte(tok), 0o600); err != nil {
		return err
	}
	fnosTokMu.Lock()
	fnosTok = tok
	fnosTokMu.Unlock()
	return nil
}

// fnosCGI 在本机调平台接口，返回原始响应体
func fnosCGI(base, token, method string, params map[string]any) (string, error) {
	if params == nil {
		params = map[string]any{}
	}
	payload, _ := json.Marshal(map[string]any{"method": method, "params": params})
	req, err := http.NewRequest("POST", base+"/cgi", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "trim "+token)
	cl := &http.Client{Timeout: 8 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	return string(raw), nil
}

// fnosRespCode 取平台响应里的业务码（0 表示成功）
func fnosRespCode(body string) (int, bool) {
	var m struct {
		Code *int   `json:"code"`
		Msg  string `json:"msg"`
	}
	if err := json.Unmarshal([]byte(body), &m); err != nil || m.Code == nil {
		return 0, false
	}
	return *m.Code, true
}

// verifyFnosToken 验证令牌是否可用，并试出「刷新挂载缓存」接口能接受的参数形式。
// 只调用只读接口与刷新接口，不做任何会改变数据的操作。
func verifyFnosToken(tok string) (bool, string) {
	tok = strings.TrimSpace(tok)
	if tok == "" {
		return false, "未填写令牌"
	}

	var lastErr string
	for _, base := range fnosLocalAddrs() {
		body, err := fnosCGI(base, tok, "appcgi.mountmgr.listmounts", nil)
		if err != nil {
			lastErr = "连不上平台服务"
			continue
		}
		low := strings.ToLower(body)
		if strings.Contains(low, "invalid token") || strings.Contains(low, "unauthorized") {
			return false, "令牌无效或已过期"
		}
		if code, ok := fnosRespCode(body); ok && code != 0 {
			// 接口存在但返回业务错误：令牌本身可能是好的，换刷新接口再试
			lastErr = fmt.Sprintf("挂载列表返回 code=%d", code)
		}

		// 试刷新接口的参数形式，成功的记住
		for _, p := range []map[string]any{{}, {"path": "/"}, {"force": true}} {
			rb, rerr := fnosCGI(base, tok, "appcgi.mountmgr.cache.refresh", p)
			if rerr != nil {
				lastErr = "刷新接口连不上"
				continue
			}
			rlow := strings.ToLower(rb)
			if strings.Contains(rlow, "invalid token") || strings.Contains(rlow, "unauthorized") {
				return false, "令牌无效或已过期"
			}
			if code, ok := fnosRespCode(rb); ok && code == 0 {
				fnosTokMu.Lock()
				fnosRefreshParam = p
				fnosTokMu.Unlock()
				return true, "令牌可用，已能刷新挂载目录"
			}
			lastErr = firstN(strings.TrimSpace(rb), 60)
		}
		return false, "令牌可用，但刷新接口调用失败：" + lastErr
	}
	if lastErr == "" {
		lastErr = "找不到平台服务"
	}
	return false, lastErr
}

// refreshViaToken 用已保存的令牌请求平台刷新挂载目录
func refreshViaToken() (bool, string) {
	tok := loadFnosToken()
	if tok == "" {
		return false, "未配置令牌"
	}
	fnosTokMu.RLock()
	param := fnosRefreshParam
	fnosTokMu.RUnlock()

	var lastErr string
	for _, base := range fnosLocalAddrs() {
		body, err := fnosCGI(base, tok, "appcgi.mountmgr.cache.refresh", param)
		if err != nil {
			lastErr = "连不上平台服务"
			continue
		}
		low := strings.ToLower(body)
		if strings.Contains(low, "invalid token") || strings.Contains(low, "unauthorized") {
			fnosTokMu.Lock()
			fnosTokOK, fnosTokMsg = false, "令牌失效，请重新配置"
			fnosTokMu.Unlock()
			return false, fnosTokMsg
		}
		if code, ok := fnosRespCode(body); ok && code == 0 {
			fnosTokMu.Lock()
			fnosTokOK, fnosTokMsg = true, "已用令牌刷新挂载"
			fnosTokMu.Unlock()
			return true, fnosTokMsg
		}
		lastErr = firstN(strings.TrimSpace(body), 40)
	}
	if lastErr == "" {
		lastErr = "平台刷新失败"
	}
	fnosTokMu.Lock()
	fnosTokOK, fnosTokMsg = false, lastErr
	fnosTokMu.Unlock()
	return false, lastErr
}
