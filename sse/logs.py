"""log 檔：每天午夜換新檔，只留 keep_days 天內的紀錄（與 DSM 端 sse-filter 的做法相同）。"""

import datetime
import logging
import logging.handlers
import os
import time


def prune_logs(path, keep_days, today=None):
    """刪掉 keep_days 天以前的舊檔（檔名 <path>.YYYY-MM-DD）。
    程式停了好幾天才重啟時不會有滾動，所以啟動時也要自己清一次。"""
    d, base = os.path.split(path)
    today = today or datetime.date.today()
    oldest = today - datetime.timedelta(days=keep_days - 1)
    try:
        names = os.listdir(d)
    except OSError:
        return
    for name in names:
        if not name.startswith(base + "."):
            continue
        p = os.path.join(d, name)
        try:
            old = datetime.datetime.strptime(name[len(base) + 1:], "%Y-%m-%d").date() < oldest
        except ValueError:
            try:
                old = os.path.getmtime(p) < time.time() - keep_days * 86400
            except OSError:
                continue
        if old:
            try:
                os.unlink(p)
            except OSError:
                pass


class DailyLog(logging.handlers.TimedRotatingFileHandler):
    """每天午夜換新檔，只留今天與前 keep_days - 1 天：最舊的紀錄不會超過 keep_days 天。"""

    def __init__(self, path, keep_days):
        super().__init__(path, when="midnight", backupCount=0, encoding="utf-8")
        self.keep_days = keep_days
        prune_logs(path, keep_days)

    def doRollover(self):
        super().doRollover()
        prune_logs(self.baseFilename, self.keep_days)


def setup(path, keep_days, level=logging.INFO):
    """root logger 改寫到 path（失敗就維持 stderr，回傳錯誤訊息）。未攔到的例外仍會到 stderr / journal。"""
    try:
        os.makedirs(os.path.dirname(path), exist_ok=True)
        handler = DailyLog(path, keep_days)
    except OSError as e:
        return "沒辦法寫 log 檔 %s：%s" % (path, e)
    handler.setFormatter(logging.Formatter("%(asctime)s %(levelname)s %(name)s %(message)s"))
    root = logging.getLogger()
    for old in list(root.handlers):
        root.removeHandler(old)
    root.addHandler(handler)
    root.setLevel(level)
    return None
