package main

// 反向代理模块：域名 → 内网后端转发，并与 DDNS 联动（站点域名自动补记录）。
// 简化端口管理：站点统一挂主服务端口（service_port=8899），由上游 NAT/路由把域名流量引到 8899。

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// tlsInsecure 内网自签证书场景：跳过证书校验（仅影响反代到后端，不对外）。
func tlsInsecure() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true}
}

type ProxySite struct {
	ID          uint     `json:"id"`
	Hosts       []string `json:"hosts"`      // 域名列表（支持 * 通配一层）
	PathPrefix  string   `json:"pathPrefix"` // 空=整站
	StripPrefix bool     `json:"stripPrefix"`
	Backend     string   `json:"backend"`    // http://192.168.1.20:8080
	ListenPort  int      `json:"listenPort"` // 0=挂主服务端口 8899
	Enabled     bool     `json:"enabled"`
	Description string   `json:"description"`
	AutoDDNS    bool     `json:"autoDDNS"`   // 联动：自动为域名创建 DDNS 记录
}

type ProxyConfig struct {
	Sites []ProxySite `json:"sites"`
}

type proxyRuntime struct {
	mu      sync.RWMutex
	cfg     ProxyConfig
	path    string
	nextID  uint
	servers map[int]*http.Server // 独立端口监听
}

var proxyRT *proxyRuntime

func initProxy(dataDir string) {
	r := &proxyRuntime{
		path:    filepath.Join(dataDir, "proxy.json"),
		servers: map[int]*http.Server{},
	}
	if b, err := os.ReadFile(r.path); err == nil {
		_ = json.Unmarshal(b, &r.cfg)
	}
	for _, s := range r.cfg.Sites {
		if s.ID > r.nextID {
			r.nextID = s.ID
		}
	}
	proxyRT = r
}

func (r *proxyRuntime) save() error {
	b, err := json.MarshalIndent(r.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.path, b, 0o600)
}

// matchProxy 按 Host + 路径匹配站点，返回转发目标与剩余路径。
func (r *proxyRuntime) match(hostHeader, path string) (*ProxySite, string, error) {
	host := strings.ToLower(strings.TrimSuffix(strings.Split(hostHeader, ":")[0], "."))
	pp, err := url.Parse(path)
	if err != nil {
		return nil, "", err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	for i := range r.cfg.Sites {
		s := &r.cfg.Sites[i]
		if !s.Enabled {
			continue
		}
		if !hostMatches(s.Hosts, host) {
			continue
		}
		prefix := normalizePrefix(s.PathPrefix)
		if prefix != "" && !strings.HasPrefix(pp.Path, prefix) {
			continue
		}
		return s, pp.Path, nil
	}
	return nil, "", nil
}

func hostMatches(hosts []string, host string) bool {
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSuffix(strings.TrimSpace(h), "."))
		if h == "" || h == host {
			continue
		}
		if h == "*" {
			return true
		}
		if strings.HasPrefix(h, "*.") {
			suffix := h[1:] // .example.com
			if strings.HasSuffix(host, suffix) && strings.Count(host, ".") == strings.Count(suffix, ".") {
				return true
			}
		} else if h == host {
			return true
		}
	}
	return false
}

func normalizePrefix(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	p = strings.TrimSuffix(p, "/")
	if p == "" {
		p = "/"
	}
	return p
}

