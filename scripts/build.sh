#!/usr/bin/env bash
# Manual cross-builds of the 5 release targets into dist/ — an alternative
# to `goreleaser release`.
set -euo pipefail
cd "$(dirname "$0")/.."

rm -rf dist
mkdir -p dist

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64 windows/amd64; do
  os=${target%/*}
  arch=${target#*/}
  ext=""
  [ "$os" = "windows" ] && ext=".exe"
  out="dist/ci-brief_${os}_${arch}${ext}"
  echo "building $out"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$out" .
done

(cd dist && shasum -a 256 ci-brief_* > checksums.txt)
echo "done:"
ls -la dist/
