# X-Box Release 发布脚本：创建 GitHub Release 并上传 fpk 资产
# 用法: python _rel.py <tag> <fpk路径> [标题] [说明文件]
# token 来源: git credential fill（不落盘）
import sys, json, ssl, subprocess, time, urllib.request, urllib.error

REPO = "xmxiaom/X-Box"

def token():
    out = subprocess.run(["git", "credential", "fill"], input="protocol=https\nhost=github.com\n\n",
                         capture_output=True, text=True).stdout
    for line in out.splitlines():
        if line.startswith("password="):
            return line.split("=", 1)[1]
    raise SystemExit("未取到 GitHub token")

def http(method, url, tok, data=None, headers=None, raw=False):
    h = {"Authorization": f"Bearer {tok}", "Accept": "application/vnd.github+json",
         "User-Agent": "xbox-release"}
    if headers: h.update(headers)
    req = urllib.request.Request(url, data=data, headers=h, method=method)
    ctx = ssl.create_default_context()
    try:
        with urllib.request.urlopen(req, context=ctx, timeout=300) as r:
            b = r.read()
            return r.status, (b if raw else (json.loads(b) if b else None))
    except urllib.error.HTTPError as e:
        b = e.read()
        return e.code, (b if raw else (json.loads(b) if b else None))

def upload_asset(tok, upload_url, name, path):
    base = upload_url.split("{", 1)[0]
    url = f"{base}?name={name}"
    size = __import__("os").path.getsize(path)
    # 用 curl 上传（urllib 在 6MB 资产上频繁被 RST）：15 次重试，上次实测第 5 次成功
    cmd = ["curl", "--retry", "15", "--retry-delay", "5", "--retry-all-errors",
           "--connect-timeout", "30", "--max-time", "1800", "-sS",
           "-X", "POST", "-H", f"Authorization: Bearer {tok}",
           "-H", "Content-Type: application/octet-stream",
           "--data-binary", f"@{path}", "-o", "/dev/null",
           "-w", "%{http_code}", url]
    for attempt in range(1, 4):
        r = subprocess.run(cmd, capture_output=True, text=True)
        code = (r.stdout or "").strip()
        if code in ("201", "202"):
            print(f"上传成功: {name} ({size} bytes)")
            return True
        print(f"上传返回 {code!r} (第{attempt}次): {r.stderr.strip()[:200]}")
        time.sleep(5 * attempt)
    return False

def main():
    tag = sys.argv[1]
    fpk = sys.argv[2]
    title = sys.argv[3] if len(sys.argv) > 3 else tag
    tok = token()
    # 查询是否已有该 tag 的 release（GET 404 = 不存在，走创建）
    code, rel = http("GET", f"https://api.github.com/repos/{REPO}/releases/tags/{tag}", tok)
    if code == 200 and rel:
        rel_id, upload_url = rel["id"], rel["upload_url"]
        print(f"Release 已存在: {rel['html_url']}")
    else:
        body = {"tag_name": tag, "name": title, "body": "", "draft": False, "prerelease": False}
        code, rel = http("POST", f"https://api.github.com/repos/{REPO}/releases", tok,
                         data=json.dumps(body).encode(), headers={"Content-Type": "application/json"})
        if code not in (201, 200):
            print("创建 Release 失败:", code, rel); sys.exit(1)
        rel_id, upload_url = rel["id"], rel["upload_url"]
        print(f"Release 已创建: {rel['html_url']}")
    # 已有同名资产则先删
    code, assets = http("GET", f"https://api.github.com/repos/{REPO}/releases/{rel_id}/assets", tok)
    for a in assets or []:
        if a["name"] == fpk.replace("\\", "/").split("/")[-1]:
            http("DELETE", f"https://api.github.com/repos/{REPO}/releases/assets/{a['id']}", tok)
            print(f"已删除旧资产: {a['name']}")
    name = fpk.replace("\\", "/").split("/")[-1]
    if not upload_asset(tok, upload_url, name, fpk):
        print("资产上传失败"); sys.exit(1)
    print("DONE")

main()
