package main

// 下载器对接：把磁力提交给 NAS 上已有的 BT 下载器执行实际下载。
//
// 支持三种主流协议（fnOS 用户常装、架构非常解耦）：
//   - qBittorrent  WebUI v2 API（默认 http://127.0.0.1:8080）
//   - transmission RPC（默认 http://127.0.0.1:9091/transmission/rpc）
//   - aria2        JSON-RPC（默认 http://127.0.0.1:6800/jsonrpc）
//
// 「视频下载」的实际字节传输发生在下载器里（X-Box 只提交任务并保存目标目录，
// 防止重复下载目录写满等风险），所以本模块职责：
//   1. download.json 配置的读写（下载目录 + 各下载器连接信息）；
//   2. 探测（POST /api/dl/probe）：逐个尝试列出的下载器，返回可用性；
//   3. 提交（POST /api/dl/start）：按配置/探测结果选一个可用的下载器，
//      附加 download-dir（若下载器支持按任务指定目录）；
//   4. 提交历史（内存会话级，供前端显示最近 N 条结果，重启即清）。
//
// 安全：提交的磁力只接受与搜索结果/用户粘贴等价的形式，校验 btih 前缀；
// 目录必须通过 safeDownloadDir（系统敏感目录黑名单 + 必须真实存在）。

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// DlClientCfg 单个下载器连接配置
type DlClientCfg struct {
	Base   string `json:"base"`             // 根地址（http://127.0.0.1:8080）
	User   string `json:"user,omitempty"`   // WebUI/RPC 用户名
	Pass   string `json:"pass,omitempty"`   // 密码（aria2 为空串，secret 独立）
	Secret string `json:"secret,omitempty"` // aria2 RPC secret
}

// DlConfig 下载板块整体配置（download.json）
type DlConfig struct {
	VideoDir string       `json:"videoDir"` // 视频下载目录（磁力交给下载器的 savepath）
	MusicDir string       `json:"musicDir"` // 音乐下载目录（网易云 mp3 直下）
	Clients  DlClientsSet `json:"clients"`
}

// DlClientsSet 三种下载器连接信息
type DlClientsSet struct {
	QBittorrent  DlClientCfg `json:"qbittorrent"`
	Transmission DlClientCfg `json:"transmission"`
	Aria2        DlClientCfg `json:"aria2"`
}

var dlConfMu sync.Mutex

// dlConfigPath 配置文件路径
func dlConfigPath() string {
	dir := os.Getenv("XBOX_DATA")
	if dir == "" {
		dir = "."
	}
	return filepath.Join(dir, "download.json")
}

// loadDlConfig 读配置（缺失/损坏返回零值配置）
func loadDlConfig() DlConfig {
	var cfg DlConfig
	b, err := os.ReadFile(dlConfigPath())
	if err != nil || json.Unmarshal(b, &cfg) != nil {
		return DlConfig{}
	}
	cfg.VideoDir = strings.TrimSpace(cfg.VideoDir)
	cfg.MusicDir = strings.TrimSpace(cfg.MusicDir)
	return cfg
}

// saveDlConfig 落盘（0600）
func saveDlConfig(cfg DlConfig) error {
	cfg.VideoDir = strings.TrimSpace(cfg.VideoDir)
	cfg.MusicDir = strings.TrimSpace(cfg.MusicDir)
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	dlConfMu.Lock()
	defer dlConfMu.Unlock()
	return os.WriteFile(dlConfigPath(), b, 0o600)
}

// ---------- 探测 ----------

// dlProbeRes 单个下载器探测结果
type dlProbeRes struct {
	Kind   string `json:"kind"` // qbittorrent / transmission / aria2
	Base   string `json:"base"`
	OK     bool   `json:"ok"`
	Authed bool   `json:"authed"` // 是否免登录/已登录（qbt 未配置账号时为 false 但 base 可达）
	Msg    string `json:"msg"`
	Via    string `json:"via"` // configured（用户填了地址）/ auto（默认端口探测）
}

const dlProbeTimeout = 3 * time.Second

// dlHTTPShort 探测用短超时客户端
var dlHTTPShort = &http.Client{Timeout: dlProbeTimeout}

