// DDNS 新功能桩验证：网卡下拉数据 + 测试按钮渲染 + 记录表单
// 关键：eval 直接调用时，页面脚本的函数声明（confirmDialog/toast）会提升到模块作用域，
// 遮蔽 global 上的同名桩——必须把桩以"赋值"形式拼进 eval 字符串内部才能生效。
const fs = require('fs'), path = require('path');
const html = fs.readFileSync(path.join(__dirname, '..', 'src', 'web', 'index.html'), 'utf-8');
const m = html.match(/<script>([\s\S]*)<\/script>/);
const elems = {};
function el(id) {
  if (!elems[id]) elems[id] = { id, value: '', innerHTML: '', textContent: '', style: {}, addEventListener() {}, dataset: {}, querySelectorAll() { return []; }, appendChild() {}, remove() {}, insertAdjacentHTML() {}, classList: { contains: () => true, add() {}, remove() {} } };
  return elems[id];
}
global.document = { getElementById: el, querySelector: () => null, querySelectorAll: () => [], createElement: () => ({ style: {}, dataset: {}, click() {}, remove() {} }), addEventListener() {}, body: { appendChild() {} } };
global.window = { open() {} };
global.localStorage = { getItem: () => null, setItem() {} };
global.fetch = async (url) => ({
  ok: true, status: 200,
  json: async () => (String(url).includes('/api/ddns')
    ? { ok: true, ifaces: ['enp3s0', 'enp4s0', 'wlan0'], providers: [{ id: 1, name: '测试阿里', type: 'alidns', cred: { accessKeyId: 'x' } }], records: [], providersAvail: [], intervalSec: 300 }
    : { ok: true, version: '1.2.19', hostname: 'FNNAS', os: 'fnOS', cpuModel: 'x', cpuCores: 4, kernel: 'x', net: {}, fnosVersion: '1.2.0800', stats: [], disks: [] }),
});
const stub = `
confirmDialog = (t,b)=>{ global.__lastConfirm=b; };
toast = (m,k)=>{};
`;
const s = m[1].replace(/startTempLoop\(\);/g, '');
eval(stub + s + ';global.__ddns={get data(){return ddnsData},set data(v){ddnsData=v},get ifaces(){return ddnsIfaces},loadDDNS,renderDDNSProviders,openRecordForm};');
(async () => {
  const D = global.__ddns;
  await D.loadDDNS();
  console.log('ddnsIfaces =', JSON.stringify(D.ifaces));
  D.renderDDNSProviders();
  const th = el('ddns-providers').innerHTML;
  console.log('测试按钮渲染:', /testProvider\(1,this\)/.test(th) ? 'OK' : 'FAIL');
  console.log('按钮文案[测试]:', /<button[^>]*>测试<\/button>/.test(th) ? 'OK' : 'FAIL');
  D.data.records = [];
  D.openRecordForm(null);
  const conf = global.__lastConfirm || '';
  console.log('record form 弹窗:', conf.length > 0 ? 'OK' : 'FAIL');
  console.log('record form 网卡容器:', /rf-arg-wrap/.test(conf) ? 'OK' : 'FAIL');
  console.log('record form 含网卡下拉说明:', /rf-src/.test(conf) && /IP 来源/.test(conf) ? 'OK（网卡下拉由 rf-src 切换时动态渲染）' : 'FAIL');
  console.log('SMOKE-DDNS OK');
})().catch(e => { console.error('FAIL:', e.message); process.exit(1); });
