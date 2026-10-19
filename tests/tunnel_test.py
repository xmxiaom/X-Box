# X-Box 隧道验证（无登录态，WSL python3 运行）：python3 tests/tunnel_test.py
import base64, json, shutil, urllib.request, urllib.parse, subprocess, time, os, tempfile, sys, re

BIN = "/mnt/e/软件/开发/WorkBuddy/飞牛应用/x-box/pkg/app/server/xbox-amd64"
TMPBIN = "/tmp/xbox_tun_test"
subprocess.run(["cp", BIN, TMPBIN])
os.chmod(TMPBIN, 0o755)
env = dict(os.environ); env["XBOX_DATA"] = tempfile.mkdtemp(); env["XBOX_DEV"] = "1"
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
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
H = "127.0.0.1:8898"
PAGE = (b'<html><head><base href="http://old.example/"><title>t</title>'
        b'<style>body{background:url(http://' + H.encode() + b'/pic.png)}</style>'
        b'<meta http-equiv="refresh" content="3;url=http://' + H.encode() + b'/next">'
        b'<meta http-equiv="refresh" content="0; URL=cgi-bin/luci/">'  # 无引号相对：不能补引号
        b'<script>var h=location.hostname;var bg="url(/ib.gif) no-repeat";</script></head><body>'
        b'<div style="background:url(/root-bg.png)">bg</div>'
        b'<a href="/rel">r</a>'                                   # 相对：交给 base
        b'<link rel="stylesheet" href="/root.css">'               # 根相对：必须拼目标站点根
        b'<a href="http://' + H.encode() + b'/abs">a</a>'         # 绝对：必须重写
        b'<img src="//' + H.encode() + b'/proto.png">'            # 协议相对：必须重写
        b'<img srcset="http://' + H.encode() + b'/s1.png 1x, /s2.png 2x">'
        b'<form action="http://' + H.encode() + b'/login" method="post"></form>'
        b'<a href="javascript:void(0)">j</a><a href="#top">h</a>'
        b'<a href="/b/v9999/http/' + H.encode() + b'/deep">d</a>'  # 已是代理路径：不得二次嵌套
        b'<div>location.host</div>'                              # HTML 文本不是 JS，绝不能改写
        b'</body></html>')
