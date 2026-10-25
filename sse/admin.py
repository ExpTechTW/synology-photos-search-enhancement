"""安裝、解除安裝與維運（以 root 執行）。

對 DSM 的變更只有這些，解除安裝會全部移除：
  /usr/local/sse/            程式、Python、filter（root 擁有）
  /usr/local/bin/sse         指令
  /usr/local/etc/sse/        安裝紀錄、filter 設定（由 config.yml 產生）、緊急開關
  /var/log/sse/              filter 與 sse 的 log
  /etc/systemd/system/sse-*  四個服務
  /etc/nginx/nginx.conf      upstream synoscgi 改成「filter 為主、synoscgi 為 backup」，前後有標記
  /etc/nginx/nginx.conf.sse-orig   套用前的原廠設定備份

使用者的資料目錄（預設 ~/sse：config.yml、索引資料庫、log）解除安裝時會詢問要不要刪，預設保留。
"""

import contextlib
import fcntl
import grp
import importlib.util
import json
import logging
import os
import pwd
import re
import shutil
import socket
import ssl
import string
import subprocess
import sys
import time
import urllib.request

from . import __version__
from . import config as cfgm
from . import wizard

# 以 root 執行時不要在別處留下 root 擁有的 __pycache__
sys.dont_write_bytecode = True

DATA = os.path.join(os.path.dirname(os.path.abspath(__file__)), "data")

# 測試時用 SSE_ROOT 把所有系統路徑移到沙盒；正式環境為空字串
ROOT = os.environ.get("SSE_ROOT", "")
SANDBOX = bool(ROOT)


def P(path):
    return ROOT + path


PREFIX = P("/usr/local/sse")
BIN_LINK = P("/usr/local/bin/sse")
ETC = P("/usr/local/etc/sse")
FILTER_CONFIG = ETC + "/filter.json"
STATE = ETC + "/state.json"
KILL_SWITCH = ETC + "/DISABLE"
UNIT_DIR = P("/etc/systemd/system")
RUN_DIR = P("/run/sse")
FILTER_SOCK = RUN_DIR + "/filter.sock"
STATUS_FILE = RUN_DIR + "/status.json"
CAPTURE_FILE = RUN_DIR + "/capture.jsonl"
LOG_DIR = P("/var/log/sse")
SYNOSCGI_SOCK = P("/run/synoscgi.sock")
NGINX_CONF = P("/etc/nginx/nginx.conf")
NGINX_BACKUP = NGINX_CONF + ".sse-orig"
NGINX_PID = P("/run/nginx.pid")
DSM_VERSION_FILE = P("/etc.defaults/VERSION")
PHOTOS_INFO = P("/var/packages/SynologyPhotos/INFO")
LOCK_FILE = P("/run/sse-ctl.lock")

# filter 用 DSM 內建的 Python（只用標準庫），索引與搜尋用 /usr/local/sse/venv 的 Python
FILTER_PYTHON = os.environ.get("SSE_PYTHON", "/usr/bin/python3")
ENGINE_PYTHON = os.environ.get("SSE_ENGINE_PYTHON", sys.executable)
NGINX_BIN = os.environ.get("SSE_NGINX", "/usr/bin/nginx")
SYSTEMCTL = os.environ.get("SSE_SYSTEMCTL", "systemctl")

INSTALL_CMD = ("curl -fsSL https://raw.githubusercontent.com/ExpTechTW/synology-photos-search-enhancement/main/install.sh"
               " | sudo sh")
PROBE_PATH = "/webapi/query.cgi?api=SYNO.API.Info&version=1&method=query&query=SYNO.API.Info"
PROBE_CANDIDATES = ("https://127.0.0.1:5001" + PROBE_PATH, "http://127.0.0.1:5000" + PROBE_PATH)

FILTER_UNIT = "sse-filter.service"
GUARD_SERVICE = "sse-nginx-guard.service"
GUARD_TRIGGERS = ("sse-nginx-guard.path", "sse-nginx-guard.timer")
ENGINE_UNITS = ("sse-engine.service", "sse-indexer.service")
ALL_UNITS = (FILTER_UNIT, GUARD_SERVICE) + GUARD_TRIGGERS + ENGINE_UNITS

MARK_BEGIN = "# >>> synology-search-enhancement"
MARK_END = "# <<< synology-search-enhancement <<<"
STMT_RE = re.compile(r"server[ \t]+unix:" + re.escape(SYNOSCGI_SOCK) + r"[ \t]*;")
SNIPPET_RE = re.compile(re.escape(MARK_BEGIN) + r' original=("[^\n]*") >>>\n(.*?)' + re.escape(MARK_END), re.S)


class Abort(Exception):
    pass


# ── 輸出（有變更的指令也記到 /var/log/sse/sse.log） ──────────────────────

_audit = {"log": None, "pending": []}


def say(msg=""):
    print(msg, flush=True)
    if _audit["log"] is not None:
        _audit["log"].info(msg)
    elif _audit["pending"] is not None:
        _audit["pending"].append(msg)


def start_audit(cmd, keep_days=3):
    if _audit["log"] is not None or not os.path.isdir(LOG_DIR):
        return
    logger = logging.getLogger("sse.audit")
    logger.propagate = False
    logger.setLevel(logging.INFO)
    if filter_module().setup_file_logging(logger, os.path.join(LOG_DIR, "sse.log"), keep_days, "%(asctime)s %(message)s"):
        return
    logger.info("── sse %s %s ──", __version__, cmd)
    for line in _audit["pending"] or []:
        logger.info(line)
    _audit["log"], _audit["pending"] = logger, None


def stop_audit():
    _audit["pending"] = None


def ok(msg):
    say("  ✓ " + msg)


def warn(msg):
    say("  ! " + msg)


def bad(msg):
    say("  ✗ " + msg)


# ── 共用工具 ────────────────────────────────────────────────────────────

def run(cmd, timeout=60):
    try:
        r = subprocess.run(cmd, stdout=subprocess.PIPE, stderr=subprocess.STDOUT, timeout=timeout)
        return r.returncode, r.stdout.decode("utf-8", "replace").strip()
    except (OSError, subprocess.TimeoutExpired) as e:
        return 127, str(e)


def systemctl(*args):
    return run([SYSTEMCTL] + list(args))


# DSM 7.3 是 systemd 219，還沒有 `enable --now` / `disable --now`（220 才有），所以分成兩步
def enable_now(*units):
    code, out = systemctl("enable", *units)
    if code != 0:
        return code, out
    return systemctl("start", *units)


def disable_now(*units):
    systemctl("stop", *units)
    return systemctl("disable", *units)


def read_text(path):
    with open(path, encoding="utf-8", errors="surrogateescape") as f:
        return f.read()


def read_bytes(path):
    with open(path, "rb") as f:
        return f.read()


def write_atomic(path, data, mode=0o644, like=None):
    """寫到同目錄的暫存檔、fsync，再 rename 蓋過去：不會留下寫一半的檔案。"""
    d = os.path.dirname(path)
    os.makedirs(d, exist_ok=True)
    tmp = os.path.join(d, ".%s.sse-tmp" % os.path.basename(path))
    with open(tmp, "wb") as f:
        f.write(data)
        f.flush()
        os.fsync(f.fileno())
    if like is not None:
        os.chmod(tmp, like.st_mode & 0o7777)
        if os.geteuid() == 0:
            os.chown(tmp, like.st_uid, like.st_gid)
    else:
        os.chmod(tmp, mode)
    os.replace(tmp, path)


