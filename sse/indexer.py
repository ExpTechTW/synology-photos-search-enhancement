"""索引器：從 Synology Photos 讀縮圖 → embedding / OCR → 寫進外掛資料庫。

只呼叫讀取類 WebAPI，不寫任何 Synology 的資料。資料庫只在這個 process 的主執行緒寫入；
worker 執行緒只負責網路（抓縮圖、呼叫 LLM）。
"""

import collections
import json
import logging
import os
import shutil
import sqlite3
import threading
import time

from . import db as dbm
from .llm import LLMError, to_f16
from .synology import SynologyError, parse_item

log = logging.getLogger("sse.indexer")

# 16×16 的純色 PNG：確認 OCR 服務是否正常時用
PROBE_PNG = (b"\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x10\x00\x00\x00\x10\x08\x02\x00\x00\x00\x90\x91h6"
             b"\x00\x00\x00\x15IDATx\x9cc\xfc\xcf\x80\x1f0\x8e*\x18U0\xaa`\x08\x0b\x00\x9a\xf7\x02\x1e\x81\xc0\x82"
             b"\xac\x00\x00\x00\x00IEND\xaeB`\x82")


class OwnerMismatch(Exception):
    pass


class ServiceDown(Exception):
    """DSM 或 LLM 暫時連不上：整體暫停、稍後再試，不算在個別照片的重試次數裡。"""


def transient(err):
    if isinstance(err, (LLMError, SynologyError)):
        return err.transient
    return isinstance(err, OSError)


class Progress:
    """indexer-status.json（放在資料庫旁邊）：目前在做什麼、處理速度、暫停原因，給 status 指令看。

    處理量的總數在資料庫裡；這裡只記資料庫裡沒有的即時狀態。
    """

    WINDOW = 600        # 用最近 10 分鐘的處理量算速度
    MIN_SPAN = 60       # 至少累積 1 分鐘才算速度

    def __init__(self, path):
        self.path = path
        now = int(time.time())
        self.data = {"pid": os.getpid(), "started": now, "phase": "starting", "phase_since": now,
                     "paused_until": None, "pause_reason": None, "scan_done": None, "scan_total": None,
                     "next_full_scan": None, "next_quick_scan": None, "processed": {"emb": 0, "ocr": 0, "txt": 0}}
        self.samples = {"emb": collections.deque(), "ocr": collections.deque(), "txt": collections.deque()}
        self.last_write = 0.0

    def set(self, phase, **kv):
        if phase != self.data["phase"]:
            self.data["phase"], self.data["phase_since"] = phase, int(time.time())
        if phase != "paused":
            self.data["paused_until"] = self.data["pause_reason"] = None
        if phase != "full_scan":
            self.data["scan_done"] = self.data["scan_total"] = None
        self.data.update(kv)
        self.write(force=True)

    def add(self, column, n):
        if n:
            self.data["processed"][column] += n
            self.samples[column].append((time.time(), self.data["processed"][column]))
        self.write()

    def rate(self, column, now):
        q = self.samples[column]
        while len(q) > 1 and q[0][0] < now - self.WINDOW:
            q.popleft()
        if len(q) < 2 or now - q[0][0] < self.MIN_SPAN:
            return None
        # 分母用「現在」而不是最後一筆：停下來時速度會跟著往下掉，不會一直顯示舊的速度
        return (q[-1][1] - q[0][1]) / (now - q[0][0])

    def write(self, force=False):
        now = time.time()
        if not force and now - self.last_write < 5:
            return
        self.last_write = now
        out = dict(self.data, updated=int(now), rate={c: self.rate(c, now) for c in self.samples})
        tmp = self.path + ".tmp"
        try:
            with open(tmp, "w", encoding="utf-8") as f:
                json.dump(out, f, ensure_ascii=False)
            os.replace(tmp, self.path)
        except OSError as e:
            log.warning("寫不了進度檔 %s：%s", self.path, e)


