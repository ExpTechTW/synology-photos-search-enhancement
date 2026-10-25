#!/usr/bin/env python3
# 安裝工具的單元測試：nginx.conf 的判斷、套用、還原，config.yml 的建立與檢查。
#   python -m unittest tests.test_admin -v   （要在裝好套件的環境跑：sh tests/run.sh）
import argparse
import os
import shutil
import tempfile
import unittest
from unittest import mock

os.environ.pop("SSE_ROOT", None)
from sse import admin as ctl  # noqa: E402
from sse import config as cfgm  # noqa: E402
from sse import wizard  # noqa: E402

DSM_CONF = """user http http;
worker_processes auto;
http {
    include mime.types;

    upstream synoscgi {
        server unix:/run/synoscgi.sock;
    }

    server {
        listen 5000;
        location ~ \\.cgi {
            include scgi_params;
            scgi_pass synoscgi;
        }
    }
}
"""


class NginxConf(unittest.TestCase):
    def test_pristine_patch_unpatch_roundtrip(self):
        self.assertEqual(ctl.nginx_classify(DSM_CONF)[0], "pristine")
        patched = ctl.nginx_patch(DSM_CONF)
        self.assertEqual(ctl.nginx_classify(patched)[0], "managed")
        self.assertIn("        server unix:/run/sse/filter.sock max_fails=1 fail_timeout=5s;\n", patched)
        self.assertIn("        server unix:/run/synoscgi.sock backup;\n", patched)
        self.assertEqual(ctl.nginx_unpatch(patched), DSM_CONF)  # 還原後逐字相同
        self.assertEqual(ctl.nginx_unpatch(DSM_CONF), DSM_CONF)
        with self.assertRaises(ctl.Abort):
            ctl.nginx_patch(patched)  # 已套用的不會再套一次

    def test_tabs_and_comments_preserved(self):
        conf = "upstream synoscgi {\n\t# 註解\n\tserver unix:/run/synoscgi.sock;  \n}\n"
        self.assertEqual(ctl.nginx_classify(conf)[0], "pristine")
        patched = ctl.nginx_patch(conf)
        self.assertIn("\tserver unix:/run/synoscgi.sock backup;", patched)
        self.assertEqual(ctl.nginx_unpatch(patched), conf)

    def assertUnknown(self, conf, needle):
        kind, why = ctl.nginx_classify(conf)
        self.assertEqual(kind, "unknown", why)
        self.assertIn(needle, why)
        with self.assertRaises(ctl.Abort):
            ctl.nginx_patch(conf)

    def test_refuses_unexpected_layouts(self):
        self.assertUnknown("upstream synoscgi { server unix:/run/synoscgi.sock; }\n", "獨立一行")
        self.assertUnknown(DSM_CONF.replace("/run/synoscgi.sock", "/run/synosse-filter.sock"), "其他 filter")
        self.assertUnknown(DSM_CONF + "upstream synoscgi {\n    server unix:/x;\n}\n", "2 個")
        self.assertUnknown(DSM_CONF.replace("synoscgi.sock;", "synoscgi.sock max_fails=0;"), "不是預期")
        self.assertUnknown(DSM_CONF.replace("synoscgi.sock;\n", "synoscgi.sock;\n        keepalive 8;\n"), "不是預期")
        self.assertUnknown(DSM_CONF.replace("upstream synoscgi", "upstream other"), "0 個")

    def test_refuses_edited_managed_block(self):
        patched = ctl.nginx_patch(DSM_CONF)
        self.assertUnknown(patched.replace("fail_timeout=5s", "fail_timeout=60s"), "手動修改")
        self.assertUnknown(patched.replace(ctl.MARK_END, ""), "不完整")
        self.assertUnknown(patched.replace("    }\n\n    server", "        keepalive 8;\n    }\n\n    server", 1),
                           "其他設定")
        with self.assertRaises(ctl.Abort):
            ctl.nginx_unpatch(patched.replace("fail_timeout=5s", "fail_timeout=60s"))



