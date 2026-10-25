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
        os.makedirs(os.path.join(self.d, "secrets"))
        for name, v in (("dsm_password", "pw"), ("llm_api_key", "sk-1")):
            with open(os.path.join(self.d, "secrets", name), "w") as f:
                f.write(v)
        with open(os.path.join(self.d, "config.toml"), "w") as f:
            f.write('[synology]\naccount = "bob"\npassword_file = "secrets/dsm_password"\nthumb_size = "m"\n'
                    '[llm]\nbase_url = "https://llm/v1"\napi_key_file = "secrets/llm_api_key"\nocr_model = "g4"\n'
                    '[index]\ndb = "x"\nocr = "all"\n[search]\nmin_score = 0.7\n')
        wizard.write_config(self.path, wizard.from_toml(os.path.join(self.d, "config.toml")))
        c = cfgm.load(self.path)
        self.assertEqual((c["synology"]["account"], c["synology"]["password"]), ("bob", "pw"))
        self.assertEqual((c["llm"]["embed_api_key"], c["llm"]["ocr_api_key"], c["llm"]["ocr_model"]), ("sk-1", "sk-1", "g4"))
        self.assertEqual(c["llm"]["ocr_extra_body"], {"reasoning_effort": "none"})   # 舊版的 OCR 行為不變
        self.assertEqual(c["search"]["min_score"], 0.7)


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
