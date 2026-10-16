// X-Box 手机端布局验证：Node 模拟后端 + 数据，供 Edge 无头截图
// 仅用于本地视觉验证，不参与 fpk 打包。
const http = require('http');
const fs = require('fs');
const path = require('path');

// HTML 指向 ../src/web/index.html
const HTML = fs.readFileSync(path.join(__dirname, '..', 'src', 'web', 'index.html'), 'utf-8');

const MOCK = {
  '/api/overview': {
    ok: true, version: '1.1.0', hostname: 'fn-nas', platform: 'linux/amd64', goVersion: 'go1.22',
    kernel: '6.12.4-fn-os', os: 'fnOS 1.2.3', cpuModel: 'Intel(R) N100', cpuCores: 4,
    uptimeSeconds: 987654, load1: 0.42, load5: 0.38, load15: 0.35,
    memTotalKB: 16384000, memAvailKB: 11534336, listeningPorts: 12, publicPorts: 2,
    disks: [
      { mount: '/', device: '/dev/nvme0n1p2', fsType: 'ext4', totalKB: 117220512, freeKB: 23488102, availKB: 22339456 },
      { mount: '/vol1', device: '/dev/sda1', fsType: 'ext4', totalKB: 3907018584, freeKB: 1234567890, availKB: 1200000000 },
    ],
    temps: {
      cpu: [{ label: 'Package id 0', tempC: 47 }, { label: 'Core 0', tempC: 44 }, { label: 'Core 1', tempC: 42 }],
      cpuMax: 47,
      disks: [
        { device: 'nvme0n1', model: 'Samsung SSD 980 500GB', mount: '/', tempC: 41, source: 'hwmon' },
        { device: 'sda', model: 'ST4000VN008-2DR166', mount: '/vol1', tempC: 36, source: 'smartctl' },
      ],
    },
    net: {
      ifaces: [
        { name: 'eth0', mac: 'AA:BB:CC:11:22:33', mtu: 1500, up: true, ipv4: ['192.168.31.166'], ipv6: ['2408:8207::1a2b'], rxBytes: 9876543210, txBytes: 1234567890, rxRate: 1536000, txRate: 204800, isDefault: true, isWan: true },
        { name: 'docker0', mac: '02:42:11:22:33:44', mtu: 1500, up: true, ipv4: ['172.17.0.1'], ipv6: [], rxBytes: 0, txBytes: 0, rxRate: 0, txRate: 0, isDefault: false, isWan: false },
        { name: 'eth1', mac: '', mtu: 1500, up: false, ipv4: [], ipv6: [], rxBytes: 0, txBytes: 0, rxRate: 0, txRate: 0, isDefault: false, isWan: false },
      ],
      gateway: '192.168.31.1', gateway6: 'fe80::1', dns: ['192.168.31.1', '223.5.5.5'],
    },
  },
  '/api/nat': {
    ok: true, nat: {
      type: 'portrestricted', typeText: '端口受限圆锥型（Port Restricted Cone）NAT',
      natLevel: 2, natLevelText: 'NAT2 · 中度开放（端口受限圆锥）',
      optimize: '端口受限圆锥：大部分 P2P 应用可用。对需要入连的服务建议做固定端口映射（路由器端口转发，或将本机设为 DMZ 主机），并优先使用飞牛自带 FN Connect 中继作为兜底通道。',
      publicIP: '113.66.23.108', publicPort: 54321, cgnat: false,
      p2pNote: 'P2P 打洞需双方同时发包（生日悖论），成功率中等',
      detail: '映射地址 113.66.23.108:54321 仅对本机曾通信过的 外部IP+端口 放行',
      testedAt: '2026-10-16 13:30:00',
    },
  },
  '/api/temps': {
    ok: true,
    cpu: [{ label: 'Package id 0', tempC: 48 }, { label: 'Core 0', tempC: 45 }],
    cpuMax: 48,
    disks: [
      { device: 'nvme0n1', model: 'Samsung SSD 980 500GB', mount: '/', tempC: 42, source: 'hwmon' },
      { device: 'sda', model: 'ST4000VN008-2DR166', mount: '/vol1', tempC: 37, source: 'smartctl' },
    ],
  },
  '/api/ports': {
    ok: true, listeners: 12, public: 2,
    ports: [
      { proto: 'tcp', localAddr: '0.0.0.0', port: 8899, state: 'LISTEN', pid: 1024, proc: 'xbox-amd64', user: 'root', public: true },
      { proto: 'tcp', localAddr: '0.0.0.0', port: 22, state: 'LISTEN', pid: 800, proc: 'sshd', user: 'root', public: true },
      { proto: 'tcp', localAddr: '127.0.0.1', port: 8000, state: 'LISTEN', pid: 512, proc: 'trim-web', user: 'trim', public: false },
      { proto: 'udp', localAddr: '0.0.0.0', port: 5353, state: '-', pid: 0, proc: 'avahi', user: 'avahi', public: false },
    ],
  },
  '/api/processes': {
    ok: true, memTotalKB: 16384000, processCount: 4,
    processes: [
      { pid: 1, name: 'systemd', user: 'root', state: 'S', cpuPercent: 0.1, memKB: 18432, memPercent: 0.11, threads: 1, cmdline: '/sbin/init splash', startedSeconds: 987654 },
      { pid: 1024, name: 'xbox-amd64', user: 'root', state: 'S', cpuPercent: 1.8, memKB: 24576, memPercent: 0.15, threads: 12, cmdline: '/vol1/@appcenter/X-Box/server/xbox-amd64', startedSeconds: 3600 },
      { pid: 2048, name: 'trim-photos', user: 'trim', state: 'R', cpuPercent: 63.5, memKB: 512000, memPercent: 3.12, threads: 24, cmdline: '/usr/local/apps/trim.photos/bin/trim-photos --scan', startedSeconds: 7200 },
      { pid: 77, name: 'kworker/u16:2', user: 'root', state: 'I', cpuPercent: 0, memKB: 0, memPercent: 0, threads: 1, cmdline: '', startedSeconds: 987000 },
    ],
  },
  '/api/services': {
    ok: true, hasDocker: true,
    services: [
      { unit: 'nginx.service', load: 'loaded', active: 'active', sub: 'running', desc: 'A high performance web server' },
      { unit: 'docker.service', load: 'loaded', active: 'active', sub: 'running', desc: 'Docker Application Container Engine' },
      { unit: 'bluetooth.service', load: 'loaded', active: 'inactive', sub: 'dead', desc: 'Bluetooth service' },
    ],
    apps: [
      { name: 'trim.photos', path: '/usr/local/apps/@appdata/trim.photos', running: true, pids: [2048] },
      { name: 'trim.media', path: '/usr/local/apps/@appdata/trim.media', running: false, pids: null },
    ],
    docker: [
      { names: 'jellyfin', image: 'jellyfin/jellyfin:latest', status: 'Up 3 days', ports: '0.0.0.0:8096->8096/tcp' },
    ],
  },
  '/api/autostart': {
    ok: true,
    items: [
      { name: 'docker.service', type: 'systemd-service', state: 'enabled', source: 'multi-user.target.wants', desc: 'Docker 容器引擎', running: true },
      { name: 'trim-sync.timer', type: 'systemd-timer', state: 'enabled', source: 'timers.target.wants', desc: '同步服务定时器', running: true },
      { name: 'rc.local', type: 'rc-local', state: 'n/a', source: '/etc/rc.local', desc: '本地启动脚本', running: true },
      { name: 'trim-backup', type: 'cron', state: 'enabled', source: '/etc/cron.d/trim-backup', desc: '每日备份任务', running: false },
    ],
  },
  '/api/ddns': {
    ok: true, intervalSec: 300,
    providersAvail: [
      { type: 'aliyun', name: '阿里云', needSecret: true },
      { type: 'tencentcloud', name: '腾讯云', needSecret: true },
      { type: 'cloudflare', name: 'Cloudflare', needSecret: true },
      { type: 'duckdns', name: 'DuckDNS', needSecret: false },
    ],
    providers: [
      { id: 1, type: 'aliyun', name: '我的阿里云', accessKeyId: 'LTAI5t***', accessKeySecret: 'secret' },
    ],
    records: [
      { id: 1, enabled: true, providerId: 1, domain: 'nas.example.com', recordType: 'A', ipSource: 'web', ipSourceArg: '', ttl: 600, autoCreated: false, lastIP: '113.66.23.108', lastStatus: 'success', lastMessage: 'OK', lastCheckAt: '2026-10-16T13:20:00Z', lastSyncAt: '2026-10-16T13:25:00Z' },
    ],
  },
  '/api/proxy': {
    ok: true,
    sites: [
      { id: 1, hosts: ['nas.example.com', '*.example.com'], backend: 'http://192.168.31.166:8080', pathPrefix: '/jellyfin', stripPrefix: true, enabled: true, autoDDNS: true, description: '影视服务' },
      { id: 2, hosts: ['files.example.com'], backend: 'http://192.168.31.166:9000', pathPrefix: '', stripPrefix: false, enabled: false, autoDDNS: false, description: '文件管理' },
    ],
  },
  '/api/nvme-sleep': {
    ok: true,
    disks: [
      { name: 'nvme0', model: 'SAMSUNG MZ9L31T9HBKL-00007', sizeText: '931.5G', pciAddr: '0000:01:00.0', state: 'live', down: false, d3coldOk: '1', deepBanned: false, mount: '/vol1' },
      { name: 'nvme1', model: 'XXX NVMe SSD 238.5GB', sizeText: '238.5G', pciAddr: '0000:02:00.0', state: 'dead', down: true, d3coldOk: '1', deepBanned: false, mount: '/vol3' },
      { name: 'nvme2', model: 'XXX NVMe SSD 119.2GB', sizeText: '119.2G', pciAddr: '0000:07:00.0', state: 'live', down: false, d3coldOk: '0', deepBanned: true, mount: '/' },
    ],
  },
  '/api/update/check': {
    ok: true,
    update: {
      current: '1.2.7', latest: '1.2.8', hasUpdate: true,
      notes: '1. 新增「系统设置」检查更新\n2. 修复若干问题',
      assetName: 'X-Box.fpk', downloadUrl: 'https://github.com/xmxiaom/X-Box/releases/download/v1.2.8/X-Box.fpk',
      publishedAt: '2026-10-17 00:10:00', checkedAt: '2026-10-17 00:11:00',
    },
  },
  '/api/scan': {
    ok: true, durationMs: 328, scannedAt: '2026-10-16 13:30:12', totalBytes: 61244064, totalFiles: 491,
    categories: [
      { id: 'apt-cache', name: 'APT 软件包缓存', desc: '已下载的 .deb 安装包缓存，删除后需要时可重新下载', paths: ['/var/cache/apt/archives'], safe: true, minAgeDays: 0, available: true, files: 12, bytes: 31457280, oldestDays: 45,
        fileList: [
          { path: '/var/cache/apt/archives/libc6_2.36-9+deb12u8_amd64.deb', size: 2818048, ageDays: 45 },
          { path: '/var/cache/apt/archives/nginx_1.22.1-9_amd64.deb', size: 921600, ageDays: 30 },
        ] },
      { id: 'log-rotated', name: '轮转旧日志', desc: '/var/log 下已轮转的旧日志，当前活跃日志不受影响', paths: ['/var/log'], safe: true, minAgeDays: 14, available: true, files: 89, bytes: 22020096, oldestDays: 120, fileList: [] },
      { id: 'journal', name: 'systemd 日志归档', desc: 'journald 二进制日志，保留最近 500MB', paths: ['/var/log/journal'], safe: true, minAgeDays: 3, available: true, files: 6, bytes: 6291456, oldestDays: 60, fileList: [] },
      { id: 'tmp', name: '系统临时文件', desc: '/tmp 与 /var/tmp 中超过 7 天未修改的普通文件', paths: ['/tmp', '/var/tmp'], safe: true, minAgeDays: 7, available: true, files: 384, bytes: 1468006, oldestDays: 30, fileList: [] },
      { id: 'appdata-logs', name: '应用旧日志', desc: '应用数据目录中超过 30 天的旧日志', paths: ['/usr/local/apps/@appdata/trim.photos/logs', '/vol1/@appdata/X-Box/logs'], safe: true, minAgeDays: 30, available: true, files: 3, bytes: 204800, oldestDays: 40, fileList: [] },
      { id: 'appdata-tmp', name: '应用临时文件', desc: '应用临时目录中超过 7 天的临时文件', paths: ['/vol1/@apptemp/trim.photos'], safe: true, minAgeDays: 7, available: true, files: 21, bytes: 512000, oldestDays: 12, fileList: [] },
      { id: 'thumbnails', name: '缩略图缓存', desc: '相册/媒体缩略图缓存，删除后会自动重新生成', paths: ['/usr/local/apps/@appdata/trim.photos/thumbnails'], safe: false, minAgeDays: 7, available: true, files: 156, bytes: 3145728, oldestDays: 90, fileList: [] },
      { id: 'crash', name: '崩溃转储', desc: '崩溃文件，排查完问题即可清理', paths: [], safe: true, minAgeDays: 1, available: false, files: 0, bytes: 0, oldestDays: 0, fileList: null },
    ],
  },
};

