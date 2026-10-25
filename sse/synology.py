"""Synology Photos WebAPI client：只用讀取類 API。

用到的 API：
  SYNO.API.Auth            login / logout
  SYNO.Foto.UserInfo       me（確認索引的是哪個使用者）
  SYNO.Foto.Browse.Item    list（完整掃描）
  SYNO.Foto.Browse.RecentlyAdded  list（快速掃描新照片）
  SYNO.Foto.Thumbnail      get（只抓縮圖，不抓原檔）
"""

import json
import ssl
import threading
import urllib.error
import urllib.parse
import urllib.request

# session 失效、需要重新登入的錯誤碼
SESSION_ERRORS = {105, 106, 107, 119}
LOGIN_ERRORS = {
    400: "帳號或密碼錯誤",
    401: "帳號已停用",
    402: "沒有權限",
    403: "需要兩步驟驗證（請改用沒有開 2FA 的帳號，或關閉 2FA）",
    404: "兩步驟驗證碼錯誤",
    407: "IP 被封鎖（登入失敗太多次）",
}


MAX_IMAGE = 12 * 1024 * 1024


class SynologyError(Exception):
    """transient=True：整體性的問題（session、登入），不是某張照片的問題。"""

    def __init__(self, api, method, code, detail=None, transient=None):
        self.code = code
        self.transient = (code in SESSION_ERRORS or code in LOGIN_ERRORS) if transient is None else transient
        msg = "%s.%s 失敗（錯誤碼 %s）" % (api, method, code)
        if detail:
            msg += "：" + detail
        super().__init__(msg)


def image_mime(data):
    if data[:3] == b"\xff\xd8\xff":
        return "image/jpeg"
    if data[:8] == b"\x89PNG\r\n\x1a\n":
        return "image/png"
    if data[:6] in (b"GIF87a", b"GIF89a"):
        return "image/gif"   # 動圖：DSM 直接回原檔，不是縮圖
    if data[:4] == b"RIFF" and data[8:12] == b"WEBP":
        return "image/webp"
    return None


class Client:
    def __init__(self, url, account, password, verify_tls=True, timeout=60):
        self.url = url.rstrip("/")
        self.account = account
        self._password = password
        self.timeout = timeout
        self.sid = None
        self._lock = threading.Lock()
        if verify_tls:
            self._ctx = ssl.create_default_context()
        else:
            self._ctx = ssl.create_default_context()
            self._ctx.check_hostname = False
            self._ctx.verify_mode = ssl.CERT_NONE

    def _post(self, params, raw=False):
        body = urllib.parse.urlencode(params).encode()
        req = urllib.request.Request(self.url + "/webapi/entry.cgi", body,
                                     {"Content-Type": "application/x-www-form-urlencoded"})
        try:
            with urllib.request.urlopen(req, timeout=self.timeout, context=self._ctx) as r:
                data = r.read()
                ctype = r.headers.get("Content-Type", "")
        except urllib.error.HTTPError as e:
            # 4xx（例如某部影片沒有縮圖時的 404）是這個請求的問題；5xx 與 429 才是 DSM 暫時有狀況
            raise SynologyError(params.get("api"), params.get("method"), None, "HTTP %d" % e.code,
                                transient=e.code >= 500 or e.code == 429)
        if raw and not ctype.startswith("application/json"):
            return data
        return json.loads(data.decode("utf-8"))

    def login(self):
        with self._lock:
            d = self._post({"api": "SYNO.API.Auth", "version": "6", "method": "login", "account": self.account,
                            "passwd": self._password, "session": "SynologyPhotos", "format": "sid"})
            if not d.get("success"):
                code = (d.get("error") or {}).get("code")
                raise SynologyError("SYNO.API.Auth", "login", code, LOGIN_ERRORS.get(code))
            self.sid = d["data"]["sid"]

    def logout(self):
        if self.sid:
            try:
                self._post({"api": "SYNO.API.Auth", "version": "6", "method": "logout", "session": "SynologyPhotos",
                            "_sid": self.sid})
            except Exception:
                pass
            self.sid = None

    def call(self, api, version, method, raw=False, **params):
        p = {"api": api, "version": str(version), "method": method}
        for k, v in params.items():
            p[k] = v if isinstance(v, str) else json.dumps(v, separators=(",", ":"))
        for attempt in (1, 2):
            if not self.sid:
                self.login()
            d = self._post(dict(p, _sid=self.sid), raw=raw)
            if isinstance(d, bytes):
                return d
            if d.get("success"):
                return d.get("data")
            code = (d.get("error") or {}).get("code")
            if code in SESSION_ERRORS and attempt == 1:
                self.sid = None
                continue
            raise SynologyError(api, method, code)

    # ── 實際用到的 API ──

    def me(self):
        d = self.call("SYNO.Foto.UserInfo", 1, "me")
        uid = d.get("id", d.get("user_id"))
        if not isinstance(uid, int):
            raise SynologyError("SYNO.Foto.UserInfo", "me", None, "回應裡沒有使用者 id：%s" % json.dumps(d)[:200])
        return uid, d.get("name") or self.account

    def list_items(self, offset, limit):
        d = self.call("SYNO.Foto.Browse.Item", 1, "list", offset=offset, limit=limit,
                      additional=["thumbnail"], sort_by="takentime", sort_direction="desc")
        return d.get("list", [])

    def count_items(self):
        return self.call("SYNO.Foto.Browse.Item", 1, "count").get("count")

    def list_recent(self, offset, limit):
        d = self.call("SYNO.Foto.Browse.RecentlyAdded", 1, "list", offset=offset, limit=limit,
                      additional=["thumbnail"])
        return d.get("list", [])

    def thumbnail(self, unit_id, cache_key, size):
        """回傳 (圖片, MIME)。格式不認得、太大都算這張照片的問題（不是服務問題）。"""
        data = self.call("SYNO.Foto.Thumbnail", 2, "get", raw=True, id=unit_id, cache_key=cache_key, type="unit",
                         size=size)
        if not isinstance(data, bytes):
            raise SynologyError("SYNO.Foto.Thumbnail", "get", None, "回應不是圖片", transient=False)
        mime = image_mime(data)
        if mime is None:
            raise SynologyError("SYNO.Foto.Thumbnail", "get", None, "不認得的圖片格式（%r）" % data[:8], transient=False)
        if len(data) > MAX_IMAGE:
            raise SynologyError("SYNO.Foto.Thumbnail", "get", None, "圖片太大（%d bytes）" % len(data), transient=False)
        return data, mime


def parse_item(raw):
    """把 Browse.Item.list 的一筆轉成索引需要的欄位；格式不對就回傳 None。"""
    try:
        uid = raw["id"]
        thumb = raw["additional"]["thumbnail"]
        ck = thumb["cache_key"]
        if not (isinstance(uid, int) and isinstance(raw["time"], int) and isinstance(ck, str)):
            return None
        return {"unit_id": uid, "owner_user_id": int(raw["owner_user_id"]), "time": raw["time"],
                "type": str(raw.get("type", "photo")), "cache_key": ck,
                "thumb_ready": thumb.get("xl") == "ready" or thumb.get("m") == "ready"}
    except (KeyError, TypeError, ValueError):
        return None
