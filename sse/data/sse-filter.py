#!/usr/bin/env python3
# synology-search-enhancement — SCGI filter（坐在 nginx 與 synoscgi 之間）
#
#   nginx ──scgi──▶ /run/sse/filter.sock（本程式）──▶ /run/synoscgi.sock
#
# 安全原則：
#   1. request 一個 byte 都不改，邊收邊轉，不整包緩衝（上傳、下載不受影響）。
#   2. 只有 SYNO.Foto.Search.Search 的回應可能被改寫，且有大小與時間上限；超過就原樣轉發。
#   3. 任何解析或注入錯誤都原樣轉發（fail-open）。
#   4. 本程式整個掛掉或沒啟動時，nginx 的 backup upstream 直接連 synoscgi（由 sse install 設定）。
#   5. 不連任何資料庫、不寫任何 Synology 檔案；log 不記 cookie、request body 或搜尋關鍵字。
#   6. engine 模式下，要注入的照片一律先用「搜尋者自己的 session」向 DSM 取一次（Browse.Item.get）：
#      DSM 沒回的就不注入，所以權限完全由 DSM 判斷，其他帳號搜尋不會看到別人的照片。
#
# 只用 Python 3.8 標準庫（DSM 7 內建）。設定檔欄位見 DEFAULTS。

import argparse
import asyncio
import collections
import datetime
import hashlib
import json
import logging
import logging.handlers
import os
import re
import signal
import socket
import stat
import sys
import time
from urllib.parse import parse_qs, urlencode

VERSION = "0.4.0"
CHUNK = 65536

DEFAULTS = {
    "listen": "/run/sse/filter.sock",
    "socket_mode": "0660",
    "upstream": "/run/synoscgi.sock",
    "status_file": "/run/sse/status.json",
    # 這個檔案存在時立即停止注入（不必重啟），由 `sse disable` 建立
    "kill_switch": "/usr/local/etc/sse/DISABLE",
    "photos_info": "/var/packages/SynologyPhotos/INFO",
    # passthrough：只轉發；demo：注入設定檔裡固定的 item（驗證用）；engine：向 sse-engine 查詢
    "mode": "passthrough",
    # 啟用注入時記錄的 Photos 版本；Photos 更新後回應格式可能改變，版本不符就自動退回純轉發
    "inject_photos_version": None,
    "inspect_max_header": 262144,
    "inspect_max_body": 65536,
    "inject_max_response": 4194304,
    "inject_timeout": 15,
    "upstream_connect_timeout": 5,
    "idle_timeout": 3600,
    "demo": None,
    "engine_socket": "/run/sse-engine/engine.sock",
    "engine_timeout": 5,
    "subrequest_timeout": 10,
    # Browse.Item.get 的版本：v1、v2 不接受手機 App 要的額外欄位（motion_photo、mobile_cache_mtime…），
    # v3～v7 都接受，回傳內容與原生 list_item 逐欄相同（Photos 1.9.0 實測）
    "item_get_version": 4,
    "max_inject": 500,
    "cache_ttl": 60,
    # 診斷用：把搜尋 API 的請求參數與回應記到檔案（不含 cookie 與 _sid），記滿 capture_max 筆自動停止
    "capture_file": None,
    "capture_max": 200,
    # log 檔：每天午夜換新檔，只留 log_keep_days 天內的紀錄（不設就寫到 stderr / journal）
    "log_file": None,
    "log_keep_days": 3,
}

SEARCH_API = "SYNO.Foto.Search.Search"
CAPTURE_MAX_LINE = 262144


def summarize_response(resp):
    """診斷錄製用：大回應只留結構與照片 id。"""
    if isinstance(resp, dict):
        data = resp.get("data")
        if isinstance(data, dict) and isinstance(data.get("list"), list):
            lst = data["list"]
            return {"success": resp.get("success"), "count": len(lst),
                    "ids": [x.get("id") for x in lst[:1000] if isinstance(x, dict)]}
        return {"success": resp.get("success"), "keys": sorted(data) if isinstance(data, dict) else None}
    return str(resp)[:2000]


# 診斷錄製時不記的 API（量大、跟搜尋無關）
CAPTURE_SKIP_APIS = ("SYNO.Foto.Thumbnail", "SYNO.FotoTeam.Thumbnail", "SYNO.Foto.Download", "SYNO.FotoTeam.Download",
                     "SYNO.Foto.Upload", "SYNO.FotoTeam.Upload", "SYNO.Foto.Streaming", "SYNO.FotoTeam.Streaming")
INJECT_METHODS = ("list_item", "list_basic_timeline_info", "get_search_timeline", "count_item")
AUTH_PARAMS = ("_sid", "SynoToken")
ENTRY_RE = re.compile(rb"/webapi/entry\.cgi(?:/|\?|$)")
# 子請求不沿用這些 header：內容、長度、路徑都是新的；不要壓縮，才能直接解析
SUB_DROP = {b"CONTENT_LENGTH", b"CONTENT_TYPE", b"HTTP_CONTENT_LENGTH", b"HTTP_CONTENT_TYPE", b"QUERY_STRING",
            b"REQUEST_URI", b"DOCUMENT_URI", b"SCRIPT_NAME", b"PATH_INFO", b"REQUEST_METHOD",
            b"HTTP_ACCEPT_ENCODING", b"HTTP_TRANSFER_ENCODING", b"HTTP_EXPECT"}

log = logging.getLogger("sse-filter")

Plan = collections.namedtuple("Plan", "method keyword params qparams bparams headers")


class SkipInject(Exception):
    """放棄注入、原樣轉發（不算錯誤）。"""