def remove(path):
    try:
        os.unlink(path)
        return True
    except FileNotFoundError:
        return False


@contextlib.contextmanager
def locked():
    os.makedirs(os.path.dirname(LOCK_FILE), exist_ok=True)
    with open(LOCK_FILE, "w") as f:
        try:
            fcntl.flock(f, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            say("另一個 sse 指令正在執行，等待中…")
            fcntl.flock(f, fcntl.LOCK_EX)
        yield


def require_root():
    if os.geteuid() != 0 and not SANDBOX:
        raise Abort("請用 root 執行：sudo sse ...")


def want_purge(args, data_dir):
    """解除安裝時要不要連資料目錄一起刪。預設保留：重新安裝就不必重建索引（照片多的話要好幾個小時）。"""
    if args.purge:
        return True
    if args.yes or not data_dir or not os.path.exists(data_dir) or not sys.stdin.isatty():
        return False
    v = input("要連資料目錄 %s 一起刪除嗎？（索引資料庫、config.yml、log；保留的話重新安裝不必重建索引）[y/N] "
              % data_dir).strip().lower()
    return v in ("y", "yes")


def confirm(yes, question):
    if yes:
        return
    if not sys.stdin.isatty():
        raise Abort("非互動模式請加 --yes")
    if input("%s [y/N] " % question).strip().lower() not in ("y", "yes"):
        raise Abort("已取消，沒有做任何變更")


def run_as(user, cmd, timeout=None, capture=True):
    """以照片擁有者的身分執行（索引資料庫要由他擁有；root 碰過的檔案他會讀寫不了）。"""
    pw = pwd.getpwnam(user)
    env = {"HOME": pw.pw_dir, "USER": user, "LOGNAME": user, "PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"}
    if SANDBOX:
        env = dict(os.environ)
    demote = None
    if os.geteuid() == 0 and pw.pw_uid != 0:
        def demote():
            os.initgroups(user, pw.pw_gid)
            os.setgid(pw.pw_gid)
            os.setuid(pw.pw_uid)
    cwd = pw.pw_dir if os.path.isdir(pw.pw_dir) else "/"
    try:
        if not capture:
            return subprocess.run(cmd, preexec_fn=demote, env=env, cwd=cwd, timeout=timeout).returncode, ""
        r = subprocess.run(cmd, preexec_fn=demote, env=env, cwd=cwd, stdout=subprocess.PIPE,
                           stderr=subprocess.STDOUT, timeout=timeout)
        return r.returncode, r.stdout.decode("utf-8", "replace").strip()
    except (OSError, subprocess.TimeoutExpired) as e:
        return 127, str(e)


def engine_cmd(state_or_cfg_path, *args):
    path = state_or_cfg_path if isinstance(state_or_cfg_path, str) else config_path(state_or_cfg_path)
    return [ENGINE_PYTHON, "-I", "-m", "sse", "--config", path] + list(args)


# ── DSM / Photos 資訊 ────────────────────────────────────────────────────

def read_kv(path):
    out = {}
    for line in read_text(path).splitlines():
        k, sep, v = line.strip().partition("=")
        if sep:
            out[k.strip()] = v.strip().strip('"')
    return out


def dsm_info():
    try:
        kv = read_kv(DSM_VERSION_FILE)
    except OSError:
        return None, None, None
    return kv.get("majorversion"), kv.get("productversion"), kv.get("buildnumber")


def photos_version():
    try:
        return read_kv(PHOTOS_INFO).get("version")
    except OSError:
        return None


def sock_alive(path):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(2)
    try:
        s.connect(path)
        return True
    except OSError:
        return False
    finally:
        s.close()


def probe_ok(url, timeout=10):
    """透過 nginx 打 DSM 的 SYNO.API.Info：整條 nginx → (filter) → synoscgi 是否正常。"""
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    try:
        with urllib.request.urlopen(url, timeout=timeout, context=ctx) as r:
            if r.status != 200:
                return False
            return json.loads(r.read(1 << 20).decode("utf-8", "replace")).get("success") is True
    except Exception:
        return False


def pick_probe(explicit, state):
    if explicit:
        return explicit if probe_ok(explicit) else None
    urls = ([state["probe_url"]] if state and state.get("probe_url") else []) + list(PROBE_CANDIDATES)
    for url in urls:
        if probe_ok(url):
            return url
    return None


def filter_status():
    try:
        with open(STATUS_FILE, encoding="utf-8") as f:
            return json.load(f)
    except (OSError, ValueError):
        return None


def load_state():
    try:
        with open(STATE, encoding="utf-8") as f:
            st = json.load(f)
    except FileNotFoundError:
        return None
    except ValueError:
        warn("安裝紀錄 %s 損毀，當作沒有紀錄處理" % STATE)
        return None
    # 0.x 版的紀錄：engine 的帳號與目錄記在 engine 底下
    if "data_dir" not in st and isinstance(st.get("engine"), dict):
        st["user"], st["data_dir"] = st["engine"]["user"], st["engine"]["home"]
    return st


def save_state(state):
    state = {k: v for k, v in state.items() if k != "engine"}
    write_atomic(STATE, json.dumps(state, indent=2, ensure_ascii=False).encode("utf-8"))


def need_state():
    st = load_state()
    if not st or not st.get("data_dir"):
        raise Abort("還沒安裝：%s" % INSTALL_CMD)
    return st


def config_path(state):
    return os.path.join(state["data_dir"], "config.yml")


def filter_module():
    spec = importlib.util.spec_from_file_location("sse_filter", os.path.join(DATA, "sse-filter.py"))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def render_filter_config(cfg):
    """由 config.yml 產生 filter 的設定（filter 只用標準庫，讀不了 YAML）。診斷錄製的設定沿用現有的。"""
    f = {"listen": FILTER_SOCK, "socket_mode": "0660", "upstream": SYNOSCGI_SOCK, "status_file": STATUS_FILE,
         "kill_switch": KILL_SWITCH, "photos_info": PHOTOS_INFO, "mode": cfg["filter"]["mode"],
         # 記下現在的 Photos 版本：Photos 更新後回應格式可能改變，filter 會自動暫停注入，sse apply 確認後恢復
         "inject_photos_version": photos_version() if cfg["filter"]["mode"] == "engine" else None,
         "engine_socket": cfg["search"]["socket"], "max_inject": cfg["filter"]["max_inject"],
         "log_file": os.path.join(LOG_DIR, "filter.log"), "log_keep_days": cfg["log"]["keep_days"]}
    try:
        with open(FILTER_CONFIG, encoding="utf-8") as fh:
            old = json.load(fh)
        for k in ("capture_file", "capture_max"):
            if k in old:
                f[k] = old[k]
    except (OSError, ValueError):
        pass
    data = (json.dumps(f, indent=2, ensure_ascii=False) + "\n").encode("utf-8")
    validate_filter_config(data)
    return data


def validate_filter_config(data):
    """用 filter 自己的解析邏輯檢查設定，避免兩邊規則不一致。"""
    tmp = os.path.join(ETC, ".filter.json.check")
    os.makedirs(ETC, exist_ok=True)
    with open(tmp, "wb") as f:
        f.write(data)
    try:
        _, err = filter_module().load_config(tmp)
    finally:
        remove(tmp)
    if err:
        raise Abort(err)


def load_config(path):
    try:
        return cfgm.load(path)
    except cfgm.ConfigError as e:
        raise Abort("config.yml 有誤：%s" % e)


# ── nginx.conf：判斷、套用、還原（純文字處理，方便測試） ──────────────────

def _strip_comments(text):
    return re.sub(r"#[^\n]*", "", text)


def _upstream_span(text):
    hits = list(re.finditer(r"\bupstream\s+synoscgi\s*\{", text))
    if len(hits) != 1:
        return None, "nginx.conf 裡有 %d 個 upstream synoscgi（預期 1 個）" % len(hits)
    start = hits[0].end()
    end = text.find("}", start)
    if end < 0:
        return None, "upstream synoscgi 沒有結尾的 }"
    return (start, end), None


def _snippet(original, indent):
    return ("%s original=%s >>>\n%sserver unix:%s max_fails=1 fail_timeout=5s;\n%sserver unix:%s backup;\n%s%s"
            % (MARK_BEGIN, json.dumps(original), indent, FILTER_SOCK, indent, SYNOSCGI_SOCK, indent, MARK_END))


def _own_line(text, a, b):
    """text[a:b] 是否獨佔一行（前面只有縮排、後面只有空白）；回傳縮排或 None。"""
    line_start = text.rfind("\n", 0, a) + 1
    line_end = text.find("\n", b)
    lead = text[line_start:a]
    tail = text[b:line_end if line_end >= 0 else len(text)]
    if lead.strip() or tail.strip():
        return None
    return lead


def nginx_classify(text):
    """回傳 (pristine | managed | unknown, 說明)。unknown 一律不自動修改。"""
    span, err = _upstream_span(text)
    if err:
        return "unknown", err
    body = text[span[0]:span[1]]
    if MARK_BEGIN in text or MARK_END in text:
        ms = list(SNIPPET_RE.finditer(text))
        if len(ms) != 1 or not (span[0] <= ms[0].start() and ms[0].end() <= span[1]):
            return "unknown", "nginx.conf 裡本外掛的標記區塊不完整，或不在 upstream synoscgi 內"
        m = ms[0]
        try:
            original = json.loads(m.group(1))
        except ValueError:
            return "unknown", "標記裡記錄的原始設定無法解析"
        indent = _own_line(text, m.start(), m.start() + len(m.group(0).split("\n", 1)[0]))
        second = m.group(2).split("\n", 1)[0]
        if indent is None or not isinstance(original, str) or not STMT_RE.fullmatch(original) or \
                m.group(0) != _snippet(original, second[:len(second) - len(second.lstrip())]):
            return "unknown", "本外掛的標記區塊被手動修改過，不自動處理"
        rest = text[span[0]:m.start()] + text[m.end():span[1]]
        if _strip_comments(rest).strip():
            return "unknown", "upstream synoscgi 裡除了本外掛的區塊還有其他設定"
        return "managed", "已套用（filter 為主、synoscgi 為 backup）"
    stmts = [s.strip() for s in _strip_comments(body).split(";") if s.strip()]
    m = STMT_RE.search(body)
    if len(stmts) == 1 and m and re.fullmatch(r"server\s+unix:" + re.escape(SYNOSCGI_SOCK), stmts[0]):
        if _own_line(text, span[0] + m.start(), span[0] + m.end()) is None:
            return "unknown", "upstream synoscgi 的 server 不是獨立一行，格式跟預期不同"
        return "pristine", "原廠設定（直連 synoscgi）"
    if "filter.sock" in body or "synosse" in body:
        return "unknown", "upstream synoscgi 指向其他 filter，請先還原 nginx.conf"
    return "unknown", "upstream synoscgi 的內容不是預期的單一 server：%s" % "; ".join(stmts)


def nginx_patch(text):
    kind, why = nginx_classify(text)
    if kind != "pristine":
        raise Abort(why)
    span, _ = _upstream_span(text)
    m = STMT_RE.search(text, span[0], span[1])
    a, b = m.start(), m.end()
    return text[:a] + _snippet(m.group(0), _own_line(text, a, b)) + text[b:]


def nginx_unpatch(text):
    kind, why = nginx_classify(text)
    if kind == "pristine":
        return text
    if kind != "managed":
        raise Abort(why)
    m = SNIPPET_RE.search(text)
    return text[:m.start()] + json.loads(m.group(1)) + text[m.end():]


def nginx_test(path):
    code, out = run([NGINX_BIN, "-t", "-c", path])
    return code == 0, out


def nginx_reload():
    cmd = [NGINX_BIN, "-c", NGINX_CONF]
    try:
        has_pid = re.search(r"^\s*pid\s", read_text(NGINX_CONF), re.M)
    except OSError:
        has_pid = True
    if not has_pid:
        # 跟 DSM 啟動 master 時一樣指定 pid 檔，reload 訊號才會送到正在跑的 master
        cmd += ["-g", "pid %s;" % NGINX_PID]
    code, out = run(cmd + ["-s", "reload"])
    return code == 0, out


def nginx_write(new_text, probe_url):
    """nginx -t → 原子替換 → reload → 健康檢查；任何一步失敗都還原成原本的檔案。"""
    if os.path.islink(NGINX_CONF):
        raise Abort("%s 是 symlink，跟預期不同，不自動修改" % NGINX_CONF)
    old = read_bytes(NGINX_CONF)
    st = os.stat(NGINX_CONF)
    tmp = os.path.join(os.path.dirname(NGINX_CONF), ".nginx.conf.sse-test")
    # 暫存檔放同一個目錄，nginx -t 解析相對路徑的 include 時才會跟正式設定一樣
    with open(tmp, "wb") as f:
        f.write(new_text.encode("utf-8", "surrogateescape"))
    try:
        good, out = nginx_test(tmp)
    finally:
        remove(tmp)
    if not good:
        raise Abort("nginx -t 沒通過，nginx.conf 沒有變更：\n" + out)
    write_atomic(NGINX_CONF, new_text.encode("utf-8", "surrogateescape"), like=st)
    good, out = nginx_reload()
    if good and probe_url:
        time.sleep(1.5)  # 等新的 worker 接手
        if not probe_ok(probe_url):
            good, out = False, "DSM 健康檢查失敗"
    if not good:
        write_atomic(NGINX_CONF, old, like=st)
        nginx_reload()
        raise Abort("套用後 %s，已自動還原 nginx.conf" % out)


def nginx_apply(probe_url):
    text = read_text(NGINX_CONF)
    kind, why = nginx_classify(text)
    if kind == "managed":
        return False
    if kind != "pristine":
        raise Abort(why)
    if not sock_alive(FILTER_SOCK):
        raise Abort("filter 沒有在跑，不套用 nginx 設定")
    new = nginx_patch(text)
    write_atomic(NGINX_BACKUP, text.encode("utf-8", "surrogateescape"), like=os.stat(NGINX_CONF))
    nginx_write(new, probe_url)
    return True


def nginx_revert():
    """還原成原廠設定。還原後的狀態就是原生狀態，所以健康檢查失敗也不會再套回去。"""
    text = read_text(NGINX_CONF)
    kind, why = nginx_classify(text)
    if kind == "pristine":
        return False
    if kind != "managed":
        raise Abort(why)
    nginx_write(nginx_unpatch(text), None)
    return True


# ── 身分 ────────────────────────────────────────────────────────────────

def nginx_worker_user():
    if os.path.isdir("/proc") and not SANDBOX:
        for pid in os.listdir("/proc"):
            if not pid.isdigit():
                continue
            try:
                with open("/proc/%s/cmdline" % pid, "rb") as f:
                    if b"nginx: worker process" not in f.read():
                        continue
                st = os.stat("/proc/%s" % pid)
                return pwd.getpwuid(st.st_uid).pw_name, grp.getgrgid(st.st_gid).gr_name
            except (OSError, KeyError):
                continue
    try:
        m = re.search(r"^\s*user\s+([^\s;]+)(?:\s+([^\s;]+))?\s*;", read_text(NGINX_CONF), re.M)
        if m:
            pw = pwd.getpwnam(m.group(1))
            return pw.pw_name, m.group(2) or grp.getgrgid(pw.pw_gid).gr_name
    except (OSError, KeyError):
        pass
    return None, None


def can_connect_as(path, user):
    code = "import socket,sys\ns=socket.socket(socket.AF_UNIX)\ns.settimeout(3)\ns.connect(sys.argv[1])"
    rc, _ = run_as(user, [sys.executable, "-I", "-c", code, path], timeout=10)
    return rc == 0


def resolve_owner(args, state):
    """照片擁有者（索引與搜尋服務以他的身分執行）與資料目錄。"""
    user = args.user or (state or {}).get("user") or os.environ.get("SUDO_USER")
    if not user or user == "root":
        raise Abort("要指定照片擁有者的帳號：%s -s -- --user <DSM 帳號>" % INSTALL_CMD)
    try:
        pw = pwd.getpwnam(user)
    except KeyError:
        raise Abort("這台 NAS 沒有 %s 這個帳號" % user)
    if pw.pw_uid == 0:
        raise Abort("索引與搜尋服務不能用 root 執行，請指定照片擁有者的帳號")
    data_dir = args.data_dir or (state or {}).get("data_dir")
    if not data_dir:
        if not os.path.isdir(pw.pw_dir):
            raise Abort("%s 沒有家目錄（%s）：請到 DSM 控制台 > 使用者 > 進階設定 啟用家目錄服務，"
                        "或用 --data-dir 指定共用資料夾裡的目錄" % (user, pw.pw_dir))
        data_dir = os.path.join(pw.pw_dir, "sse")
    return user, pw, os.path.realpath(data_dir)


def prepare_data_dir(user, pw, data_dir):
    if os.path.exists(data_dir):
        st = os.stat(data_dir)
        if st.st_uid != pw.pw_uid and not SANDBOX:
            raise Abort("%s 的擁有者不是 %s" % (data_dir, user))
    for d in (data_dir, os.path.join(data_dir, "data"), os.path.join(data_dir, "logs")):
        os.makedirs(d, exist_ok=True)
        os.chmod(d, 0o750)   # DSM 的 homes 有 ACL，新目錄可能繼承成所有人可寫，明確收緊
        if os.geteuid() == 0:
            os.chown(d, pw.pw_uid, pw.pw_gid)


def ensure_config(args, user, pw, data_dir, dry_run):
    """確保 data_dir/config.yml 存在且正確。回傳 (路徑, 說明)。"""
    path = os.path.join(data_dir, "config.yml")
    gid = pw.pw_gid
    if os.path.exists(path) and not args.import_config:
        load_config(path)
        return path, "沿用 %s" % path
    if dry_run:
        return path, "會建立 %s" % path
    if args.import_config:
        with open(args.import_config, encoding="utf-8") as f:
            text = f.read()
        how = "從 %s 匯入" % args.import_config
    elif os.path.exists(os.path.join(data_dir, "config.toml")):
        text = wizard.from_toml(os.path.join(data_dir, "config.toml"))
        how = "從舊版的 config.toml 轉換"
    elif sys.stdin.isatty():
        text = wizard.interactive(user)
        how = "設定精靈"
    else:
        wizard.write_config(path, wizard.render({("synology", "account"): user}), pw.pw_uid, gid)
        raise Abort("已建立設定檔範本 %s：填好 DSM 密碼與模型服務後，再執行一次安裝：\n    %s" % (path, INSTALL_CMD))
    wizard.write_config(path, text, pw.pw_uid, gid)
    load_config(path)
    return path, "%s，寫到 %s" % (how, path)


# ── 共用流程 ────────────────────────────────────────────────────────────

def wait_filter_ready(timeout=20):
    end = time.time() + timeout
    while time.time() < end:
        st = filter_status()
        if sock_alive(FILTER_SOCK) and st and st.get("version"):
            return st
        time.sleep(0.3)
    raise Abort("filter 沒有在 %d 秒內啟動（sudo sse logs filter）" % timeout)


def wait_injection(mode, timeout=10):
    st = wait_filter_ready()
    end = time.time() + timeout
    while time.time() < end and (st["mode"] != mode or (mode != "passthrough" and not st["injection_active"])):
        time.sleep(0.5)
        st = filter_status() or st
    return st


def verify_traffic(probe_url, timeout=15):
    """確認 DSM 正常，而且流量真的經過 filter 並成功轉給 synoscgi。

    只看健康檢查不夠：filter 連不上 synoscgi 時，nginx 會把 GET 重試到 backup，健康檢查照樣通過。
    """
    s0 = filter_status()
    conns = s0["stats"]["connections"] if s0 else 0
    errors = s0["stats"]["upstream_errors"] if s0 else 0
    end = time.time() + timeout
    while time.time() < end:
        if not probe_ok(probe_url):
            raise Abort("DSM 健康檢查失敗")
        time.sleep(1.2)
        s = filter_status()
        if s and s["stats"]["upstream_errors"] > errors:
            raise Abort("filter 連不上 synoscgi：%s" % ((s.get("last_error") or {}).get("message") or ""))
        if s and s["stats"]["connections"] > conns:
            return
    raise Abort("nginx 沒有把流量送進 filter（設定沒生效）")


def render_unit(name, ctx, cfg_path):
    with open(os.path.join(DATA, "systemd", name), encoding="utf-8") as f:
        tpl = string.Template(f.read())
    return tpl.substitute(user=ctx["nginx_user"], group=ctx["nginx_group"], euser=ctx["user"],
                          egroup=ctx["nginx_group"], python=FILTER_PYTHON, engine_python=ENGINE_PYTHON,
                          prefix=PREFIX, filter_config=FILTER_CONFIG, config=cfg_path, nginx_conf=NGINX_CONF)


def engine_ping(sock, timeout=5):
    s = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)
    s.settimeout(timeout)
    try:
        s.connect(sock)
        s.sendall(b'{"op":"ping"}\n')
        buf = b""
        while not buf.endswith(b"\n") and len(buf) < 65536:
            chunk = s.recv(4096)
            if not chunk:
                break
            buf += chunk
        r = json.loads(buf.decode("utf-8"))
        return r if r.get("ok") else None
    except (OSError, ValueError):
        return None
    finally:
        s.close()


