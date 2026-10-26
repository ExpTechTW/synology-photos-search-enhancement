#!/bin/sh
# 打包 release（CI 與測試共用）：
#   sh scripts/package.sh v2026.10.26+build.1 dist [OS/ARCH ...]
#
# 預設產生 linux 的 amd64、arm64、armv7：dist/sse-linux-<arch>.tar.gz 與 .sha256，裡面是 sse 執行檔
# （CGO_ENABLED=0 的靜態執行檔，版本號是 tag 的版本）、LICENSE、README。測試時可以指定別的平台，例如 darwin/arm64。
#
# 另外產生 synology-photos-search-enhancement.tar.gz（與 .sha256）：Python 版的 sudo sse update 下載的是這個檔名，
# 裡面放新版的 install.sh，由它接手下載 sse，所以舊版也能直接更新上來。
set -eu
TAG="$1"
OUT="$2"
shift 2
[ $# -gt 0 ] || set -- linux/amd64 linux/arm64 linux/armv7
echo "$TAG" | grep -Eq '^v[0-9]{4}\.[0-9]{2}\.[0-9]{2}\+build\.[0-9]+$' || { echo "版本格式不對：$TAG" >&2; exit 1; }
SRC="$(cd "$(dirname "$0")/.." && pwd)"
MODULE=github.com/ExpTechTW/synology-photos-search-enhancement
LEGACY=synology-photos-search-enhancement
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

mkdir -p "$OUT"
OUT="$(cd "$OUT" && pwd)"

# 檔案擁有者記成 root：NAS 上以 root 解開時不會出現奇怪的 uid
pack() { # pack <輸出檔> <目錄> <檔案…>
    out="$1" dir="$2"
    shift 2
    if tar --version 2>/dev/null | grep -q GNU; then
        tar -C "$dir" --owner=0 --group=0 --numeric-owner -czf "$out" "$@"
    else
        tar -C "$dir" --uid 0 --gid 0 -czf "$out" "$@"
    fi
    (cd "$(dirname "$out")" &&
     if command -v sha256sum >/dev/null; then sha256sum "$(basename "$out")"; else shasum -a 256 "$(basename "$out")"; fi \
        >"$(basename "$out").sha256")
    echo "${out}（${TAG#v}）"
}

for target in "$@"; do
    os="${target%/*}" arch="${target#*/}"
    case "$arch" in
        armv7) goarch=arm goarm=7 ;;
        *) goarch="$arch" goarm="" ;;
    esac
    d="$tmp/$os-$arch"
    mkdir -p "$d"
    (cd "$SRC" && CGO_ENABLED=0 GOOS="$os" GOARCH="$goarch" GOARM="$goarm" \
        go build -trimpath -ldflags "-s -w -X $MODULE/internal/version.Version=${TAG#v}" -o "$d/sse" ./cmd/sse)
    cp "$SRC/LICENSE" "$SRC/README.md" "$SRC/README.zh-TW.md" "$d/"
    pack "$OUT/sse-$os-$arch.tar.gz" "$d" sse LICENSE README.md README.zh-TW.md
done

mkdir -p "$tmp/legacy/$LEGACY"
cp "$SRC/install.sh" "$tmp/legacy/$LEGACY/"
# 舊版的 install.sh 要看到 pyproject.toml 才認得這是安裝檔
printf '# 讓 Python 版的 install.sh 認得這個安裝檔；程式在 sse-linux-*.tar.gz，由 install.sh 下載。\n' \
    >"$tmp/legacy/$LEGACY/pyproject.toml"
pack "$OUT/$LEGACY.tar.gz" "$tmp/legacy" "$LEGACY"
