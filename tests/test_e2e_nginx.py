#!/usr/bin/env python3
# 端對端測試：真的 nginx（設定成跟 sse install 套用的一樣：filter 為主、synoscgi 為 backup）
# + sse-filter + 假 synoscgi，驗證轉發、注入、大檔案、filter 掛掉時的退回行為。
#
#   SSE_NGINX_BIN=/path/to/nginx SSE_FILTER_PY=/path/to/python3.8 python3 -m unittest tests.test_e2e_nginx -v
#
# macOS 的 unix socket 路徑上限 104 bytes，可用 SSE_TEST_TMP 指定較短的暫存目錄。
import hashlib
import http.client
import json
import os
import shutil
import signal
import socket
import subprocess
import sys
import tempfile
import threading
import time
import unittest
from urllib.parse import urlencode

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
NGINX = os.environ.get("SSE_NGINX_BIN")
FILTER_PY = os.environ.get("SSE_FILTER_PY", sys.executable)

SCGI_PARAMS = """
scgi_param  REQUEST_METHOD     $request_method;
scgi_param  REQUEST_URI        $request_uri;
scgi_param  QUERY_STRING       $query_string;
scgi_param  CONTENT_TYPE       $content_type;
scgi_param  DOCUMENT_URI       $document_uri;
scgi_param  SCGI               1;
scgi_param  SERVER_PROTOCOL    $server_protocol;
scgi_param  REMOTE_ADDR        $remote_addr;
scgi_param  SERVER_PORT        $server_port;
"""

DEMO_ITEM = {"additional": {"thumbnail": {"cache_key": "90001_1750004000", "m": "ready", "preview": "broken",
                                          "sm": "ready", "unit_id": 90001, "xl": "ready"}},
             "filename": "IMG_0001.jpg", "filesize": 123456, "folder_id": 7, "id": 90001,
             "indexed_time": 1750003000000, "owner_user_id": 1, "time": 1750000000, "type": "photo"}


def free_port():
    s = socket.socket()
    s.bind(("127.0.0.1", 0))
    port = s.getsockname()[1]
    s.close()
    return port


def wait_for(cond, timeout=10, step=0.1):
    end = time.time() + timeout
    while time.time() < end:
        if cond():
            return True
        time.sleep(step)
    return False


def sock_alive(path):
    s = socket.socket(socket.AF_UNIX)
    s.settimeout(1)
    try:
        s.connect(path)
        return True
    except OSError:
        return False
    finally:
        s.close()


def rss_kb(pid):
    out = subprocess.run(["ps", "-o", "rss=", "-p", str(pid)], capture_output=True, text=True).stdout.strip()
    return int(out) if out else 0


class RssSampler(threading.Thread):
    def __init__(self, pid):
        super().__init__(daemon=True)
        self.pid, self.peak, self.running = pid, 0, True

    def run(self):
        while self.running:
            self.peak = max(self.peak, rss_kb(self.pid))
            time.sleep(0.1)


