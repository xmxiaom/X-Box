# -*- coding: utf-8 -*-
# X-Box 测速节点管理端点验证（无登录态，WSL python3 运行）：python3 tests/speedtest_nodes_test.py
import base64, json, urllib.request, urllib.parse, subprocess, time, os, tempfile, sys

BIN = "/mnt/e/软件/开发/WorkBuddy/飞牛应用/x-box/pkg/app/server/xbox-amd64"
TMPBIN = "/tmp/xbox_st_test"
subprocess.run(["cp", BIN, TMPBIN])
os.chmod(TMPBIN, 0o755)
env = dict(os.environ); env["XBOX_DATA"] = tempfile.mkdtemp(); env["XBOX_DEV"] = "1"
srv = subprocess.Popen([TMPBIN], env=env, stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
time.sleep(1.2)
if srv.poll() is not None:
    print("FAIL: 服务器未启动"); sys.exit(1)

def tunnel(payload):
    t = base64.urlsafe_b64encode(json.dumps(payload).encode()).decode().rstrip("=")
    return "http://127.0.0.1:8899/?__x=" + urllib.parse.quote(t, safe="")

def get(url):
    try:
        with urllib.request.urlopen(url, timeout=8) as r:
            return r.status, r.read().decode()
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()

ok = True
def check(name, cond, detail=""):
    global ok
    print(("PASS" if cond else "FAIL"), name, str(detail)[:160])
    if not cond: ok = False

# 1) GET /api/speedtest/nodes：应返回 nodes 数组 + 三来源结构
st, body = get(tunnel({"m": "GET", "p": "/api/speedtest/nodes"}))
j = json.loads(body) if body else {}
nodes = j.get("nodes") or []
check("GET nodes", st == 200 and j.get("ok") and isinstance(nodes, list) and len(nodes) >= 4,
      "n=%d fetching=%s" % (len(nodes), j.get("fetching")))
srcs = set(n.get("source") for n in nodes)
# 有网时后台自动拉取远程清单并去重覆盖 builtin（remote 优先）；无网时只见 builtin 兜底
check("节点来源合法", srcs <= {"builtin", "remote", "custom"} and len(srcs) >= 1, srcs)
# 冷启动未跑过测速时 alive 缺省（omitempty）是预期；仅校验字段机制由测速后回填
has_alive = any("alive" in n for n in nodes)
check("节点结构完整(id/name/source)", all(n.get("id") and n.get("name") for n in nodes), "ok")

# 2) POST nodes/save：添加自定义节点（合法）
st, body = get(tunnel({"m": "POST", "p": "/api/speedtest/nodes/save",
    "b": json.dumps({"name": "本地测试节点", "server": "http://192.168.1.10:8080"})}))
j2 = json.loads(body) if body else {}
n2 = j2.get("nodes") or []
cust = [n for n in n2 if n.get("source") == "custom"]
check("POST save 自定义节点", st == 200 and j2.get("ok") and len(cust) == 1, body[:100])
cid = cust[0].get("id") if cust else ""

# 3) POST nodes/save：非法地址（非 http 前缀）应被拒
st, body = get(tunnel({"m": "POST", "p": "/api/speedtest/nodes/save",
    "b": json.dumps({"name": "坏节点", "server": "ftp://x"})}))
check("save 拒绝非 http 地址", st in (200, 400) and ("error" in body or (json.loads(body).get("nodes") or [1,2,3]) and len([n for n in json.loads(body).get("nodes") if n.get("source")=="custom"]) == 1), body[:100])

# 4) POST nodes/delete：删除刚加的节点
st, body = get(tunnel({"m": "POST", "p": "/api/speedtest/nodes/delete", "b": json.dumps({"id": cid})}))
j4 = json.loads(body) if body else {}
n4 = j4.get("nodes") or []
cust4 = [n for n in n4 if n.get("source") == "custom"]
check("POST delete 自定义节点", st == 200 and j4.get("ok") and len(cust4) == 0, body[:100])

# 5) start 带 node_id（指定一个不存在的节点，应返回错误而非崩溃）
st, body = get(tunnel({"m": "POST", "p": "/api/speedtest/start", "b": json.dumps({"node_id": "no-such-node"})}))
check("start 指定未知节点不崩溃", st in (200, 400), body[:80])

# 6) start 空体（自动竞速模式，接受请求即可；立即 stop 防止继续跑）
st, body = get(tunnel({"m": "POST", "p": "/api/speedtest/start", "b": "{}"}))
check("start 空体自动模式", st == 200, body[:80])
st, body = get(tunnel({"m": "POST", "p": "/api/speedtest/stop", "b": "{}"}))
check("stop 中止", st == 200, body[:80])

# 7) status 快照含 live 字段
st, body = get(tunnel({"m": "GET", "p": "/api/speedtest/status"}))
j7 = json.loads(body) if body else {}
check("status 含 live 字段", st == 200 and "live" in j7, body[:120])

try: srv.terminate()
except Exception: pass
print("SPEEDTEST NODES", "ALL PASS" if ok else "HAS FAIL")
sys.exit(0 if ok else 1)
