#!/usr/bin/env python3
# filter 的單元測試：解析、注入、時間軸分段、設定、log。用 DSM 內建的 Python 3.8 跑，確認相容。
#   python3.8 -m unittest tests.test_filter -v
import importlib.util
import json
import os
import tempfile
import unittest

REPO = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))


def load(name, filename):
    spec = importlib.util.spec_from_file_location(name, os.path.join(REPO, "sse", "data", filename))
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


flt = load("sse_filter", "sse-filter.py")

def scgi(headers, body=b""):
    h = b"CONTENT_LENGTH\0" + str(len(body)).encode() + b"\0"
    for k, v in headers.items():
        h += k + b"\0" + v + b"\0"
    return str(len(h)).encode() + b":" + h + b"," + body


SEARCH = b"api=SYNO.Foto.Search.Search&version=7&method=list_item&offset=0&limit=100&keyword=%22test%22"
FORM = {b"REQUEST_METHOD": b"POST", b"REQUEST_URI": b"/webapi/entry.cgi",
        b"CONTENT_TYPE": b"application/x-www-form-urlencoded; charset=UTF-8"}


def sniff(raw, step=None):
    s = flt.Sniffer(262144, 65536)
    if step:
        for i in range(0, len(raw), step):
            s.feed(raw[i:i + step])
    else:
        s.feed(raw)
    return s


class Sniffer(unittest.TestCase):
    def test_search_plan_even_when_fed_byte_by_byte(self):
        for step in (None, 1, 7):
            p = sniff(scgi(FORM, SEARCH), step).plan()
            self.assertEqual((p.method, p.keyword), ("list_item", "test"))

    def test_query_string_and_path_info(self):
        h = {b"REQUEST_METHOD": b"GET", b"REQUEST_URI": b"/webapi/entry.cgi/SYNO.Foto.Search.Search?" + SEARCH,
             b"QUERY_STRING": SEARCH}
        self.assertEqual(sniff(scgi(h)).plan().method, "list_item")

    def test_not_inspected(self):
        cases = [
            scgi({**FORM, b"REQUEST_URI": b"/webapi/other.cgi"}, SEARCH),
            scgi({**FORM, b"CONTENT_TYPE": b"multipart/form-data; boundary=x"}, SEARCH),
            scgi(FORM, SEARCH.replace(b"SYNO.Foto.Search", b"SYNO.FotoTeam.Search")),
            scgi(FORM, SEARCH.replace(b"list_item", b"suggest")),
            scgi(FORM, SEARCH.replace(b"%22test%22", b"")),
            scgi(FORM, b"x" * 70000),
            b"garbage-without-netstring",
            b"5:abc,",
        ]
        for raw in cases:
            self.assertIsNone(sniff(raw).plan(), raw[:60])

    def test_skip_reasons(self):
        self.assertEqual(sniff(scgi({**FORM, b"CONTENT_TYPE": b"application/json"}, b'{"api":1}')).skip_reason,
                         "Content-Type application/json")
        self.assertEqual(sniff(scgi({**FORM, b"REQUEST_URI": b"/x.cgi"}, SEARCH)).skip_reason, "不是 entry.cgi")
        self.assertEqual(sniff(scgi(FORM, b"x" * 70000)).skip_reason, "body 太大（70000 bytes）")

    def test_keyword_normalization(self):
        self.assertEqual(flt.normalize_keyword(' "Test" '), "Test")
        self.assertEqual(flt.normalize_keyword('"\\u8c93"'), "貓")
        self.assertEqual(flt.normalize_keyword("'dog'"), "dog")
        self.assertEqual(flt.normalize_keyword('"'), '"')


ITEM = {"id": 90001, "owner_user_id": 1, "time": 1750000000, "type": "photo"}


