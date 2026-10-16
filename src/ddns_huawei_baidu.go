package main

// DDNS 签名类服务商：华为云 / 百度云。
import (
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ============ 华为云（SDK-HMAC-SHA256） ============

type hwDDNS struct{ providerBase }

func (d *hwDDNS) ensure(domain, sub, rt, ip string, ttl int) error {
	ak := d.c("accessKeyId")
	sk := d.c("accessKeySecret")
	region := d.c("region")
	if region == "" {
		region = "cn-north-4"
	}
	projectID := d.c("projectId")
	if ak == "" || sk == "" || projectID == "" {
		return fmt.Errorf("缺少 accessKeyId / accessKeySecret / projectId")
	}
	host := "dns." + region + ".myhuaweicloud.com"
	// 查 zone
	zoneID, err := d.zoneID(host, ak, sk, region, domain)
	if err != nil {
		return err
	}
	// 查记录
	recID, curIP, err := d.findRecord(host, ak, sk, region, projectID, zoneID, domain, rt)
	if err != nil {
		return err
	}
	if curIP == ip {
		return nil
	}
	if recID != "" {
		return d.hwPut(host, ak, sk, region, projectID, recID, domain, rt, ip, ttl)
	}
	return d.hwPost(host, ak, sk, region, projectID, zoneID, domain, rt, ip, ttl)
}

func (d *hwDDNS) zoneID(host, ak, sk, region, domain string) (string, error) {
	body, err := hwRequest(http.MethodGet, host, ak, sk, region, "/v2.1/zones?name="+domain, "")
	if err != nil {
		return "", err
	}
	var j struct {
		Zones []struct {
			ID   string `json:"id"`
			Name string `json:"name"`
		} `json:"zones"`
	}
	if err := jsonUnmarshal(body, &j); err != nil {
		return "", fmt.Errorf("华为云 zone 解析失败: %s", string(body))
	}
	if len(j.Zones) == 0 {
		return "", fmt.Errorf("华为云未找到域名 %s 的 zone", domain)
	}
	return j.Zones[0].ID, nil
}

func (d *hwDDNS) findRecord(host, ak, sk, region, projectID, zoneID, full, rt string) (string, string, error) {
	body, err := hwRequest(http.MethodGet, host, ak, sk, region, "/v2.1/"+projectID+"/recordsets?name="+full+"&type="+rt, "")
	if err != nil {
		return "", "", err
	}
	var j struct {
		Recordsets []struct {
			ID     string   `json:"id"`
			Name   string   `json:"name"`
			Type   string   `json:"type"`
			Records []string `json:"records"`
		} `json:"recordsets"`
	}
	if err := jsonUnmarshal(body, &j); err != nil {
		return "", "", fmt.Errorf("华为云记录解析失败: %s", string(body))
	}
	for _, r := range j.Recordsets {
		if r.Name == full+"." && r.Type == rt && len(r.Records) > 0 {
			return r.ID, r.Records[0], nil
		}
	}
	return "", "", nil
}

func (d *hwDDNS) hwPost(host, ak, sk, region, projectID, zoneID, full, rt, ip string, ttl int) error {
	payload := map[string]any{
		"name":    full,
		"type":    rt,
		"records": []string{ip},
		"ttl":     ttlOr1(ttl),
	}
	b, _ := json.Marshal(payload)
	_, err := hwRequest(http.MethodPost, host, ak, sk, region, "/v2.1/"+projectID+"/zones/"+zoneID+"/recordsets", string(b))
	return err
}

func (d *hwDDNS) hwPut(host, ak, sk, region, projectID, recID, full, rt, ip string, ttl int) error {
	payload := map[string]any{
		"name":    full,
		"type":    rt,
		"records": []string{ip},
		"ttl":     ttlOr1(ttl),
	}
	b, _ := json.Marshal(payload)
	_, err := hwRequest(http.MethodPut, host, ak, sk, region, "/v2.1/"+projectID+"/recordsets/"+recID, string(b))
	return err
}

func hwRequest(method, host, ak, sk, region, path, body string) ([]byte, error) {
	t := time.Now().UTC().Format("20060102T150405Z")
	signedHeaders := "host;x-sdk-date"
	canonicalHeaders := "host:" + host + "\nx-sdk-date:" + t + "\n"
	payloadHash := sha256Hex(body)
	canonicalReq := method + "\n" + path + "\n" + "" + "\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + payloadHash

	// 签名密钥
	kDate := hmacSHA256([]byte(sk), "20191125")
	kRegion := hmacSHA256(kDate, region)
	kService := hmacSHA256(kRegion, "dns")
	kSigning := hmacSHA256(kService, "bce_request")

	scope := "20191125/" + region + "/dns/bce_request"
	stringToSign := "SDK-HMAC-SHA256\n" + t + "\n" + scope + "\n" + sha256Hex(canonicalReq)
	signature := hex.EncodeToString(hmacSHA256(kSigning, stringToSign))
	auth := fmt.Sprintf("SDK-HMAC-SHA256 Access=%s, SignedHeaders=%s, Signature=%s", ak, signedHeaders, signature)

	req, _ := http.NewRequest(method, "https://"+host+path, strings.NewReader(body))
	req.Header.Set("Host", host)
	req.Header.Set("X-Sdk-Date", t)
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("华为云返回 HTTP %d: %s", resp.StatusCode, string(b))
	}
	return b, nil
}

