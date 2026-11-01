#!/bin/sh
set -eu

version=${VERSION:-0.1.0-dev}
version=${version#v}

case "$version" in
  ""|*[!0-9A-Za-z.+-]*)
    echo "版本号只能包含数字、字母、点、加号与连字符：$version" >&2
    exit 2
    ;;
esac

targets="linux/amd64 linux/arm64 darwin/amd64 darwin/arm64"
tmpdir=$(mktemp -d)
trap 'rm -rf "$tmpdir"' EXIT

rm -rf dist
mkdir -p dist
distdir=$(pwd)/dist

for target in $targets; do
  goos=${target%/*}
  goarch=${target#*/}
  binary=lax
  archive="lax_${version}_${goos}_${goarch}.tar.gz"

  out="$tmpdir/$binary"
  CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
    go build -trimpath -ldflags "-s -w -X lax/internal/cli.version=$version" -o "$out" ./cmd/lax

  tar -czf "$distdir/$archive" -C "$tmpdir" "$binary"
  rm -f "$out"
done

if command -v sha256sum >/dev/null 2>&1; then
  (cd dist && sha256sum lax_* > checksums.txt)
else
  (cd dist && shasum -a 256 lax_* > checksums.txt)
fi

echo "已生成 dist/ 下的 $version 发布包"
