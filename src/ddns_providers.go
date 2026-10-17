package main

// DDNS 服务商实现：DuckDNS / 花生壳 / Cloudflare / NameCheap / NameSilo / DNSPod Token（简单协议）
// 签名家（阿里云/腾讯云/华为云/百度云）见 ddns_signed.go。

import (
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type dnsProvider interface {
	ensure(domain, sub, rt, ip string, ttl int) error
}

func newDNSProvider(t string, cred map[string]string) (dnsProvider, error) {
	b := providerBase{cred: cred}
	switch t {
	case "duckdns":
		return &duckDNS{b}, nil
	case "oray":
		return &orayDDNS{b}, nil
	case "cloudflare":
		return &cfDDNS{b}, nil
	case "namecheap":
		return &namecheapDDNS{b}, nil
	case "namesilo":
		return &namesiloDDNS{b}, nil
	case "alidns":
		return &aliDDNS{b}, nil
	case "tencentcloud":
		return &tcDDNS{b}, nil
	case "dnspod":
		return &dnspodDDNS{b}, nil
	case "huaweicloud":
		return &hwDDNS{b}, nil
	case "baiducloud":
		return &baiduDDNS{b}, nil
	default:
		return nil, fmt.Errorf("不支持的服务商: %s", t)
	}
}

type providerBase struct{ cred map[string]string }

func (p *providerBase) c(k string) string { return p.cred[k] }

// ============ DNSPod Token（旧版 dnsapi.cn，login_token=id,token） ============

type dnspodDDNS struct{ providerBase }

func (d *dnspodDDNS) ensure(domain, sub, rt, ip string, ttl int) error {
	id := d.c("id")
	token := d.c("token")
	if id == "" || token == "" {
		return fmt.Errorf("缺少 Token ID / Token")
	}
	recs, err := d.describe(domain, sub, rt)
	if err != nil {
		return err
	}
	var recID string
	for _, r := range recs {
		if r.Value == ip {
			return nil // 已是最新
		}
		recID = r.ID
	}
	if recID != "" {
		return d.modify(id, token, recID, domain, sub, rt, ip, ttl)
	}
	return d.create(id, token, domain, sub, rt, ip, ttl)
}

type dnspodRecord struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Type  string `json:"type"`
	Value string `json:"value"`
}

func (d *dnspodDDNS) describe(domain, sub, rt string) ([]dnspodRecord, error) {
	body, err := d.post("Record.List", domain, sub, rt, "", 0)
	if err != nil {
		return nil, err
	}
	var j struct {
		Status struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"status"`
		Records []dnspodRecord `json:"records"`
	}
	if err := jsonUnmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("DNSPod 响应解析失败: %s", string(body))
	}
	if j.Status.Code != "1" {
		return nil, fmt.Errorf("DNSPod 错误 %s: %s", j.Status.Code, j.Status.Message)
	}
	var out []dnspodRecord
	for _, r := range j.Records {
		if r.Name == sub && r.Type == rt {
			out = append(out, r)
		}
	}
	return out, nil
}

func (d *dnspodDDNS) create(id, token, domain, sub, rt, ip string, ttl int) error {
	_, err := d.postWithCreds(id, token, "Record.Create", domain, sub, rt, ip, ttl)
	return err
}

func (d *dnspodDDNS) modify(id, token, recID, domain, sub, rt, ip string, ttl int) error {
	_, err := d.postWithCredsAndID(id, token, "Record.Modify", domain, sub, rt, ip, ttl, recID)
	return err
}

func (d *dnspodDDNS) post(action, domain, sub, rt, ip string, ttl int) ([]byte, error) {
	id := d.c("id")
	token := d.c("token")
	return d.postWithCreds(id, token, action, domain, sub, rt, ip, ttl)
}

func (d *dnspodDDNS) postWithCreds(id, token, action, domain, sub, rt, ip string, ttl int) ([]byte, error) {
	return d.postWithCredsAndID(id, token, action, domain, sub, rt, ip, ttl, "")
}

func (d *dnspodDDNS) postWithCredsAndID(id, token, action, domain, sub, rt, ip string, ttl int, recID string) ([]byte, error) {
	if id == "" || token == "" {
		return nil, fmt.Errorf("缺少 Token ID / Token")
	}
	values := url.Values{}
	values.Set("login_token", id+","+token)
	values.Set("format", "json")
	values.Set("domain", domain)
	if sub != "" && sub != "@" {
		values.Set("sub_domain", sub)
	}
	if rt != "" {
		values.Set("record_type", rt)
	}
	if action == "Record.Create" || action == "Record.Modify" {
		values.Set("record_line", "默认")
		values.Set("value", ip)
		if ttl > 0 {
			values.Set("ttl", fmt.Sprintf("%d", ttl))
		}
	}
	if recID != "" {
		values.Set("record_id", recID)
	}
	req, _ := http.NewRequest(http.MethodPost, "https://dnsapi.cn/"+action, strings.NewReader(values.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", "X-Box/"+version+" (xiaom@fnos)")
	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := readAllLimit(resp.Body, 1<<20)
	var j struct {
		Status struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"status"`
	}
	_ = jsonUnmarshal(b, &j)
	if j.Status.Code != "" && j.Status.Code != "1" {
		return b, fmt.Errorf("DNSPod 错误 %s: %s", j.Status.Code, j.Status.Message)
	}
	if resp.StatusCode >= 300 {
		return b, fmt.Errorf("DNSPod HTTP %d: %s", resp.StatusCode, string(b))
	}
	return b, nil
}

