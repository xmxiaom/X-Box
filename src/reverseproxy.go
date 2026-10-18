package main

// 反向代理：应用自己拉起一个独立 nginx 实例（复用系统二进制，但配置/pid/日志/端口
// 全部独立），完全不碰飞牛自带的那个网关实例。
//
// 为什么这样做（三轮踩坑后的结论）：
//   - 改飞牛网关的 conf.d 并 reload：飞牛自身连接会抖一下（客户端弹"连接已断开"），
//     而且它的 conf.d 在系统重启时会被重建，规则会丢；
//   - 应用自己监听 TCP 转发：多占一个端口；
//   - 纯路径挂载（/app/xxx/r/id/）：不占端口，但站点跑在子路径下，SPA 无法运行。
//
// 现在的模型：规则（目标+端口）存应用数据卷；由本应用生成的 nginx.conf 驱动一个
// **独立实例**。改规则只 reload 这个实例，飞牛网关完全不受影响。
// 应用自身依然不监听任何 TCP 端口（面板只走网关 socket）。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type ProxyRule struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Port   int    `json:"port"`
	Domain string `json:"domain"`
	Target string `json:"target"`
	Desc   string `json:"desc"`
}

var proxyReID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,40}$`)
var proxyReDomain = regexp.MustCompile(`^[a-zA-Z0-9.*_-]{1,120}$`)
var proxyReTarget = regexp.MustCompile(`^https?://[^\s;{}]+$`)

var proxyStore struct{ mu sync.Mutex }

// ---------------- 规则持久化 ----------------

func proxyRulesPath() string {
	dir := os.Getenv("XBOX_DATA")
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "reverse-proxy.json")
}

func loadProxyRules() []ProxyRule {
	proxyStore.mu.Lock()
	defer proxyStore.mu.Unlock()
	b, err := os.ReadFile(proxyRulesPath())
	if err != nil {
		return nil
	}
	var out []ProxyRule
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Port < out[j].Port })
	return out
}

func saveProxyRules(rules []ProxyRule) error {
	proxyStore.mu.Lock()
	defer proxyStore.mu.Unlock()
	b, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(proxyRulesPath(), b, 0o600)
}

// ---------------- 独立 nginx 实例 ----------------

// ownNginxDir 本应用自己的 nginx 工作目录（配置、pid、日志、临时文件都在这里）。
func ownNginxDir() string {
	dir := os.Getenv("XBOX_DATA")
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "nginx")
}

// systemNginxBin 复用系统 nginx 的可执行文件（不启动它的实例，只借二进制）。
func systemNginxBin() string {
	if v := strings.TrimSpace(os.Getenv("XBOX_NGINX_BIN")); v != "" {
		return v
	}
	for _, p := range []string{"/usr/trim/nginx/sbin/nginx", "/usr/sbin/nginx", "/usr/bin/nginx"} {
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return ""
}

// renderOwnConf 生成本应用实例的主配置。
func renderOwnConf(rules []ProxyRule) string {
	var b strings.Builder
	b.WriteString("# 由 X-Box 生成：本应用专用的 nginx 实例配置（与飞牛网关实例互不影响）\n")
	b.WriteString("worker_processes 1;\n")
	b.WriteString("pid logs/nginx.pid;\n")
	b.WriteString("error_log logs/error.log warn;\n")
	b.WriteString("events { worker_connections 512; }\n")
	b.WriteString("http {\n")
	b.WriteString("\taccess_log off;\n")
	b.WriteString("\tclient_max_body_size 0;\n")
	b.WriteString("\tclient_body_temp_path temp/body;\n")
	b.WriteString("\tproxy_temp_path temp/proxy;\n")
	b.WriteString("\tfastcgi_temp_path temp/fastcgi;\n")
	b.WriteString("\tuwsgi_temp_path temp/uwsgi;\n")
	b.WriteString("\tscgi_temp_path temp/scgi;\n")
	for _, r := range rules {
		b.WriteString("\tserver {\n")
		b.WriteString(fmt.Sprintf("\t\tlisten %d;\n", r.Port))
		if d := strings.TrimSpace(r.Domain); d != "" {
			b.WriteString("\t\tserver_name " + d + ";\n")
		} else {
			b.WriteString("\t\tserver_name _;\n")
		}
		b.WriteString("\t\tlocation / {\n")
		b.WriteString("\t\t\tproxy_pass " + r.Target + ";\n")
		b.WriteString("\t\t\tproxy_http_version 1.1;\n")
		b.WriteString("\t\t\tproxy_set_header Host $host;\n")
		b.WriteString("\t\t\tproxy_set_header X-Real-IP $remote_addr;\n")
		b.WriteString("\t\t\tproxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
		b.WriteString("\t\t\tproxy_set_header X-Forwarded-Proto $scheme;\n")
		b.WriteString("\t\t\tproxy_set_header Upgrade $http_upgrade;\n")
		b.WriteString("\t\t\tproxy_set_header Connection $http_connection;\n")
		b.WriteString("\t\t\tproxy_read_timeout 3600s;\n")
		b.WriteString("\t\t\tproxy_send_timeout 3600s;\n")
		b.WriteString("\t\t}\n")
		b.WriteString("\t}\n")
	}
	b.WriteString("}\n")
	return b.String()
}

// runOwn 以「本应用实例」的身份执行 nginx 命令（-p 指向自己的目录）。
func runOwn(args ...string) string {
	bin := systemNginxBin()
	if bin == "" {
		return "未找到 nginx 可执行文件"
	}
	dir := ownNginxDir()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	a := append([]string{"-p", dir + "/", "-c", "nginx.conf"}, args...)
	out, err := exec.CommandContext(ctx, bin, a...).CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "nginx 命令超时（8s）：" + strings.TrimSpace(string(out))
		}
		return strings.TrimSpace(string(out))
	}
	return ""
}