def wait_engine(sock, timeout=None):
    # 第一次啟動要建資料庫、載入向量；SSE_ENGINE_WAIT 給測試縮短
    timeout = timeout or int(os.environ.get("SSE_ENGINE_WAIT", "120"))
    end = time.time() + timeout
    while time.time() < end:
        ping = engine_ping(sock)
        if ping:
            return ping
        time.sleep(1)
    raise Abort("搜尋服務沒有在 %d 秒內回應（sudo sse logs engine）" % timeout)


def old_version_leftovers(state):
    """0.x 版留下的東西（不影響運作，確認不需要後可以刪）。"""
    found = [p for p in (os.path.join(PREFIX, "ssectl.py"), os.path.join(PREFIX, "systemd")) if os.path.exists(p)]
    d = (state or {}).get("data_dir")
    if d:
        found += [os.path.join(d, n) for n in ("venv", "src", "python", "cache", "bin", "secrets", "config.toml")
                  if os.path.exists(os.path.join(d, n))]
    return found


# ── install ─────────────────────────────────────────────────────────────

def preflight(args, state):
    say("安裝前檢查：")
    problems = []
    ctx = {}

    def check(cond, good, why):
        if cond:
            ok(good)
        else:
            bad(why)
            problems.append(why)
        return cond

    major, ver, build = dsm_info()
    check(major == "7", "DSM %s（build %s）" % (ver, build), "需要 DSM 7（讀到 %r）" % major)
    ctx["dsm_build"] = build
    pv = photos_version()
    check(pv, "Synology Photos %s" % pv, "找不到 Synology Photos（%s）" % PHOTOS_INFO)
    ctx["photos_version"] = pv
    code, out = run([FILTER_PYTHON, "-c", "import sys; print('%d.%d' % sys.version_info[:2])"])
    py_ok = code == 0 and re.fullmatch(r"\d+\.\d+", out) and tuple(map(int, out.split("."))) >= (3, 8)
    check(py_ok, "DSM 的 Python %s（filter 用）" % out, "需要 DSM 內建的 Python 3.8 以上（%s：%s）" % (FILTER_PYTHON, out))
    code, out = run([SYSTEMCTL, "--version"])
    check(code == 0, "systemd 可用", "systemctl 無法執行：%s" % out)
    try:
        conf = read_text(NGINX_CONF)
    except OSError as e:
        conf = None
        check(False, "", "讀不到 %s：%s" % (NGINX_CONF, e))
    if conf is not None:
        check(not os.path.islink(NGINX_CONF), "nginx.conf 是一般檔案", "nginx.conf 是 symlink，跟預期不同")
        good, out = nginx_test(NGINX_CONF)
        check(good, "nginx -t 目前的設定通過", "nginx -t 目前就不通過，請先處理：%s" % out)
        kind, why = nginx_classify(conf)
        check(kind in ("pristine", "managed"), "nginx upstream：%s" % why, why)
        ctx["nginx_kind"] = kind
    check(sock_alive(SYNOSCGI_SOCK), "synoscgi socket 正常", "連不上 %s" % SYNOSCGI_SOCK)
    user, group = nginx_worker_user()
    if check(user, "nginx worker 身分：%s:%s" % (user, group), "判斷不出 nginx worker 的身分"):
        check(can_connect_as(SYNOSCGI_SOCK, user), "%s 可以連 synoscgi（filter 會用這個身分執行）" % user,
              "%s 連不上 synoscgi" % user)
    ctx["nginx_user"], ctx["nginx_group"] = user, group
    url = pick_probe(args.probe_url, state)
    check(url, "DSM 健康檢查：%s" % url, "DSM 健康檢查網址都失敗，請用 --probe-url 指定")
    ctx["probe_url"] = url
    if problems:
        raise Abort("有 %d 項檢查沒通過，沒有做任何變更。" % len(problems))
    return ctx


