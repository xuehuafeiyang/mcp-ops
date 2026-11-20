#!/usr/bin/env bash
# mcp-ops 发布打包脚本
#
#   ./scripts/release.sh              # 全平台交叉编译（linux/darwin/windows）
#   ./scripts/release.sh 0.5.1        # 指定版本号
#   HOST_ONLY=1 ./scripts/release.sh  # 只构建当前平台，快速验证
#
# 产物：dist/mcp-ops_<version>_<os>_<arch>.tar.gz|.zip + dist/SHA256SUMS
set -euo pipefail

VERSION="${1:-0.6.0}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
DIST="$ROOT/dist"
PKG="mcp-ops"

# 发布包内含的文件/目录
PAYLOAD=(config.example.yaml README.md docs/MANUAL.md examples)

if [[ "${HOST_ONLY:-0}" == "1" ]]; then
  PLATFORMS=("$(go env GOOS)/$(go env GOARCH)")
else
  PLATFORMS=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64)
fi

echo "==> 版本 v$VERSION，目标平台 ${#PLATFORMS[@]} 个"
rm -rf "$DIST"
mkdir -p "$DIST"

# 清理旧的编译产物，确保打到包里的是新构建
cd "$ROOT"
go vet ./... >/dev/null

for p in "${PLATFORMS[@]}"; do
  os="${p%/*}"
  arch="${p#*/}"
  name="${PKG}_${VERSION}_${os}_${arch}"
  stage="$DIST/$name"
  mkdir -p "$stage"

  bin="opsctl"
  [[ "$os" == "windows" ]] && bin="opsctl.exe"

  echo "--> 编译 $os/$arch"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build \
    -trimpath \
    -ldflags "-s -w -X main.version=$VERSION" \
    -o "$stage/$bin" ./cmd/opsctl

  for item in "${PAYLOAD[@]}"; do
    if [[ -d "$ROOT/$item" ]]; then
      cp -r "$ROOT/$item" "$stage/"
    else
      cp "$ROOT/$item" "$stage/"
    fi
  done

  # 说明书放一份在包根目录，省得用户翻 docs/
  cp "$ROOT/docs/MANUAL.md" "$stage/MANUAL.md"

  if [[ "$os" == "windows" ]]; then
    (cd "$DIST" && zip -qr "$name.zip" "$name")
    rm -rf "$stage"
  else
    tar -czf "$DIST/$name.tar.gz" -C "$DIST" "$name"
    rm -rf "$stage"
  fi
done

cd "$DIST"
sha256sum ./*.tar.gz ./*.zip > SHA256SUMS 2>/dev/null || true
echo "==> 产物："
ls -lh "$DIST" | tail -n +2
echo "==> 校验和写入 dist/SHA256SUMS"
