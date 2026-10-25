"""索引與搜尋相關的指令：都以照片擁有者的身分執行（sse 以 root 執行時會自動切換身分）。

  check                        設定、DSM 登入、模型服務、資料庫逐項檢查
  index [--once] [--full]      索引器（systemd 的 sse-indexer）
  serve                        搜尋服務（給 sse-filter；systemd 的 sse-engine）
  progress [--brief | --json]  索引進度（sudo sse status 會一併顯示）
  search <字> [--explain]      試搜尋、看分數
  tag add|rm|list <標籤> [id…]  手動標籤（只存在外掛資料庫）
  retry-failed                 把放棄的照片重新排入
  compact                      壓實資料庫
"""

import asyncio
import json
import logging
import os
import signal
import sys
import threading
import time

from . import __version__
from . import db as dbm
from .indexer import PROBE_PNG, Indexer, OwnerMismatch, compact_if_worthwhile
from .llm import LLM
from .search import HUB_BANK, QUERY_BANK_PREFIX, Index
from .synology import Client, parse_item


def make_llm(cfg):
    return LLM.from_config(cfg)


def make_dsm(cfg):
    c = cfg["synology"]
    return Client(c["url"], c["account"], c["password"], c["verify_tls"], c["timeout"])


def cmd_check(cfg, args):
    ok = True

    def step(name, fn):
        nonlocal ok
        try:
            msg = fn()
            print("  ✓ %s%s" % (name, "：" + msg if msg else ""))
            return True
        except Exception as e:
            ok = False
            print("  ✗ %s：%s" % (name, e))
            return False

    print("sse %s 檢查（%s）：" % (__version__, cfg["path"]))
    dsm = [None]

    def dsm_login():
        dsm[0] = make_dsm(cfg)
        dsm[0].login()
        uid, name = dsm[0].me()
        conn = dbm.connect(cfg["index"]["db"])
        stored = dbm.get_meta(conn, "owner_user_id")
        if stored is not None and int(stored) != uid:
            raise OwnerMismatch("資料庫屬於使用者 id %s，目前登入的是 %s（id %s）" % (stored, name, uid))
        return "%s（id %s）" % (name, uid)

    if step("DSM 登入（%s，帳號 %s）" % (cfg["synology"]["url"], cfg["synology"]["account"]), dsm_login):
        def thumb():
            items = [x for x in (parse_item(r) for r in dsm[0].list_items(0, 5)) if x]
            if not items:
                return "還沒有照片，略過"
            out = []
            for k in ("thumb_size", "ocr_thumb_size"):
                data, _ = dsm[0].thumbnail(items[0]["unit_id"], items[0]["cache_key"], cfg["synology"][k])
                out.append("%s %d KB" % (cfg["synology"][k], len(data) // 1024))
            return "照片 %s 張，縮圖 %s" % ("{:,}".format(dsm[0].count_items()), "、".join(out))
        step("讀取照片與縮圖", thumb)
        dsm[0].logout()

    llm = make_llm(cfg)
    c = cfg["llm"]
    step("文字 embedding（%s %s）" % (c["embed_base_url"], c["embed_model"]),
         lambda: "%d 維" % len(llm.embed_query("測試")))
    step("圖片 embedding", lambda: "%d 維" % len(llm.embed_image(PROBE_PNG)))
    if cfg["index"]["ocr"] != "off":
        step("OCR（%s %s）" % (c["ocr_base_url"], c["ocr_model"]), lambda: "回應 %r" % llm.ocr(PROBE_PNG, "image/png")[:40])

    def dbcheck():
        conn = dbm.connect(cfg["index"]["db"])
        conn.execute("SELECT count(*) FROM ocr_fts WHERE ocr_fts MATCH ?", ('"abc"',)).fetchone()
        return cfg["index"]["db"]
    step("資料庫", dbcheck)
    return 0 if ok else 1


def cmd_index(cfg, args):
    conn = dbm.connect(cfg["index"]["db"])
    ix = Indexer(cfg, make_dsm(cfg), make_llm(cfg), conn)
    try:
        if args.once:
            ix.run_once(full=args.full or dbm.get_meta(conn, "last_full_scan") is None)
            print(json.dumps(dbm.stats(conn), ensure_ascii=False))
            return 0
        stop = threading.Event()
        for sig in (signal.SIGTERM, signal.SIGINT):
            signal.signal(sig, lambda *_: stop.set())
        ix.loop(stop)
        return 0
    except OwnerMismatch as e:
        logging.error("%s", e)
        return 2   # systemd 不重啟（RestartPreventExitStatus=2）
    finally:
        ix.dsm.logout()


def cmd_serve(cfg, args):
    from .server import Server
    asyncio.run(Server(cfg, make_llm(cfg)).serve())
    return 0


def cmd_tag(cfg, args):
    """手動標籤：只寫外掛資料庫。搜尋那個字時，標了的照片會出現在原生搜尋結果裡。"""
    conn = dbm.connect(cfg["index"]["db"])
    tag = (args.tag or "").strip().casefold()
    if args.action == "list":
        rows = conn.execute("SELECT tag, unit_id FROM tag" + (" WHERE tag=?" if tag else "") + " ORDER BY tag, unit_id",
                            (tag,) if tag else ()).fetchall()
        for t, uid in rows:
            print("%s\t%s" % (t, uid))
        if not rows:
            print("沒有標籤")
        return 0
    if not tag:
        print("錯誤：要指定標籤", file=sys.stderr)
        return 1
    if args.action == "add":
        if not args.ids:
            print("錯誤：要指定照片 id（sse search 會列出照片 id）", file=sys.stderr)
            return 1
        known = {r[0] for r in conn.execute("SELECT unit_id FROM item WHERE deleted=0 AND unit_id IN (%s)"
                                            % ",".join("?" * len(args.ids)), args.ids)}
        unknown = [i for i in args.ids if i not in known]
        if unknown:
            print("錯誤：索引裡沒有這些照片（id 打錯，或還沒掃描到）：%s" % unknown, file=sys.stderr)
            return 1
        with conn:
            conn.executemany("INSERT OR IGNORE INTO tag(tag, unit_id) VALUES (?,?)", [(tag, i) for i in args.ids])
        print("已標上「%s」：%s" % (tag, args.ids))
    else:
        with conn:
            if args.ids:
                n = conn.executemany("DELETE FROM tag WHERE tag=? AND unit_id=?", [(tag, i) for i in args.ids]).rowcount
            else:
                n = conn.execute("DELETE FROM tag WHERE tag=?", (tag,)).rowcount
        print("已移除 %d 筆「%s」" % (n, tag))
    return 0


def cmd_compact(cfg, args):
    """手動壓實（索引器在跑也可以用：WAL 模式下讀取不受影響，寫入會等壓實做完）。"""
    conn = dbm.connect(cfg["index"]["db"])
    size, wasted = dbm.space_stats(conn)
    print("主檔 %.1f MB，可回收約 %.1f MB" % (size / 1e6, wasted / 1e6))
    r = compact_if_worthwhile(conn, cfg["index"]["db"], force=True)
    if r:
        print("硬碟用量：%.1f MB → %.1f MB" % (r[0] / 1e6, r[1] / 1e6))
    return 0


def cmd_retry_failed(cfg, args):
    print("重新排入：embedding %d、OCR %d" % dbm.requeue_failed(dbm.connect(cfg["index"]["db"])))
    return 0


def cmd_search(cfg, args):
    if not os.path.exists(cfg["index"]["db"]):
        print("還沒有索引資料庫：索引器還沒跑過")
        return 1
    idx = Index(cfg["index"]["db"], cfg["search"]["dims"])
    idx.refresh()
    llm = make_llm(cfg)
    idx.set_bank([llm._embed(QUERY_BANK_PREFIX + q) for q in HUB_BANK])
    t = time.monotonic()
    qv = llm.embed_query(args.query)
    hits = idx.search(qv, args.query, cfg["search"])
    print("「%s」：%d 張（%d ms，索引 %d 張）" % (args.query, len(hits), (time.monotonic() - t) * 1000, idx.ids.size))
    labels = {"vec": "圖片", "text": "照片文字", "ocr": "文字逐字", "tag": "標籤"}
    for h in hits[:args.limit]:
        print("  %-10s %s  %-6s %.3f %s" % (h["id"], time.strftime("%Y-%m-%d", time.gmtime(h["time"])), h["type"],
                                            h["score"], labels.get(h["src"], h["src"])))
    if args.explain:
        sc = cfg["search"]
        top, (mean, std) = idx.explain(qv)
        print("\n圖片：分數最高的 %d 張（hub 校正後；平均 %.3f、標準差 %.3f；門檻 min_score=%.2f max_drop=%.2f）："
              % (len(top), mean, std, sc["min_score"], sc["max_drop"]))
        for uid, s in top:
            print("  %-10s %.3f" % (uid, s))
        top, (mean, std) = idx.explain(qv, text=True)
        if top:
            texts = dict(idx.conn().execute("SELECT unit_id, text FROM ocr WHERE unit_id IN (%s)"
                                            % ",".join(str(u) for u, _ in top)).fetchall())
            print("\n照片裡的文字：分數最高的 %d 張（平均 %.3f、標準差 %.3f；門檻 text_min_score=%.2f）："
                  % (len(top), mean, std, sc["text_min_score"]))
            for uid, s in top:
                print("  %-10s %.3f  %s" % (uid, s, (texts.get(uid) or "").replace("\n", " / ")[:60]))
    return 0


# ── 索引進度 ────────────────────────────────────────────────────────────

def fmt_n(n):
    return "{:,}".format(n)


def fmt_dur(sec):
    sec = int(sec)
    if sec < 60:
        return "不到 1 分鐘"
    if sec < 3600:
        return "%d 分鐘" % round(sec / 60)
    h, m = divmod(round(sec / 60), 60)
    return "%d 小時 %d 分" % (h, m) if h < 48 else "%.1f 天" % (sec / 86400)


def fmt_time(ts):
    if not ts:
        return "—"
    t = time.localtime(ts)
    return time.strftime("%H:%M" if t[:3] == time.localtime()[:3] else "%m-%d %H:%M", t)


def bar(frac, width=20):
    n = max(0, min(width, int(round(frac * width))))
    return "█" * n + "░" * (width - n)


def indexer_alive(prog):
    """進度檔裡的 pid 是不是還在跑的索引器（避免 pid 被重複使用時誤判）。"""
    pid = (prog or {}).get("pid")
    if not pid:
        return False
    try:
        os.kill(pid, 0)
    except PermissionError:
        return True
    except OSError:
        return False
    try:
        with open("/proc/%d/cmdline" % pid, "rb") as f:
            return b"\0sse\0" in f.read()
    except OSError:
        return True  # 沒有 /proc（macOS 測試）


def progress_report(cfg):
    db = cfg["index"]["db"]
    conn = dbm.connect(db, readonly=True)
    st = dbm.stats(conn)
    st["db_bytes"] = dbm.file_bytes(db)
    st["last_compact"] = int(dbm.get_meta(conn, "last_compact", 0)) or None
    st["last_full_scan"] = int(dbm.get_meta(conn, "last_full_scan", 0)) or None
    st["last_quick_scan"] = int(dbm.get_meta(conn, "last_quick_scan", 0)) or None
    st["last_full_scan_result"] = json.loads(dbm.get_meta(conn, "last_full_scan_result", "null"))
    st["ocr_enabled"] = cfg["index"]["ocr"] != "off"
    try:
        with open(os.path.join(os.path.dirname(db), "indexer-status.json"), encoding="utf-8") as f:
            prog = json.load(f)
    except (OSError, ValueError):
        prog = None
    st["indexer"] = prog
    st["indexer_running"] = indexer_alive(prog)
    total = st["items"]

    def column(done, pending, failed, key):
        rate = ((prog or {}).get("rate") or {}).get(key) if st["indexer_running"] else None
        return {"done": done, "pending": pending, "failed": failed, "total": total,
                "percent": round(100.0 * (done + failed) / total, 1) if total else 100.0,
                "rate": rate, "eta_seconds": int(pending / rate) if rate and pending else None}

    st["embedding"] = column(st["embedded"], st["embed_pending"], st["embed_failed"], "emb")
    st["ocr"] = column(st["ocr_done"], st["ocr_pending"], st["ocr_failed"], "ocr")
    return st


PHASES = {"starting": "啟動中", "full_scan": "完整掃描", "quick_scan": "快速掃描", "embedding": "embedding",
          "ocr": "OCR", "text_embedding": "照片文字向量", "idle": "全部做完，等待新照片", "compacting": "壓實資料庫",
          "paused": "暫停", "stopped": "已停止", "done": "完成", "error": "錯誤"}


def describe_state(st):
    prog = st["indexer"] or {}
    if not st["indexer_running"]:
        if prog.get("phase") == "error":
            return "沒有在跑：%s" % prog.get("pause_reason")
        return "沒有在跑"
    phase = prog.get("phase")
    if phase == "paused":
        return "暫停中，%s 再試（%s）" % (fmt_time(prog.get("paused_until")), prog.get("pause_reason"))
    if phase == "full_scan" and prog.get("scan_total"):
        return "完整掃描中 %s / %s" % (fmt_n(prog.get("scan_done") or 0), fmt_n(prog["scan_total"]))
    return "%s（%s 開始）" % (PHASES.get(phase, phase), fmt_time(prog.get("phase_since")))


def describe_column(c, waiting=None):
    if not c["pending"]:
        return "完成"
    if waiting:
        return waiting
    if c["eta_seconds"]:
        eta = fmt_dur(c["eta_seconds"])
        return "%.1f 張/秒，%s" % (c["rate"], eta if eta.startswith("不到") else "約 " + eta)
    return "計算速度中"


def cmd_progress(cfg, args):
    if not os.path.exists(cfg["index"]["db"]):
        print("還沒有索引資料庫：索引器還沒跑過")
        return 0
    st = progress_report(cfg)
    if args.json:
        print(json.dumps(st, ensure_ascii=False, indent=2))
        return 0
    e, o = st["embedding"], st["ocr"]
    ocr_wait = "embedding 做完後開始" if e["pending"] else None
    if args.brief:
        line = "embedding %s / %s（%s%%，%s）" % (fmt_n(e["done"]), fmt_n(e["total"]), e["percent"], describe_column(e))
        if st["ocr_enabled"]:
            line += "；OCR %s / %s（%s%%，%s）" % (fmt_n(o["done"]), fmt_n(o["total"]), o["percent"],
                                                describe_column(o, ocr_wait))
        print(line)
        print("索引器：" + describe_state(st))
        return 0
    prog = st["indexer"] or {}
    print("索引進度：%s 張照片%s" % (fmt_n(st["items"]), "（另有 %s 張已刪除）" % fmt_n(st["deleted"]) if st["deleted"] else ""))
    print("  索引器    %s" % describe_state(st))
    print("  embedding %s %6s / %s（%5.1f%%）  %s" % (bar(e["percent"] / 100), fmt_n(e["done"]), fmt_n(e["total"]),
                                                  e["percent"], describe_column(e)))
    if st["ocr_enabled"]:
        print("  OCR       %s %6s / %s（%5.1f%%）  %s" % (bar(o["percent"] / 100), fmt_n(o["done"]), fmt_n(o["total"]),
                                                      o["percent"], describe_column(o, ocr_wait)))
        tv, wt = st["text_embedded"], st["with_text"]
        print("  照片裡有文字：%s 張，文字向量 %s 張%s" % (fmt_n(wt), fmt_n(tv), "" if tv >= wt else "（做完一批 OCR 就跟著做）"))
    else:
        print("  OCR       關閉")
    if e["failed"] or o["failed"]:
        print("  失敗：embedding %d、OCR %d（各試 %d 次都沒成功；sse retry-failed 重新排入）"
              % (e["failed"], o["failed"], cfg["index"]["max_tries"]))
    r = st["last_full_scan_result"]
    print("  完整掃描  上次 %s%s，下次約 %s" % (
        fmt_time(st["last_full_scan"]),
        "（%s 張、新增或變更 %s、刪除 %s，花 %s）" % (fmt_n(r["seen"]), fmt_n(r["changed"]), fmt_n(r["removed"]),
                                             fmt_dur(r["seconds"])) if r else "",
        fmt_time(prog.get("next_full_scan")) if st["indexer_running"] else "—"))
    print("  快速掃描  上次 %s，下次約 %s" % (fmt_time(st["last_quick_scan"]),
                                       fmt_time(prog.get("next_quick_scan")) if st["indexer_running"] else "—"))
    print("  資料庫    %.1f MB（%s；上次壓實 %s）" % (st["db_bytes"] / 1e6, cfg["index"]["db"], fmt_time(st["last_compact"])))
    return 0
