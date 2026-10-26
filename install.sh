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
# This script only downloads the `sse` program for this CPU (amd64 / arm64 / armv7) and checks its SHA-256;
# `sse` does the rest. SSE_BINARY=/path/to/sse installs that file instead (for development); so does running
# ./install.sh inside a clone of the repository after `make build/sse`.
# 這個腳本只下載這台 NAS 的 CPU 用的 sse 程式並驗證 SHA-256，其餘都交給 sse。
# SSE_BINARY=/path/to/sse 安裝指定的檔案（開發用）；在原始碼目錄執行 ./install.sh 時安裝 build/sse。
#
# 整份腳本包在 main 裡、最後一行才執行：curl | sh 傳輸中斷時不會只跑到一半。

REPO=ExpTechTW/synology-photos-search-enhancement

say() { printf '%s\n' "$*"; }
die() { printf '✗ %s\n' "$*" >&2; exit 1; }

usage() {
    if [ -f "$0" ]; then sed -n '2,23p' "$0" | sed 's/^# \{0,1\}//'; else say "https://github.com/$REPO"; fi
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
checkout_dir() {
    case "$0" in
        install.sh | */install.sh) ;;
        *) return 1 ;;
    esac
    d="$(cd "$(dirname "$0")" 2>/dev/null && pwd)" || return 1
    [ -f "$d/go.mod" ] && [ -d "$d/cmd/sse" ] && printf '%s\n' "$d"
}

# 這台機器要下載的檔案，例如 sse-linux-amd64.tar.gz
asset_name() {
    os="$(uname -s | tr '[:upper:]' '[:lower:]')"
    case "$os" in
        linux | darwin) ;;
        *) die "unsupported OS / 不支援的系統：$os" ;;
    esac
    case "$(uname -m)" in
        x86_64 | amd64) arch=amd64 ;;
        aarch64 | arm64) arch=arm64 ;;
        armv7* | armv8l) arch=armv7 ;;
        *) die "unsupported CPU / 不支援的 CPU：$(uname -m)" ;;
    esac
    printf 'sse-%s-%s.tar.gz\n' "$os" "$arch"
}

# 下載、驗證、解開 sse 到 $WORK/sse
download_sse() {
    for c in curl tar sha256sum; do command -v $c >/dev/null || die "missing command / 找不到指令：$c"; done
    asset="$(asset_name)"
    if [ -n "$WANT" ]; then
        url="$WEB/releases/download/$WANT/$asset"
        say "Downloading $WANT / 下載 ${WANT}（${asset}）"
    else
        url="$WEB/releases/latest/download/$asset"
        say "Downloading the latest release / 下載最新版（${asset}）"
    fi
    if ! curl -fsSL -o "$WORK/$asset" "$url"; then
        [ -n "$WANT" ] || say "(no stable release yet? try --version TAG / 還沒有穩定版的話，可以用 --version 指定版本)" >&2
        die "download failed / 下載失敗：$url"
    fi
    curl -fsSL -o "$WORK/$asset.sha256" "$url.sha256" || die "download failed / 下載失敗：$url.sha256"
    sum="$(cut -d ' ' -f 1 <"$WORK/$asset.sha256")"
    echo "$sum  $WORK/$asset" | sha256sum -c - >/dev/null 2>&1 || die "checksum mismatch / sha256 不符"
    tar xzf "$WORK/$asset" -C "$WORK" || die "bad package / 安裝檔內容不對"
    [ -f "$WORK/sse" ] || die "bad package / 安裝檔內容不對"
}

