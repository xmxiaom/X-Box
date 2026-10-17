package main

// DDNS 服务商连通性测试：必填凭据检查 + 真实轻量 API 调用（只读/最小请求，
// 绝不修改用户任何解析记录）。duckdns / oray 无只读接口，退化为 API 域名 TCP
// 连通性检查（调用方会在结果中注明"仅连通性验证"）。
// 注意：本文件名不可带 _test.go 后缀（Go 会将其排除出正常编译）。

import (
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"
)

func credMissing(cred map[string]string, keys ...string) error {
	var lack []string
	for _, k := range keys {
		if cred[k] == "" {
			lack = append(lack, k)
		}
	}
	if len(lack) > 0 {
		return fmt.Errorf("缺少必填凭据字段：%s", strings.Join(lack, ", "))
	}
	return nil
}

// testDNSProvider 返回 nil 表示通；非 nil 为不通的具体原因。
func testDNSProvider(t string, cred map[string]string) error {
	switch t {
	case "alidns":
		if err := credMissing(cred, "accessKeyId", "accessKeySecret"); err != nil {
			return err
		}
		params := map[string]string{
			"Action": "DescribeDomains", "PageSize": "1",
			"AccessKeyId": cred["accessKeyId"], "Format": "json", "Version": "2015-01-09",
			"SignatureMethod": "HMAC-SHA1", "SignatureVersion": "1.0",
			"SignatureNonce": fmt.Sprintf("%d", time.Now().UnixNano()),
			"Timestamp":      time.Now().UTC().Format("2006-01-02T15:04:05Z"),
		}
		params["Signature"] = aliSign(params, cred["accessKeySecret"])
		body, err := httpGet(defaultHTTPClient, "https://alidns.aliyuncs.com/?"+aliEncodeParams(params))
		if err != nil {
			return fmt.Errorf("无法连接阿里云 API：%v", err)
		}
		var j struct {
			Code    string `json:"Code"`
			Message string `json:"Message"`
		}
		_ = jsonUnmarshal([]byte(body), &j)
		if j.Code != "" {
			return fmt.Errorf("阿里云返回错误 %s：%s", j.Code, j.Message)
		}
		return nil

	case "tencentcloud":
		if err := credMissing(cred, "secretId", "secretKey"); err != nil {
			return err
		}
		body, err := tcCall(cred["secretId"], cred["secretKey"], "DescribeRecordList", map[string]any{})
		if err != nil {
			return fmt.Errorf("无法连接腾讯云 API：%v", err)
		}
		var j struct {
			Response struct {
				Error *struct {
					Code    string `json:"Code"`
					Message string `json:"Message"`
				} `json:"Error"`
			} `json:"Response"`
		}
		_ = jsonUnmarshal(body, &j)
		if j.Response.Error != nil {
			code := j.Response.Error.Code
			// 认证/签名类错误 = 凭据无效；参数类错误说明签名已通过（凭据有效）
			if strings.Contains(code, "Auth") || strings.Contains(code, "Signature") || strings.Contains(code, "Credential") {
				return fmt.Errorf("凭据无效（%s：%s）", code, j.Response.Error.Message)
			}
			return nil
		}
		return nil

	case "huaweicloud":
		if err := credMissing(cred, "accessKeyId", "accessKeySecret"); err != nil {
			return err
		}
		region := cred["region"]
		if region == "" {
			region = "cn-north-4"
		}
		host := "dns." + region + ".myhuaweicloud.com"
		body, err := hwRequest(http.MethodGet, host, cred["accessKeyId"], cred["accessKeySecret"], region, "/v2.1/zones?limit=1", "")
		if err != nil {
			return fmt.Errorf("无法连接华为云 API：%v", err)
		}
		if strings.Contains(string(body), "APIGW.0301") || strings.Contains(string(body), "Unauthorized") || strings.Contains(string(body), "Forbidden") {
			return fmt.Errorf("凭据无效：%s", truncate(string(body), 120))
		}
		return nil

	case "baiducloud":
		if err := credMissing(cred, "accessKeyId", "accessKeySecret"); err != nil {
			return err
		}
		_, err := baiduRequest(http.MethodPost, "bcd.baidubce.com", cred["accessKeyId"], cred["accessKeySecret"], "/v1/domain/resolve/list?domain=baidu.com", "{}")
		if err != nil {
			msg := err.Error()
			if strings.Contains(msg, "AccessDenied") || strings.Contains(msg, "权限") || strings.Contains(msg, "认证") {
				return fmt.Errorf("凭据无效：%s", truncate(msg, 120))
			}
			return fmt.Errorf("无法连接百度云 API：%s", truncate(msg, 120))
		}
		return nil

	case "cloudflare":
		if err := credMissing(cred, "apiToken"); err != nil {
			return err
		}
		return cfTokenTest(cred["apiToken"])

	case "namecheap":
		if err := credMissing(cred, "apiKey", "userName"); err != nil {
			return err
		}
		u := fmt.Sprintf("https://api.namecheap.com/xml.response?ApiUser=%s&ApiKey=%s&UserName=%s&Command=namecheap.domains.list&ClientIp=1.2.3.4",
			cred["userName"], cred["apiKey"], cred["userName"])
		body, err := httpGet(defaultHTTPClient, u)
		if err != nil {
			return fmt.Errorf("无法连接 Namecheap API：%v", err)
		}
		if !strings.Contains(body, `Status="OK"`) {
			return fmt.Errorf("Namecheap 返回异常：%s", truncate(body, 150))
		}
		return nil

	case "namesilo":
		if err := credMissing(cred, "apiKey"); err != nil {
			return err
		}
		body, err := httpGet(defaultHTTPClient, "https://www.namesilo.com/api/listRecords?version=1&type=xml&key="+cred["apiKey"]+"&domain=example.com")
		if err != nil {
			return fmt.Errorf("无法连接 NameSilo API：%v", err)
		}
		// code 110/111/112 = API key 无效；300 = 域名不存在（说明 key 有效）
		if strings.Contains(body, "<code>110</code>") || strings.Contains(body, "<code>111</code>") || strings.Contains(body, "<code>112</code>") {
			return fmt.Errorf("API Key 无效：%s", truncate(body, 150))
		}
		return nil

	case "duckdns":
		if err := credMissing(cred, "token"); err != nil {
			return err
		}
		if !tcpAlive("www.duckdns.org:443") {
			return fmt.Errorf("无法连接 www.duckdns.org:443（网络/DNS 异常）")
		}
		return nil // 无只读 API，仅连通性

	case "oray":
		if err := credMissing(cred, "username", "password"); err != nil {
			return err
		}
		if !tcpAlive("ddns.oray.com:443") {
			return fmt.Errorf("无法连接 ddns.oray.com:443（网络/DNS 异常）")
		}
		return nil // 无只读 API，仅连通性

	default:
		return fmt.Errorf("不支持的服务商: %s", t)
	}
}

// cfTokenTest Cloudflare Token 有效性（GET /zones?per_page=1 带 Bearer）。
func cfTokenTest(token string) error {
	req, err := http.NewRequest(http.MethodGet, "https://api.cloudflare.com/client/v4/zones?per_page=1", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := defaultHTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("无法连接 Cloudflare API：%v", err)
	}
	defer resp.Body.Close()
	body, _ := readAllLimit(resp.Body, 1<<20)
	var j struct {
		Success bool `json:"success"`
		Errors  []struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"errors"`
	}
	_ = jsonUnmarshal(body, &j)
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		msg := "Token 无效或权限不足"
		if len(j.Errors) > 0 {
			msg = j.Errors[0].Message
		}
		return fmt.Errorf("Cloudflare 认证失败：%s", msg)
	}
	if !j.Success && len(j.Errors) > 0 {
		return fmt.Errorf("Cloudflare 返回错误：%s", j.Errors[0].Message)
	}
	return nil
}

// tcpAlive TCP 连通性探测（3 秒超时）。
func tcpAlive(addr string) bool {
	c, err := net.DialTimeout("tcp", addr, 3*time.Second)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
