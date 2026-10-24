#!/usr/bin/env bash
# 构建当前平台的产物（Linux / macOS）
# 用法：bash scripts/build-unix.sh
set -euo pipefail
cd "$(dirname "$0")/.."

echo "========================================"
echo "BiliLottery Build (Linux/macOS)"
echo "========================================"

if ! command -v go >/dev/null 2>&1; then
    echo "ERROR: Go was not found in PATH." >&2
    exit 1
fi
go version

echo ""
echo "[1/2] Downloading Go modules..."
if [ -z "${GOPROXY:-}" ]; then
    export GOPROXY="https://goproxy.cn,direct"
fi
go mod tidy

echo ""
echo "[2/2] Building..."
export CGO_ENABLED=0

plat="$(go env GOOS)"
arch="$(go env GOARCH)"
case "$plat" in
    darwin) name="mac" ;;
    linux)  name="linux" ;;
    *)      name="$plat" ;;
esac

mkdir -p dist
out="dist/BiliLottery-${name}-${arch}"
go build -trimpath -buildvcs=false -ldflags="-s -w" -o "$out" .

echo ""
echo "BUILD SUCCESS"
echo "Output: $(pwd)/${out}"
echo "Run it with: ./${out}"