# ── 設定 ────────────────────────────────────────────────────────────────

def validate_demo(demo):
    if not isinstance(demo, dict):
        raise ValueError("mode=demo 需要 demo 區塊")
    kw = demo.get("keyword")
    if not isinstance(kw, str) or not kw.strip():
        raise ValueError("demo.keyword 必須是非空字串")
    items = demo.get("items")
    if not isinstance(items, list) or not items:
        raise ValueError("demo.items 必須是非空陣列")
    for it in items:
        if not isinstance(it, dict):
            raise ValueError("demo.items 的每一筆都必須是物件")
        for k in ("id", "owner_user_id", "time"):
            if not isinstance(it.get(k), int) or isinstance(it.get(k), bool):
                raise ValueError("demo.items 的每一筆都需要整數欄位 %s" % k)


def load_config(path):
    """回傳 (cfg, error)。設定有問題時退回純轉發，而不是讓程式起不來。"""
    cfg = dict(DEFAULTS)
    if not path:
        return cfg, None
    try:
        with open(path, encoding="utf-8") as f:
            user = json.load(f)
        if not isinstance(user, dict):
            raise ValueError("設定檔最外層必須是物件")
    except (OSError, ValueError) as e:
        return cfg, "讀不到設定檔，改用預設值純轉發：%s" % e
    unknown = sorted(set(user) - set(DEFAULTS))
    for k in DEFAULTS:
        if k in user:
            cfg[k] = user[k]
    try:
        if unknown:
            raise ValueError("未知的設定欄位：%s" % ", ".join(unknown))
        if cfg["mode"] not in ("passthrough", "demo", "engine"):
            raise ValueError("mode 只能是 passthrough、demo 或 engine")
        int(str(cfg["socket_mode"]), 8)
        if cfg["mode"] == "demo":
            validate_demo(cfg["demo"])
        if cfg["mode"] == "engine" and not isinstance(cfg["engine_socket"], str):
            raise ValueError("mode=engine 需要 engine_socket")
        for k in ("engine_timeout", "subrequest_timeout", "max_inject", "cache_ttl", "capture_max", "item_get_version"):
            if not isinstance(cfg[k], (int, float)) or isinstance(cfg[k], bool) or cfg[k] <= 0:
                raise ValueError("%s 必須是正數" % k)
        if cfg["log_file"] is not None and not isinstance(cfg["log_file"], str):
            raise ValueError("log_file 必須是路徑字串")
        if not isinstance(cfg["log_keep_days"], int) or isinstance(cfg["log_keep_days"], bool) or \
                not 1 <= cfg["log_keep_days"] <= 365:
            raise ValueError("log_keep_days 必須是 1～365 的整數")
    except ValueError as e:
        cfg["mode"], cfg["demo"] = "passthrough", None
        return cfg, "設定檔有誤，改為純轉發：%s" % e
    return cfg, None


# ── log 檔 ──────────────────────────────────────────────────────────────

def prune_logs(path, keep_days, today=None):
    """刪掉 keep_days 天以前的舊檔（檔名 <path>.YYYY-MM-DD）。
    程式停了好幾天才重啟時不會有滾動，所以啟動時也要自己清一次。"""
    d, base = os.path.split(path)
    today = today or datetime.date.today()
    oldest = today - datetime.timedelta(days=keep_days - 1)
    try:
        names = os.listdir(d)
    except OSError:
        return
    for name in names:
        if not name.startswith(base + "."):
            continue
        p = os.path.join(d, name)
        try:
            day = datetime.datetime.strptime(name[len(base) + 1:], "%Y-%m-%d").date()
            old = day < oldest
        except ValueError:
            try:
                old = os.path.getmtime(p) < time.time() - keep_days * 86400
            except OSError:
                continue
        if old:
            try:
                os.unlink(p)
            except OSError:
                pass


class DailyLog(logging.handlers.TimedRotatingFileHandler):
    """每天午夜換新檔，只留今天與前 keep_days - 1 天：最舊的紀錄不會超過 keep_days 天。"""

    def __init__(self, path, keep_days):
        super().__init__(path, when="midnight", backupCount=0, encoding="utf-8")
        self.keep_days = keep_days
        prune_logs(path, keep_days)

    def doRollover(self):
        super().doRollover()
        prune_logs(self.baseFilename, self.keep_days)


def setup_file_logging(logger, path, keep_days, fmt="%(asctime)s %(levelname)s %(message)s"):
    """把 logger 改成寫到 path。失敗就保留原本的輸出，回傳錯誤訊息。"""
    try:
        os.makedirs(os.path.dirname(path), exist_ok=True)
        handler = DailyLog(path, keep_days)
    except OSError as e:
        return "沒辦法寫 log 檔 %s，改寫到 stderr：%s" % (path, e)
    handler.setFormatter(logging.Formatter(fmt))
    for old in list(logger.handlers):
        logger.removeHandler(old)
    logger.addHandler(handler)
    return None


def read_photos_version(path):
    try:
        with open(path, encoding="utf-8", errors="replace") as f:
            for line in f:
                if line.startswith("version="):
                    return line.split("=", 1)[1].strip().strip('"')
    except OSError:
        pass
    return None


# ── 解析 request（只看副本，不影響轉發） ────────────────────────────────

def normalize_keyword(v):
    if v is None:
        return ""
    v = v.strip()
    if len(v) >= 2 and v[0] == v[-1] == '"':
        try:
            j = json.loads(v)
            v = j if isinstance(j, str) else v[1:-1]
        except ValueError:
            v = v[1:-1]
    elif len(v) >= 2 and v[0] == v[-1] == "'":
        v = v[1:-1]
    return v.strip()


