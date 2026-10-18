package main

// 反向代理：路由规则交给飞牛自带网关（nginx）监听，应用不自己占端口。
//
// 工作方式：在 /usr/trim/nginx/conf/conf.d 下写入独立的 xbox-proxy-*.conf，
// 网关按 server_name 路由到目标服务；证书沿用飞牛的证书托管（trim_hook_cert_switch），
// 外部访问走的就是飞牛网关的 443，与官方应用同一套入口。
//
// 安全边界：
//   - 只写/删本应用生成的文件，不修改主配置，不碰其他站点；
//   - 每次改动后先 nginx -t 校验，失败立即回滚旧文件；
//   - 校验通过才 reload，reload 失败同样回滚。

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

type ProxyRule struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Domain      string `json:"domain"`      // 支持多个，空格/逗号分隔
	PathPrefix  string `json:"pathPrefix"`  // 可选，如 /app
	StripPrefix bool   `json:"stripPrefix"` // 转发时去掉路径前缀
	Port        int    `json:"port"`        // 0 = 由网关 443 托管
	Target      string `json:"target"`
	TLS         bool   `json:"tls"`
	Desc        string `json:"desc"`
}

var proxyReID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,40}$`)
var proxyReDomain = regexp.MustCompile(`^[a-zA-Z0-9.*_-]{1,120}$`)
var proxyReTarget = regexp.MustCompile(`^https?://[^\s;{}]+$`)

// splitDomains 把 "a.com, b.com" 拆成 ["a.com","b.com"]。
func splitDomains(v string) []string {
	f := strings.FieldsFunc(v, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' })
	out := make([]string, 0, len(f))
	for _, x := range f {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

// nginxPaths 探测飞牛网关位置。XBOX_NGINX_ROOT 仅供测试。
func nginxPaths() (bin, mainConf, confDir string, ok bool) {
	root := strings.TrimSpace(os.Getenv("XBOX_NGINX_ROOT"))
	if root == "" {
		root = "/usr/trim/nginx"
	}
	bin = filepath.Join(root, "sbin", "nginx")
	mainConf = filepath.Join(root, "conf", "nginx.conf")
	confDir = filepath.Join(root, "conf", "conf.d")
	if _, err := os.Stat(mainConf); err != nil {
		return "", "", "", false
	}
	if b, err := os.ReadFile(mainConf); err == nil && strings.Contains(string(b), "conf.d") {
		if st, err := os.Stat(confDir); err == nil && st.IsDir() {
			return bin, mainConf, confDir, true
		}
	}
	return bin, mainConf, "", true
}

func proxyRules() []ProxyRule {
	_, _, confDir, ok := nginxPaths()
	if !ok || confDir == "" {
		return nil
	}
	files, _ := filepath.Glob(filepath.Join(confDir, "xbox-proxy-*.conf"))
	out := make([]ProxyRule, 0, len(files))
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, line := range strings.Split(string(b), "\n") {
			const tag = "# XBOX-PROXY-META: "
			if !strings.HasPrefix(line, tag) {
				continue
			}
			var r ProxyRule
			if json.Unmarshal([]byte(strings.TrimPrefix(line, tag)), &r) == nil {
				out = append(out, r)
			}
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Domain < out[j].Domain })
	return out
}

// renderProxyConf 生成 server 段。Port=0 时由网关 443 + 证书托管接管。
func renderProxyConf(r ProxyRule) string {
	meta, _ := json.Marshal(r)
	var b strings.Builder
	b.WriteString("# 由 X-Box 生成，请勿手工修改；在面板「反向代理」里删除即可\n")
	b.WriteString("# XBOX-PROXY-META: " + string(meta) + "\n")
	b.WriteString("server {\n")
	if r.TLS {
		if r.Port == 0 || r.Port == 443 {
			b.WriteString("\tlisten 443 ssl;\n")
		} else {
			b.WriteString(fmt.Sprintf("\tlisten %d ssl;\n", r.Port))
		}
		b.WriteString("\tssl_certificate $ssl_server_name;\n")
		b.WriteString("\tssl_certificate_key $ssl_server_name;\n")
		b.WriteString("\ttrim_hook_cert_switch on;\n") // 由飞牛按域名自动挂证书
		b.WriteString("\tssl_session_cache shared:SSL:1m;\n")
		b.WriteString("\tssl_session_timeout 5m;\n")
		b.WriteString("\tssl_ciphers HIGH:!aNULL:!MD5;\n")
		b.WriteString("\tssl_prefer_server_ciphers on;\n")
	} else {
		port := r.Port
		if port == 0 {
			port = 80
		}
		b.WriteString(fmt.Sprintf("\tlisten %d;\n", port))
	}
	domains := splitDomains(r.Domain)
	b.WriteString("\tserver_name " + strings.Join(domains, " ") + ";\n")
	b.WriteString("\tclient_max_body_size 0;\n")
	loc := "/"
	if r.PathPrefix != "" {
		loc = "/" + strings.Trim(r.PathPrefix, "/") + "/"
	}
	target := r.Target
	if r.StripPrefix {
		target = strings.TrimSuffix(target, "/") + "/"
	}
	b.WriteString("\tlocation " + loc + " {\n")
	b.WriteString("\t\tproxy_pass " + target + ";\n")
	b.WriteString("\t\tproxy_http_version 1.1;\n")
	b.WriteString("\t\tproxy_set_header Host $host;\n")
	b.WriteString("\t\tproxy_set_header X-Real-IP $remote_addr;\n")
	b.WriteString("\t\tproxy_set_header X-Forwarded-For $proxy_add_x_forwarded_for;\n")
	b.WriteString("\t\tproxy_set_header X-Forwarded-Proto $scheme;\n")
	// $http_connection 是客户端原始值：普通请求 keep-alive/close，WebSocket 是 upgrade
	b.WriteString("\t\tproxy_set_header Upgrade $http_upgrade;\n")
	b.WriteString("\t\tproxy_set_header Connection $http_connection;\n")
	b.WriteString("\t\tproxy_read_timeout 3600s;\n")
	b.WriteString("\t\tproxy_send_timeout 3600s;\n")
	b.WriteString("\t}\n")
	b.WriteString("}\n")
	return b.String()
}

func nginxRun(bin, mainConf string, args ...string) string {
	if bin == "" {
		return ""
	}
	if _, err := os.Stat(bin); err != nil {
		return ""
	}
	// 加超时：某些设备上 nginx -t 会读全量配置（含证书），耗时可能超过网关的
	// 转发超时；不设上限会把请求一起拖死。
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	a := append(args, "-c", mainConf)
	out, err := exec.CommandContext(ctx, bin, a...).CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return "nginx 命令超时（8s）：" + strings.TrimSpace(string(out))
		}
		return strings.TrimSpace(string(out))
	}
	return ""
}

