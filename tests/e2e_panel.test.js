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
    check(doc.getElementById('ver').textContent === 'v1.2.38', 'A: 版本未加载(enterApp 未跑): ' + doc.getElementById('ver').textContent);
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
    check(tun.indexOf('/?__x=') === 0, 'T: apiTunnel 输出格式错误');
    const tp = JSON.parse(b64d(decodeURIComponent(tun.slice(tun.indexOf('=') + 1))));
    check(tp.m === 'POST' && tp.p === '/api/clean' && tp.b === '{"cats":["tmp"]}' && tp.sid === undefined, 'T: 隧道 payload 错误（不应有 sid）: ' + JSON.stringify(tp));
    const tun2 = win.apiTunnel('/api/ddns/record/save', { method: 'POST', body: JSON.stringify({ name: '中文域名' }) });
    const tp2 = JSON.parse(b64d(decodeURIComponent(tun2.slice(tun2.indexOf('=') + 1))));
    check(JSON.parse(tp2.b).name === '中文域名', 'T: 中文载荷编解码失败');
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
