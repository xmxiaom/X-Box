// 网关模式复现：/app/X-Box 前缀下百度地图与光猫二级菜单
// 用法：XBOX_URL=http://127.0.0.1:8899/app/X-Box/ node tests/gw_check.js [site]
const { chromium } = require('playwright');

(async () => {
  const base = process.env.XBOX_URL || 'http://127.0.0.1:8899/app/X-Box/';
  const site = process.env.SITE || 'https://map.baidu.com/';
  const browser = await chromium.launch({ channel: 'msedge' });
  const ctx = await browser.newContext({
    viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true,
    userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1',
  });
  const page = await ctx.newPage();
  const bad = [];
  page.on('response', r => { if (r.status() >= 400) bad.push(r.status() + ' ' + r.url().slice(0, 220)); });
  page.on('pageerror', e => bad.push('JSERR ' + String(e).slice(0, 120)));

  await page.goto(base, { waitUntil: 'networkidle', timeout: 25000 });
  console.log('[1] 面板加载:', await page.title());
  await page.evaluate(() => { const m = document.querySelector('.menu-item[data-page="browser"]'); if (m) m.click(); });
  await page.waitForTimeout(300);
  await page.fill('#browser-url', site);
  await page.evaluate(() => goBrowser());
  await page.waitForTimeout(9000);

  const info = await page.evaluate(() => {
    const f = [...document.querySelectorAll('.browser-frame-item')].find(x => x.classList.contains('on'));
    if (!f) return { err: 'no frame' };
    let txt = '';
    try { txt = (f.contentDocument.body.innerText || '').slice(0, 400); } catch (e) { txt = 'ERR ' + e.message; }
    return { src: f.src, txt };
  });
  console.log('[2] iframe src:', info.src);
  console.log('[3] 页面文本:', JSON.stringify((info.txt || '').slice(0, 220)));
  console.log('[4] 嵌套循环提示:', /嵌套循环/.test(info.txt || '') ? 'YES' : 'no');
  console.log('[5] 失败/报错', bad.length);
  bad.slice(0, 15).forEach(b => console.log('   ', b));
  await page.screenshot({ path: 'tests/gw_map.png' });
  await browser.close();
})().catch(e => { console.error('FAIL', e); process.exit(1); });