const server = http.createServer((req, res) => {
  const u = req.url.split('?')[0];
  if (u === '/') {
    const q = req.url.split('?')[1] || '';
    const shot = q.includes('shot=') ? q.split('shot=')[1].split('&')[0] : '';
    const diag = q.includes('diag=1');
    let html = HTML;
    if (shot) {
      html = html.replace('</body>', `<script>setTimeout(()=>{const el=document.querySelector('[data-page="${shot}"]');if(el)el.click();${shot === 'clean' ? "setTimeout(()=>startScan(),300);" : ''}},150)</script></body>`);
    }
    if (diag) {
      html = html.replace('</body>', `<script>setTimeout(()=>{
        const r = el => { if(!el) return null; const b = el.getBoundingClientRect(); return {x:Math.round(b.x),y:Math.round(b.y),w:Math.round(b.width),h:Math.round(b.height)}; };
        const cs = (el, props) => { if(!el) return null; const s = getComputedStyle(el); const o = {}; props.forEach(p => o[p] = s[p]); return o; };
        const ct = document.querySelector('.page.active .cat-toolbar');
        const row = document.querySelector('.page.active .row');
        const rows = document.querySelector('.page.active .rows');
        const kids = row ? [...row.children].map(el => { const b = el.getBoundingClientRect(); return {cls: el.className, x: Math.round(b.x), w: Math.round(b.width), h: Math.round(b.height), sw: el.scrollWidth, cw: el.clientWidth }; }) : [];
        const ctKids = ct ? [...ct.children].map(el => { const b = el.getBoundingClientRect(); return {tag: el.tagName, cls: el.className, txt: (el.textContent||'').slice(0,10), x: Math.round(b.x), w: Math.round(b.width) }; }) : [];
        const hdr = row ? row.children[0] : null;
        const hdrKids = hdr ? [...hdr.children].map(el => { const b = el.getBoundingClientRect(); const s = getComputedStyle(el); return {cls: el.className, w: Math.round(b.width), sw: el.scrollWidth, flex: s.flex, minW: s.minWidth}; }) : [];
        const chain = [];
        let p = row ? row.parentElement : null;
        while (p && chain.length < 8) {
          chain.push({tag: p.tagName, cls: (p.className||'').toString().slice(0,30), w: p.getBoundingClientRect().width|0, sw: p.scrollWidth, cw: p.clientWidth, ovf: getComputedStyle(p).overflowX});
          p = p.parentElement;
        }
        // 找出 .content 内所有撑宽元素（深度优先，限制数量）
        const wide = [];
        const root = document.querySelector('.content');
        if (root) {
          const threshold = innerWidth * 0.98;
          const walk = (el, depth) => {
            if (depth > 6 || !el.getBoundingClientRect) return;
            const sw = el.scrollWidth;
            if (sw > threshold) {
              const b = el.getBoundingClientRect();
              const s = getComputedStyle(el);
              wide.push({tag: el.tagName, cls: (el.className||'').toString().slice(0,40), sw: sw, w: Math.round(b.width), d: depth, disp: s.display, minW: s.minWidth, maxW: s.maxWidth});
              if (wide.length >= 12) return;
            }
            [...el.children].forEach(c => walk(c, depth+1));
          };
          walk(root, 0);
        }
        const out = {
          vw: innerWidth,
          rowKids: kids,
          ctKids: ctKids,
          hdrKids: hdrKids,
          rowCS: row ? cs(row, ['minWidth','width','flexBasis','alignSelf','flex','display','alignItems','boxSizing']) : null,
          rowsCS: rows ? cs(rows, ['display','width','alignItems','flexDirection']) : null,
          hdrCS: hdr ? cs(hdr, ['minWidth','width','flexWrap','display','alignSelf']) : null,
          contentSW: root ? root.scrollWidth : null,
          wide: wide.slice(0, 12),
        };
        const d = document.createElement('div'); d.id='DIAG'; d.textContent = JSON.stringify(out);
        document.body.appendChild(d);
      }, 2500)</script></body>`);
    }
    res.writeHead(200, { 'Content-Type': 'text/html; charset=utf-8' });
    res.end(html);
    return;
  }
  const mock = MOCK[u];
  if (mock && req.method === 'GET') {
    res.writeHead(200, { 'Content-Type': 'application/json; charset=utf-8' });
    res.end(JSON.stringify(mock));
    return;
  }
  if (req.method === 'POST') {
    res.writeHead(200, { 'Content-Type': 'application/json; charset=utf-8' });
    res.end(JSON.stringify({ ok: true, message: '模拟成功', freedBytes: 1024, files: 2, freedText: '1.00 KB', failures: [] }));
    return;
  }
  res.writeHead(404); res.end('{}');
});
server.listen(8899, '127.0.0.1', () => console.log('mock server on 8899'));
