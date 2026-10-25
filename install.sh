#!/bin/sh
# synology-photos-search-enhancement: install / update / uninstall（安裝、更新、解除安裝）
#
#   curl -fsSL https://raw.githubusercontent.com/ExpTechTW/synology-photos-search-enhancement/main/install.sh | sudo sh
#
#   … | sudo sh -s -- [install]             install the latest release（安裝最新版；已安裝就重新安裝）
#   … | sudo sh -s -- update                update to the latest release（更新，等同 sudo sse update）
#   … | sudo sh -s -- uninstall [--purge]   remove everything（解除安裝）
#
# Options / 選項:
#   --channel release|pre-release   release (default, stable) or pre-release (every change on main)
#                                   release（預設，穩定版）或 pre-release（main 分支的每次更新）
#   --version vYYYY.MM.DD+build.N   install this version（安裝指定版本）
#   --force                         update even when not newer（版本相同或較舊也重新安裝）
#   --check                         update: only check for a new version（只檢查有沒有新版）
#   Anything else goes to `sse install`: --user NAME, --data-dir DIR, --from-config FILE, --dry-run, --yes
#   其他選項交給 sse install。
#
# Run from a git checkout (sudo ./install.sh), it installs that code instead of a release.
# 在原始碼目錄執行（sudo ./install.sh）時，安裝的是這份原始碼，不下載 release。
#
# Everything goes to /usr/local/sse (uv, a private Python 3.12, numpy, pyyaml); the system Python is not touched.
# 程式都放在 /usr/local/sse（uv、獨立的 Python 3.12、numpy、pyyaml），不碰系統的 Python。
#
# 整份腳本包在 main 裡、最後一行才執行：curl | sh 傳輸中斷時不會只跑到一半。

REPO=ExpTechTW/synology-photos-search-enhancement
NAME=synology-photos-search-enhancement
UV_VERSION=0.11.21

say() { printf '%s\n' "$*"; }
die() { printf '✗ %s\n' "$*" >&2; exit 1; }

usage() {
    sed -n '2,24p' "$0" 2>/dev/null | sed 's/^# \{0,1\}//' || say "https://github.com/$REPO"
}

# curl | sh 時 stdin 是腳本本身：要互動的程式改從終端機讀
run_tty() {
    if [ ! -t 0 ] && (exec </dev/tty) 2>/dev/null; then
        "$@" </dev/tty
    else
        "$@"
    fi
}

# 以 sh install.sh、./install.sh 執行，而且旁邊就是原始碼：回傳原始碼目錄
local_source() {
    case "$0" in
        install.sh | */install.sh) ;;
        *) return 1 ;;
    esac
    d="$(cd "$(dirname "$0")" 2>/dev/null && pwd)" || return 1
    [ -f "$d/pyproject.toml" ] && [ -f "$d/sse/__init__.py" ] && printf '%s\n' "$d"
}

# 用 DSM 內建的 Python 3.8 解析 GitHub 的回應、比較版本
py() {
    "$PY" -I - "$@" <<'PYEOF'
import json
import re
import sys

TAG = re.compile(r"^v?(\d{4})\.(\d{2})\.(\d{2})\+build\.(\d+)$")
ASSET = "synology-photos-search-enhancement.tar.gz"


def ver(tag):
    m = TAG.match((tag or "").strip())
    return tuple(int(x) for x in m.groups()) if m else (0, 0, 0, 0)   # 原始碼安裝的開發版比任何 release 舊


op = sys.argv[1]
if op == "pick":                     # pick <channel> <releases.json> <指定版本或空白>
    channel, path, want = sys.argv[2:5]
    with open(path) as f:
        rels = [r for r in json.load(f) if not r.get("draft")]
    if want:
        rels = [r for r in rels if r.get("tag_name", "").lstrip("v") == want.lstrip("v")]
    elif channel == "release":
        rels = [r for r in rels if not r.get("prerelease")]
    if not rels:
        sys.exit("✗ no matching release / 找不到%s" % ("版本 " + want if want else " %s 的版本" % channel))
    rel = max(rels, key=lambda r: ver(r.get("tag_name")))
    urls = {a.get("name"): a.get("browser_download_url") for a in rel.get("assets") or []}
    if not urls.get(ASSET) or not urls.get(ASSET + ".sha256"):
        sys.exit("✗ release has no package / 版本 %s 沒有安裝檔" % rel.get("tag_name"))
    print(rel["tag_name"])
    print(urls[ASSET])
    print(urls[ASSET + ".sha256"])
elif op == "cmp":                    # cmp <已安裝> <新版> → newer / same / older
    a, b = ver(sys.argv[2]), ver(sys.argv[3])
    print("newer" if b > a else "same" if b == a else "older")
elif op == "channel":                # channel <state.json>：安裝時選的 channel
    try:
        with open(sys.argv[2]) as f:
            print(json.load(f).get("channel") or "")
    except (OSError, ValueError):
        print("")
PYEOF
}

