#!/usr/bin/env python3
# 索引器 + 搜尋服務測試：假 DSM、假 LLM，資料庫是真的 SQLite。
#   python -m unittest tests.test_engine -v      （要在裝好 sse 套件的 Python 3.12 環境）
import asyncio
import json
import os
import shutil
import socket
import sys
import tempfile
import threading
import time
import unittest

import yaml

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, os.path.dirname(HERE))
sys.path.insert(0, HERE)

import fake_dsm  # noqa: E402
import fake_llm  # noqa: E402
from sse import config as cfgm  # noqa: E402
from sse import db as dbm  # noqa: E402
from sse.cli import main  # noqa: E402
from sse.commands import make_dsm, make_llm  # noqa: E402
from sse.indexer import Indexer, OwnerMismatch, ServiceDown  # noqa: E402
from sse.llm import to_f16  # noqa: E402
from sse.search import Index, select  # noqa: E402
from sse.server import Server  # noqa: E402

import numpy as np  # noqa: E402

T0 = fake_dsm.E2E_T0


def library():
    return fake_dsm.engine_library()


def wait_until(cond, timeout=10):
    end = time.time() + timeout
    while time.time() < end:
        if cond():
            return True
        time.sleep(0.05)
    return False


class Engine(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp(prefix="sse-eng.", dir=os.environ.get("SSE_TEST_TMP"))
        self.dsm = fake_dsm.FakeDSM(library())
        self.http = fake_dsm.serve_http(self.dsm)
        self.llm = fake_llm.FakeLLM()
        self.llm_srv = fake_llm.serve(self.llm)
        self.cfg_path = os.path.join(self.d, "config.yml")
        self.write_cfg()

    def write_cfg(self, account="alice", password="pw-alice", **sections):
        doc = {
            "synology": {"url": "http://127.0.0.1:%d" % self.http.server_port, "account": account,
                         "password": password},
            "embedding": {"base_url": "http://127.0.0.1:%d/v1" % self.llm_srv.server_port, "api_key": "sk-test"},
            "ocr": {"model": "fake-vision"},
            "index": {"page_size": 7},
            "search": {"socket": self.d + "/engine.sock", "refresh_interval": 1},
        }
        for sec, vals in sections.items():
            doc.setdefault(sec, {}).update(vals)
        fd = os.open(self.cfg_path, os.O_WRONLY | os.O_CREAT | os.O_TRUNC, 0o600)
        with os.fdopen(fd, "w") as f:
            yaml.safe_dump(doc, f, allow_unicode=True)
        self.cfg = cfgm.load(self.cfg_path)

    def tearDown(self):
        self.http.shutdown()
        self.llm_srv.shutdown()
        shutil.rmtree(self.d, ignore_errors=True)

    def indexer(self):
        return Indexer(self.cfg, make_dsm(self.cfg), make_llm(self.cfg), dbm.connect(self.cfg["index"]["db"]))

    def search(self, q, idx=None):
        idx = idx or Index(self.cfg["index"]["db"], self.cfg["search"]["dims"])
        idx.refresh()
        return idx.search(make_llm(self.cfg).embed_query(q), q, self.cfg["search"])

    def concept_ids(self, concept):
        return {it["id"] for it in self.dsm.items.values() if it["concept"] == concept and it["owner_user_id"] == 1}

    def test_config_permissions_enforced(self):
        os.chmod(self.cfg_path, 0o644)
        with self.assertRaises(cfgm.ConfigError) as cm:
            cfgm.load(self.cfg_path)
        self.assertIn("chmod 600", str(cm.exception))
        os.chmod(self.cfg_path, 0o600)
        with open(self.cfg_path, "a") as f:
            f.write("embedding:\n  imagesize: xl\n")
        with self.assertRaises(cfgm.ConfigError) as cm:
            cfgm.load(self.cfg_path)

    def test_config_validation(self):
        self.write_cfg(ocr={"base_url": "", "api_key": ""}, embedding={"api_key": "sk-e"})
        self.assertEqual((self.cfg["llm"]["ocr_base_url"], self.cfg["llm"]["ocr_api_key"]),
                         (self.cfg["llm"]["embed_base_url"], "sk-e"))   # OCR 沒填就沿用 embedding 的服務
        for bad in ({"embedding": {"image_size": "huge"}}, {"search": {"min_score": "high"}},
                    {"ocr": {"model": ""}}, {"synology": {"password": ""}}):
            with self.assertRaises(cfgm.ConfigError):
                self.write_cfg(**bad)
        self.write_cfg(ocr={"enabled": False, "model": ""})
        self.assertEqual(self.cfg["index"]["ocr"], "off")
        self.write_cfg(password=123456)   # YAML 把純數字當成整數
        self.assertEqual(self.cfg["synology"]["password"], "123456")

    def test_index_and_search(self):
        ix = self.indexer()
        ix.run_once(full=True)
        st = dbm.stats(ix.db)
        self.assertEqual((st["items"], st["embedded"], st["ocr_done"], st["with_text"]), (20, 20, 20, 2))
        self.assertEqual(st["owner_user_id"], "1")
        self.assertEqual({h["id"] for h in self.search("cat")}, self.concept_ids("cat"))  # 不含 bob 的 300
        self.assertEqual({h["id"] for h in self.search("beach")}, self.concept_ids("beach"))
        self.assertEqual(self.search("volcano"), [])
        # OCR：長字串用 FTS、短字串用 LIKE
        self.assertEqual([(h["id"], h["src"]) for h in self.search("38475629")], [(200, "ocr")])
        # 某張照片的文字裡剛好有「cat」：OCR 命中是加進來，不能讓向量找到的貓照片消失
        self.dsm.items[202] = fake_dsm.make_item(202, 1, T0 + 202 * 3600, "receipt", "cat food 2kg")
        self.indexer().run_once(full=False)
        self.assertEqual({h["id"] for h in self.search("cat")}, self.concept_ids("cat") | {202})
        self.assertEqual([h["id"] for h in self.search("停車")], [201])
        hits = self.search("dog")
        self.assertEqual([h["time"] for h in hits], sorted((h["time"] for h in hits), reverse=True))
        # 再跑一次不會重做
        before = dict(self.llm.calls)
        ix.run_once(full=True)
        self.assertEqual(self.llm.calls["embed_image"], before["embed_image"])

    def test_changes_deletes_and_safety(self):
        ix = self.indexer()
        ix.run_once(full=True)
        idx = Index(self.cfg["index"]["db"], self.cfg["search"]["dims"])
        idx.refresh()
        # 刪掉一張、改一張縮圖（cache_key 變了）、新增一張
        del self.dsm.items[101]
        self.dsm.items[102]["mtime"] += 1
        self.dsm.items[102]["concept"] = "beach"
        self.dsm.items[400] = fake_dsm.make_item(400, 1, T0 + 999999, "dog")
        ix.run_once(full=True)
        self.assertNotIn(101, {h["id"] for h in self.search("cat", idx)})
        self.assertIn(102, {h["id"] for h in self.search("beach", idx)})
        self.assertIn(400, {h["id"] for h in self.search("dog", idx)})
        self.assertEqual(int(idx.ids.size), 20)  # 增量更新後索引大小正確
        # API 突然只回很少照片：不能把其他照片當成被刪除
        saved = dict(self.dsm.items)
        self.dsm.items = {k: v for k, v in saved.items() if k in (103, 300)}
        ix.full_scan()
        self.assertEqual(dbm.stats(ix.db)["deleted"], 1)
        self.dsm.items = saved

    def test_settings_change_requeues(self):
        ix = self.indexer()
        ix.run_once(full=True)
        calls = self.llm.calls["embed_image"]
        self.write_cfg(embedding={"image_size": "xl"})
        ix = self.indexer()
        ix.run_once(full=False)
        self.assertEqual(self.llm.calls["embed_image"] - calls, 20)   # 每張用新尺寸重做一次
        self.assertEqual(ix.db.execute("SELECT DISTINCT model FROM vec").fetchall(), [("embeddinggemma-2@xl",)])
        ix.run_once(full=False)
        self.assertEqual(self.llm.calls["embed_image"] - calls, 20)   # 之後不再重做

    def test_progress_report(self):
        import contextlib
        import io
        ix = self.indexer()
        ix.run_once(full=True)
        prog_path = os.path.join(self.d, "data", "indexer-status.json")
        with open(prog_path) as f:
            prog = json.load(f)
        self.assertEqual((prog["phase"], prog["processed"]["emb"], prog["processed"]["ocr"]), ("done", 20, 20))
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            self.assertEqual(main(["--config", self.cfg_path, "progress", "--json"]), 0)
        st = json.loads(out.getvalue())
        self.assertEqual((st["embedding"]["percent"], st["ocr"]["percent"]), (100.0, 100.0))
        self.assertEqual(st["last_full_scan_result"]["seen"], 20)
        self.assertTrue(st["indexer_running"])   # 進度檔的 pid 是這個測試 process
        # 有待處理的照片：百分比與「OCR 等 embedding」
        for uid in (900, 901, 902, 903, 904):
            self.dsm.items[uid] = fake_dsm.make_item(uid, 1, T0, "dog")
        ix.full_scan()
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            main(["--config", self.cfg_path, "progress", "--brief"])
        brief = out.getvalue()
        self.assertIn("embedding 20 / 25（80.0%", brief)
        self.assertIn("embedding 做完後開始", brief)
        out = io.StringIO()
        with contextlib.redirect_stdout(out):
            main(["--config", self.cfg_path, "progress"])
        self.assertIn("████████████████░░░░", out.getvalue())
        self.assertIn("完整掃描  上次", out.getvalue())

    def test_progress_rate_and_pause(self):
        from sse.indexer import Progress
        p = Progress(os.path.join(self.d, "p.json"))
        now = time.time()
        p.samples["emb"].extend([(now - 300, 0), (now - 150, 1000), (now - 1, 2000)])
        self.assertAlmostEqual(p.rate("emb", now), 2000 / 300, places=1)
        self.assertIsNone(p.rate("ocr", now))
        p.samples["ocr"].append((now - 30, 5))
        self.assertIsNone(p.rate("ocr", now))   # 不到 1 分鐘不算速度
        p.set("paused", paused_until=int(now + 60), pause_reason="LLM 連不上")
        with open(p.path) as f:
            self.assertEqual(json.load(f)["pause_reason"], "LLM 連不上")
        p.set("embedding")
        with open(p.path) as f:
            self.assertIsNone(json.load(f)["pause_reason"])

    def test_log_files(self):
        from sse import logs
        import datetime
        import logging
        d = os.path.join(self.d, "logs")
        os.makedirs(d)
        for day in ("2026-10-20", "2026-10-25"):
            open(os.path.join(d, "indexer.log." + day), "w").close()
        logs.prune_logs(os.path.join(d, "indexer.log"), 3, today=datetime.date(2026, 10, 26))
        self.assertEqual(os.listdir(d), ["indexer.log.2026-10-25"])
        root = logging.getLogger()
        saved = list(root.handlers)
        try:
            self.assertIsNone(logs.setup(os.path.join(d, "indexer.log"), 3))
            logging.getLogger("sse.test").info("寫進 log 檔")
            root.handlers[0].flush()
            with open(os.path.join(d, "indexer.log"), encoding="utf-8") as f:
                self.assertIn("寫進 log 檔", f.read())
        finally:
            for h in list(root.handlers):
                root.removeHandler(h)
                h.close()
            for h in saved:
                root.addHandler(h)

    def test_text_semantic_search(self):
        self.write_cfg(search={"text_min_score": 0.4})
        # 文字寫著 invoice，搜 receipt：逐字比對找不到，只能靠照片文字的向量
        self.dsm.items[700] = fake_dsm.make_item(700, 1, T0 + 700 * 3600, "screen", "invoice 2026-03")
        ix = self.indexer()
        ix.run_once(full=True)
        st = dbm.stats(ix.db)
        self.assertEqual((st["with_text"], st["text_embedded"]), (3, 3))
        self.assertEqual(ix.db.execute("SELECT DISTINCT model FROM ocr_vec").fetchall(), [("embeddinggemma-2@text2",)])
        # 200 的圖片本身就是收據（圖片向量），700 是文字寫著 invoice 的截圖（文字向量）
        self.assertEqual({(h["id"], h["src"]) for h in self.search("receipt")}, {(700, "text"), (200, "vec")})
        self.assertEqual({h["id"] for h in self.search("cat")}, self.concept_ids("cat"))   # 圖片搜尋不受影響
        # 照片換了：舊的文字與文字向量都作廢，重做
        self.dsm.items[700]["mtime"] += 1
        self.dsm.items[700]["text"] = "parking"
        ix.run_once(full=True)
        self.assertEqual([h["id"] for h in self.search("receipt")], [200])
        self.assertEqual(dbm.stats(ix.db)["text_embedded"], 3)
        # 照片刪了：文字向量一起刪
        del self.dsm.items[700]
        ix.run_once(full=True)
        self.assertEqual(ix.db.execute("SELECT count(*) FROM ocr_vec WHERE unit_id=700").fetchone()[0], 0)

    def test_short_text_not_embedded(self):
        self.write_cfg(search={"text_min_score": 0.1})
        self.dsm.items[710] = fake_dsm.make_item(710, 1, T0, "statusbar", "12:30 / 88")   # 只有時間、數字
        ix = self.indexer()
        ix.run_once(full=True)
        self.assertEqual(ix.db.execute("SELECT length(v) FROM ocr_vec WHERE unit_id=710").fetchone()[0], 0)
        self.assertEqual(ix.text_embed_pending(), 0)                                   # 不會一直重試
        self.assertNotIn(710, {h["id"] for h in self.search("anything")})

    def test_old_database_without_ocr_vec(self):
        self.indexer().run_once(full=True)
        conn = dbm.connect(self.cfg["index"]["db"])
        conn.execute("DROP TABLE ocr_vec")
        conn.commit()
        idx = Index(self.cfg["index"]["db"], 512)
        self.assertEqual(idx.refresh(), 20)      # 搜尋服務照常載入圖片向量
        self.assertEqual({h["id"] for h in self.search("cat", idx)}, self.concept_ids("cat"))

    def test_manual_tag(self):
        import contextlib
        import io
        self.indexer().run_once(full=True)
        run = lambda *a: main(["--config", self.cfg_path, "tag"] + list(a))  # noqa: E731
        with contextlib.redirect_stdout(io.StringIO()), contextlib.redirect_stderr(io.StringIO()):
            self.assertEqual(run("add", "TEST", "203", "300"), 1)   # 300 是 bob 的，不在 alice 的索引裡
            self.assertEqual(run("add", "TEST", "103"), 0)
        hits = self.search("test")
        self.assertEqual([(h["id"], h["src"]) for h in hits], [(103, "tag")])   # 不分大小寫
        self.assertEqual(self.search(" Test ")[0]["id"], 103)
        self.assertEqual({h["id"] for h in self.search("cat")}, self.concept_ids("cat"))  # 不影響其他搜尋
        del self.dsm.items[103]
        self.indexer().run_once(full=True)
        self.assertEqual(self.search("test"), [])   # 照片刪了就不再出現
        with contextlib.redirect_stdout(io.StringIO()):
            self.assertEqual(run("rm", "test"), 0)
        self.assertEqual(Index(self.cfg["index"]["db"], 512).tag_hits("test"), [])

    def test_one_request_at_a_time(self):
        # 索引器與搜尋服務同時在跑：embedding 與 OCR 各自一次只有一個請求、一個請求只有一筆
        self.llm.delay = 0.02
        dbm.connect(self.cfg["index"]["db"]).close()
        srv = Server(self.cfg, make_llm(self.cfg))
        loop = asyncio.new_event_loop()
        t = threading.Thread(target=loop.run_until_complete, args=(srv.serve(),), daemon=True)
        t.start()
        sock = self.cfg["search"]["socket"]
        self.assertTrue(wait_until(lambda: os.path.exists(sock)))
        ix = self.indexer()
        ix.ic["yield_to_search"] = 0
        it = threading.Thread(target=ix.run_once, kwargs={"full": True})
        it.start()
        n = 0
        while it.is_alive():
            s = socket.socket(socket.AF_UNIX)
            s.connect(sock)
            s.sendall(json.dumps({"op": "search", "q": "query %d" % n}).encode() + b"\n")
            while not s.recv(65536).endswith(b"\n"):
                pass
            s.close()
            n += 1
        it.join()
        loop.call_soon_threadsafe(srv.stop.set)
        t.join(5)
        self.assertGreater(n, 3)
        self.assertEqual(dbm.stats(ix.db)["ocr_done"], 20)
        self.assertEqual((self.llm.max_inflight, self.llm.max_inputs), ({"embed": 1, "ocr": 1}, 1))

    def test_indexer_yields_to_search(self):
        ix = self.indexer()
        ix.ic["yield_to_search"] = 2
        open(ix.marker, "w").close()                     # 剛有人搜尋
        t = time.time()
        ix.run_once(full=True)
        self.assertGreaterEqual(time.time() - t, 1.5)    # 先等搜尋結束才送圖片
        self.assertEqual(dbm.stats(ix.db)["embedded"], 20)
        os.utime(ix.marker, (0, 0))
        self.assertFalse(ix.searched_recently())

    def test_sqlite_settings(self):
        w = dbm.connect(self.cfg["index"]["db"])
        r = dbm.connect(self.cfg["index"]["db"], readonly=True)
        get = lambda c, k: c.execute("PRAGMA " + k).fetchone()[0]  # noqa: E731
        self.assertEqual((get(w, "journal_mode"), get(w, "cache_size"), get(w, "wal_autocheckpoint"),
                          get(w, "journal_size_limit"), get(w, "page_size")), ("wal", -16384, 128, 8388608, 65536))
        self.assertEqual((get(r, "cache_size"), get(r, "mmap_size")), (-8192, 268435456))
        with self.assertRaises(Exception):
            r.execute("INSERT INTO meta VALUES ('x', 'y')")          # 唯讀連線寫不進去

    def test_compact(self):
        ix = self.indexer()
        ix.run_once(full=True)
        # 模擬大量補做：照新到舊（id 由大到小）插入幾千筆向量，頁只會填一半
        rng = np.random.default_rng(5)
        with ix.db:
            for uid in range(6000, 1000, -1):
                ix.db.execute("INSERT INTO item(unit_id, owner_user_id, time, type, cache_key, seen_scan, emb_state, rev) "
                              "VALUES (?,1,?,'photo','k',1,1,1)", (uid, T0 + uid))
                ix.db.execute("INSERT INTO vec VALUES (?,?,?)", (uid, "m", to_f16(rng.standard_normal(768).tolist())))
        before_hits = self.search("cat")
        size, wasted = dbm.space_stats(ix.db)
        self.assertGreater(wasted, size * 0.25)
        r = ix.maybe_compact()
        self.assertIsNotNone(r)
        self.assertEqual(json.loads(dbm.get_meta(ix.db, "last_compact_result"))["after"], r[1])
        self.assertLess(r[1], r[0] * 0.75)
        size2, wasted2 = dbm.space_stats(ix.db)
        self.assertLess(wasted2, size2 * 0.15)
        self.assertLess(os.path.getsize(self.cfg["index"]["db"] + "-wal"), 9 * 1024 * 1024)   # WAL 也縮回去了
        self.assertEqual(self.search("cat"), before_hits)                                   # 資料沒變
        self.assertEqual([h["id"] for h in self.search("38475629")], [200])                 # OCR 全文索引照常
        self.assertIsNone(ix.maybe_compact())                                               # 一天最多檢查一次
        dbm.set_meta(ix.db, "last_compact_check", 0)
        ix.db.commit()
        self.assertIsNone(ix.maybe_compact())                                               # 沒什麼好回收就不做

    def test_cli_sets_sqlite_tmpdir(self):
        import contextlib
        import io
        saved = os.environ.pop("SQLITE_TMPDIR", None)
        try:
            out = io.StringIO()
            with contextlib.redirect_stdout(out):
                self.assertEqual(main(["--config", self.cfg_path, "progress"]), 0)
            self.assertIn("索引器還沒跑過", out.getvalue())
            self.assertEqual(os.environ["SQLITE_TMPDIR"], os.path.join(self.d, "data"))
        finally:
            os.environ.pop("SQLITE_TMPDIR", None)
            if saved is not None:
                os.environ["SQLITE_TMPDIR"] = saved

    def test_quick_scan_picks_up_new_photo(self):
        ix = self.indexer()
        ix.run_once(full=True)
        self.dsm.items[500] = fake_dsm.make_item(500, 1, T0 - 99999, "mountain")  # 舊照片、剛加入
        self.assertEqual(ix.quick_scan(), 1)
        ix.run_once(full=False)
        self.assertIn(500, {h["id"] for h in self.search("mountain")})

    def test_owner_mismatch_refused(self):
        self.indexer().run_once(full=True)
        self.write_cfg(account="bob", password="pw-bob")
        with self.assertRaises(OwnerMismatch):
            self.indexer().run_once(full=True)

    def test_truncated_ocr_keeps_partial_text(self):
        self.dsm.items[890] = fake_dsm.make_item(890, 1, T0, "screenshot", "LONG:第一行\n第二行 hello\n第二行 hello\n第三")
        ix = self.indexer()
        ix.run_once(full=True)
        self.assertEqual(dbm.stats(ix.db)["ocr_failed"], 0)
        self.assertEqual(ix.db.execute("SELECT text FROM ocr WHERE unit_id=890").fetchone()[0], "第一行\n第二行 hello\n第三")
        self.assertEqual([h["id"] for h in self.search("hello")], [890])

    def test_service_outage_does_not_burn_retries(self):
        self.llm.fail_ocr = True   # LLM 回 500：服務問題
        ix = self.indexer()
        with self.assertRaises(ServiceDown):
            ix.run_once(full=True)
        st = dbm.stats(ix.db)
        self.assertEqual((st["embedded"], st["ocr_failed"], st["ocr_pending"]), (20, 0, 20))
        self.llm.fail_ocr = False
        ix.run_once(full=False)
        self.assertEqual(dbm.stats(ix.db)["ocr_done"], 20)

    def test_gif_and_unknown_format(self):
        self.dsm.items[880] = fake_dsm.make_item(880, 1, T0, "gif")      # 動圖：照樣 embed，跳過 OCR
        self.dsm.items[881] = fake_dsm.make_item(881, 1, T0, "weird")    # 不認得的格式：只算這張的錯誤
        self.dsm.items[882] = fake_dsm.make_item(882, 1, T0, "nothumb", typ="video")  # 縮圖 HTTP 404：也只算這張
        self.dsm.items[883] = fake_dsm.make_item(883, 1, T0, "undecodable")  # LLM 回 500 但服務正常：也只算這張
        self.dsm.items[884] = fake_dsm.make_item(884, 1, T0, "heic-no-m")  # 沒有 m 縮圖：改用 xl，照樣成功
        ix = self.indexer()
        ix.run_once(full=True)   # 不會因為這兩張而整體暫停（ServiceDown）
        st = dbm.stats(ix.db)
        self.assertEqual((st["embedded"], st["embed_failed"]), (22, 3))
        self.assertEqual(ix.db.execute("SELECT text FROM ocr WHERE unit_id=880").fetchone()[0], "")
        self.assertIn(880, {h["id"] for h in self.search("gif")})

    def test_broken_photo_retries_then_gives_up(self):
        self.dsm.items[777] = fake_dsm.make_item(777, 1, T0, "broken")   # 縮圖永遠拿不到
        ix = self.indexer()
        ix.run_once(full=True)
        st = dbm.stats(ix.db)
        self.assertEqual((st["embedded"], st["embed_failed"]), (20, 1))
        self.assertEqual(ix.db.execute("SELECT emb_tries FROM item WHERE unit_id=777").fetchone()[0], 3)
        self.assertEqual(dbm.requeue_failed(ix.db), (1, 0))

    def test_server_protocol_and_query_cache(self):
        self.indexer().run_once(full=True)
        srv = Server(self.cfg, make_llm(self.cfg))
        loop = asyncio.new_event_loop()
        t = threading.Thread(target=loop.run_until_complete, args=(srv.serve(),), daemon=True)
        t.start()
        sock = self.cfg["search"]["socket"]
        end = time.time() + 10
        while not getattr(srv, "ready", False) and time.time() < end:
            time.sleep(0.05)

        def ask(req):
            s = socket.socket(socket.AF_UNIX)
            s.connect(sock)
            s.sendall(req)
            data = b""
            while not data.endswith(b"\n"):
                chunk = s.recv(65536)
                if not chunk:
                    break
                data += chunk
            s.close()
            return json.loads(data)

        self.assertEqual(oct(os.stat(sock).st_mode & 0o777), "0o660")
        ping = ask(b'{"op":"ping"}\n')
        self.assertEqual((ping["ok"], ping["owner_user_id"], ping["items"]), (True, 1, 20))
        r = ask(json.dumps({"op": "search", "q": "cat"}).encode() + b"\n")
        self.assertEqual({h["id"] for h in r["hits"]}, self.concept_ids("cat"))
        self.assertLess(time.time() - os.path.getmtime(dbm.search_marker(self.cfg["index"]["db"])), 5)  # 有通知索引器
        calls = self.llm.calls["embed_text"]
        ask(json.dumps({"op": "search", "q": "cat"}).encode() + b"\n")
        self.assertEqual(self.llm.calls["embed_text"], calls)  # 第二次走快取
        self.assertFalse(ask(b'{"op":"search","q":""}\n')["ok"])
        self.assertFalse(ask(b'not json\n')["ok"])
        # 新照片在 refresh_interval 內出現在搜尋結果
        self.dsm.items[600] = fake_dsm.make_item(600, 1, T0 + 10 ** 7, "cat")
        self.indexer().run_once(full=False)
        time.sleep(2.5)
        r = ask(json.dumps({"op": "search", "q": "cat"}).encode() + b"\n")
        self.assertIn(600, {h["id"] for h in r["hits"]})
        loop.call_soon_threadsafe(srv.stop.set)
        t.join(5)
        self.assertFalse(os.path.exists(sock))  # 停止時移除 socket


class Select(unittest.TestCase):
    def test_threshold_rules(self):
        s = np.array([0.72, 0.70, 0.66, 0.63, 0.55], dtype=np.float32)
        self.assertEqual(list(select(s, 0.64, 0.06, 500)), [0, 1, 2])
        self.assertEqual(list(select(s, 0.64, 0.03, 500)), [0, 1])
        self.assertEqual(list(select(s * 0.8, 0.64, 0.06, 500)), [])   # 沒有夠像的就不回傳
        big = np.concatenate([np.full(5000, 0.55, dtype=np.float32), s])
        self.assertEqual(sorted(select(big, 0.64, 0.06, 2)), [5000, 5001])  # max_results
        common = np.concatenate([np.full(3000, 0.74, dtype=np.float32), np.full(7000, 0.62, dtype=np.float32)])
        self.assertEqual(select(common, 0.71, 0.08, 5000).size, 3000)  # 常見主題不會被整體分布刷掉

    def test_hub_correction(self):
        d = tempfile.mkdtemp(dir=os.environ.get("SSE_TEST_TMP"))
        try:
            conn = dbm.connect(d + "/i.db")
            rng = np.random.default_rng(3)
            bank = rng.standard_normal((5, 768)).astype(np.float32)
            hubby = bank.mean(axis=0) + 0.3 * rng.standard_normal(768).astype(np.float32)  # 跟所有通用查詢都像
            query = rng.standard_normal(768).astype(np.float32)
            target = query + 0.6 * rng.standard_normal(768).astype(np.float32)
            with conn:
                for uid, v in ((1, hubby + 0.55 * query / np.linalg.norm(query) * np.linalg.norm(hubby)), (2, target)):
                    conn.execute("INSERT INTO item(unit_id, owner_user_id, time, type, cache_key, seen_scan, emb_state, rev) "
                                 "VALUES (?,1,0,'photo','k',1,1,1)", (uid,))
                    conn.execute("INSERT INTO vec VALUES (?,?,?)", (uid, "m", to_f16((v / np.linalg.norm(v)).tolist())))
                dbm.set_meta(conn, "rev", 1)
            idx = Index(d + "/i.db", 512)
            idx.refresh()
            q = query / np.linalg.norm(query)
            raw = dict(zip(idx.ids.tolist(), idx.scores(q[:512] / np.linalg.norm(q[:512]))[1].tolist()))
            idx.set_bank(list(bank))
            fixed = dict(zip(idx.ids.tolist(), idx.scores(q[:512] / np.linalg.norm(q[:512]))[1].tolist()))
            self.assertLess(fixed[1] - raw[1], fixed[2] - raw[2])   # hub 照片被扣分較多
        finally:
            shutil.rmtree(d, ignore_errors=True)


if __name__ == "__main__":
    unittest.main()