class Inject(unittest.TestCase):
    def resp(self, data):
        return {"data": data, "success": True}

    def test_list_item_dedupe_and_first_page_only(self):
        d = flt.inject_response("list_item", {}, self.resp({"list": [{"id": 1}]}), [ITEM])
        self.assertEqual([x["id"] for x in d["data"]["list"]], [90001, 1])
        self.assertIsNone(flt.inject_response("list_item", {}, self.resp({"list": [{"id": 90001}]}), [ITEM]))
        self.assertIsNone(flt.inject_response("list_item", {"offset": "100"}, self.resp({"list": []}), [ITEM]))

    def test_section_format(self):
        d = flt.inject_response("get_search_timeline", {}, self.resp({"section": []}), [ITEM])
        self.assertEqual(json.dumps(d["data"]["section"][0], separators=(",", ":")),
                         '{"limit":100,"list":[{"day":15,"item_count":1,"month":6,"year":2025}],"offset":0}')

    def test_count_and_basic(self):
        self.assertEqual(flt.inject_response("count_item", {}, self.resp({"count": 4}), [ITEM])["data"]["count"], 5)
        d = flt.inject_response("list_basic_timeline_info", {}, self.resp({"list": []}), [ITEM])
        self.assertEqual(d["data"]["list"], [{"id": 90001, "owner_user_id": 1, "time": 1750000000}])

    def test_leaves_errors_alone(self):
        self.assertIsNone(flt.inject_response("list_item", {}, {"success": False, "error": {"code": 120}}, [ITEM]))
        self.assertIsNone(flt.inject_response("count_item", {}, self.resp({"count": "4"}), [ITEM]))
        self.assertIsNone(flt.inject_response("list_item", {}, self.resp({"list": "x"}), [ITEM]))

    def test_cgi_response_roundtrip(self):
        body = b'{"data":{"count":1},"success":true}'
        raw = b"Status: 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s" % (len(body), body)
        lines, data = flt.parse_cgi_response(raw)
        out = flt.build_cgi_response(lines, flt.inject_response("count_item", {}, data, [ITEM]))
        head, new = out.split(b"\r\n\r\n", 1)
        self.assertEqual(json.loads(new)["data"]["count"], 2)
        self.assertIn(b"Content-Length: %d" % len(new), head)   # 長度跟著改
        self.assertIsNone(flt.parse_cgi_response(raw.replace(b"200 OK", b"500 Error")))
        self.assertIsNone(flt.parse_cgi_response(raw.replace(b"Content-Type", b"Content-Encoding: gzip\r\nX")))
        self.assertIsNone(flt.parse_cgi_response(raw.replace(b'"success":true}', b'"success":tr')))
        self.assertIsNone(flt.parse_cgi_response(b"no header separator"))


DAY = 86400


def items_by_day(counts, t0=1700000000 + 400 * DAY):
    """依序產生新到舊的照片：counts[i] 張在第 i 天（往回推）。"""
    out, uid = [], 0
    for i, c in enumerate(counts):
        for _ in range(c):
            uid += 1
            out.append({"id": uid, "owner_user_id": 1, "time": t0 - i * DAY})
    return out


class Sections(unittest.TestCase):
    """規則由 Photos 1.9.0 的原生回應反推，並在真實 NAS 上用 30 個關鍵字 × day/month 逐筆比對過。"""

    def shape(self, secs):
        return [(s["limit"], s["offset"], [e["item_count"] for e in s["list"]]) for s in secs]

    def test_accumulate_until_over_100_and_merge_tail(self):
        self.assertEqual(self.shape(flt.build_sections(items_by_day([60, 50, 30]), "day")), [(140, 0, [60, 50, 30])])
        self.assertEqual(self.shape(flt.build_sections(items_by_day([60, 40, 30]), "day")), [(130, 0, [60, 40, 30])])
        self.assertEqual(self.shape(flt.build_sections(items_by_day([5]), "day")), [(5, 0, [5])])
        self.assertEqual(flt.build_sections([], "day"), [])

    def test_big_group_split_evenly(self):
        # 42 張還沒滿 → 前面沒有一般段可併，自成一段；350 張的那天拆成 3 段；最後 20 張前一段是大組，自成一段
        got = self.shape(flt.build_sections(items_by_day([20, 22, 350, 20]), "day"))
        self.assertEqual(got, [(42, 0, [20, 22]), (117, 0, [350]), (117, 117, [350]), (116, 234, [350]), (20, 0, [20])])

    def test_pending_merges_into_previous_normal_before_big_group(self):
        got = self.shape(flt.build_sections(items_by_day([60, 50, 40, 200]), "day"))
        self.assertEqual(got, [(150, 0, [60, 50, 40]), (100, 0, [200]), (100, 100, [200])])

    def test_month_unit_and_utc(self):
        secs = flt.build_sections([{"id": 1, "owner_user_id": 1, "time": 1704067199},   # 2023-12-31 23:59:59 UTC
                                   {"id": 2, "owner_user_id": 1, "time": 1704067200}], "month")  # 2024-01-01 UTC
        self.assertEqual(secs, [{"limit": 2, "offset": 0, "list": [{"item_count": 1, "month": 12, "year": 2023},
                                                                   {"item_count": 1, "month": 1, "year": 2024}]}])

    def test_merge_keeps_native_tie_order(self):
        native = [{"id": 5, "owner_user_id": 1, "time": 100}, {"id": 9, "owner_user_id": 1, "time": 100},
                  {"id": 3, "owner_user_id": 1, "time": 50}]
        ours = [{"id": 7, "owner_user_id": 1, "time": 100}, {"id": 3, "owner_user_id": 1, "time": 50}]
        self.assertEqual([x["id"] for x in flt.merge_basic(native, ours)], [5, 9, 7, 3])