func availableProviders() []map[string]string {
	return []map[string]string{
		{"type": "alidns", "name": "阿里云解析"},
		{"type": "tencentcloud", "name": "腾讯云"},
		{"type": "dnspod", "name": "DNSPod"},
		{"type": "huaweicloud", "name": "华为云"},
		{"type": "cloudflare", "name": "Cloudflare"},
		{"type": "baiducloud", "name": "百度云"},
		{"type": "duckdns", "name": "DuckDNS"},
		{"type": "oray", "name": "花生壳"},
		{"type": "namecheap", "name": "NameCheap"},
		{"type": "namesilo", "name": "NameSilo"},
	}
}

func readAllLimit(r io.Reader, n int64) ([]byte, error) {
	return io.ReadAll(io.LimitReader(r, n))
}

func httpGet(client *http.Client, u string) (string, error) {
	resp, err := client.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := readAllLimit(resp.Body, 1<<20)
	if err != nil {
		return "", err
	}
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return strings.TrimSpace(string(b)), nil
}

var defaultHTTPClient = &http.Client{Timeout: 12 * time.Second}

// ============ DuckDNS ============

type duckDNS struct{ providerBase }

func (d *duckDNS) ensure(domain, sub, rt, ip string, ttl int) error {
	// DuckDNS 仅支持 IPv4 或 IPv6；domain 形如 xxx.duckdns.org，sub=xxx
	token := d.c("token")
	if token == "" {
		return fmt.Errorf("缺少 token")
	}
	var u string
	if rt == "AAAA" {
		u = fmt.Sprintf("https://www.duckdns.org/update?domains=%s&token=%s&ipv6=%s&verbose=true", url.QueryEscape(sub), url.QueryEscape(token), url.QueryEscape(ip))
	} else {
		u = fmt.Sprintf("https://www.duckdns.org/update?domains=%s&token=%s&ip=%s&verbose=true", url.QueryEscape(sub), url.QueryEscape(token), url.QueryEscape(ip))
	}
	body, err := httpGet(defaultHTTPClient, u)
	if err != nil {
		return err
	}
	if !strings.HasPrefix(body, "OK") && !strings.Contains(body, "OK") {
		return fmt.Errorf("DuckDNS 返回未成功: %s", body)
	}
	return nil
}

// ============ 花生壳 Oray ============

