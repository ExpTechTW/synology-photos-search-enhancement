#!/usr/bin/env python3
# 安裝 / 維運 / 解除安裝的完整流程：照 README 的步驟跑真的 install.sh 與 sse 指令。
# 沙盒裡模擬 DSM 的檔案配置；真的 nginx（DSM 風格的 nginx.conf）、真的 sse（filter、索引器、搜尋服務都是它）；
# 假 DSM（兩個帳號）、假 LLM、假 systemctl、假 GitHub（release 由 scripts/package.sh 編譯打包）。
#
#   make build/sse && SSE_BIN=$PWD/build/sse SSE_NGINX_BIN=/path/to/nginx SSE_TEST_TMP=/tmp \
#       python3 -m unittest tests.test_install_sandbox -v
import grp
import hashlib
import http.client
import json
import os
import pwd
import shutil
import socket
import subprocess
import sys
import tarfile
import tempfile
import time
import unittest
from urllib.parse import urlencode

HERE = os.path.dirname(os.path.abspath(__file__))
REPO = os.path.dirname(HERE)
sys.path.insert(0, HERE)

import fake_github  # noqa: E402
import fake_llm  # noqa: E402

NGINX = os.environ.get("SSE_NGINX_BIN")
SSE_BIN = os.environ.get("SSE_BIN")
USER = pwd.getpwuid(os.getuid()).pw_name
UNITS = ("sse-filter.service", "sse-nginx-guard.service", "sse-nginx-guard.path", "sse-nginx-guard.timer",
         "sse-engine.service", "sse-indexer.service")
CATS = [105, 104, 103, 102, 101]    # alice 的貓（engine 找到的，含原生的 102、101）
NATIVE_CATS = [102, 101]            # 原生搜尋（標籤）找到的


def sha(data):
    return hashlib.sha256(data).hexdigest()


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


def dsm_login_umask():
    """DSM 的 /etc/profile 設了 umask 077：使用者從 SSH 登入後執行安裝就是這個環境。"""
    os.umask(0o077)


def mode(path):
    return os.stat(path).st_mode & 0o777


def break_serve(src):
    """做一個壞掉的版本：搜尋服務（sse … serve）起不來，其他指令正常。"""
    path = os.path.join(src, "cmd/sse/main.go")
    with open(path) as f:
        text = f.read()
    head = "func run(argv []string) int {\n"
    assert head in text
    with open(path, "w") as f:
        f.write(text.replace(head, head + '\tif len(argv) > 0 && argv[len(argv)-1] == "serve" {\n'
                             '\t\tfmt.Fprintln(os.Stderr, "broken build")\n\t\treturn 1\n\t}\n', 1))


def wait_for(cond, timeout=10):
    end = time.time() + timeout
    while time.time() < end:
        if cond():
            return True
        time.sleep(0.3)
    return False


