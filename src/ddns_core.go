package main

// DDNS 模块：配置模型、持久化、调度、IP 来源解析、HTTP API。
// 服务商实现对齐飞牛内置（腾讯云/DNSPod、阿里云、华为云、花生壳、DuckDNS、Cloudflare）
// 并扩展百度云、Namecheap、NameSilo，共 9 家；反向代理可联动自动补记录。

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ============ 模型 ============

type DDNSProvider struct {
	ID   uint            `json:"id"`
	Name string          `json:"name"`
	Type string          `json:"type"` // alidns / tencentcloud / huaweicloud / cloudflare / baiducloud / duckdns / oray / namecheap / namesilo
	Cred map[string]string `json:"cred"`
}

type DDNSRecord struct {
	ID          uint      `json:"id"`
	Enabled     bool      `json:"enabled"`
	ProviderID  uint      `json:"providerId"`
	Domain      string    `json:"domain"`     // 完整记录名，如 nas.example.com
	RecordType  string    `json:"recordType"` // A / AAAA
	IPSource    string    `json:"ipSource"`   // auto / web / interface / custom / dns
	IPSourceArg string    `json:"ipSourceArg"`
	TTL         int       `json:"ttl"`
	AutoCreated bool      `json:"autoCreated"` // 反向代理联动自动创建
	LastIP      string    `json:"lastIP"`
	LastStatus  string    `json:"lastStatus"` // pending / success / failed
	LastMessage string    `json:"lastMessage"`
	LastCheckAt time.Time `json:"lastCheckAt"`
	LastSyncAt  time.Time `json:"lastSyncAt"`
}

type DDNSConfig struct {
	IntervalSec int            `json:"intervalSec"` // 全局同步间隔，默认 300
	Providers   []DDNSProvider `json:"providers"`
	Records     []DDNSRecord   `json:"records"`
}

// ============ 运行时 ============

type ddnsRuntime struct {
	mu        sync.RWMutex
	cfg       DDNSConfig
	path      string
	stop      chan struct{}
	done      chan struct{}
	triggerCh chan struct{}
	nextID    uint
}

var ddns *ddnsRuntime

func initDDNS(dataDir string) {
	r := &ddnsRuntime{
		path:      filepath.Join(dataDir, "ddns.json"),
		stop:      make(chan struct{}),
		done:      make(chan struct{}),
		triggerCh: make(chan struct{}, 4),
	}
	r.cfg.IntervalSec = 300
	if b, err := os.ReadFile(r.path); err == nil {
		_ = json.Unmarshal(b, &r.cfg)
	}
	if r.cfg.IntervalSec < 30 {
		r.cfg.IntervalSec = 30
	}
	// 计算下一个 ID
	for _, p := range r.cfg.Providers {
		if p.ID > r.nextID {
			r.nextID = p.ID
		}
	}
	for _, rec := range r.cfg.Records {
		if rec.ID > r.nextID {
			r.nextID = rec.ID
		}
	}
	ddns = r
	go r.loop()
}

func (r *ddnsRuntime) loop() {
	defer close(r.done)
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-r.stop:
			return
		case <-r.triggerCh:
		case <-ticker.C:
		}
		r.scan()
	}
}

func (r *ddnsRuntime) scan() {
	r.mu.RLock()
	cfg := r.cfg
	r.mu.RUnlock()
	for i := range cfg.Records {
		rec := &cfg.Records[i]
		if !rec.Enabled {
			continue
		}
		due := rec.LastCheckAt.IsZero() || time.Since(rec.LastCheckAt) >= time.Duration(cfg.IntervalSec)*time.Second
		if !due {
			continue
		}
		r.syncRecord(rec)
	}
	r.mu.Lock()
	_ = r.saveLocked()
	r.mu.Unlock()
}

