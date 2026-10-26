#!/usr/bin/env python3
# 測試用的假 systemctl：只實作 sse 用到的子指令，讀 $SSE_ROOT/etc/systemd/system 裡的 unit。
#   - Type=oneshot 的 service：同步執行 ExecStart
#   - 其他 service：背景執行 ExecStart，記下 pid；也模擬 RuntimeDirectory 的建立與移除
#   - path / timer：只記錄啟用狀態，不會真的觸發
import json
import os
import shlex
import shutil
import signal
import subprocess
import sys
import time

ROOT = os.environ["SSE_ROOT"]
UNITS = os.path.join(ROOT, "etc/systemd/system")
STATE = os.path.join(ROOT, "var/fake-systemd.json")


def load():
    try:
        with open(STATE) as f:
            return json.load(f)
    except FileNotFoundError:
        return {"enabled": [], "pids": {}, "active": []}


def save(st):
    os.makedirs(os.path.dirname(STATE), exist_ok=True)
    with open(STATE, "w") as f:
        json.dump(st, f)


def unit_conf(name):
    conf = {}
    with open(os.path.join(UNITS, name)) as f:
        for line in f:
            k, sep, v = line.strip().partition("=")
            if sep and not line.startswith("#"):
                conf[k] = v
    return conf


def alive(pid):
    try:
        os.kill(pid, 0)
        return True
    except OSError:
        return False


def start(st, name):
    if not os.path.exists(os.path.join(UNITS, name)):
        print("Unit %s not found." % name, file=sys.stderr)
        return 5
    if not name.endswith(".service"):
        if name not in st["active"]:
            st["active"].append(name)
        return 0
    conf = unit_conf(name)
    cmd = shlex.split(conf["ExecStart"])
    if conf.get("Type") == "oneshot":
        return subprocess.run(cmd).returncode
    pid = st["pids"].get(name)
    if pid and alive(pid):
        return 0
    if conf.get("RuntimeDirectory"):
        os.makedirs(os.path.join(ROOT, "run", conf["RuntimeDirectory"]), exist_ok=True)
    log = open(os.path.join(ROOT, "var", name + ".log"), "a")
    p = subprocess.Popen(cmd, stdout=log, stderr=log, stdin=subprocess.DEVNULL, start_new_session=True)
    st["pids"][name] = p.pid
    return 0


def parse_duration(raw):
    """systemd 的時間：純數字是秒（20），也可以是 20s、1min。看不懂就用預設 90 秒。"""
    s = (raw or "").strip().lower()
    i = 0
    while i < len(s) and (s[i].isdigit() or s[i] == "."):
        i += 1
    if i == 0:
        return 90.0
    try:
        n = float(s[:i])
    except ValueError:
        return 90.0
    mult = {"": 1, "s": 1, "us": 1e-6, "ms": 1e-3, "m": 60, "min": 60, "h": 3600}.get(s[i:].strip())
    if mult is None:
        return 90.0
    return n * mult


def _dead(pid):
    try:
        got, _ = os.waitpid(pid, os.WNOHANG)
        if got:
            return True
    except ChildProcessError:
        return not alive(pid)
    return not alive(pid)


def _wait_dead(pid, timeout):
    """等 pid 結束。timeout 為 None 時一直等到結束。回傳是否已結束。"""
    end = None if timeout is None else time.monotonic() + timeout
    while not _dead(pid):
        if end is not None and time.monotonic() >= end:
            return False
        time.sleep(0.05)
    return True


def stop(st, name):
    if name in st["active"]:
        st["active"].remove(name)
    pid = st["pids"].pop(name, None)
    if pid and alive(pid):
        # 跟 systemd 一樣：先 SIGTERM，過了 TimeoutStopSec（沒寫是 90 秒）再 SIGKILL。
        limit = 90.0
        unit = os.path.join(UNITS, name)
        if os.path.exists(unit):
            limit = parse_duration(unit_conf(name).get("TimeoutStopSec", "90"))
        if os.environ.get("FAKE_STOP_TIMEOUT"):   # 測試結束時清理用：不必等
            limit = float(os.environ["FAKE_STOP_TIMEOUT"])
        try:
            os.kill(pid, signal.SIGTERM)
        except OSError:
            pass
        if not _wait_dead(pid, limit):
            try:
                os.kill(pid, signal.SIGKILL)
            except OSError:
                pass
            _wait_dead(pid, None)
    if os.path.exists(os.path.join(UNITS, name)):
        rd = unit_conf(name).get("RuntimeDirectory")
        if rd:
            shutil.rmtree(os.path.join(ROOT, "run", rd), ignore_errors=True)
    return 0


def main(argv):
    st = load()
    if argv[:1] == ["--version"]:
        print("systemd 219 (fake)")
        return 0
    if "--now" in argv:  # 跟 DSM 7.3 一樣是 systemd 219，還沒有 --now
        print("systemctl: unrecognized option '--now'", file=sys.stderr)
        return 1
    now = False
    cmd, names = argv[0], argv[1:]
    rc = 0
    if cmd in ("daemon-reload", "reset-failed"):
        pass
    elif cmd == "enable":
        for n in names:
            if not os.path.exists(os.path.join(UNITS, n)):
                print("Unit %s not found." % n, file=sys.stderr)
                rc = 1
                continue
            if n not in st["enabled"]:
                st["enabled"].append(n)
            if now:
                rc = start(st, n) or rc
    elif cmd == "disable":
        for n in names:
            if n in st["enabled"]:
                st["enabled"].remove(n)
            if now:
                stop(st, n)
    elif cmd == "start":
        for n in names:
            rc = start(st, n) or rc
    elif cmd == "stop":
        for n in names:
            stop(st, n)
    elif cmd == "restart":
        for n in names:
            stop(st, n)
            rc = start(st, n) or rc
    elif cmd == "is-active":
        n = names[0]
        on = (st["pids"].get(n) and alive(st["pids"][n])) or n in st["active"]
        print("active" if on else "inactive")
        rc = 0 if on else 3
    elif cmd == "is-enabled":
        on = names[0] in st["enabled"]
        print("enabled" if on else "disabled")
        rc = 0 if on else 1
    else:
        print("fake systemctl: 不支援 %s" % cmd, file=sys.stderr)
        rc = 1
    save(st)
    return rc


sys.exit(main(sys.argv[1:]))