def rollback(fresh, written, nginx_changed, probe_url, new_dirs):
    say("開始回復到安裝前的狀態：")
    steps = []
    if fresh:
        steps.append(("停用 guard", lambda: disable_now(*GUARD_TRIGGERS)))
    if nginx_changed:
        steps.append(("還原 nginx.conf", nginx_revert))
    if fresh:
        steps.append(("停用服務", lambda: disable_now(FILTER_UNIT, *ENGINE_UNITS)))
    for path, old in reversed(written):
        if old is None:
            steps.append(("移除 %s" % path, lambda p=path: remove(p)))
        else:
            steps.append(("還原 %s" % path, lambda p=path, o=old: write_atomic(p, o[0], o[1])))
    for d in sorted(new_dirs, reverse=True):
        steps.append(("移除目錄 %s" % d, lambda d=d: shutil.rmtree(d, ignore_errors=True) if d == LOG_DIR
                      else (os.path.isdir(d) and os.rmdir(d))))
    steps.append(("重新載入 systemd", lambda: systemctl("daemon-reload")))
    if not fresh:
        steps.append(("用舊版重新啟動服務", lambda: systemctl("restart", FILTER_UNIT, *ENGINE_UNITS)))
    for desc, fn in steps:
        try:
            fn()
            ok(desc)
        except Exception as e:
            bad("%s 失敗：%s" % (desc, e))
    if probe_url and probe_ok(probe_url):
        ok("回復完成，DSM 回應正常")
    else:
        bad("回復後 DSM 健康檢查沒通過，請執行 sudo sse doctor，或照 README 的「手動還原」處理")


