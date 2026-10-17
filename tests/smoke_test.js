// X-Box 前端冒烟测试：DOM 桩 + 边界数据，验证渲染函数不抛异常
const fs = require('fs');
const path = require('path');

function makeEl(id) {
  return {
    id, innerHTML: '', textContent: '', value: '', checked: false, disabled: false,
    style: {}, dataset: {},
    classList: { add() {}, remove() {}, contains() { return false; } },
    addEventListener() {}, removeEventListener() {},
    querySelector() { return makeEl('sub'); },
    querySelectorAll() { return []; },
    setAttribute() {}, getAttribute() { return null; },
    appendChild() {}, remove() {},
  };
}
const elements = {};
global.document = {
  getElementById(id) { return elements[id] || (elements[id] = makeEl(id)); },
  querySelectorAll() { return []; },
  querySelector() { return makeEl('q'); },
  createElement() { return makeEl('new'); },
  addEventListener() {},
};
global.window = global;
const realSetTimeout = global.setTimeout;
global.setInterval = () => 0;
global.setTimeout = (f) => { try { f && f(); } catch (e) {} return 0; }; // toast 自动移除用立即执行

// fetch 桩：overview / nat / ports / processes / services 全覆盖（r.ok 供 api() 的状态检查）
// 隧道解码：页面 api() 走 /?__x=<base64url(JSON{m,p,b,sid})>，需还原真实路径再路由
function b64d(s){ s=String(s).replace(/-/g,'+').replace(/_/g,'/'); while(s.length%4) s+='='; return decodeURIComponent(escape(atob(s))); }
global.fetch = async (url) => {
  let u = String(url);
  const qi = u.indexOf('?__x=');
  if (qi >= 0) {
    try { const tp = JSON.parse(b64d(decodeURIComponent(u.slice(qi + 5).split('&')[0]))); if (tp.p) u = tp.p; } catch(e){}
  }
  return {
  ok: true, status: 200,
  json: async () => {
    if (u.includes('/api/auth/status')) return { ok: true, configured: true, authed: true, user: 'admin' }; // 已登录，boot 直通 enterApp
    if (u.includes('/api/auth/listusers')) return { ok: true, users: [{user:'admin', current:true}] };
    if (u.includes('/api/overview')) return {
      ok: true, version: '1.1.0', hostname: 'nas', cpuCores: 4, cpuModel: 'Test CPU',
      load1: 0.5, memTotalKB: 8000000, memAvailKB: 4000000, uptimeSeconds: 90000,
      kernel: '6.1.0', listeningPorts: 5, publicPorts: 1,
      disks: [
        {mount: '/', device: '/dev/sda1', fsType: 'ext4', totalKB: 100000000, availKB: 40000000},
        {mount: '/vol1', device: '/dev/nvme0n1p1', fsType: 'ext4', totalKB: 200000000, availKB: 100000000},
      ],
      temps: {
        cpu: [{label: 'Package id 0', tempC: 52.5}, {label: 'Core 0', tempC: 50}],
        cpuMax: 52.5,
        disks: [
          {device: 'sda', model: 'ST4000VN008', mount: '/', tempC: 38, source: 'smartctl'},
          {device: 'nvme0n1', model: 'Samsung 980', mount: '/vol1', tempC: 46.5, source: 'hwmon'},
        ],
      },
      net: {
        ifaces: [
          {name: 'eth0', up: true, ipv4: ['192.168.1.2'], ipv6: ['fe80::1'], mac: 'AA:BB:CC:DD:EE:FF', isDefault: true, rxRate: 1536000, txRate: 204800},
          {name: 'docker0', up: true, ipv4: ['172.17.0.1'], ipv6: [], mac: '02:42:11:22:33:44', isDefault: false, rxRate: 0, txRate: 0},
          {name: 'eth1', up: false, ipv4: [], ipv6: [], mac: '', isDefault: false, rxRate: 0, txRate: 0},
        ],
        gateway: '192.168.1.1', gateway6: 'fe80::1', dns: ['192.168.1.1', '223.5.5.5'],
      },
    };
    if (u.includes('/api/nat')) return { ok: true, nat: { type: 'symmetric', natLevel: 3, natLevelText: 'NAT3 · 严格（对称型）', optimize: '对称型 NAT 下 P2P 打洞困难：建议改用中继方案', publicIP: '1.2.3.4', publicPort: 12345, cgnat: false, p2pNote: '需中继', detail: 'x', testedAt: 'now' } };
    if (u.includes('/api/temps')) return { ok: true, cpuMax: 52.5, disks: [{device:'sda', mount:'/', tempC:38}, {device:'nvme0n1', mount:'/vol1', tempC:46.5}] };
    if (u.includes('/api/ports')) return { ok: true, ports: [] }; // listeners/public 缺失
    if (u.includes('/api/processes')) return { ok: true, processes: [] };
    if (u.includes('/api/services')) return { ok: true, services: null, apps: null, docker: null };
    return { ok: true };
  },
  };
}

