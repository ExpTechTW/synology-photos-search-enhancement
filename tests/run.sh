#!/bin/sh
# 跑全部測試。需要 uv；第一次會下載 Python 3.8（DSM 內建的版本）、3.12，並編譯 nginx 1.23.1（DSM 7.3 的版本）。
#
#   sh tests/run.sh            全部
#   sh tests/run.sh quick      只跑不需要 nginx 的（幾十秒）
#
# 可用環境變數改用自己的：SSE_NGINX_BIN、SSE_FILTER_PY；暫存目錄 SSE_TEST_TMP（macOS 的 unix socket 路徑上限 104 字元）
set -eu
cd "$(dirname "$0")/.."
CACHE="${XDG_CACHE_HOME:-$HOME/.cache}/sse-tests"
mkdir -p "$CACHE"
export SSE_TEST_TMP="${SSE_TEST_TMP:-/tmp}"
PY38="${SSE_FILTER_PY:-}"
if [ -z "$PY38" ]; then
    uv python install --quiet 3.8
    PY38="$(uv python find 3.8)"
fi
export SSE_FILTER_PY="$PY38"
export PYTHONDONTWRITEBYTECODE=1

echo "== filter（DSM 內建的 Python 3.8，只用標準庫）"
"$PY38" -m unittest tests.test_filter
echo "== sse（Python 3.12）"
uv run --quiet --no-project --python 3.12 --with-editable . python -W ignore -m unittest tests.test_admin tests.test_engine
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
echo "== 端對端（真的 nginx $("$SSE_NGINX_BIN" -v 2>&1 | sed 's/.*\///')）"
uv python install --quiet 3.12     # install.sh 用 uv 管理的 Python 3.12
python3 -W ignore -m unittest tests.test_e2e_nginx tests.test_e2e_engine tests.test_install_sandbox