def cmd_install(args):
    require_root()
    with locked():
        state = load_state()
        fresh = not (state and state.get("version"))
        ctx = preflight(args, state)
        user, pw, data_dir = resolve_owner(args, state)
        ctx["user"] = user
        ok("照片擁有者：%s，資料目錄：%s" % (user, data_dir))
        if not args.dry_run:
            prepare_data_dir(user, pw, data_dir)
        cfg_path, how = ensure_config(args, user, pw, data_dir, args.dry_run)
        ok("設定檔：%s" % how)
        if os.path.exists(cfg_path):
            say("\n檢查 DSM 帳號與模型服務（以 %s 的身分）：" % user)
            code, out = run_as(user, engine_cmd(cfg_path, "check"), timeout=600)
            for line in out.splitlines():
                say("  " + line)
            if code != 0:
                raise Abort("檢查沒通過（還沒改動系統）：修正 %s 後再執行一次安裝：\n    %s" % (cfg_path, INSTALL_CMD))
        cfg = load_config(cfg_path) if os.path.exists(cfg_path) else None
        mode = cfg["filter"]["mode"] if cfg else "engine"
        say("\n將會：")
        say("  - 安裝 filter 與服務（filter 以 %s 身分執行，索引與搜尋以 %s 身分執行）" % (ctx["nginx_user"], user))
        if ctx["nginx_kind"] == "pristine":
            say("  - nginx upstream 改成 filter 為主、synoscgi 為 backup（原檔備份到 %s）" % NGINX_BACKUP)
        say("  - 模式：%s" % ("engine（AI 找到的照片加進原生搜尋結果）" if mode == "engine" else "passthrough（只轉發）"))
        if args.dry_run:
            say("\n--dry-run：所有檢查都通過，沒有做任何變更。")
            return 0
        confirm(args.yes, "\n確定要%s嗎？" % ("安裝" if fresh else "升級"))
        written = []
        nginx_changed = False
        new_dirs = [d for d in (ETC, LOG_DIR) if not os.path.exists(d)]
        # log 目錄給 filter（nginx worker 身分）寫；sse 自己以 root 寫 sse.log
        os.makedirs(LOG_DIR, mode=0o750, exist_ok=True)
        if os.geteuid() == 0:
            os.chown(LOG_DIR, pwd.getpwnam(ctx["nginx_user"]).pw_uid, grp.getgrnam(ctx["nginx_group"]).gr_gid)
        os.chmod(LOG_DIR, 0o750)
        start_audit("install", cfg["log"]["keep_days"])

        def put(path, data, mode=0o644):
            old = (read_bytes(path), os.stat(path).st_mode & 0o7777) if os.path.exists(path) else None
            write_atomic(path, data, mode)
            written.append((path, old))

        def track(path):
            written.append((path, (read_bytes(path), os.stat(path).st_mode & 0o7777) if os.path.exists(path) else None))

        say("\n%s中：" % ("安裝" if fresh else "升級"))
        try:
            put(os.path.join(PREFIX, "sse-filter.py"), read_bytes(os.path.join(DATA, "sse-filter.py")))
            # 先以純轉發模式接上 nginx，確認沒問題再開啟注入
            current_mode = (filter_status() or {}).get("mode") if not fresh else None
            first = dict(cfg, filter=dict(cfg["filter"], mode=current_mode or "passthrough"))
            put(FILTER_CONFIG, render_filter_config(first))
            for unit in ALL_UNITS:
                put(os.path.join(UNIT_DIR, unit), render_unit(unit, ctx, cfg_path).encode())
            put(BIN_LINK, ('#!/bin/sh\nexec "%s" -I -m sse "$@"\n' % ENGINE_PYTHON).encode(), 0o755)
            ok("檔案已就位")
            for a in (("daemon-reload",), ("enable", FILTER_UNIT), ("restart", FILTER_UNIT)):
                code, out = systemctl(*a)
                if code != 0:
                    raise Abort("systemctl %s 失敗：%s" % (" ".join(a), out))
            st = wait_filter_ready()
            ok("filter %s 已啟動" % st["version"])
            if ctx["nginx_kind"] == "pristine":
                track(NGINX_BACKUP)
            nginx_changed = nginx_apply(ctx["probe_url"])
            ok("nginx 已改為 filter 為主、synoscgi 為 backup" if nginx_changed else "nginx 設定原本就是套用狀態")
            verify_traffic(ctx["probe_url"])
            ok("流量確認經過 filter，DSM 回應正常")
            code, out = enable_now(*GUARD_TRIGGERS)
            if code != 0:
                raise Abort("啟用 guard 失敗：%s" % out)
            ok("guard 已啟用（DSM 重新產生 nginx.conf 時自動重新套用）")
            for a in (("enable", *ENGINE_UNITS), ("restart", *ENGINE_UNITS)):
                code, out = systemctl(*a)
                if code != 0:
                    raise Abort("systemctl %s 失敗：%s" % (" ".join(a), out))
            ping = wait_engine(cfg["search"]["socket"])
            ok("索引器與搜尋服務已啟動（目前索引 %d 張）" % ping.get("items", 0))
            if not can_connect_as(cfg["search"]["socket"], ctx["nginx_user"]):
                raise Abort("%s 連不上搜尋服務（%s）" % (ctx["nginx_user"], cfg["search"]["socket"]))
            put(FILTER_CONFIG, render_filter_config(cfg))
            code, out = systemctl("restart", FILTER_UNIT)
            if code != 0:
                raise Abort("重啟 filter 失敗：%s" % out)
            st = wait_injection(cfg["filter"]["mode"])
            ok("模式：%s（%s）" % (st["mode"], st.get("injection_reason") or ""))
            new_state = dict(state or {})
            new_state.update({"version": __version__, "installed_at": new_state.get("installed_at", int(time.time())),
                              "updated_at": int(time.time()), "dsm_build": ctx["dsm_build"],
                              "channel": args.channel or new_state.get("channel") or "release",
                              "source": args.source or "local",
                              "photos_version": ctx["photos_version"], "nginx_user": ctx["nginx_user"],
                              "nginx_group": ctx["nginx_group"], "probe_url": ctx["probe_url"], "auto_apply": True,
                              "user": user, "data_dir": data_dir})
            save_state(new_state)
        except Exception as e:
            bad(str(e))
            rollback(fresh, written, nginx_changed, ctx["probe_url"], new_dirs)
            return 1
        for p in (os.path.join(PREFIX, "ssectl.py"),):
            remove(p)
        shutil.rmtree(os.path.join(PREFIX, "systemd"), ignore_errors=True)
        left = old_version_leftovers(new_state)
        if left:
            warn("舊版留下的檔案已經用不到，確認後可以刪除：%s" % "、".join(left))
        say("\n完成。索引器會在背景建立索引（照片多的話要幾個小時），做到哪、搜尋就加到哪。")
        say("  看進度：sudo sse status")
        say("  改設定：編輯 %s，再執行 sudo sse apply" % cfg_path)
        return 0