# ── 程式放到 /usr/local/sse ──────────────────────────────────────────────

install_uv() {
    case "$(uname -m)" in
        x86_64) arch=x86_64; sum=9dadff5b9e7b1d2d011e41852a1cbca713d9d5d88194f2eb6bd240fa4fb0a719 ;;
        aarch64 | arm64) arch=aarch64; sum=e71badaed2a2c3a404a0a00974b51c7ed5f5bc7be947916846005b739c68a5a2 ;;
        *) die "unsupported CPU / 不支援的 CPU：$(uname -m)" ;;
    esac
    say "Downloading uv $UV_VERSION / 下載 uv"
    curl -fsSL -o "$WORK/uv.tar.gz" \
        "https://github.com/astral-sh/uv/releases/download/$UV_VERSION/uv-$arch-unknown-linux-musl.tar.gz"
    echo "$sum  $WORK/uv.tar.gz" | sha256sum -c - >/dev/null || die "uv checksum mismatch / uv 的 sha256 不符"
    tar xzf "$WORK/uv.tar.gz" -C "$WORK"
    cp "$WORK/uv-$arch-unknown-linux-musl/uv" "$PREFIX/bin/uv"
    chmod 755 "$PREFIX/bin/uv"
}

# 裝到 venv.new，成功後才換掉 venv；更新失敗時換回原本的版本
install_from() {
    src="$1"
    shift
    mkdir -p "$PREFIX/bin"
    chmod 755 "$PREFIX"
    UV="${SSE_UV:-$PREFIX/bin/uv}"
    if [ -z "${SSE_UV:-}" ] && ! "$UV" --version 2>/dev/null | grep -q "^uv $UV_VERSION "; then
        install_uv
    fi
    export UV_PYTHON_INSTALL_DIR="${UV_PYTHON_INSTALL_DIR:-$PREFIX/python}"
    export UV_CACHE_DIR="${UV_CACHE_DIR:-$PREFIX/cache}"     # 測試時可指向自己的 uv 快取
    export UV_NO_CONFIG=1
    rm -rf "$PREFIX/venv.new" "$PREFIX/venv.prev"            # 上次中斷留下的
    if "$PREFIX/venv/bin/python" -c 'import sys; assert sys.version_info >= (3, 12)' 2>/dev/null; then
        cp -a "$PREFIX/venv" "$PREFIX/venv.new"              # 沿用已經裝好的 numpy，不必重新下載
    else
        say "Installing Python 3.12 / 安裝 Python 3.12"
        # only-managed：不要用到 DSM 內建的 Python 3.8；relocatable：之後才能整個目錄改名
        "$UV" venv --quiet --relocatable --python 3.12 --python-preference only-managed "$PREFIX/venv.new"
    fi
    say "Installing $NAME / 安裝程式"
    "$UV" pip install --quiet --python "$PREFIX/venv.new/bin/python" --reinstall-package "$NAME" "$src"
    [ "$UV_CACHE_DIR" != "$PREFIX/cache" ] || rm -rf "$UV_CACHE_DIR"    # 下載快取用不到了，省空間

    if [ "$DRY" = 1 ]; then
        rc=0
        run_tty "$PREFIX/venv.new/bin/python" -I -m sse install "$@" || rc=$?
        rm -rf "$PREFIX/venv.new"
        [ "$NEW" = 0 ] || rm -rf "$PREFIX"    # 第一次就只是 --dry-run：不留痕跡
        exit $rc
    fi

    [ ! -d "$PREFIX/venv" ] || mv "$PREFIX/venv" "$PREFIX/venv.prev"
    mv "$PREFIX/venv.new" "$PREFIX/venv"
    rc=0
    run_tty "$PREFIX/venv/bin/python" -I -m sse install --channel "$CHANNEL" --source "$SOURCE" "$@" || rc=$?
    if [ $rc -ne 0 ]; then
        if [ -d "$PREFIX/venv.prev" ]; then
            # sse install 失敗時已經把系統設定回復成原本的樣子；這裡把程式也換回去，再重啟用到程式的服務
            say "Restoring the previous version / 換回原本的版本"
            rm -rf "$PREFIX/venv"
            mv "$PREFIX/venv.prev" "$PREFIX/venv"
            for u in sse-engine.service sse-indexer.service; do
                if [ -f "$ROOT/etc/systemd/system/$u" ]; then
                    "$SYSTEMCTL" restart "$u" >/dev/null 2>&1 || true
                fi
            done
        fi
        exit $rc
    fi
    rm -rf "$PREFIX/venv.prev"
    # 留一份給 sudo sse update 用
    cp "$src/install.sh" "$PREFIX/install.sh.tmp"
    chmod 755 "$PREFIX/install.sh.tmp"
    mv "$PREFIX/install.sh.tmp" "$PREFIX/install.sh"
}

