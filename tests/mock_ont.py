#!/usr/bin/env python3
"""ZTE 风格框架页 mock（带会话校验）：
模拟光猫行为——未携带会话 Cookie 时所有 .gch 一律 404，菜单点击依赖会话。
用于验证代理是否能在浏览器丢 Cookie 的情况下仍维持会话。
跑法：python3 tests/mock_ont.py 8897
"""
import sys
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import urlparse, parse_qs

PAGES = {
    "1001": "设备信息",
    "1002": "PON统计数据",
    "1003": "WAN统计数据",
    "1004": "语音统计数据",
    "1005": "LAN统计数据",
}
VALID = set()  # 已发放的会话

FRAME = """<!DOCTYPE html><html><head><meta charset="gb2312"><title>ONT</title>
<style>*{margin:0;padding:0}iframe{{border:0;display:block}}</style></head><body>
<iframe width="900" height="60" src="top.gch" name="topFrame" id="topFrame" scrolling="no"></iframe>
<iframe width="900" height="500" src="getpage.gch?pid=1002&nextpage=1001" name="mainFrame" id="mainFrame" scrolling="no"></iframe>
</body></html>"""

TOP = """<!DOCTYPE html><html><head><meta charset="gb2312"><title>top</title>
<script src="/js/menu.js"></script></head><body>
<span>联通</span><input type="hidden" id="IF_UPLOADING" value="0">
<input type="hidden" id="temClickURL" value="">
<a href="javascript:openLink('getpage.gch?pid=1002&nextpage=1001')" id="t1">设备信息</a>
<a href="javascript:openLink('getpage.gch?pid=1002&nextpage=1002')" id="t2">统计数据</a>
</body></html>"""

# 三级菜单：完全复刻设备 menu.js 的跳转方式（top.mainFrame.location.href 赋值）
PAGE = """<!DOCTYPE html><html><head><meta charset="gb2312"><title>{title}</title>
<script src="/js/menu.js"></script></head><body>
<div id="cur">当前页：{title}（pid={pid}）</div>
<ul>
 <li><a href="javascript:openLink('getpage.gch?pid=1002&nextpage=1003')" id="m3">WAN统计数据</a></li>
 <li><a href="javascript:openLink('getpage.gch?pid=1002&nextpage=1004')" id="m4">语音统计数据</a></li>
 <li><a href="javascript:openLink('getpage.gch?pid=1002&nextpage=1005')" id="m5">LAN统计数据</a></li>
</ul>
<input type="hidden" id="IF_UPLOADING" value="0">
<input type="hidden" id="temClickURL" value="">
</body></html>"""

MENU_JS = """function ReplaceDemo(ss){return String(ss).replace(/ /g,"&nbsp;");}
function getObj(id){return document.getElementById(id);}
function openLink(pageurl){
  var tag=getObj("IF_UPLOADING").value;
  if(tag=="1"){top.mainFrame.location.href="#";return;}
  if(getObj("temClickURL").value==""){
    getObj("temClickURL").value=pageurl;
    top.mainFrame.location.href=ReplaceDemo(pageurl);
  }else if(getObj("temClickURL").value!=pageurl){
    getObj("temClickURL").value=pageurl;
    top.mainFrame.location.href=ReplaceDemo(pageurl);
  }else{
    top.mainFrame.location.href="#";
  }
}"""

NOTFOUND = """<HTML><HEAD><TITLE>404 Not Found</TITLE></HEAD><BODY><H2>404 Not Found</H2>
The requested URL was not found on this server.<HR><ADDRESS>Mini web server ZTE corp 2005.</ADDRESS></BODY></HTML>"""


class H(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, *a):
        pass

    def _send(self, code, body, ct="text/html", extra=None):
        b = body.encode("gb18030", "replace")
        self.send_response(code)
        self.send_header("Content-Type", ct + "; charset=gb2312")
        self.send_header("Content-Length", str(len(b)))
        for k, v in (extra or []):
            self.send_header(k, v)
        self.end_headers()
        self.wfile.write(b)

    def do_GET(self):
        u = urlparse(self.path)
        q = parse_qs(u.query)
        ck = self.headers.get("Cookie") or ""
        sid = ""
        for part in ck.split(";"):
            part = part.strip()
            if part.startswith("SID="):
                sid = part[4:]

        # 登录页 / 框架页：发放会话
        if u.path in ("/", "/unicom.html"):
            sid = str(uuid.uuid4())[:8]
            VALID.add(sid)
            return self._send(200, FRAME, extra=[("Set-Cookie", "SID=%s; Path=/" % sid)])

        # 所有 .gch 都要求有效会话，否则 404（与真实设备一致）
        if u.path.endswith(".gch"):
            if not sid or sid not in VALID:
                return self._send(404, NOTFOUND)
            if u.path == "/top.gch":
                return self._send(200, TOP)
            if u.path == "/getpage.gch":
                pid = (q.get("nextpage") or ["1001"])[0]
                title = PAGES.get(pid, "未知页")
                return self._send(200, PAGE.format(title=title, pid=pid))

        if u.path == "/js/menu.js":
            return self._send(200, MENU_JS, ct="application/javascript")
        return self._send(404, NOTFOUND)


if __name__ == "__main__":
    port = int(sys.argv[1]) if len(sys.argv) > 1 else 8897
    ThreadingHTTPServer(("127.0.0.1", port), H).serve_forever()
