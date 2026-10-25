"""config.yml：所有設定都在這一個檔案（DSM 帳密、模型端點、搜尋門檻…）。

檔案裡有密碼與 API key，所以權限必須是 600（只有擁有者能讀）。
使用者看到的欄位見 data/config.example.yml；這裡把它轉成程式內部用的結構。
"""

import copy
import os
import stat

import yaml

DEFAULTS = {
    "synology": {
        "account": "",
        "password": "",
        "url": "https://127.0.0.1:5001",
        "verify_tls": False,
        "timeout": 60,
    },
    "embedding": {
        "base_url": "",
        "api_key": "",
        "model": "embeddinggemma-2",
        "media_marker": "<__media__>",
        "image_size": "m",
        "timeout": 120,
    },
    "ocr": {
        "enabled": True,
        "base_url": "",
        "api_key": "",
        "model": "",
        "image_size": "xl",
        "max_tokens": 4096,
        "extra_body": {},
        "text_embedding": True,
        "timeout": 120,
    },
    "search": {
        "min_score": 0.71,
        "max_drop": 0.08,
        "text_min_score": 0.75,
        "text_max_drop": 0.08,
        "max_results": 500,
        "dims": 512,
        "ocr_match": True,
        "refresh_interval": 15,
        "query_cache": 2048,
        "socket": "/run/sse-engine/engine.sock",
    },
    "index": {
        "page_size": 500,
        "quick_scan_interval": 600,
        "full_scan_interval": 21600,
        "max_tries": 3,
        "yield_to_search": 15,
        "auto_compact": True,
    },
    "filter": {
        "mode": "engine",
        "max_inject": 500,
    },
    "log": {
        "keep_days": 3,
    },
}

IMAGE_SIZES = ("sm", "m", "xl")


class ConfigError(Exception):
    pass


def _merge(base, user, path):
    if user is None:
        user = {}
    if not isinstance(user, dict):
        raise ConfigError("%s 必須是一個區塊（key: value）" % (path or "最外層"))
    unknown = sorted(set(user) - set(base))
    if unknown:
        raise ConfigError("不認得的設定：%s" % ", ".join(path + k for k in unknown))
    out = {}
    for k, default in base.items():
        v = user.get(k, default)
        if isinstance(default, dict) and k != "extra_body":
            out[k] = _merge(default, user.get(k), path + k + ".")
            continue
        if v is None:
            v = default
        if isinstance(default, bool):
            if not isinstance(v, bool):
                raise ConfigError("%s%s 必須是 true 或 false" % (path, k))
        elif isinstance(default, (int, float)):
            if isinstance(v, bool) or not isinstance(v, (int, float)):
                raise ConfigError("%s%s 必須是數字" % (path, k))
            if isinstance(default, int) and not isinstance(default, bool) and not isinstance(v, int):
                raise ConfigError("%s%s 必須是整數" % (path, k))
        elif isinstance(default, str):
            if isinstance(v, (int, float)) and not isinstance(v, bool):
                v = str(v)   # 例如純數字的密碼
            if not isinstance(v, str):
                raise ConfigError("%s%s 必須是文字" % (path, k))
        elif isinstance(default, dict) and not isinstance(v, dict):
            raise ConfigError("%s%s 必須是一個區塊" % (path, k))
        out[k] = v
    return out


def check_permissions(path):
    st = os.stat(path)
    if st.st_mode & (stat.S_IRWXG | stat.S_IRWXO):
        raise ConfigError("%s 裡有密碼，權限不能讓其他人讀（目前 %o），請執行：chmod 600 %s"
                          % (path, st.st_mode & 0o777, path))


def read(path):
    """讀 config.yml 並檢查，回傳使用者的設定（跟 config.yml 相同的結構，補上預設值）。"""
    try:
        with open(path, encoding="utf-8") as f:
            user = yaml.safe_load(f)
    except OSError as e:
        raise ConfigError("讀不到設定檔 %s：%s" % (path, e))
    except yaml.YAMLError as e:
        raise ConfigError("設定檔格式錯誤：%s" % e)
    c = _merge(DEFAULTS, user, "")
    s, e, o = c["synology"], c["embedding"], c["ocr"]
    if not s["account"]:
        raise ConfigError("synology.account 沒有填（照片擁有者的 DSM 帳號）")
    if not s["password"]:
        raise ConfigError("synology.password 沒有填")
    if not e["base_url"]:
        raise ConfigError("embedding.base_url 沒有填（embedding 服務的網址，例如 http://192.168.1.10:8080/v1）")
    for k, v in (("embedding.image_size", e["image_size"]), ("ocr.image_size", o["image_size"])):
        if v not in IMAGE_SIZES:
            raise ConfigError("%s 只能是 %s" % (k, "、".join(IMAGE_SIZES)))
    if o["enabled"] and not o["model"]:
        raise ConfigError("ocr.model 沒有填（要能看圖的模型）；不用 OCR 的話把 ocr.enabled 設成 false")
    if c["search"]["dims"] not in (128, 256, 512, 768):
        raise ConfigError("search.dims 只能是 128、256、512、768")
    if c["filter"]["mode"] not in ("engine", "passthrough"):
        raise ConfigError("filter.mode 只能是 engine 或 passthrough")
    if not 1 <= c["log"]["keep_days"] <= 365:
        raise ConfigError("log.keep_days 必須是 1～365")
    return c


def load(path, check_perms=True):
    """讀 config.yml，回傳程式內部用的結構。資料庫與 log 放在 config.yml 所在的目錄底下。"""
    path = os.path.abspath(os.path.expanduser(path))
    if check_perms:
        try:
            check_permissions(path)
        except FileNotFoundError:
            raise ConfigError("找不到設定檔 %s" % path)
    c = read(path)
    data_dir = os.path.dirname(path)
    s, e, o = c["synology"], c["embedding"], c["ocr"]
    return {
        "path": path,
        "data_dir": data_dir,
        "user": copy.deepcopy(c),
        "synology": {
            "url": s["url"], "account": s["account"], "password": s["password"], "verify_tls": s["verify_tls"],
            "timeout": s["timeout"], "thumb_size": e["image_size"], "ocr_thumb_size": o["image_size"],
        },
        "llm": {
            "embed_base_url": e["base_url"], "embed_api_key": e["api_key"], "embed_model": e["model"],
            "media_marker": e["media_marker"], "embed_timeout": e["timeout"],
            # OCR 沒填網址就跟 embedding 用同一個服務（連 API key 一起沿用）
            "ocr_base_url": o["base_url"] or e["base_url"],
            "ocr_api_key": o["api_key"] if o["base_url"] else (o["api_key"] or e["api_key"]),
            "ocr_model": o["model"], "ocr_extra_body": o["extra_body"], "ocr_max_tokens": o["max_tokens"],
            "ocr_timeout": o["timeout"],
        },
        "index": dict(c["index"], db=os.path.join(data_dir, "data", "index.db"),
                      ocr="all" if o["enabled"] else "off", ocr_embed=o["enabled"] and o["text_embedding"]),
        "search": dict(c["search"]),
        "log": {"dir": os.path.join(data_dir, "logs"), "keep_days": c["log"]["keep_days"]},
        "filter": dict(c["filter"]),
    }
