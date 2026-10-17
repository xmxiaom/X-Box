package main

// 面板登录保护（多账户 + 空闲超时）。
// 暴露 8899 后，所有 /api/* 接口需经会话鉴权，未登录拦截。
// 账户存于 dataDir/auth.json（每账户独立盐 + PBKDF2-HMAC-SHA256，60000 迭代）；首次无账户时可用环境变量 XBOX_PASS
// 初始化 admin 账户，否则首次通过 /api/login 创建（家庭网络首次由本人完成）。
// 会话内存态：空闲 10 分钟未操作即失效，绝对有效期 24 小时。

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	sessionIdleTimeout = 10 * time.Minute
	sessionMaxAge     = 24 * time.Hour
)

type account struct {
	User     string `json:"user"`
	PassHash string `json:"passHash"`
	Salt     string `json:"salt"`
}

type sessionInfo struct {
	User       string
	LastActive time.Time
}

type authStore struct {
	mu       sync.RWMutex
	accounts []account
	path     string
	sessions map[string]*sessionInfo
}

var authRT *authStore

func initAuth(dataDir string) {
	r := &authStore{
		path:     filepath.Join(dataDir, "auth.json"),
		sessions: map[string]*sessionInfo{},
	}
	if b, err := os.ReadFile(r.path); err == nil {
		var m struct {
			Accounts []account `json:"accounts"`
		}
		if json.Unmarshal(b, &m) == nil {
			r.accounts = m.Accounts
		}
	}
	// 未配置任何账户：环境变量 XBOX_PASS 优先；否则使用默认账户 admin/admin（请登录后及时修改）。
	if len(r.accounts) == 0 {
		pw := os.Getenv("XBOX_PASS")
		if pw == "" {
			pw = "admin"
		}
		_ = r.addAccount("admin", pw)
	}
	// 防爆破锁定态重置：锁定计数本就在内存中，服务重启（安装/升级/手动重启）时自然清零，
	// 与会话一同刷新——避免用户反复尝试后被长期挡在门外，重装即可恢复
	loginFails = &failLimiter{byIP: map[string]*failEntry{}, byUser: map[string]*failEntry{}}
	authRT = r
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// 密码哈希：PBKDF2-HMAC-SHA256（纯标准库实现），迭代 60000 次 + 每账户随机盐，抗暴力破解。
const pbkdf2Iter = 60000

func pbkdf2Hex(password, salt string, iter int) string {
	return hex.EncodeToString(pbkdf2SHA256([]byte(password), []byte(salt), iter, 32))
}

func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	prf := hmac.New(sha256.New, password)
	hashLen := sha256.Size
	blocks := (keyLen + hashLen - 1) / hashLen
	out := make([]byte, 0, blocks*hashLen)
	idx := make([]byte, 4)
	for block := 1; block <= blocks; block++ {
		binary.BigEndian.PutUint32(idx, uint32(block))
		prf.Reset()
		prf.Write(salt)
		prf.Write(idx)
		u := prf.Sum(nil)
		t := append([]byte(nil), u...)
		for i := 2; i <= iter; i++ {
			prf.Reset()
			prf.Write(u)
			u = prf.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func (r *authStore) addAccount(user, pass string) error {
	user = strings.TrimSpace(user)
	if user == "" {
		return fmt.Errorf("用户名不能为空")
	}
	if strings.TrimSpace(pass) == "" {
		return fmt.Errorf("密码不能为空")
	}
	for _, a := range r.accounts {
		if strings.EqualFold(a.User, user) {
			return fmt.Errorf("账户已存在")
		}
	}
	salt := randHex(8)
	r.accounts = append(r.accounts, account{User: user, PassHash: pbkdf2Hex(pass, salt, pbkdf2Iter), Salt: salt})
	return r.save()
}

func (r *authStore) save() error {
	b, _ := json.Marshal(map[string]any{"accounts": r.accounts})
	return os.WriteFile(r.path, b, 0o600)
}

func (r *authStore) verify(user, pass string) *account {
	for i := range r.accounts {
		a := &r.accounts[i]
		if strings.EqualFold(a.User, user) && pbkdf2Hex(pass, a.Salt, pbkdf2Iter) == a.PassHash {
			return a
		}
	}
	return nil
}

func (r *authStore) changePass(user, old, newp string) error {
	if strings.TrimSpace(newp) == "" {
		return fmt.Errorf("新密码不能为空")
	}
	if r.verify(user, old) == nil {
		return fmt.Errorf("原密码错误")
	}
	for i := range r.accounts {
		if strings.EqualFold(r.accounts[i].User, user) {
			r.accounts[i].Salt = randHex(8)
			r.accounts[i].PassHash = pbkdf2Hex(newp, r.accounts[i].Salt, pbkdf2Iter)
		}
	}
	return r.save()
}

func (r *authStore) delAccount(user string) error {
	if len(r.accounts) <= 1 {
		return fmt.Errorf("至少保留一个账户")
	}
	idx := -1
	for i, a := range r.accounts {
		if strings.EqualFold(a.User, user) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("账户不存在")
	}
	r.accounts = append(r.accounts[:idx], r.accounts[idx+1:]...)
	return r.save()
}

func (r *authStore) newSession(user string) string {
	token := randHex(24)
	r.sessions[token] = &sessionInfo{User: user, LastActive: time.Now()}
	return token
}

func (r *authStore) delSession(token string) {
	delete(r.sessions, token)
}

// checkSession 校验会话：存在、未超空闲/绝对时限；通过则刷新活跃时间并返回用户名。
// 支持三种凭据（按优先级）：Authorization: Bearer <token> 头部、xbox_sid Cookie、
// ?sid=<token> 查询参数。后两者覆盖 fnOS 客户端 iframe 场景：远程访问域名下第三方
// Cookie 被拦截、隐藏 iframe 无法自定义请求头，只能靠 URL 带 token。
func (r *authStore) checkSession(req *http.Request) (string, bool) {
	token := ""
	if ah := req.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
		token = strings.TrimSpace(ah[len("Bearer "):])
	}
	if token == "" {
		if c, err := req.Cookie("xbox_sid"); err == nil {
			token = c.Value
		}
	}
	if token == "" {
		token = strings.TrimSpace(req.URL.Query().Get("sid"))
	}
	if token == "" {
		return "", false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	si, ok := r.sessions[token]
	if !ok {
		return "", false
	}
	if time.Since(si.LastActive) > sessionMaxAge {
		delete(r.sessions, token)
		return "", false
	}
	if time.Since(si.LastActive) > sessionIdleTimeout {
		delete(r.sessions, token)
		return "", false
	}
	si.LastActive = time.Now() // 操作即续期，10 分钟无操作失效
	return si.User, true
}

// authed 仅返回是否登录有效（中间件用）。
func (r *authStore) authed(req *http.Request) bool {
	_, ok := r.checkSession(req)
	return ok
}

// authMiddleware 拦截 /api/*（白名单除外）：未登录返回 401 {needLogin:true}。
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path
		if p == "/" || strings.HasPrefix(p, "/api/login") ||
			p == "/api/logout" || p == "/api/auth/status" {
			next.ServeHTTP(w, r)
			return
		}
		if strings.HasPrefix(p, "/api/") {
			if authRT == nil || !authRT.authed(r) {
				writeJSON(w, 401, map[string]any{"ok": false, "needLogin": true, "error": "登录已过期，请重新登录"})
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---- 防暴力破解：每 IP / 每账户连续失败锁定 ----
type failEntry struct {
	count       int
	lastFail    time.Time
	lockedUntil time.Time
}
type failLimiter struct {
	mu     sync.Mutex
	byIP   map[string]*failEntry
	byUser map[string]*failEntry
}
var loginFails = &failLimiter{byIP: map[string]*failEntry{}, byUser: map[string]*failEntry{}}

const (
	maxFails     = 5
	lockDuration = 5 * time.Minute
)

func clientIP(r *http.Request) string { return r.RemoteAddr }

func (f *failLimiter) isLocked(ip, user string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, e := range []*failEntry{f.byIP[ip], f.byUser[user]} {
		if e != nil && time.Now().Before(e.lockedUntil) {
			return true
		}
	}
	return false
}

func (f *failLimiter) record(ip, user string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now()
	for _, kv := range []struct {
		m map[string]*failEntry
		k string
	}{ {f.byIP, ip}, {f.byUser, user} } {
		e := kv.m[kv.k]
		if e == nil {
			e = &failEntry{}
			kv.m[kv.k] = e
		}
		e.count++
		e.lastFail = now
		if e.count >= maxFails {
			e.lockedUntil = now.Add(lockDuration)
			e.count = 0
		}
	}
}

func (f *failLimiter) clear(ip, user string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.byIP, ip)
	delete(f.byUser, user)
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	ip := clientIP(r)
	if loginFails.isLocked(ip, req.User) {
		fail(w, 429, "登录失败次数过多，已临时锁定，请 5 分钟后再试")
		return
	}
	authRT.mu.Lock()
	if len(authRT.accounts) == 0 {
		user := strings.TrimSpace(req.User)
		if user == "" {
			user = "admin"
		}
		if strings.TrimSpace(req.Pass) == "" {
			authRT.mu.Unlock()
			fail(w, 400, "请设置管理员账户和密码")
			return
		}
		if err := authRT.addAccount(user, req.Pass); err != nil {
			authRT.mu.Unlock()
			fail(w, 400, err.Error())
			return
		}
	} else if authRT.verify(req.User, req.Pass) == nil {
		authRT.mu.Unlock()
		loginFails.record(ip, req.User)
		fail(w, 401, "账户或密码错误")
		return
	}
	user := strings.TrimSpace(req.User)
	if user == "" {
		user = "admin"
	}
	token := authRT.newSession(user)
	authRT.mu.Unlock()
	loginFails.clear(ip, req.User)
	http.SetCookie(w, &http.Cookie{
		Name: "xbox_sid", Value: token, Path: "/", HttpOnly: true,
		MaxAge: int(sessionMaxAge.Seconds()), SameSite: http.SameSiteLaxMode,
	})
	// 同时在响应体返回 token：供 iframe 内嵌场景（PC/手机客户端第三方 Cookie 被拦截时）
	// 前端改用 Authorization: Bearer <token> 头部鉴权
	ok(w, map[string]any{"sid": token})
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	token := ""
	if ah := r.Header.Get("Authorization"); strings.HasPrefix(ah, "Bearer ") {
		token = strings.TrimSpace(ah[len("Bearer "):])
	}
	if token == "" {
		if c, err := r.Cookie("xbox_sid"); err == nil {
			token = c.Value
		}
	}
	if token != "" {
		authRT.delSession(token)
	}
	http.SetCookie(w, &http.Cookie{Name: "xbox_sid", Value: "", Path: "/", MaxAge: -1})
	ok(w, map[string]any{"ok": true})
}

func (s *Server) handleAuthStatus(w http.ResponseWriter, r *http.Request) {
	authRT.mu.RLock()
	configured := len(authRT.accounts) > 0
	user, authed := authRT.checkSession(r)
	authRT.mu.RUnlock()
	ok(w, map[string]any{"configured": configured, "authed": authed, "user": user})
}

func (s *Server) handleAuthChangePass(w http.ResponseWriter, r *http.Request) {
	user, _ := authRT.checkSession(r) // 中间件已放行；再次取用户并续期
	var req struct {
		Old    string `json:"old"`
		New    string `json:"new"`
		Target string `json:"target"` // 非空且非当前账户时，由已登录者修改其他账户密码（家庭单管理员本地应用）
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	authRT.mu.Lock()
	defer authRT.mu.Unlock()
	if req.Target != "" && !strings.EqualFold(req.Target, user) {
		if err := authRT.changePassAdmin(req.Target, req.New); err != nil {
			fail(w, 400, err.Error())
			return
		}
		ok(w, map[string]any{"ok": true})
		return
	}
	if err := authRT.changePass(user, req.Old, req.New); err != nil {
		fail(w, 400, err.Error())
		return
	}
	ok(w, map[string]any{"ok": true})
}

func (s *Server) handleAuthAddUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	authRT.mu.Lock()
	defer authRT.mu.Unlock()
	if err := authRT.addAccount(req.User, req.Pass); err != nil {
		fail(w, 400, err.Error())
		return
	}
	ok(w, map[string]any{"ok": true})
}

func (s *Server) handleAuthDelUser(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User string `json:"user"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	authRT.mu.Lock()
	defer authRT.mu.Unlock()
	if err := authRT.delAccount(req.User); err != nil {
		fail(w, 400, err.Error())
		return
	}
	ok(w, map[string]any{"ok": true})
}

// 列出账户（仅用户名，不暴露密码/哈希）。current 标出当前登录账户。
func (r *authStore) listUsers(current string) []map[string]any {
	out := make([]map[string]any, 0, len(r.accounts))
	for _, a := range r.accounts {
		out = append(out, map[string]any{"user": a.User, "current": strings.EqualFold(a.User, current)})
	}
	return out
}

func (s *Server) handleAuthListUsers(w http.ResponseWriter, r *http.Request) {
	current, _ := authRT.checkSession(r)
	authRT.mu.RLock()
	users := authRT.listUsers(current)
	authRT.mu.RUnlock()
	ok(w, map[string]any{"users": users})
}

// 修改账户名：同步更新该账户的会话用户名，避免改名后原会话失效。
func (r *authStore) renameAccount(oldU, newU string) error {
	oldU = strings.TrimSpace(oldU)
	newU = strings.TrimSpace(newU)
	if oldU == "" || newU == "" {
		return fmt.Errorf("账户名不能为空")
	}
	if strings.EqualFold(oldU, newU) {
		return fmt.Errorf("新名称与当前相同")
	}
	idx := -1
	for i, a := range r.accounts {
		if strings.EqualFold(a.User, oldU) {
			idx = i
			break
		}
	}
	if idx < 0 {
		return fmt.Errorf("账户不存在")
	}
	for i, a := range r.accounts {
		if i != idx && strings.EqualFold(a.User, newU) {
			return fmt.Errorf("账户名已存在")
		}
	}
	r.accounts[idx].User = newU
	for _, si := range r.sessions {
		if strings.EqualFold(si.User, oldU) {
			si.User = newU
		}
	}
	return r.save()
}

func (s *Server) handleAuthRename(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Old string `json:"old"`
		New string `json:"new"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	authRT.mu.Lock()
	defer authRT.mu.Unlock()
	if err := authRT.renameAccount(req.Old, req.New); err != nil {
		fail(w, 400, err.Error())
		return
	}
	ok(w, map[string]any{"ok": true})
}

// 由已登录者直接修改其他账户密码（不校验原密码）。
func (r *authStore) changePassAdmin(user, newp string) error {
	if strings.TrimSpace(newp) == "" {
		return fmt.Errorf("新密码不能为空")
	}
	found := false
	for i := range r.accounts {
		if strings.EqualFold(r.accounts[i].User, user) {
			r.accounts[i].Salt = randHex(8)
			r.accounts[i].PassHash = pbkdf2Hex(newp, r.accounts[i].Salt, pbkdf2Iter)
			found = true
		}
	}
	if !found {
		return fmt.Errorf("账户不存在")
	}
	return r.save()
}
