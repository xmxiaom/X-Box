package main

// 终端模块：命令执行控制台 + PTY 交互会话（v1.3.22）。
//
// 两种形态：
//  1. exec 模式（旧兼容）：POST /api/terminal/exec 单命令串行执行，60s 超时，
//     无 TTY；长任务用 nohup ... >/log 2>&1 & 转后台。保持 cd 会话目录。
//  2. PTY 模式（v1.3.22 新增）：长驻 script -qec bash 伪终端会话，start/write/stop
//     三接口轮询读写，支持交互式命令（top/vi/ssh 等）。零 Go 依赖——利用
//     util-linux 的 script 命令做 PTY 载体（fnOS 基于 Debian 必有），
//     WSL 实测管道输入转发、退出码透传均正常。
//
// 兼容策略：PTY 探测不可用（无 script）时 start 返回 {pty:false}，前端降级 exec 模式；
// 现有 exec/info/close 接口保留，tunnel_test 与旧客户端不破坏。
//
// 注意：此终端以运行 X-Box 的权限（fnOS 上为 root）执行任意命令；面板访问面
// 由 fnOS 账号体系兜底，与 X-Box「无登录态、依赖 fnOS 远程访问」的安全模型一致。

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type terminalRuntime struct {
	mu  sync.Mutex
	cwd string

	// PTY 长驻会话（nil 表示当前未启动 PTY，走 exec 模式）
	pty *ptySession
}

// ptySession 一次长驻 script -qec bash 伪终端会话。
type ptySession struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser // 会话 stdin（管道写端）
	outBuf *syncBuf       // 会话 stdout 累积缓冲（线程安全）
	errBuf *syncBuf       // 会话 stderr 累积缓冲（线程安全）
	stopCh chan struct{}  // 通知收尾协程退出
	last   time.Time      // 最近一次活跃（write/读输出）时间，用于空闲回收
	done   chan struct{}  // 会话进程退出后关闭
}

// syncBuf 线程安全的输出累积缓冲：script 子进程由 os/exec 内部 goroutine 后台
// 写 stdout/stderr 管道，而读取发生在请求持锁段，普通 bytes.Buffer 会数据竞态。
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

// ReadAndReset 取出全部累积内容并清空（调用者无需另行持锁）。
func (s *syncBuf) ReadAndReset() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := s.b.String()
	s.b.Reset()
	return out
}

var termRT *terminalRuntime

const (
	ptyIdleTimeout = 5 * time.Minute // 会话空闲自动回收
	ptyWriteWait   = 250 * time.Millisecond // write 后等待输出累积的时长
	ptyMaxRead     = 256 * 1024              // 单次输出上限，防止无限增长
)

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

// startPTYIdleReaper 启动空闲会话回收 goroutine（main 中调用）。
func startPTYIdleReaper() {
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for range ticker.C {
			termRT.mu.Lock()
			if termRT.pty != nil && time.Since(termRT.pty.last) > ptyIdleTimeout {
				p := termRT.pty
				termRT.pty = nil
				termRT.mu.Unlock()
				killPTYSession(p)
				continue
			}
			termRT.mu.Unlock()
		}
	}()
}

// killPTYSession 终止会话并回收资源（幂等）。
func killPTYSession(p *ptySession) {
	if p == nil {
		return
	}
	if p.cmd != nil && p.cmd.Process != nil {
		_ = p.cmd.Process.Kill()
		// 尽力等待收尾，避免僵尸
		select {
		case <-p.done:
		case <-time.After(2 * time.Second):
		}
	}
	if p.stdin != nil {
		_ = p.stdin.Close()
	}
	close(p.stopCh)
}

// scriptAvailable 探测系统是否带 util-linux script 命令。
func scriptAvailable() bool {
	_, err := exec.LookPath("script")
	return err == nil
}

