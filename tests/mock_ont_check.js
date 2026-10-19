// 通过代理访问 ZTE 风格 mock（带会话校验），验证三级菜单能否切换
const { chromium } = require('playwright');
const PROXY = process.env.XBOX_PROXY || 'http://127.0.0.1:8899/app/X-Box/b/http/127.0.0.1:8897/unicom.html';

(async () => {
  const browser = await chromium.launch({ channel: 'msedge' });
  const ctx = await browser.newContext({ viewport: { width: 1200, height: 820 } });
  const page = await ctx.newPage();
  const bad = [];
  page.on('response', r => { if (r.status() >= 400) bad.push(r.status() + ' ' + r.url().slice(0, 170)); });

  const mainFrame = () => page.frames().find(f => f.url().includes('getpage.gch'));
  const curText = async () => {
    const f = mainFrame();
    if (!f) return '(no main frame)';
    try { return await f.$eval('#cur', e => e.textContent.trim()); } catch (e) { return '(no #cur) ' + f.url().slice(-40); }
  };

  await page.goto(PROXY, { waitUntil: 'load', timeout: 20000 });
  await page.waitForTimeout(2500);
  console.log('[0] 初始:', await curText());

  for (const [id, desc] of [['t1', '顶部菜单→设备信息'], ['t2', '顶部菜单→统计数据']]) {
    const top = page.frames().find(f => f.url().includes('top.gch'));
    if (!top) { console.log(`[${id}] 顶部框架缺失`); continue; }
    try { await top.click('#' + id, { timeout: 3000 }); } catch (e) { console.log(`[${id}] 点击失败`, e.message.slice(0, 50)); }
    await page.waitForTimeout(2000);
    console.log(`[${id}] ${desc} →`, await curText());
  }

  for (const [id, desc] of [['m3', '三级→WAN'], ['m4', '三级→语音'], ['m5', '三级→LAN']]) {
    const f = mainFrame();
    if (!f) { console.log(`[${id}] 主框架缺失`); continue; }
    const before = await curText();
    try { await f.click('#' + id, { timeout: 3000 }); } catch (e) { console.log(`[${id}] 点击失败`, e.message.slice(0, 50)); }
    await page.waitForTimeout(2200);
    const after = await curText();
    console.log(`[${id}] ${desc}: ${before} → ${after} ${before !== after ? '✓变化' : '✗未变'}`);
  }

  console.log('[9] 失败请求:', bad.length);
  bad.slice(0, 10).forEach(b => console.log('    ', b));
  await page.screenshot({ path: 'tests/mock_ont_result.png' });
  await browser.close();
})().catch(e => { console.error('FAIL', e.message); process.exit(1); });
