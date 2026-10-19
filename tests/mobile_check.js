// 手机视口实测：面板加载 → 输入百度地图 → 检查是否出现嵌套循环/白屏
// 用法：NODE_PATH=<managed node_modules> node tests/mobile_check.js [url]
const { chromium } = require('playwright');

(async () => {
  const base = process.env.XBOX_URL || 'http://127.0.0.1:8899/';
  const browser = await chromium.launch({ channel: 'msedge' });
  const ctx = await browser.newContext({
    viewport: { width: 390, height: 844 },
    deviceScaleFactor: 3,
    isMobile: true,
    hasTouch: true,
    userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1',
  });
  const page = await ctx.newPage();
  const errs = [];
  page.on('console', m => { if (m.type() === 'error') errs.push(m.text().slice(0, 160)); });
  page.on('pageerror', e => errs.push('PAGEERR ' + String(e).slice(0, 160)));
  const bad = [];
  page.on('response', r => { if (r.status() >= 400) bad.push(r.status() + ' ' + r.url().slice(0, 200)); });

  await page.goto(base, { waitUntil: 'networkidle', timeout: 20000 });
  const title = await page.title();
  console.log('[1] 面板标题:', title);

  // 面板自身是否有横向溢出
  const overflow = await page.evaluate(() => document.documentElement.scrollWidth - window.innerWidth);
  console.log('[2] 面板横向溢出(px):', overflow);

  // 切到浏览器页签
  await page.evaluate(() => { const m = document.querySelector('.menu-item[data-page="browser"]'); if (m) m.click(); });
  await page.waitForTimeout(400);

  // 输入百度地图
  await page.fill('#browser-url', 'https://map.baidu.com/');
  await page.evaluate(() => goBrowser());
  await page.waitForTimeout(8000);

  // 读取 iframe 内容
  const info = await page.evaluate(() => {
    const f = [...document.querySelectorAll('.browser-frame-item')].find(x => x.classList.contains('on'));
    if (!f) return { err: 'no frame' };
    let body = '';
    try { body = (f.contentDocument && f.contentDocument.body && f.contentDocument.body.innerText || '').slice(0, 300); } catch (e) { body = 'cross-origin: ' + e.message; }
    return { src: f.src, body: body };
  });
  console.log('[3] iframe src:', info.src);
  console.log('[4] iframe 文本前 300:', JSON.stringify(info.body));
  console.log('[5] 是否出现嵌套循环提示:', /嵌套循环/.test(info.body || '') ? 'YES' : 'no');
  console.log('[6] 控制台错误数:', errs.length, errs.slice(0, 5));
  console.log('[7] 失败请求:', bad.length);
  bad.slice(0, 12).forEach(b => console.log('    ', b));

  await page.screenshot({ path: 'tests/mobile_map.png', fullPage: false });
  await browser.close();
})().catch(e => { console.error('FAIL', e); process.exit(1); });
