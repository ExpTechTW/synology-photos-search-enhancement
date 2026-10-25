"""測試用的假 GitHub：/repos/<repo>/releases（release 清單）與 release 的下載檔。

  gh = FakeGitHub(root)
  gh.add("v2026.10.26+build.1", prerelease=False)   # 用 scripts/package.sh 打包目前的原始碼
  gh.url → install.sh 的 SSE_GITHUB_API
"""

import json
import os
import shutil
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
ASSET = "synology-photos-search-enhancement.tar.gz"


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
                path = self.path.split("?", 1)[0]
                if path.endswith("/releases"):
                    body = json.dumps(list(reversed(gh.releases))).encode()   # 跟 GitHub 一樣新建立的在前面
                elif path.startswith("/download/"):
                    f = os.path.join(gh.root, path[len("/download/"):])
                    if not os.path.isfile(f):
                        return self.send_error(404)
                    gh.downloads += path.endswith(".tar.gz")
                    with open(f, "rb") as fh:
                        body = fh.read()
                else:
                    return self.send_error(404)
                self.send_response(200)
                self.send_header("Content-Length", str(len(body)))
                self.end_headers()
                self.wfile.write(body)

        self.srv = ThreadingHTTPServer(("127.0.0.1", 0), H)
        threading.Thread(target=self.srv.serve_forever, daemon=True).start()
        self.base = "http://127.0.0.1:%d" % self.srv.server_port
        self.url = self.base + "/repos/ExpTechTW/synology-photos-search-enhancement"

    def add(self, tag, prerelease=False, edit=None):
        """打包目前的原始碼成這個版本。edit(dir)：打包前修改原始碼（例如做一個壞掉的版本）。"""
        d = os.path.join(self.root, tag.replace("+", "_"))
        src = os.path.join(self.root, "src-" + tag.replace("+", "_"))
        shutil.copytree(REPO, src, ignore=shutil.ignore_patterns(".git", "__pycache__", "*.egg-info"))
        if edit:
            edit(src)
        subprocess.run(["sh", os.path.join(src, "scripts/package.sh"), tag, d], check=True,
                       stdout=subprocess.DEVNULL)
        rel = os.path.basename(d)
        self.releases.append({
            "tag_name": tag, "draft": False, "prerelease": prerelease,
            "assets": [{"name": n, "browser_download_url": "%s/download/%s/%s" % (self.base, rel, n)}
                       for n in (ASSET, ASSET + ".sha256")]})

    def stop(self):
        self.srv.shutdown()