def _first_values(qs):
    out = {}
    for k, vs in parse_qs(qs, keep_blank_values=True).items():
        out[k] = vs[0]
    return out


class Sniffer:
    """邊轉發邊解析 SCGI request 的副本，只用來判斷要不要檢查回應。

    任何看不懂的格式都放棄解析（skip），轉發照常進行。
    """

    def __init__(self, max_header, max_body):
        self.max_header = max_header
        self.max_body = max_body
        self.buf = bytearray()
        self.state = "len"
        self.hlen = 0
        self.clen = 0
        self.headers = None
        self.params = None
        self.qparams = None
        self.bparams = None
        self.skip_reason = None

    def feed(self, data):
        if self.state in ("done", "skip"):
            return
        self.buf += data
        try:
            self._advance()
        except (ValueError, IndexError) as e:
            self._skip("解析失敗：%s" % e)

    def _skip(self, reason=None):
        self.state = "skip"
        self.skip_reason = self.skip_reason or reason
        self.buf = bytearray()

    def _advance(self):
        if self.state == "len":
            i = self.buf.find(b":")
            if i < 0:
                if len(self.buf) > 10:
                    raise ValueError("netstring 長度欄位過長")
                return
            n = bytes(self.buf[:i])
            if not n.isdigit():
                raise ValueError("netstring 長度不是數字")
            self.hlen = int(n)
            if self.hlen > self.max_header:
                raise ValueError("header 太大")
            del self.buf[:i + 1]
            self.state = "head"
        if self.state == "head":
            if len(self.buf) < self.hlen + 1:
                return
            if self.buf[self.hlen] != 0x2C:  # ','
                raise ValueError("netstring 結尾不是逗號")
            parts = bytes(self.buf[:self.hlen]).split(b"\0")
            del self.buf[:self.hlen + 1]
            if len(parts) % 2 != 1 or parts[-1] != b"":
                raise ValueError("header 格式錯誤")
            self.headers = collections.OrderedDict((parts[i], parts[i + 1]) for i in range(0, len(parts) - 1, 2))
            if not self._candidate():
                self._skip()
                return
            self.state = "body"
        if self.state == "body":
            if len(self.buf) < self.clen:
                return
            body = bytes(self.buf[:self.clen])
            self.buf = bytearray()
            self.state = "done"
            self.qparams = _first_values(self.headers.get(b"QUERY_STRING", b"").decode("utf-8", "replace"))
            self.bparams = _first_values(body.decode("utf-8", "replace")) if body else {}
            self.params = dict(self.qparams)
            self.params.update(self.bparams)

    def entry_cgi(self):
        uri = (self.headers or {}).get(b"REQUEST_URI") or (self.headers or {}).get(b"SCRIPT_NAME") or b""
        return bool(ENTRY_RE.search(uri.split(b"?", 1)[0] + b"?"))

    def _candidate(self):
        h = self.headers
        try:
            self.clen = int(h.get(b"CONTENT_LENGTH", b"0") or b"0")
        except ValueError:
            self.skip_reason = "CONTENT_LENGTH 不是數字"
            return False
        if not self.entry_cgi():
            self.skip_reason = "不是 entry.cgi"
            return False
        if self.clen < 0 or self.clen > self.max_body:
            self.skip_reason = "body 太大（%d bytes）" % self.clen
            return False
        if h.get(b"REQUEST_METHOD") not in (b"GET", b"POST"):
            self.skip_reason = "HTTP 方法 %s" % h.get(b"REQUEST_METHOD", b"").decode("ascii", "replace")
            return False
        ctype = h.get(b"CONTENT_TYPE", b"").lower()
        if self.clen and not ctype.startswith(b"application/x-www-form-urlencoded"):
            self.skip_reason = "Content-Type %s" % ctype.decode("ascii", "replace")
            return False
        return True

    def plan(self):
        if self.state != "done" or self.params is None:
            return None
        p = self.params
        if p.get("api") != SEARCH_API or p.get("method") not in INJECT_METHODS:
            return None
        kw = normalize_keyword(p.get("keyword"))
        if not kw:
            return None
        return Plan(p["method"], kw, p, self.qparams, self.bparams, self.headers)


# ── 注入（純函式，方便測試） ─────────────────────────────────────────────

def _to_int(v, default):
    try:
        return int(v)
    except (TypeError, ValueError):
        return default


def utc_day(ts):
    d = datetime.datetime.utcfromtimestamp(ts)
    return d.year, d.month, d.day


def inject_response(method, params, data, items):
    """demo 模式：把固定的 items 併進原生搜尋回應。回傳修改後的 data，不需要改就回傳 None。"""
    if not isinstance(data, dict) or data.get("success") is not True:
        return None
    body = data.get("data")
    if not isinstance(body, dict):
        return None
    if method == "count_item":
        c = body.get("count")
        if not isinstance(c, int) or isinstance(c, bool):
            return None
        body["count"] = c + len(items)
        return data
    # 只注入第一頁，避免往下捲時每一頁都重複出現
    if _to_int(params.get("offset"), 0) != 0:
        return None
    if method in ("list_item", "list_basic_timeline_info"):
        lst = body.get("list")
        if not isinstance(lst, list):
            return None
        existing = {x.get("id") for x in lst if isinstance(x, dict)}
        new = [it for it in items if it["id"] not in existing]
        if not new:
            return None
        if method == "list_basic_timeline_info":
            new = [{"id": it["id"], "owner_user_id": it["owner_user_id"], "time": it["time"]} for it in new]
        body["list"] = new + lst
        return data
    if method == "get_search_timeline":
        sec = body.get("section")
        if not isinstance(sec, list):
            return None
        days = collections.Counter(utc_day(it["time"]) for it in items)
        lst = [{"day": d, "item_count": n, "month": m, "year": y}
               for (y, m, d), n in sorted(days.items(), reverse=True)]
        body["section"] = [{"limit": 100, "list": lst, "offset": 0}] + sec
        return data
    return None


