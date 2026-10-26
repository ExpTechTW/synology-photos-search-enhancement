#!/bin/sh
# 跑全部測試。需要 Go 與 python3（只用標準庫）；第一次會編譯 nginx 1.23.1（DSM 7.3 的版本）。
#
#   sh tests/run.sh            全部
#   sh tests/run.sh quick      只跑 Go 的測試（幾秒）
#
# 可用環境變數改用自己的：SSE_NGINX_BIN；暫存目錄 SSE_TEST_TMP（macOS 的 unix socket 路徑上限 104 字元）
set -eu
cd "$(dirname "$0")/.."
CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/sse-tests"
mkdir -p "$CACHE"
export SSE_TEST_TMP="${SSE_TEST_TMP:-/tmp}"
export PYTHONDONTWRITEBYTECODE=1

echo "== Go（go vet、go test、編譯 build/sse）"
go vet ./...
go test ./...
make build/sse
export SSE_BIN="$PWD/build/sse"
[ "${1:-}" = quick ] && exit 0

if [ -z "${SSE_NGINX_BIN:-}" ]; then
    SSE_NGINX_BIN="$CACHE/nginx-1.23.1/objs/nginx"
    if [ ! -x "$SSE_NGINX_BIN" ]; then
        echo "== 編譯 nginx 1.23.1（只有第一次）"
        (cd "$CACHE" &&
         curl -fsSL https://nginx.org/download/nginx-1.23.1.tar.gz | tar xz &&
         curl -fsSL https://github.com/PCRE2Project/pcre2/releases/download/pcre2-10.40/pcre2-10.40.tar.gz | tar xz &&
         cd nginx-1.23.1 &&
         ./configure --with-pcre=../pcre2-10.40 --without-http_gzip_module >/dev/null &&
         make -j4 >/dev/null)
    fi
fi
export SSE_NGINX_BIN
echo "== 端對端（真的 nginx $("$SSE_NGINX_BIN" -v 2>&1 | sed 's/.*\///')、${SSE_BIN}）"
python3 -W ignore -m unittest tests.test_e2e_nginx tests.test_e2e_engine tests.test_install_sandbox
