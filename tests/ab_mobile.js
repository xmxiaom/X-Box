// A/B 对比：手机端访问百度，记录 502 与失败请求（网关模式）
// 用法：node tests/ab_mobile.js <label>
const { chromium } = require('playwright');
const label = process.argv[2] || 'run';
const base = process.env.XBOX_URL || 'http://127.0.0.1:8899/app/X-Box/';

(async () => {
  const browser = await chromium.launch({ channel: 'msedge' });
  const ctx = await browser.newContext({
    viewport: { width: 390, height: 844 }, isMobile: true, hasTouch: true,
    userAgent: 'Mozilla/5.0 (iPhone; CPU iPhone OS 17_0 like Mac OS X) AppleWebKit/605.1.15 (KHTML, like Gecko) Version/17.0 Mobile/15E148 Safari/604.1',
  });
  const page = await ctx.newPage();
  const bad = [];
  page.on('response', r => { if (r.status() >= 400) bad.push(r.status() + ' ' + r.url().slice(0, 200)); });

  await page.goto(base, { waitUntil: 'networkidle', timeout: 25000 });
  await page.evaluate(() => { const m = document.querySelector('.menu-item[data-page="browser"]'); if (m) m.click(); });
  await page.waitForTimeout(300);
  await page.fill('#browser-url', 'https://www.baidu.com/');
  await page.evaluate(() => goBrowser());
  await page.waitForTimeout(9000);

  const info = await page.evaluate(() => {
    const f = [...document.querySelectorAll('.browser-frame-item')].find(x => x.classList.contains('on'));
    if (!f) return {};
    let t = '';
    try { t = (f.contentDocument.body.innerText || '').slice(0, 200); } catch (e) { t = 'ERR ' + e.message; }
    return { src: f.src, txt: t };
  });
  const is502 = /无法访问目标地址|502/.test(info.txt || '');
  console.log('[' + label + '] src:', info.src);
  console.log('[' + label + '] 502?', is502 ? 'YES' : 'no');
  console.log('[' + label + '] 文本:', JSON.stringify((info.txt || '').slice(0, 120)));
  console.log('[' + label + '] 失败请求:', bad.length);
  bad.slice(0, 10).forEach(b => console.log('   ', b));
  await page.screenshot({ path: 'tests/ab_' + label + '.png' });
  await browser.close();
})().catch(e => { console.error('FAIL', e.message); process.exit(1); });