// ownRunning 判断本应用实例是否在跑。
func ownRunning() bool {
	b, err := os.ReadFile(filepath.Join(ownNginxDir(), "logs", "nginx.pid"))
	if err != nil {
		return false
	}
	pid, perr := strconv.Atoi(strings.TrimSpace(string(b)))
	if perr != nil || pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// applyOwnConfig 写配置并（重）加载本应用实例。返回错误文本，空串为成功。
func applyOwnConfig(rules []ProxyRule) string {
	dir := ownNginxDir()
	for _, sub := range []string{"", "logs", "temp/body", "temp/proxy", "temp/fastcgi", "temp/uwsgi", "temp/scgi"} {
		_ = os.MkdirAll(filepath.Join(dir, sub), 0o755)
	}
	conf := renderOwnConf(rules)
	if err := os.WriteFile(filepath.Join(dir, "nginx.conf"), []byte(conf), 0o644); err != nil {
		return "写入配置失败：" + err.Error()
	}
	if msg := runOwn("-t"); msg != "" {
		return "配置校验未通过：" + msg
	}
	if ownRunning() {
		if msg := runOwn("-s", "reload"); msg != "" {
			return "重载失败：" + msg
		}
		return ""
	}
	if len(rules) == 0 {
		return "" // 没有规则就不需要常驻
	}
	if msg := runOwn(); msg != "" {
		return "启动失败：" + msg
	}
	return ""
}

// syncProxyOnStart 启动时确保实例按当前规则运行。
func (s *Server) syncProxyOnStart() {
	rules := loadProxyRules()
	if len(rules) == 0 {
		return
	}
	if msg := applyOwnConfig(rules); msg != "" {
		log.Printf("[反代] 启动同步失败：%s", msg)
		return
	}
	log.Printf("[反代] 已按 %d 条规则启动独立实例", len(rules))
}

// ---------------- HTTP 接口 ----------------

func (s *Server) handleProxyStatus(w http.ResponseWriter, r *http.Request) {
	bin := systemNginxBin()
	ok(w, map[string]any{
		"available": bin != "",
		"binary":    bin,
		"dir":       ownNginxDir(),
		"running":   ownRunning(),
	})
}

func (s *Server) handleProxyList(w http.ResponseWriter, r *http.Request) {
	rules := loadProxyRules()
	b, _ := os.ReadFile(filepath.Join(ownNginxDir(), "logs", "error.log"))
	tail := ""
	if len(b) > 0 {
		lines := strings.Split(strings.TrimSpace(string(b)), "\n")
		if n := len(lines); n > 0 {
			if n > 6 {
				lines = lines[n-6:]
			}
			tail = strings.Join(lines, "\n")
		}
	}
	ok(w, map[string]any{
		"rules":   rules,
		"running": ownRunning(),
		"dir":     ownNginxDir(),
		"log":     tail,
	})
}

func (s *Server) handleProxySave(w http.ResponseWriter, r *http.Request) {
	var rule ProxyRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		fail(w, 400, "参数解析失败")
		return
	}
	rule.ID = strings.TrimSpace(rule.ID)
	rule.Name = strings.TrimSpace(rule.Name)
	rule.Domain = strings.TrimSpace(rule.Domain)
	rule.Target = strings.TrimSpace(rule.Target)
	if rule.ID == "" || !proxyReID.MatchString(rule.ID) {
		fail(w, 400, "标识只能用字母/数字/下划线/短横线")
		return
	}
	if rule.Port < 1024 || rule.Port > 65535 {
		fail(w, 400, "监听端口请使用 1024-65535（80/443 由飞牛占用）")
		return
	}
	if rule.Domain != "" && !proxyReDomain.MatchString(rule.Domain) {
		fail(w, 400, "域名格式不合法")
		return
	}
	if !proxyReTarget.MatchString(rule.Target) {
		fail(w, 400, "目标地址需形如 http://192.168.1.10:8096")
		return
	}
	u, err := url.Parse(rule.Target)
	if err != nil || u.Host == "" {
		fail(w, 400, "目标地址不合法")
		return
	}
	if rule.Name == "" {
		rule.Name = rule.ID
	}

	old := loadProxyRules()
	for _, x := range old {
		if x.ID == rule.ID {
			continue
		}
		if x.Port == rule.Port {
			fail(w, 400, fmt.Sprintf("端口 %d 已被规则「%s」使用", rule.Port, x.Name))
			return
		}
	}

	merged := make([]ProxyRule, 0, len(old)+1)
	found := false
	for _, x := range old {
		if x.ID == rule.ID {
			merged = append(merged, rule)
			found = true
		} else {
			merged = append(merged, x)
		}
	}
	if !found {
		merged = append(merged, rule)
	}

	// 先落盘配置再应用：失败要能回滚到旧规则
	if err := saveProxyRules(merged); err != nil {
		fail(w, 500, "保存失败："+err.Error())
		return
	}
	if msg := applyOwnConfig(merged); msg != "" {
		_ = saveProxyRules(old)
		_ = applyOwnConfig(old)
		fail(w, 400, "应用失败，已回滚："+msg)
		return
	}
	ok(w, map[string]any{"saved": rule})
}