def _basic(x):
    return {"id": x["id"], "owner_user_id": x["owner_user_id"], "time": x["time"]}


def merge_basic(native, ours):
    """原生結果與外掛結果合併成一份清單，id 不重複。

    原生只依時間新到舊排（同時間的順序是它自己的），所以用穩定排序：原生之間的相對順序不變，
    外掛的照片插在同時間的原生照片後面。
    """
    seen = collections.OrderedDict()
    for x in list(native) + list(ours):
        seen.setdefault(x["id"], _basic(x))
    return sorted(seen.values(), key=lambda x: -x["time"])


def apply_time_range(items, params):
    """start_time / end_time（Photos 1.9.0 只有 list_item 與 list_basic_timeline_info 會套用）。
    sort_direction、item_type 原生會忽略，這裡也不處理。"""
    start = _to_int(params.get("start_time"), None)
    end = _to_int(params.get("end_time"), None)
    if start is not None:
        items = [x for x in items if x["time"] >= start]
    if end is not None:
        items = [x for x in items if x["time"] <= end]
    return items


def build_sections(items, unit, chunk=100):
    """依合併後的清單重建 get_search_timeline 的 section（規則由 Photos 1.9.0 的原生回應反推）：

    - 以天（或月）為一組，依清單順序累積，超過 chunk 張就成一段
    - 單一組就超過 chunk 張時自己成段，平均拆成「張數 // chunk」段；
      這些段的 list 都是那一整組（item_count 是整組張數），offset 是該段在組內的起點
    - 遇到這種大組或清單結尾時，還沒滿的部分併入前一段（前一段是大組拆出來的就自成一段）
    - 一般段的 limit 是該段張數，offset 是 0
    """
    groups = []
    for x in items:
        y, m, d = utc_day(x["time"])
        key = (y, m) if unit == "month" else (y, m, d)
        if groups and groups[-1][0] == key:
            groups[-1][1] += 1
        else:
            groups.append([key, 1])
    secs = []   # [是否為大組, [(key, 張數)], limit, offset]
    cur, n = [], 0

    def flush():
        if cur:
            if secs and not secs[-1][0]:
                secs[-1][1].extend(cur)
                secs[-1][2] += sum(c for _, c in cur)
            else:
                secs.append([False, list(cur), sum(c for _, c in cur), 0])
        del cur[:]

    for key, c in groups:
        if c > chunk:
            flush()
            n = 0
            parts = c // chunk
            base, extra = divmod(c, parts)
            off = 0
            for i in range(parts):
                size = base + (1 if i < extra else 0)
                secs.append([True, [(key, c)], size, off])
                off += size
            continue
        cur.append((key, c))
        n += c
        if n > chunk:
            secs.append([False, list(cur), n, 0])
            del cur[:]
            n = 0
    flush()
    out = []
    for _, gs, limit, offset in secs:
        lst = []
        for key, c in gs:
            e = {"item_count": c, "month": key[1], "year": key[0]}
            if unit != "month":
                e = {"day": key[2], "item_count": c, "month": key[1], "year": key[0]}
            lst.append(e)
        out.append({"limit": limit, "list": lst, "offset": offset})
    return out


def page_of(items, params):
    offset = max(0, _to_int(params.get("offset"), 0))
    limit = _to_int(params.get("limit"), None)
    return items[offset:offset + limit] if limit is not None and limit >= 0 else items[offset:]


def parse_cgi_response(raw):
    """synoscgi 的 CGI 回應 → (header 行, JSON)。不是 200、有壓縮或分塊、不是 JSON 就回傳 None（不碰）。"""
    sep = raw.find(b"\r\n\r\n")
    if sep < 0:
        return None
    lines = raw[:sep].split(b"\r\n")
    hdrs = {}
    for ln in lines:
        k, _, v = ln.partition(b":")
        hdrs[k.strip().lower()] = v.strip().lower()
    status = hdrs.get(b"status")
    if status and not status.startswith(b"200"):
        return None
    if hdrs.get(b"content-encoding", b"identity") != b"identity" or b"transfer-encoding" in hdrs:
        return None
    try:
        return lines, json.loads(raw[sep + 4:].decode("utf-8"))
    except ValueError:
        return None


def build_cgi_response(lines, data):
    body = json.dumps(data, ensure_ascii=True, separators=(",", ":")).encode("ascii")
    out = []
    for ln in lines:
        name = ln.partition(b":")[0]
        if name.strip().lower() == b"content-length":
            ln = name + b": " + str(len(body)).encode("ascii")
        out.append(ln)
    return b"\r\n".join(out) + b"\r\n\r\n" + body


def build_scgi(headers, body):
    """SCGI 規定 CONTENT_LENGTH 必須是第一個 header。"""
    h = b"CONTENT_LENGTH\0" + str(len(body)).encode() + b"\0"
    h += b"SCGI\0" + headers.get(b"SCGI", b"1") + b"\0"
    for k, v in headers.items():
        if k not in (b"CONTENT_LENGTH", b"SCGI"):
            h += k + b"\0" + v + b"\0"
    return str(len(h)).encode() + b":" + h + b"," + body