func (r *ddnsRuntime) syncRecord(rec *DDNSRecord) {
	rec.LastCheckAt = time.Now()
	ip, err := r.resolveIP(rec)
	if err != nil {
		rec.LastStatus = "failed"
		rec.LastMessage = "IP 解析失败: " + err.Error()
		return
	}
	prov := r.providerByID(rec.ProviderID)
	if prov == nil {
		rec.LastStatus = "failed"
		rec.LastMessage = "找不到服务商"
		return
	}
	client, err := newDNSProvider(prov.Type, prov.Cred)
	if err != nil {
		rec.LastStatus = "failed"
		rec.LastMessage = err.Error()
		return
	}
	sub, domain := splitDomain(rec.Domain)
	err = client.ensure(domain, sub, rec.RecordType, ip, rec.TTL)
	if err != nil {
		rec.LastStatus = "failed"
		rec.LastMessage = err.Error()
		return
	}
	if ip != rec.LastIP {
		rec.LastMessage = fmt.Sprintf("已更新 %s → %s", rec.LastIP, ip)
	}
	rec.LastIP = ip
	rec.LastStatus = "success"
	rec.LastSyncAt = time.Now()
}

func (r *ddnsRuntime) providerByID(id uint) *DDNSProvider {
	for i := range r.cfg.Providers {
		if r.cfg.Providers[i].ID == id {
			return &r.cfg.Providers[i]
		}
	}
	return nil
}

func (r *ddnsRuntime) resolveIP(rec *DDNSRecord) (string, error) {
	switch rec.IPSource {
	case "custom":
		if net.ParseIP(rec.IPSourceArg) == nil {
			return "", fmt.Errorf("自定义 IP 非法")
		}
		return rec.IPSourceArg, nil
	case "interface":
		if rec.RecordType == "AAAA" {
			return nicIPv6(rec.IPSourceArg)
		}
		return nicIPv4(rec.IPSourceArg)
	case "dns":
		ips, err := net.LookupIP(rec.IPSourceArg)
		if err != nil {
			return "", err
		}
		for _, ip := range ips {
			if rec.RecordType == "AAAA" && ip.To4() == nil {
				return ip.String(), nil
			}
			if rec.RecordType != "AAAA" && ip.To4() != nil {
				return ip.String(), nil
			}
		}
		return "", fmt.Errorf("DNS 解析无匹配类型记录")
	case "web":
		if rec.IPSourceArg != "" {
			client := &http.Client{Timeout: 5 * time.Second}
			resp, err := client.Get(rec.IPSourceArg)
			if err != nil {
				return "", err
			}
			defer resp.Body.Close()
			b, _ := readAllLimit(resp.Body, 256)
			ip := strings.TrimSpace(string(b))
			if net.ParseIP(ip) == nil {
				return "", fmt.Errorf("web 源返回非 IP: %s", ip)
			}
			if rec.RecordType == "AAAA" && !strings.Contains(ip, ":") {
				return "", fmt.Errorf("web 源返回非 IPv6")
			}
			return ip, nil
		}
		fallthrough
	default: // auto
		family := 4
		if rec.RecordType == "AAAA" {
			family = 6
		}
		ip, _, err := queryPublicIP(family)
		if err != nil {
			return "", err
		}
		return ip, nil
	}
}