# ── release ─────────────────────────────────────────────────────────────

fetch_release() {
    curl -fsSL -H "Accept: application/vnd.github+json" -o "$WORK/releases.json" "$API/releases?per_page=100" \
        || die "cannot reach GitHub / 連不上 GitHub：$API"
    out="$(py pick "$CHANNEL" "$WORK/releases.json" "$WANT")" || exit 1
    TAG="$(printf '%s\n' "$out" | sed -n 1p)"
    URL="$(printf '%s\n' "$out" | sed -n 2p)"
    SHA_URL="$(printf '%s\n' "$out" | sed -n 3p)"
}

download_release() {
    say "Downloading $TAG / 下載 $TAG"
    curl -fsSL -o "$WORK/$NAME.tar.gz" "$URL" || die "download failed / 下載失敗：$URL"
    curl -fsSL -o "$WORK/$NAME.tar.gz.sha256" "$SHA_URL" || die "download failed / 下載失敗：$SHA_URL"
    sum="$(cut -d ' ' -f 1 <"$WORK/$NAME.tar.gz.sha256")"
    echo "$sum  $WORK/$NAME.tar.gz" | sha256sum -c - >/dev/null || die "checksum mismatch / sha256 不符"
    tar xzf "$WORK/$NAME.tar.gz" -C "$WORK"
    [ -f "$WORK/$NAME/install.sh" ] && [ -f "$WORK/$NAME/pyproject.toml" ] || die "bad package / 安裝檔內容不對"
}