main() {
    set -eu
    # DSM 的 /etc/profile 設了 umask 077：從 SSH 登入後執行時，建立的目錄會變成只有 root 能進入，
    # filter（http）與照片擁有者都執行不了程式
    umask 022
    ROOT="${SSE_ROOT:-}"                     # 測試用：把系統路徑移到沙盒
    PREFIX="$ROOT/usr/local/sse"
    STATE="$ROOT/usr/local/etc/sse/state.json"
    WEB="${SSE_GITHUB_URL:-https://github.com/$REPO}"

    # install / update / uninstall 與 --version 這裡要用；所有選項都原樣交給 sse
    CMD="" WANT=""
    n=$#
    prev=""
    while [ "$n" -gt 0 ]; do
        a="$1"
        shift
        n=$((n - 1))
        case "$a" in
            install | update | uninstall)
                if [ -z "$CMD" ] && [ "$prev" != --channel ] && [ "$prev" != --version ]; then CMD="$a"; else set -- "$@" "$a"; fi ;;
            -h | --help) usage; exit 0 ;;
            *)
                [ "$prev" != --version ] || WANT="$a"
                case "$a" in --version=*) WANT="${a#*=}" ;; esac
                set -- "$@" "$a" ;;
        esac
        prev="$a"
    done
    CMD="${CMD:-install}"
    # Python 版的 sudo sse update 下載了這個腳本來接手：它已經選好版本與管道
    if [ -n "${SSE_SOURCE:-}" ] && [ "$SSE_SOURCE" != local ] && [ -z "$WANT" ]; then
        WANT="$SSE_SOURCE"
        set -- "$@" --version "$SSE_SOURCE"
        [ -z "${SSE_CHANNEL:-}" ] || set -- "$@" --channel "$SSE_CHANNEL"
    fi

    if [ -z "$ROOT" ]; then
        [ "$(id -u)" = 0 ] || die "please run with sudo / 請用 sudo 執行：curl -fsSL … | sudo sh"
        grep -q '^majorversion="7"' /etc.defaults/VERSION 2>/dev/null || die "DSM 7 is required / 需要 DSM 7"
    fi

    if [ "$CMD" = uninstall ]; then
        if [ -x "$PREFIX/sse" ] && "$PREFIX/sse" --version >/dev/null 2>&1; then
            rc=0
            run_tty "$PREFIX/sse" uninstall "$@" || rc=$?
            exit $rc
        fi
        if [ -x "$PREFIX/venv/bin/python" ]; then   # 還是 Python 版
            rc=0
            run_tty "$PREFIX/venv/bin/python" -I -m sse uninstall "$@" || rc=$?
            exit $rc
        fi
        [ -e "$STATE" ] || [ -e "$PREFIX" ] || die "not installed / 沒有安裝"
        # 程式壞了或不見了：下載一份來解除安裝
    fi

    # 下載的程式放在 /usr/local 底下：DSM 的 /tmp 掛載成 noexec，執行不了
    mkdir -p "$ROOT/usr/local"
    WORK="$(mktemp -d "$ROOT/usr/local/.sse-work.XXXXXX")"
    trap 'rm -rf "$WORK" ${SSE_WORK:+"$SSE_WORK"}' EXIT
    chmod 755 "$WORK"     # 安裝前檢查會以 nginx 的帳號執行它
    # 本機的程式（開發用）：SSE_BINARY，或在原始碼目錄執行 ./install.sh 時的 build/sse
    bin="" flags=""
    if [ -n "${SSE_BINARY:-}" ]; then
        [ -f "$SSE_BINARY" ] || die "SSE_BINARY not found / 找不到 $SSE_BINARY"
        bin="$SSE_BINARY"
    elif src="$(checkout_dir)"; then
        [ -f "$src/build/sse" ] || die "build it first / 先編譯：make build/sse（給 NAS 用要加 GOOS=linux GOARCH=amd64 或 arm64）"
        bin="$src/build/sse"
    fi
    if [ -n "$bin" ]; then
        say "Using $bin / 使用本機的程式"
        cp "$bin" "$WORK/sse"
        flags="--local"
    else
        download_sse
    fi
    chmod 755 "$WORK/sse"
    "$WORK/sse" --version >/dev/null 2>&1 || die "cannot run sse / 執行不了下載的 sse（CPU 或系統不符？）"

    rc=0
    if [ "$CMD" = uninstall ]; then
        run_tty "$WORK/sse" uninstall "$@" || rc=$?
    else
        run_tty "$WORK/sse" bootstrap $flags "$CMD" "$@" || rc=$?
    fi
    exit $rc
}

main "$@"