// ptyRealCwd 读取 PTY 会话内 bash 的真实工作目录。
// 注意：util-linux script 会 fork（而非 exec 替换自身）一个子进程去跑目标
// shell（bash），父 script 进程的 cwd 始终不变，必须经
// /proc/<scriptpid>/task/<scriptpid>/children 找到 bash 子进程再读其 cwd。
// 读失败返回空串。
func ptyRealCwd(cmd *exec.Cmd) string {
	if cmd == nil || cmd.Process == nil {
		return ""
	}
	spid := strconv.Itoa(cmd.Process.Pid)
	// 找子进程 PID（bash）
	childBytes, err := os.ReadFile("/proc/" + spid + "/task/" + spid + "/children")
	var childPid string
	if err == nil {
		childPid = strings.TrimSpace(string(childBytes))
		if i := strings.Index(childPid, " "); i >= 0 {
			childPid = childPid[:i] // 取第一个子进程
		}
	}
	// 优先读 bash 子进程 cwd，读不到退回父 script 进程
	var target string
	if childPid != "" {
		if t, err := os.Readlink("/proc/" + childPid + "/cwd"); err == nil {
			target = t
		}
	}
	if target == "" {
		if t, err := os.Readlink("/proc/" + spid + "/cwd"); err == nil {
			target = t
		}
	}
	return target
}

// syncPTYCwd 在 PTY 会话结束时把 bash 最后目录写回 termRT.cwd，
// 保证降级 exec 与 info 的起始目录与用户最后 cd 到的位置一致。
func syncPTYCwd(cmd *exec.Cmd) {
	real := ptyRealCwd(cmd)
	if real == "" {
		return
	}
	termRT.mu.Lock()
	termRT.cwd = real
	termRT.mu.Unlock()
}

// handleTerminalStart POST /api/terminal/start：拉起长驻 PTY 会话。
// 返回 {pty:true, cwd}；script 不可用时返回 {pty:false} 由前端降级 exec。
func (s *Server) handleTerminalStart(w http.ResponseWriter, r *http.Request) {
	termRT.mu.Lock()
	defer termRT.mu.Unlock()

	// 已有会话：直接复用（保持单会话模型）
	if termRT.pty != nil {
		termRT.pty.last = time.Now()
		ok(w, map[string]any{"pty": true, "cwd": termRT.cwd})
		return
	}
	if !scriptAvailable() {
		ok(w, map[string]any{"pty": false, "cwd": termRT.cwd})
		return
	}

	cmd := exec.Command("script", "-qec", "bash", "/dev/null")
	cmd.Dir = termRT.cwd
	cmd.Env = append(os.Environ(), "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	// TERM 必须存在，否则部分交互程序（top/vi）行为异常
	cmd.Env = append(cmd.Env, "TERM=xterm-256color")

	stdin, err := cmd.StdinPipe()
	if err != nil {
		fail(w, 500, "PTY stdin 管道创建失败: "+err.Error())
		return
	}
	outBuf := &syncBuf{}
	errBuf := &syncBuf{}
	cmd.Stdout = outBuf
	cmd.Stderr = errBuf

	stopCh := make(chan struct{})
	done := make(chan struct{})
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		fail(w, 500, "PTY 启动失败: "+err.Error())
		return
	}
	// 收尾协程：进程退出后标记 done（幂等关闭 stopCh 由 kill 负责）；
	// 同时尝试把 bash 最后工作目录同步回 termRT.cwd（供降级 exec 与信息展示）
	go func() {
		_ = cmd.Wait()
		syncPTYCwd(cmd)
		select {
		case <-done:
		default:
			close(done)
		}
	}()

	termRT.pty = &ptySession{
		cmd:    cmd,
		stdin:  stdin,
		outBuf: outBuf,
		errBuf: errBuf,
		stopCh: stopCh,
		last:   time.Now(),
		done:   done,
	}
	ok(w, map[string]any{"pty": true, "cwd": termRT.cwd})
}