def load_cfg(data):
    with tempfile.NamedTemporaryFile("w", suffix=".json", delete=False) as f:
        f.write(data if isinstance(data, str) else json.dumps(data))
    try:
        return flt.load_config(f.name)
    finally:
        os.unlink(f.name)


class Config(unittest.TestCase):
    def test_bad_configs_fall_back_to_passthrough(self):
        demo = {"keyword": "test", "items": [ITEM]}
        for data in ("{not json", {"mode": "inject"}, {"mode": "demo"}, {"typo": 1},
                     {"mode": "demo", "demo": {"keyword": "", "items": [ITEM]}},
                     {"mode": "demo", "demo": {"keyword": "x", "items": [{"id": "1"}]}}):
            cfg, err = load_cfg(data)
            self.assertEqual(cfg["mode"], "passthrough", data)
            self.assertTrue(err, data)
        cfg, err = load_cfg({"mode": "demo", "demo": demo})
        self.assertEqual((cfg["mode"], err), ("demo", None))


class Capture(unittest.TestCase):
    def test_large_response_stays_valid_json(self):
        d = tempfile.mkdtemp()
        try:
            path = os.path.join(d, "capture.jsonl")
            f = flt.Filter(dict(flt.DEFAULTS, capture_file=path))
            big = {"success": True, "data": {"list": [{"id": i, "filename": "x" * 300} for i in range(2000)]}}
            f.capture("native", {"api": "SYNO.Foto.Search.Search", "_sid": "secret"}, big)
            f.capture("native", {"api": "SYNO.Foto.Search.Search"}, {"success": True, "data": {"count": 3}})
            with open(path) as fh:
                recs = [json.loads(line) for line in fh]          # 每一行都是完整的 JSON
            self.assertEqual(recs[0]["response"]["summary"]["count"], 2000)
            self.assertEqual(recs[0]["response"]["summary"]["ids"][:3], [0, 1, 2])
            self.assertNotIn("_sid", recs[0]["params"])
            self.assertEqual(recs[1]["response"]["data"]["count"], 3)
        finally:
            import shutil
            shutil.rmtree(d)


class Logs(unittest.TestCase):
    def test_prune_keeps_only_recent_days(self):
        import datetime
        d = tempfile.mkdtemp()
        try:
            base = os.path.join(d, "filter.log")
            today = datetime.date(2026, 10, 26)
            for name in ("filter.log", "filter.log.2026-10-25", "filter.log.2026-10-24", "filter.log.2026-10-23",
                         "filter.log.2026-10-01", "other.log.2026-10-01", "filter.log.junk"):
                open(os.path.join(d, name), "w").close()
            os.utime(os.path.join(d, "filter.log.junk"), (0, 0))
            flt.prune_logs(base, 3, today=today)
            # 只留今天與前 2 天：最舊的紀錄不超過 3 天
            self.assertEqual(sorted(os.listdir(d)), ["filter.log", "filter.log.2026-10-24", "filter.log.2026-10-25",
                                                     "other.log.2026-10-01"])
        finally:
            import shutil
            shutil.rmtree(d)

    def test_file_logging_and_bad_config(self):
        import logging
        d = tempfile.mkdtemp()
        try:
            lg = logging.getLogger("sse-test-logs")
            self.assertIsNone(flt.setup_file_logging(lg, os.path.join(d, "x", "f.log"), 3))
            lg.setLevel(logging.INFO)
            lg.info("hello")
            lg.handlers[0].flush()
            with open(os.path.join(d, "x", "f.log")) as f:
                self.assertIn("hello", f.read())
            self.assertTrue(flt.setup_file_logging(lg, "/proc/nope/f.log", 3))   # 寫不了：回傳錯誤、保留原本的輸出
        finally:
            import shutil
            shutil.rmtree(d)
        for bad in ({"log_keep_days": 0}, {"log_keep_days": "3"}, {"log_file": 5}):
            cfg, err = load_cfg(bad)
            self.assertTrue(err, bad)
        cfg, err = load_cfg({"log_file": "/var/log/sse/filter.log", "log_keep_days": 3})
        self.assertIsNone(err)


if __name__ == "__main__":
    unittest.main()
