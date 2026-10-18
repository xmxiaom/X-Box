package main

// 反向代理入口（走应用自身的网关，不碰系统配置）。
//
// 场景：某个内网服务（例如 127.0.0.1:8096）想通过飞牛网关访问，从而复用
// "飞牛账号已登录"这一层认证，并且不必记 port、不必开防火墙。
//
// 做法：给服务起一个稳定入口 /r/<id>/，访问时跳进应用已有的浏览器代理
// （/b/{ver}/{scheme}/{host}/...）。这样资源改写、Cookie 隔离、WebSocket 透传、
// 各项防护全部复用现成实现，也不写任何系统 nginx 配置。
//
// 规则存在应用数据目录（XBOX_DATA/reverse.json），卸载时随数据卷保留/删除。

import (
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

type ProxyRule struct {
	ID     string `json:"id"`     // 路径标识（字母数字短横线）
	Name   string `json:"name"`   // 显示名
	Target string `json:"target"` // http://host:port
}

var proxyReID = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,40}$`)

var proxyStore struct {
	mu sync.Mutex
}

func proxyFilePath() string {
	dir := os.Getenv("XBOX_DATA")
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "reverse.json")
}

func proxyLoad() []ProxyRule {
	proxyStore.mu.Lock()
	defer proxyStore.mu.Unlock()
	b, err := os.ReadFile(proxyFilePath())
	if err != nil {
		return nil
	}
	var out []ProxyRule
	if json.Unmarshal(b, &out) != nil {
		return nil
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

func proxySaveAll(rules []ProxyRule) error {
	proxyStore.mu.Lock()
	defer proxyStore.mu.Unlock()
	b, err := json.MarshalIndent(rules, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(proxyFilePath(), b, 0o600)
}

func proxyFind(id string) (ProxyRule, bool) {
	for _, r := range proxyLoad() {
		if r.ID == id {
			return r, true
		}
	}
	return ProxyRule{}, false
}

// handleProxyList 列出规则，并给出可直接访问的完整地址。
func (s *Server) handleProxyList(w http.ResponseWriter, r *http.Request) {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	host := r.Host
	rules := proxyLoad()
	out := make([]map[string]any, 0, len(rules))
	for _, x := range rules {
		out = append(out, map[string]any{
			"id":     x.ID,
			"name":   x.Name,
			"target": x.Target,
			"path":   appBase + "/r/" + x.ID + "/",
			"url":    scheme + "://" + host + appBase + "/r/" + x.ID + "/",
		})
	}
	ok(w, map[string]any{"rules": out})
}

func (s *Server) handleProxySave(w http.ResponseWriter, r *http.Request) {
	var rule ProxyRule
	if err := json.NewDecoder(r.Body).Decode(&rule); err != nil {
		fail(w, 400, "参数解析失败")
		return
	}
	rule.ID = strings.TrimSpace(rule.ID)
	rule.Name = strings.TrimSpace(rule.Name)
	rule.Target = strings.TrimSpace(rule.Target)
	if rule.ID == "" || !proxyReID.MatchString(rule.ID) {
		fail(w, 400, "标识只能用字母/数字/下划线/短横线（会作为访问路径）")
		return
	}
	u, err := url.Parse(rule.Target)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		fail(w, 400, "目标地址需形如 http://192.168.1.10:8096")
		return
	}
	if rule.Name == "" {
		rule.Name = rule.ID
	}
	rules := proxyLoad()
	replaced := false
	for i := range rules {
		if rules[i].ID == rule.ID {
			rules[i] = rule
			replaced = true
			break
		}
	}
	if !replaced {
		rules = append(rules, rule)
	}
	if err := proxySaveAll(rules); err != nil {
		fail(w, 500, "保存失败："+err.Error())
		return
	}
	ok(w, map[string]any{"saved": rule, "path": appBase + "/r/" + rule.ID + "/"})
}

func (s *Server) handleProxyDelete(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" || !proxyReID.MatchString(id) {
		fail(w, 400, "标识不合法")
		return
	}
	rules := proxyLoad()
	kept := rules[:0]
	found := false
	for _, x := range rules {
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
	if err := proxySaveAll(kept); err != nil {
		fail(w, 500, "保存失败："+err.Error())
		return
	}
	ok(w, map[string]any{"deleted": id})
}

// handleProxyEntry /r/<id>/... → 进入浏览器代理。
// 直接 302 到 /b/{ver}/{scheme}/{host}/...，后续请求全部复用现有代理与防护。
func (s *Server) handleProxyEntry(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.RequestURI(), "/r/")
	parts := strings.SplitN(rest, "/", 2)
	id := parts[0]
	sub := "/"
	if len(parts) == 2 && parts[1] != "" {
		sub += parts[1]
	}
	rule, found := proxyFind(id)
	if !found {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`<body style="font:14px system-ui,sans-serif;padding:28px;color:#555">没有找到这条反向代理规则，请在「反向代理」页添加。</body>`))
		return
	}
	u, err := url.Parse(rule.Target)
	if err != nil {
		fail(w, 400, "目标地址非法")
		return
	}
	target := appBase + "/b/" + browserVerSeg() + "/" + u.Scheme + "/" + u.Host + sub
	http.Redirect(w, r, target, http.StatusFound)
}
