#!/bin/bash
# build.sh - 编译 qoder-cpa-plugin
# 用法: ./scripts/build.sh [linux|darwin|windows] [amd64|arm64]
# 默认: 当前平台

set -e

TARGET_OS=${1:-$(go env GOOS)}
TARGET_ARCH=${2:-$(go env GOARCH)}
ROOT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
PLUGIN_DIR="$ROOT_DIR/plugins/qoder"
OUT_DIR="$ROOT_DIR/dist/$TARGET_OS-$TARGET_ARCH"

echo "=== qoder-cpa-plugin build ==="
echo "Target: $TARGET_OS/$TARGET_ARCH"

EXT="so"
if [ "$TARGET_OS" = "darwin" ]; then
  EXT="dylib"
elif [ "$TARGET_OS" = "windows" ]; then
  EXT="dll"
fi

mkdir -p "$OUT_DIR"

export CGO_ENABLED=1
export GOOS=$TARGET_OS
export GOARCH=$TARGET_ARCH

if [ -z "${CC:-}" ]; then
  case "$TARGET_OS" in
    darwin) CC="clang" ;;
    windows) CC="x86_64-w64-mingw32-gcc" ;;
    *) CC="gcc" ;;
  esac
fi
export CC

cd "$PLUGIN_DIR"
go build -buildmode=c-shared -trimpath -ldflags="-s -w" -o "$OUT_DIR/qoder.$EXT" .

echo "Built: $OUT_DIR/qoder.$EXT ($(du -h "$OUT_DIR/qoder.$EXT" | cut -f1))"
