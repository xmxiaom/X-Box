# X-Box 隧道验证（WSL python3 运行）：python3 tests/tunnel_test.py
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

# 1) 隧道登录（GET /?__x= 承载 POST /api/login）
st, body = get(tunnel({"m": "POST", "p": "/api/login", "b": json.dumps({"user": "admin", "pass": "admin"})}))
j = json.loads(body) if body else {}
check("隧道登录(返sid)", st == 200 and j.get("ok") and j.get("sid"), body)
sid = j.get("sid", "")

# 2) 隧道 GET overview（payload 带 sid）
st, body = get(tunnel({"m": "GET", "p": "/api/overview", "sid": sid}))
j = json.loads(body) if body else {}
check("隧道GET(带sid)", st == 200 and j.get("ok") and j.get("version"), body)

# 3) 隧道 POST adduser（带 sid）
st, body = get(tunnel({"m": "POST", "p": "/api/auth/adduser", "b": json.dumps({"user": "bob", "pass": "pw"}), "sid": sid}))
check("隧道POST业务(带sid)", st == 200 and json.loads(body).get("ok"), body)

# 4) 隧道错误密码
st, body = get(tunnel({"m": "POST", "p": "/api/login", "b": json.dumps({"user": "admin", "pass": "bad"})}))
check("隧道错误密码", st == 401 and "账户或密码错误" in body, body)

# 5) 无 sid 的隧道 GET → 401
st, body = get(tunnel({"m": "GET", "p": "/api/overview"}))
check("隧道无sid=401", st == 401 and json.loads(body).get("needLogin"), body)

# 6) 无效隧道 → 回落 HTML
st, body = get("http://127.0.0.1:8899/?__x=@@@bad@@@")
check("无效隧道回落HTML", st == 200 and "<html" in body, body[:40])

# 7) 隧道指向非 /api → 回落 HTML
st, body = get(tunnel({"m": "GET", "p": "/"}))
check("非API路径回落HTML", st == 200 and "<html" in body, body[:40])

# 8) 回归：直连 /api/login
req = urllib.request.Request("http://127.0.0.1:8899/api/login", data=json.dumps({"user":"admin","pass":"admin"}).encode(), headers={"Content-Type":"application/json"})
with urllib.request.urlopen(req, timeout=5) as r:
    j = json.loads(r.read().decode())
check("直连登录回归", j.get("ok") and j.get("sid"), "")

# 9) 中文载荷隧道（DDNS 保存场景）
st, body = get(tunnel({"m": "POST", "p": "/api/ddns/provider/save", "b": json.dumps({"name": "中文名称", "type": "aliyun"}), "sid": sid}))
check("中文载荷隧道", st in (200, 400) and ("ok" in body or "error" in body), body)

srv.kill(); os.remove(TMPBIN)
print("TUNNEL", "ALL PASS" if ok else "HAS FAILURES")
sys.exit(0 if ok else 1)
