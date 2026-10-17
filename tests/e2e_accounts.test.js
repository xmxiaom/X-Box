// X-Box 登录/账户端到端测试（jsdom 加载真实页面 JS + fetch 桩模拟后端）：
//  场景A（已登录）：boot 直通 → .app 显示、登录层隐藏；账户管理列表/模态全链路
//  场景B（未登录，fail-safe）：登录层【默认可见】（JS 挂了也是登录框而非空白）；
//          错误密码显示后端错误信息；正确密码登录 → .app 显示
//  回归：DDNS 表格包 tblwrap、登录输入 16px、小屏 0.15s 过渡、退出登录在卡片底部、无 prompt()、ES2017 语法地板
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const html = fs.readFileSync(path.join(__dirname, '..', 'src', 'web', 'index.html'), 'utf-8');
const failures = [];
const check = (cond, msg) => { if (!cond) failures.push(msg); };
const sleep = (ms) => new Promise(r => setTimeout(r, ms)); // Node 定时器

function jsonRes(obj) { return { ok: true, status: 200, json: async () => obj }; }

function makeDom(authed, opts) {
  opts = opts || {};
  const calls = [];
  const usersDB = [
    { user: 'admin', current: true },
    { user: 'bob', current: false },
  ];
  const stubFetch = async (url, o) => {
    o = o || {};
    const u = String(url);
    calls.push({ url: u, method: o.method || 'GET', body: o.body || '', headers: (o && o.headers) || {} });
    if (u.includes('/api/auth/status')) return jsonRes(opts.statusOverride || { ok: true, configured: true, authed: authed, user: authed ? 'admin' : '' });
    if (u.includes('/api/login')) {
      const b = JSON.parse(o.body || '{}');
      if (b.pass === 'pw-correct') return jsonRes({ ok: true, sid: 'test-sid-123' });
      return { ok: false, status: 401, json: async () => ({ ok: false, error: '账户或密码错误' }) };
    }
    if (u.includes('/api/auth/listusers')) return jsonRes({ ok: true, users: usersDB });
    if (u.includes('/api/overview')) return jsonRes({
      ok: true, version: '1.2.29', hostname: 'nas', cpuCores: 4, cpuModel: 'Test CPU', load1: 0.5,
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
      window.fetch = stubFetch;
      // XMLHttpRequest 桩（jsdom 自带 XHR 会走真实网络打 127.0.0.1:8899 失败）：
      // 登录通道现在由 ES5 独立脚本经 XHR 发送，必须桩掉才能测
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
      if (opts.noFetch) delete window.fetch; // 场景C：模拟 fetch 不可用的降级内核
      window.setInterval = () => 0; // 阻断轮询定时器
      window.setTimeout = () => 0;  // 阻断 startTempLoop 递归；测试自身用 Node 定时器
      window.confirm = () => true;
      window.prompt = () => { failures.push('FAIL: 页面调用了 prompt()（应使用模态表单）'); return null; };
    },
  });
  return { calls, doc: dom.window.document, win: dom.window, usersDB };
}

