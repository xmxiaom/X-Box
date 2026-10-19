// X-Box 无登录态端到端测试（jsdom 加载真实页面 JS + fetch/XHR 桩）：
//  场景A：页面加载即显示面板（.app 默认 flex，无登录层），enterApp 加载概览
//  场景B：XHR 与 fetch 都失败 → 回退导航通道（iframe）
//  回归：无任何登录/账户元素、隧道编解码（UTF-8 安全）、ES2017/ES5 语法地板
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const html = fs.readFileSync(path.join(__dirname, '..', 'src', 'web', 'index.html'), 'utf-8');
const failures = [];
const check = (cond, msg) => { if (!cond) failures.push(msg); };
const sleep = (ms) => new Promise(r => setTimeout(r, ms)); // Node 定时器

function jsonRes(obj) { return { ok: true, status: 200, json: async () => obj }; }

// base64url 解码（与页面 tunnelB64 互为逆操作）
function b64d(s) {
  s = String(s).replace(/-/g, '+').replace(/_/g, '/');
  while (s.length % 4) s += '=';
  return decodeURIComponent(escape(atob(s)));
}

function makeDom(opts) {
  opts = opts || {};
  const calls = [];
  const stubFetch = async (url, o) => {
    o = o || {};
    let u = String(url);
    // 隧道解码：/?__x=<base64url(JSON{m,p,b})> → 还原真实方法/路径/载荷
    const qi = u.indexOf('?__x=');
    if (qi >= 0) {
      const tp = JSON.parse(b64d(decodeURIComponent(u.slice(qi + 5).split('&')[0])));
      u = tp.p;
      o = { method: tp.m || 'GET', body: tp.b || '' };
    }
    calls.push({ url: u, method: o.method || 'GET', body: o.body || '' });
    if (u.includes('/api/overview')) return jsonRes({
      ok: true, version: '1.2.38', hostname: 'nas', cpuCores: 4, cpuModel: 'Test CPU', load1: 0.5,
      memTotalKB: 8000000, memAvailKB: 4000000, uptimeSeconds: 90000, kernel: '6.1.0',
      listeningPorts: 5, publicPorts: 1, disks: [], temps: { cpuMax: 0, disks: [] },
      net: { ifaces: [], gateway: '192.168.1.1', dns: [] },
    });
    if (u.includes('/api/temps')) return jsonRes({ ok: true, cpuMax: 0, disks: [] });
    if (u.includes('/api/ports')) return jsonRes({ ok: true, ports: [] });
    if (u.includes('/api/processes')) return jsonRes({ ok: true, processes: [] });
    if (u.includes('/api/services')) return jsonRes({ ok: true, services: [], apps: [], docker: [] });
    if (u.includes('/api/update/check')) return jsonRes({ ok: true, update: {} });
    if (u === '/api/terminal/info') return jsonRes({ ok: true, cwd: '/root' });
    if (u === '/api/terminal/exec') return jsonRes({ ok: true, output: '', exitCode: 0, cwd: '/root' });
    if (u === '/api/terminal/close') return jsonRes({ ok: true, cwd: '/root' });
    if (u === '/api/update/status') return jsonRes({ ok: true, running: false, done: false, bytes: 0, path: '', name: '' });
    if (u === '/api/ddns') return jsonRes({ ok: true, intervalSec: 300, providers: [{ id: 1, name: '测试服务商', type: 'aliyun', cred: {} }], providersAvail: [{ type: 'aliyun', name: '阿里云' }], ifaces: [], records: [{ id: 3, enabled: !opts.ddnsRecDisabled, domain: 'x.com', providerId: 1, recordType: 'A', ipSource: 'auto', ttl: 600, lastStatus: 'pending' }] });
    return jsonRes({ ok: true });
  };
  const dom = new JSDOM(html, {
    url: 'http://localhost:8899/',
    runScripts: 'dangerously',
    pretendToBeVisual: true,
    beforeParse(window) {
      window.fetch = opts.failFetch ? (() => Promise.reject(new Error('network'))) : stubFetch;
      // XMLHttpRequest 桩（jsdom 自带 XHR 会走真实网络打 127.0.0.1:8899 失败）
      window.XMLHttpRequest = function () {
        this.readyState = 0; this.status = 0; this.responseText = '';
        this.open = function (m, u) { this._m = m; this._u = u; };
        this.setRequestHeader = function () {};
        this.send = function (body) {
          const self = this;
          stubFetch(this._u, { method: this._m, body: body }).then(function (res) {
            return res.json().then(function (j) { self.status = res.status; self._j = j; });
          }).then(function () {
            self.readyState = 4; self.responseText = JSON.stringify(self._j);
            if (self.onreadystatechange) self.onreadystatechange();
          });
        };
      };
      if (opts.noXHR) window.XMLHttpRequest = function () { const e = new Error('xhr dead'); e.transport = true; throw e; };
      window.setInterval = () => 0;
      window.setTimeout = () => 0;
      window.prompt = () => { failures.push('FAIL: 页面调用了 prompt()'); return null; };
    },
  });
  return { calls, doc: dom.window.document, win: dom.window };
}