# ── update：交給 install.sh（下載新版，用新版自己的安裝流程） ─────────────────

def cmd_update(args):
    require_root()
    script = os.path.join(PREFIX, "install.sh")
    if not os.path.exists(script):
        raise Abort("找不到 %s，請重新安裝（設定與索引會保留）：%s" % (script, INSTALL_CMD))
    cmd = ["sh", script, "update"]
    if args.check:
        cmd.append("--check")
    if args.force:
        cmd.append("--force")
    if args.channel:
        cmd += ["--channel", args.channel]
    if args.want:
        cmd += ["--version", args.want]
    sys.stdout.flush()
    # 換成 install.sh：更新會替換 /usr/local/sse/venv，也就是這個 Python 本身
    os.execvp("sh", cmd)


# ── apply：套用 config.yml ────────────────────────────────────────────────

def cmd_apply(args):
    require_root()
    with locked():
        state = need_state()
        path = config_path(state)
        cfg = load_config(path)
        ok("config.yml 格式正確")
        if cfg["synology"]["account"] != state["user"]:
            warn("synology.account（%s）跟執行服務的帳號（%s）不同：DSM 登入用前者，檔案權限用後者"
                 % (cfg["synology"]["account"], state["user"]))
        if args.check:
            code, out = run_as(state["user"], engine_cmd(path, "check"), timeout=600)
            for line in out.splitlines():
                say("  " + line)
            if code != 0:
                raise Abort("檢查沒通過，沒有套用")
        data = render_filter_config(cfg)
        changed = not os.path.exists(FILTER_CONFIG) or read_bytes(FILTER_CONFIG) != data
        write_atomic(FILTER_CONFIG, data)
        if changed or not sock_alive(FILTER_SOCK):
            code, out = systemctl("restart", FILTER_UNIT)
            if code != 0:
                raise Abort("重啟 filter 失敗：%s" % out)
        code, out = systemctl("restart", *ENGINE_UNITS)
        if code != 0:
            raise Abort("重啟索引器與搜尋服務失敗：%s" % out)
        wait_engine(cfg["search"]["socket"])
        ok("索引器與搜尋服務已用新設定重新啟動")
        st = wait_injection(cfg["filter"]["mode"])
        ok("filter：模式 %s（%s）" % (st["mode"], st.get("injection_reason") or ""))
        state["photos_version"] = photos_version()
        save_state(state)
        return 0


# ── uninstall ───────────────────────────────────────────────────────────

