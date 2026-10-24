#!/usr/bin/env bash
# X-Box 构建脚本：交叉编译双架构 → 打包 fpk
set -e
BASE="/g/软件/开发/WorkBuddy/飞牛应用/x-box"
TOOL="/g/软件/开发/WorkBuddy/飞牛应用/linksstar-fnos/.toolchain"
SRC="$BASE/src"
PKG="$BASE/pkg"
export GOROOT="$TOOL/go"
export PATH="$GOROOT/bin:$PATH"
export GOFLAGS="-mod=mod"
export CGO_ENABLED=0

echo "=== $(date) X-Box build start ==="
echo "[1] 编译 linux/amd64"
cd "$SRC"
GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o "$PKG/app/server/xbox-amd64" .
echo "[2] 编译 linux/arm64"
GOOS=linux GOARCH=arm64 go build -trimpath -ldflags="-s -w" -o "$PKG/app/server/xbox-arm64" .
chmod +x "$PKG/app/server/xbox-amd64" "$PKG/app/server/xbox-arm64"
ls -la "$PKG/app/server/"

echo "[3] 生成图标（纯 Python 无依赖；产物被 gitignore，源码目录静态保持干净）"
# gen_icons.py 在仓库外 tools/（开发侧工具），输出 4 枚 256px 图标到 pkg/
PYGEN="C:/Users/Administrator/.workbuddy/binaries/python/versions/3.13.12/python.exe"
[ -x "$PYGEN" ] || PYGEN="$(command -v python3 || command -v python || echo python3)"
cd "$BASE"
"$PYGEN" "$BASE/../tools/gen_icons.py"

echo "[4] 打包 fpk（platform=all）"
cd "$BASE"
# manifest 版本与界面版本（src/main.go 的 version 常量）强制同步：
# fnOS 以 manifest version 判断能否覆盖安装，两处不一致会导致同版本号无法覆盖安装
APP_VER=$(grep -oP 'version\s+=\s+"\K[0-9.]+' "$SRC/main.go" | head -1)
if [ -n "$APP_VER" ]; then
  sed -i "s/^version  *= .*/version               = $APP_VER/" "$PKG/manifest"
  echo "manifest version -> $APP_VER"
fi
# 产物输出到仓库外的 dist/：源码目录只放源码，不放安装包
DIST="$BASE/../dist"
mkdir -p "$DIST"
rm -f "$DIST/X-Box.fpk"
"$TOOL/fnpack.exe" build -d "$PKG"
mv -f "$BASE/X-Box.fpk" "$DIST/X-Box.fpk"
ls -la "$DIST/"*.fpk
echo "=== $(date) X-Box build DONE ==="
