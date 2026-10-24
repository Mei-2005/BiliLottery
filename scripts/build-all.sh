#!/usr/bin/env bash
# 交叉编译全部支持的平台，产物输出到 dist/
# 用法：bash scripts/build-all.sh
set -euo pipefail
cd "$(dirname "$0")/.."

if ! command -v go >/dev/null 2>&1; then
    echo "ERROR: Go was not found in PATH." >&2
    exit 1
fi
go version

if [ -z "${GOPROXY:-}" ]; then
    export GOPROXY="https://goproxy.cn,direct"
fi
go mod tidy

mkdir -p dist

# Windows 需要嵌入应用清单（rsrc 不可用时跳过清单，不影响功能）
build_windows() {
    local ldflags="-s -w -H=windowsgui"
    if command -v rsrc >/dev/null 2>&1; then
        rsrc -manifest app.manifest -o rsrc_windows_amd64.syso
        if [ -s rsrc_windows_amd64.syso ]; then
            ldflags="-s -w -H=windowsgui"
        fi
    else
        echo "warning: rsrc not found, building Windows binary without manifest" >&2
    fi
    GOOS=windows GOARCH=amd64 CGO_ENABLED=0 \
        go build -trimpath -buildvcs=false -ldflags="$ldflags" -o dist/BiliLottery-windows-amd64.exe .
}

build_unix() {
    local goos="$1" goarch="$2" name="$3"
    GOOS="$goos" GOARCH="$goarch" CGO_ENABLED=0 \
        go build -trimpath -buildvcs=false -ldflags="-s -w" -o "dist/BiliLottery-${name}-${goarch}" .
}

echo ""
echo "[1/4] windows/amd64"
build_windows

echo "[2/4] linux/amd64"
build_unix linux amd64 linux

echo "[3/4] darwin/amd64"
build_unix darwin amd64 mac

echo "[4/4] darwin/arm64"
build_unix darwin arm64 mac

echo ""
rm -f rsrc_windows_amd64.syso
echo "BUILD SUCCESS"
ls -lh dist