(async () => {
  /* ---------- 场景A：已登录直通 + 账户管理 ---------- */
  {
    const { calls, doc, win } = makeDom(true);
    await sleep(250);
    check(doc.querySelector('.app').classList.contains('show'), 'A: .app 未显示（登录直通失败）');
    check(doc.getElementById('login-mask').style.display === 'none', 'A: 登录层未隐藏');

    const rows = [...doc.querySelectorAll('#acct-list .acct-row')];
    check(rows.length === 2, 'A: 账户行数=' + rows.length + '，应为 2');
    const rowTexts = rows.map(r => r.textContent);
    check(rowTexts.some(t => t.includes('admin') && t.includes('当前登录')), 'A: admin 行缺「当前登录」徽标');
    check(rowTexts.some(t => t.includes('bob')), 'A: bob 行未渲染');
    const bobRow = rows.find(r => r.textContent.includes('bob'));
    const adminRow = rows.find(r => r.textContent.includes('admin'));
    check(bobRow && [...bobRow.querySelectorAll('button')].some(b => b.textContent === '删除'), 'A: bob 行缺删除按钮');
    check(adminRow && ![...adminRow.querySelectorAll('button')].some(b => b.textContent === '删除'), 'A: 当前登录账户不应有删除按钮');
    check(!!bobRow.querySelector('.acct-avatar'), 'A: 账户行缺头像圈');
    check(typeof win.openAddUser === 'function' && typeof win.openChangePass === 'function' && typeof win.openRename === 'function', 'A: 模态函数缺失');

    win.openAddUser();
    check(doc.getElementById('modal').classList.contains('show'), 'A: 新增账户模态未弹出');
    doc.getElementById('au-user').value = 'carl';
    doc.getElementById('au-pass').value = 'pw123';
    doc.getElementById('modal-ok').click();
    await sleep(30);
    let c = calls.find(x => x.url.includes('/api/auth/adduser'));
    check(!!c && JSON.parse(c.body).user === 'carl' && JSON.parse(c.body).pass === 'pw123', 'A: adduser payload 错误: ' + (c && c.body));
    check(!doc.getElementById('modal').classList.contains('show'), 'A: 确认后模态未关闭');

    win.openChangePass('bob');
    check(doc.getElementById('modal-title').textContent === '修改密码', 'A: 改密模态标题错误');
    doc.getElementById('ap-new').value = 'newpw';
    doc.getElementById('modal-ok').click();
    await sleep(30);
    c = calls.find(x => x.url.includes('/api/auth/changepass'));
    check(!!c && JSON.parse(c.body).target === 'bob' && JSON.parse(c.body).new === 'newpw', 'A: changepass payload 错误: ' + (c && c.body));

    win.openRename('bob');
    doc.getElementById('ar-new').value = 'bobby';
    doc.getElementById('modal-ok').click();
    await sleep(30);
    c = calls.find(x => x.url.includes('/api/auth/rename'));
    check(!!c && JSON.parse(c.body).old === 'bob' && JSON.parse(c.body).new === 'bobby', 'A: rename payload 错误: ' + (c && c.body));

    win.delUser('bob');
    check(doc.getElementById('modal').classList.contains('show'), 'A: 删除确认模态未弹出');
    doc.getElementById('modal-ok').click();
    await sleep(30);
    c = calls.find(x => x.url.includes('/api/auth/deluser'));
    check(!!c && JSON.parse(c.body).user === 'bob', 'A: deluser payload 错误: ' + (c && c.body));

    // 空表单新增 → 应有 toast 提示，不发起请求、不崩溃
    win.openAddUser();
    const addCallsBefore = calls.filter(x => x.url.includes('/api/auth/adduser')).length;
    doc.getElementById('modal-ok').click();
    await sleep(30);
    check(calls.filter(x => x.url.includes('/api/auth/adduser')).length === addCallsBefore, 'A: 空表单不应发起 adduser 请求');
  }

  /* ---------- 场景B：未登录 fail-safe + 登录链路 ---------- */
  {
    const { calls, doc, win } = makeDom(false);
    // 关键 fail-safe：登录层在 HTML 默认可见（不依赖 JS 显示），旧内核 JS 挂掉也是登录框而非空白。
    // 初始可见性来自样式表（非内联），需用 getComputedStyle 检查
    check(win.getComputedStyle(doc.getElementById('login-mask')).display === 'flex', 'B: 登录层初始不可见（fail-safe 失效）');
    await sleep(200);
    check(!doc.querySelector('.app').classList.contains('show'), 'B: 未登录时 .app 不应显示');
    check(doc.getElementById('login-mask').style.display === 'flex', 'B: boot 后登录层应保持可见');

    // 错误密码 → 显示后端错误信息，不进入面板
    doc.getElementById('login-user').value = 'admin';
    doc.getElementById('login-pass').value = 'wrong';
    doc.getElementById('login-btn').click();
    await sleep(50);
    check(doc.getElementById('login-msg').textContent === '账户或密码错误', 'B: 错误密码未显示后端错误: ' + doc.getElementById('login-msg').textContent);
    check(!doc.querySelector('.app').classList.contains('show'), 'B: 登录失败不应进入面板');
    check(doc.getElementById('login-btn').disabled === false, 'B: 登录失败后按钮未恢复可点');

    // 正确密码 → 进入面板，登录层隐藏，账户列表加载
    doc.getElementById('login-pass').value = 'pw-correct';
    doc.getElementById('login-btn').click();
    await sleep(80);
    check(doc.querySelector('.app').classList.contains('show'), 'B: 登录成功后 .app 未显示');
    check(doc.getElementById('login-mask').style.display === 'none', 'B: 登录成功后登录层未隐藏');
    check(doc.getElementById('ver').textContent === 'v1.2.29', 'B: 登录后未加载概览（ver 未更新）'); // let 声明的 currentUser 不挂 window，用概览加载结果验证 enterApp
    check(win.__xboxToken === 'test-sid-123', 'B: 登录后 token 未存入 window.__xboxToken');
    check(win.localStorage.getItem('xbox_sid_token') === 'test-sid-123', 'B: token 未持久化到 localStorage');
    // Bearer 链路：登录后的 API 请求必须带 Authorization 头部（iframe 第三方 Cookie 被拦截时的生命线）
    const listCall = calls.find(x => x.url.includes('/api/auth/listusers'));
    check(!!listCall && listCall.headers['Authorization'] === 'Bearer test-sid-123', 'B: 登录后请求未带 Bearer 头部');

    // 退出登录 → 回登录层，token 清除
    win.doLogout();
    await sleep(30);
    check(doc.getElementById('login-mask').style.display === 'flex', 'B: 退出登录后未回登录层');
    check(!doc.querySelector('.app').classList.contains('show'), 'B: 退出登录后 .app 仍显示');
    check(win.__xboxToken === '' && win.localStorage.getItem('xbox_sid_token') === null, 'B: 退出登录后 token 未清除');
  }

  /* ---------- 场景C：降级内核（fetch 不可用）→ 登录必须仍可用 ---------- */
  {
    // 模拟旧 webview：fetch 不存在（主脚本 api() 会抛错但被 boot/enterApp 的 try/catch 兜住），
    // 登录走 ES5 + XHR 独立层，必须能正常登录并进入面板
    const { doc, win } = makeDom(false, { noFetch: true });
    check(win.getComputedStyle(doc.getElementById('login-mask')).display === 'flex', 'C: 登录层初始不可见（fail-safe 失效）');
    await sleep(200);
    check(!doc.querySelector('.app').classList.contains('show'), 'C: 未登录时 .app 不应显示');
    doc.getElementById('login-user').value = 'admin';
    doc.getElementById('login-pass').value = 'pw-correct';
    doc.getElementById('login-btn').click();
    await sleep(80);
    check(doc.querySelector('.app').classList.contains('show'), 'C: fetch 缺失时登录后 .app 未显示');
    check(doc.getElementById('login-mask').style.display === 'none', 'C: fetch 缺失时登录后登录层未隐藏');
    check(win.__xboxToken === 'test-sid-123', 'C: fetch 缺失时 token 未保存');
    // 错误密码同样要有提示
    win.doLogout();
    await sleep(30);
    doc.getElementById('login-pass').value = 'wrong';
    doc.getElementById('login-btn').click();
    await sleep(50);
    check(doc.getElementById('login-msg').textContent === '账户或密码错误', 'C: 降级内核错误密码无提示');
  }

  /* ---------- 回归项（静态检查同一文档） ---------- */
  {
    const { doc } = makeDom(true);
    for (const id of ['ddns-providers', 'ddns-records']) {
      const el = doc.getElementById(id);
      check(!!el && !!el.closest('.tblwrap'), 'R: #' + id + ' 未包进 tblwrap');
    }
    const foot = doc.querySelector('.acct-footer');
    check(!!foot && [...foot.querySelectorAll('button')].some(b => b.textContent === '退出登录'), 'R: 退出登录按钮不在 acct-footer');
  }
  check(/\.login-input\{[^}]*font-size:16px/.test(html), 'R: .login-input 非 16px');
  check(/transition:transform \.15s ease,opacity \.15s ease/.test(html), 'R: 小屏过渡动画非 0.15s');
  check(!/transition:none/.test(html), 'R: 存在 transition:none（动画被关闭）');
  check(/login-mask\{[^}]*display:flex/.test(html), 'R: 登录层 CSS 默认非 flex（fail-safe 缺失）');
  // 语法地板：主脚本必须 ES2017 可解析；ES5 登录层必须 ES5 可解析（零现代语法）
  const blocks = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)].map(m => m[1]);
  check(blocks.length === 2, 'R: script 块数量=' + blocks.length + '，应为 2（主脚本 + ES5 登录层）');
  const acorn = require('acorn');
  try { acorn.parse(blocks[0], { ecmaVersion: 2017 }); } catch (e) { failures.push('R: 主脚本非 ES2017 兼容: ' + e.message.slice(0, 60)); }
  try { acorn.parse(blocks[1], { ecmaVersion: 5 }); } catch (e) { failures.push('R: ES5 登录层非 ES5 兼容: ' + e.message.slice(0, 60)); }
  const stripped = blocks[1].replace(/\/\*[\s\S]*?\*\//g, '').replace(/(^|[^:])\/\/.*$/gm, '$1'); // 去注释，避免注释文字误报
  check(/\bconst\b|\blet\b|=>|async |await |\.\.\.|\?\?/.test(stripped) === false, 'R: ES5 登录层含现代语法');

  if (failures.length) {
    console.error(failures.join('\n'));
    console.error('E2E FAIL: ' + failures.length + ' 项未通过');
    process.exit(1);
  }
  console.log('E2E OK: 已登录直通 / 账户模态全链路 / 未登录 fail-safe + 登录成功失败链路 / ES2017 语法地板 全部通过');
  process.exit(0);
})().catch(e => { console.error('E2E FAIL (异常):', e); process.exit(1); });