// handleRoot 主服务 "/"：先尝试反代，否则返回应用页面。
func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if site, _, _ := proxyRT.match(r.Host, r.URL.Path); site != nil {
		proxyRT.serve(site, w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// 注入版本标识到登录卡片：便于确认客户端实际加载的构建版本（iframe 缓存/未重启排障）
	page := bytes.Replace(indexHTML, []byte("<!--XBOX_VER-->"), []byte("v"+version), 1)
	_, _ = w.Write(page)
}

func (r *proxyRuntime) serve(site *ProxySite, w http.ResponseWriter, req *http.Request) {
	backend, err := url.Parse(site.Backend)
	if err != nil {
		http.Error(w, "后端地址非法: "+err.Error(), 502)
		return
	}
	if backend.Scheme == "" {
		backend.Scheme = "http"
	}
	prefix := normalizePrefix(site.PathPrefix)
	rp := &httputil.ReverseProxy{
		Director: func(out *http.Request) {
			out.URL.Scheme = backend.Scheme
			out.URL.Host = backend.Host
			out.Host = backend.Host // 关键：保持 Host，避免 Jellyfin 等回跳内网 IP
			out.Header.Set("X-Forwarded-For", req.RemoteAddr)
			out.Header.Set("X-Forwarded-Host", req.Host)
			out.Header.Set("X-Forwarded-Uri", req.RequestURI)
			path := req.URL.Path
			if site.StripPrefix && prefix != "" {
				path = strings.TrimPrefix(path, prefix)
				if path == "" {
					path = "/"
				}
			}
			out.URL.Path = singleJoiningSlash(backend.Path, path)
		},
		Transport: &http.Transport{
			Proxy: nil, // 不走环境代理
			DialContext: (&net.Dialer{Timeout: 3 * time.Second}).DialContext,
			TLSClientConfig: tlsInsecure(),
		},
		FlushInterval: -1, // 等价 proxy_buffering off，支持 SSE/流式
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("X-Frame-Options")
			return nil
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, e error) {
			w.Header().Set("Cache-Control", "no-store")
			http.Error(w, fmt.Sprintf("后端 %s 不可达：%s", site.Backend, e.Error()), 502)
		},
	}
	rp.ServeHTTP(w, req)
}

func singleJoiningSlash(a, b string) string {
	if b == "" {
		return a
	}
	aslash := strings.HasSuffix(a, "/")
	bslash := strings.HasPrefix(b, "/")
	switch {
	case aslash && bslash:
		return a + b[1:]
	case !aslash && !bslash:
		return a + "/" + b
	}
	return a + b
}

// ============ API ============

func (s *Server) handleProxyList(w http.ResponseWriter, r *http.Request) {
	proxyRT.mu.RLock()
	sites := proxyRT.cfg.Sites
	proxyRT.mu.RUnlock()
	ok(w, map[string]any{"sites": sites})
}

type proxySiteReq struct {
	ID          uint     `json:"id"`
	Hosts       []string `json:"hosts"`
	PathPrefix  string   `json:"pathPrefix"`
	StripPrefix bool     `json:"stripPrefix"`
	Backend     string   `json:"backend"`
	Enabled     bool     `json:"enabled"`
	Description string   `json:"description"`
	AutoDDNS    bool     `json:"autoDDNS"`
}

func (s *Server) handleProxySave(w http.ResponseWriter, r *http.Request) {
	var req proxySiteReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	if len(req.Hosts) == 0 || req.Backend == "" {
		fail(w, 400, "域名与后端地址不能为空")
		return
	}
	if !strings.HasPrefix(req.Backend, "http://") && !strings.HasPrefix(req.Backend, "https://") {
		fail(w, 400, "后端地址需以 http:// 或 https:// 开头")
		return
	}
	proxyRT.mu.Lock()
	if req.ID == 0 {
		proxyRT.nextID++
		req.ID = proxyRT.nextID
		proxyRT.cfg.Sites = append(proxyRT.cfg.Sites, ProxySite{
			ID: req.ID, Hosts: req.Hosts, PathPrefix: req.PathPrefix, StripPrefix: req.StripPrefix,
			Backend: req.Backend, Enabled: req.Enabled, Description: req.Description, AutoDDNS: req.AutoDDNS,
		})
	} else {
		found := false
		for i := range proxyRT.cfg.Sites {
			if proxyRT.cfg.Sites[i].ID == req.ID {
				site := &proxyRT.cfg.Sites[i]
				site.Hosts = req.Hosts
				site.PathPrefix = req.PathPrefix
				site.StripPrefix = req.StripPrefix
				site.Backend = req.Backend
				site.Enabled = req.Enabled
				site.Description = req.Description
				site.AutoDDNS = req.AutoDDNS
				found = true
				break
			}
		}
		if !found {
			fail(w, 400, "站点不存在")
			proxyRT.mu.Unlock()
			return
		}
	}
	// 联动 DDNS：为每个域名自动补记录
	msg := ""
	if req.AutoDDNS {
		created, err := linkProxyToDDNS(req.Hosts, req.ID)
		if err != nil {
			msg = "（DDNS 联动: " + err.Error() + "）"
		} else if created > 0 {
			msg = fmt.Sprintf("（DDNS 联动：自动创建 %d 条记录）", created)
		}
	}
	_ = proxyRT.save()
	proxyRT.mu.Unlock()
	ok(w, map[string]any{"id": req.ID, "message": msg})
}