def cmd_uninstall(args):
    require_root()
    with locked():
        state = load_state() or {}
        url = pick_probe(args.probe_url, state)
        data_dir = state.get("data_dir")
        purge = want_purge(args, data_dir)
        say("將會：停用並移除所有服務、把 nginx.conf 還原成原廠設定、移除 %s。" % PREFIX)
        if data_dir:
            say("資料目錄 %s（索引資料庫、config.yml）：%s" % (data_dir, "一併刪除" if purge else "保留"))
        say("不會動：Synology Photos 的資料庫與照片。")
        confirm(args.yes, "確定要解除安裝嗎？")
        say("\n解除安裝中：")
        # 1. 先停 guard，免得它在還原後又把設定套回去
        disable_now(*GUARD_TRIGGERS)
        ok("guard 已停用")
        # 2. 還原 nginx；還原不了就停在這裡，不移除 filter
        try:
            text = read_text(NGINX_CONF)
        except OSError as e:
            raise Abort("讀不到 %s：%s" % (NGINX_CONF, e))
        kind, why = nginx_classify(text)
        if kind == "managed":
            nginx_revert()
            ok("nginx.conf 已還原為原廠設定")
        elif kind == "pristine":
            ok("nginx.conf 已經是原廠設定")
        elif FILTER_SOCK in text:
            bad(why)
            say("nginx.conf 仍指向 filter 但格式跟預期不同，為了安全，停在這裡不移除 filter。")
            say("請照 README 的「手動還原」處理 nginx.conf 後，再執行一次 uninstall。")
            return 1
        else:
            warn("nginx.conf 的 upstream 跟預期不同，但沒有指向本外掛，略過：%s" % why)
        if FILTER_SOCK in read_text(NGINX_CONF):
            bad("nginx.conf 仍然指向 filter，停止解除安裝")
            return 1
        # 3. 停服務、移除 systemd 設定
        disable_now(FILTER_UNIT, *ENGINE_UNITS)
        for unit in ALL_UNITS:
            remove(os.path.join(UNIT_DIR, unit))
        systemctl("daemon-reload")
        systemctl("reset-failed", *ALL_UNITS)
        ok("服務已移除")
        # 4. 移除設定、log、備份、程式（程式最後刪：這個指令本身就在跑 /usr/local/sse 裡的 Python）
        for path in (STATE, KILL_SWITCH, FILTER_CONFIG, NGINX_BACKUP, BIN_LINK, FILTER_SOCK, STATUS_FILE, CAPTURE_FILE):
            remove(path)
        for d in (ETC, RUN_DIR):
            with contextlib.suppress(OSError):
                os.rmdir(d)
        shutil.rmtree(LOG_DIR, ignore_errors=True)
        if purge and data_dir and os.path.exists(os.path.join(data_dir, "config.yml")):
            shutil.rmtree(data_dir, ignore_errors=True)
            ok("資料目錄已刪除：%s" % data_dir)
        shutil.rmtree(PREFIX, ignore_errors=True)
        ok("程式與設定已移除")
        left = [p for p in [os.path.join(UNIT_DIR, u) for u in ALL_UNITS] + [PREFIX, ETC, NGINX_BACKUP, BIN_LINK,
                                                                           FILTER_SOCK, LOG_DIR] if os.path.exists(p)]
        if left:
            warn("還有殘留：%s" % ", ".join(left))
        if url:
            healthy = probe_ok(url)
            (ok if healthy else bad)("DSM 健康檢查" + ("正常" if healthy else "失敗"))
        if data_dir and not purge and os.path.exists(data_dir):
            say("\n資料目錄保留在 %s；不需要的話：rm -rf %s" % (data_dir, data_dir))
        say("\n解除安裝完成。" if not left else "\n解除安裝完成，但有殘留，見上方。")
        say("搜尋已回到 Synology Photos 原本的結果：只找得到 Photos 自己認得的（物件分類、人物、地點、標籤），\n"
            "照片裡的文字與 AI 相似搜尋找到的照片不會再出現。已經開著的 Photos 網頁請重新整理，App 請重新開啟。")
        return 0 if not left else 1


# ── status / doctor / logs ──────────────────────────────────────────────

def collect():
    state = load_state()
    info = {"state": state, "dsm": dsm_info(), "photos": photos_version(), "units": {},
            "filter_alive": sock_alive(FILTER_SOCK), "filter": filter_status(),
            "kill_switch": os.path.exists(KILL_SWITCH), "engine": None, "cfg": None}
    for u in ALL_UNITS:
        if os.path.exists(os.path.join(UNIT_DIR, u)):
            info["units"][u] = (systemctl("is-active", u)[1] or "?", systemctl("is-enabled", u)[1] or "?")
    if state and state.get("data_dir"):
        try:
            info["cfg"] = cfgm.load(config_path(state))
            info["engine"] = engine_ping(info["cfg"]["search"]["socket"])
        except cfgm.ConfigError as e:
            info["cfg_error"] = str(e)
    try:
        info["nginx"] = nginx_classify(read_text(NGINX_CONF))
    except OSError as e:
        info["nginx"] = ("unknown", "讀不到 nginx.conf：%s" % e)
    return info


def show(info):
    st = info["state"]
    say("synology-photos-search-enhancement %s" % __version__)
    if not st:
        say("  還沒安裝")
    else:
        say("  照片擁有者：%s，資料目錄：%s" % (st.get("user"), st.get("data_dir")))
        source = st.get("source") or "local"
        say("  更新管道：%s（%s；檢查新版：sudo sse update --check）"
            % (st.get("channel") or "release", "從原始碼安裝" if source == "local" else "release " + source))
    say("  DSM %s（build %s），Photos %s" % (info["dsm"][1], info["dsm"][2], info["photos"]))
    say("  nginx：%s" % info["nginx"][1])
    for u, (active, enabled) in info["units"].items():
        say("  %-26s %s / %s" % (u, active, enabled))
    f = info["filter"]
    if info["filter_alive"] and f:
        s = f["stats"]
        say("  filter：模式 %s，%s" % (f["mode"], f.get("injection_reason") or ""))
        say("          連線 %d（進行中 %d）、注入 %d、略過 %d、注入錯誤 %d、synoscgi 錯誤 %d" %
            (s["connections"], s["active"], s["injected"], s["inject_skipped"], s["inject_errors"],
             s["upstream_errors"]))
        if f.get("last_error"):
            say("          最後錯誤：%s（%s）" % (f["last_error"]["message"],
                                            time.strftime("%Y-%m-%d %H:%M:%S", time.localtime(f["last_error"]["at"]))))
    else:
        say("  filter：沒有在跑（nginx 會自動直連 synoscgi，DSM 照常運作）")
    if info.get("cfg_error"):
        say("  config.yml：%s" % info["cfg_error"])
    elif st and not info["engine"]:
        say("  搜尋服務：沒有回應")
    elif info["engine"]:
        say("  搜尋服務：索引 %d 張" % info["engine"].get("items", 0))
    if f and f.get("capture"):
        say("  診斷錄製：%s（%d / %d 筆）" % (f["capture"]["file"], f["capture"]["count"], f["capture"]["max"]))
    if info["kill_switch"]:
        say("  緊急開關：開啟（不注入）")


def cmd_status(args):
    info = collect()
    show(info)
    st = info["state"]
    if st and st.get("data_dir") and info["cfg"]:
        code, out = run_as(st["user"], engine_cmd(st, "progress"), timeout=60)
        say("")
        for line in out.splitlines():
            say(line)
    url = pick_probe(getattr(args, "probe_url", None), st)
    say("\n  DSM 健康檢查：%s" % ("正常" if url else "失敗"))
    return 0


def log_path(source, state):
    if source in ("filter", "sse"):
        return os.path.join(LOG_DIR, source + ".log")
    return os.path.join(need_state()["data_dir"], "logs", source + ".log")


def cmd_logs(args):
    require_root()
    path = log_path(args.source, load_state())
    if not os.path.exists(path):
        raise Abort("還沒有 log：%s" % path)
    older = sorted(n for n in os.listdir(os.path.dirname(path)) if n.startswith(os.path.basename(path) + "."))
    print("==> %s%s <==" % (path, "（另有：%s）" % "、".join(older) if older else ""), flush=True)
    if args.follow:
        os.execvp("tail", ["tail", "-n", str(args.n), "-F", path])
    code, out = run(["tail", "-n", str(args.n), path])
    print(out)
    return code


