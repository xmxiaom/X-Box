#!/usr/bin/env bash
# X-Box 构建脚本：交叉编译双架构 → 打包 fpk
set -e
BASE="/e/软件/开发/WorkBuddy/飞牛应用/x-box"
TOOL="/e/软件/开发/WorkBuddy/飞牛应用/linksstar-fnos/.toolchain"
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

echo "[3] 打包 fpk（platform=all）"
cd "$BASE"
# manifest 版本与界面版本（src/main.go 的 version 常量）强制同步：
# fnOS 以 manifest version 判断能否覆盖安装，两处不一致会导致同版本号无法覆盖安装
APP_VER=$(grep -oP 'version\s+=\s+"\K[0-9.]+' "$SRC/main.go" | head -1)
if [ -n "$APP_VER" ]; then
  sed -i "s/^version  *= .*/version               = $APP_VER/" "$PKG/manifest"
  echo "manifest version -> $APP_VER"
fi
rm -f X-Box.fpk
"$TOOL/fnpack.exe" build -d "$PKG"
ls -la "$BASE/"*.fpk
echo "=== $(date) X-Box build DONE ==="