func (s *Server) handleProxyDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" || !proxyReID.MatchString(id) {
		fail(w, 400, "标识不合法")
		return
	}
	old := loadProxyRules()
	kept := make([]ProxyRule, 0, len(old))
	found := false
	for _, x := range old {
		if x.ID == id {
			found = true
			continue
		}
		kept = append(kept, x)
	}
	if !found {
		fail(w, 404, "规则不存在")
		return
	}
	if err := saveProxyRules(kept); err != nil {
		fail(w, 500, "保存失败："+err.Error())
		return
	}
	if msg := applyOwnConfig(kept); msg != "" {
		_ = saveProxyRules(old)
		_ = applyOwnConfig(old)
		fail(w, 400, "删除后应用失败，已回滚："+msg)
		return
	}
	ok(w, map[string]any{"deleted": id})
}

// handleProxyEntry 兼容旧链接：提示改用端口访问。
func (s *Server) handleProxyEntry(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.RequestURI(), "/r/")
	id = strings.SplitN(id, "/", 2)[0]
	port := 0
	for _, x := range loadProxyRules() {
		if x.ID == id {
			port = x.Port
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if port == 0 {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`<body style="font:14px system-ui,sans-serif;padding:24px;color:#555">没有找到这条反向代理规则。</body>`))
		return
	}
	_, _ = w.Write([]byte(`<body style="font:14px system-ui,sans-serif;padding:24px;color:#555">该规则由本应用的独立反代实例监听，请访问 <b>http://NAS地址:` +
		strconv.Itoa(port) + `/</b></body>`))
}