// 异步应用：保存立刻返回，校验/重载在后台做，前端轮询状态。
// 原因：同步跑 nginx -t + reload 可能超过网关转发超时，导致调用方拿到 502、
// 而应用侧还在执行甚至回滚，表现为"提示失败而且规则也没了"。
type applyInfo struct {
	State string    `json:"state"` // pending / applied / failed
	Error string    `json:"error"`
	At    time.Time `json:"at"`
}

var applyMu sync.Mutex
var applyState = map[string]applyInfo{}

func setApply(id, state, msg string) {
	applyMu.Lock()
	applyState[id] = applyInfo{State: state, Error: msg, At: time.Now()}
	applyMu.Unlock()
}

func getApply(id string) applyInfo {
	applyMu.Lock()
	defer applyMu.Unlock()
	if v, ok := applyState[id]; ok {
		return v
	}
	return applyInfo{State: "applied"}
}

// applyProxyAsync 后台校验并重载；失败回滚文件。
func applyProxyAsync(id, path string, prev []byte, hadPrev bool, bin, mainConf string) {
	go func() {
		setApply(id, "pending", "")
		time.Sleep(300 * time.Millisecond) // 先让保存响应发出去
		if msg := nginxRun(bin, mainConf, "-t"); msg != "" {
			if hadPrev {
				_ = os.WriteFile(path, prev, 0o644)
			} else {
				_ = os.Remove(path)
			}
			setApply(id, "failed", "配置校验未通过，已回滚："+msg)
			return
		}
		if msg := nginxRun(bin, mainConf, "-s", "reload"); msg != "" {
			if hadPrev {
				_ = os.WriteFile(path, prev, 0o644)
			} else {
				_ = os.Remove(path)
			}
			_ = nginxRun(bin, mainConf, "-s", "reload")
			setApply(id, "failed", "网关重载失败，已回滚："+msg)
			return
		}
		setApply(id, "applied", "")
		log.Printf("[反代] 规则 %s 已生效", id)
	}()
}

func (s *Server) handleProxyStatus(w http.ResponseWriter, r *http.Request) {
	bin, mainConf, confDir, found := nginxPaths()
	ok(w, map[string]any{
		"available": found && confDir != "",
		"confDir":   confDir,
		"mainConf":  mainConf,
		"binary":    bin,
	})
}