def subrequest_bytes(plan, params):
    """用原請求的身分（cookie、SynoToken、_sid、來源 IP）組一個新的 WebAPI 請求。"""
    headers = collections.OrderedDict((k, v) for k, v in plan.headers.items() if k not in SUB_DROP)
    q = {k: plan.qparams[k] for k in AUTH_PARAMS if k in plan.qparams}
    b = dict(params)
    b.update({k: plan.bparams[k] for k in AUTH_PARAMS if k in plan.bparams})
    body = urlencode(b).encode()
    headers[b"REQUEST_METHOD"] = b"POST"
    headers[b"CONTENT_TYPE"] = b"application/x-www-form-urlencoded"
    headers[b"QUERY_STRING"] = urlencode(q).encode()
    headers[b"REQUEST_URI"] = b"/webapi/entry.cgi" + (b"?" + headers[b"QUERY_STRING"] if q else b"")
    headers[b"DOCUMENT_URI"] = b"/webapi/entry.cgi"
    headers[b"SCRIPT_NAME"] = b"/webapi/entry.cgi"
    return build_scgi(headers, body)


def auth_key(plan):
    h = hashlib.sha256()
    h.update(plan.headers.get(b"HTTP_COOKIE", b""))
    for k in AUTH_PARAMS:
        h.update(b"\0" + plan.params.get(k, "").encode())
    h.update(b"\0" + plan.headers.get(b"HTTP_X_SYNO_TOKEN", b""))
    return h.hexdigest()


class TTLCache:
    def __init__(self, ttl, size=256):
        self.ttl, self.size = ttl, size
        self.data = collections.OrderedDict()

    def get(self, k):
        v = self.data.get(k)
        if v is None:
            return None
        if time.monotonic() - v[0] > self.ttl:
            del self.data[k]
            return None
        return v[1]

    def put(self, k, v):
        self.data[k] = (time.monotonic(), v)
        self.data.move_to_end(k)
        while len(self.data) > self.size:
            self.data.popitem(last=False)


class DemoInjector:
    def __init__(self, demo):
        self.keyword = demo["keyword"].strip().casefold()
        self.items = demo["items"]

    def wants(self, plan):
        return plan.keyword.casefold() == self.keyword

    async def rewrite(self, plan, data):
        return inject_response(plan.method, plan.params, data, self.items)


