"""搜尋服務：unix socket，一行 JSON 請求、一行 JSON 回應。給 DSM 端的 sse-filter 呼叫。

  {"op": "ping"}
  {"op": "search", "q": "貓", "item_type": "all"}
  → {"ok": true, "owner_user_id": 1, "hits": [{"id": 1, "time": 1700000000, "type": "photo", ...}]}
"""

import asyncio
import collections
import json
import logging
import os
import signal
import socket
import stat
import time
from concurrent.futures import ThreadPoolExecutor

from . import db as dbm
from .llm import LLMError
from .search import HUB_BANK, QUERY_BANK_PREFIX, Index

log = logging.getLogger("sse.server")
MAX_LINE = 65536


class QueryCache:
    def __init__(self, size):
        self.size = size
        self.data = collections.OrderedDict()

    def get(self, k):
        v = self.data.get(k)
        if v is not None:
            self.data.move_to_end(k)
        return v

    def put(self, k, v):
        self.data[k] = v
        self.data.move_to_end(k)
        while len(self.data) > self.size:
            self.data.popitem(last=False)


class Server:
    def __init__(self, cfg, llm):
        self.cfg = cfg
        self.scfg = cfg["search"]
        self.llm = llm
        self.index = Index(cfg["index"]["db"], self.scfg["dims"])
        self.cache = QueryCache(self.scfg["query_cache"])
        self.pool = ThreadPoolExecutor(4)
        self.stats = collections.Counter()
        self.started = time.time()

    def mark_search(self):
        path = dbm.search_marker(self.cfg["index"]["db"])
        try:
            with open(path, "a"):
                os.utime(path)
        except OSError:
            pass

    async def embed_query(self, q):
        self.mark_search()  # 讓索引器先別送新的圖片，GPU 留給這次查詢
        key = (self.llm.embed_model, q)
        v = self.cache.get(key)
        if v is None:
            self.stats["embed_calls"] += 1
            v = await asyncio.get_running_loop().run_in_executor(self.pool, self.llm.embed_query, q)
            self.cache.put(key, v)
        else:
            self.stats["cache_hits"] += 1
        return v

    async def do_search(self, req):
        q = req.get("q")
        if not isinstance(q, str) or not q.strip() or len(q) > 200:
            return {"ok": False, "error": "q 必須是 1～200 字的字串"}
        t0 = time.monotonic()
        qv = await self.embed_query(q.strip())
        hits = await asyncio.get_running_loop().run_in_executor(
            self.pool, self.index.search, qv, q.strip(), self.scfg, req.get("item_type", "all"))
        self.stats["searches"] += 1
        return {"ok": True, "owner_user_id": self.index.owner, "hits": hits,
                "took_ms": int((time.monotonic() - t0) * 1000)}

    async def handle(self, reader, writer):
        try:
            line = await asyncio.wait_for(reader.readline(), 10)
            if not line or len(line) > MAX_LINE:
                return
            try:
                req = json.loads(line)
                if not isinstance(req, dict):
                    raise ValueError
            except ValueError:
                resp = {"ok": False, "error": "請求不是 JSON 物件"}
            else:
                op = req.get("op")
                if op == "ping":
                    resp = {"ok": True, "owner_user_id": self.index.owner, "items": int(self.index.ids.size),
                            "rev": self.index.rev, "uptime": int(time.time() - self.started), "stats": dict(self.stats)}
                elif op == "search":
                    try:
                        resp = await self.do_search(req)
                    except LLMError as e:
                        self.stats["llm_errors"] += 1
                        resp = {"ok": False, "error": "查詢 embedding 失敗：%s" % e}
                else:
                    resp = {"ok": False, "error": "不支援的 op"}
            writer.write(json.dumps(resp, ensure_ascii=False).encode() + b"\n")
            await writer.drain()
        except Exception as e:
            self.stats["errors"] += 1
            log.error("處理請求失敗：%r", e)
        finally:
            writer.close()

    async def ensure_bank(self):
        """通用查詢的向量（hub 校正用）。LLM 暫時連不上就先不校正，下一輪再試。"""
        if self.index.bank is not None:
            return
        try:
            loop = asyncio.get_running_loop()
            vecs = [await loop.run_in_executor(self.pool, self.llm._embed, QUERY_BANK_PREFIX + q) for q in HUB_BANK]
            await asyncio.get_running_loop().run_in_executor(self.pool, self.index.set_bank, vecs)
            log.info("hub 校正已啟用")
        except LLMError as e:
            log.warning("取不到通用查詢的向量，暫時不做 hub 校正：%s", e)

    async def refresh_loop(self):
        while True:
            await self.ensure_bank()
            try:
                n = await asyncio.get_running_loop().run_in_executor(self.pool, self.index.refresh)
                if n:
                    log.info("索引更新 %d 筆，共 %d 張（rev %d）", n, self.index.ids.size, self.index.rev)
            except Exception as e:
                log.error("讀取索引失敗：%s", e)
            await asyncio.sleep(self.scfg["refresh_interval"])

    async def serve(self):
        path = self.scfg["socket"]
        os.makedirs(os.path.dirname(path), exist_ok=True)
        if os.path.lexists(path):
            if not stat.S_ISSOCK(os.lstat(path).st_mode):
                raise SystemExit("%s 已存在且不是 socket" % path)
            s = socket.socket(socket.AF_UNIX)
            try:
                s.connect(path)
                raise SystemExit("%s 已有其他程序在監聽" % path)
            except OSError:
                os.unlink(path)
            finally:
                s.close()
        await asyncio.get_running_loop().run_in_executor(self.pool, self.index.refresh)
        log.info("載入 %d 張照片的向量（rev %d）", self.index.ids.size, self.index.rev)
        server = await asyncio.start_unix_server(self.handle, path=path, limit=MAX_LINE)
        # 0660：只有服務本身與 nginx 群組（sse-filter）能連
        os.chmod(path, 0o660)
        stop = self.stop = asyncio.Event()
        loop = asyncio.get_running_loop()
        for sig in (signal.SIGTERM, signal.SIGINT):
            try:
                loop.add_signal_handler(sig, stop.set)
            except (RuntimeError, ValueError):
                pass  # 不在主執行緒（測試）時沒辦法接 signal
        self.ready = True
        refresher = asyncio.ensure_future(self.refresh_loop())
        log.info("搜尋服務啟動：%s", path)
        await stop.wait()
        refresher.cancel()
        server.close()
        try:
            os.unlink(path)
        except OSError:
            pass
        log.info("搜尋服務已停止")