class ConfigFile(unittest.TestCase):
    def setUp(self):
        self.d = tempfile.mkdtemp()
        self.path = os.path.join(self.d, "config.yml")

    def tearDown(self):
        shutil.rmtree(self.d)

    def write(self, values):
        wizard.write_config(self.path, wizard.render(values))

    def test_template_needs_filling(self):
        self.write({})
        with self.assertRaises(cfgm.ConfigError) as cm:
            cfgm.load(self.path)
        self.assertIn("synology.account", str(cm.exception))

    def test_minimal_config(self):
        self.write({("synology", "account"): "alice", ("synology", "password"): 'p@ss: "x" #1',
                    ("embedding", "base_url"): "http://gpu:8080/v1", ("ocr", "model"): "gemma3"})
        c = cfgm.load(self.path)
        self.assertEqual(c["synology"]["password"], 'p@ss: "x" #1')                    # 特殊字元
        self.assertEqual(c["llm"]["ocr_base_url"], "http://gpu:8080/v1")               # OCR 沿用 embedding 的服務
        self.assertEqual(c["index"]["db"], os.path.join(self.d, "data", "index.db"))   # 資料放在 config.yml 旁邊
        self.assertEqual(c["filter"]["mode"], "engine")
        self.assertEqual(oct(os.stat(self.path).st_mode & 0o777), "0o600")

    def test_validation(self):
        base = {("synology", "account"): "alice", ("synology", "password"): "pw",
                ("embedding", "base_url"): "http://gpu/v1", ("ocr", "model"): "m"}
        self.write({**base, ("ocr", "model"): ""})   # 開了 OCR 卻沒填模型
        with self.assertRaises(cfgm.ConfigError) as cm:
            cfgm.load(self.path)
        self.assertIn("ocr.model", str(cm.exception))
        self.write({**base, ("ocr", "enabled"): False, ("ocr", "model"): ""})
        self.assertEqual(cfgm.load(self.path)["index"]["ocr"], "off")
        with open(self.path, "a") as f:
            f.write("\nsearch:\n  typo_key: 1\n")
        with self.assertRaises(cfgm.ConfigError) as cm:
            cfgm.load(self.path)
        self.assertIn("search.typo_key", str(cm.exception))
        self.write({**base, ("filter", "mode"): "inject"})
        with self.assertRaises(cfgm.ConfigError):
            cfgm.load(self.path)
        self.write(base)
        os.chmod(self.path, 0o644)
        with self.assertRaises(cfgm.ConfigError) as cm:
            cfgm.load(self.path)
        self.assertIn("chmod 600", str(cm.exception))

    def test_migrate_private_version(self):
        # 舊版的路徑寫成 ~/sse/...：要用照片擁有者的家目錄，不是執行安裝的 root 的
        home = os.path.join(self.d, "home")
        os.makedirs(os.path.join(home, "sse", "secrets"))
        with open(os.path.join(home, "sse", "secrets", "dsm_password"), "w") as f:
            f.write("pw\n")
        os.makedirs(os.path.join(self.d, "secrets"))
        with open(os.path.join(self.d, "secrets", "llm_api_key"), "w") as f:   # 相對於 config.toml 的路徑
            f.write("sk-1")
        with open(os.path.join(self.d, "config.toml"), "w") as f:
            # 舊版會用到的欄位都放進來：每一個都要在範本裡找得到位置
            f.write('[synology]\nurl = "https://127.0.0.1:5001"\naccount = "bob"\npassword_file = "~/sse/secrets/dsm_password"\n'
                    'verify_tls = false\nthumb_size = "m"\nocr_thumb_size = "sm"\n'
                    '[llm]\nbase_url = "https://llm/v1"\napi_key_file = "secrets/llm_api_key"\nembed_model = "e2"\n'
                    'ocr_model = "g4"\n[index]\ndb = "x"\nocr = "all"\n'
                    '[search]\nsocket = "/run/sse-engine/engine.sock"\ndims = 512\nmin_score = 0.7\nmax_drop = 0.08\n'
                    'text_min_score = 0.76\n')
        wizard.write_config(self.path, wizard.from_toml(os.path.join(self.d, "config.toml"), home))
        c = cfgm.load(self.path)
        self.assertEqual((c["synology"]["account"], c["synology"]["password"]), ("bob", "pw"))
        self.assertEqual((c["llm"]["embed_api_key"], c["llm"]["ocr_api_key"], c["llm"]["ocr_model"]), ("sk-1", "sk-1", "g4"))
        self.assertEqual(c["llm"]["ocr_extra_body"], {"reasoning_effort": "none"})   # 舊版的 OCR 行為不變
        self.assertEqual((c["search"]["min_score"], c["search"]["text_min_score"]), (0.7, 0.76))
        self.assertEqual((c["synology"]["thumb_size"], c["synology"]["ocr_thumb_size"], c["llm"]["embed_model"]),
                         ("m", "sm", "e2"))

    def test_interactive_wizard(self):
        # 設定精靈填的每個欄位都要在範本裡找得到位置，填完要是合法的設定
        answers = iter(["alice", "http://gpu:8080/v1", "", "y", "n", "http://ocr:11434/v1", "gemma3:12b"])
        secrets = iter(["pw 123", "sk-e", "sk-o"])
        with mock.patch("builtins.input", lambda prompt="": next(answers)), \
                mock.patch("getpass.getpass", lambda prompt="": next(secrets)), \
                mock.patch("builtins.print"):
            text = wizard.interactive("alice")
        wizard.write_config(self.path, text)
        c = cfgm.load(self.path)
        self.assertEqual((c["synology"]["account"], c["synology"]["password"], c["llm"]["embed_model"]),
                         ("alice", "pw 123", "embeddinggemma-2"))
        self.assertEqual((c["llm"]["embed_api_key"], c["llm"]["ocr_base_url"], c["llm"]["ocr_api_key"],
                          c["llm"]["ocr_model"]), ("sk-e", "http://ocr:11434/v1", "sk-o", "gemma3:12b"))

    def test_migrate_failure_is_explained(self):
        import pwd
        with open(os.path.join(self.d, "config.toml"), "w") as f:
            f.write('[synology]\naccount = "bob"\npassword_file = "~/nope/dsm_password"\n[llm]\nbase_url = "x"\n')
        args = argparse.Namespace(import_config=None)
        for dry in (True, False):   # dry-run 也要真的轉換一次，才能事先發現問題
            with self.assertRaises(ctl.Abort) as cm:
                ctl.ensure_config(args, "bob", pwd.getpwuid(os.getuid()), self.d, dry)
            self.assertIn("config.toml 轉換失敗", str(cm.exception))
            self.assertIn("nope/dsm_password", str(cm.exception))
        self.assertFalse(os.path.exists(os.path.join(self.d, "config.yml")))


