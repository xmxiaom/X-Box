// 真实层次复现：面板 → iframe(代理页) → topFrame/mainFrame
// 验证管理页的 top.mainFrame 跳转在代理下是否仍可用
const { chromium } = require('playwright');
const PANEL = process.env.XBOX_PANEL || 'http://127.0.0.1:8899/app/X-Box/';
const TARGET = process.env.XBOX_TARGET || 'http://127.0.0.1:8897/unicom.html';

(async () => {
  const browser = await chromium.launch({ channel: 'msedge' });
  const ctx = await browser.newContext({ viewport: { width: 1200, height: 820 } });
  const page = await ctx.newPage();
  const bad = [];
  page.on('response', r => { if (r.status() >= 400) bad.push(r.status() + ' ' + r.url().slice(0, 170)); });

  await page.goto(PANEL, { waitUntil: 'networkidle', timeout: 25000 });
  await page.evaluate(() => { const m = document.querySelector('.menu-item[data-page="browser"]'); if (m) m.click(); });
  await page.waitForTimeout(300);
  await page.fill('#browser-url', TARGET);
  await page.evaluate(() => goBrowser());
  await page.waitForTimeout(6000);

  const mainFrame = () => page.frames().find(f => f.url().includes('getpage.gch'));
  const curText = async () => {
    const f = mainFrame();
    if (!f) return '(no main frame)';
    try { return await f.$eval('#cur', e => e.textContent.trim()); } catch (e) { return '(no #cur) ' + f.url().slice(-40); }
  };
  const topFrame = () => page.frames().find(f => f.url().includes('top.gch'));

  console.log('[0] 层级:', page.frames().map(f => f.url().replace(/^https?:\/\/[^/]+/, '').slice(-48)));
  console.log('[0] 初始:', await curText());

  for (const [id, desc] of [['t2', '顶部菜单→统计数据'], ['t1', '顶部菜单→设备信息'], ['t2', '顶部菜单→统计数据(第2次)']]) {
    const tf = topFrame();
    if (!tf) { console.log(`[${id}] 顶部框架缺失`); continue; }
    const before = await curText();
    try { await tf.click('#' + id, { timeout: 3000 }); } catch (e) { console.log(`[${id}] 点击失败`, e.message.slice(0, 40)); }
    await page.waitForTimeout(2200);
    const after = await curText();
    console.log(`[${id}] ${desc}: ${before} → ${after} ${before !== after ? '✓' : '(未变)'}`);
  }

  for (const [id, desc] of [['m3', '三级→WAN'], ['m4', '三级→语音'], ['m5', '三级→LAN']]) {
    const f = mainFrame();
    if (!f) { console.log(`[${id}] 主框架缺失`); continue; }
    const before = await curText();
    try { await f.click('#' + id, { timeout: 3000 }); } catch (e) { console.log(`[${id}] 点击失败`, e.message.slice(0, 40)); }
    await page.waitForTimeout(2200);
    const after = await curText();
    console.log(`[${id}] ${desc}: ${before} → ${after} ${before !== after ? '✓变化' : '✗未变'}`);
  }

  console.log('[9] 失败请求:', bad.length);
  bad.slice(0, 10).forEach(b => console.log('    ', b));
  await page.screenshot({ path: 'tests/mock_real_result.png' });
  await browser.close();
})().catch(e => { console.error('FAIL', e.message); process.exit(1); });
