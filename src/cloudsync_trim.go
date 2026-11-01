package main

// 刷新网盘挂载目录：复刻文件管理里「刷新按钮」的动作。
//
// 为什么不是 rclone：官方论坛已确认，缓存压在 fnOS 自己的 WebDAV 桥进程
// （/usr/trim/bin/webdav）里，rclone rc coreFlush 刷不掉、SIGHUP 也没用，
// 只有文件管理的刷新按钮能让新文件立刻出现——它走的是 fnOS 平台自己的接口。
//
// 调用链（凭据仅用于向本机 fnOS 登录换取 token）：
//   1) POST {addr}/v/api/v1/login  {username,password,app_name} → data.token
//   2) POST {addr}/cgi             {method:"appcgi.mountmgr.cache.refresh",params:{...}}
//      请求头 Authorization: trim <token>
//
// token 只在内存缓存，过期或 401 时自动重新登录；账号密码存 XBOX_DATA（0600）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type trimCred struct {
	User string `json:"user"`
	Pass string `json:"pass"`
}

var (
	trimMu       sync.Mutex
	trimTok      string
	trimTokAt    time.Time
	trimLastAt   time.Time
	trimLastOK   bool
	trimLastNote string
)

func trimCredPath() string {
	dir := os.Getenv("XBOX_DATA")
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "fnos-account.json")
}

func loadTrimCred() (trimCred, bool) {
	b, err := os.ReadFile(trimCredPath())
	if err != nil {
		return trimCred{}, false
	}
	var c trimCred
	if json.Unmarshal(b, &c) != nil || c.User == "" {
		return trimCred{}, false
	}
	return c, true
}

func saveTrimCred(c trimCred) error {
	if c.User == "" || c.Pass == "" {
		if err := os.Remove(trimCredPath()); err != nil && !os.IsNotExist(err) {
			return err
		}
		trimMu.Lock()
		trimTok = ""
		trimMu.Unlock()
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	trimMu.Lock()
	trimTok = "" // 账号变更后作废旧 token
	trimMu.Unlock()
	return os.WriteFile(trimCredPath(), b, 0o600)
}

func trimAddr() string {
	if v := strings.TrimSpace(os.Getenv("XBOX_TRIM_ADDR")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:8000"
}

// trimLogin 用本机 fnOS 账号登录换 token
func trimLogin(c trimCred) (string, error) {
	body, _ := json.Marshal(map[string]string{
		"username": c.User, "password": c.Pass, "app_name": "trim",
	})
	cl := &http.Client{Timeout: 8 * time.Second}
	resp, err := cl.Post(trimAddr()+"/v/api/v1/login", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var out struct {
		Code int    `json:"code"`
		Msg  string `json:"msg"`
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return "", fmt.Errorf("登录响应无法解析：%s", strings.TrimSpace(string(raw)))
	}
	if out.Code != 0 || out.Data.Token == "" {
		if out.Msg == "" {
			out.Msg = strings.TrimSpace(string(raw))
		}
		return "", fmt.Errorf("登录失败：%s（code=%d）", out.Msg, out.Code)
	}
	return out.Data.Token, nil
}

// trimCGI 调 fnOS 平台接口（Authorization: trim <token>）
func trimCGI(token, method string, params map[string]any) error {
	if params == nil {
		params = map[string]any{}
	}
	payload, _ := json.Marshal(map[string]any{"method": method, "params": params})
	req, err := http.NewRequest("POST", trimAddr()+"/cgi", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "trim "+token)
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	text := strings.TrimSpace(string(raw))
	low := strings.ToLower(text)
	if resp.StatusCode != 200 || strings.Contains(low, "invalid token") {
		if len(text) > 160 {
			text = text[:160]
		}
		return fmt.Errorf("HTTP %d：%s", resp.StatusCode, text)
	}
	return nil
}

// trimCGIRaw 调平台接口并返回原始响应（诊断用）
func trimCGIRaw(token, method string, params map[string]any) (string, error) {
	if params == nil {
		params = map[string]any{}
	}
	payload, _ := json.Marshal(map[string]any{"method": method, "params": params})
	req, err := http.NewRequest("POST", trimAddr()+"/cgi", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "trim "+token)
	cl := &http.Client{Timeout: 10 * time.Second}
	resp, err := cl.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 256<<10))
	return string(raw), nil
}

// trimToken 取有效 token（内存缓存 30 分钟，401 时会重新登录）
func trimToken(force bool) (string, trimCred, error) {
	c, ok := loadTrimCred()
	if !ok {
		return "", c, fmt.Errorf("未配置 fnOS 账号")
	}
	trimMu.Lock()
	tok, at := trimTok, trimTokAt
	trimMu.Unlock()
	if !force && tok != "" && time.Since(at) < 30*time.Minute {
		return tok, c, nil
	}
	t, err := trimLogin(c)
	if err != nil {
		return "", c, err
	}
	trimMu.Lock()
	trimTok, trimTokAt = t, time.Now()
	trimMu.Unlock()
	return t, c, nil
}

// refreshMountCache 让 fnOS 刷新挂载目录缓存（等价点一次刷新按钮）。
// 5 秒节流，避免实时模式下反复触发。
func refreshMountCache(srcPath string) bool {
	trimMu.Lock()
	if time.Since(trimLastAt) < 5*time.Second {
		ok := trimLastOK
		trimMu.Unlock()
		return ok
	}
	trimLastAt = time.Now()
	trimMu.Unlock()

	if _, _, err := trimToken(false); err != nil {
		trimMu.Lock()
		changed := trimLastOK || trimLastNote != err.Error()
		trimLastOK, trimLastNote = false, err.Error()
		trimMu.Unlock()
		if changed {
			log.Printf("[同步] 挂载缓存刷新不可用：%v", err)
		}
		return false
	}

	params := map[string]any{"path": srcPath}
	err := trimCGI(mustToken(), "appcgi.mountmgr.cache.refresh", params)
	if err != nil && strings.Contains(strings.ToLower(err.Error()), "invalid token") {
		if _, _, e2 := trimToken(true); e2 == nil {
			err = trimCGI(mustToken(), "appcgi.mountmgr.cache.refresh", params)
		}
	}
	trimMu.Lock()
	defer trimMu.Unlock()
	if err != nil {
		if trimLastOK || trimLastNote != err.Error() {
			log.Printf("[同步] 挂载缓存刷新失败（%s）：%v", srcPath, err)
			trimLastNote = err.Error()
		}
		trimLastOK = false
		return false
	}
	if !trimLastOK {
		log.Printf("[同步] 已通过 fnOS 接口刷新挂载目录（%s）", srcPath)
	}
	trimLastNote = ""
	trimLastOK = true
	return true
}

func mustToken() string {
	trimMu.Lock()
	defer trimMu.Unlock()
	return trimTok
}