# 文字向量的前處理版本：改了前處理規則就加一，舊的文字向量會自動重做
TEXT_PREP_VERSION = 2
# 字母或中文字少於這個數的文字不做向量（例如只有時間、數字的狀態列）：這種向量很空泛，跟什麼都有點像
TEXT_MIN_LETTERS = 4


def meaningful(text):
    return sum(1 for ch in text if ch.isalpha()) >= TEXT_MIN_LETTERS


COMPACT_MIN_BYTES = 8 * 1024 * 1024
COMPACT_MIN_RATIO = 0.25
COMPACT_CHECK_INTERVAL = 86400


def compact_if_worthwhile(conn, db_path, force=False, progress=None):
    """可回收空間達 25% 且至少 8 MB 才壓實（force 則一定做）。回傳 (壓實前, 壓實後) 的硬碟用量，沒做就回傳 None。"""
    with conn:
        dbm.set_meta(conn, "last_compact_check", int(time.time()))
    try:
        size, wasted = dbm.space_stats(conn)
    except sqlite3.OperationalError as e:
        log.warning("沒辦法估算可回收的空間（%s），不壓實", e)
        return None
    if not force and (wasted < COMPACT_MIN_BYTES or wasted < size * COMPACT_MIN_RATIO):
        return None
    # VACUUM 會先在暫存目錄建一份壓實後的副本（main() 已把暫存目錄設成資料庫所在的目錄）
    tmp = os.environ.get("SQLITE_TMPDIR") or "/tmp"
    need = (size - wasted) * 1.2
    if shutil.disk_usage(tmp).free < need:
        log.warning("暫存目錄 %s 的空間不足 %.0f MB，這次不壓實", tmp, need / 1e6)
        return None
    if progress is not None:
        progress.set("compacting")
    before = dbm.file_bytes(db_path)
    t = time.time()
    dbm.compact(conn)
    after = dbm.file_bytes(db_path)
    with conn:
        dbm.set_meta(conn, "last_compact", int(time.time()))
        dbm.set_meta(conn, "last_compact_result", json.dumps(
            {"before": before, "after": after, "seconds": round(time.time() - t, 1)}))
    log.info("壓實資料庫：%.1f MB → %.1f MB（%.1f 秒）", before / 1e6, after / 1e6, time.time() - t)
    return before, after