@unittest.skipUnless(NGINX and SSE_BIN, "需要 SSE_NGINX_BIN 指向 nginx 執行檔、SSE_BIN 指向 sse 執行檔（make build/sse）")
class Install(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.fake = cls.llm_srv = cls.gh = None
        cls.root = tempfile.mkdtemp(prefix="sbx.", dir=os.environ.get("SSE_TEST_TMP"))
        try:
            cls._setup()
        except BaseException:
            cls.tearDownClass()
            raise

    @classmethod
    def _setup(cls):
        r = cls.root
        for d in ("bin", "run", "var/ng", "etc/nginx", "etc.defaults", "var/packages/SynologyPhotos", "home"):
            os.makedirs(os.path.join(r, d), exist_ok=True)
        cls.write("etc.defaults/VERSION", 'majorversion="7"\nproductversion="7.3.2"\nbuildnumber="86009"\n')
        cls.write("var/packages/SynologyPhotos/INFO", 'package="SynologyPhotos"\nversion="1.9.0-10120"\n')
        cls.write("etc/nginx/scgi_params", "scgi_param REQUEST_METHOD $request_method;\n"
                  "scgi_param REQUEST_URI $request_uri;\nscgi_param QUERY_STRING $query_string;\n"
                  "scgi_param CONTENT_TYPE $content_type;\nscgi_param HTTP_COOKIE $http_cookie;\n")
        s = socket.socket()
        s.bind(("127.0.0.1", 0))
        cls.port = s.getsockname()[1]
        s.close()
        me = pwd.getpwuid(os.getuid())
        cls.conf_path = os.path.join(r, "etc/nginx/nginx.conf")
        cls.original = """user %(user)s %(group)s;
pid %(r)s/run/nginx.pid;
error_log %(r)s/var/nginx-error.log info;
events { worker_connections 256; }
http {
    access_log off;
    client_body_temp_path %(r)s/var/ng/body; scgi_temp_path %(r)s/var/ng/scgi; proxy_temp_path %(r)s/var/ng/proxy;
    fastcgi_temp_path %(r)s/var/ng/fcgi; uwsgi_temp_path %(r)s/var/ng/uwsgi;

    upstream synoscgi {
        server unix:%(r)s/run/synoscgi.sock;
    }

    server {
        listen 127.0.0.1:%(port)d;
        location ~ \\.cgi {
            include scgi_params;
            scgi_pass synoscgi;
        }
    }
}
""" % {"user": me.pw_name, "group": grp.getgrgid(me.pw_gid).gr_name, "r": r, "port": cls.port}
        cls.write("etc/nginx/nginx.conf", cls.original)
        cls.write("bin/nginx", "#!/bin/sh\nexec %s -e %s/var/nginx-startup.log \"$@\"\n" % (NGINX, r), 0o755)
        cls.write("bin/systemctl", "#!/bin/sh\nexec %s %s/fake_systemctl.py \"$@\"\n" % (sys.executable, HERE), 0o755)
        env = {k: v for k, v in os.environ.items() if k not in ("SSE_BINARY", "SSE_SOURCE", "SSE_CHANNEL", "SSE_WORK")}
        cls.env = dict(env, SSE_ROOT=r, SSE_NGINX=os.path.join(r, "bin/nginx"),
                       SSE_SYSTEMCTL=os.path.join(r, "bin/systemctl"), SSE_ENGINE_WAIT="20")
        cls.fake = subprocess.Popen([sys.executable, os.path.join(HERE, "fake_synoscgi.py"),
                                     os.path.join(r, "run/synoscgi.sock"), os.path.join(r, "var/requests.log"),
                                     "dsm-engine"], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
        assert wait_for(lambda: sock_alive(os.path.join(r, "run/synoscgi.sock")))
        subprocess.run([cls.env["SSE_NGINX"], "-c", cls.conf_path], check=True)
        cls.probe = "http://127.0.0.1:%d/webapi/query.cgi?api=SYNO.API.Info&version=1&method=query" % cls.port
        assert wait_for(cls.dsm_ok)
        cls.llm = fake_llm.FakeLLM()
        cls.llm_srv = fake_llm.serve(cls.llm)
        cls.data = os.path.join(r, "home", USER, "sse")
        cls.engine_sock = os.path.join(r, "run/sse-engine/engine.sock")
        cls.good_cfg = os.path.join(r, "good.yml")
        cls.write("good.yml", cls.config_text(), 0o600)
        cls.gh = fake_github.FakeGitHub(os.path.join(r, "github"))
        cls.env["SSE_GITHUB_API"] = cls.gh.url
        cls.env["SSE_GITHUB_URL"] = cls.gh.web

    @classmethod
    def config_text(cls, **sections):
        doc = {
            "synology": {"url": "http://127.0.0.1:%d" % cls.port, "account": "alice", "password": "pw-alice"},
            "embedding": {"base_url": "http://127.0.0.1:%d/v1" % cls.llm_srv.server_port, "api_key": "sk-test"},
            "ocr": {"model": "fake-vision"},
            "search": {"socket": os.path.join(cls.root, "run/sse-engine/engine.sock"), "refresh_interval": 1},
        }
        for sec, vals in sections.items():
            doc.setdefault(sec, {}).update(vals)
        return json.dumps(doc, ensure_ascii=False, indent=2)   # JSON 也是合法的 YAML

    @classmethod
    def tearDownClass(cls):
        r = cls.root
        if os.path.exists(os.path.join(r, "bin/systemctl")):
            subprocess.run([os.path.join(r, "bin/systemctl"), "stop"] + list(UNITS), env=dict(os.environ, SSE_ROOT=r))
        if os.path.exists(os.path.join(r, "run/nginx.pid")):
            subprocess.run([os.path.join(r, "bin/nginx"), "-c", cls.conf_path, "-s", "stop"])
        if cls.fake:
            cls.fake.terminate()
            cls.fake.wait()
        if cls.llm_srv:
            cls.llm_srv.shutdown()
        if cls.gh:
            cls.gh.stop()
        shutil.rmtree(r, ignore_errors=True)

    @classmethod
    def write(cls, rel, text, mode=0o644):
        path = os.path.join(cls.root, rel)
        with open(path, "w") as f:
            f.write(text)
        os.chmod(path, mode)

    @classmethod
    def dsm_ok(cls):
        c = http.client.HTTPConnection("127.0.0.1", cls.port, timeout=5)
        try:
            c.request("GET", cls.probe.split(str(cls.port), 1)[1])
            r = c.getresponse()
            return r.status == 200 and json.loads(r.read())["success"]
        except (OSError, ValueError):
            return False
        finally:
            c.close()

    # ── 工具 ──

    def install(self, *args):
        """sudo SSE_BINARY=build/sse ./install.sh …（安裝本機編譯的 sse）"""
        r = subprocess.run(["sh", os.path.join(REPO, "install.sh")] + list(args), env=dict(self.env, SSE_BINARY=SSE_BIN),
                           preexec_fn=dsm_login_umask, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                           stderr=subprocess.STDOUT, text=True, timeout=600)
        return r.returncode, r.stdout

    def curl_sh(self, *args):
        """curl -fsSL …/install.sh | sudo sh -s -- …：stdin 是腳本本身，$0 是 sh。"""
        with open(os.path.join(REPO, "install.sh"), "rb") as f:
            script = f.read()
        r = subprocess.run(["sh", "-s", "--"] + list(args), input=script, env=self.env, preexec_fn=dsm_login_umask,
                           stdout=subprocess.PIPE,
                           stderr=subprocess.STDOUT, timeout=600, start_new_session=True)   # 沒有 /dev/tty
        return r.returncode, r.stdout.decode()

    def version(self):
        return self.sse("--version")[1].strip()

    def check_modes(self):
        """在 umask 077 下安裝，filter（http）與照片擁有者照樣要能執行程式、讀 filter 的設定。"""
        for rel in ("usr/local/sse", "usr/local/sse/sse", "usr/local/etc/sse", "usr/local/bin/sse"):
            self.assertEqual(mode(self.p(rel)), 0o755, rel)
        self.assertEqual(os.readlink(self.p("usr/local/bin/sse")), self.p("usr/local/sse/sse"))
        self.assertEqual(mode(self.p("usr/local/etc/sse/filter.json")) & 0o044, 0o044)
        self.assertEqual(mode(self.data), 0o700)                    # config.yml 與照片裡的文字：只有擁有者
        self.assertEqual(mode(os.path.join(self.data, "config.yml")), 0o600)
        self.assertEqual(sorted(os.listdir(self.p("usr/local/sse"))), ["sse"])   # 沒有 sse.prev 或 Python 版的東西
        self.no_work_dirs()

    def no_work_dirs(self):
        """下載用的暫存目錄（/usr/local/.sse-work.*）都清掉了。"""
        self.assertEqual([n for n in os.listdir(self.p("usr/local")) if n.startswith(".sse-work")]
                         if os.path.isdir(self.p("usr/local")) else [], [])

    def install_args(self, *extra):
        return ["--user", USER, "--data-dir", self.data, "--probe-url", self.probe, "--yes"] + list(extra)

    def sse(self, *args):
        """sudo sse …（install 建立的指令）"""
        r = subprocess.run([self.p("usr/local/bin/sse")] + list(args), env=self.env, stdin=subprocess.DEVNULL,
                           preexec_fn=dsm_login_umask,
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=240)
        return r.returncode, r.stdout

    def p(self, rel):
        return os.path.join(self.root, rel)

    def read(self, rel):
        with open(self.p(rel), "rb") as f:
            return f.read()

    def conf(self):
        with open(self.conf_path) as f:
            return f.read()

    def set_conf(self, text):
        with open(self.conf_path, "w") as f:
            f.write(text)
        subprocess.run([self.env["SSE_NGINX"], "-c", self.conf_path, "-s", "reload"], check=True,
                       stderr=subprocess.DEVNULL)
        time.sleep(1)

    def edit_config(self, **sections):
        with open(os.path.join(self.data, "config.yml"), "w") as f:   # 沿用原本的 600 權限
            f.write(self.config_text(**sections))

    def login(self, account, pw):
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=10)
        try:
            c.request("POST", "/webapi/entry.cgi", urlencode({"api": "SYNO.API.Auth", "version": "6", "method": "login",
                                                              "account": account, "passwd": pw, "format": "sid"}),
                      {"Content-Type": "application/x-www-form-urlencoded"})
            return json.loads(c.getresponse().read())["data"]["sid"]
        finally:
            c.close()

    def find(self, account, keyword, method="list_item"):
        sid = self.login(account, "pw-" + account)
        c = http.client.HTTPConnection("127.0.0.1", self.port, timeout=30)
        try:
            c.request("POST", "/webapi/entry.cgi",
                      urlencode({"api": "SYNO.Foto.Search.Search", "version": "7", "method": method,
                                 "keyword": json.dumps(keyword), "additional": '["thumbnail"]'}),
                      {"Content-Type": "application/x-www-form-urlencoded", "Cookie": "id=" + sid})
            d = json.loads(c.getresponse().read())["data"]
            return d["count"] if method == "count_item" else [x["id"] for x in d["list"]]
        finally:
            c.close()

    def finds(self, keyword, expected, account="alice", timeout=12):
        """filter 重啟後 nginx 會先直連 synoscgi 一陣子（fail_timeout），所以用等的。"""
        got = []

        def check():
            got[:] = [self.find(account, keyword)]
            return got[0] == expected
        return wait_for(check, timeout) or self.fail("搜尋 %r 得到 %r，預期 %r" % (keyword, got[0], expected))

    def progress(self):
        rc, out = self.sse("progress", "--json")
        return json.loads(out) if rc == 0 else {}

    def nothing_installed(self):
        """系統回到原狀（第一次安裝失敗時，下載的程式也不留）。"""
        rels = ["usr/local/sse", "usr/local/etc/sse", "usr/local/bin/sse", "etc/nginx/nginx.conf.sse-orig", "var/log/sse",
                "run/sse", "run/sse-engine"] + ["etc/systemd/system/" + u for u in UNITS]
        for rel in rels:
            self.assertFalse(os.path.lexists(self.p(rel)), rel)
        self.assertEqual(self.conf(), self.original)
        self.assertTrue(self.dsm_ok())
        self.no_work_dirs()

    # ── 安裝前 ──

    def test_01_dry_run_leaves_no_trace(self):
        rc, out = self.install("--dry-run", *self.install_args())
        self.assertEqual(rc, 0, out)
        self.assertIn("會建立 %s" % os.path.join(self.data, "config.yml"), out)
        self.assertIn("沒有做任何變更", out)
        self.nothing_installed()
        self.assertFalse(os.path.exists(self.data))

    def test_02_preflight_lists_all_problems(self):
        self.write("etc/nginx/nginx.conf", self.original.replace("synoscgi.sock;", "other-filter.sock;"))
        self.write("var/packages/SynologyPhotos/INFO", "")
        try:
            rc, out = self.install(*self.install_args())
        finally:
            self.write("etc/nginx/nginx.conf", self.original)
            self.write("var/packages/SynologyPhotos/INFO", 'version="1.9.0-10120"\n')
        self.assertEqual(rc, 1, out)
        self.assertIn("其他 filter", out)
        self.assertIn("找不到 Synology Photos", out)
        self.assertIn("有 2 項檢查沒通過，沒有做任何變更", out)
        self.nothing_installed()

    def test_03_first_run_without_terminal_writes_template(self):
        rc, out = self.install(*self.install_args())
        self.assertEqual(rc, 1, out)
        self.assertIn("已建立設定檔範本", out)
        path = os.path.join(self.data, "config.yml")
        self.assertEqual(oct(os.stat(path).st_mode & 0o777), "0o600")
        with open(path) as f:
            text = f.read()
        self.assertIn('account: "%s"' % USER, text)
        self.assertIn("# ", text)   # 範本的說明都留著
        self.nothing_installed()

    def test_04_rollback_when_engine_does_not_start(self):
        # 搜尋服務起不來（socket 的目錄其實是檔案）：已經改過 nginx，要全部回復
        bad = self.p("bad.yml")
        with open(bad, "w") as f:
            f.write(self.config_text(search={"socket": self.p("etc.defaults/VERSION/engine.sock")}))
        os.chmod(bad, 0o600)
        rc, out = self.install(*self.install_args("--from-config", bad))
        self.assertEqual(rc, 1, out)
        self.assertIn("流量確認經過 filter", out)              # 走到 nginx 之後才失敗
        self.assertIn("搜尋服務沒有在 20 秒內回應", out)
        self.assertIn("還原 nginx.conf", out)
        self.assertIn("回復完成，DSM 回應正常", out)
        self.nothing_installed()

    # ── 安裝 ──

    def test_05_install(self):
        rc, out = self.install(*self.install_args("--from-config", self.good_cfg))
        self.assertEqual(rc, 0, out)
        self.assertIn("✓ DSM 登入", out)
        self.assertIn("✓ OCR", out)
        self.assertIn("流量確認經過 filter", out)
        self.assertIn("模式：engine", out)
        conf = self.conf()
        self.assertIn("run/sse/filter.sock max_fails=1 fail_timeout=5s;", conf)
        self.assertIn("run/synoscgi.sock backup;", conf)
        self.assertEqual(sha(self.read("etc/nginx/nginx.conf.sse-orig")), sha(self.original.encode()))
        self.assertTrue(sock_alive(self.p("run/sse/filter.sock")))
        self.assertTrue(sock_alive(self.engine_sock))
        # config.yml 放在資料目錄，權限 600；filter.json 由它產生
        cfg = os.path.join(self.data, "config.yml")
        self.assertEqual(oct(os.stat(cfg).st_mode & 0o777), "0o600")
        with open(cfg) as f:
            self.assertEqual(f.read(), self.config_text())
        filter_cfg = json.loads(self.read("usr/local/etc/sse/filter.json"))
        self.assertEqual((filter_cfg["mode"], filter_cfg["engine_socket"]), ("engine", self.engine_sock))
        self.assertNotIn("pw-alice", json.dumps(filter_cfg))   # 密碼不會進 filter 的設定
        self.check_modes()
        state = json.loads(self.read("usr/local/etc/sse/state.json"))
        self.assertEqual((state["dsm_build"], state["user"], state["data_dir"]),
                         ("86009", USER, os.path.realpath(self.data)))
        # log：filter 寫 /var/log/sse/filter.log；安裝過程（含安裝前檢查）記在 sse.log
        self.assertEqual(oct(os.stat(self.p("var/log/sse")).st_mode & 0o777), "0o750")
        self.assertIn(b"sse-filter", self.read("var/log/sse/filter.log"))
        audit = self.read("var/log/sse/sse.log").decode()
        self.assertIn("安裝前檢查", audit)
        self.assertIn("流量確認經過 filter", audit)

    def test_06_indexes_and_searches(self):
        self.assertTrue(wait_for(lambda: (self.progress().get("ocr") or {}).get("percent") == 100.0, timeout=90),
                        self.sse("progress")[1])
        self.assertEqual(self.progress()["embedding"]["percent"], 100.0)
        self.finds("cat", CATS)
        self.assertEqual(self.find("alice", "cat", "count_item"), 5)
        self.assertEqual(self.find("alice", "38475629"), [200])            # 照片裡的文字
        self.assertEqual(self.find("alice", "停車"), [201])
        self.assertEqual(self.find("bob", "cat"), [300])                    # bob 看不到 alice 的照片
        self.assertEqual(self.find("alice", "volcano"), [])
        rc, out = self.sse("search", "cat")
        self.assertEqual(rc, 0, out)
        self.assertIn("105", out)

    def test_07_status_doctor_logs(self):
        before = os.path.getsize(self.p("var/log/sse/sse.log"))
        rc, out = self.sse("status")
        self.assertEqual(rc, 0, out)
        self.assertIn("filter：模式 engine", out)
        self.assertIn("搜尋服務：索引 20 張", out)
        self.assertIn("20 / 20（100.0%）", out)
        self.assertIn("DSM 健康檢查：正常", out)
        rc, out = self.sse("doctor")
        self.assertEqual(rc, 0, out)
        self.assertIn("沒有發現問題", out)
        rc, out = self.sse("logs", "filter", "-n", "5")
        self.assertEqual(rc, 0, out)
        self.assertIn("filter.log", out)
        rc, out = self.sse("logs")
        self.assertEqual(rc, 0, out)
        self.assertIn("完整掃描：20 張", out)
        self.assertEqual(os.path.getsize(self.p("var/log/sse/sse.log")), before)   # 唯讀指令不寫 sse.log
        self.assertTrue(os.path.exists(os.path.join(self.data, "logs/engine.log")))

    def test_08_apply_config_changes(self):
        filter_json = self.read("usr/local/etc/sse/filter.json")
        # 寫錯的設定：拒絕套用，什麼都不動
        self.edit_config(search={"min_scor": 0.5})
        rc, out = self.sse("apply")
        self.assertEqual(rc, 1, out)
        self.assertIn("不認得的設定：search.min_scor", out)
        self.assertEqual(self.read("usr/local/etc/sse/filter.json"), filter_json)
        # 改成只轉發：搜尋結果回到原生
        self.edit_config(filter={"mode": "passthrough"})
        rc, out = self.sse("apply")
        self.assertEqual(rc, 0, out)
        self.assertIn("filter：模式 passthrough", out)
        self.finds("cat", NATIVE_CATS)
        self.edit_config()
        rc, out = self.sse("apply", "--check")
        self.assertEqual(rc, 0, out)
        self.assertIn("✓ DSM 登入", out)
        self.finds("cat", CATS)
        self.assertIn("── sse", self.read("var/log/sse/sse.log").decode())

    def test_09_guard_reapplies_after_dsm_regenerates_conf(self):
        self.set_conf(self.original)
        rc, out = self.sse("guard")
        self.assertEqual(rc, 0, out)
        self.assertIn("已重新套用", out)
        self.assertIn("filter.sock max_fails", self.conf())

    def test_10_guard_pauses_after_dsm_update(self):
        self.write("etc.defaults/VERSION", 'majorversion="7"\nproductversion="7.3.3"\nbuildnumber="86100"\n')
        self.set_conf(self.original)
        rc, out = self.sse("guard")
        self.assertEqual(rc, 0, out)
        self.assertIn("暫停自動套用", out)
        self.assertEqual(self.conf(), self.original)
        rc, out = self.sse("doctor")
        self.assertIn("sudo sse reapply", out)
        rc, out = self.sse("reapply")
        self.assertEqual(rc, 0, out)
        self.assertIn("filter.sock max_fails", self.conf())
        self.assertEqual(json.loads(self.read("usr/local/etc/sse/state.json"))["dsm_build"], "86100")

    def test_11_kill_switch(self):
        self.assertEqual(self.sse("disable")[0], 0)
        self.finds("cat", NATIVE_CATS)
        self.assertIn("緊急開關：開啟", self.sse("status")[1])
        self.assertEqual(self.sse("enable")[0], 0)
        self.finds("cat", CATS)

    def test_12_bypass(self):
        rc, out = self.sse("bypass", "on")
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.conf(), self.original)
        self.assertEqual(self.sse("guard")[0], 0)
        self.assertEqual(self.conf(), self.original)   # bypass 期間 guard 不會套回去
        rc, out = self.sse("bypass", "off")
        self.assertEqual(rc, 0, out)
        self.assertIn("filter.sock max_fails", self.conf())
        self.finds("cat", CATS)

    def test_13_capture(self):
        rc, out = self.sse("capture", "on", "--max", "3")
        self.assertEqual(rc, 0, out)
        self.finds("dog", self.find("alice", "dog"))   # 等 filter 重啟完
        sid = self.login("alice", "pw-alice")
        rc, out = self.sse("capture", "show")
        self.assertIn('"kind": "native"', out)
        self.assertNotIn(sid, out)                     # 不記 session
        self.assertEqual(self.sse("capture", "off")[0], 0)
        self.assertIn("沒有診斷紀錄", self.sse("capture", "show")[1])

    def test_14_engine_down_falls_back_to_native(self):
        # filter 會把同一個搜尋字的結果快取 60 秒，所以用還沒搜過的字
        subprocess.run([self.env["SSE_SYSTEMCTL"], "stop", "sse-engine.service"], env=self.env)
        self.assertEqual(self.find("alice", "mountain"), [])   # 原生找不到，engine 沒回應：DSM 照常回原生結果
        rc, out = self.sse("doctor")
        self.assertIn("搜尋服務沒有回應", out)
        subprocess.run([self.env["SSE_SYSTEMCTL"], "start", "sse-engine.service"], env=self.env)
        self.assertTrue(wait_for(lambda: sock_alive(self.engine_sock), timeout=20))
        self.assertEqual(len(self.find("alice", "beach")), 6)

    def test_15_uninstall_refuses_hand_edited_conf(self):
        managed = self.conf()
        self.write("etc/nginx/nginx.conf", managed.replace("fail_timeout=5s", "fail_timeout=60s"))
        try:
            rc, out = self.sse("uninstall", "--yes")
            self.assertEqual(rc, 1, out)
            self.assertIn("停在這裡不移除 filter", out)
            self.assertTrue(sock_alive(self.p("run/sse/filter.sock")))
            self.assertTrue(os.path.exists(self.p("etc/systemd/system/sse-filter.service")))
        finally:
            self.write("etc/nginx/nginx.conf", managed)

    def test_16_upgrade_in_place(self):
        state = json.loads(self.read("usr/local/etc/sse/state.json"))
        os.chmod(self.p("usr/local/sse"), 0o700)                      # 在 umask 077 下裝出來的樣子
        unit = self.p("etc/systemd/system/sse-indexer.service")
        with open(unit, "a") as f:
            f.write("# 舊版範本\n")
        rc, out = self.install("--yes", "--probe-url", self.probe)   # 帳號與資料目錄沿用安裝紀錄
        self.assertEqual(rc, 0, out)
        self.assertIn("升級中", out)
        self.assertIn("沿用 %s" % os.path.join(os.path.realpath(self.data), "config.yml"), out)
        self.assertIn("nginx 設定原本就是套用狀態", out)
        self.assertIn("目前索引 20 張", out)                   # 索引保留
        with open(unit) as f:
            self.assertNotIn("舊版範本", f.read())
        self.assertEqual(json.loads(self.read("usr/local/etc/sse/state.json"))["installed_at"], state["installed_at"])
        self.check_modes()
        self.finds("cat", CATS)

    # ── 從 GitHub release 安裝與更新（curl | sh） ──

    def test_17_switch_to_release(self):
        self.gh.add("v2026.10.25+build.1")
        rc, out = self.curl_sh("--yes", "--probe-url", self.probe)
        self.assertEqual(rc, 0, out)
        self.assertIn("下載最新版", out)
        self.assertIn("最新版：v2026.10.25+build.1", out)
        self.assertIn("目前索引 20 張", out)
        self.assertEqual(self.version(), "2026.10.25+build.1")
        state = json.loads(self.read("usr/local/etc/sse/state.json"))
        self.assertEqual((state["channel"], state["source"]), ("release", "v2026.10.25+build.1"))
        self.check_modes()
        self.assertIn("更新管道：release（release v2026.10.25+build.1", self.sse("status")[1])
        self.finds("cat", CATS)

    def test_18_update(self):
        rc, out = self.sse("update")
        self.assertEqual(rc, 0, out)
        self.assertIn("已是最新版本", out)
        self.gh.add("v2026.10.26+build.1", prerelease=True)
        self.assertIn("已是最新版本", self.sse("update", "--check")[1])    # 只有 pre-release 有新版
        downloads = self.gh.downloads
        rc, out = self.sse("update", "--check", "--channel", "pre-release")
        self.assertIn("有新版", out)
        self.assertEqual((self.gh.downloads, self.version()), (downloads, "2026.10.25+build.1"))   # 只檢查
        rc, out = self.sse("update", "--channel", "pre-release")
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.version(), "2026.10.26+build.1")
        self.assertEqual(json.loads(self.read("usr/local/etc/sse/state.json"))["channel"], "pre-release")
        self.gh.add("v2026.10.26+build.2", prerelease=True)
        rc, out = self.curl_sh("update")                                   # 沿用 pre-release
        self.assertEqual(rc, 0, out)
        self.assertEqual(self.version(), "2026.10.26+build.2")
        rc, out = self.sse("update", "--channel", "release")               # release 比較舊：不降版
        self.assertEqual(rc, 0, out)
        self.assertIn("不降版", out)
        self.assertEqual(self.version(), "2026.10.26+build.2")
        self.finds("cat", CATS)

    def test_19_failed_update_rolls_back(self):
        self.gh.add("v2026.10.26+build.3", prerelease=True, edit=break_serve)   # 新版的搜尋服務起不來
        rc, out = self.sse("update")
        self.assertEqual(rc, 1, out)
        self.assertIn("搜尋服務沒有在 20 秒內回應", out)
        self.assertIn("換回原本的版本", out)
        self.assertEqual(self.version(), "2026.10.26+build.2")
        self.assertEqual(sorted(os.listdir(self.p("usr/local/sse"))), ["sse"])   # sse.prev 換回去了
        self.no_work_dirs()
        self.assertTrue(wait_for(lambda: sock_alive(self.engine_sock), timeout=20))
        # filter 也用舊版重啟過：等 nginx 的 fail_timeout 過了，搜尋結果又有 AI 找到的照片
        self.assertTrue(wait_for(lambda: len(self.find("alice", "mountain")) == 3, timeout=15))
        self.assertEqual(json.loads(self.read("usr/local/etc/sse/state.json"))["source"], "v2026.10.26+build.2")
        self.assertEqual(self.sse("doctor")[0], 0)
        self.assertIn("沒有發現問題", self.sse("doctor")[1])

    def test_20_bad_checksum_refused(self):
        rel = self.gh.releases[-1]
        with open(self.gh.asset_path(rel["tag_name"]), "ab") as f:
            f.write(b"tampered")
        self.gh.releases[-1] = dict(rel, tag_name="v2026.10.26+build.4")   # 網址還是指向剛剛改過的檔案
        rc, out = self.sse("update", "--force")
        self.assertEqual(rc, 1, out)
        self.assertIn("sha256 不符", out)
        self.assertEqual(self.version(), "2026.10.26+build.2")
        self.no_work_dirs()

    def test_21_uninstall_keeps_data(self):
        rc, out = self.sse("uninstall", "--yes")
        self.assertEqual(rc, 0, out)
        self.assertIn("解除安裝完成。", out)
        self.assertIn("資料目錄保留在", out)
        self.nothing_installed()
        self.assertTrue(os.path.exists(os.path.join(self.data, "config.yml")))
        self.assertTrue(os.path.exists(os.path.join(self.data, "data/index.db")))
        self.assertEqual(self.find("alice", "cat"), NATIVE_CATS)

    def test_22_reinstall_then_purge(self):
        # 重新安裝：沿用資料目錄裡的 config.yml 與索引
        rc, out = self.curl_sh("--channel", "release", *self.install_args())
        self.assertEqual(rc, 0, out)
        self.assertIn("沿用", out)
        self.assertIn("目前索引 20 張", out)
        self.assertEqual(self.version(), "2026.10.25+build.1")
        self.finds("cat", CATS)
        rc, out = self.curl_sh("uninstall", "--purge", "--yes")
        self.assertEqual(rc, 0, out)
        self.assertIn("資料目錄已刪除", out)
        self.nothing_installed()
        self.assertFalse(os.path.exists(self.data))


    # ── 從 Python 版升級 ──

    def python_era_install(self):
        """做出 Python 版安裝後的樣子：/usr/local/sse 裡有 venv、uv、sse-filter.py，服務用 Python 執行。"""
        r, prefix = self.root, self.p("usr/local/sse")
        for d in ("venv/bin", "python/cpython-3.12", "bin", "cache"):
            os.makedirs(os.path.join(prefix, d), exist_ok=True)
        py = os.path.join(prefix, "venv/bin/python")
        self.write(py, "#!/bin/sh\nexec sleep 3600\n", 0o755)        # 舊的服務：只要跑得起來
        for name in ("sse-filter.py", "install.sh", "bin/uv"):
            self.write(os.path.join(prefix, name), "# Python 版\n", 0o755)
        os.makedirs(self.p("usr/local/bin"), exist_ok=True)
        self.write("usr/local/bin/sse", '#!/bin/sh\nexec "%s" -I -m sse "$@"\n' % py, 0o755)
        group = grp.getgrgid(os.getgid()).gr_name
        cfg_path = os.path.join(self.data, "config.yml")
        units = {
            "sse-filter.service": "[Service]\nType=simple\nRuntimeDirectory=sse\n"
                                  "ExecStart=%s -I %s/sse-filter.py --config %s/usr/local/etc/sse/filter.json\n"
                                  % (py, prefix, r),
            "sse-engine.service": "[Service]\nType=simple\nRuntimeDirectory=sse-engine\n"
                                  "ExecStart=%s -I -m sse --config %s serve\n" % (py, cfg_path),
            "sse-indexer.service": "[Service]\nType=simple\nExecStart=%s -I -m sse --config %s index\n" % (py, cfg_path),
            "sse-nginx-guard.service": "[Service]\nType=oneshot\nExecStart=%s -I -m sse guard\n" % py,
            "sse-nginx-guard.path": "[Path]\nPathChanged=%s\n" % self.conf_path,
            "sse-nginx-guard.timer": "[Timer]\nOnUnitActiveSec=10min\n",
        }
        os.makedirs(self.p("etc/systemd/system"), exist_ok=True)
        for name, text in units.items():
            self.write("etc/systemd/system/" + name, "# Python 版\n" + text)
        os.makedirs(os.path.join(self.data, "data"), exist_ok=True)
        os.makedirs(os.path.join(self.data, "logs"), exist_ok=True)
        os.chmod(self.data, 0o700)
        self.write(cfg_path, self.config_text(), 0o600)
        os.makedirs(self.p("usr/local/etc/sse"), exist_ok=True)
        os.makedirs(self.p("var/log/sse"), mode=0o750, exist_ok=True)
        filter_cfg = {
            "listen": self.p("run/sse/filter.sock"), "socket_mode": "0660", "upstream": self.p("run/synoscgi.sock"),
            "status_file": self.p("run/sse/status.json"), "kill_switch": self.p("usr/local/etc/sse/DISABLE"),
            "photos_info": self.p("var/packages/SynologyPhotos/INFO"), "mode": "engine",
            "inject_photos_version": "1.9.0-10120", "engine_socket": self.engine_sock, "max_inject": 500,
            "log_file": self.p("var/log/sse/filter.log"), "log_keep_days": 3,
            "capture_file": self.p("run/sse/capture.jsonl"), "capture_max": 7}
        self.write("usr/local/etc/sse/filter.json", json.dumps(filter_cfg, indent=2, ensure_ascii=False) + "\n")
        self.state = {
            "version": "2026.10.20+build.1", "installed_at": 1700000000, "updated_at": 1700000000, "dsm_build": "86009",
            "channel": "release", "source": "v2026.10.20+build.1", "photos_version": "1.9.0-10120",
            "nginx_user": USER, "nginx_group": group, "probe_url": self.probe, "auto_apply": True, "user": USER,
            "data_dir": os.path.realpath(self.data)}
        self.write("usr/local/etc/sse/state.json", json.dumps(self.state, indent=2, ensure_ascii=False))
        # nginx.conf：Python 版套用的標記（格式跟 Go 版相同）
        stmt = "server unix:%s/run/synoscgi.sock;" % r
        managed = self.original.replace("        %s\n" % stmt, (
            "        # >>> synology-search-enhancement original=%s >>>\n"
            "        server unix:%s/run/sse/filter.sock max_fails=1 fail_timeout=5s;\n"
            "        server unix:%s/run/synoscgi.sock backup;\n"
            "        # <<< synology-search-enhancement <<<\n") % (json.dumps(stmt), r, r))
        self.assertNotEqual(managed, self.original)
        self.write("etc/nginx/nginx.conf.sse-orig", self.original)
        self.set_conf(managed)
        systemctl = self.env["SSE_SYSTEMCTL"]
        subprocess.run([systemctl, "enable"] + list(UNITS), env=self.env, check=True)
        subprocess.run([systemctl, "start", "sse-filter.service", "sse-engine.service", "sse-indexer.service",
                        "sse-nginx-guard.path", "sse-nginx-guard.timer"], env=self.env, check=True)
        self.assertTrue(self.dsm_ok())   # 舊的 filter 不在：nginx 直連 synoscgi（backup）

    def python_era_files(self):
        """Python 版安裝的檔案（比對是否原樣還原用）。"""
        files = {}
        for top in ("usr/local/sse", "usr/local/bin", "usr/local/etc/sse", "etc/systemd/system"):
            for d, _, names in os.walk(self.p(top)):
                for n in names:
                    path = os.path.join(d, n)
                    files[os.path.relpath(path, self.root)] = (os.path.islink(path), mode(path), self.read(path))
        files["nginx.conf"] = (False, mode(self.conf_path), self.conf().encode())
        return files

    def old_update(self, tag, channel):
        """Python 版的 sudo sse update：舊的 install.sh 從 release 下載 synology-photos-search-enhancement.tar.gz、
        驗證後解開到 /tmp，執行裡面的（新版）install.sh update，並用環境變數告訴它版本與管道。"""
        work = tempfile.mkdtemp(prefix="old-update.", dir=self.root)
        with tarfile.open(self.gh.asset_path(tag, fake_github.LEGACY)) as t:
            t.extractall(work, **({"filter": "data"} if hasattr(tarfile, "data_filter") else {}))
        script = os.path.join(work, "synology-photos-search-enhancement", "install.sh")
        self.assertTrue(os.path.exists(os.path.join(os.path.dirname(script), "pyproject.toml")))   # 舊版檢查這個
        r = subprocess.run(["sh", script, "update"], preexec_fn=dsm_login_umask, stdin=subprocess.DEVNULL,
                           env=dict(self.env, SSE_WORK=work, SSE_SOURCE=tag, SSE_CHANNEL=channel),
                           stdout=subprocess.PIPE, stderr=subprocess.STDOUT, text=True, timeout=600)
        self.assertFalse(os.path.exists(work))
        return r.returncode, r.stdout

    def test_23_failed_upgrade_from_python_version_restores_it(self):
        self.python_era_install()
        before = self.python_era_files()
        self.gh.add("v2026.10.27+build.1", prerelease=True, edit=break_serve)
        rc, out = self.old_update("v2026.10.27+build.1", "pre-release")
        self.assertEqual(rc, 1, out)
        self.assertIn("升級中", out)
        self.assertIn("搜尋服務沒有在 20 秒內回應", out)
        self.assertIn("換回原本的版本", out)
        # Python 版的檔案、服務、安裝紀錄、filter 設定、nginx.conf 都跟原本一模一樣
        after = self.python_era_files()
        self.assertEqual(sorted(after), sorted(before))
        for path in before:
            self.assertEqual(after[path], before[path], path)
        self.no_work_dirs()
        self.assertTrue(self.dsm_ok())

    def test_24_upgrade_from_python_version(self):
        if not os.path.exists(self.p("usr/local/sse/venv")):
            self.python_era_install()
        rc, out = self.old_update("v2026.10.25+build.1", "release")
        self.assertEqual(rc, 0, out)
        self.assertIn("下載 v2026.10.25+build.1", out)
        self.assertIn("升級中", out)
        self.assertIn("沿用 %s" % os.path.join(os.path.realpath(self.data), "config.yml"), out)
        self.assertIn("nginx 設定原本就是套用狀態", out)
        self.assertEqual(self.version(), "2026.10.25+build.1")
        # Python 版的程式都清掉了，服務改成執行 sse
        self.check_modes()
        for unit in UNITS:
            text = self.read("etc/systemd/system/" + unit).decode()
            self.assertNotIn("Python 版", text)
            self.assertNotIn("python", text)
            if unit.endswith(".service"):
                self.assertIn("ExecStart=%s " % self.p("usr/local/sse/sse"), text)
        # 安裝紀錄、診斷錄製的設定、資料目錄都沿用
        state = json.loads(self.read("usr/local/etc/sse/state.json"))
        self.assertEqual((state["installed_at"], state["user"], state["data_dir"], state["channel"], state["source"]),
                         (1700000000, USER, os.path.realpath(self.data), "release", "v2026.10.25+build.1"))
        filter_cfg = json.loads(self.read("usr/local/etc/sse/filter.json"))
        self.assertEqual((filter_cfg["capture_max"], filter_cfg["mode"]), (7, "engine"))
        self.assertTrue(wait_for(lambda: self.find("alice", "cat") == CATS, timeout=90))
        rc, out = self.curl_sh("uninstall", "--purge", "--yes")
        self.assertEqual(rc, 0, out)
        self.nothing_installed()


if __name__ == "__main__":
    unittest.main()
