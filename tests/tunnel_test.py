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

# 10) 更新下载：经 GitHub API 资产端点（穿透 objects.githubusercontent.com 不可达）
try:
    rel = json.load(urllib.request.urlopen(urllib.request.Request(
        "https://api.github.com/repos/xmxiaom/X-Box/releases/latest",
        headers={"User-Agent": "xbox-test"}), timeout=15))
    asset = next((a for a in rel.get("assets", []) if a["name"].endswith(".fpk")), None)
except Exception as e:
    rel, asset = {}, None
    print("（GitHub 不可达，跳过更新下载实测）", e)
if asset:
    st, body = get_long(tunnel({"m": "POST", "p": "/api/update/download",
                                "b": json.dumps({"url": "https://api.github.com/repos/xmxiaom/X-Box/releases/assets/%d" % asset["id"], "name": "X-Box-test.fpk"})}))
    j = json.loads(body) if body else {}
    check("更新下载(API资产端点)", st == 200 and j.get("ok") and j.get("bytes", 0) > 1000000, body[:120])
    # SSRF 防护：非 GitHub 地址必须拒绝
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
