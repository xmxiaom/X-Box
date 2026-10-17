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

# 2) 隧道 POST 业务接口（空载荷触发应用错误，验证传输与分发）
st, body = get(tunnel({"m": "POST", "p": "/api/proxy/save", "b": json.dumps({})}))
check("隧道POST业务", st in (200, 400) and ("ok" in body or "error" in body), body)

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

# 10) 反代 Host 精确匹配回归（v1.2.40 修复 hostMatches 精确匹配 continue bug）
st, body = get(tunnel({"m": "POST", "p": "/api/proxy/save", "b": json.dumps({"hosts": ["proxy-test.local"], "backend": "http://127.0.0.1:1/", "enabled": True})}))
check("保存反代站点", st == 200 and json.loads(body).get("ok"), body)
site_id = json.loads(body).get("id", 0)
def get_host(host):
    req = urllib.request.Request("http://127.0.0.1:8899/", headers={"Host": host})
    try:
        with urllib.request.urlopen(req, timeout=5) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()
st, body = get_host("proxy-test.local:8899")   # 带端口（浏览器实际行为）
check("反代精确匹配(带端口)", st == 502 and "不可达" in body, "status=%s %s" % (st, body[:60]))
st, body = get_host("proxy-test.local")        # 不带端口
check("反代精确匹配(不带端口)", st == 502 and "不可达" in body, "status=%s %s" % (st, body[:60]))
st, body = get_host("nomatch.local")           # 不匹配 → 回落面板 HTML
check("反代不匹配回落面板", st == 200 and "<html" in body, "status=%s" % st)
st, body = get(tunnel({"m": "POST", "p": "/api/proxy/del?id=%d" % site_id}))
check("清理反代站点", st == 200 and json.loads(body).get("ok"), body)

srv.kill(); os.remove(TMPBIN)
print("TUNNEL", "ALL PASS" if ok else "HAS FAILURES")
sys.exit(0 if ok else 1)