// HTML 指向 ../src/web/index.html
const html = fs.readFileSync(path.join(__dirname, '..', 'src', 'web', 'index.html'), 'utf-8');
const m = html.match(/<script>([\s\S]*?)<\/script>/); // 非贪婪：只取第一个 script 块（主脚本），避免把 ES5 登录层和中间 HTML 也吃进来
if (!m) { console.error('FAIL: no script block'); process.exit(1); }
// startTempLoop 是递归定时器，本文件的 setTimeout 桩是立即执行，会让 tick 无限循环卡死进程：
// 删掉所有"调用"（带分号；函数定义 startTempLoop(){ 不含分号，不会被误伤）
m[1] = m[1].replace(/startTempLoop\(\);/g, '');

const test = `
;(async () => {
  await new Promise(r => realSetTimeout(r, 80)); // 等 boot() 的异步链跑完

  // 1) 仪表盘：网络详情表 + NAT 等级 + 优化建议 + 温度
  renderNet({ifaces: []});
  const netHTML = renderNet({ifaces: [
    {name:'eth0', up:true,  ipv4:['192.168.1.2'], ipv6:['fe80::1'], mac:'AA:BB:CC:DD:EE:FF', isDefault:true,  rxRate:1000, txRate:0},
    {name:'docker0', up:true, ipv4:['172.17.0.1'], ipv6:[], mac:'02:42:11:22:33:44', isDefault:false, rxRate:0, txRate:0},
    {name:'vethabc', up:true, ipv4:['10.0.0.2'], ipv6:[], isDefault:false, rxRate:0, txRate:0},
    {name:'eth1', up:false, ipv4:[], ipv6:[], isDefault:false, rxRate:0, txRate:0},
  ], gateway:'192.168.1.1', gateway6:'fe80::1', dns:['192.168.1.1','223.5.5.5']});
  console.assert(netHTML.includes('192.168.1.2') && netHTML.includes('fe80::1') && netHTML.includes('AA:BB:CC:DD:EE:FF'), 'FAIL: 网络详情表缺字段');
  console.assert(!netHTML.includes('docker0') && !netHTML.includes('veth') && !netHTML.includes('>eth1<'), 'FAIL: 虚拟/宕机网口未过滤');
  console.assert(netHTML.includes('192.168.1.1') && netHTML.includes('223.5.5.5'), 'FAIL: 网关/DNS 缺失');
  await runNAT(false);
  const natHTML = document.getElementById('nat-result').innerHTML;
  console.assert(natHTML.includes('NAT3') && natHTML.includes('优化建议') && natHTML.includes('中继'), 'FAIL: NAT 等级或优化建议缺失');
  // boot 已跑过 loadOverview：校验 CPU 温度与硬盘温度渲染
  const statsHTML = document.getElementById('ov-stats').innerHTML;
  console.assert(/🌡️ [\\d.]+°C/.test(statsHTML), 'FAIL: CPU 温度未渲染');
  const disksHTML = document.getElementById('ov-disks').innerHTML;
  console.assert(!disksHTML.includes('°C'), 'FAIL: 磁盘行内不应再有温度（已统一到汇总行）');

  // 2) 垃圾清理：空目录分类 + fileList 为 null + 有文件分类
  cleanData = {
    categories: [
      {id:'apt-cache', name:'APT 缓存', desc:'d', safe:true, minAgeDays:0, paths:[], files:0, bytes:0, fileList:null},
      {id:'tmp', name:'临时文件', desc:'d', safe:true, minAgeDays:7, paths:['/tmp'], files:2, bytes:2048,
       fileList:[{path:'/tmp/a', size:1024, ageDays:9}, {path:'/tmp/b', size:1024, ageDays:3}]},
    ],
    totalBytes: 2048, totalFiles: 2, scannedAt: '2026-10-16 12:00:00', durationMs: 12,
  };
  renderClean();
  renderClean(); // 第二次调用：clean-empty 已被首次挂载移除，回归 null.style 崩溃场景
  // renderClean 已改 DOM API 挂载（桩内 innerHTML 不回填），直接断言单卡模板输出
  const cardHTML = catCardHTML(cleanData.categories[1]);
  console.assert((cardHTML.match(/class="file-line"/g)||[]).length === 2, 'FAIL: 文件行未渲染');
  const emptyCard = catCardHTML(cleanData.categories[0]);
  console.assert(emptyCard.includes('本机无相关目录'), 'FAIL: 空目录徽标缺失');
  const subTxt = document.getElementById('clean-sub').textContent;
  console.assert(/已加载 \\d+\\/2 类/.test(subTxt), 'FAIL: 渲染计数缺失: '+subTxt);
  updateSelInfo('tmp');

  // 3) 进程表：字段残缺 + 僵尸进程 + 高 CPU
  procsData = [
    {pid: 1, name: 'init', user: 'root', state: 'S', cpuPercent: 0, memKB: 100, memPercent: 0.1, startedSeconds: 90000},
    {pid: 42, name: 'miner', user: 'www', state: 'R', cpuPercent: 187.5, memKB: 204800, memPercent: 2.5, startedSeconds: 3600},
    {pid: 77, name: 'zomb', user: 'root', state: 'Z'},
    {pid: 88},
  ];
  renderProcs();

  // 4) DDNS：providers/records 为 null（复现 length 崩溃场景）
  ddnsData = { intervalSec: 300, providers: null, records: null, providersAvail: [] };
  renderDDNSProviders(); renderDDNSRecords();

  // 5) 应用列表：pids 为 null
  appsData = [{name:'app1', path:'/vol1/@appdata/app1', running:true, pids:null}];
  renderApps();

  // 6) baseDevName 分区后缀剥离
  console.assert(baseDevName('/dev/sda1')==='sda' && baseDevName('/dev/nvme0n1p3')==='nvme0n1' && baseDevName('/dev/mmcblk0p1')==='mmcblk0', 'FAIL: baseDevName');

  // 7) 温度渲染：存储卡底部汇总行（renderDiskTemps）
  document.getElementById('ov-disktemps').innerHTML = renderDiskTemps({cpuMax: 52.5, disks: [{device:'sda', mount:'/', tempC:38}, {device:'nvme0n1', tempC:46.5}]});
  const dtAll = document.getElementById('ov-disktemps').innerHTML;
  console.assert(dtAll.includes('sda') && dtAll.includes('38.0°C') && dtAll.includes('nvme0n1'), 'FAIL: 硬盘温度汇总行缺失');

  console.log('SMOKE OK: 全部渲染函数在边界数据下无异常');
  process.exit(0); // 显式退出，防止遗漏的定时器（如温度轮询）让进程挂着不退出
})().catch(e => { console.error('SMOKE FAIL:', e); process.exit(1); });
`;

try {
  eval(m[1] + test);
} catch (e) {
  console.error('SMOKE FAIL (load):', e);
  process.exit(1);
}
