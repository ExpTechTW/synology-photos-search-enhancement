"""sse 指令。

  sudo sse status                     整體狀態與索引進度
  sudo sse update [--check]           更新到最新版
  sudo sse apply [--check]            改完 config.yml 後套用
  sudo sse doctor                     診斷並給建議
  sudo sse logs [filter|engine|indexer|sse] [-n 50] [-f]
  sse search <字> [--explain]         試搜尋、看分數
  sse tag add|rm|list <標籤> [id…]     手動標籤
  sse retry-failed | compact
  sudo sse disable | enable           緊急開關：停止 / 恢復注入
  sudo sse bypass on | off            nginx 直連 synoscgi（排查問題用）
  sudo sse reapply                    DSM 更新後確認沒問題，重新套用
  sudo sse capture on | show | off    錄下搜尋請求（排查用）
  sudo sse uninstall [--purge]

索引與搜尋的指令會以照片擁有者的身分執行：以 root 執行時自動切換，避免資料庫檔案變成 root 擁有。
"""

import argparse
import logging
import os
import sys

from . import __version__
from . import admin, commands, logs
from .config import ConfigError, load

ADMIN = ("install", "update", "apply", "uninstall", "status", "doctor", "logs", "disable", "enable", "bypass",
         "reapply", "capture", "guard")
ENGINE = ("check", "index", "serve", "progress", "search", "tag", "retry-failed", "compact")
# 常駐的指令寫到 log 檔（每天換檔，只留 log.keep_days 天內的）；其他指令照常輸出到終端機
LOG_FILES = {"index": "indexer.log", "serve": "engine.log"}


def parser():
    ap = argparse.ArgumentParser(prog="sse", description="synology-photos-search-enhancement %s" % __version__)
    ap.add_argument("--config", help="config.yml 的路徑（預設：安裝時的資料目錄）")
    ap.add_argument("-v", "--verbose", action="store_true")
    ap.add_argument("--version", action="version", version=__version__)
    sub = ap.add_subparsers(dest="cmd", metavar="<指令>")

    p = sub.add_parser("install", help="安裝或升級（由 install.sh 呼叫）")
    p.add_argument("--user", help="照片擁有者的 DSM 帳號（預設：執行 sudo 的帳號）")
    p.add_argument("--data-dir", help="放 config.yml 與索引的目錄（預設：~帳號/sse）")
    p.add_argument("--from-config", dest="import_config", metavar="FILE", help="用這份 config.yml，不跑設定精靈")
    p.add_argument("--dry-run", action="store_true", help="只檢查，不做任何變更")
    p.add_argument("--yes", action="store_true", help="不詢問直接執行")
    p.add_argument("--probe-url", help="DSM 健康檢查網址（預設自動偵測 5001/5000）")
    p.add_argument("--channel", choices=("release", "pre-release"), help=argparse.SUPPRESS)   # install.sh 傳入
    p.add_argument("--source", help=argparse.SUPPRESS)
    p = sub.add_parser("update", help="更新到最新版（從 GitHub release 下載）")
    p.add_argument("--check", action="store_true", help="只檢查有沒有新版")
    p.add_argument("--force", action="store_true", help="版本相同或較舊也重新安裝")
    p.add_argument("--channel", choices=("release", "pre-release"),
                   help="release（穩定版）或 pre-release（main 分支的每次更新）；之後的更新沿用")
    p.add_argument("--version", dest="want", metavar="TAG", help="安裝指定版本（例如 v2026.10.26+build.1）")
    p = sub.add_parser("apply", help="改完 config.yml 後套用")
    p.add_argument("--check", action="store_true", help="先檢查 DSM 帳號與模型服務")
    p = sub.add_parser("uninstall", help="解除安裝")
    p.add_argument("--purge", action="store_true", help="連資料目錄（索引資料庫、config.yml）一起刪，不詢問")
    p.add_argument("--yes", action="store_true", help="不詢問直接執行（資料目錄保留，除非加 --purge）")
    p.add_argument("--probe-url")
    for name, help_ in (("status", "整體狀態與索引進度"), ("doctor", "診斷並給建議"), ("reapply", "DSM 更新後重新套用")):
        sub.add_parser(name, help=help_).add_argument("--probe-url")
    p = sub.add_parser("logs", help="看 log（每天換檔，只留幾天內的）")
    p.add_argument("source", nargs="?", default="indexer", choices=("filter", "engine", "indexer", "sse"))
    p.add_argument("-n", type=int, default=50, help="最後幾行")
    p.add_argument("-f", "--follow", action="store_true", help="持續顯示新的 log")
    sub.add_parser("disable", help="緊急開關：停止注入")
    sub.add_parser("enable", help="關閉緊急開關")
    p = sub.add_parser("bypass", help="讓 nginx 直連 synoscgi（排查問題用）")
    p.add_argument("state", choices=("on", "off"))
    p.add_argument("--probe-url")
    p = sub.add_parser("capture", help="錄下搜尋請求與回應（排查用）")
    p.add_argument("action", choices=("on", "off", "show"))
    p.add_argument("--max", type=int, default=200)
    sub.add_parser("guard", help="（由 systemd 呼叫）")

    sub.add_parser("check", help="檢查 DSM 帳號、模型服務、資料庫")
    p = sub.add_parser("index", help="（由 systemd 呼叫）索引器")
    p.add_argument("--once", action="store_true", help="跑完一輪就結束")
    p.add_argument("--full", action="store_true", help="搭配 --once：做完整掃描")
    sub.add_parser("serve", help="（由 systemd 呼叫）搜尋服務")
    p = sub.add_parser("progress", help="索引進度")
    p.add_argument("--brief", action="store_true")
    p.add_argument("--json", action="store_true")
    p = sub.add_parser("search", help="試搜尋、看分數")
    p.add_argument("query")
    p.add_argument("--explain", action="store_true")
    p.add_argument("--limit", type=int, default=30)
    p = sub.add_parser("tag", help="手動標籤：tag add 旅行 123 456、tag rm 旅行、tag list")
    p.add_argument("action", choices=("add", "rm", "list"))
    p.add_argument("tag", nargs="?")
    p.add_argument("ids", nargs="*", type=int)
    sub.add_parser("retry-failed", help="把放棄的照片重新排入")
    sub.add_parser("compact", help="壓實資料庫")
    return ap


