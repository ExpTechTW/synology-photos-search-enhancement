"""測試用的假 DSM / Synology Photos：在記憶體裡模擬帳號、個人空間的照片、原生搜尋與縮圖。

同一份邏輯給兩種前端用：
  - SCGI（fake_synoscgi.py）：放在 nginx + sse-filter 後面
  - HTTP（serve_http）：給 sse-engine 的索引器直接連

假圖片的內容是 b"\\xff\\xd8\\xff" + b"FAKE:<概念>|TEXT:<圖中文字>"，fake_llm 看得懂。
"""

import hashlib
import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs

USERS = {"alice": (1, "pw-alice"), "bob": (2, "pw-bob")}
# 手機 App 的 list_item 會要求的額外欄位；Browse.Item.get 要 v3 以上才接受（Photos 1.9.0 實測）
APP_ONLY_FIELDS = {"description", "tag", "exif", "address", "gps", "geocoding_id", "video_meta", "mobile_cache_mtime",
                   "folder", "rating", "motion_photo"}
APP_ADDITIONAL = json.dumps(["description", "tag", "exif", "resolution", "orientation", "address", "gps", "geocoding_id",
                             "thumbnail", "video_meta", "mobile_cache_mtime", "folder", "rating", "motion_photo"])


def _load_filter():
    import importlib.util
    import os
    path = os.path.join(os.path.dirname(os.path.dirname(os.path.abspath(__file__))), "sse", "data", "sse-filter.py")
    spec = importlib.util.spec_from_file_location("sse_filter_for_fake", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


FILTER = _load_filter()


def fake_image(concept, text=""):
    return b"\xff\xd8\xff" + ("FAKE:%s|TEXT:%s" % (concept, text)).encode()


def make_item(uid, owner, t, concept, text="", tags=(), typ="photo"):
    return {"id": uid, "owner_user_id": owner, "time": t, "type": typ, "filename": "IMG_%d.jpg" % uid,
            "concept": concept, "text": text, "tags": list(tags), "mtime": t + 7}


def params_of(qs):
    return {k: v[0] for k, v in parse_qs(qs, keep_blank_values=True).items()}


def jparam(p, k, default=None):
    v = p.get(k)
    if v is None:
        return default
    try:
        return json.loads(v)
    except ValueError:
        return v


class FakeDSM:
    def __init__(self, items=()):
        self.lock = threading.Lock()
        self.items = {it["id"]: it for it in items}
        self.sessions = {}
        self.calls = []
        self.strict_get = True   # Browse.Item.get 只要有一張沒權限就整批失敗（最嚴格的情況）

    # ── 資料 ──

    def serialize(self, it, additional):
        out = {"filename": it["filename"], "filesize": 1000 + it["id"], "folder_id": 1, "id": it["id"],
               "indexed_time": it["mtime"] * 1000, "owner_user_id": it["owner_user_id"], "time": it["time"],
               "type": it["type"], "additional": {}}
        if "thumbnail" in (additional or []):
            out["additional"]["thumbnail"] = {"cache_key": "%d_%d" % (it["id"], it["mtime"]), "m": "ready",
                                              "preview": "broken", "sm": "ready", "unit_id": it["id"], "xl": "ready"}
        if "resolution" in (additional or []):
            out["additional"]["resolution"] = {"height": 960, "width": 1280}
        for k in sorted(set(additional or []) & APP_ONLY_FIELDS):
            out["additional"][k] = {"fake": k}
        return out

    def owned(self, uid):
        return sorted((it for it in self.items.values() if it["owner_user_id"] == uid),
                      key=lambda x: (-x["time"], -x["id"]))

    def native_search(self, uid, kw, p, time_range):
        """跟 Photos 1.9.0 實測一樣：item_type、sort_direction 不理會；時間範圍只有 list 類的 method 會套用。"""
        kw = kw.lower()
        hits = [it for it in self.owned(uid) if any(kw in t.lower() for t in it["tags"])]
        if time_range and "start_time" in p:
            hits = [it for it in hits if it["time"] >= int(p["start_time"])]
        if time_range and "end_time" in p:
            hits = [it for it in hits if it["time"] <= int(p["end_time"])]
        return hits

    # ── API ──

    def handle(self, p, cookies):
        """回傳 (content-type, body bytes)。"""
        with self.lock:
            self.calls.append((p.get("api"), p.get("method")))
            api, method = p.get("api"), p.get("method")
            if api == "SYNO.API.Info":
                return self.ok({"SYNO.API.Info": {"maxVersion": 1}})
            if api == "SYNO.API.Auth" and method == "login":
                u = USERS.get(p.get("account"))
                if not u or p.get("passwd") != u[1]:
                    return self.err(400)
                sid = hashlib.sha256(("%s%d" % (p["account"], len(self.sessions))).encode()).hexdigest()[:20]
                self.sessions[sid] = u[0]
                return self.ok({"sid": sid})
            if api == "SYNO.API.Auth" and method == "logout":
                return self.ok({})
            sid = p.get("_sid") or cookies.get("id")
            uid = self.sessions.get(sid)
            if api and api.startswith("SYNO.Foto") and uid is None:
                return self.err(119)
            if api == "SYNO.Foto.UserInfo" and method == "me":
                name = [n for n, (i, _) in USERS.items() if i == uid][0]
                return self.ok({"id": uid, "name": name})
            if api == "SYNO.Foto.Browse.Item" and method == "count":
                return self.ok({"count": len(self.owned(uid))})
            if api == "SYNO.Foto.Browse.Item" and method == "list":
                off, lim = int(p.get("offset", 0)), int(p.get("limit", 100))
                return self.ok({"list": [self.serialize(it, jparam(p, "additional", []))
                                         for it in self.owned(uid)[off:off + lim]]})
            if api == "SYNO.Foto.Browse.RecentlyAdded" and method == "list":
                off, lim = int(p.get("offset", 0)), int(p.get("limit", 100))
                items = sorted(self.owned(uid), key=lambda x: -x["id"])
                return self.ok({"list": [self.serialize(it, jparam(p, "additional", []))
                                         for it in items[off:off + lim]]})
            if api == "SYNO.Foto.Browse.Item" and method == "get":
                add = set(jparam(p, "additional", []) or [])
                if "bogus_field" in add or (int(p.get("version", 1)) < 3 and add & APP_ONLY_FIELDS):
                    return self.err(120)   # 跟 Photos 1.9.0 一樣：{"name": "additional", "reason": "condition"}
                ids = jparam(p, "id", [])
                found = [self.items[i] for i in ids if i in self.items and self.items[i]["owner_user_id"] == uid]
                if self.strict_get and len(found) != len(ids):
                    return self.err(803)
                return self.ok({"list": [self.serialize(it, jparam(p, "additional", [])) for it in found]})
            if api == "SYNO.Foto.Thumbnail" and method == "get":
                it = self.items.get(int(p.get("id", 0)))
                if not it or it["owner_user_id"] != uid or jparam(p, "cache_key") != "%d_%d" % (it["id"], it["mtime"]) \
                        or it["concept"] == "broken":
                    return self.err(803)
                if it["concept"] == "nothumb" or (it["concept"] == "heic-no-m" and jparam(p, "size") == "m"):
                    return "http404", b""
                if it["concept"] == "gif":
                    return "image/gif", b"GIF89a" + fake_image("gif")[3:]
                if it["concept"] == "weird":
                    return "image/heic", b"\x00\x00\x00\x18ftypheic"
                return "image/jpeg", fake_image(it["concept"], it["text"])
            if api == "SYNO.Foto.Search.Search":
                if not jparam(p, "keyword"):
                    return self.err(100)
                hits = self.native_search(uid, jparam(p, "keyword", ""), p,
                                          method in ("list_item", "list_basic_timeline_info"))
                if method == "count_item":
                    return self.ok({"count": len(hits)})
                if method == "list_basic_timeline_info":
                    return self.ok({"list": [{"id": it["id"], "owner_user_id": it["owner_user_id"],
                                              "time": it["time"]} for it in hits]})
                if method == "list_item":
                    off, lim = int(p.get("offset", 0)), int(p.get("limit", 100))
                    return self.ok({"list": [self.serialize(it, jparam(p, "additional", []))
                                             for it in hits[off:off + lim]]})
                if method == "get_search_timeline":
                    # 分段規則已用真實 NAS 的 60 組回應驗證過（見 sse-filter.build_sections）
                    unit = jparam(p, "timeline_group_unit", "day")
                    return self.ok({"section": FILTER.build_sections(hits, unit)})
            return self.err(103)

    @staticmethod
    def ok(data):
        return "application/json; charset=utf-8", json.dumps({"data": data, "success": True}).encode()

    @staticmethod
    def err(code):
        return "application/json; charset=utf-8", json.dumps({"error": {"code": code}, "success": False}).encode()


def cookies_of(header):
    out = {}
    for part in (header or "").split(";"):
        k, _, v = part.strip().partition("=")
        if k:
            out[k] = v
    return out


def serve_http(dsm, port=0):
    class H(BaseHTTPRequestHandler):
        def log_message(self, *a):
            pass

        def _do(self, body):
            p = params_of(self.path.split("?", 1)[1] if "?" in self.path else "")
            p.update(params_of(body))
            ctype, out = dsm.handle(p, cookies_of(self.headers.get("Cookie")))
            if ctype == "http404":
                self.send_error(404)
                return
            self.send_response(200)
            self.send_header("Content-Type", ctype)
            self.send_header("Content-Length", str(len(out)))
            self.end_headers()
            self.wfile.write(out)

        def do_GET(self):
            self._do("")

        def do_POST(self):
            n = int(self.headers.get("Content-Length", 0))
            self._do(self.rfile.read(n).decode())

    srv = ThreadingHTTPServer(("127.0.0.1", port), H)
    threading.Thread(target=srv.serve_forever, daemon=True).start()
    return srv


E2E_T0 = 1700000000
# 端對端測試用的照片：alice（id 1）30 張、一天一張；bob（id 2）2 張。
# 原生搜尋只認得 tags：alice 的 2、5 被標了 cat；bob 的 99 也是。
E2E_ENGINE_HITS = {"cat": [2, 3, 7, 11, 20, 25, 99], "dog": [5, 2], "beach": [8],
                   "mixed": [4, 9999, 99, 6]}  # 9999 已被刪除、99 是 bob 的


def e2e_library():
    items = [make_item(i, 1, E2E_T0 + i * 86400, "x", tags=["cat"] if i in (2, 5) else ["dog"] if i in (5,) else ())
             for i in range(1, 31)]
    items.append(make_item(99, 2, E2E_T0 + 99 * 86400, "cat", tags=["cat"]))
    items.append(make_item(98, 2, E2E_T0 + 98 * 86400, "dog"))
    return items


def engine_library():
    """完整流程測試用：alice 有各種內容的照片（原生只認得 tags），bob 有一張貓。"""
    items, uid = [], 100
    for concept, n in (("cat", 5), ("dog", 4), ("beach", 6), ("mountain", 3)):
        for k in range(n):
            uid += 1
            items.append(make_item(uid, 1, E2E_T0 + uid * 3600, concept, tags=["cat"] if (concept == "cat" and k < 2) else ()))
    items.append(make_item(200, 1, E2E_T0 + 200 * 3600, "receipt", "全家便利商店\nAB-38475629\n總計 NT$91"))
    items.append(make_item(201, 1, E2E_T0 + 201 * 3600, "sign", "停車場入口"))
    items.append(make_item(300, 2, E2E_T0, "cat", tags=["cat"]))
    return items