class Indexer:
    def __init__(self, cfg, dsm, llm, conn):
        self.cfg = cfg
        self.ic = cfg["index"]
        self.dsm = dsm
        self.llm = llm
        self.db = conn
        self.owner = None
        self.embed_tag = "%s@%s" % (llm.embed_model, cfg["synology"]["thumb_size"])
        self.ocr_tag = "%s@%s" % (llm.ocr_model, cfg["synology"]["ocr_thumb_size"])
        self.text_tag = "%s@text%d" % (llm.embed_model, TEXT_PREP_VERSION)
        self.progress = Progress(os.path.join(os.path.dirname(cfg["index"]["db"]), "indexer-status.json"))
        self.marker = dbm.search_marker(cfg["index"]["db"])

    # ── 使用者 ──

    def bind_owner(self):
        uid, name = self.dsm.me()
        stored = dbm.get_meta(self.db, "owner_user_id")
        if stored is not None and int(stored) != uid:
            raise OwnerMismatch("資料庫是使用者 %s 的索引，但目前登入的是 %s（id %s），拒絕混在一起"
                                % (stored, name, uid))
        if stored is None:
            dbm.set_meta(self.db, "owner_user_id", uid)
            dbm.set_meta(self.db, "owner_name", name)
            self.db.commit()
        self.owner = uid
        self.requeue_stale()
        return uid, name

    def requeue_stale(self):
        """模型或縮圖尺寸改了：用舊設定做的向量 / OCR 重新排入（舊的先留著，重做完才覆蓋）。"""
        with self.db:
            rev = dbm.next_rev(self.db)
            a = self.db.execute("UPDATE item SET emb_state=0, emb_tries=0, rev=? WHERE emb_state=1 AND unit_id IN "
                                "(SELECT unit_id FROM vec WHERE model<>?)", (rev, self.embed_tag)).rowcount
            b = self.db.execute("UPDATE item SET ocr_state=0, ocr_tries=0, rev=? WHERE ocr_state=1 AND unit_id IN "
                                "(SELECT unit_id FROM ocr WHERE model<>?)", (rev, self.ocr_tag)).rowcount
            c = self.db.execute("DELETE FROM ocr_vec WHERE model<>?", (self.text_tag,)).rowcount
        if a or b or c:
            log.info("設定改變，重新排入：embedding %d、OCR %d、文字向量 %d", a, b, c)
        return a, b

    # ── 掃描 ──

    def _upsert(self, items, scan_id, rev):
        changed = 0
        for it in items:
            if it["owner_user_id"] != self.owner:
                continue
            row = self.db.execute("SELECT cache_key, time, type, deleted FROM item WHERE unit_id=?",
                                  (it["unit_id"],)).fetchone()
            if row is None:
                self.db.execute("INSERT INTO item(unit_id, owner_user_id, time, type, cache_key, seen_scan, rev) "
                                "VALUES (?,?,?,?,?,?,?)", (it["unit_id"], it["owner_user_id"], it["time"], it["type"],
                                                            it["cache_key"], scan_id, rev))
                changed += 1
            elif row[0] != it["cache_key"]:
                # 照片或縮圖變了：之前的向量與 OCR 都不再可信
                for table in ("vec", "ocr", "ocr_vec"):
                    self.db.execute("DELETE FROM %s WHERE unit_id=?" % table, (it["unit_id"],))
                self.db.execute("UPDATE item SET cache_key=?, time=?, type=?, deleted=0, seen_scan=?, emb_state=0, "
                                "emb_tries=0, ocr_state=0, ocr_tries=0, last_error=NULL, rev=? WHERE unit_id=?",
                                (it["cache_key"], it["time"], it["type"], scan_id, rev, it["unit_id"]))
                changed += 1
            elif (row[1], row[2], row[3]) != (it["time"], it["type"], 0):
                self.db.execute("UPDATE item SET time=?, type=?, deleted=0, seen_scan=?, rev=? WHERE unit_id=?",
                                (it["time"], it["type"], scan_id, rev, it["unit_id"]))
                changed += 1
            elif scan_id is not None:
                self.db.execute("UPDATE item SET seen_scan=? WHERE unit_id=?", (scan_id, it["unit_id"]))
        return changed

    def _page(self, fetch, offset):
        raw = fetch(offset, self.ic["page_size"])
        items = [parse_item(x) for x in raw]
        bad = sum(1 for x in items if x is None)
        if bad:
            log.warning("有 %d 筆照片資料格式看不懂，略過", bad)
        return raw, [x for x in items if x is not None]

    def full_scan(self):
        started = time.time()
        scan_id = int(dbm.get_meta(self.db, "scan_seq", 0)) + 1
        try:
            total = self.dsm.count_items()
        except Exception:
            total = None
        self.progress.set("full_scan", scan_done=0, scan_total=total)
        offset = seen = changed = 0
        while True:
            raw, items = self._page(self.dsm.list_items, offset)
            if not raw:
                break
            with self.db:
                rev = dbm.next_rev(self.db)
                changed += self._upsert(items, scan_id, rev)
            seen += len(items)
            offset += len(raw)
            self.progress.data["scan_done"] = offset
            self.progress.write()
        existing = self.db.execute("SELECT count(*) FROM item WHERE deleted=0 AND seen_scan<>?", (scan_id,)).fetchone()[0]
        total = self.db.execute("SELECT count(*) FROM item WHERE deleted=0").fetchone()[0]
        removed = 0
        if existing and seen < total * 0.5:
            # 一次少了一半以上，比較可能是 API 出問題而不是真的刪掉那麼多照片
            log.warning("完整掃描只看到 %d 張，但索引裡有 %d 張；這次不標記刪除", seen, total)
        elif existing:
            with self.db:
                rev = dbm.next_rev(self.db)
                ids = [r[0] for r in self.db.execute("SELECT unit_id FROM item WHERE deleted=0 AND seen_scan<>?",
                                                     (scan_id,))]
                for uid in ids:
                    for table in ("vec", "ocr", "ocr_vec"):
                        self.db.execute("DELETE FROM %s WHERE unit_id=?" % table, (uid,))
                self.db.execute("UPDATE item SET deleted=1, rev=? WHERE deleted=0 AND seen_scan<>?", (rev, scan_id))
                removed = len(ids)
        with self.db:
            dbm.set_meta(self.db, "scan_seq", scan_id)
            dbm.set_meta(self.db, "last_full_scan", int(time.time()))
            dbm.set_meta(self.db, "last_full_scan_result", json.dumps(
                {"seen": seen, "changed": changed, "removed": removed, "seconds": round(time.time() - started, 1)}))
        log.info("完整掃描：%d 張，新增或變更 %d，刪除 %d", seen, changed, removed)
        return seen, changed, removed

    def quick_scan(self, max_pages=5):
        """看最近新增的照片，直到一整頁都沒有變化為止。"""
        scan_id = int(dbm.get_meta(self.db, "scan_seq", 0))
        self.progress.set("quick_scan")
        offset = changed = 0
        for _ in range(max_pages):
            raw, items = self._page(self.dsm.list_recent, offset)
            if not raw:
                break
            with self.db:
                rev = dbm.next_rev(self.db)
                n = self._upsert(items, scan_id, rev)
            changed += n
            if n == 0:
                break
            offset += len(raw)
        with self.db:
            dbm.set_meta(self.db, "last_quick_scan", int(time.time()))
        if changed:
            log.info("快速掃描：新增或變更 %d", changed)
        return changed

    # ── embedding ──

    def _fail(self, column, uid, cache_key, err):
        state_col, tries_col = column + "_state", column + "_tries"
        with self.db:
            rev = dbm.next_rev(self.db)
            self.db.execute("UPDATE item SET %s=%s+1, last_error=?, rev=?, %s=CASE WHEN %s+1>=? THEN 2 ELSE %s END "
                            "WHERE unit_id=? AND cache_key=?" % (tries_col, tries_col, state_col, tries_col, state_col),
                            (str(err)[:500], rev, self.ic["max_tries"], uid, cache_key))

    def _thumb(self, uid, ck, size):
        """抓縮圖；那個尺寸 DSM 沒產生（HTTP 404，例如某些 HEIC 的 m 是 ame_defect）就換其他尺寸。"""
        sizes = [size] + [s for s in (("xl", "m", "sm") if size != "sm" else ("m", "xl")) if s != size]
        err = None
        for s in sizes:
            try:
                return self.dsm.thumbnail(uid, ck, s)
            except SynologyError as e:
                if e.transient or "HTTP 404" not in str(e):
                    raise
                err = e
        raise err

    def searched_recently(self):
        try:
            return time.time() - os.path.getmtime(self.marker) < self.ic["yield_to_search"]
        except OSError:
            return False

    def yield_to_search(self, limit=120):
        """有人在搜尋：等安靜下來再送下一批（最多等 limit 秒，避免一直有人搜尋時索引完全停住）。"""
        if not self.ic["yield_to_search"]:
            return
        end = time.time() + limit
        while self.searched_recently() and time.time() < end:
            time.sleep(1)

    def _embed_one(self, uid, ck):
        """回傳 (向量, None) 或 (None, 錯誤)。"""
        self.yield_to_search()
        try:
            img = self._thumb(uid, ck, self.cfg["synology"]["thumb_size"])[0]
            return self.llm.embed_image(img), None
        except (SynologyError, LLMError, OSError) as e:
            return None, e

    def _healthy(self, err, column):
        """看起來像服務問題的錯誤，是不是其實只跟這張照片有關：對同一個服務發一個很便宜的請求確認它正常。
        （例如 llama.cpp 解不開某張圖時回 500、某部影片沒有縮圖時 DSM 回錯誤）"""
        try:
            if not isinstance(err, LLMError):
                self.dsm.me()
            elif column == "emb":
                self.llm.embed_query("ping")
            else:
                self.llm.ocr(PROBE_PNG, "image/png")
            return True
        except Exception:
            return False

    def _record_failures(self, column, failed):
        """個別照片的錯誤計入重試次數；服務真的有問題則整體暫停。回傳處理掉的張數。"""
        health = {}
        down = []
        for uid, ck, e in failed:
            if transient(e):
                kind = type(e)
                if kind not in health:
                    health[kind] = self._healthy(e, column)
                if not health[kind]:
                    down.append(e)
                    continue
            log.warning("%s 失敗 %s：%s", "embedding" if column == "emb" else "OCR", uid, e)
            self._fail(column, uid, ck, e)
        if down:
            raise ServiceDown(str(down[0]))
        return len(failed) - len(down)

    def embed_pending(self, limit=256):
        """回傳這一輪處理掉（成功或確定失敗）的張數；0 表示沒有待處理的。"""
        rows = self.db.execute("SELECT unit_id, cache_key FROM item WHERE deleted=0 AND emb_state=0 "
                               "ORDER BY time DESC LIMIT ?", (limit,)).fetchall()
        if not rows:
            return 0
        self.progress.set("embedding")
        done = 0
        for uid, ck in rows:   # 一張做完才送下一張
            v, err = self._embed_one(uid, ck)
            if err is not None:
                n = self._record_failures("emb", [(uid, ck, err)])
                done += n
                self.progress.add("emb", n)
                continue
            with self.db:
                rev = dbm.next_rev(self.db)
                cur = self.db.execute("UPDATE item SET emb_state=1, last_error=NULL, rev=? "
                                      "WHERE unit_id=? AND cache_key=? AND deleted=0", (rev, uid, ck))
                if cur.rowcount:  # 處理期間照片沒有被改掉或刪掉才寫入
                    self.db.execute("INSERT OR REPLACE INTO vec(unit_id, model, v) VALUES (?,?,?)",
                                    (uid, self.embed_tag, to_f16(v)))
            done += 1
            self.progress.add("emb", 1)
        return done

    # ── OCR ──

    def _ocr_one(self, row):
        uid, ck = row
        try:
            img, mime = self._thumb(uid, ck, self.cfg["synology"]["ocr_thumb_size"])
            if mime == "image/gif":
                return uid, ck, "", None  # OCR 模型讀不了 GIF（實測會回提示詞本身）；動圖多半也沒有要搜的文字
            return uid, ck, self.llm.ocr(img, mime), None
        except (SynologyError, LLMError, OSError) as e:
            return uid, ck, None, e

    def ocr_pending(self, limit=64):
        if self.ic["ocr"] == "off":
            return 0
        rows = self.db.execute("SELECT unit_id, cache_key FROM item WHERE deleted=0 AND emb_state=1 AND ocr_state=0 "
                               "ORDER BY time DESC LIMIT ?", (limit,)).fetchall()
        if rows:
            self.progress.set("ocr")
        done = 0
        for row in rows:   # 一張做完才送下一張
            uid, ck, text, err = self._ocr_one(row)
            if err is not None:
                n = self._record_failures("ocr", [(uid, ck, err)])
                done += n
                self.progress.add("ocr", n)
                continue
            with self.db:
                rev = dbm.next_rev(self.db)
                cur = self.db.execute("UPDATE item SET ocr_state=1, last_error=NULL, rev=? "
                                      "WHERE unit_id=? AND cache_key=? AND deleted=0", (rev, uid, ck))
                if cur.rowcount:
                    self.db.execute("INSERT OR REPLACE INTO ocr(unit_id, model, text) VALUES (?,?,?)",
                                    (uid, self.ocr_tag, text))
                    self.db.execute("DELETE FROM ocr_vec WHERE unit_id=?", (uid,))
            done += 1
            self.progress.add("ocr", 1)
        return done

    # ── OCR 文字的向量 ──

    def text_embed_pending(self, limit=64):
        """把 OCR 讀出的文字做成向量。文字很短、GPU 負擔很小，所以不讓給搜尋。"""
        if self.ic["ocr"] == "off" or not self.ic["ocr_embed"]:
            return 0
        rows = self.db.execute("SELECT o.unit_id, o.text FROM ocr o JOIN item i USING(unit_id) "
                               "LEFT JOIN ocr_vec t USING(unit_id) WHERE i.deleted=0 AND o.text<>'' "
                               "AND t.unit_id IS NULL ORDER BY i.time DESC LIMIT ?", (limit,)).fetchall()
        if not rows:
            return 0
        self.progress.set("text_embedding")
        out = {uid: None for uid, text in rows if not meaningful(text)}   # 記成做過（空向量），不再重試
        rows = [(uid, text) for uid, text in rows if uid not in out]
        try:
            for uid, text in rows:   # 一段做完才送下一段
                try:
                    out[uid] = self.llm.embed_text(text)
                except LLMError as e:
                    if e.transient and not self._healthy(e, "emb"):
                        raise
                    log.warning("文字向量失敗 %s：%s", uid, e)
                    out[uid] = None
        except LLMError as e:
            raise ServiceDown(str(e))
        with self.db:
            rev = dbm.next_rev(self.db)
            for uid, v in out.items():
                self.db.execute("INSERT OR REPLACE INTO ocr_vec(unit_id, model, v) VALUES (?,?,?)",
                                (uid, self.text_tag, to_f16(v) if v is not None else b""))
                self.db.execute("UPDATE item SET rev=? WHERE unit_id=?", (rev, uid))
        self.progress.add("txt", len(out))
        return len(out)

    # ── 主迴圈 ──

    def maybe_compact(self):
        """大量補做索引之後，趁閒下來時壓實（每天最多檢查一次）。"""
        if not self.ic["auto_compact"]:
            return None
        if time.time() - float(dbm.get_meta(self.db, "last_compact_check", 0)) < COMPACT_CHECK_INTERVAL:
            return None
        return compact_if_worthwhile(self.db, self.cfg["index"]["db"], progress=self.progress)

    def run_once(self, full=True):
        self.bind_owner()
        if full:
            self.full_scan()
        else:
            self.quick_scan()
        while self.embed_pending():
            pass
        while self.ocr_pending() or self.text_embed_pending():
            pass
        self.progress.set("done")

    def loop(self, stop: threading.Event):
        try:
            self.bind_owner()
        except OwnerMismatch as e:
            self.progress.set("error", pause_reason=str(e))
            raise
        last_full = float(dbm.get_meta(self.db, "last_full_scan", 0))
        next_full = last_full + self.ic["full_scan_interval"]
        next_quick = 0.0
        backoff = 0
        while not stop.is_set():
            try:
                now = time.time()
                if now >= next_full:
                    self.full_scan()
                    next_full = now + self.ic["full_scan_interval"]
                    next_quick = now + self.ic["quick_scan_interval"]
                elif now >= next_quick:
                    self.quick_scan()
                    next_quick = now + self.ic["quick_scan_interval"]
                self.progress.data.update(next_full_scan=int(next_full), next_quick_scan=int(next_quick))
                # 文字向量排在 OCR 前面：每做完一批 OCR，下一輪就先把那批文字做成向量
                worked = self.embed_pending() or self.text_embed_pending() or self.ocr_pending()
                backoff = 0
                if not worked:
                    # 大量補做索引之後，趁閒下來時壓實一次
                    self.maybe_compact()
                    self.progress.set("idle")
                    stop.wait(min(30, max(1, min(next_full, next_quick) - time.time())))
            except OwnerMismatch:
                raise
            except Exception as e:
                backoff = min(600, max(30, backoff * 2))
                if isinstance(e, ServiceDown):
                    log.warning("服務暫時連不上，%d 秒後再試：%s", backoff, e)
                else:
                    log.error("索引出錯，%d 秒後重試：%s", backoff, e)
                self.progress.set("paused", paused_until=int(time.time() + backoff), pause_reason=str(e)[:300])
                stop.wait(backoff)
        self.progress.set("stopped")
