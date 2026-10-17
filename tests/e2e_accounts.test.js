// X-Box v1.2.29 端到端测试（jsdom 加载真实页面 JS + fetch 桩模拟后端）：
//  1) 已登录 boot 直通：.app 显示、登录层隐藏（v1.2.28 回归）
//  2) 账户管理列表：全部账户渲染、当前登录徽标、非当前账户才有删除
//  3) 新增/改密/改名/删除：模态表单 → 确认 → 正确的 API 与 payload
//  4) v1.2.29 回归：DDNS 表格包 tblwrap、登录输入 16px、小屏 0.15s 过渡、退出登录在卡片底部、无 prompt()
const fs = require('fs');
const path = require('path');
const { JSDOM } = require('jsdom');

const html = fs.readFileSync(path.join(__dirname, '..', 'src', 'web', 'index.html'), 'utf-8');
const failures = [];
const check = (cond, msg) => { if (!cond) failures.push(msg); };

// ---- fetch 桩：记录所有调用 + 按路径返回 ----
const calls = []; // {url, method, body}
function jsonRes(obj) {
  return { ok: true, status: 200, json: async () => obj };
}
const usersDB = [
  { user: 'admin', current: true },
  { user: 'bob', current: false },
];
const stubFetch = async (url, opts = {}) => {
  const u = String(url);
  calls.push({ url: u, method: opts.method || 'GET', body: opts.body || '' });
  if (u.includes('/api/auth/status')) return jsonRes({ ok: true, configured: true, authed: true, user: 'admin' });
  if (u.includes('/api/auth/listusers')) return jsonRes({ ok: true, users: usersDB });
  if (u.includes('/api/auth/adduser')) return jsonRes({ ok: true });
  if (u.includes('/api/auth/changepass')) return jsonRes({ ok: true });
  if (u.includes('/api/auth/rename')) return jsonRes({ ok: true });
  if (u.includes('/api/auth/deluser')) return jsonRes({ ok: true });
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
    window.setInterval = () => 0; // 阻断轮询定时器
    window.setTimeout = () => 0;  // 阻断 startTempLoop 递归；测试自身用 Node 定时器
    window.confirm = () => true;
    window.prompt = () => { failures.push('FAIL: 页面调用了 prompt()（应使用模态表单）'); return null; };
  },
});
const { window } = dom;
const doc = window.document;
const sleep = (ms) => new Promise(r => setTimeout(r, ms)); // Node 定时器，不受页面桩影响