func (r *ddnsRuntime) saveLocked() error {
	b, err := json.MarshalIndent(r.cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(r.path, b, 0o600)
}

func (r *ddnsRuntime) trigger() {
	select {
	case r.triggerCh <- struct{}{}:
	default:
	}
}

// ============ HTTP API ============

func (s *Server) handleDDNSList(w http.ResponseWriter, r *http.Request) {
	ddns.mu.RLock()
	defer ddns.mu.RUnlock()
	ok(w, map[string]any{
		"intervalSec":    ddns.cfg.IntervalSec,
		"providers":      ddns.cfg.Providers,
		"records":        ddns.cfg.Records,
		"providersAvail": availableProviders(),
		"ifaces":         physicalIfaces(),
	})
}

// ifaceInfo 网卡信息（供 DDNS「网卡地址」IP 来源下拉展示名称与对应 IP）。
type ifaceInfo struct {
	Name  string `json:"name"`
	IPv4  string `json:"ipv4"`
	IPv6  string `json:"ipv6"`
}

// physicalIfaces 物理网卡（含 IPv4/IPv6）列表（供 DDNS「网卡地址」IP 来源下拉选择）：
// 排除 lo、docker/veth/br- 等虚拟网卡，仅保留可用于公网访问的真实网卡。
func physicalIfaces() []ifaceInfo {
	var out []ifaceInfo
	for _, n := range listNICs() {
		name := n.Name
		if name == "lo" || strings.HasPrefix(name, "docker") || strings.HasPrefix(name, "veth") ||
			strings.HasPrefix(name, "br-") || strings.HasPrefix(name, "virbr") || strings.HasPrefix(name, "tun") ||
			strings.HasPrefix(name, "tap") {
			continue
		}
		out = append(out, ifaceInfo{Name: name, IPv4: n.IPv4, IPv6: n.IPv6})
	}
	if len(out) == 0 {
		out = append(out, ifaceInfo{Name: "eth0", IPv4: "", IPv6: ""})
	}
	return out
}

// handleDDNSProviderTest POST /api/ddns/provider/test {id}：服务商连通性测试。
// 用保存的凭据做一次真实轻量 API 调用（只读/最小请求，不修改任何解析记录）。
func (s *Server) handleDDNSProviderTest(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID uint `json:"id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ID == 0 {
		fail(w, 400, "参数错误")
		return
	}
	ddns.mu.RLock()
	var prov *DDNSProvider
	for i := range ddns.cfg.Providers {
		if ddns.cfg.Providers[i].ID == req.ID {
			p := ddns.cfg.Providers[i]
			prov = &p
			break
		}
	}
	ddns.mu.RUnlock()
	if prov == nil {
		fail(w, 404, "服务商不存在")
		return
	}
	if err := testDNSProvider(prov.Type, prov.Cred); err != nil {
		// 注意：必须走 fail（非 200 + ok:false），前端才能进入 catch 显示失败态
		fail(w, 400, err.Error())
		return
	}
	ok(w, map[string]any{"message": "连接正常，凭据有效"})
}

type ddnsProviderReq struct {
	ID   uint            `json:"id"`
	Name string          `json:"name"`
	Type string          `json:"type"`
	Cred map[string]string `json:"cred"`
}

func (s *Server) handleDDNSProviderSave(w http.ResponseWriter, r *http.Request) {	var req ddnsProviderReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	if req.Type == "" || !providerSupported(req.Type) {
		fail(w, 400, "不支持的服务商类型")
		return
	}
	ddns.mu.Lock()
	defer ddns.mu.Unlock()
	if req.ID == 0 {
		ddns.nextID++
		req.ID = ddns.nextID
		ddns.cfg.Providers = append(ddns.cfg.Providers, DDNSProvider{ID: req.ID, Name: req.Name, Type: req.Type, Cred: req.Cred})
	} else {
		found := false
		for i := range ddns.cfg.Providers {
			if ddns.cfg.Providers[i].ID == req.ID {
				ddns.cfg.Providers[i].Name = req.Name
				ddns.cfg.Providers[i].Type = req.Type
				ddns.cfg.Providers[i].Cred = req.Cred
				found = true
				break
			}
		}
		if !found {
			fail(w, 400, "服务商不存在")
			return
		}
	}
	_ = ddns.saveLocked()
	ok(w, map[string]any{"id": req.ID})
}

func (s *Server) handleDDNSProviderDel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseUint(r.URL.Query().Get("id"), 10, 64)
	ddns.mu.Lock()
	defer ddns.mu.Unlock()
	ddns.cfg.Providers = filterProviders(ddns.cfg.Providers, uint(id))
	ddns.cfg.Records = filterRecordsByProvider(ddns.cfg.Records, uint(id))
	_ = ddns.saveLocked()
	ok(w, map[string]any{"ok": true})
}

type ddnsRecordReq struct {
	ID          uint   `json:"id"`
	Enabled     bool   `json:"enabled"`
	ProviderID  uint   `json:"providerId"`
	Domain      string `json:"domain"`
	RecordType  string `json:"recordType"`
	IPSource    string `json:"ipSource"`
	IPSourceArg string `json:"ipSourceArg"`
	TTL         int    `json:"ttl"`
}

func (s *Server) handleDDNSRecordSave(w http.ResponseWriter, r *http.Request) {
	var req ddnsRecordReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		fail(w, 400, "参数错误")
		return
	}
	if req.Domain == "" {
		fail(w, 400, "域名不能为空")
		return
	}
	if req.RecordType != "A" && req.RecordType != "AAAA" {
		req.RecordType = "A"
	}
	if req.IPSource == "" {
		req.IPSource = "auto"
	}
	ddns.mu.Lock()
	defer ddns.mu.Unlock()
	if req.ID == 0 {
		ddns.nextID++
		req.ID = ddns.nextID
		ddns.cfg.Records = append(ddns.cfg.Records, DDNSRecord{
			ID: req.ID, Enabled: req.Enabled, ProviderID: req.ProviderID, Domain: req.Domain,
			RecordType: req.RecordType, IPSource: req.IPSource, IPSourceArg: req.IPSourceArg, TTL: req.TTL,
			LastStatus: "pending",
		})
	} else {
		found := false
		for i := range ddns.cfg.Records {
			if ddns.cfg.Records[i].ID == req.ID {
				rec := &ddns.cfg.Records[i]
				rec.Enabled = req.Enabled
				rec.ProviderID = req.ProviderID
				rec.Domain = req.Domain
				rec.RecordType = req.RecordType
				rec.IPSource = req.IPSource
				rec.IPSourceArg = req.IPSourceArg
				rec.TTL = req.TTL
				found = true
				break
			}
		}
		if !found {
			fail(w, 400, "记录不存在")
			return
		}
	}
	_ = ddns.saveLocked()
	ddns.trigger()
	ok(w, map[string]any{"id": req.ID})
}

func (s *Server) handleDDNSRecordDel(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseUint(r.URL.Query().Get("id"), 10, 64)
	ddns.mu.Lock()
	defer ddns.mu.Unlock()
	ddns.cfg.Records = filterRecords(ddns.cfg.Records, uint(id))
	_ = ddns.saveLocked()
	ok(w, map[string]any{"ok": true})
}

func (s *Server) handleDDNSSync(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseUint(r.URL.Query().Get("id"), 10, 64)
	ddns.mu.RLock()
	cfg := ddns.cfg
	ddns.mu.RUnlock()
	if id == 0 {
		ddns.trigger()
		ok(w, map[string]any{"ok": true, "message": "已触发全量同步"})
		return
	}
	for i := range cfg.Records {
		if cfg.Records[i].ID == uint(id) {
			rec := cfg.Records[i]
			go func() {
				ddns.mu.RLock()
				// 在锁外跑同步（同步会再取锁保存）
				ddns.mu.RUnlock()
				ddns.syncRecord(&rec)
				ddns.mu.Lock()
				// 写回
				for j := range ddns.cfg.Records {
					if ddns.cfg.Records[j].ID == rec.ID {
						ddns.cfg.Records[j] = rec
						break
					}
				}
				_ = ddns.saveLocked()
				ddns.mu.Unlock()
			}()
			ok(w, map[string]any{"ok": true, "message": "已触发单条同步"})
			return
		}
	}
	fail(w, 404, "记录不存在")
}

// ============ 工具 ============

func splitDomain(full string) (sub, domain string) {
	full = strings.TrimSpace(full)
	full = strings.TrimSuffix(full, ".")
	parts := strings.Split(full, ".")
	if len(parts) <= 2 {
		return "@", full // 根域名（如 example.com）
	}
	return strings.Join(parts[:len(parts)-2], "."), strings.Join(parts[len(parts)-2:], ".")
}

func filterProviders(in []DDNSProvider, id uint) []DDNSProvider {
	out := in[:0]
	for _, p := range in {
		if p.ID != id {
			out = append(out, p)
		}
	}
	return out
}

func filterRecords(in []DDNSRecord, id uint) []DDNSRecord {
	out := in[:0]
	for _, r := range in {
		if r.ID != id {
			out = append(out, r)
		}
	}
	return out
}

func filterRecordsByProvider(in []DDNSRecord, pid uint) []DDNSRecord {
	out := in[:0]
	for _, r := range in {
		if r.ProviderID != pid {
			out = append(out, r)
		}
	}
	return out
}

func providerSupported(t string) bool {
	for _, p := range availableProviders() {
		if p["type"] == t {
			return true
		}
	}
	return false
}

// setDNSRecordStatus 反代联动创建记录后写回状态时调用（占位，保持接口稳定）。
func setDNSRecordStatus(id uint, status, msg string) {
	ddns.mu.Lock()
	defer ddns.mu.Unlock()
	for i := range ddns.cfg.Records {
		if ddns.cfg.Records[i].ID == id {
			ddns.cfg.Records[i].LastStatus = status
			ddns.cfg.Records[i].LastMessage = msg
			_ = ddns.saveLocked()
			return
		}
	}
}

var _ = readAllLimit
