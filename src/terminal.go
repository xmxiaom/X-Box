package main

// 终端模块：命令执行控制台。保持 cd 会话目录，单会话串行执行（互斥锁），
// 30s 超时保护。纯标准库，无 PTY（fnOS 远程客户端 iframe 内 WebSocket 受限，
// 且项目零依赖），兼容现有 API 隧道（POST 经 formBridge / 隧道分发）。
//
// 注意：此终端以运行 X-Box 的权限（fnOS 上为 root）执行任意命令；面板访问面
// 由 fnOS 账号体系兜底，与 X-Box「无登录态、依赖 fnOS 远程访问」的安全模型一致。

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type terminalRuntime struct {
	mu  sync.Mutex
	cwd string
}

var termRT *terminalRuntime

func initTerminal() {
	cwd := "/root"
	if _, err := os.Stat("/root"); err != nil {
		if wd, err := os.Getwd(); err == nil {
			cwd = wd
		} else {
			cwd = "/"
		}
	}
	termRT = &terminalRuntime{cwd: cwd}
}

// handleTerminalInfo 返回当前会话工作目录。
func (s *Server) handleTerminalInfo(w http.ResponseWriter, r *http.Request) {
	termRT.mu.Lock()
	cwd := termRT.cwd
	termRT.mu.Unlock()
	ok(w, map[string]any{"cwd": cwd})
}

// handleTerminalExec 执行一条命令，返回合并输出与退出码；支持 cd 维持会话目录。
func (s *Server) handleTerminalExec(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Cmd string `json:"cmd"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	cmd := strings.TrimSpace(req.Cmd)
	if cmd == "" {
		termRT.mu.Lock()
		cwd := termRT.cwd
		termRT.mu.Unlock()
		ok(w, map[string]any{"output": "", "exitCode": 0, "cwd": cwd})
		return
	}

	// cd：仅维护会话目录，不真正起子进程（与其他命令串行，避免竞态）
	lower := strings.ToLower(cmd)
	if lower == "cd" || strings.HasPrefix(lower, "cd ") || strings.HasPrefix(lower, "cd\t") {
		rest := strings.TrimSpace(cmd[2:])
		if rest == "" || rest == "~" {
			rest = "/root"
		} else if strings.HasPrefix(rest, "~") {
			rest = "/root" + rest[1:]
		}
		termRT.mu.Lock()
		var newCwd string
		if strings.HasPrefix(rest, "/") {
			newCwd = filepath.Clean(rest)
		} else {
			newCwd = filepath.Join(termRT.cwd, rest)
		}
		if info, err := os.Stat(newCwd); err == nil && info.IsDir() {
			termRT.cwd = newCwd
			termRT.mu.Unlock()
			ok(w, map[string]any{"output": "", "exitCode": 0, "cwd": newCwd})
			return
		}
		cwd := termRT.cwd
		termRT.mu.Unlock()
		ok(w, map[string]any{"output": "cd: " + rest + ": 不是目录或不存在\n", "exitCode": 1, "cwd": cwd})
		return
	}

	termRT.mu.Lock()
	dir := termRT.cwd
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	ecmd := exec.CommandContext(ctx, "sh", "-c", cmd)
	ecmd.Dir = dir
	var out strings.Builder
	ecmd.Stdout = &out
	ecmd.Stderr = &out
	exitCode := 0
	runErr := ecmd.Run()
	cwd := termRT.cwd
	termRT.mu.Unlock()

	if ctx.Err() == context.DeadlineExceeded {
		ok(w, map[string]any{"output": out.String() + "\n[命令超时（30s），已被终止]\n", "exitCode": -1, "cwd": cwd})
		return
	}
	if runErr != nil {
		if ee, ok2 := runErr.(*exec.ExitError); ok2 {
			exitCode = ee.ExitCode()
		} else {
			ok(w, map[string]any{"output": out.String() + "\n" + runErr.Error() + "\n", "exitCode": -1, "cwd": cwd})
			return
		}
	}
	ok(w, map[string]any{"output": out.String(), "exitCode": exitCode, "cwd": cwd})
}
