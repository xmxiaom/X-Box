#!/usr/bin/env python3
import os, sys, json, urllib.parse, urllib.request, urllib.error

TOKEN = os.environ["GITHUB_TOKEN"]
UPLOAD_URL = os.environ["UPLOAD_URL"].rstrip("/")
FILE_PATH = os.environ["FILE_PATH"]
NAME = os.environ.get("NAME") or os.path.basename(FILE_PATH)

url = f"{UPLOAD_URL}?name={urllib.parse.quote(NAME)}"
data = open(FILE_PATH, "rb").read()
req = urllib.request.Request(url, data=data, method="POST")
req.add_header("Authorization", f"token {TOKEN}")
req.add_header("Content-Type", "application/octet-stream")
req.add_header("Content-Length", str(len(data)))

for attempt in range(1, 4):
    try:
        with urllib.request.urlopen(req, timeout=120) as resp:
            code = resp.getcode()
            body = resp.read().decode("utf-8", "replace")
            print(f"attempt {attempt}: HTTP {code}, {len(data)} bytes")
            if 200 <= code <= 299:
                j = json.loads(body)
                print("browser_download_url:", j.get("browser_download_url"))
                sys.exit(0)
            else:
                print("resp:", body[:300])
    except urllib.error.HTTPError as e:
        print(f"attempt {attempt}: HTTPError {e.code} {e.read().decode('utf-8','replace')[:300]}")
    except Exception as e:
        print(f"attempt {attempt}: {type(e).__name__}: {e}")
    # rebuild request object (data stream consumed)
    req = urllib.request.Request(url, data=data, method="POST")
    req.add_header("Authorization", f"token {TOKEN}")
    req.add_header("Content-Type", "application/octet-stream")
    req.add_header("Content-Length", str(len(data)))

sys.exit(1)
