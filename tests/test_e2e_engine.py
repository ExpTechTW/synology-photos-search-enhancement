#!/usr/bin/env python3
# filter engine 模式的端對端測試：真的 nginx → sse filter（Go 執行檔，engine 模式）→ 假 synoscgi（假 DSM，兩個帳號）
# ＋ 假的 sse-engine（unix socket，回傳固定結果）。
#
#   make build/sse
#   SSE_NGINX_BIN=/path/to/nginx python3 -m unittest tests.test_e2e_engine -v
import asyncio
import http.client
import json
import os
import shutil
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from urllib.parse import urlencode

HERE = os.path.dirname(os.path.abspath(__file__))
sys.path.insert(0, HERE)

import fake_dsm  # noqa: E402
from test_e2e_nginx import NGINX, SSE_BIN, SCGI_PARAMS, free_port, sock_alive, wait_for  # noqa: E402

DAY = 86400
T0 = fake_dsm.E2E_T0


class FakeEngine:
    """回傳 fake_dsm.E2E_ENGINE_HITS；owner 是 alice（1）。"""

    def __init__(self, path):
        self.path = path
        self.queries = []
        self.loop = asyncio.new_event_loop()
        self.ready = threading.Event()
        threading.Thread(target=self.loop.run_until_complete, args=(self.main(),), daemon=True).start()
        self.ready.wait(5)

    async def handle(self, r, w):
        req = json.loads(await r.readline())
        self.queries.append(req)
        hits = [{"id": i, "time": T0 + i * DAY, "type": "photo", "score": 0.7, "src": "vec"}
                for i in fake_dsm.E2E_ENGINE_HITS.get(req.get("q"), [])]
        w.write(json.dumps({"ok": True, "owner_user_id": 1, "hits": hits}).encode() + b"\n")
        await w.drain()
        w.close()

    async def main(self):
        self.server = await asyncio.start_unix_server(self.handle, path=self.path)
        self.ready.set()
        await asyncio.Event().wait()

    def stop(self):
        self.loop.call_soon_threadsafe(self.server.close)
        time.sleep(0.2)
        if os.path.exists(self.path):
            os.unlink(self.path)