// handleTerminalWrite POST /api/terminal/write：写入命令到会话 stdin，
// 等待短时间后返回累积输出。body: {data:"..."}  resp: {pty:true, output:"...", cwd}
func (s *Server) handleTerminalWrite(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Data string `json:"data"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, "参数错误")
		return
	}

	termRT.mu.Lock()
	p := termRT.pty
	if p == nil {
		termRT.mu.Unlock()
		fail(w, 409, "PTY 会话未启动，请先 start")
		return
	}
	// 会话已死：清理并报错，前端可重试 start
	select {
	case <-p.done:
		termRT.pty = nil
		termRT.mu.Unlock()
		fail(w, 409, "PTY 会话已结束，请重新 start")
		return
	default:
	}
	p.last = time.Now()
	if _, err := p.stdin.Write([]byte(req.Data)); err != nil {
		termRT.pty = nil
		termRT.mu.Unlock()
		fail(w, 500, "PTY 写入失败: "+err.Error())
		return
	}
	// 短等累积输出，避免前端空轮询
	termRT.mu.Unlock()
	time.Sleep(ptyWriteWait)

	termRT.mu.Lock()
	defer termRT.mu.Unlock()
	p = termRT.pty
	if p == nil {
		ok(w, map[string]any{"pty": true, "output": "", "cwd": termRT.cwd})
		return
	}
	// 会话存活期间同步 bash 真实 cwd（script exec bash 后 PID 不变，
	// /proc/<pid>/cwd 即当前目录），保证 stop/降级 exec 的起始目录准确
	if real := ptyRealCwd(p.cmd); real != "" {
		termRT.cwd = real
	}
	out := p.outBuf.ReadAndReset()
	errOut := p.errBuf.ReadAndReset()
	output := out
	if errOut != "" {
		if output != "" {
			output += "\n"
		}
		output += errOut
	}
	if len(output) > ptyMaxRead {
		output = output[len(output)-ptyMaxRead:]
	}
	ok(w, map[string]any{"pty": true, "output": output, "cwd": termRT.cwd})
}

// handleTerminalStop POST /api/terminal/stop：终止 PTY 会话，回 exec 模式。
func (s *Server) handleTerminalStop(w http.ResponseWriter, r *http.Request) {
	termRT.mu.Lock()
	p := termRT.pty
	if p == nil {
		termRT.mu.Unlock()
		ok(w, map[string]any{"pty": false, "cwd": termRT.cwd})
		return
	}
	termRT.pty = nil
	// kill 前进程还活着，同步 bash 最后工作目录供降级 exec 使用
	if real := ptyRealCwd(p.cmd); real != "" {
		termRT.cwd = real
	}
	termRT.mu.Unlock()
	killPTYSession(p)
	ok(w, map[string]any{"pty": false, "cwd": termRT.cwd})
}

// handleTerminalInfo 返回当前会话工作目录。
func (s *Server) handleTerminalInfo(w http.ResponseWriter, r *http.Request) {
	termRT.mu.Lock()
	cwd := termRT.cwd
	termRT.mu.Unlock()
	ok(w, map[string]any{"cwd": cwd})
}

// handleTerminalClose POST /api/terminal/close：断开会话，工作目录复位 /root。
func (s *Server) handleTerminalClose(w http.ResponseWriter, r *http.Request) {
	termRT.mu.Lock()
	termRT.cwd = "/root"
	termRT.mu.Unlock()
	ok(w, map[string]any{"cwd": "/root"})
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
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ecmd := exec.CommandContext(ctx, "sh", "-c", cmd)
	ecmd.Dir = dir
	// 补全 PATH：fnOS 应用运行环境的 PATH 可能不全，缺 /usr/sbin 等会导致
	// ping/ip/iptables 等常用网络工具 "not found"，表现为"对外连接能力不足"
	ecmd.Env = append(os.Environ(), "PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin")
	var out strings.Builder
	ecmd.Stdout = &out
	ecmd.Stderr = &out
	exitCode := 0
	runErr := ecmd.Run()
	cwd := termRT.cwd
	termRT.mu.Unlock()

	if ctx.Err() == context.DeadlineExceeded {
		ok(w, map[string]any{"output": out.String() + "\n[命令超时（60s），已被终止]\n[提示：长耗时任务请转后台执行，如  nohup wget -O /tmp/x.fpk <URL> >/tmp/x.log 2>&1 &  （输出重定向后 sh 立即返回，任务在 NAS 上继续）]\n", "exitCode": -1, "cwd": cwd})
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