func (s *Server) handleProxyList(w http.ResponseWriter, r *http.Request) {
	_, _, confDir, found := nginxPaths()
	if !found || confDir == "" {
		fail(w, 400, "未找到飞牛网关的 conf.d 目录，当前环境不支持")
		return
	}
	rules := proxyRules()
	out := make([]map[string]any, 0, len(rules))
	for _, r := range rules {
		st := getApply(r.ID)
		out = append(out, map[string]any{
			"id": r.ID, "name": r.Name, "domain": r.Domain, "pathPrefix": r.PathPrefix,
			"stripPrefix": r.StripPrefix, "port": r.Port, "target": r.Target,
			"tls": r.TLS, "desc": r.Desc,
			"applyState": st.State, "applyError": st.Error,
		})
	}
	ok(w, map[string]any{"rules": out, "confDir": confDir})
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
	rule.Domain = strings.Join(splitDomains(rule.Domain), " ")
	if rule.Domain == "" {
		fail(w, 400, "请填写域名（网关按域名路由）")
		return
	}
	for _, d := range splitDomains(rule.Domain) {
		if !proxyReDomain.MatchString(d) {
			fail(w, 400, "域名格式不合法："+d)
			return
		}
	}
	// 80/443 被飞牛自身占用：第三方 server 块监听它们会与系统站点冲突，
	// 证书钩子取不到域名证书时还会导致 reload 失败。因此端口必填且要求高位。
	if rule.Port <= 0 || rule.Port > 65535 {
		fail(w, 400, "请填写监听端口（建议 8443 以上；80/443 由飞牛占用）")
		return
	}
	if rule.Port < 1024 {
		fail(w, 400, "监听端口请使用 1024 以上（80/443 由飞牛自身占用）")
		return
	}
	if !proxyReTarget.MatchString(rule.Target) {
		fail(w, 400, "目标地址需形如 http://192.168.1.10:8096")
		return
	}
	if rule.Name == "" {
		rule.Name = rule.ID
	}

	bin, mainConf, confDir, found := nginxPaths()
	if !found || confDir == "" {
		fail(w, 400, "未找到飞牛网关的 conf.d 目录，当前环境不支持")
		return
	}
	// 同域名冲突：两条规则抢同一 server_name，网关只会命中其一
	for _, x := range proxyRules() {
		if x.ID == rule.ID {
			continue
		}
		for _, d1 := range splitDomains(rule.Domain) {
			for _, d2 := range splitDomains(x.Domain) {
				if strings.EqualFold(d1, d2) {
					fail(w, 400, "域名 "+d1+" 已被规则「"+x.Name+"」使用")
					return
				}
			}
		}
	}

	path := filepath.Join(confDir, "xbox-proxy-"+rule.ID+".conf")
	var prev []byte
	hadPrev := false
	if b, e := os.ReadFile(path); e == nil {
		prev, hadPrev = b, true
	}
	if err := os.WriteFile(path, []byte(renderProxyConf(rule)), 0o644); err != nil {
		fail(w, 500, "写入配置失败："+err.Error())
		return
	}
	// 立即返回，校验与重载在后台进行（避免超过网关转发超时导致 502）
	ok(w, map[string]any{"saved": rule, "applying": true})
	applyProxyAsync(rule.ID, path, prev, hadPrev, bin, mainConf)
}

func (s *Server) handleProxyDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" || !proxyReID.MatchString(id) {
		fail(w, 400, "标识不合法")
		return
	}
	bin, mainConf, confDir, found := nginxPaths()
	if !found || confDir == "" {
		fail(w, 400, "未找到飞牛网关的 conf.d 目录，当前环境不支持")
		return
	}
	path := filepath.Join(confDir, "xbox-proxy-"+id+".conf")
	prev, err := os.ReadFile(path)
	if err != nil {
		fail(w, 404, "规则不存在")
		return
	}
	if err := os.Remove(path); err != nil {
		fail(w, 500, "删除失败："+err.Error())
		return
	}
	ok(w, map[string]any{"deleted": id, "applying": true})
	go func() {
		setApply(id, "pending", "")
		time.Sleep(300 * time.Millisecond)
		if msg := nginxRun(bin, mainConf, "-t"); msg != "" {
			_ = os.WriteFile(path, prev, 0o644)
			setApply(id, "failed", "删除后校验失败，已回滚："+msg)
			return
		}
		_ = nginxRun(bin, mainConf, "-s", "reload")
		setApply(id, "applied", "")
	}()
}

// handleProxyEntry 兼容旧链接：提示改用域名访问。
func (s *Server) handleProxyEntry(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.RequestURI(), "/r/")
	id = strings.SplitN(id, "/", 2)[0]
	domain := ""
	for _, x := range proxyRules() {
		if x.ID == id {
			domain = x.Domain
			break
		}
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if domain == "" {
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`<body style="font:14px system-ui,sans-serif;padding:24px;color:#555">没有找到这条反向代理规则。</body>`))
		return
	}
	_, _ = w.Write([]byte(`<body style="font:14px system-ui,sans-serif;padding:24px;color:#555">该规则由飞牛网关按域名接管，请访问 <b>https://` +
		domain + `/</b></body>`))
}