type orayDDNS struct{ providerBase }

func (d *orayDDNS) ensure(domain, sub, rt, ip string, ttl int) error {
	user := d.c("username")
	pw := d.c("password")
	if user == "" || pw == "" {
		return fmt.Errorf("缺少用户名/密码")
	}
	host := domain // 花生壳用完整域名
	u := fmt.Sprintf("http://%s:%s@ddns.oray.com:80/ph/update?hostname=%s&myip=%s",
		url.QueryEscape(user), url.QueryEscape(pw), url.QueryEscape(host), url.QueryEscape(ip))
	body, err := httpGet(defaultHTTPClient, u)
	if err != nil {
		return err
	}
	// 成功: good / nochg；失败: nohost / abuse / !donator / 911
	if strings.HasPrefix(body, "good") || strings.HasPrefix(body, "nochg") {
		return nil
	}
	return fmt.Errorf("花生壳返回: %s", body)
}

// ============ Cloudflare ============

type cfDDNS struct{ providerBase }

func (d *cfDDNS) zoneID(domain string) (string, error) {
	token := d.c("apiToken")
	if token == "" {
		return "", fmt.Errorf("缺少 apiToken")
	}
	u := "https://api.cloudflare.com/client/v4/zones?name=" + url.QueryEscape(domain)
	resp, err := defaultHTTPClient.Get(u)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, _ := readAllLimit(resp.Body, 1<<20)
	var j struct {
		Success bool `json:"success"`
		Result  []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"result"`
	}
	if err := jsonUnmarshal(b, &j); err != nil {
		return "", err
	}
	if !j.Success || len(j.Result) == 0 {
		return "", fmt.Errorf("Cloudflare 查询 zone 失败: %s", string(b))
	}
	return j.Result[0].ID, nil
}

func (d *cfDDNS) ensure(domain, sub, rt, ip string, ttl int) error {
	token := d.c("apiToken")
	if token == "" {
		return fmt.Errorf("缺少 apiToken")
	}
	zid, err := d.zoneID(domain)
	if err != nil {
		return err
	}
	// 查已有记录
	u := fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records?name=%s&type=%s", zid, url.QueryEscape(domain), rt)
	resp, err := defaultHTTPClient.Get(u)
	if err != nil {
		return err
	}
	b, _ := readAllLimit(resp.Body, 1<<20)
	resp.Body.Close()
	var j struct {
		Success bool `json:"success"`
		Result  []struct {
			ID     string `json:"id"`
			Content string `json:"content"`
		} `json:"result"`
	}
	if err := jsonUnmarshal(b, &j); err != nil {
		return err
	}
	client := defaultHTTPClient
	if len(j.Result) > 0 && j.Result[0].Content == ip {
		return nil // 已是最新
	}
	body := fmt.Sprintf(`{"name":%q,"type":%q,"content":%q,"ttl":%d,"proxied":false}`, domain, rt, ip, ttlOr1(ttl))
	if len(j.Result) > 0 {
		// 更新
		ureq, _ := http.NewRequest(http.MethodPut, fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records/%s", zid, j.Result[0].ID), strings.NewReader(body))
		ureq.Header.Set("Authorization", "Bearer "+token)
		ureq.Header.Set("Content-Type", "application/json")
		return doCF(client, ureq)
	}
	// 新增
	creq, _ := http.NewRequest(http.MethodPost, fmt.Sprintf("https://api.cloudflare.com/client/v4/zones/%s/dns_records", zid), strings.NewReader(body))
	creq.Header.Set("Authorization", "Bearer "+token)
	creq.Header.Set("Content-Type", "application/json")
	return doCF(client, creq)
}

func doCF(client *http.Client, req *http.Request) error {
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := readAllLimit(resp.Body, 1<<20)
	var j struct {
		Success bool   `json:"success"`
		Errors  []any  `json:"errors"`
	}
	if err := jsonUnmarshal(b, &j); err != nil {
		return err
	}
	if !j.Success {
		return fmt.Errorf("Cloudflare 调用失败: %s", string(b))
	}
	return nil
}

// ============ NameCheap ============

type namecheapDDNS struct{ providerBase }

func (d *namecheapDDNS) ensure(domain, sub, rt, ip string, ttl int) error {
	pw := d.c("apiKey")
	user := d.c("userName")
	if pw == "" || user == "" {
		return fmt.Errorf("缺少 apiKey / userName")
	}
	if rt == "AAAA" {
		return fmt.Errorf("NameCheap 仅支持 A 记录")
	}
	u := fmt.Sprintf("https://dynamicdns.park-your-domain.com/update?host=%s&domain=%s&password=%s&ip=%s",
		url.QueryEscape(sub), url.QueryEscape(domain), url.QueryEscape(pw), url.QueryEscape(ip))
	body, err := httpGet(defaultHTTPClient, u)
	if err != nil {
		return err
	}
	if !strings.Contains(body, "<ErrCount>0</ErrCount>") {
		return fmt.Errorf("NameCheap 返回: %s", body)
	}
	return nil
}

// ============ NameSilo ============

type namesiloDDNS struct{ providerBase }

type namesiloListResp struct {
	XMLName xml.Name `xml:"namesilo"`
	Reply   struct {
		Code    int    `xml:"code"`
		Detail  string `xml:"detail"`
		Resource struct {
			Records []struct {
				ID    string `xml:"record_id,attr"`
				Host  string `xml:"host"`
				Type  string `xml:"type"`
				Value string `xml:"value"`
			} `xml:"records>resource_record"`
		} `xml:"reply>resource"`
	} `xml:"reply"`
}

func (d *namesiloDDNS) ensure(domain, sub, rt, ip string, ttl int) error {
	key := d.c("apiKey")
	if key == "" {
		return fmt.Errorf("缺少 apiKey")
	}
	if rt == "AAAA" {
		return fmt.Errorf("NameSilo 动态接口仅支持 A 记录")
	}
	// 列出
	u := fmt.Sprintf("https://www.namesilo.com/api/dnsListRecords?version=1&type=xml&key=%s&domain=%s", url.QueryEscape(key), url.QueryEscape(domain))
	body, err := httpGet(defaultHTTPClient, u)
	if err != nil {
		return err
	}
	var lr namesiloListResp
	if err := xml.Unmarshal([]byte(body), &lr); err != nil {
		return err
	}
	if lr.Reply.Code != 300 {
		return fmt.Errorf("NameSilo 列表失败: %s", lr.Reply.Detail)
	}
	var recID string
	for _, r := range lr.Reply.Resource.Records {
		if r.Host == domain && r.Type == rt {
			recID = r.ID
			break
		}
	}
	if recID != "" {
		u = fmt.Sprintf("https://www.namesilo.com/api/dnsUpdateRecord?version=1&type=xml&key=%s&domain=%s&rrid=%s&rrhost=%s&rrvalue=%s&rrttl=%d",
			url.QueryEscape(key), url.QueryEscape(domain), url.QueryEscape(recID), url.QueryEscape(sub), url.QueryEscape(ip), ttlOr1(ttl))
	} else {
		u = fmt.Sprintf("https://www.namesilo.com/api/dnsAddRecord?version=1&type=xml&key=%s&domain=%s&rrtype=%s&rrhost=%s&rrvalue=%s&rrttl=%d",
			url.QueryEscape(key), url.QueryEscape(domain), rt, url.QueryEscape(sub), url.QueryEscape(ip), ttlOr1(ttl))
	}
	body, err = httpGet(defaultHTTPClient, u)
	if err != nil {
		return err
	}
	if !strings.Contains(body, "<code>300</code>") {
		return fmt.Errorf("NameSilo 写入失败: %s", body)
	}
	return nil
}

func ttlOr1(ttl int) int {
	if ttl <= 0 {
		return 600
	}
	return ttl
}