def engine_main(args, argv):
    path = args.config
    if not path:
        state = admin.load_state()
        if not state or not state.get("data_dir"):
            print("錯誤：還沒安裝，或用 --config 指定 config.yml", file=sys.stderr)
            return 1
        path = admin.config_path(state)
        # root 執行時切換成照片擁有者：資料庫檔案必須由他擁有，root 建立的 -wal、-shm 他會讀寫不了
        if os.geteuid() == 0 and not admin.SANDBOX and state.get("user"):
            code, _ = admin.run_as(state["user"], admin.engine_cmd(path, *argv), capture=False)
            return code
    level = logging.DEBUG if args.verbose else logging.INFO
    logging.basicConfig(stream=sys.stderr, level=level, format="%(levelname)s %(name)s %(message)s")
    try:
        cfg = load(path)
    except ConfigError as e:
        print("錯誤：%s" % e, file=sys.stderr)
        return 1
    # SQLite 的暫存檔（例如壓實時的副本）放在資料庫旁邊：DSM 的 /tmp 是 tmpfs，會吃記憶體。
    # 要在第一次開資料庫之前設定，SQLite 只在初始化時讀一次
    os.environ.setdefault("SQLITE_TMPDIR", os.path.dirname(cfg["index"]["db"]))
    os.makedirs(os.environ["SQLITE_TMPDIR"], exist_ok=True)
    if args.cmd in LOG_FILES:
        err = logs.setup(os.path.join(cfg["log"]["dir"], LOG_FILES[args.cmd]), cfg["log"]["keep_days"], level)
        if err:
            logging.getLogger("sse").error(err)
    from .indexer import OwnerMismatch, ServiceDown
    from .llm import LLMError
    from .synology import SynologyError
    try:
        return getattr(commands, "cmd_" + args.cmd.replace("-", "_"))(cfg, args)
    except (SynologyError, LLMError, OwnerMismatch, ServiceDown) as e:
        print("錯誤：%s" % e, file=sys.stderr)
        return 1


def main(argv=None):
    argv = list(sys.argv[1:] if argv is None else argv)
    ap = parser()
    args = ap.parse_args(argv)
    if not args.cmd:
        ap.print_help()
        return 2
    # 一般帳號看不到系統狀態（/run/sse 只有 root 與 nginx 能讀），只顯示索引進度
    if args.cmd == "status" and os.geteuid() != 0 and not admin.SANDBOX:
        args = ap.parse_args(([] if not args.config else ["--config", args.config]) + ["progress"])
    if args.cmd in ADMIN:
        return admin.dispatch(args.cmd, args, argv)
    # 切換身分時要把 --config 的值一起拿掉，engine_cmd 會重新加上
    if args.config and "--config" in argv:
        i = argv.index("--config")
        argv = argv[:i] + argv[i + 2:]
    return engine_main(args, argv)