@unittest.skipUnless(NGINX, "需要 SSE_NGINX_BIN 指向 nginx 執行檔")
class E2E(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.fake = cls.filter = cls.ng = cls.childlog = None
        try:
            cls._setup()
        except BaseException:
            cls.tearDownClass()  # setUpClass 失敗時 unittest 不會呼叫 tearDownClass，子程序要自己收
            raise

    @classmethod
    def _setup(cls):
        cls.d = tempfile.mkdtemp(prefix="sse.", dir=os.environ.get("SSE_TEST_TMP"))
        d = cls.d
        cls.up = os.path.join(d, "up.sock")
        cls.fsock = os.path.join(d, "f.sock")
        cls.reqlog = os.path.join(d, "requests.log")
        cls.status = os.path.join(d, "status.json")
        cls.kill = os.path.join(d, "DISABLE")
        cls.info = os.path.join(d, "INFO")
        cls.port = free_port()
        with open(cls.info, "w") as f:
            f.write('package="SynologyPhotos"\nversion="1.8.1-10089"\n')
        cls.cfg = os.path.join(d, "filter.json")
        with open(cls.cfg, "w") as f:
            json.dump({"listen": cls.fsock, "socket_mode": "0600", "upstream": cls.up, "status_file": cls.status,
                       "kill_switch": cls.kill, "photos_info": cls.info, "mode": "demo",
                       "inject_photos_version": "1.8.1-10089",
                       "demo": {"keyword": "test", "items": [DEMO_ITEM]}}, f)
        cls.childlog = open(os.path.join(d, "children.log"), "a")
        cls.fake = subprocess.Popen([sys.executable, os.path.join(HERE, "fake_synoscgi.py"), cls.up, cls.reqlog],
                                    stdout=subprocess.DEVNULL, stderr=cls.childlog)
        assert wait_for(lambda: sock_alive(cls.up)), "假 synoscgi 沒起來"
        cls.start_filter()
        ng = os.path.join(d, "nginx")
        os.makedirs(os.path.join(ng, "conf"))
        os.makedirs(os.path.join(ng, "logs"))
        with open(os.path.join(ng, "conf", "scgi_params"), "w") as f:
            f.write(SCGI_PARAMS)
        with open(os.path.join(ng, "conf", "nginx.conf"), "w") as f:
            f.write("""
pid %(ng)s/nginx.pid;
error_log %(ng)s/logs/error.log info;
events { worker_connections 256; }
http {
    access_log off;
    client_max_body_size 0;
    client_body_temp_path %(ng)s/body; scgi_temp_path %(ng)s/scgi; proxy_temp_path %(ng)s/proxy;
    fastcgi_temp_path %(ng)s/fcgi; uwsgi_temp_path %(ng)s/uwsgi;
    upstream synoscgi {
        server unix:%(f)s max_fails=1 fail_timeout=5s;
        server unix:%(up)s backup;
    }
    server {
        listen 127.0.0.1:%(port)d;
        location ~ \\.cgi { scgi_pass synoscgi; include scgi_params; scgi_read_timeout 600; }
    }
}
""" % {"ng": ng, "f": cls.fsock, "up": cls.up, "port": cls.port})
        cls.ng = ng
        subprocess.run([NGINX, "-p", ng, "-c", os.path.join(ng, "conf", "nginx.conf")], check=True)
        assert wait_for(lambda: cls.http("GET", "/webapi/query.cgi?api=SYNO.API.Info")[0] == 200), "nginx 沒起來"

    @classmethod
    def tearDownClass(cls):
        if cls.ng and os.path.exists(os.path.join(cls.ng, "nginx.pid")):
            subprocess.run([NGINX, "-p", cls.ng, "-c", os.path.join(cls.ng, "conf", "nginx.conf"), "-s", "stop"])
        cls.stop_filter()
        if cls.fake:
            cls.fake.terminate()
            cls.fake.wait()
        if cls.childlog:
            cls.childlog.close()
        shutil.rmtree(cls.d, ignore_errors=True)

    @classmethod
    def start_filter(cls):
        cls.filter = subprocess.Popen([FILTER_PY, "-I", os.path.join(REPO, "sse", "data", "sse-filter.py"), "--config", cls.cfg],
                                      stdout=subprocess.DEVNULL, stderr=cls.childlog)
        assert wait_for(lambda: sock_alive(cls.fsock)), "filter 沒起來"

    @classmethod
    def stop_filter(cls, sig=signal.SIGTERM):
        if cls.filter and cls.filter.poll() is None:
            cls.filter.send_signal(sig)
            cls.filter.wait(30)
        cls.filter = None

    @classmethod
    def http(cls, method, path, body=None, headers=None, timeout=120):
        c = http.client.HTTPConnection("127.0.0.1", cls.port, timeout=timeout)
        try:
            c.request(method, path, body=body, headers=headers or {})
            r = c.getresponse()
            return r.status, r.read()
        except (ConnectionError, OSError):
            return 0, b""
        finally:
            c.close()

    def post_form(self, path, params, timeout=120):
        return self.http("POST", path, urlencode(params).encode(),
                         {"Content-Type": "application/x-www-form-urlencoded"}, timeout)

    def search(self, method, keyword, **extra):
        p = {"api": "SYNO.Foto.Search.Search", "version": "7", "method": method, "keyword": json.dumps(keyword)}
        p.update(extra)
        status, body = self.post_form("/webapi/entry.cgi", p)
        self.assertEqual(status, 200)
        return json.loads(body)

    def stats(self):
        with open(self.status) as f:
            return json.load(f)

    def rid_count(self, rid):
        with open(self.reqlog) as f:
            return sum(1 for line in f if line.split()[1] == rid)

    def connections(self):
        wait_for(lambda: False, timeout=1.2)  # 狀態檔每秒更新一次
        return self.stats()["stats"]["connections"]

    # ── 轉發與注入 ──

    def test_01_traffic_goes_through_filter(self):
        before = self.connections()
        status, body = self.http("GET", "/webapi/query.cgi?api=SYNO.API.Info")
        self.assertEqual(status, 200)
        self.assertTrue(json.loads(body)["success"])
        self.assertGreater(self.connections(), before)

    def test_02_demo_injection(self):
        wait_for(lambda: self.stats()["injection_active"], timeout=7)
        li = self.search("list_item", "test")["data"]["list"]
        self.assertEqual([x["id"] for x in li], [90001, 1])
        self.assertEqual(li[0], DEMO_ITEM)
        self.assertEqual(self.search("count_item", "test")["data"]["count"], 2)
        sec = self.search("get_search_timeline", "test")["data"]["section"]
        # 跟真實 DSM 回應的 section 格式完全相同
        self.assertEqual(sec[0], {"limit": 100, "list": [{"day": 15, "item_count": 1, "month": 6, "year": 2025}],
                                  "offset": 0})
        basic = self.search("list_basic_timeline_info", "test")["data"]["list"]
        self.assertEqual(basic[0], {"id": 90001, "owner_user_id": 1, "time": 1750000000})
        # 其他關鍵字、第二頁、FotoTeam：都不動
        self.assertEqual([x["id"] for x in self.search("list_item", "dog")["data"]["list"]], [1])
        self.assertEqual([x["id"] for x in self.search("list_item", "test", offset="100")["data"]["list"]], [1])
        status, body = self.post_form("/webapi/entry.cgi", {"api": "SYNO.FotoTeam.Search.Search", "method": "list_item",
                                                            "keyword": '"test"'})
        self.assertNotIn(b"90001", body)

    def test_03_large_upload_streams(self):
        size = 200 * 1024 * 1024
        block = os.urandom(1 << 20)
        expect = hashlib.sha256()
        for _ in range(size >> 20):
            expect.update(block)
        sampler = RssSampler(self.filter.pid)
        sampler.start()
        status, body = self.http("POST", "/webapi/entry.cgi?api=SYNO.Foto.Upload.Item&rid=up1",
                                 (block for _ in range(size >> 20)),
                                 {"Content-Type": "application/octet-stream", "Content-Length": str(size)})
        sampler.running = False
        self.assertEqual(status, 200)
        got = json.loads(body)["data"]
        self.assertEqual((got["len"], got["sha256"]), (size, expect.hexdigest()))
        self.assertLess(sampler.peak, 64 * 1024, "filter 記憶體用量 %d KB，疑似整包緩衝" % sampler.peak)

    def test_04_large_download_streams(self):
        size = 200 * 1024 * 1024
        block = hashlib.sha256(b"x").digest() * 2048
        expect = hashlib.sha256()
        sent = 0
        while sent < size:
            piece = block[:min(len(block), size - sent)]
            expect.update(piece)
            sent += len(piece)
        sampler = RssSampler(self.filter.pid)
        sampler.start()
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=120)
        c.request("GET", "/webapi/entry.cgi?api=SYNO.Foto.Download&bigresp=%d" % size)
        r = c.getresponse()
        got = hashlib.sha256()
        n = 0
        while True:
            chunk = r.read(1 << 20)
            if not chunk:
                break
            got.update(chunk)
            n += len(chunk)
        c.close()
        sampler.running = False
        self.assertEqual((n, got.hexdigest()), (size, expect.hexdigest()))
        self.assertLess(sampler.peak, 64 * 1024, "filter 記憶體用量 %d KB，疑似整包緩衝" % sampler.peak)

    def test_05_kill_switch_and_photos_version(self):
        open(self.kill, "w").close()
        try:
            self.assertTrue(wait_for(lambda: not self.stats()["injection_active"], timeout=8))
            self.assertEqual([x["id"] for x in self.search("list_item", "test")["data"]["list"]], [1])
        finally:
            os.unlink(self.kill)
        self.assertTrue(wait_for(lambda: self.stats()["injection_active"], timeout=8))
        with open(self.info, "w") as f:
            f.write('version="1.9.0-20000"\n')
        try:
            self.assertTrue(wait_for(lambda: not self.stats()["injection_active"], timeout=8))
            self.assertIn("Photos 版本", self.stats()["injection_reason"])
            self.assertEqual([x["id"] for x in self.search("list_item", "test")["data"]["list"]], [1])
        finally:
            with open(self.info, "w") as f:
                f.write('version="1.8.1-10089"\n')
        self.assertTrue(wait_for(lambda: self.stats()["injection_active"], timeout=8))

    # ── filter 掛掉時的退回 ──

    def test_06_filter_stopped_falls_back_once(self):
        self.stop_filter()
        try:
            for i in range(5):
                status, body = self.post_form("/webapi/entry.cgi", {"api": "X", "rid": "down%d" % i})
                self.assertEqual(status, 200)
                self.assertEqual(self.rid_count("down%d" % i), 1, "POST 被送了不只一次")
        finally:
            self.start_filter()

    def test_07_filter_killed_mid_request_no_duplicate(self):
        time.sleep(6)  # 上一個測試停過 filter，等 fail_timeout 過去，確保這個 request 會走 filter
        result = {}

        def slow():
            result["r"] = self.post_form("/webapi/entry.cgi", {"api": "X", "rid": "mid", "sleep": "2"})

        before = self.stats()["stats"]["connections"]
        t = threading.Thread(target=slow)
        t.start()
        self.assertTrue(wait_for(lambda: os.path.exists(self.reqlog) and self.rid_count("mid") == 1, timeout=5))
        self.assertTrue(wait_for(lambda: self.stats()["stats"]["connections"] > before, timeout=3),
                        "這個 request 沒有經過 filter，測試前提不成立")
        self.stop_filter(signal.SIGKILL)
        t.join(30)
        try:
            # synoscgi 已收到 request：nginx 不能把 POST 再送給 backup 一次
            time.sleep(2.5)
            self.assertEqual(self.rid_count("mid"), 1, "filter 中途掛掉後 POST 被重送")
            self.assertEqual(result["r"][0], 502)
        finally:
            self.start_filter()

    def test_08_back_to_filter_after_restart(self):
        self.stop_filter()
        self.assertEqual(self.http("GET", "/webapi/query.cgi?api=SYNO.API.Info")[0], 200)
        self.start_filter()
        time.sleep(6)  # fail_timeout=5s 之後 nginx 會再試主要的 upstream
        before = self.connections()
        for _ in range(3):
            self.assertEqual(self.http("GET", "/webapi/query.cgi?api=SYNO.API.Info")[0], 200)
        self.assertGreater(self.connections(), before)


if __name__ == "__main__":
    unittest.main()