class EngineInjector:
    """向 sse-engine 查詢，再用搜尋者自己的 session 向 DSM 驗證、取得照片資料。"""

    def __init__(self, flt):
        self.f = flt
        self.cfg = flt.cfg
        self.engine_cache = TTLCache(self.cfg["cache_ttl"])
        self.basic_cache = TTLCache(self.cfg["cache_ttl"])

    def wants(self, plan):
        return True

    async def engine_search(self, keyword, item_type):
        key = (keyword, item_type)
        hit = self.engine_cache.get(key)
        if hit is not None:
            return hit
        r, w = await asyncio.wait_for(asyncio.open_unix_connection(self.cfg["engine_socket"], limit=1 << 22),
                                      self.cfg["engine_timeout"])
        try:
            w.write(json.dumps({"op": "search", "q": keyword, "item_type": item_type}).encode() + b"\n")
            await w.drain()
            line = await asyncio.wait_for(r.readline(), self.cfg["engine_timeout"])
        finally:
            await _close(w)
        resp = json.loads(line)
        if not resp.get("ok"):
            raise SkipInject("engine：%s" % resp.get("error"))
        owner = resp.get("owner_user_id")
        hits = [h for h in resp.get("hits", []) if isinstance(h.get("id"), int) and isinstance(h.get("time"), int)]
        out = (owner, hits[:self.cfg["max_inject"]])
        self.engine_cache.put(key, out)
        return out

    async def call(self, plan, params):
        """對 synoscgi 發一個子請求，回傳 data；失敗丟 SkipInject。"""
        raw = subrequest_bytes(plan, params)
        r, w = await asyncio.wait_for(asyncio.open_unix_connection(self.cfg["upstream"], limit=CHUNK),
                                      self.cfg["upstream_connect_timeout"])
        try:
            w.write(raw)
            await w.drain()
            buf = bytearray()
            deadline = time.monotonic() + self.cfg["subrequest_timeout"]
            while len(buf) <= self.cfg["inject_max_response"]:
                data = await asyncio.wait_for(r.read(CHUNK), max(0.1, deadline - time.monotonic()))
                if not data:
                    break
                buf += data
        finally:
            await _close(w)
        self.f.bump("subrequests")
        parsed = parse_cgi_response(bytes(buf))
        self.f.capture("sub", params, parsed[1] if parsed else bytes(buf[:2000]).decode("utf-8", "replace"))
        if parsed is None:
            raise SkipInject("子請求 %s.%s 回應看不懂" % (params.get("api"), params.get("method")))
        d = parsed[1]
        if not isinstance(d, dict) or d.get("success") is not True or not isinstance(d.get("data"), dict):
            code = (d.get("error") or {}).get("code") if isinstance(d, dict) else None
            raise SkipInject("子請求 %s.%s 失敗（%s）" % (params.get("api"), params.get("method"), code))
        return d["data"]

    async def get_items(self, plan, ids, additional):
        """Browse.Item.get：DSM 用搜尋者的權限回傳照片資料；沒權限、已刪除的不會出現在結果裡。

        一張都取不到時丟 SkipInject：多半是請求本身有問題（例如參數不被接受），
        不能當成「這些照片都被刪了」而回傳空清單。
        """
        out = {}
        last_error = None
        for i in range(0, len(ids), 100):
            chunk = ids[i:i + 100]
            params = {"api": "SYNO.Foto.Browse.Item", "version": str(int(self.cfg["item_get_version"])),
                      "method": "get", "id": json.dumps(chunk)}
            if additional is not None:
                params["additional"] = additional
            try:
                data = await self.call(plan, params)
                got = data.get("list")
            except SkipInject as e:
                last_error = e
                got = []
                if len(chunk) > 1:
                    # 一整批失敗時逐張再試：可能只是其中一張被刪掉
                    for uid in chunk:
                        try:
                            got += (await self.call(plan, dict(params, id=json.dumps([uid])))).get("list") or []
                        except SkipInject as e2:
                            last_error = e2
            for it in got or []:
                if isinstance(it, dict) and isinstance(it.get("id"), int):
                    out[it["id"]] = it
        if ids and not out:
            raise SkipInject("一張照片資料都取不到：%s" % last_error)
        return out

    async def context(self, plan):
        """回傳 (完整的原生結果基本清單, 驗證過的外掛結果)；同一個身分與搜尋字在 cache_ttl 內共用。

        原生的清單不帶時間範圍去取：count_item 與 get_search_timeline 本來就不理會時間範圍，
        list_item 與 list_basic_timeline_info 再自己套用，四個 method 才會來自同一份清單。
        """
        owner, hits = await self.engine_search(plan.keyword, "all")  # 原生忽略 item_type，這裡也一樣
        if not hits:
            return None, []
        key = (auth_key(plan), plan.keyword)
        ctx = self.basic_cache.get(key)
        if ctx is not None:
            return ctx
        got = await self.get_items(plan, [h["id"] for h in hits], None)
        ours = []
        for h in hits:
            it = got.get(h["id"])
            if it is None or it.get("owner_user_id") != owner or not isinstance(it.get("time"), int):
                continue
            ours.append(_basic(it))
        if not ours:
            self.basic_cache.put(key, (None, []))
            return None, []
        params = {k: v for k, v in plan.params.items()
                  if k not in AUTH_PARAMS + ("method", "offset", "limit", "additional", "timeline_group_unit",
                                             "start_time", "end_time")}
        params["method"] = "list_basic_timeline_info"
        native = (await self.call(plan, params)).get("list")
        if not isinstance(native, list) or not all(isinstance(x, dict) and isinstance(x.get("id"), int)
                                                   and isinstance(x.get("time"), int) for x in native):
            raise SkipInject("原生 list_basic_timeline_info 格式看不懂")
        ctx = (native, ours)
        self.basic_cache.put(key, ctx)
        return ctx

    async def rewrite(self, plan, data):
        if not isinstance(data, dict) or data.get("success") is not True or not isinstance(data.get("data"), dict):
            return None
        body = data["data"]
        native, ours = await self.context(plan)
        if not ours:
            return None
        native_ids = {x["id"] for x in native}
        if all(x["id"] in native_ids for x in ours):
            return None  # 原生已經全部找得到，不必改
        merged = merge_basic(native, ours)
        if plan.method in ("list_item", "list_basic_timeline_info"):
            merged = apply_time_range(merged, plan.params)
        if plan.method == "count_item":
            if not isinstance(body.get("count"), int):
                return None
            body["count"] = len(merged)
        elif plan.method == "list_basic_timeline_info":
            body["list"] = merged
        elif plan.method == "get_search_timeline":
            if not isinstance(body.get("section"), list):
                return None
            unit = normalize_keyword(plan.params.get("timeline_group_unit")) or "day"
            body["section"] = build_sections(merged, unit)
        elif plan.method == "list_item":
            if not isinstance(body.get("list"), list):
                return None
            page = [x["id"] for x in page_of(merged, plan.params)]
            # 原生回應裡已經有的照片直接沿用，其餘才向 DSM 取
            got = {x["id"]: x for x in body["list"] if isinstance(x, dict) and isinstance(x.get("id"), int)}
            missing = [i for i in page if i not in got]
            if missing:
                got.update(await self.get_items(plan, missing, plan.params.get("additional")))
            body["list"] = [got[i] for i in page if i in got]
        else:
            return None
        return data


# ── 伺服器 ──────────────────────────────────────────────────────────────

def prepare_socket_path(path):
    os.makedirs(os.path.dirname(path), exist_ok=True)
    try:
        st = os.lstat(path)
    except FileNotFoundError:
        return
    if not stat.S_ISSOCK(st.st_mode):
        raise SystemExit("%s 已存在且不是 socket，拒絕覆蓋" % path)
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(1)
    try:
        s.connect(path)
    except OSError:
        os.unlink(path)  # 上次沒清掉的殘留 socket
        return
    finally:
        s.close()
    raise SystemExit("%s 已有其他程序在監聽，拒絕啟動" % path)


async def _close(writer):
    if writer is None:
        return
    try:
        writer.close()
        await asyncio.wait_for(writer.wait_closed(), 5)
    except Exception:
        pass