func (s *Server) handleProxyDel(w http.ResponseWriter, r *http.Request) {
	id, _ := decodeID(r, "id")
	proxyRT.mu.Lock()
	var hosts []string
	for i := range proxyRT.cfg.Sites {
		if proxyRT.cfg.Sites[i].ID == id {
			hosts = proxyRT.cfg.Sites[i].Hosts
			break
		}
	}
	proxyRT.cfg.Sites = filterProxySites(proxyRT.cfg.Sites, id)
	_ = proxyRT.save()
	proxyRT.mu.Unlock()

	// 删除联动创建的自动 DDNS 记录（仅清本站点域名）
	removed := 0
	if len(hosts) > 0 && ddns != nil {
		ddns.mu.Lock()
		kept := ddns.cfg.Records[:0]
		for _, rec := range ddns.cfg.Records {
			if rec.AutoCreated && hostIn(hosts, rec.Domain) {
				removed++
				continue
			}
			kept = append(kept, rec)
		}
		ddns.cfg.Records = kept
		_ = ddns.saveLocked()
		ddns.mu.Unlock()
	}
	ok(w, map[string]any{"ok": true, "removedDDNS": removed})
}

// hostIn 判断域名是否落在给定主机列表（精确匹配，忽略大小写）。
func hostIn(hosts []string, domain string) bool {
	d := strings.ToLower(strings.TrimSpace(domain))
	for _, h := range hosts {
		if strings.ToLower(strings.TrimSpace(h)) == d {
			return true
		}
	}
	return false
}

func filterProxySites(in []ProxySite, id uint) []ProxySite {
	out := in[:0]
	for _, s := range in {
		if s.ID != id {
			out = append(out, s)
		}
	}
	return out
}

// linkProxyToDDNS 为站点域名自动创建 DDNS 记录（联动）。
func linkProxyToDDNS(hosts []string, siteID uint) (int, error) {
	if ddns == nil || len(ddns.cfg.Providers) == 0 {
		return 0, fmt.Errorf("请先配置至少一个 DDNS 服务商")
	}
	created := 0
	for _, h := range hosts {
		h = strings.ToLower(strings.TrimSpace(h))
		if h == "" || h == "*" || strings.HasPrefix(h, "*.") {
			continue
		}
		exists := false
		for _, rec := range ddns.cfg.Records {
			if rec.Domain == h && rec.AutoCreated {
				exists = true
				break
			}
		}
		if exists {
			continue
		}
		ddns.nextID++
		ddns.cfg.Records = append(ddns.cfg.Records, DDNSRecord{
			ID: ddns.nextID, Enabled: true, ProviderID: ddns.cfg.Providers[0].ID,
			Domain: h, RecordType: "A", IPSource: "auto", TTL: 600,
			AutoCreated: true, LastStatus: "pending",
		})
		created++
	}
	if created > 0 {
		ddns.mu.Lock()
		_ = ddns.saveLocked()
		ddns.mu.Unlock()
		ddns.trigger()
	}
	return created, nil
}

func decodeID(r *http.Request, key string) (uint, error) {
	return parseUint(r.URL.Query().Get(key))
}

func parseUint(s string) (uint, error) {
	var v uint64
	_, err := fmt.Sscanf(s, "%d", &v)
	return uint(v), err
}