@unittest.skipUnless(NGINX, "需要 SSE_NGINX_BIN 指向 nginx 執行檔")
class EngineE2E(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.fake = cls.filter = cls.ng = cls.engine = None
        cls.d = tempfile.mkdtemp(prefix="sse.", dir=os.environ.get("SSE_TEST_TMP"))
        try:
            cls._setup()
        except BaseException:
            cls.tearDownClass()
            raise

    @classmethod
    def _setup(cls):
        d = cls.d
        cls.up, cls.fsock, cls.esock = d + "/up.sock", d + "/f.sock", d + "/e.sock"
        cls.reqlog, cls.status, cls.info = d + "/requests.log", d + "/status.json", d + "/INFO"
        cls.capture = d + "/capture.jsonl"
        with open(cls.info, "w") as f:
            f.write('version="1.8.1-10089"\n')
        cls.cfg = d + "/filter.json"
        with open(cls.cfg, "w") as f:
            json.dump({"listen": cls.fsock, "socket_mode": "0600", "upstream": cls.up, "status_file": cls.status,
                       "kill_switch": d + "/DISABLE", "photos_info": cls.info, "mode": "engine",
                       "inject_photos_version": "1.8.1-10089", "engine_socket": cls.esock, "cache_ttl": 1,
                       "capture_file": cls.capture, "capture_max": 500}, f)
        cls.log = open(d + "/children.log", "a")
        cls.fake = subprocess.Popen([sys.executable, HERE + "/fake_synoscgi.py", cls.up, cls.reqlog, "dsm"],
                                    stdout=subprocess.DEVNULL, stderr=cls.log)
        assert wait_for(lambda: sock_alive(cls.up))
        cls.engine = FakeEngine(cls.esock)
        cls.filter = subprocess.Popen([SSE_BIN, "filter", "--config", cls.cfg],
                                      stdout=subprocess.DEVNULL, stderr=cls.log)
        assert wait_for(lambda: sock_alive(cls.fsock))
        cls.port = free_port()
        ng = cls.ng = d + "/nginx"
        os.makedirs(ng + "/conf")
        os.makedirs(ng + "/logs")
        with open(ng + "/conf/scgi_params", "w") as f:
            f.write(SCGI_PARAMS + "scgi_param  REMOTE_ADDR $remote_addr;\n")
        with open(ng + "/conf/nginx.conf", "w") as f:
            f.write("""
pid %(ng)s/nginx.pid;
error_log %(ng)s/logs/error.log info;
events { worker_connections 256; }
http {
    access_log off;
    client_body_temp_path %(ng)s/body; scgi_temp_path %(ng)s/scgi; proxy_temp_path %(ng)s/proxy;
    fastcgi_temp_path %(ng)s/fcgi; uwsgi_temp_path %(ng)s/uwsgi;
    upstream synoscgi {
        server unix:%(f)s max_fails=1 fail_timeout=5s;
        server unix:%(up)s backup;
    }
    server {
        listen 127.0.0.1:%(port)d;
        location ~ \\.cgi { scgi_pass synoscgi; include scgi_params; }
    }
}
""" % {"ng": ng, "f": cls.fsock, "up": cls.up, "port": cls.port})
        subprocess.run([NGINX, "-p", ng, "-c", ng + "/conf/nginx.conf"], check=True)
        assert wait_for(lambda: cls.get_status() and cls.get_status()["injection_active"], timeout=10)

    @classmethod
    def tearDownClass(cls):
        if cls.ng and os.path.exists(cls.ng + "/nginx.pid"):
            subprocess.run([NGINX, "-p", cls.ng, "-c", cls.ng + "/conf/nginx.conf", "-s", "stop"])
        for p in (cls.filter, cls.fake):
            if p and p.poll() is None:
                p.terminate()
                p.wait(30)
        if cls.engine:
            cls.engine.stop()
        if getattr(cls, "log", None):
            cls.log.close()
        shutil.rmtree(cls.d, ignore_errors=True)

    @classmethod
    def get_status(cls):
        try:
            with open(cls.status) as f:
                return json.load(f)
        except (OSError, ValueError):
            return None

    def call(self, params, sid="sid-alice"):
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=30)
        try:
            headers = {"Content-Type": "application/x-www-form-urlencoded"}
            if sid:
                headers["Cookie"] = "id=%s; other=1" % sid
            c.request("POST", "/webapi/entry.cgi", urlencode(params), headers)
            r = c.getresponse()
            self.assertEqual(r.status, 200)
            return json.loads(r.read())
        finally:
            c.close()

    def search(self, method, kw, sid="sid-alice", **extra):
        p = {"api": "SYNO.Foto.Search.Search", "version": "7", "method": method, "keyword": json.dumps(kw),
             "item_type": '"all"'}
        p.update(extra)
        return self.call(p, sid)

    def ids(self, method, kw, sid="sid-alice", **extra):
        return [x["id"] for x in self.search(method, kw, sid, **extra)["data"]["list"]]

    def test_01_merged_results_consistent_across_methods(self):
        # 原生找到 {2, 5}，engine 找到 {2, 3, 7, 11, 20, 25, 99}；99 是 bob 的，不能出現
        want = [25, 20, 11, 7, 5, 3, 2]
        self.assertEqual(self.search("count_item", "cat")["data"]["count"], 7)
        self.assertEqual(self.ids("list_basic_timeline_info", "cat"), want)
        pages = [self.ids("list_item", "cat", offset=str(o), limit="3") for o in (0, 3, 6, 9)]
        self.assertEqual(pages, [want[0:3], want[3:6], want[6:], []])
        sec = self.search("get_search_timeline", "cat", timeline_group_unit='"day"')["data"]["section"]
        self.assertEqual(sum(e["item_count"] for s in sec for e in s["list"]), 7)
        item = self.search("list_item", "cat", additional='["thumbnail","resolution"]')["data"]["list"][0]
        self.assertEqual(item["additional"]["thumbnail"]["cache_key"], "25_%d" % (T0 + 25 * DAY + 7))
        self.assertIn("resolution", item["additional"])

    def test_02_time_range_and_ignored_params(self):
        tr = {"start_time": str(T0 + 5 * DAY), "end_time": str(T0 + 12 * DAY)}
        self.assertEqual(self.ids("list_item", "cat", **tr), [11, 7, 5])
        self.assertEqual(self.ids("list_basic_timeline_info", "cat", **tr), [11, 7, 5])
        # 跟原生一樣：count_item 與 get_search_timeline 不理會時間範圍；sort_direction、item_type 也不理會
        self.assertEqual(self.search("count_item", "cat", **tr)["data"]["count"], 7)
        sec = self.search("get_search_timeline", "cat", **tr)["data"]["section"]
        self.assertEqual(sum(s["limit"] for s in sec), 7)
        self.assertEqual(self.ids("list_item", "cat", sort_direction='"asc"', limit="2"), [25, 20])
        self.assertEqual(self.search("count_item", "cat", item_type='"video"')["data"]["count"], 7)

    def test_03_other_user_gets_only_native_results(self):
        self.assertEqual(self.ids("list_item", "cat", sid="sid-bob"), [99])
        self.assertEqual(self.search("count_item", "cat", sid="sid-bob")["data"]["count"], 1)

    def test_04_invalid_session_untouched(self):
        r = self.search("list_item", "cat", sid="sid-nobody")
        self.assertEqual((r["success"], r["error"]["code"]), (False, 119))
        r = self.search("count_item", "cat", sid=None)
        self.assertEqual(r["error"]["code"], 119)

    def test_05_only_engine_hits_when_native_empty(self):
        self.assertEqual(self.ids("list_item", "dog"), [5, 2])
        self.assertEqual(self.search("count_item", "beach")["data"]["count"], 1)
        self.assertEqual(self.ids("list_item", "volcano"), [])

    def test_06_engine_down_falls_back_to_native(self):
        path = self.engine.path
        self.engine.stop()
        try:
            time.sleep(1.2)  # cache_ttl
            self.assertEqual(self.ids("list_item", "cat"), [5, 2])
            self.assertEqual(self.search("count_item", "cat")["data"]["count"], 2)
        finally:
            type(self).engine = FakeEngine(path)
        time.sleep(1.2)
        self.assertEqual(self.search("count_item", "cat")["data"]["count"], 7)

    def test_07_deleted_and_foreign_photos_skipped(self):
        # Browse.Item.get 只要有一張拿不到就整批失敗（假 DSM 用最嚴格的行為）→ filter 逐張重試，只留下拿得到的
        before = self.get_status()["stats"]["subrequests"]
        self.assertEqual(self.ids("list_item", "mixed"), [6, 4])
        self.assertEqual(self.search("count_item", "mixed")["data"]["count"], 2)
        time.sleep(1.1)
        self.assertGreaterEqual(self.get_status()["stats"]["subrequests"] - before, 5)
        self.assertEqual(self.get_status()["stats"]["inject_errors"], 0)

    def test_07b_app_style_sid_param(self):
        # 手機 App 用 _sid 參數而不是 cookie
        p = {"api": "SYNO.Foto.Search.Search", "version": "7", "method": "count_item", "keyword": '"cat"',
             "_sid": "sid-alice"}
        self.assertEqual(self.call(p, sid=None)["data"]["count"], 7)
        p["_sid"] = "sid-bob"
        self.assertEqual(self.call(p, sid=None)["data"]["count"], 1)

    def app_list_item(self, kw, sid="sid-alice", additional=fake_dsm.APP_ADDITIONAL):
        # 跟手機 App（Photos 1.9.0）實際送出的一樣：只送 list_item、version 5、14 種額外欄位、limit 1000
        return self.call({"api": "SYNO.Foto.Search.Search", "version": "5", "method": "list_item", "_sid": sid,
                          "keyword": json.dumps(kw), "offset": "0", "limit": "1000", "additional": additional,
                          "geocoding_accept_language": '"cht"'}, sid=None)

    def test_07c_mobile_app_request(self):
        time.sleep(1.2)
        items = self.app_list_item("cat")["data"]["list"]
        self.assertEqual([x["id"] for x in items], [25, 20, 11, 7, 5, 3, 2])
        native_keys = {k for k in items[4]}           # 5 是原生找到的
        for x in items:                                # 注入的照片跟原生的欄位完全相同
            self.assertEqual(set(x), native_keys)
            self.assertEqual(set(x["additional"]), set(items[4]["additional"]))
        self.assertEqual([x["id"] for x in self.app_list_item("dog")["data"]["list"]], [5, 2])

    def test_07d_unfetchable_items_fall_back_to_native(self):
        # 取不到要注入的照片資料：回傳原生結果（不是空清單），記成略過
        time.sleep(1.2)  # 狀態檔每秒更新一次，先讓上一個測試的計數寫進去
        before = self.get_status()["stats"]
        items = self.app_list_item("cat", additional=json.dumps(["thumbnail", "bogus_field"]))["data"]["list"]
        self.assertEqual([x["id"] for x in items], [5, 2])
        time.sleep(1.2)
        after = self.get_status()["stats"]
        self.assertEqual(after["injected"], before["injected"])
        self.assertEqual(after["inject_skipped"], before["inject_skipped"] + 1)

    def test_08_capture_redacts_auth(self):
        # 不是搜尋的 Photos 請求也記參數；看不懂的請求（例如 JSON body）記下原因
        self.call({"api": "SYNO.Foto.Browse.Item", "version": "1", "method": "list", "offset": "0", "limit": "1",
                   "_sid": "sid-alice"}, sid=None)
        self.call({"api": "SYNO.Foto.Thumbnail", "method": "get", "id": "2"})   # 縮圖：量大，不記
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        c.request("POST", "/webapi/entry.cgi", '{"api":"SYNO.Foto.Search.Search"}', {"Content-Type": "application/json"})
        c.getresponse().read()
        c.close()
        with open(self.capture) as f:
            recs = [json.loads(line) for line in f]
        for r in recs:
            self.assertNotIn("_sid", r["params"])
        kinds = {r["kind"] for r in recs}
        self.assertTrue({"native", "sub", "request", "skipped"} <= kinds, kinds)
        self.assertIn({"kind": "request", "api": "SYNO.Foto.Browse.Item"},
                      [{"kind": r["kind"], "api": r["params"].get("api")} for r in recs])
        self.assertNotIn("SYNO.Foto.Thumbnail", [r["params"].get("api") for r in recs])
        skipped = [r["response"] for r in recs if r["kind"] == "skipped"]
        self.assertEqual(skipped[-1]["reason"], "Content-Type application/json")
        self.assertEqual(oct(os.stat(self.capture).st_mode & 0o777), "0o600")

    def test_09_status_counters(self):
        st = self.get_status()
        self.assertEqual(st["mode"], "engine")
        self.assertGreater(st["stats"]["injected"], 0)
        self.assertEqual(st["stats"]["inject_errors"], 0)


if __name__ == "__main__":
    unittest.main()
