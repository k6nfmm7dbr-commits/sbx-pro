#!/usr/bin/env bash
# build-release.sh — 交叉编译 sbx-manager / sbx-agent 并生成 SHA256SUMS
# 用法: ./scripts/build-release.sh [输出目录，默认 dist]
set -euo pipefail
cd "$(dirname "$0")/.." || exit 1

OUT="${1:-dist}"
VERSION="$(cat internal/version/version.go | grep -oE '"[0-9]+\.[0-9]+\.[0-9]+"' | tr -d '"' | head -1)"
[[ -n "$VERSION" ]] || VERSION="dev"
LDFLAGS="-s -w -X github.com/k6nfmm7dbr-commits/sbx-pro/internal/version.Version=${VERSION}"

rm -rf "$OUT"; mkdir -p "$OUT"

# 架构列表（第一版聚焦 amd64/arm64，覆盖绝大多数 VPS）。
ARCHS="amd64 arm64"

build_one() { # build_one <cmd> <goarch>
  local cmd="$1" goarch="$2"
  echo "==> building linux/$goarch -> ${cmd}-linux-${goarch}"
  env GOOS=linux GOARCH="$goarch" CGO_ENABLED=0 \
    go build -trimpath -ldflags "$LDFLAGS" -o "$OUT/${cmd}-linux-${goarch}" "./cmd/${cmd}"
}

for arch in $ARCHS; do
  build_one sbx-manager "$arch"
  build_one sbx-agent "$arch"
done

cd "$OUT" || exit 1
sha256sum sbx-manager-linux-* sbx-agent-linux-* > SHA256SUMS
echo "---- 产物 ----"
ls -la
echo "---- SHA256SUMS ----"
cat SHA256SUMS