class Filter:
    STATS = ("connections", "active", "inspected", "injected", "inject_skipped", "inject_errors",
             "upstream_errors", "client_errors", "subrequests", "engine_errors")

    def __init__(self, cfg, cfg_error=None):
        self.cfg = cfg
        self.stats = dict.fromkeys(self.STATS, 0)
        self.started = time.time()
        self.last_error = None
        self._err_window = (0.0, 0)
        if cfg["mode"] == "demo":
            self.injector = DemoInjector(cfg["demo"])
        elif cfg["mode"] == "engine":
            self.injector = EngineInjector(self)
        else:
            self.injector = None
        self._gate = (-1e9, False, "")
        self.photos_version = None
        self.tasks = set()
        self._dirty = True
        self.captured = 0
        if cfg_error:
            self.note_error(cfg_error)

    def note_error(self, msg):
        self.last_error = {"at": int(time.time()), "message": msg}
        self._dirty = True
        start, n = self._err_window
        now = time.monotonic()
        if now - start > 60:
            start, n = now, 0
        if n < 10:
            log.error(msg)
        elif n == 10:
            log.error("錯誤太多，接下來 60 秒不再逐筆記錄")
        self._err_window = (start, n + 1)

    def bump(self, key, n=1):
        self.stats[key] += n
        self._dirty = True

    def capture(self, kind, params, response):
        path = self.cfg["capture_file"]
        if not path or self.captured >= self.cfg["capture_max"]:
            return
        self.captured += 1
        rec = {"t": int(time.time()), "kind": kind,
               "params": {k: v for k, v in params.items() if k not in AUTH_PARAMS}, "response": response}
        line = json.dumps(rec, ensure_ascii=False)
        if len(line) > CAPTURE_MAX_LINE:
            # 太大就把回應換成摘要（直接截斷字串會讓這一行變成壞掉的 JSON）
            rec["response"] = {"truncated_bytes": len(line), "summary": summarize_response(response)}
            line = json.dumps(rec, ensure_ascii=False)[:CAPTURE_MAX_LINE * 2]
        try:
            fd = os.open(path, os.O_WRONLY | os.O_CREAT | os.O_APPEND, 0o600)
            with os.fdopen(fd, "a", encoding="utf-8") as f:
                f.write(line + "\n")
        except OSError as e:
            log.warning("寫不了診斷紀錄：%s", e)

    def capture_request(self, sniffer):
        """診斷錄製：沒被當成搜尋的 Photos 請求也記下參數（不記回應），用來看用戶端實際送了什麼。"""
        if sniffer.state == "done" and sniffer.params is not None:
            api = sniffer.params.get("api") or ""
            if api.startswith(("SYNO.Foto.", "SYNO.FotoTeam.")) and not api.startswith(CAPTURE_SKIP_APIS):
                self.capture("request", sniffer.params, None)
        elif sniffer.state == "skip" and sniffer.headers is not None and sniffer.entry_cgi():
            h = sniffer.headers
            self.capture("skipped", {}, {
                "reason": sniffer.skip_reason,
                "method": h.get(b"REQUEST_METHOD", b"").decode("ascii", "replace"),
                "path": (h.get(b"REQUEST_URI") or b"").split(b"?", 1)[0].decode("utf-8", "replace"),
                "content_type": h.get(b"CONTENT_TYPE", b"").decode("ascii", "replace"),
                "content_length": h.get(b"CONTENT_LENGTH", b"").decode("ascii", "replace")})

    # 注入開關：模式、緊急開關、Photos 版本，每 5 秒重新確認一次
    def injection_active(self):
        now = time.monotonic()
        if now - self._gate[0] < 5:
            return self._gate[1]
        active, reason = self._check_gate()
        if reason != self._gate[2]:
            (log.info if active else log.warning)("注入狀態：%s", reason)
            self._dirty = True
        self._gate = (now, active, reason)
        return active

    def _check_gate(self):
        if self.injector is None:
            return False, "純轉發模式"
        if os.path.exists(self.cfg["kill_switch"]):
            return False, "緊急開關已開啟（%s）" % self.cfg["kill_switch"]
        self.photos_version = read_photos_version(self.cfg["photos_info"])
        want = self.cfg["inject_photos_version"]
        if not want or self.photos_version != want:
            return False, "Photos 版本 %s 與啟用時的 %s 不同，暫停注入" % (self.photos_version, want)
        return True, "注入中（%s）" % self.cfg["mode"]

    async def handle(self, creader, cwriter):
        task = asyncio.current_task()
        self.tasks.add(task)
        self.bump("connections")
        self.bump("active")
        uwriter = None
        pump = None
        try:
            try:
                ureader, uwriter = await asyncio.wait_for(
                    asyncio.open_unix_connection(self.cfg["upstream"], limit=CHUNK),
                    self.cfg["upstream_connect_timeout"])
            except Exception as e:
                self.bump("upstream_errors")
                self.note_error("連不上 synoscgi：%r" % e)
                return
            sniffer = Sniffer(self.cfg["inspect_max_header"], self.cfg["inspect_max_body"])
            pump = asyncio.ensure_future(self.pump_request(creader, uwriter, sniffer))
            await self.pump_response(ureader, cwriter, sniffer)
        except (ConnectionError, asyncio.TimeoutError, OSError):
            self.bump("client_errors")
        except Exception as e:
            self.bump("client_errors")
            self.note_error("轉發錯誤：%r" % e)
        finally:
            if pump is not None:
                pump.cancel()
            await _close(uwriter)
            await _close(cwriter)
            self.bump("active", -1)
            self.tasks.discard(task)

    async def pump_request(self, creader, uwriter, sniffer):
        try:
            while True:
                data = await creader.read(CHUNK)
                if not data:
                    # 照 nginx 的行為轉達 EOF，不自己多做或少做
                    if uwriter.can_write_eof():
                        uwriter.write_eof()
                    return
                # 先解析副本再轉發：synoscgi 收完 request 才會回應，所以回應到達前一定已解析完
                sniffer.feed(data)
                uwriter.write(data)
                await uwriter.drain()
        except (ConnectionError, OSError):
            return

    async def _read(self, reader, timeout):
        return await asyncio.wait_for(reader.read(CHUNK), timeout)

    async def relay(self, first, ureader, cwriter):
        cwriter.write(first)
        await cwriter.drain()
        while True:
            data = await self._read(ureader, self.cfg["idle_timeout"])
            if not data:
                return
            cwriter.write(data)
            await cwriter.drain()

    async def pump_response(self, ureader, cwriter, sniffer):
        first = await self._read(ureader, self.cfg["idle_timeout"])
        if not first:
            return
        plan = sniffer.plan() if (self.injection_active() or self.cfg["capture_file"]) else None
        if plan is None and self.cfg["capture_file"]:
            self.capture_request(sniffer)
        if plan is None or (not self.cfg["capture_file"] and not self.injector.wants(plan)):
            await self.relay(first, ureader, cwriter)
            return
        self.bump("inspected")
        loop = asyncio.get_event_loop()
        deadline = loop.time() + self.cfg["inject_timeout"]
        buf = bytearray(first)
        eof = False
        try:
            while len(buf) <= self.cfg["inject_max_response"]:
                remaining = deadline - loop.time()
                if remaining <= 0:
                    break
                data = await self._read(ureader, remaining)
                if not data:
                    eof = True
                    break
                buf += data
        except asyncio.TimeoutError:
            pass
        if not eof:  # 回應太大或太慢：放棄注入，已收到的部分原樣送出後繼續轉發
            self.bump("inject_skipped")
            await self.relay(bytes(buf), ureader, cwriter)
            return
        out = await self.try_inject(plan, bytes(buf), deadline)
        cwriter.write(out if out is not None else bytes(buf))
        await cwriter.drain()

    async def try_inject(self, plan, raw, deadline):
        parsed = parse_cgi_response(raw)
        self.capture("native", plan.params, parsed[1] if parsed else raw[:2000].decode("utf-8", "replace"))
        if parsed is None or not self.injection_active() or not self.injector.wants(plan):
            return None
        lines, data = parsed
        try:
            remaining = max(0.1, deadline - asyncio.get_event_loop().time())
            new = await asyncio.wait_for(self.injector.rewrite(plan, data), remaining)
        except SkipInject as e:
            self.bump("inject_skipped")
            log.info("略過注入：%s", e)
            return None
        except (asyncio.TimeoutError, ConnectionError, OSError) as e:
            self.bump("engine_errors")
            self.note_error("注入逾時或連線失敗，原樣轉發：%r" % e)
            return None
        except Exception as e:
            self.bump("inject_errors")
            self.note_error("注入失敗，原樣轉發：%r" % e)
            return None
        if new is None:
            return None
        self.bump("injected")
        return build_cgi_response(lines, new)

    def status(self):
        return {
            "version": VERSION,
            "pid": os.getpid(),
            "started": int(self.started),
            "updated": int(time.time()),
            "mode": self.cfg["mode"],
            "injection_active": self._gate[1],
            "injection_reason": self._gate[2],
            "photos_version": self.photos_version,
            "capture": {"file": self.cfg["capture_file"], "count": self.captured, "max": self.cfg["capture_max"]}
            if self.cfg["capture_file"] else None,
            "stats": dict(self.stats),
            "last_error": self.last_error,
        }

    def write_status(self):
        path = self.cfg["status_file"]
        tmp = path + ".tmp"
        try:
            with open(tmp, "w", encoding="utf-8") as f:
                json.dump(self.status(), f, ensure_ascii=False)
            os.chmod(tmp, 0o644)
            os.replace(tmp, path)
            self._dirty = False
        except OSError as e:
            log.warning("寫不了狀態檔 %s：%s", path, e)

    async def status_loop(self):
        while True:
            self.injection_active()
            if self._dirty:
                self.write_status()
            await asyncio.sleep(1)

    async def serve(self):
        path = self.cfg["listen"]
        prepare_socket_path(path)
        server = await asyncio.start_unix_server(self.handle, path=path, limit=CHUNK, backlog=512)
        os.chmod(path, int(str(self.cfg["socket_mode"]), 8))
        stop = asyncio.Event()
        loop = asyncio.get_event_loop()
        for sig in (signal.SIGTERM, signal.SIGINT):
            loop.add_signal_handler(sig, stop.set)
        log.info("sse-filter %s 啟動：%s → %s（模式 %s）", VERSION, path, self.cfg["upstream"], self.cfg["mode"])
        status_task = asyncio.ensure_future(self.status_loop())
        await stop.wait()
        # 先移除 socket：nginx 會立刻改走 backup 直連 synoscgi，不必等 fail_timeout
        server.close()
        try:
            os.unlink(path)
        except OSError:
            pass
        await server.wait_closed()
        if self.tasks:
            log.info("等待 %d 個進行中的連線結束", len(self.tasks))
            await asyncio.wait(list(self.tasks), timeout=25)
        status_task.cancel()
        self.write_status()
        log.info("sse-filter 已停止")


def main(argv=None):
    ap = argparse.ArgumentParser(description="synology-search-enhancement SCGI filter")
    ap.add_argument("--config", help="設定檔（JSON）")
    args = ap.parse_args(argv)
    logging.basicConfig(stream=sys.stderr, level=logging.INFO, format="%(levelname)s %(message)s")
    cfg, err = load_config(args.config)
    if cfg["log_file"]:
        log_err = setup_file_logging(logging.getLogger(), cfg["log_file"], cfg["log_keep_days"])
        if log_err:
            log.error(log_err)
    asyncio.run(Filter(cfg, err).serve())


if __name__ == "__main__":
    main()