JS = b'var a=location.hostname;var b=window.location.origin;var c=location.port;'
class BH(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/page':
            if self.headers.get('If-None-Match') == '"abc"':   # 条件请求 → 304
                self.send_response(304); self.end_headers(); return
            self.send_response(200)
            self.send_header('Content-Type', 'text/html')
            self.send_header('Set-Cookie', 'sid=abc123; Path=/')
            self.send_header('Cache-Control', 'max-age=60')
            self.send_header('ETag', '"abc"')
            self.end_headers(); self.wfile.write(PAGE)
        elif self.path == '/s.js':
            self.send_response(200); self.send_header('Content-Type', 'application/javascript'); self.end_headers(); self.wfile.write(JS)
        elif self.path == '/u.js':
            self.send_response(200); self.send_header('Content-Type', 'application/javascript'); self.end_headers()
            self.wfile.write(b'var code="if(location.host==1){}";var h=location.host;var p=location.port;')
        elif self.path == '/t.js':
            self.send_response(200); self.send_header('Content-Type', 'application/javascript'); self.end_headers()
            self.wfile.write(b'if(top.location.href!=self.location.href){top.location="http://x/y";}document.domain="192.168.1.1";location.href="/a.html";')
        elif self.path == '/p.js':
            # 自定义对象上的同名属性（n.location.protocol / h[o][h.location.hostname]）
            # 不是全局 location：旧正则从 location 处起匹配 → n."https:" 整包 SyntaxError
            # （实测百度 all_async_search 第 3017 行、min_super 第 59 行）
            self.send_response(200); self.send_header('Content-Type', 'application/javascript'); self.end_headers()
            self.wfile.write(b'var s=("https:"===n.location.protocol?1:0);var t=h[o][h.location.hostname];var u=location.host;')
        elif self.path == '/f.js':
            # 帧跳出惯用法（光猫/路由器登录页必备）：右侧是 Location 对象，
            # 包成导航等于把当前 URL 再导航一次——在 iframe 里就是自己刷自己，
            # 页面永远白屏（实测 192.168.1.1 的 if(top!=self){top.location=self.location;}）
            self.send_response(200); self.send_header('Content-Type', 'application/javascript'); self.end_headers()
            self.wfile.write(b'if(top!=self){top.location = self.location;}var r=/["\']/g;var k=location.host;')
        elif self.path == '/n.js':
            # 赋值语句没有分号、且嵌在 void(...) 里（百度 all_async_search 实测）：
            # 正则"吞到下一个分号"会把 ):void 0} 也包进 __xbp() → 语法错误
            self.send_response(200); self.send_header('Content-Type', 'application/javascript'); self.end_headers()
            self.wfile.write(b'var h=function(e,t){return e?void(f&&(g.p(e,t),location.href=build())):void 0};')
        elif self.path == '/v.js':
            # 裸 location = x 可能是局部变量赋值（jQuery 1.x：location=window.location），
            # 绝不能当成导航改写
            self.send_response(200); self.send_header('Content-Type', 'application/javascript'); self.end_headers()
            self.wfile.write(b'var a=typeof undefined,location=window.location,doc=window.document;boot(location.href);')
        elif self.path == '/forbidden':
            body_txt = b'FORBIDDEN' + b'x' * 2000
            self.send_response(403); self.send_header('Content-Type', 'text/html'); self.end_headers()
            self.wfile.write(body_txt)
        elif self.path.startswith('/slow'):
            time.sleep(1.5)
            self.send_response(200); self.send_header('Content-Type', 'text/plain'); self.end_headers()
            self.wfile.write(b'SLOW')
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
    def do_POST(self):
        # 回显请求体长度与传输方式：用来确认代理转发 POST 时带 Content-Length
        # 而不是 chunked（嵌入式 httpd 不支持 chunked，会直接断连）
        n = self.headers.get('Content-Length')
        te = self.headers.get('Transfer-Encoding') or ''
        ln = int(n) if (n and n.isdigit()) else 0
        body = self.rfile.read(ln) if ln else b''
        self.send_response(200); self.send_header('Content-Type', 'text/plain'); self.end_headers()
        self.wfile.write(('CL=%s;TE=%s;LEN=%d' % (n, te, len(body))).encode())
    def log_message(self, *a): pass
bh = ThreadingHTTPServer(('127.0.0.1', 8898), BH)  # 多线程：并发限流用例需要
threading.Thread(target=bh.serve_forever, daemon=True).start()

B = "http://127.0.0.1:8899/b/http/" + H
st, body = get(B + "/page")
# 重写后的 URL 带版本段（/b/v1254/...）：改版后自动弃用浏览器里的旧改写结果。
# 前缀随版本变化，所以从注入的 <base> 里取，避免每版都改测试。
mp = re.search(r'<base href="(/b/[^/"]*/)http/', body)
PRE = mp.group(1) if mp else "/b/"
BP = PRE + "http/" + H
check("浏览器代理base注入", st == 200 and ('<base href="' + BP + '/page">') in body and 'old.example' not in body, body[:200])
check("浏览器代理运行时脚本注入", "__xbox" in body and "MutationObserver" in body, body[:120])
check("浏览器代理绝对URL重写", 'href="' + BP + '/abs"' in body, body[:300])
check("浏览器代理协议相对重写", 'src="' + BP + '/proto.png"' in body, body[:300])
check("浏览器代理根相对URL重写", 'href="' + BP + '/root.css"' in body, body[:300])
check("浏览器代理srcset重写", BP + '/s1.png 1x' in body, body[:300])
check("浏览器代理表单action重写", 'action="' + BP + '/login"' in body, body[:300])
check("浏览器代理保留非URL协议", 'javascript:void(0)' in body and 'href="#top"' in body, body[:300])
# 幂等：已经是代理路径的属性值不能再套一层（v1.2.55 曾因此死循环：属性每轮变长
# → MutationObserver 反复触发 → 浏览器卡死）
check("浏览器代理改写幂等", ('href="/b/v9999/http/' + H + '/deep"') in body
      and (BP + '/b/v9999') not in body, body[:400])
# HTML 可见文本不是 JS 代码，其中的 location.host 不能被改写
check("浏览器代理HTML文本不改写", '<div>location.host</div>' in body, body[:300])
check("浏览器代理CSS url()重写", 'url(' + BP + '/pic.png)' in body, body[:300])
# style 属性里的 url() 同样不能补引号（外层已被 HTML 属性引号包着）
check("浏览器代理CSS不改引号", 'url(' + BP + '/root-bg.png)' in body and 'url("' + BP + '"' not in body, body[:400])
# <script> 内的 url() 字符串不是 CSS，绝不能改写（否则整段脚本报废）
check("浏览器代理CSS跳过script", 'var bg="url(/ib.gif) no-repeat"' in body, body[:400])
check("浏览器代理meta refresh重写", 'url=' + BP + '/next' in body, body[:300])
# meta content 自带的引号不能再补一层（否则 content="0; URL="x"" → 解析成刷新到
# 空地址 → 无限刷新自己，实测 192.168.1.50 的 LuCI 首页就卡在这里）
check("浏览器代理meta refresh不加引号", 'content="0; URL=cgi-bin/luci/"' in body
      and 'URL="cgi-bin' not in body, body[:400])
# 页面 JS 拿 location.hostname/host/origin/port 拼绝对 URL：必须替换为**面板**值，
# 让拼出的 URL 落到面板根下再由孤儿兜底回代理（v1.2.47 替换为真实目标值反而引导
# JS 拼出 http://192.168.1.1/... 直连 → https 面板下混合内容被拦 → 页面空白）
check("浏览器代理内联JS location替换", 'var h="127.0.0.1";' in body, body[:400])
st, body = get(B + "/s.js")
check("浏览器代理JS文件location替换", st == 200 and body == 'var a="127.0.0.1";var b="http://127.0.0.1:8899";var c="8899";', body[:120])
# Content-Type 必须原样保留：模块脚本对 MIME 严格校验，JS 被标成 text/html 会
# 让整个模块图被拒（实测 fnOS 前端 Vite/ESM 全白）
rr = urllib.request.urlopen(B + "/s.js", timeout=5); ct = rr.headers.get("Content-Type") or ""; rr.close()
check("浏览器代理保留JS类型", "javascript" in ct.lower(), ct)
rq = urllib.request.urlopen(B + "/style.css", timeout=5); ct2 = rq.headers.get("Content-Type") or ""; rq.close()
check("浏览器代理保留CSS类型", "text/css" in ct2.lower(), ct2)
# 老光猫页面两类必崩写法的中和（v1.2.50）：
# document.domain 赋值在现代 Chrome 抛错中断整个 script；top.location 跨源读抛 SecurityError
st, body = get(B + "/t.js")
check("浏览器代理top.location中和", st == 200 and 'top.location' not in body, body[:160])
check("浏览器代理document.domain中和", st == 200 and 'document.domain=' not in body, body[:160])
# location 赋值必须经 __xbp 走代理（v1.2.50 未包装导致跳转直连 → 逃逸 → 无限重载循环）
check("浏览器代理location赋值走代理", st == 200 and body.count('__xbp(') == 2
      and 'self.location.href=(window.__xbp?__xbp("http://x/y"):("http://x/y"));' in body
      and 'self.location.href=(window.__xbp?__xbp("/a.html"):("/a.html"));' in body, body[:220])
# 字符串/注释里的 location.* 绝不能替换（否则 SyntaxError，实测 music.taihe.com、
# image.baidu.com 整页脚本报废："Uncaught SyntaxError: Unexpected string"）
st, body = get(B + "/u.js")
check("浏览器代理字符串内不替换", st == 200 and '"if(location.host==1){}"' in body
      and body.endswith('var h="127.0.0.1:8899";var p="8899";'), body[:160])
# 自定义对象上的同名属性不能改写（百度大包实测：n.location.protocol → n."https:"）
st, body = get(B + "/p.js")
check("浏览器代理自定义对象同名属性不改写", st == 200
      and 'n.location.protocol' in body and 'h[o][h.location.hostname]' in body
      and 'var u="127.0.0.1:8899";' in body, body[:240])
# 帧跳出惯用法（top.location = self.location）必须中和为无操作（v1.2.54）
st, body = get(B + "/f.js")
check("浏览器代理帧跳出赋值中和", st == 200 and '{void 0;}' in body
      and '__xbp(self.location)' not in body, body[:240])
# 正则字面量里的引号不能干扰字符串掩码（否则后续字符串被当代码改写）
check("浏览器代理正则字面量不干扰掩码", st == 200 and 'var r=/["\']/g;' in body
      and 'var k="127.0.0.1:8899";' in body, body[:240])
# 赋值无分号、嵌在外层表达式里：只能按括号配平截断，不能吞到下一个 ';'
st, body = get(B + "/n.js")
check("浏览器代理赋值表达式边界", st == 200
      and 'self.location.href=(window.__xbp?__xbp(build()):(build())))' in body
      and ':void 0};' in body, body[:240])
# 裸 location = x（局部变量赋值）不能当导航改写（jQuery 1.x 实测：整包报废）
st, body = get(B + "/v.js")
check("浏览器代理变量同名赋值不改写", st == 200
      and 'location=window.location' in body and 'boot(location.href)' in body, body[:220])
# POST 必须带 Content-Length（chunked 会被光猫/路由器这类 httpd 直接断连 → EOF）
st, body = get(urllib.request.Request(B + "/postecho", data=b"action=login&x=1",
               headers={"Content-Type": "application/x-www-form-urlencoded"}))
check("浏览器代理POST带Content-Length", st == 200 and "CL=16;" in body and "TE=;" in body
      and "LEN=16" in body, body)
# /api/temps 必须与 overview 里的 temps 同构（扁平）：前端轮询按顶层取 disks/cpuMax。
# 之前包了一层 {temps:{...}}，导致"首次有温度、5 秒后变成暂无传感器"。
st, body = get(tunnel({"m": "GET", "p": "/api/temps"}))
j = json.loads(body) if body else {}
check("温度接口结构", st == 200 and j.get("ok") and ("cpuMax" in j) and ("disks" in j), body[:100])
st, body = get(B + "/style.css")
check("浏览器代理CSS文件重写", st == 200 and BP + '/pic.png' in body, body[:120])
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
check("浏览器代理跳转Location重写", code == 302 and loc == BP + "/next", "%s %s" % (code, loc))
# Cookie：回写带站点隔离 Path；入向透传给目标
r1 = urllib.request.urlopen(B + "/page", timeout=5)
sc = r1.headers.get("Set-Cookie") or ""
r1.close()
check("浏览器代理Cookie回写", "sid=abc123" in sc and ("Path=" + BP + "/") in sc, sc)
st, body = get(urllib.request.Request(B + "/ck", headers={"Cookie": "sid=abc123"}))
check("浏览器代理Cookie透传", st == 200 and "COOKIE:sid=abc123" in body, body[:80])
# 304 必须原样返回：不得重写、不得写入注入脚本（v1.2.53 修复：此前给 304 写了
# body，浏览器收到"带 body 的 304"会反复重新加载 —— 实测 192.168.1.50 每秒几十次 304）
rq3 = urllib.request.Request(B + "/page", headers={"If-None-Match": '"abc"'})
try:
    r4 = urllib.request.urlopen(rq3, timeout=5)
    raw4, code4 = r4.read(), r4.status
    r4.close()
except urllib.error.HTTPError as e:
    raw4, code4 = e.read(), e.code
check("浏览器代理304不写body", code4 == 304 and len(raw4) == 0, "%s len=%d" % (code4, len(raw4)))
# 目标不可达：返回可读的中文 HTML 错误页（请求由 NAS 发出，失败即 NAS→目标不通）
st, body = get("http://127.0.0.1:8899/b/http/127.0.0.1:9/")
check("浏览器代理失败错误页", st == 502 and "无法访问目标地址" in body, body[:80])
# 文本类响应 gzip 回传（远程访问经 fnOS 域名中转，带宽常是瓶颈）
import gzip as _gz
rq = urllib.request.Request(B + "/page", headers={"Accept-Encoding": "gzip"})
rr = urllib.request.urlopen(rq, timeout=5)
raw = rr.read(); enc = (rr.headers.get("Content-Encoding") or "").lower(); rr.close()
try:
    txt = _gz.decompress(raw).decode("utf-8", "replace")
except Exception:
    txt = raw.decode("utf-8", "replace")
check("浏览器代理文本gzip回传", enc == "gzip" and ('<base href="' + BP + '/page">') in txt and len(raw) < len(txt), enc)
# 非 200 响应也会压缩回传：编码头必须在写状态码之前设置，否则浏览器收到 gzip
# 字节却没有解压标记 → 整页乱码（未登录的设备管理页 403 曾全屏乱码）
import gzip as _gz2
rq403 = urllib.request.Request(B + "/forbidden", headers={"Accept-Encoding": "gzip"})
try:
    r403 = urllib.request.urlopen(rq403, timeout=5)
except urllib.error.HTTPError as e:
    r403 = e
enc403 = (r403.headers.get("Content-Encoding") or "").lower()
raw403 = r403.read()
try:
    dec403 = _gz2.decompress(raw403) if enc403 == "gzip" else raw403
except Exception:
    dec403 = b""
check("非200响应编码一致", enc403 == "gzip" and (b"FORBIDDEN" in dec403), enc403 + " len=" + str(len(raw403)))
# 缓存头透传（二次访问命中浏览器缓存 / 条件请求 304）
rq2 = urllib.request.urlopen(B + "/page", timeout=5)
cc = rq2.headers.get("Cache-Control") or ""; et = rq2.headers.get("ETag") or ""; rq2.close()
check("浏览器代理缓存头透传", "max-age=60" in cc and et == '"abc"', cc + " | " + et)
# 孤儿路径兜底：页面 JS 用面板 host 拼出的绝对 URL 落到面板根下 → 307 回最近站点代理路径
st, body = get(B + "/page")  # 先建立"最近站点"上下文
try:
    rr = op.open("http://127.0.0.1:8899/cgi-bin/x?a=1", timeout=5)
    oloc, ocode = rr.headers.get("Location") or "", rr.status
    rr.close()
except urllib.error.HTTPError as e:
    oloc, ocode = e.headers.get("Location") or "", e.code
check("孤儿路径兜底回代理", ocode == 307 and oloc == BP + "/cgi-bin/x?a=1", "%s %s" % (ocode, oloc))
# 代理面板自身必须拒绝：/b/https/面板域名/... 会打回面板 → 面板 307 回 /b/...
# 形成每秒几十条的自环（实测百度地图 worker_wasm 请求）
st, body = get("http://127.0.0.1:8899/b/http/127.0.0.1:8899/")
check("浏览器代理拒绝代理自身", st == 400 and "面板自身" in body, "%s %s" % (st, body[:80]))
# 代理防护：自激/自环会拖垮整机，服务端必须有硬边界
st, body = get("http://127.0.0.1:8899/b/http/" + H + "/page?x=" + ("a" * 5000))
check("防护-超长地址拒绝", st == 400 and "过长" in body, body[:60])
# 重复前缀折叠：页面 JS 二次拼接会产出同站重复前缀（百度地图移动版实测），
# 服务端应 307 纠正到单前缀地址而不是直接报错（urllib 自动跟随 → 200）
st, body = get("http://127.0.0.1:8899/b/v9999/http/" + H + "/b/v9999/http/" + H + "/page")
check("嵌套-同站重复前缀自动折叠", st == 200 and "嵌套" not in body, "%s %s" % (st, body[:60]))
# 跨站嵌套是真的嵌套（A 站页面里再嵌 B 站），必须仍拒绝
st, body = get("http://127.0.0.1:8899/b/v9999/http/" + H + "/b/v9999/http/127.0.0.1:8897/page")
check("防护-跨站嵌套拒绝", st == 400 and "嵌套" in body, body[:60])
st, body = get("http://127.0.0.1:8899/b/http/127.0.0.1/")
check("防护-本机网关拒绝", st == 400 and "自环" in body, body[:60])
# 并发闸门：24 个槽 + 1.5s 慢响应，40 并发里必须出现 429
import concurrent.futures as _cf
def _hit(_i):
    try:
        with urllib.request.urlopen("http://127.0.0.1:8899/b/http/" + H + "/slow", timeout=20) as r:
            return r.status
    except urllib.error.HTTPError as e:
        return e.code
    except Exception:
        return 0
with _cf.ThreadPoolExecutor(max_workers=40) as ex:
    codes = list(ex.map(_hit, range(40)))
check("防护-并发限流生效", codes.count(429) > 0 and codes.count(200) > 0, str(sorted(set(codes))))
# 旧路径（不带版本段）与任意版本段都要能正常服务（面板里写死的 /b/... 仍可用）
st, body = get("http://127.0.0.1:8899/b/v9999/http/" + H + "/page")
check("浏览器代理版本段兼容", st == 200 and ('<base href="' + BP + '/page">') in body, body[:100])
# 响应源码快照：诊断"200 但页面空白"时直接看目标返回了什么
st, body = get(tunnel({"m": "GET", "p": "/api/browser/snap?target=" + urllib.parse.quote("http://" + H + "/page", safe="")}))
j = json.loads(body) if body else {}
check("浏览器代理响应源码查看", st == 200 and j.get("ok") and "<html>" in (j.get("body") or ""), body[:120])
# 代理请求日志：面板「诊断」据此区分「代理没收到请求 / NAS 连不上目标 / 页面本身问题」
st, body = get(tunnel({"m": "GET", "p": "/api/browser/log"}))
j = json.loads(body) if body else {}
items = j.get("items") or []
check("浏览器代理日志端点", st == 200 and j.get("ok") and len(items) > 0
      and any(i.get("target", "").startswith("http://127.0.0.1:8898") for i in items)
      and any(i.get("status") == 200 for i in items)
      and any("<html>" in (i.get("snippet") or "") for i in items), body[:160])

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
    # 轮询状态直到完成（最长 300s；网络慢时需更久，避免把"下载中"误判为失败）
    done, last = False, {}
    for _ in range(200):
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

# 15) fnOS 统一网关模式：unix socket + /app/<appname> 前缀
#     官方契约要求应用监听 app.sock，网关校验登录态后把 /app/{appname}/... 转进来。
#     服务端必须剥前缀路由、并把前缀补回所有对外生成的绝对路径，否则网关不认。
import socket as _sk, base64 as _b64
sock_dir = tempfile.mkdtemp()
sock_path = os.path.join(sock_dir, "app.sock")
env_gw = dict(env); env_gw["XBOX_SOCKET"] = sock_path; env_gw["XBOX_BASE"] = "/app/X-Box"
srv_gw = subprocess.Popen([TMPBIN], env=env_gw, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
time.sleep(1.2)

def gw(path):
    # 目标页可能是 GB2312，不能按 UTF-8 直接解码整个响应，先按字节切出状态码
    out = subprocess.run(["curl", "-s", "--unix-socket", sock_path, "-w", "|%{http_code}",
                          "http://localhost" + path], capture_output=True, timeout=10).stdout
    body_b, _, code_b = out.rpartition(b"|")
    return code_b.decode("ascii", "replace"), body_b.decode("utf-8", "replace")

ok_sock = os.path.exists(sock_path)
check("网关入口socket已创建", ok_sock, sock_path)
if ok_sock:
    code, page = gw("/app/X-Box/")
    check("网关入口首页", code == "200" and 'var b="/app/X-Box"' in page, code)
    tp = _b64.urlsafe_b64encode(json.dumps({"m": "GET", "p": "/api/overview"}).encode()).decode().rstrip("=")
    code, js = gw("/app/X-Box/?__x=" + tp)
    check("网关入口隧道API", code == "200" and '"version"' in js, code)
    # 版本段随发布变化：从接口取当前版本拼出来，避免硬编码
    try:
        seg = "v" + str(json.loads(js).get("version", "")).replace(".", "")
    except Exception:
        seg = ""
    code, page = gw("/app/X-Box/b/" + seg + "/http/192.168.1.1/")
    check("网关入口代理路径带前缀", code == "200" and re.search(
        r'<base href="/app/X-Box/b/v\d+/http/192\.168\.1\.1/"', page) is not None, code + " " + page[:120])
    # 全量接口：确认前缀剥离没有破坏任何一个端点
    for ep in ["/api/ports", "/api/processes", "/api/services", "/api/autostart",
               "/api/nat", "/api/ddns", "/api/terminal/info", "/api/browser/log", "/api/temps"]:
        tp = _b64.urlsafe_b64encode(json.dumps({"m": "GET", "p": ep}).encode()).decode().rstrip("=")
        code, js = gw("/app/X-Box/?__x=" + tp)
        check("网关入口API " + ep, code == "200" and '"ok"' in js, code + " " + js[:60])
srv_gw.kill(); shutil.rmtree(sock_dir, ignore_errors=True)

srv.kill(); os.remove(TMPBIN)
print("TUNNEL", "ALL PASS" if ok else "HAS FAILURES")
sys.exit(0 if ok else 1)
