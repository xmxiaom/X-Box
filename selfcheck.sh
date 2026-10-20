#!/usr/bin/env bash
# X-Box 打包自检：结构 / 脚本 / JSON / 二进制 / 图标 / fpk / 安全约束
# 用法：bash selfcheck.sh   （任意位置执行均可）
set -u
BASE="$(cd "$(dirname "$0")" && pwd)"
PKG="$BASE/pkg"
PY="C:/Users/xiaom/.workbuddy/binaries/python/versions/3.13.12/python.exe"
PASS=0; FAIL=0

ok()   { echo "  ✅ $1"; PASS=$((PASS+1)); }
bad()  { echo "  ❌ $1"; FAIL=$((FAIL+1)); }
check(){ if eval "$2"; then ok "$1"; else bad "$1"; fi; }

cd "$BASE" || exit 1

echo "========== X-Box 自检 =========="

echo "[1] manifest 字段"
for k in appname version display_name desc platform source maintainer desktop_uidir desktop_applaunchname service_port checkport; do
  check "manifest 含 $k" "grep -q '^$k' pkg/manifest"
done
check "appname = X-Box" "grep -q '^appname * = *X-Box$' pkg/manifest"
check "platform = all" "grep -q '^platform * = *all$' pkg/manifest"
check "service_port = 8899" "grep -q '^service_port * = *8899$' pkg/manifest"

echo "[2] 生命周期脚本语法"
for f in pkg/cmd/*; do
  check "$(basename "$f") bash -n" "bash -n '$f'"
done

echo "[3] JSON 合法性"
check "config/privilege 合法 JSON 且 run-as=root" "\"$PY\" -c \"import json;d=json.load(open('pkg/config/privilege'));assert d['defaults']['run-as']=='root'\""
check "config/resource 合法 JSON" "\"$PY\" -c \"import json;json.load(open('pkg/config/resource'))\""
check "app/ui/config 合法 JSON 且网关 socket 正确" "\"$PY\" -c \"import json;d=json.load(open('pkg/app/ui/config'));a=d['.url']['X-Box.Application'];assert a['gatewaySocket']=='app.sock' and a['gatewayPrefix']=='/app/X-Box'\""

echo "[4] 双架构二进制"
for a in amd64 arm64; do
  f="pkg/app/server/xbox-$a"
  check "xbox-$a 存在且非空" "[ -s '$f' ]"
  check "xbox-$a 为 ELF" "\"$PY\" -c \"assert open('$f','rb').read(4)==b'\x7fELF'\""
done
check "二进制含全部 API 路由" "grep -qa 'api/scan' pkg/app/server/xbox-amd64 && grep -qa 'api/clean' pkg/app/server/xbox-amd64 && grep -qa 'api/ports' pkg/app/server/xbox-amd64 && grep -qa 'api/overview' pkg/app/server/xbox-amd64"
check "二进制含白名单根（清理安全闸）" "grep -qa '/var/cache/apt/archives' pkg/app/server/xbox-amd64 && grep -qa 'systemd/coredump' pkg/app/server/xbox-amd64"
check "二进制含内嵌 UI（中文标题）" "grep -qa '垃圾清理' pkg/app/server/xbox-amd64"
check "二进制监听 8899" "grep -qa '8899' pkg/app/server/xbox-amd64"
check "无登录/令牌逻辑（应用户要求）" "! grep -qa 'access_token' pkg/app/server/xbox-amd64 && ! grep -q 'handleLogin' src/main.go"

echo "[5] 图标（内外统一）"
check "ICON.PNG 256px（与 ui 同源高清，见 gen_icons.py 说明）" "\"$PY\" -c \"import struct;d=open('pkg/ICON.PNG','rb').read();assert struct.unpack('>II',d[16:24])==(256,256)\""
check "ICON_256.PNG 256px" "\"$PY\" -c \"import struct;d=open('pkg/ICON_256.PNG','rb').read();assert struct.unpack('>II',d[16:24])==(256,256)\""
check "ui icon_64/icon_256 齐备" "[ -f 'pkg/app/ui/images/icon_64.png' ] && [ -f 'pkg/app/ui/images/icon_256.png' ]"
check "根图标与 ui 图标字节一致" "\"$PY\" -c \"assert open('pkg/ICON.PNG','rb').read()==open('pkg/app/ui/images/icon_64.png','rb').read();assert open('pkg/ICON_256.PNG','rb').read()==open('pkg/app/ui/images/icon_256.png','rb').read()\""
check "UI 图标由外部文件承载（config 引用 images/icon_256.png）" "grep -q 'images/icon_256.png' pkg/app/ui/config"

echo "[6] fpk 包结构"
FPK="X-Box.fpk"
check "fpk 存在" "[ -s '$FPK' ]"
check "fpk 为 gzip" "\"$PY\" -c \"assert open('X-Box.fpk','rb').read(2)==b'\x1f\x8b'\""
check "fpk 含 app.tgz/manifest/ICON/cmd/config" "\"$PY\" -c \"import tarfile;names=set(tarfile.open('X-Box.fpk').getnames());need={'app.tgz','manifest','ICON.PNG','ICON_256.PNG','cmd/main','config/privilege'};assert need.issubset(names),names-need\""

echo "[7] 安全约束（清理白名单硬闸）"
check "清理复核 Lstat（不跟随符号链接）" "grep -q 'os.Lstat' src/junk.go"
check "扫描跳过符号链接/socket" "grep -q 'd.Type()&^fs.ModePerm' src/junk.go"
check "代码无 /home 白名单（不碰用户数据）" "! grep -q '白名单.*home' src/junk.go"

echo
echo "========== 结果：通过 $PASS / 失败 $FAIL =========="
[ $FAIL -eq 0 ] && echo "✅ 自检全部通过" || echo "❌ 存在失败项，禁止交付"
exit $FAIL