(async () => {
  await sleep(250); // 等 boot() 异步链完成

  // ---- 1) 登录直通 ----
  check(doc.querySelector('.app').classList.contains('show'), 'FAIL: .app 未显示（登录直通失败）');
  check(doc.getElementById('login-mask').style.display === 'none', 'FAIL: 登录层未隐藏');

  // ---- 2) 账户列表渲染 ----
  const rows = [...doc.querySelectorAll('#acct-list .acct-row')];
  check(rows.length === 2, 'FAIL: 账户行数=' + rows.length + '，应为 2');
  const rowTexts = rows.map(r => r.textContent);
  check(rowTexts.some(t => t.includes('admin') && t.includes('当前登录')), 'FAIL: admin 行缺「当前登录」徽标');
  check(rowTexts.some(t => t.includes('bob')), 'FAIL: bob 行未渲染');
  const bobRow = rows.find(r => r.textContent.includes('bob'));
  const adminRow = rows.find(r => r.textContent.includes('admin'));
  check(bobRow && [...bobRow.querySelectorAll('button')].some(b => b.textContent === '删除'), 'FAIL: bob 行缺删除按钮');
  check(adminRow && ![...adminRow.querySelectorAll('button')].some(b => b.textContent === '删除'), 'FAIL: 当前登录账户不应有删除按钮');
  check(!!bobRow.querySelector('.acct-avatar'), 'FAIL: 账户行缺头像圈');
  // 行内按钮应走模态（openChangePass/openRename），不再是内联 prompt 版本
  check(typeof window.openAddUser === 'function' && typeof window.openChangePass === 'function' && typeof window.openRename === 'function', 'FAIL: 模态函数缺失');
  check(typeof window.addUser === 'undefined' && typeof window.changePassFor === 'undefined' && typeof window.renameAccount === 'undefined', 'FAIL: 旧版 prompt 函数仍存在');

  // ---- 3a) 新增账户（模态） ----
  window.openAddUser();
  check(doc.getElementById('modal').classList.contains('show'), 'FAIL: 新增账户模态未弹出');
  doc.getElementById('au-user').value = 'carl';
  doc.getElementById('au-pass').value = 'pw123';
  doc.getElementById('modal-ok').click();
  await sleep(30);
  let c = calls.find(x => x.url.includes('/api/auth/adduser'));
  check(!!c && JSON.parse(c.body).user === 'carl' && JSON.parse(c.body).pass === 'pw123', 'FAIL: adduser payload 错误: ' + (c && c.body));
  check(!doc.getElementById('modal').classList.contains('show'), 'FAIL: 确认后模态未关闭');

  // ---- 3b) 改密（模态，target 携带） ----
  window.openChangePass('bob');
  check(doc.getElementById('modal').classList.contains('show'), 'FAIL: 改密模态未弹出');
  check(doc.getElementById('modal-title').textContent === '修改密码', 'FAIL: 改密模态标题错误');
  doc.getElementById('ap-new').value = 'newpw';
  doc.getElementById('modal-ok').click();
  await sleep(30);
  c = calls.find(x => x.url.includes('/api/auth/changepass'));
  check(!!c && JSON.parse(c.body).target === 'bob' && JSON.parse(c.body).new === 'newpw', 'FAIL: changepass payload 错误: ' + (c && c.body));

  // ---- 3c) 改名（模态） ----
  window.openRename('bob');
  doc.getElementById('ar-new').value = 'bobby';
  doc.getElementById('modal-ok').click();
  await sleep(30);
  c = calls.find(x => x.url.includes('/api/auth/rename'));
  check(!!c && JSON.parse(c.body).old === 'bob' && JSON.parse(c.body).new === 'bobby', 'FAIL: rename payload 错误: ' + (c && c.body));

  // ---- 3d) 删除（模态确认） ----
  window.delUser('bob');
  check(doc.getElementById('modal').classList.contains('show'), 'FAIL: 删除确认模态未弹出');
  doc.getElementById('modal-ok').click();
  await sleep(30);
  c = calls.find(x => x.url.includes('/api/auth/deluser'));
  check(!!c && JSON.parse(c.body).user === 'bob', 'FAIL: deluser payload 错误: ' + (c && c.body));

  // ---- 4) v1.2.29 回归 ----
  // ④ DDNS 两张表包进 .tblwrap（移动端溢出修复）；id 挂在 tbody 上，需向上找两层
  for (const id of ['ddns-providers', 'ddns-records']) {
    const t = doc.getElementById(id);
    check(!!t && !!t.closest('.tblwrap'), 'FAIL: #' + id + ' 未包进 tblwrap');
  }
  // ③ 登录输入 16px（防 iOS 聚焦放大） + ⑤ 小屏 0.15s 过渡（保留动画）
  check(/\.login-input\{[^}]*font-size:16px/.test(html), 'FAIL: .login-input 非 16px');
  check(/transition:transform \.15s ease,opacity \.15s ease/.test(html), 'FAIL: 小屏过渡动画非 0.15s');
  check(!/transition:none/.test(html), 'FAIL: 存在 transition:none（动画被关闭）');
  // ⑥ 退出登录在卡片底部独立区（不再挤在标题或内联块）
  const foot = doc.querySelector('.acct-footer');
  check(!!foot && [...foot.querySelectorAll('button')].some(b => b.textContent === '退出登录'), 'FAIL: 退出登录按钮不在 acct-footer');
  // 标题右侧为「+ 新增账户」主按钮
  const h3 = [...doc.querySelectorAll('h3')].find(h => h.textContent.includes('账户管理'));
  check(!!h3 && h3.querySelector('.h-action button').textContent.includes('新增账户'), 'FAIL: 标题右侧缺「+ 新增账户」');
  // 模态表单走 .form-row（对齐 DDNS 弹窗风格）
  window.openAddUser();
  check(!!doc.querySelector('#modal-body .form-row'), 'FAIL: 新增账户模态未用 form-row 结构');
  doc.getElementById('modal-ok').click(); // 空表单确认 → 应有 toast，不崩溃
  await sleep(30);
  check(calls.filter(x => x.url.includes('/api/auth/adduser')).length === 1, 'FAIL: 空表单不应发起 adduser 请求');

  if (failures.length) {
    console.error(failures.join('\n'));
    console.error('E2E FAIL: ' + failures.length + ' 项未通过');
    process.exit(1);
  }
  console.log('E2E OK: 登录直通 / 账户列表 / 新增·改密·改名·删除模态链路 / v1.2.29 回归 全部通过');
  process.exit(0);
})().catch(e => { console.error('E2E FAIL (异常):', e); process.exit(1); });