class ReadInput(unittest.TestCase):
    """DSM 的終端機沒開 iutf8：用倒退鍵刪掉注音字只會刪一個位元組，送來殘缺的 UTF-8。"""

    def answers(self, *lines):
        it = iter(lines)

        def fake(prompt=""):
            v = next(it)
            if isinstance(v, Exception):
                raise v
            return v
        return mock.patch("builtins.input", fake), mock.patch("builtins.print")

    def test_broken_utf8_asks_again(self):
        broken = b"\xe3\x84y".decode("utf-8", "surrogateescape")   # 「ㄗ」被刪掉一個位元組後再打 y
        decode_error = UnicodeDecodeError("utf-8", b"\xe3\x84y", 0, 2, "invalid continuation byte")
        for bad in (broken, "\ufffdy", decode_error):
            a, p = self.answers(bad, "y")
            with a, p as printed:
                self.assertTrue(wizard.ask_yes("確定？", default=False))
            self.assertIn("無法辨識", printed.call_args_list[0][0][0])

    def test_answers(self):
        for lines, default, want in ((("",), False, False), (("",), True, True), (("ｙ",), False, True),
                                     ((" YES ",), False, True), (("n",), True, False), (("好", "y"), False, True),
                                     ((EOFError(),), True, True)):
            a, p = self.answers(*lines)
            with a, p:
                self.assertEqual(wizard.ask_yes("確定？", default=default), want, lines)
        a, p = self.answers("ｈｔｔｐ://nas:8080/v1")
        with a, p:
            self.assertEqual(wizard.ask("網址"), "http://nas:8080/v1")   # 全形換成半形
        with mock.patch("getpass.getpass", lambda prompt="": " ｐｗ "):
            self.assertEqual(wizard.read_line("密碼", secret=True), "ｐｗ")    # 密碼不改


class UninstallPrompt(unittest.TestCase):
    """解除安裝時詢問要不要刪資料目錄，預設保留。"""

    def setUp(self):
        self.d = tempfile.mkdtemp()

    def tearDown(self):
        shutil.rmtree(self.d, ignore_errors=True)

    def ask(self, answer, purge=False, yes=False, tty=True, data_dir=None):
        args = argparse.Namespace(purge=purge, yes=yes)
        asked = []

        def fake_input(prompt):
            asked.append(prompt)
            return answer
        with mock.patch("builtins.input", fake_input), mock.patch("sys.stdin") as stdin:
            stdin.isatty.return_value = tty
            return ctl.want_purge(args, self.d if data_dir is None else data_dir), asked

    def test_default_keeps(self):
        self.assertEqual(self.ask(""), (False, [mock.ANY]))
        self.assertEqual(self.ask("n")[0], False)
        self.assertEqual(self.ask("y")[0], True)
        self.assertEqual(self.ask("YES")[0], True)

    def test_no_question_when_not_interactive(self):
        self.assertEqual(self.ask("y", yes=True), (False, []))      # --yes：保留
        self.assertEqual(self.ask("y", tty=False), (False, []))     # 不是終端機：保留
        self.assertEqual(self.ask("", purge=True), (True, []))       # --purge：直接刪
        self.assertEqual(self.ask("y", data_dir=os.path.join(self.d, "nope")), (False, []))


if __name__ == "__main__":
    unittest.main()
