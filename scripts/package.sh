#!/bin/sh
# 打包 release（CI 與測試共用）：
#   sh scripts/package.sh v2026.10.26+build.1 dist
# 產生 dist/synology-photos-search-enhancement.tar.gz 與 .sha256；程式的版本號改成 tag 的版本。
set -eu
TAG="$1"
OUT="$2"
NAME=synology-photos-search-enhancement
echo "$TAG" | grep -Eq '^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}\+build\.[0-9]+$' || { echo "版本格式不對：$TAG" >&2; exit 1; }
SRC="$(cd "$(dirname "$0")/.." && pwd)"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mkdir "$tmp/$NAME"
(cd "$SRC" && cp -R install.sh pyproject.toml README.md README.zh-TW.md LICENSE sse docs "$tmp/$NAME/")
find "$tmp/$NAME" -name __pycache__ -type d -prune -exec rm -rf {} +
sed "s/^__version__ = .*/__version__ = \"${TAG#v}\"/" "$SRC/sse/__init__.py" >"$tmp/$NAME/sse/__init__.py"
grep -q "^__version__ = \"${TAG#v}\"" "$tmp/$NAME/sse/__init__.py"

mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"
# 檔案擁有者記成 root：NAS 上以 root 解開時不會出現奇怪的 uid
if tar --version 2>/dev/null | grep -q GNU; then
    tar -C "$tmp" --owner=0 --group=0 --numeric-owner -czf "$OUT/$NAME.tar.gz" "$NAME"
else
    tar -C "$tmp" --uid 0 --gid 0 -czf "$OUT/$NAME.tar.gz" "$NAME"
fi
cd "$OUT"
if command -v sha256sum >/dev/null; then
    sha256sum "$NAME.tar.gz" >"$NAME.tar.gz.sha256"
else
    shasum -a 256 "$NAME.tar.gz" >"$NAME.tar.gz.sha256"
fi
echo "$OUT/$NAME.tar.gz（${TAG#v}）"
