// 光猫代理登录 + 三级菜单复现（凭据只从环境变量读取）
// 用法：XBOX_ONT_PASS=*** node tests/ont_menu.js
const { chromium } = require('playwright');
const PASS = process.env.XBOX_ONT_PASS || '';
const base = process.env.XBOX_URL || 'http://127.0.0.1:8899/app/X-Box/';

(async () => {
  const browser = await chromium.launch({ channel: 'msedge' });
  const ctx = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  const page = await ctx.newPage();
  const bad = [];
  page.on('response', r => { if (r.status() >= 400) bad.push(r.status() + ' ' + r.url().slice(0, 180)); });

  await page.goto(base, { waitUntil: 'networkidle', timeout: 25000 });
  await page.evaluate(() => { const m = document.querySelector('.menu-item[data-page="browser"]'); if (m) m.click(); });
  await page.waitForTimeout(300);
  await page.fill('#browser-url', 'http://192.168.1.1/');
  await page.evaluate(() => goBrowser());
  await page.waitForTimeout(6000);

  // 登录：切换管理员 → 填密码 → dosubmit()
  if (PASS) {
    const r = await page.evaluate((pw) => {
      const f = [...document.querySelectorAll('.browser-frame-item')].find(x => x.classList.contains('on'));
      const w = f.contentWindow, d = f.contentDocument;
      const pwd = d.getElementById('Frm_Password');
      if (!pwd) return 'no pwd field (可能已登录)';
      try { w.GetAccessAccount('adm'); } catch (e) { return 'GetAccessAccount err ' + e.message; }
      pwd.disabled = false;
      pwd.value = pw;
      pwd.dispatchEvent(new w.Event('input', { bubbles: true }));
      try { w.dosubmit(); } catch (e) { return 'dosubmit err ' + e.message; }
      return 'submitted';
    }, PASS);
    console.log('[1] 登录:', r);
    await page.waitForTimeout(10000);
  }

  console.log('[2] 地址栏:', await page.inputValue('#browser-url'));
  console.log('[3] frames:', page.frames().map(f => f.url().replace('http://127.0.0.1:8899/app/X-Box', '').slice(0, 110)));

  // 框架页结构 + 各子框架里的菜单
  const info = await page.evaluate(() => {
    const out = { iframes: [], menus: [] };
    const f = [...document.querySelectorAll('.browser-frame-item')].find(x => x.classList.contains('on'));
    if (!f || !f.contentDocument) return { err: 'no frame' };
    const d = f.contentDocument;
    d.querySelectorAll('iframe').forEach(i => out.iframes.push({ name: i.name, id: i.id, src: (i.getAttribute('src') || '') }));
    // 在每个子框架里找可点的菜单项（含中文 + href/onclick）
    d.querySelectorAll('iframe').forEach(i => {
      try {
        const sd = i.contentDocument;
        if (!sd) return;
        sd.querySelectorAll('a,li,div,span').forEach(el => {
          const t = (el.textContent || '').trim();
          if (!/统计数据|PON|WAN|语音|LAN|WLAN|状态|网络|安全|应用|管理|设备信息/.test(t)) return;
          if (t.length > 14) return;
          const href = el.getAttribute && el.getAttribute('href');
          const oc = el.getAttribute && el.getAttribute('onclick');
          if ((href && href !== 'javascript:void(0);') || oc) {
            if (out.menus.length < 30) out.menus.push({ frame: i.name || i.id, t: t.slice(0, 14), href: (href || '').slice(0, 70), onclick: (oc || '').slice(0, 80) });
          }
        });
      } catch (e) {}
    });
    return out;
  });
  console.log('[4] iframes:', JSON.stringify(info.iframes || info));
  console.log('[5] 菜单项样例:');
  (info.menus || []).slice(0, 14).forEach(m => console.log('    ', JSON.stringify(m)));

  // 点击第一个"统计数据/三级"菜单，观察 mainFrame 的 src 是否变化
  const before = await page.evaluate(() => {
    const f = [...document.querySelectorAll('.browser-frame-item')].find(x => x.classList.contains('on'));
    const i = f.contentDocument.querySelector('iframe[name="mainFrame"],#mainFrame');
    return i ? i.src : '';
  });
  const clicked = await page.evaluate(() => {
    const f = [...document.querySelectorAll('.browser-frame-item')].find(x => x.classList.contains('on'));
    const d = f.contentDocument;
    const cands = [];
    d.querySelectorAll('iframe').forEach(i => {
      try {
        const sd = i.contentDocument; if (!sd) return;
        sd.querySelectorAll('a').forEach(a => { const t = (a.textContent || '').trim(); if (/PON|WAN|语音|LAN|WLAN/.test(t) && t.length < 14) cands.push(a); });
      } catch (e) {}
    });
    if (!cands.length) return 'no third-level link';
    const a = cands[0];
    const rec = { text: (a.textContent || '').trim(), href: a.getAttribute('href'), target: a.getAttribute('target'), onclick: (a.getAttribute('onclick') || '').slice(0, 100) };
    a.click();
    return JSON.stringify(rec);
  });
  console.log('[6] 点击三级菜单:', clicked);
  await page.waitForTimeout(5000);

  const after = await page.evaluate(() => {
    const f = [...document.querySelectorAll('.browser-frame-item')].find(x => x.classList.contains('on'));
    const i = f.contentDocument.querySelector('iframe[name="mainFrame"],#mainFrame');
    return i ? i.src : '';
  });
  console.log('[7] mainFrame 前:', before.replace('http://127.0.0.1:8899/app/X-Box', ''));
  console.log('[7] mainFrame 后:', after.replace('http://127.0.0.1:8899/app/X-Box', ''));
  console.log('[8] 是否变化:', before !== after ? 'YES（正常）' : 'NO（未响应）');
  console.log('[9] 失败请求:', bad.length); bad.slice(0, 10).forEach(b => console.log('    ', b));
  await page.screenshot({ path: 'tests/ont_menu.png' });
  await browser.close();
})().catch(e => { console.error('FAIL', e.message); process.exit(1); });