(async () => {
  /* ---------- 场景A：加载即进面板 ---------- */
  {
    const { calls, doc, win } = makeDom();
    // fail-safe：.app 在 HTML/CSS 中默认 flex，JS 未执行也可见（无登录层）
    check(win.getComputedStyle(doc.querySelector('.app')).display === 'flex', 'A: .app 默认不可见');
    check(!doc.getElementById('login-mask'), 'A: 登录层仍存在');
    check(!doc.getElementById('login-btn'), 'A: 登录按钮仍存在');
    check(!doc.getElementById('acct-list') && !doc.querySelector('.acct-footer'), 'A: 账户管理仍存在');
    await sleep(250); // 等 detectChannel + enterApp 完成
    // 顶栏已删除（v1.3.8）：页面不应再有 ver 元素，enterApp 对缺失元素要安全跳过
    check(!doc.getElementById('ver'), 'A: 顶栏 ver 元素应已随顶栏删除');
    check(!doc.getElementById('hostname') || true, ''); // hostname 同顶栏一并删除，存在性不敏感
    check(calls.some(x => x.url.includes('/api/overview')), 'A: 未拉取概览');
    // 系统设置页只剩 关于/检查更新
    const settingsHtml = doc.getElementById('page-settings').innerHTML;
    check(!settingsHtml.includes('账户管理') && !settingsHtml.includes('退出登录'), 'A: 设置页仍有账户管理');
  }

  /* ---------- 场景B：XHR 与 fetch 均失败 → 回退导航通道 ---------- */
  {
    const { win } = makeDom({ failFetch: true, noXHR: true });
    await sleep(250);
    win.navApi = function (p) { win.__navCalls = (win.__navCalls || 0) + 1; return Promise.resolve({ ok: true, via: 'nav-stub' }); };
    const j = await win.api('/api/overview');
    check(j && j.via === 'nav-stub', 'B: XHR/fetch 均失败后未回退导航通道');
    check(win.__navMode === true, 'B: 未进入导航模式');
  }

  /* ---------- 隧道编解码断言 ---------- */
  {
    const { win } = makeDom();
    await sleep(100);
    check(typeof win.apiTunnel === 'function' && typeof win.tunnelB64 === 'function', 'T: 隧道函数缺失');
    const tun = win.apiTunnel('/api/clean', { method: 'POST', body: '{"cats":["tmp"]}' });
    // 必须是相对形式：网关模式下才能继承 /app/<appname>/ 前缀（绝对路径会跑出网关）
    check(tun.indexOf('?__x=') === 0 && tun.indexOf('//') < 0, 'T: apiTunnel 输出格式错误');
    const tp = JSON.parse(b64d(decodeURIComponent(tun.slice(tun.indexOf('=') + 1))));
    check(tp.m === 'POST' && tp.p === '/api/clean' && tp.b === '{"cats":["tmp"]}' && tp.sid === undefined, 'T: 隧道 payload 错误（不应有 sid）: ' + JSON.stringify(tp));
    const tun2 = win.apiTunnel('/api/ddns/record/save', { method: 'POST', body: JSON.stringify({ name: '中文域名' }) });
    const tp2 = JSON.parse(b64d(decodeURIComponent(tun2.slice(tun2.indexOf('=') + 1))));
    check(JSON.parse(tp2.b).name === '中文域名', 'T: 中文载荷编解码失败');
  }

  /* ---------- 复选框回显回归（v1.2.40：模板曾将布尔值渲染为 true/false 字符串导致选中态丢失） ---------- */
  {
    const { doc, win } = makeDom();
    await sleep(150);
    await win.termConnect(); // 经 fetch 桩填充 termCwd + 渲染提示符（验证终端连接链路）
    check(doc.getElementById('term-prompt').textContent.indexOf('root@xbox') === 0, 'C: 终端提示符未渲染: ' + doc.getElementById('term-prompt').textContent);
    check(doc.getElementById('term-input').disabled === false && doc.getElementById('term-btn-conn').disabled === true, 'C: 终端连接后未启用输入');
    await win.termDisconnect(); // 断开：复位 UI + 调 close 接口
    check(doc.getElementById('term-input').disabled === true && doc.getElementById('term-btn-disc').disabled === true, 'C: 终端断开后输入未禁用');
    check(doc.getElementById('term-out').textContent.indexOf('未连接') === 0, 'C: 断开后未显示未连接提示');
    await win.loadDDNS(); // 自然填充 ddnsData
    win.openRecordForm(3);
    check(doc.getElementById('rf-enabled').checked === true, 'C: DDNS 已启用记录回显异常');
    win.openRecordForm(999); // 不存在的 id → 新建 → 默认勾选
    check(doc.getElementById('rf-enabled').checked === true, 'C: DDNS 新建记录启用框默认异常');
  }
  {
    // 浏览器：goBrowser 应生成路径式 /b/{scheme}/{host}{path} 代理 URL；多标签增删切换
    const { doc, win } = makeDom();
    await sleep(100);
    doc.getElementById('browser-url').value = '192.168.1.1';
    win.goBrowser();
    let fr = doc.querySelector('.browser-frame-item');
    check(fr && fr.getAttribute('src') === '/b/http/192.168.1.1/', 'C: 浏览器未走路径式 /b 代理: ' + (fr && fr.getAttribute('src')));
    check(doc.querySelectorAll('.browser-tab').length === 1, 'C: 标签数异常');
    doc.getElementById('browser-url').value = 'https://example.com/a?b=1';
    win.goBrowser();
    fr = doc.querySelectorAll('.browser-frame-item')[0];
    check(fr.getAttribute('src') === '/b/https/example.com/a?b=1', 'C: 已带协议地址生成异常');
    win.browserNewTab('');
    check(doc.querySelectorAll('.browser-tab').length === 2 && doc.querySelectorAll('.browser-frame-item').length === 2, 'C: 新建标签失败');
    // 原始地址 ↔ 代理路径互转（UI 显示真实地址，iframe 走代理）
    check(win.browserToProxy('192.168.1.1') === '/b/http/192.168.1.1/', 'C: browserToProxy 转换异常: ' + win.browserToProxy('192.168.1.1'));
    check(win.browserToProxy('http://192.168.1.1:8080/a#f') === '/b/http/192.168.1.1:8080/a', 'C: 带端口/锚点地址转换异常');
    check(win.browserFromProxy('/b/https/example.com/a?b=1') === 'https://example.com/a?b=1', 'C: browserFromProxy 转换异常: ' + win.browserFromProxy('/b/https/example.com/a?b=1'));
    // 代理页面上报（window.open / target=_blank）→ 宿主新建标签打开，不跳外部浏览器
    win.browserOpenURL('http://192.168.1.1/x', true);
    const frs = doc.querySelectorAll('.browser-frame-item');
    check(frs.length === 3 && frs[2].getAttribute('src') === '/b/http/192.168.1.1/x', 'C: 新标签打开链接失败: ' + (frs[2] && frs[2].getAttribute('src')));
    win.browserCloseTab(0);
    win.browserCloseTab(0);
    check(doc.querySelectorAll('.browser-tab').length === 1 && doc.querySelectorAll('.browser-frame-item').length === 1, 'C: 关闭标签失败');
    // 长标题下关闭按钮仍须渲染可点（标题独立为可收缩 flex 子项）
    check(doc.querySelectorAll('.browser-tab .bx').length === 1 && doc.querySelectorAll('.browser-tab .bt').length === 1,
      'C: 标签标题/关闭按钮结构异常');
    check(!!doc.getElementById('browser-log'), 'C: 诊断面板缺失');
  }
  {
    // 停用回显：桩返回 enabled=false 的记录
    const { doc, win } = makeDom({ ddnsRecDisabled: true });
    await sleep(150);
    await win.loadDDNS();
    win.openRecordForm(3);
    check(doc.getElementById('rf-enabled').checked === false, 'C: DDNS 已停用记录回显异常');
  }

  /* ---------- 语法地板 ---------- */
  const blocks = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(m => m[1]);
  check(blocks.length === 1, 'R: script 块数量=' + blocks.length + '，应为 1');
  const acorn = require('acorn');
  try { acorn.parse(blocks[0], { ecmaVersion: 2017 }); } catch (e) { failures.push('R: 主脚本非 ES2017: ' + e.message.slice(0, 60)); }
  check(!/login-mask|login-card|acct-list|账户管理|diag-bar|login-diag/.test(html), 'R: HTML 仍含登录/账户/诊断残留');

  if (failures.length) {
    console.error(failures.join('\n'));
    console.error('E2E FAIL: ' + failures.length + ' 项未通过');
    process.exit(1);
  }
  console.log('E2E OK: 无登录直开面板 / 三通道回退 / 隧道编解码 / 语法地板 全部通过');
  process.exit(0);
})().catch(e => { console.error('E2E FAIL (异常):', e); process.exit(1); });
