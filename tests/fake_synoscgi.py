#!/usr/bin/env python3
# 測試用的假 synoscgi：在 unix socket 上說 SCGI，回 CGI 格式的 JSON。
#
#   python3 fake_synoscgi.py <socket> <request-log> [dsm]
#
# 每個 request 的 rid 參數會寫進 request-log 一行，用來檢查 POST 有沒有被送兩次。
# 依參數決定行為：
#   api=SYNO.API.Info                      → {"success":true,...}（給 sse 的健康檢查）
#   api=SYNO.Foto.*（dsm 模式）            → fake_dsm.FakeDSM（帳號、權限、原生搜尋、Browse.Item.get）
#   api=SYNO.Foto.Search.Search（一般模式）→ 固定的原生搜尋回應（只有 id 1）
#   bigresp=N                              → 回 N bytes 的資料
#   sleep=S                                → 延遲 S 秒再回
#   其他                                   → 回 request body 的長度與 sha256
import asyncio
import hashlib
import json
import os
import sys
from urllib.parse import parse_qs

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import fake_dsm  # noqa: E402

SOCK, LOG = sys.argv[1], sys.argv[2]
DSM = None
if len(sys.argv) > 3 and sys.argv[3] in ("dsm", "dsm-engine"):
    DSM = fake_dsm.FakeDSM(fake_dsm.e2e_library() if sys.argv[3] == "dsm" else fake_dsm.engine_library())
    DSM.sessions.update({"sid-alice": 1, "sid-bob": 2})
SMALL = 65536


def params_of(qs):
    return {k: v[0] for k, v in parse_qs(qs, keep_blank_values=True).items()}


def search_body(method):
    if method == "list_item":
        return {"data": {"list": [{"id": 1, "owner_user_id": 1, "time": 1700000000, "type": "photo",
                                   "filename": "native.jpg"}]}, "success": True}
    if method == "list_basic_timeline_info":
        return {"data": {"list": [{"id": 1, "owner_user_id": 1, "time": 1700000000}]}, "success": True}
    if method == "get_search_timeline":
        return {"data": {"section": [{"limit": 100, "list": [{"day": 14, "item_count": 1, "month": 11, "year": 2023}],
                                      "offset": 0}]}, "success": True}
    if method == "count_item":
        return {"data": {"count": 1}, "success": True}
    return {"success": False, "error": {"code": 103}}


def cgi(body, ctype=b"application/json; charset=utf-8"):
    return (b"Status: 200 OK\r\nContent-Type: " + ctype + b"\r\nContent-Length: " +
            str(len(body)).encode() + b"\r\n\r\n" + body)


async def handle(reader, writer):
    try:
        n = b""
        while not n.endswith(b":"):
            c = await reader.readexactly(1)
            n += c
        head = await reader.readexactly(int(n[:-1]))
        assert await reader.readexactly(1) == b","
        parts = head.split(b"\0")
        h = {parts[i]: parts[i + 1] for i in range(0, len(parts) - 1, 2)}
        assert head.startswith(b"CONTENT_LENGTH\0"), "SCGI 規定 CONTENT_LENGTH 必須是第一個 header"
        clen = int(h.get(b"CONTENT_LENGTH", b"0") or b"0")
        sha = hashlib.sha256()
        small = bytearray()
        left = clen
        while left:
            chunk = await reader.read(min(left, 1 << 20))
            if not chunk:
                break
            left -= len(chunk)
            sha.update(chunk)
            if len(small) < SMALL:
                small += chunk[:SMALL - len(small)]
        p = params_of(h.get(b"QUERY_STRING", b"").decode())
        if clen <= SMALL and h.get(b"CONTENT_TYPE", b"").startswith(b"application/x-www-form-urlencoded"):
            p.update(params_of(bytes(small).decode()))
        with open(LOG, "a") as f:
            f.write("%s %s %s.%s\n" % (h.get(b"REQUEST_METHOD", b"").decode(), p.get("rid", "-"), p.get("api"),
                                        p.get("method")))
        if "sleep" in p:
            await asyncio.sleep(float(p["sleep"]))
        if p.get("api") == "SYNO.API.Info":
            out = cgi(json.dumps({"data": {"SYNO.API.Info": {"maxVersion": 1}}, "success": True}).encode())
        elif DSM is not None and (p.get("api") or "").startswith(("SYNO.Foto", "SYNO.API.Auth")):
            ctype, body = DSM.handle(p, fake_dsm.cookies_of(h.get(b"HTTP_COOKIE", b"").decode()))
            out = cgi(body, ctype.encode())
        elif p.get("api") == "SYNO.Foto.Search.Search":
            out = cgi(json.dumps(search_body(p.get("method")), separators=(",", ":")).encode())
        elif "bigresp" in p:
            size = int(p["bigresp"])
            writer.write(b"Status: 200 OK\r\nContent-Type: application/octet-stream\r\nContent-Length: " +
                         str(size).encode() + b"\r\n\r\n")
            block = hashlib.sha256(b"x").digest() * 2048  # 64 KiB
            sent = 0
            while sent < size:
                piece = block[:min(len(block), size - sent)]
                writer.write(piece)
                await writer.drain()
                sent += len(piece)
            out = b""
        else:
            out = cgi(json.dumps({"success": True, "data": {"len": clen - left, "sha256": sha.hexdigest(),
                                                             "rid": p.get("rid")}}).encode())
        writer.write(out)
        await writer.drain()
    except (asyncio.IncompleteReadError, ConnectionError):
        pass
    finally:
        writer.close()


async def main():
    if os.path.exists(SOCK):
        os.unlink(SOCK)
    server = await asyncio.start_unix_server(handle, path=SOCK)
    async with server:
        await server.serve_forever()


asyncio.run(main())
