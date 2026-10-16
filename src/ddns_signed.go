package main

// DDNS 签名类服务商：阿里云解析 / 腾讯云 DNSPod。
import (
	"crypto/hmac"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
)

func jsonUnmarshal(b []byte, v any) error { return json.Unmarshal(b, v) }

// ============ 阿里云解析（HMAC-SHA1） ============

type aliDDNS struct{ providerBase }

func (d *aliDDNS) ensure(domain, sub, rt, ip string, ttl int) error {
	ak := d.c("accessKeyId")
	sk := d.c("accessKeySecret")
	if ak == "" || sk == "" {
		return fmt.Errorf("缺少 AccessKeyId / AccessKeySecret")
	}
	// 查询已有记录
	recs, err := d.describe(domain)
	if err != nil {
		return err
	}
	var recID, curIP string
	for _, r := range recs {
		if r.RR == sub && r.Type == rt {
			recID, curIP = r.RecordID, r.Value
			break
		}
	}
	if recID != "" && curIP == ip {
		return nil // 已最新
	}
	if recID != "" {
		err = d.update(recID, domain, sub, rt, ip, ttl)
	} else {
		err = d.add(domain, sub, rt, ip, ttl)
	}
	return err
}

func (d *aliDDNS) describe(full string) ([]struct {
	RecordID string `json:"RecordId"`
	RR       string `json:"RR"`
	Type     string `json:"Type"`
	Value    string `json:"Value"`
}, error) {
	ak := d.c("accessKeyId")
	sk := d.c("accessKeySecret")
	params := map[string]string{
		"Action":           "DescribeSubDomainRecords",
		"SubDomain":        full,
		"PageSize":         "100",
		"AccessKeyId":      ak,
		"Format":           "JSON",
		"Version":          "2015-01-09",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   fmt.Sprintf("%d", time.Now().UnixNano()),
		"SignatureVersion": "1.0",
		"Timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	sig := aliSign(params, sk)
	params["Signature"] = sig
	q := aliEncodeParams(params)
	body, err := httpGet(defaultHTTPClient, "https://alidns.aliyuncs.com/?"+q)
	if err != nil {
		return nil, err
	}
	var j struct {
		DomainRecords struct {
			Record []struct {
				RecordID string `json:"RecordId"`
				RR       string `json:"RR"`
				Type     string `json:"Type"`
				Value    string `json:"Value"`
			} `json:"Record"`
		} `json:"DomainRecords"`
	}
	if err := jsonUnmarshal([]byte(body), &j); err != nil {
		return nil, fmt.Errorf("阿里云解析响应解析失败: %s", body)
	}
	return j.DomainRecords.Record, nil
}

func (d *aliDDNS) add(domain, sub, rt, ip string, ttl int) error {
	ak := d.c("accessKeyId")
	sk := d.c("accessKeySecret")
	params := map[string]string{
		"Action":           "AddDomainRecord",
		"DomainName":       domain,
		"RR":               sub,
		"Type":             rt,
		"Value":            ip,
		"TTL":              fmt.Sprintf("%d", ttlOr1(ttl)),
		"AccessKeyId":      ak,
		"Format":           "JSON",
		"Version":          "2015-01-09",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   fmt.Sprintf("%d", time.Now().UnixNano()),
		"SignatureVersion": "1.0",
		"Timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	sig := aliSign(params, sk)
	params["Signature"] = sig
	_, err := httpGet(defaultHTTPClient, "https://alidns.aliyuncs.com/?"+aliEncodeParams(params))
	return err
}

func (d *aliDDNS) update(recID, domain, sub, rt, ip string, ttl int) error {
	ak := d.c("accessKeyId")
	sk := d.c("accessKeySecret")
	params := map[string]string{
		"Action":           "UpdateDomainRecord",
		"RecordId":         recID,
		"RR":               sub,
		"Type":             rt,
		"Value":            ip,
		"TTL":              fmt.Sprintf("%d", ttlOr1(ttl)),
		"AccessKeyId":      ak,
		"Format":           "JSON",
		"Version":          "2015-01-09",
		"SignatureMethod":  "HMAC-SHA1",
		"SignatureNonce":   fmt.Sprintf("%d", time.Now().UnixNano()),
		"SignatureVersion": "1.0",
		"Timestamp":        time.Now().UTC().Format("2006-01-02T15:04:05Z"),
	}
	sig := aliSign(params, sk)
	params["Signature"] = sig
	_, err := httpGet(defaultHTTPClient, "https://alidns.aliyuncs.com/?"+aliEncodeParams(params))
	return err
}

func aliEncodeParams(params map[string]string) string {
	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	for i, k := range keys {
		if i > 0 {
			sb.WriteString("&")
		}
		sb.WriteString(aliPct(k) + "=" + aliPct(params[k]))
	}
	return sb.String()
}

func aliPct(s string) string {
	e := url.QueryEscape(s)
	e = strings.ReplaceAll(e, "+", "%20")
	e = strings.ReplaceAll(e, "*", "%2A")
	e = strings.ReplaceAll(e, "%7E", "~")
	return e
}

func aliSign(params map[string]string, secret string) string {
	canonical := aliEncodeParams(params)
	stringToSign := "GET&" + aliPct("/") + "&" + aliPct(canonical)
	mac := hmac.New(sha1.New, []byte(secret+"&"))
	mac.Write([]byte(stringToSign))
	return base64.StdEncoding.EncodeToString(mac.Sum(nil))
}

// ============ 腾讯云 DNSPod（TC3-HMAC-SHA256） ============

type tcDDNS struct{ providerBase }

func (d *tcDDNS) ensure(domain, sub, rt, ip string, ttl int) error {
	secretId := d.c("secretId")
	secretKey := d.c("secretKey")
	if secretId == "" || secretKey == "" {
		return fmt.Errorf("缺少 SecretId / SecretKey")
	}
	// 查询已有
	recs, err := d.describe(secretId, secretKey, domain, sub, rt)
	if err != nil {
		return err
	}
	var recID int
	for _, r := range recs {
		if r.Value == ip {
			return nil // 已有且最新
		}
		recID = r.RecordID
	}
	if recID != 0 {
		return d.modify(secretId, secretKey, domain, sub, rt, ip, ttl, recID)
	}
	return d.create(secretId, secretKey, domain, sub, rt, ip, ttl)
}

type tcRecord struct {
	RecordID int    `json:"RecordId"`
	Value    string `json:"Value"`
}

func (d *tcDDNS) describe(sid, skey, domain, sub, rt string) ([]tcRecord, error) {
	payload := map[string]any{
		"Domain":      domain,
		"Subdomain":   sub,
		"RecordType":  rt,
		"RecordLine":  "默认",
	}
	body, err := tcCall(sid, skey, "DescribeRecordList", payload)
	if err != nil {
		return nil, err
	}
	var j struct {
		Response struct {
			RecordList []tcRecord `json:"RecordList"`
		} `json:"Response"`
	}
	if err := jsonUnmarshal(body, &j); err != nil {
		return nil, fmt.Errorf("腾讯云解析响应解析失败: %s", string(body))
	}
	return j.Response.RecordList, nil
}

func (d *tcDDNS) create(sid, skey, domain, sub, rt, ip string, ttl int) error {
	payload := map[string]any{
		"Domain":      domain,
		"Subdomain":   sub,
		"RecordType":  rt,
		"RecordLine":  "默认",
		"Value":       ip,
		"TTL":         ttlOr1(ttl),
	}
	_, err := tcCall(sid, skey, "CreateRecord", payload)
	return err
}

func (d *tcDDNS) modify(sid, skey, domain, sub, rt, ip string, ttl, recID int) error {
	payload := map[string]any{
		"Domain":      domain,
		"Subdomain":   sub,
		"RecordType":  rt,
		"RecordLine":  "默认",
		"Value":       ip,
		"TTL":         ttlOr1(ttl),
		"RecordId":    recID,
	}
	_, err := tcCall(sid, skey, "ModifyRecord", payload)
	return err
}

func tcCall(sid, skey, action string, payload map[string]any) ([]byte, error) {
	host := "dnspod.tencentcloudapi.com"
	service := "dnspod"
	version := "2021-03-23"
	bodyBytes, _ := json.Marshal(payload)
	bodyStr := string(bodyBytes)
	timestamp := time.Now().Unix()
	date := time.Unix(timestamp, 0).UTC().Format("2006-01-02")

	// 拼接规范请求串
	hashedPayload := sha256Hex(bodyStr)
	canonicalReq := strings.Join([]string{"POST", "/", "", "content-type:application/json; charset=utf-8", "host:" + host, "", "content-type;host", hashedPayload}, "\n")
	creqHash := sha256Hex(canonicalReq)
	stringToSign := "TC3-HMAC-SHA256\n" + fmt.Sprintf("%d", timestamp) + "\n" + date + "/" + service + "/tc3_request\n" + creqHash

	secretDate := hmacSHA256([]byte("TC3"+skey), date)
	secretService := hmacSHA256(secretDate, service)
	secretSigning := hmacSHA256(secretService, "tc3_request")
	signature := hex.EncodeToString(hmacSHA256(secretSigning, stringToSign))

	auth := fmt.Sprintf("TC3-HMAC-SHA256 Credential=%s/%s/%s/tc3_request, SignedHeaders=content-type;host, Signature=%s",
		sid, date, service, signature)

	req, _ := http.NewRequest(http.MethodPost, "https://"+host, strings.NewReader(bodyStr))
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Host", host)
	req.Header.Set("X-TC-Action", action)
	req.Header.Set("X-TC-Timestamp", fmt.Sprintf("%d", timestamp))
	req.Header.Set("X-TC-Version", version)
	req.Header.Set("Authorization", auth)

	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var j struct {
		Response struct {
			Error struct {
				Code    string `json:"Code"`
				Message string `json:"Message"`
			} `json:"Error"`
		} `json:"Response"`
	}
	if err := jsonUnmarshal(b, &j); err == nil && j.Response.Error.Code != "" {
		return nil, fmt.Errorf("腾讯云解析错误 %s: %s", j.Response.Error.Code, j.Response.Error.Message)
	}
	return b, nil
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, data string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(data))
	return mac.Sum(nil)
}