// ============ 百度云（bce-auth-v1） ============

type baiduDDNS struct{ providerBase }

func (d *baiduDDNS) ensure(domain, sub, rt, ip string, ttl int) error {
	ak := d.c("accessKeyId")
	sk := d.c("accessKeySecret")
	if ak == "" || sk == "" {
		return fmt.Errorf("缺少 accessKeyId / accessKeySecret")
	}
	host := "bcd.baidubce.com"
	// 列出
	listBody, err := baiduRequest(http.MethodPost, host, ak, sk, "/v1/domain/resolve/list?domain="+domain, "{}")
	if err != nil {
		return err
	}
	var lst struct {
		Resolves []struct {
			ID     string `json:"id"`
			Domain string `json:"domain"`
			RdType string `json:"rdType"`
			Rdata  string `json:"rdata"`
		} `json:"resolves"`
	}
	_ = jsonUnmarshal(listBody, &lst)
	var recID, curIP string
	for _, r := range lst.Resolves {
		if r.Domain == domain && r.RdType == rt {
			recID, curIP = r.ID, r.Rdata
			break
		}
	}
	if curIP == ip {
		return nil
	}
	if recID != "" {
		payload := map[string]any{"domain": domain, "rdType": rt, "rdata": ip, "ttl": ttlOr1(ttl), "id": recID}
		b, _ := json.Marshal(payload)
		_, err = baiduRequest(http.MethodPost, host, ak, sk, "/v1/domain/resolve/edit", string(b))
	} else {
		payload := map[string]any{"domain": domain, "rdType": rt, "rdata": ip, "ttl": ttlOr1(ttl), "zoneName": domain}
		b, _ := json.Marshal(payload)
		_, err = baiduRequest(http.MethodPost, host, ak, sk, "/v1/domain/resolve/add", string(b))
	}
	return err
}

func baiduRequest(method, host, ak, sk, path, body string) ([]byte, error) {
	t := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	expiration := 1800
	signedHeaders := "host;x-bce-date"
	canonicalHeaders := "host:" + host + "\nx-bce-date:" + t + "\n"
	canonicalReq := method + "\n" + path + "\n" + "" + "\n" + canonicalHeaders + "\n" + signedHeaders + "\n" + sha256Hex(body)
	signingKey := hmacSHA256([]byte(sk), ak+"/"+t+"/"+fmt.Sprintf("%d", expiration)+"/"+signedHeaders)
	signature := hex.EncodeToString(hmacSHA256(signingKey, canonicalReq))
	auth := fmt.Sprintf("bce-auth-v1/%s/%s/%d/%s/%s", ak, t, expiration, signedHeaders, signature)

	req, _ := http.NewRequest(method, "https://"+host+path, strings.NewReader(body))
	req.Header.Set("Host", host)
	req.Header.Set("X-Bce-Date", t)
	req.Header.Set("Authorization", auth)
	req.Header.Set("Content-Type", "application/json")
	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("百度云返回 HTTP %d: %s", resp.StatusCode, string(b))
	}
	return b, nil
}

// 保证 sort 包被使用（华为/百度签名未直接用到 sort，保留占位避免告警）。
var _ = sort.Strings