main() {
    set -eu
    ROOT="${SSE_ROOT:-}"                     # 測試用：把系統路徑移到沙盒
    PREFIX="$ROOT/usr/local/sse"
    STATE="$ROOT/usr/local/etc/sse/state.json"
    API="${SSE_GITHUB_API:-https://api.github.com/repos/$REPO}"
    PY="${SSE_PYTHON:-/usr/bin/python3}"
    SYSTEMCTL="${SSE_SYSTEMCTL:-systemctl}"

    # 選項：自己用的拿掉，其他留在 "$@" 交給 sse install / uninstall
    CMD="" CHANNEL="" WANT="" FORCE=0 CHECK=0 DRY=0
    n=$#
    while [ "$n" -gt 0 ]; do
        a="$1"
        shift
        n=$((n - 1))
        case "$a" in
            install | update | uninstall)
                if [ -z "$CMD" ]; then CMD="$a"; else set -- "$@" "$a"; fi ;;
            --channel | --version)
                [ "$n" -gt 0 ] || die "$a needs a value / 要指定值"
                if [ "$a" = --channel ]; then CHANNEL="$1"; else WANT="$1"; fi
                shift
                n=$((n - 1)) ;;
            --channel=*) CHANNEL="${a#*=}" ;;
            --version=*) WANT="${a#*=}" ;;
            --force) FORCE=1 ;;
            --check) CHECK=1 ;;
            -h | --help) usage; exit 0 ;;
            *)
                [ "$a" != --dry-run ] || DRY=1
                set -- "$@" "$a" ;;
        esac
    done
    CMD="${CMD:-install}"

    if [ -z "$ROOT" ]; then
        [ "$(id -u)" = 0 ] || die "please run with sudo / 請用 sudo 執行：curl -fsSL … | sudo sh"
        grep -q '^majorversion="7"' /etc.defaults/VERSION 2>/dev/null || die "DSM 7 is required / 需要 DSM 7"
    fi

    if [ "$CMD" = uninstall ]; then
        [ -x "$PREFIX/venv/bin/python" ] || die "not installed / 沒有安裝"
        rc=0
        run_tty "$PREFIX/venv/bin/python" -I -m sse uninstall "$@" || rc=$?
        exit $rc
    fi

    for c in curl tar sha256sum; do command -v $c >/dev/null || die "missing command / 找不到指令：$c"; done
    NEW=0
    [ -e "$PREFIX" ] || NEW=1
    WORK="$(mktemp -d)"
    trap 'rm -rf "$WORK" ${SSE_WORK:+"$SSE_WORK"}' EXIT

    CHANNEL="${CHANNEL:-${SSE_CHANNEL:-}}"
    [ -n "$CHANNEL" ] || CHANNEL="$(py channel "$STATE" 2>/dev/null || true)"
    CHANNEL="${CHANNEL:-release}"
    case "$CHANNEL" in
        release | pre-release) ;;
        *) die "--channel must be release or pre-release / --channel 只能是 release 或 pre-release" ;;
    esac

    if src="$(local_source)"; then
        [ "$CMD" != update ] || [ "$DRY" = 1 ] || set -- --yes "$@"   # 要求更新就是同意了，不再詢問
        SOURCE="${SSE_SOURCE:-local}"
        [ -n "${SSE_SOURCE:-}" ] || say "Installing from $src / 安裝這個目錄裡的原始碼"
        install_from "$src" "$@"
        exit 0
    fi

    # 從 GitHub release 安裝
    if [ "$CMD" = update ]; then
        [ -x "$PREFIX/venv/bin/python" ] || die "not installed / 還沒安裝：curl -fsSL … | sudo sh"
    fi
    fetch_release
    if [ -x "$PREFIX/venv/bin/python" ] && [ -z "$WANT" ]; then
        cur="$("$PREFIX/venv/bin/python" -I -c 'import sse; print(sse.__version__)' 2>/dev/null || true)"
        rel="$(py cmp "$cur" "$TAG")"
        say "Installed / 目前：${cur:-?}   latest $CHANNEL / 最新版：$TAG"
        if [ "$CHECK" = 1 ]; then
            if [ "$rel" = newer ]; then say "New version available / 有新版：sudo sse update"; else say "Up to date / 已是最新版本"; fi
            exit 0
        fi
        if [ "$CMD" = update ] && [ "$FORCE" = 0 ]; then
            if [ "$rel" = same ]; then say "Up to date / 已是最新版本"; exit 0; fi
            if [ "$rel" = older ]; then
                say "$TAG is older than the installed version; not downgrading (add --force to do it anyway)"
                say "$TAG 比目前安裝的版本舊，不降版（要降版請加 --force）"
                exit 0
            fi
        fi
    fi
    download_release
    # 用新版自己的 install.sh 安裝：安裝流程的修正跟著新版一起生效
    SSE_WORK="$WORK" SSE_SOURCE="$TAG" SSE_CHANNEL="$CHANNEL" exec sh "$WORK/$NAME/install.sh" "$CMD" "$@"
}

main "$@"