// dlProbeKind 探测一种下载器（用户填了 base 优先用，否则试默认端口）
func dlProbeKind(kind string, cfg DlClientCfg) []dlProbeRes {
	bases := []string{}
	via := "auto"
	if b := strings.TrimSpace(cfg.Base); b != "" {
		bases = []string{strings.TrimSuffix(b, "/")}
		via = "configured"
	} else {
		switch kind {
		case "qbittorrent":
			bases = []string{"http://127.0.0.1:8080"}
		case "transmission":
			bases = []string{"http://127.0.0.1:9091"}
		case "aria2":
			bases = []string{"http://127.0.0.1:6800/jsonrpc", "http://127.0.0.1:6800/rpc"}
		default:
			return nil
		}
	}
	var out []dlProbeRes
	for _, base := range bases {
		ok, authed, msg := dlPing(kind, base, cfg)
		out = append(out, dlProbeRes{
			Kind: kind, Base: base, OK: ok, Authed: authed,
			Msg: msg, Via: via,
		})
		if ok {
			// 是否还需要试其它 candidate（base 缺省时若第一候选失败再继续）
			break
		}
	}
	return out
}

// dlPing base 判活 + 免登录验证
func dlPing(kind, base string, cfg DlClientCfg) (ok, authed bool, msg string) {
	base = strings.TrimSuffix(base, "/")
	switch kind {
	case "qbittorrent":
		// /api/v2/app/version 不需要登录时会 403，可判定 base 可达
		if r, err := http.Get(base + "/api/v2/app/version"); err == nil {
			defer r.Body.Close()
			if r.StatusCode == 200 {
				b, _ := io.ReadAll(io.LimitReader(r.Body, 256))
				// 未带 SID cookie 时 qbt 新版本返回 200 + "Forbidden"
				t := strings.TrimSpace(string(b))
				if t == "Forbidden" {
					return true, false, "服务可达（需要登录凭据）"
				}
				return true, true, "qBittorrent " + t
			}
			return false, false, fmt.Sprintf("HTTP %d", r.StatusCode)
		} else {
			return false, false, "连接失败：" + err.Error()
		}
	case "transmission":
		r, err := dlHTTPShort.Post(base+"/transmission/rpc", "application/json", bytes.NewReader([]byte(`{"method":"session-get"}`)))
		if err != nil {
			return false, false, "连接失败：" + err.Error()
		}
		defer r.Body.Close()
		switch r.StatusCode {
		case 200, 409:
			return true, true, "transmission RPC 可达"
		case 401:
			return true, false, "服务可达（需要登录凭据）"
		default:
			return false, false, fmt.Sprintf("HTTP %d", r.StatusCode)
		}
	case "aria2":
		r, err := dlHTTPShort.Post(base, "application/json", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":"xbox","method":"aria2.getVersion","params":[]}`)))
		if err != nil {
			return false, false, "连接失败：" + err.Error()
		}
		defer r.Body.Close()
		var j struct {
			Result struct {
				Version string `json:"version"`
			} `json:"result"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&j) == nil && j.Result.Version != "" {
			return true, true, "aria2 " + j.Result.Version
		}
		if r.StatusCode == 200 {
			return true, false, "服务可达（需要 secret）"
		}
		if j.Error != nil {
			return true, false, j.Error.Message
		}
		return false, false, fmt.Sprintf("HTTP %d", r.StatusCode)
	}
	return false, false, "未知类型"
}

// dlProbeAll 按配置探测全部下载器
func dlProbeAll(cfg DlConfig) []dlProbeRes {
	var out []dlProbeRes
	out = append(out, dlProbeKind("qbittorrent", cfg.Clients.QBittorrent)...)
	out = append(out, dlProbeKind("transmission", cfg.Clients.Transmission)...)
	out = append(out, dlProbeKind("aria2", cfg.Clients.Aria2)...)
	return out
}

// ---------- 提交 ----------

// dlSubmit 提交磁力到首个可用的下载器；dir 为目标目录（空则不指定）。
// 返回：实际使用的下载器描述。
func dlSubmit(cfg DlConfig, magnet string, dir string) (string, error) {
	if !strings.HasPrefix(magnet, "magnet:?xt=urn:btih:") {
		return "", fmt.Errorf("不是有效的磁力链接")
	}
	// 候选顺序：显式指定 kind（配置里填过 base 的）→ 自动探测
	tries := []struct {
		kind string
		set  DlClientCfg
	}{
		{"qbittorrent", cfg.Clients.QBittorrent},
		{"transmission", cfg.Clients.Transmission},
		{"aria2", cfg.Clients.Aria2},
	}
	var lastErr error
	for _, t := range tries {
		base := strings.TrimSpace(t.set.Base)
		if base == "" {
			// 未配置：也试默认端口（用户可能装了但没填），只要登录成功即可用
			switch t.kind {
			case "qbittorrent":
				base = "http://127.0.0.1:8080"
			case "transmission":
				base = "http://127.0.0.1:9091"
			case "aria2":
				base = "http://127.0.0.1:6800/jsonrpc"
			}
		}
		// 探测若明确不可达就跳过，避免提交反馈超时
		probe := dlProbeKind(t.kind, t.set)
		if len(probe) == 0 || (!probe[0].OK) {
			if strings.TrimSpace(t.set.Base) == "" {
				continue
			}
			lastErr = fmt.Errorf("%s 连接失败", t.kind)
			continue
		}
		if !probe[0].Authed {
			lastErr = fmt.Errorf("%s 可达但需要登录凭据，请在设置里填写地址/用户名/密码", t.kind)
			continue
		}
		var err error
		switch t.kind {
		case "qbittorrent":
			err = dlAddQbt(base, t.set, magnet, dir)
		case "transmission":
			err = dlAddTrans(base, t.set, magnet, dir)
		case "aria2":
			err = dlAddAria(base, t.set, magnet, dir)
		}
		if err == nil {
			return t.kind, nil
		}
		lastErr = fmt.Errorf("%s 提交失败：%v", t.kind, err)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("未检测到可用的 BT 下载器")
	}
	return "", lastErr
}

// dlAddQbt qBittorrent：登录 → multipart 添加磁力（savepath 按 torrent 单独指目录）
func dlAddQbt(base string, cfg DlClientCfg, magnet, dir string) error {
	form := url.Values{}
	form.Set("username", cfg.User)
	form.Set("password", cfg.Pass)
	resp, err := dlHTTPShort.PostForm(base+"/api/v2/auth/login", form)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	if !strings.Contains(strings.TrimSpace(string(body)), "Ok.") && resp.StatusCode != 200 {
		return fmt.Errorf("登录失败（HTTP %d）", resp.StatusCode)
	}
	cookies := ""
	for _, c := range resp.Cookies() {
		cookies += c.Name + "=" + c.Value + "; "
	}
	if len(resp.Cookies()) == 0 {
		return fmt.Errorf("登录响应缺少 SID")
	}
	buf := &bytes.Buffer{}
	mw := multipart.NewWriter(buf)
	fw, _ := mw.CreateFormField("urls")
	fw.Write([]byte(magnet))
	if dir != "" {
		fw, _ = mw.CreateFormField("savepath")
		fw.Write([]byte(dir))
	}
	mw.Close()
	req, err := http.NewRequest("POST", base+"/api/v2/torrents/add", buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Cookie", cookies)
	resp2, err := dlHTTPShort.Do(req)
	if err != nil {
		return err
	}
	defer resp2.Body.Close()
	if resp2.StatusCode != 200 {
		return fmt.Errorf("添加失败（HTTP %d）", resp2.StatusCode)
	}
	return nil
}

// dlAddTrans transmission：先拿 session-id → torrent-add
func dlAddTrans(base string, cfg DlClientCfg, magnet, dir string) error {
	client := &http.Client{Timeout: 10 * time.Second}
	sid := ""
	call := func(method, body string) (*http.Response, error) {
		req, err := http.NewRequest("POST", base+"/transmission/rpc", strings.NewReader(body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		if sid != "" {
			req.Header.Set("X-Transmission-Session-Id", sid)
		}
		if cfg.User != "" || cfg.Pass != "" {
			req.SetBasicAuth(cfg.User, cfg.Pass)
		}
		return client.Do(req)
	}
	r, err := call("POST", `{"method":"session-get"}`)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(r.Body, 1<<14))
	r.Body.Close()
	if r.StatusCode == 409 {
		sid = r.Header.Get("X-Transmission-Session-Id")
		r, err = call("POST", `{"method":"session-get"}`)
		if err != nil {
			return err
		}
		io.Copy(io.Discard, io.LimitReader(r.Body, 1<<14))
		r.Body.Close()
	} else if r.StatusCode == 401 {
		return fmt.Errorf("登录失败（凭据错误）")
	}
	if sid == "" {
		return fmt.Errorf("拿不到 session-id")
	}
	args := map[string]any{"filename": magnet}
	if dir != "" {
		args["download-dir"] = dir
	}
	b, _ := json.Marshal(map[string]any{"method": "torrent-add", "arguments": args})
	r, err = call("POST", string(b))
	if err != nil {
		return err
	}
	defer r.Body.Close()
	var j struct {
		Result    string `json:"result"`
		Arguments struct {
			TorrentAdded *map[string]any `json:"torrent-added"`
			TorrentDup   *map[string]any `json:"torrent-duplicate"`
		} `json:"arguments"`
	}
	err = json.NewDecoder(io.LimitReader(r.Body, 1<<18)).Decode(&j)
	if err != nil {
		return err
	}
	if j.Result != "success" {
		return fmt.Errorf("%s", j.Result)
	}
	return nil
}

// dlAddAria aria2 JSON-RPC：addUri
func dlAddAria(base string, cfg DlClientCfg, magnet, dir string) error {
	token := ""
	if cfg.Secret != "" {
		token = "token:" + cfg.Secret
	}
	params := []any{}
	if token != "" {
		params = append(params, token)
	}
	params = append(params, []string{magnet})
	if dir != "" {
		params = append(params, map[string]string{"dir": dir})
	}
	b, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": "xbox",
		"method": "aria2.addUri", "params": params,
	})
	resp, err := dlHTTPShort.Post(base, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var j struct {
		Result any `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.NewDecoder(io.LimitReader(resp.Body, 1<<16)).Decode(&j) == nil {
		if j.Error != nil {
			return fmt.Errorf("%s", j.Error.Message)
		}
		if j.Result != nil {
			return nil
		}
	}
	return fmt.Errorf("响应异常（HTTP %d）", resp.StatusCode)
}

// ---------- 提交历史（内存会话级） ----------

// dlHistEntry 已提交任务记录
type dlHistEntry struct {
	At     time.Time `json:"at"`
	Title  string    `json:"title"`
	Client string    `json:"client"`
	OK     bool      `json:"ok"`
	Msg    string    `json:"msg"`
}

var dlHist struct {
	mu   sync.Mutex
	list []dlHistEntry
}

func dlHistPush(e dlHistEntry) {
	dlHist.mu.Lock()
	defer dlHist.mu.Unlock()
	dlHist.list = append(dlHist.list, e)
	if len(dlHist.list) > 30 {
		dlHist.list = dlHist.list[len(dlHist.list)-30:]
	}
}

func dlHistList() []dlHistEntry {
	dlHist.mu.Lock()
	defer dlHist.mu.Unlock()
	out := make([]dlHistEntry, len(dlHist.list))
	copy(out, dlHist.list)
	// 前端按时间倒序显示
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out
}

// ---------- 缺省目录 ----------

// dlDefaultDir 缺省下载目录：首个媒体根（/vol1 等）下的 downloads 子目录。
// 不主动创建（用户保存配置时才建），返回建议值。
func dlDefaultDir(kind string) string {
	for _, root := range mediaRoots() {
		if root == "/" {
			continue
		}
		cand := filepath.Join(root, "downloads")
		if kind == "music" {
			cand = filepath.Join(root, "downloads", "music")
		}
		if fi, err := os.Stat(cand); err == nil && fi.IsDir() {
			return cand
		}
	}
	// 找不到现成目录就给第一个媒体根下的建议路径
	if root := mediaRoots(); len(root) > 0 && root[0] != "/" {
		if kind == "music" {
			return filepath.Join(root[0], "downloads", "music")
		}
		return filepath.Join(root[0], "downloads")
	}
	return ""
}
