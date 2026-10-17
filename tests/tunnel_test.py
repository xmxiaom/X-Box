# X-Box 隧道验证（无登录态，WSL python3 运行）：python3 tests/tunnel_test.py
import base64, json, urllib.request, urllib.parse, subprocess, time, os, tempfile, sys

BIN = "/mnt/e/软件/开发/WorkBuddy/飞牛应用/x-box/pkg/app/server/xbox-amd64"
TMPBIN = "/tmp/xbox_tun_test"
subprocess.run(["cp", BIN, TMPBIN])
os.chmod(TMPBIN, 0o755)
env = dict(os.environ); env["XBOX_DATA"] = tempfile.mkdtemp()
srv = subprocess.Popen([TMPBIN], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
time.sleep(1.2)
if srv.poll() is not None:
    print("FAIL: 服务器未启动"); sys.exit(1)

def tunnel(payload):
    t = base64.urlsafe_b64encode(json.dumps(payload).encode()).decode().rstrip("=")  # 与前端一致：无填充
    return "http://127.0.0.1:8899/?__x=" + urllib.parse.quote(t, safe="")

def get(url):
    try:
        with urllib.request.urlopen(url, timeout=5) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()

ok = True
def check(name, cond, detail=""):
    global ok
    print(("PASS" if cond else "FAIL"), name, str(detail)[:120])
    if not cond: ok = False

# 1) 隧道 GET overview（无登录态，无任何凭据）：应 200 + version
st, body = get(tunnel({"m": "GET", "p": "/api/overview"}))
j = json.loads(body) if body else {}
check("隧道GET(无凭据)", st == 200 and j.get("ok") and j.get("version"), body)

# 2) 隧道 POST 业务接口（终端 exec 经隧道分发）
st, body = get(tunnel({"m": "POST", "p": "/api/terminal/exec", "b": json.dumps({"cmd": "echo tunnel-ok"})})) 
j = json.loads(body) if body else {}
check("隧道POST终端exec", st == 200 and j.get("ok") and "tunnel-ok" in (j.get("output") or ""), body[:80])

# 3) 隧道 POST 带真实载荷（DDNS provider save，合法结构）
st, body = get(tunnel({"m": "POST", "p": "/api/ddns/provider/save", "b": json.dumps({"name": "测试", "type": "aliyun", "cred": {}})}))
check("隧道POST真实载荷", st in (200, 400) and ("ok" in body or "error" in body), body)

# 4) 无效隧道 → 回落 HTML
st, body = get("http://127.0.0.1:8899/?__x=@@@bad@@@")
check("无效隧道回落HTML", st == 200 and "<html" in body, body[:40])

# 5) 隧道指向非 /api → 回落 HTML
st, body = get(tunnel({"m": "GET", "p": "/"}))
check("非API路径回落HTML", st == 200 and "<html" in body, body[:40])

# 6) 回归：直连 /api/overview（无凭据）
st, body = get("http://127.0.0.1:8899/api/overview")
check("直连GET回归", st == 200 and json.loads(body).get("ok"), body[:60])

# 7) 中文载荷
st, body = get(tunnel({"m": "POST", "p": "/api/ddns/provider/save", "b": json.dumps({"name": "中文名称", "type": "aliyun"})}))
check("中文载荷隧道", st in (200, 400) and ("ok" in body or "error" in body), body)

# 8) 登录端点应已移除（404/405 均算已移除）
try:
    req = urllib.request.Request("http://127.0.0.1:8899/api/login", data=b"{}", headers={"Content-Type": "application/json"})
    with urllib.request.urlopen(req, timeout=5) as r:
        st = r.status
except urllib.error.HTTPError as e:
    st = e.code
check("登录端点已移除(404/405)", st in (404, 405), "status=%s" % st)

# 9) 监听策略：必须含 127.0.0.1:8899，且不得出现 0.0.0.0/:: 通配（不对公网/所有网卡暴露）
ss = subprocess.run(["ss", "-tln"], capture_output=True, text=True).stdout
lines = [l for l in ss.splitlines() if ":8899" in l]
has_loop = any("127.0.0.1:8899" in l for l in lines)
no_wild = not any(("0.0.0.0:8899" in l or "[::]:8899" in l or "*:8899" in l) for l in lines)
check("监听含回环", has_loop, " | ".join(lines))
check("无通配绑定(0.0.0.0/::)", no_wild, " | ".join(lines))

# 10) 终端：cd 维持会话目录 + pwd 回填 + 退出码
st, body = get(tunnel({"m": "POST", "p": "/api/terminal/exec", "b": json.dumps({"cmd": "cd /tmp"})}))
j = json.loads(body) if body else {}
check("终端cd更新目录", st == 200 and j.get("ok") and j.get("cwd") == "/tmp", body[:80])
st, body = get(tunnel({"m": "POST", "p": "/api/terminal/exec", "b": json.dumps({"cmd": "pwd"})}))
j = json.loads(body) if body else {}
check("终端pwd返回cwd", st == 200 and j.get("ok") and "/tmp" in (j.get("output") or ""), body[:80])
st, body = get(tunnel({"m": "POST", "p": "/api/terminal/exec", "b": json.dumps({"cmd": "false"})}))
j = json.loads(body) if body else {}
check("终端非零退出码", st == 200 and j.get("ok") and j.get("exitCode") == 1, body[:80])

def get_long(url, timeout=180):
    try:
        with urllib.request.urlopen(url, timeout=timeout) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()

# 11) 终端断开：close 复位工作目录到 /root
st, body = get(tunnel({"m": "POST", "p": "/api/terminal/close", "b": "{}"}))
j = json.loads(body) if body else {}
check("终端close复位目录", st == 200 and j.get("ok") and j.get("cwd") == "/root", body[:80])

# 12) 下载状态端点（异步下载任务查询）
st, body = get(tunnel({"m": "GET", "p": "/api/update/status"}))
j = json.loads(body) if body else {}
check("下载状态端点", st == 200 and j.get("ok") and "running" in j and "done" in j, body[:80])

# 13) 浏览器代理 /b/{scheme}/{host}/{path}：本地起迷你 HTTP 服务，验证
#     base 注入、绝对 URL 全量重写、CSS/meta/JS 重写、跳转重写、Cookie 隔离
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
H = "127.0.0.1:8898"
PAGE = (b'<html><head><base href="http://old.example/"><title>t</title>'
        b'<style>body{background:url(http://' + H.encode() + b'/pic.png)}</style>'
        b'<meta http-equiv="refresh" content="3;url=http://' + H.encode() + b'/next">'
        b'<script>var h=location.hostname;</script></head><body>'
        b'<a href="/rel">r</a>'                                   # 相对：交给 base
        b'<a href="http://' + H.encode() + b'/abs">a</a>'         # 绝对：必须重写
        b'<img src="//' + H.encode() + b'/proto.png">'            # 协议相对：必须重写
        b'<img srcset="http://' + H.encode() + b'/s1.png 1x, /s2.png 2x">'
        b'<form action="http://' + H.encode() + b'/login" method="post"></form>'
        b'<a href="javascript:void(0)">j</a><a href="#top">h</a>'
        b'</body></html>')
JS = b'var a=location.hostname;var b=window.location.origin;var c=location.port;'
class BH(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/page':
            self.send_response(200)
            self.send_header('Content-Type', 'text/html')
            self.send_header('Set-Cookie', 'sid=abc123; Path=/')
            self.end_headers(); self.wfile.write(PAGE)
        elif self.path == '/s.js':
            self.send_response(200); self.send_header('Content-Type', 'application/javascript'); self.end_headers(); self.wfile.write(JS)
        elif self.path == '/style.css':
            self.send_response(200); self.send_header('Content-Type', 'text/css'); self.end_headers()
            self.wfile.write(b'.x{background:url("http://' + H.encode() + b'/pic.png")}')
        elif self.path == '/redir':
            self.send_response(302)
            self.send_header('Location', 'http://' + H + '/next')
            self.end_headers()
        elif self.path == '/pic.png':
            self.send_response(200); self.send_header('Content-Type', 'image/png'); self.end_headers(); self.wfile.write(b'PNGDATA')
        elif self.path.startswith('/echo'):
            self.send_response(200); self.send_header('Content-Type', 'text/plain'); self.end_headers(); self.wfile.write(('QUERY:' + self.path).encode())
        elif self.path.startswith('/ck'):
            self.send_response(200); self.send_header('Content-Type', 'text/plain'); self.end_headers()
            self.wfile.write(('COOKIE:' + (self.headers.get('Cookie') or '')).encode())
        else:
            self.send_response(404); self.end_headers()
    def log_message(self, *a): pass
bh = HTTPServer(('127.0.0.1', 8898), BH)
threading.Thread(target=bh.serve_forever, daemon=True).start()

B = "http://127.0.0.1:8899/b/http/" + H
st, body = get(B + "/page")
check("浏览器代理base注入", st == 200 and '<base href="/b/http/' + H + '/page">' in body and 'old.example' not in body, body[:200])
check("浏览器代理运行时脚本注入", "__xbox" in body and "MutationObserver" in body, body[:120])
check("浏览器代理绝对URL重写", 'href="/b/http/' + H + '/abs"' in body, body[:300])
check("浏览器代理协议相对重写", 'src="/b/http/' + H + '/proto.png"' in body, body[:300])
check("浏览器代理srcset重写", '/b/http/' + H + '/s1.png 1x' in body, body[:300])
check("浏览器代理表单action重写", 'action="/b/http/' + H + '/login"' in body, body[:300])
check("浏览器代理保留非URL协议", 'javascript:void(0)' in body and 'href="#top"' in body, body[:300])
check("浏览器代理CSS url()重写", 'url("/b/http/' + H + '/pic.png")' in body or "url('/b/http/" + H + "/pic.png')" in body, body[:300])
check("浏览器代理meta refresh重写", 'url="/b/http/' + H + '/next"' in body or "url='/b/http/" + H + "/next'" in body, body[:300])
# 页面 JS 拿 location.hostname/host/origin/port 拼绝对 URL：必须替换为真实目标值
# （否则拼出的是面板域名 → 404，或直连目标 → 拒绝连接）
check("浏览器代理内联JS location替换", '<script>var h="127.0.0.1";</script>' in body, body[:400])
st, body = get(B + "/s.js")
check("浏览器代理JS文件location替换", st == 200 and body == 'var a="127.0.0.1";var b="http://127.0.0.1:8898";var c="8898";', body[:120])
st, body = get(B + "/style.css")
check("浏览器代理CSS文件重写", st == 200 and '/b/http/' + H + '/pic.png' in body, body[:120])
st, body = get(B + "/pic.png")
check("浏览器代理透传", st == 200 and body == "PNGDATA", body[:40])
st, body = get(B + "/echo?k=v")
check("浏览器代理查询参数透传", st == 200 and 'QUERY:/echo?k=v' in body, body[:80])
# 3xx 不跟随 + Location 重写为代理路径（否则浏览器直连目标 → 拒绝连接）
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, req, fp, code, msg, headers, newurl): return None
op = urllib.request.build_opener(NoRedirect)
try:
    rr = op.open(B + "/redir", timeout=5)
    loc, code = rr.headers.get("Location") or "", rr.status
    rr.close()
except urllib.error.HTTPError as e:
    loc, code = e.headers.get("Location") or "", e.code
check("浏览器代理跳转Location重写", code == 302 and loc == "/b/http/" + H + "/next", "%s %s" % (code, loc))
# Cookie：回写带站点隔离 Path；入向透传给目标
r1 = urllib.request.urlopen(B + "/page", timeout=5)
sc = r1.headers.get("Set-Cookie") or ""
r1.close()
check("浏览器代理Cookie回写", "sid=abc123" in sc and "Path=/b/http/" + H + "/" in sc, sc)
st, body = get(urllib.request.Request(B + "/ck", headers={"Cookie": "sid=abc123"}))
check("浏览器代理Cookie透传", st == 200 and "COOKIE:sid=abc123" in body, body[:80])
# 目标不可达：返回可读的中文 HTML 错误页（请求由 NAS 发出，失败即 NAS→目标不通）
st, body = get("http://127.0.0.1:8899/b/http/127.0.0.1:9/")
check("浏览器代理失败错误页", st == 502 and "无法访问目标地址" in body, body[:80])

# 14) 更新下载：异步任务 + 轮询状态（经 GitHub API 资产端点）
try:
    rel = json.load(urllib.request.urlopen(urllib.request.Request(
        "https://api.github.com/repos/xmxiaom/X-Box/releases/latest",
        headers={"User-Agent": "xbox-test"}), timeout=15))
    asset = next((a for a in rel.get("assets", []) if a["name"].endswith(".fpk")), None)
except Exception as e:
    rel, asset = {}, None
    print("（GitHub 不可达，跳过更新下载实测）", e)
if asset:
    st, body = get(tunnel({"m": "POST", "p": "/api/update/download",
                           "b": json.dumps({"url": "https://api.github.com/repos/xmxiaom/X-Box/releases/assets/%d" % asset["id"], "name": "X-Box-test.fpk"})}))
    j = json.loads(body) if body else {}
    check("更新下载启动异步任务", st == 200 and j.get("ok") and j.get("started"), body[:120])
    # 轮询状态直到完成（最长 180s）
    done, last = False, {}
    for _ in range(120):
        time.sleep(1.5)
        st, body = get(tunnel({"m": "GET", "p": "/api/update/status"}))
        last = json.loads(body) if body else {}
        if last.get("done") and not last.get("running"):
            done = True
            break
    check("更新下载轮询完成", done and not last.get("error") and last.get("bytes", 0) > 1000000, json.dumps(last)[:150])
    # SSRF 防护：非 GitHub 地址必须拒绝（立即返回，不启动任务）
    st, body = get(tunnel({"m": "POST", "p": "/api/update/download", "b": json.dumps({"url": "https://evil.example.com/x.fpk", "name": "x.fpk"})}))
    check("下载SSRF防护", st == 400 and "仅支持" in body, body[:80])
    # 检查更新：发布时间本地化 + 下载 URL 走 API 端点（仅在本地版本低于远程最新版时填充）
    st, body = get(tunnel({"m": "GET", "p": "/api/update/check"}))
    upd = (json.loads(body).get("update") or {}) if body else {}
    if upd.get("hasUpdate"):
        pa = upd.get("publishedAt") or ""
        check("发布时间本地化格式", bool(pa) and "T" not in pa and "Z" not in pa, pa)
        check("下载URL走API端点", (upd.get("downloadUrl") or "").startswith("https://api.github.com/repos/xmxiaom/X-Box/releases/assets/"), upd.get("downloadUrl"))
    else:
        print("SKIP 发布时间/下载URL 断言（本地版本 %s 不低于远程 %s，发布新版本后此路径生效）" % (upd.get("current"), upd.get("latest")))
else:
    check("更新下载(API资产端点)", False, "未找到 fpk 资产")

srv.kill(); os.remove(TMPBIN)
print("TUNNEL", "ALL PASS" if ok else "HAS FAILURES")
sys.exit(0 if ok else 1)
