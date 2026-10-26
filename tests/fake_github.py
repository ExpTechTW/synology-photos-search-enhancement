"""測試用的假 GitHub：release 清單（API）與 release 的下載檔（跟 github.com 相同的網址格式）。

  gh = FakeGitHub(root)
  gh.add("v2026.10.26+build.1", prerelease=False)   # 用 scripts/package.sh 編譯、打包目前的原始碼
  gh.url → SSE_GITHUB_API（release 清單）
  gh.web → SSE_GITHUB_URL（install.sh 下載 releases/latest/download/… 與 releases/download/<tag>/…）

只編譯跑測試的這台機器的平台（例如 sse-darwin-arm64.tar.gz），install.sh 與 sse update 下載的也是這個檔名。
"""

import json
import os
import platform
import shutil
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import unquote

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
OWNER_REPO = "ExpTechTW/synology-photos-search-enhancement"
LEGACY = "synology-photos-search-enhancement.tar.gz"   # Python 版的 sudo sse update 下載的檔名


def host_target():
    """跑測試的這台機器：(GOOS/GOARCH 的寫法, 安裝檔名)。"""
    os_name = platform.system().lower()
    arch = {"x86_64": "amd64", "amd64": "amd64", "aarch64": "arm64", "arm64": "arm64"}.get(platform.machine().lower(),
                                                                                        "armv7")
    return "%s/%s" % (os_name, arch), "sse-%s-%s.tar.gz" % (os_name, arch)


TARGET, ASSET = host_target()


def tag_dir(tag):
    return tag.replace("+", "_")


class FakeGitHub:
    def __init__(self, root):
        self.root = root
        self.releases = []
        self.downloads = 0
        os.makedirs(root, exist_ok=True)
        gh = self

        class H(BaseHTTPRequestHandler):
            def log_message(self, *a):
                pass

            def do_GET(self):
                path = unquote(self.path.split("?", 1)[0])
                api = "/repos/%s/releases" % OWNER_REPO
                latest = "/%s/releases/latest/download/" % OWNER_REPO
                by_tag = "/%s/releases/download/" % OWNER_REPO
                if path == api:
                    body = json.dumps(list(reversed(gh.releases))).encode()   # 跟 GitHub 一樣新建立的在前面
                    return self.reply(body)
                if path.startswith(latest):
                    stable = [r for r in gh.releases if not r["prerelease"] and not r["draft"]]
                    if not stable:
                        return self.send_error(404)
                    return self.file(stable[-1]["tag_name"], path[len(latest):])
                if path.startswith(by_tag):
                    tag, _, name = path[len(by_tag):].partition("/")
                    return self.file(tag, name)
                self.send_error(404)

            def file(self, tag, name):
                f = os.path.join(gh.root, tag_dir(tag), name)
                if "/" in name or not os.path.isfile(f):
                    return self.send_error(404)
                gh.downloads += name.endswith(".tar.gz")
                with open(f, "rb") as fh:
                    self.reply(fh.read())

            def reply(self, body):
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.base = "http://127.0.0.1:%d" % self.srv.server_port
        self.url = self.base + "/repos/" + OWNER_REPO
        self.web = self.base + "/" + OWNER_REPO

    def add(self, tag, prerelease=False, edit=None):
        """打包目前的原始碼成這個版本。edit(dir)：打包前修改原始碼（例如做一個壞掉的版本）。"""
        d = os.path.join(self.root, tag_dir(tag))
        src = os.path.join(self.root, "src-" + tag_dir(tag))
        shutil.copytree(REPO, src, ignore=shutil.ignore_patterns(".git", "__pycache__", "*.egg-info", "build", "dist"))
        if edit:
            edit(src)
        subprocess.run(["sh", os.path.join(src, "scripts/package.sh"), tag, d, TARGET], check=True,
                       stdout=subprocess.DEVNULL)
        shutil.rmtree(src, ignore_errors=True)
        self.releases.append({
            "tag_name": tag, "draft": False, "prerelease": prerelease,
            "assets": [{"name": n, "browser_download_url": "%s/releases/download/%s/%s" % (self.web, tag, n)}
                       for n in (ASSET, ASSET + ".sha256", LEGACY, LEGACY + ".sha256")]})

    def asset_path(self, tag, name=ASSET):
        return os.path.join(self.root, tag_dir(tag), name)

    def stop(self):
        self.srv.shutdown()