def cmd_doctor(args):
    info = collect()
    show(info)
    say("\n診斷：")
    st, advice = info["state"], []
    url = pick_probe(args.probe_url, st)
    (ok if url else bad)("DSM 健康檢查" + ("正常" if url else "失敗"))
    good, out = nginx_test(NGINX_CONF)
    (ok if good else bad)("nginx -t " + ("通過" if good else "沒通過：" + out))
    (ok if sock_alive(SYNOSCGI_SOCK) else bad)("synoscgi socket")
    if st:
        if info["dsm"][2] != st.get("dsm_build"):
            warn("DSM 更新過（build %s → %s），已暫停自動套用 nginx 設定" % (st.get("dsm_build"), info["dsm"][2]))
            advice.append("確認 DSM 與 Photos 都正常後，執行 sudo sse reapply 重新套用")
        if not st.get("auto_apply", True):
            warn("目前是 bypass 狀態（nginx 直連 synoscgi）")
            advice.append("要恢復時執行 sudo sse bypass off")
        elif info["nginx"][0] == "pristine" and info["dsm"][2] == st.get("dsm_build"):
            warn("nginx 是原廠設定，guard 應該會在 10 分鐘內重新套用")
            advice.append("想立刻套用可執行 sudo sse reapply")
        if info["nginx"][0] == "unknown":
            bad("nginx.conf 的格式跟預期不同，本外掛不會自動修改它")
            advice.append("參考 README 的「手動還原」")
        if not info["filter_alive"]:
            bad("filter 沒有在跑")
            advice.append("看 log：sudo sse logs filter")
        if info.get("cfg_error"):
            bad("config.yml 有誤：%s" % info["cfg_error"])
            advice.append("修正 config.yml 後執行 sudo sse apply")
        f = info["filter"] or {}
        if f.get("mode") == "engine" and not info.get("engine"):
            bad("搜尋服務沒有回應（搜尋結果暫時只有原生的）")
            advice.append("看 log：sudo sse logs engine")
        if f.get("mode") == "engine" and not f.get("injection_active") and not info["kill_switch"]:
            warn("注入暫停中：%s" % f.get("injection_reason"))
            advice.append("Photos 更新過的話，確認搜尋正常後執行 sudo sse apply 重新啟用")
    for p in old_version_leftovers(st):
        warn("舊版留下的檔案：%s" % p)
    if advice:
        say("\n建議：")
        for a in advice:
            say("  - " + a)
    else:
        say("\n沒有發現問題。")
    return 0


# ── 維運指令 ────────────────────────────────────────────────────────────

def cmd_disable(args):
    require_root()
    need_state()
    write_atomic(KILL_SWITCH, b"", 0o644)
    ok("緊急開關已開啟：5 秒內停止注入，filter 繼續純轉發。恢復：sudo sse enable")
    return 0


def cmd_enable(args):
    require_root()
    need_state()
    remove(KILL_SWITCH)
    ok("緊急開關已關閉")
    return 0


def apply_nginx_now(state, args):
    say("重新套用前檢查：")
    build = dsm_info()[2]
    good, out = nginx_test(NGINX_CONF)
    if not good:
        raise Abort("nginx -t 沒通過：%s" % out)
    ok("nginx -t 通過")
    kind, why = nginx_classify(read_text(NGINX_CONF))
    if kind == "unknown":
        raise Abort(why)
    ok("nginx upstream：%s" % why)
    if not sock_alive(SYNOSCGI_SOCK):
        raise Abort("連不上 synoscgi")
    if not sock_alive(FILTER_SOCK):
        systemctl("start", FILTER_UNIT)
        wait_filter_ready()
    ok("filter 執行中")
    url = pick_probe(args.probe_url, state)
    if not url:
        raise Abort("DSM 健康檢查失敗")
    changed = nginx_apply(url)
    verify_traffic(url)
    ok(("nginx 已重新套用" if changed else "nginx 原本就是套用狀態") + "，流量確認經過 filter")
    state.update({"dsm_build": build, "photos_version": photos_version(), "probe_url": url, "auto_apply": True})
    save_state(state)


def cmd_reapply(args):
    require_root()
    with locked():
        apply_nginx_now(need_state(), args)
        return 0


def cmd_bypass(args):
    require_root()
    with locked():
        state = need_state()
        if args.state == "on":
            state["auto_apply"] = False
            save_state(state)
            changed = nginx_revert()
            ok("nginx 已直連 synoscgi" + ("" if changed else "（原本就是）") + "；filter 仍在跑但不在路徑上")
            say("恢復：sudo sse bypass off")
        else:
            apply_nginx_now(state, args)
        return 0


def cmd_guard(args):
    """由 systemd 觸發（nginx.conf 被改寫、開機、每 10 分鐘）。條件不安全就什麼都不做。"""
    with locked():
        state = load_state()
        if not state or not state.get("auto_apply", True):
            return 0
        build = dsm_info()[2]
        if build != state.get("dsm_build"):
            say("DSM 已更新（build %s → %s），暫停自動套用；確認正常後執行 sudo sse reapply" %
                (state.get("dsm_build"), build))
            return 0
        try:
            kind, why = nginx_classify(read_text(NGINX_CONF))
        except OSError as e:
            say("讀不到 nginx.conf：%s" % e)
            return 1
        if kind == "managed":
            return 0
        if kind == "unknown":
            say("nginx.conf 格式跟預期不同，不自動套用：%s" % why)
            return 1
        if not sock_alive(FILTER_SOCK):
            say("filter 沒有在跑，不套用")
            return 0
        try:
            nginx_apply(state.get("probe_url"))
            say("nginx.conf 被重新產生過，已重新套用")
            return 0
        except Abort as e:
            say("重新套用失敗（nginx 維持原廠設定）：%s" % e)
            return 1


def cmd_capture(args):
    require_root()
    with locked():
        need_state()
        if args.action == "show":
            try:
                with open(CAPTURE_FILE, encoding="utf-8") as f:
                    sys.stdout.write(f.read())
            except FileNotFoundError:
                say("沒有診斷紀錄")
            return 0
        with open(FILTER_CONFIG, encoding="utf-8") as f:
            fc = json.load(f)
        if args.action == "on":
            fc["capture_file"], fc["capture_max"] = CAPTURE_FILE, args.max
        else:
            fc.pop("capture_file", None)
            fc.pop("capture_max", None)
            remove(CAPTURE_FILE)
        data = (json.dumps(fc, indent=2, ensure_ascii=False) + "\n").encode("utf-8")
        validate_filter_config(data)
        write_atomic(FILTER_CONFIG, data)
        code, out = systemctl("restart", FILTER_UNIT)
        if code != 0:
            raise Abort("重啟 filter 失敗：%s" % out)
        wait_filter_ready()
        if args.action == "on":
            ok("開始記錄搜尋 API（最多 %d 筆，存在 %s，重開機就會消失）" % (args.max, CAPTURE_FILE))
            say("  記錄內容含搜尋字與照片檔名，但不含 cookie、_sid、SynoToken。看完請執行 sudo sse capture off")
        else:
            ok("已停止記錄並刪除紀錄")
        return 0


READONLY = {"status", "doctor", "logs"}


def dispatch(cmd, args, argv):
    if cmd in READONLY or (cmd == "capture" and args.action == "show") or (cmd == "install" and args.dry_run):
        stop_audit()
    elif cmd not in ("install", "update") and (os.geteuid() == 0 or SANDBOX):
        start_audit(" ".join(argv))
    try:
        return globals()["cmd_" + cmd](args)
    except Abort as e:
        bad(str(e))
        return 1
    except KeyboardInterrupt:
        bad("已中斷")
        return 130
