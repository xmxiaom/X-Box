// 复现「打开百度 → 点击地图」链路（网关模式）
const { chromium } = require('playwright');

(async () => {
  const base = process.env.XBOX_URL || 'http://127.0.0.1:8899/app/X-Box/';
  const browser = await chromium.launch({ channel: 'msedge' });
  const ctx = await browser.newContext({
    viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true,
    userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1',
  });
  const page = await ctx.newPage();
  const bad = [];
  page.on('response', r => { if (r.status() >= 400) bad.push(r.status() + ' ' + r.url().slice(0, 260)); });

  await page.goto(base, { waitUntil: 'networkidle', timeout: 25000 });
  await page.evaluate(() => { const m = document.querySelector('.menu-item[data-page="browser"]'); if (m) m.click(); });
  await page.waitForTimeout(300);
  await page.fill('#browser-url', 'https://www.baidu.com/');
  await page.evaluate(() => goBrowser());
  await page.waitForTimeout(6000);

  // 在 iframe 内找「地图」链接并点击
  const clicked = await page.evaluate(() => {
    const f = [...document.querySelectorAll('.browser-frame-item')].find(x => x.classList.contains('on'));
    if (!f || !f.contentDocument) return 'no frame';
    const doc = f.contentDocument;
    const as = [...doc.querySelectorAll('a')];
    const target = as.find(a => (a.textContent || '').trim() === '地图') ||
                   as.find(a => /map\.baidu\.com/.test(a.getAttribute('href') || ''));
    if (!target) return 'no link; links=' + as.length;
    const href = target.getAttribute('href');
    target.click();
    return 'clicked href=' + href;
  });
  console.log('[1]', clicked);
  await page.waitForTimeout(8000);

  const info = await page.evaluate(() => {
    const f = [...document.querySelectorAll('.browser-frame-item')].find(x => x.classList.contains('on'));
    if (!f) return { err: 'no frame' };
    let txt = '';
    try { txt = (f.contentDocument.body.innerText || '').slice(0, 400); } catch (e) { txt = 'ERR ' + e.message; }
    return { src: f.src, txt };
  });
  console.log('[2] iframe src:', info.src);
  console.log('[3] 文本:', JSON.stringify((info.txt || '').slice(0, 200)));
  console.log('[4] 嵌套循环:', /嵌套循环/.test(info.txt || '') ? 'YES' : 'no');
  console.log('[5] 失败请求:', bad.length);
  bad.slice(0, 15).forEach(b => console.log('   ', b));
  await page.screenshot({ path: 'tests/gw_baidu_click.png' });
  await browser.close();
})().catch(e => { console.error('FAIL', e); process.exit(1); });
