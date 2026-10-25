"""外掛資料庫（SQLite）。

只存搜尋需要的最少資料：照片 id、時間、類型、縮圖版本、向量、OCR 文字。
不存 item JSON：注入時由 filter 用使用者自己的 session 向 DSM 取，格式與權限都由 DSM 決定。

向量存 f16 × 768（模型原始輸出），搜尋時再截短、量化，改格式不必重跑模型。
"""

import os
import sqlite3

SCHEMA_VERSION = 1

# SQLite 自己的 page cache（KiB）。page 是 64 KB，預設 2 MB 只放得下 31 頁：
#   寫入端（索引器）交易中改過的頁要先放在這裡，太小會在交易進行中途就寫出去，多做 I/O
#   讀取端（搜尋服務）主要靠 mmap 走 OS 的檔案快取，這裡放常用的 B-tree 內部頁就夠
WRITER_CACHE_KIB = 16384
READER_CACHE_KIB = 8192
# WAL 累積到 128 頁（8 MB）就寫回主檔，寫回後暫存檔縮回 8 MB；預設 1000 頁 × 64 KB 會長到 64 MB
WAL_AUTOCHECKPOINT_PAGES = 128
WAL_SIZE_LIMIT = 8 * 1024 * 1024

# 狀態：0 待處理、1 完成、2 放棄（重試多次仍失敗）
PENDING, DONE, FAILED = 0, 1, 2

SCHEMA = """
CREATE TABLE IF NOT EXISTS meta (
    k TEXT PRIMARY KEY,
    v TEXT
);
CREATE TABLE IF NOT EXISTS item (
    unit_id       INTEGER PRIMARY KEY,   -- Synology 的 item id
    owner_user_id INTEGER NOT NULL,
    time          INTEGER NOT NULL,      -- Synology 的 time 欄位（拍攝時間）
    type          TEXT    NOT NULL,      -- photo | video | live
    cache_key     TEXT    NOT NULL,      -- 縮圖版本，變了就重做
    seen_scan     INTEGER NOT NULL,      -- 最後一次在完整掃描中看到的掃描編號
    deleted       INTEGER NOT NULL DEFAULT 0,
    emb_state     INTEGER NOT NULL DEFAULT 0,
    emb_tries     INTEGER NOT NULL DEFAULT 0,
    ocr_state     INTEGER NOT NULL DEFAULT 0,
    ocr_tries     INTEGER NOT NULL DEFAULT 0,
    last_error    TEXT,
    rev           INTEGER NOT NULL       -- 搜尋服務用來增量更新
);
CREATE INDEX IF NOT EXISTS idx_item_rev ON item(rev);
CREATE INDEX IF NOT EXISTS idx_item_emb ON item(emb_state, deleted);
CREATE INDEX IF NOT EXISTS idx_item_ocr ON item(ocr_state, deleted);
CREATE TABLE IF NOT EXISTS vec (
    unit_id INTEGER PRIMARY KEY,
    model   TEXT NOT NULL,
    v       BLOB NOT NULL                -- float16 × 768，little-endian
);
CREATE TABLE IF NOT EXISTS ocr (
    unit_id INTEGER PRIMARY KEY,
    model   TEXT NOT NULL,
    text    TEXT NOT NULL                -- 沒有文字時是空字串
);
-- OCR 文字的向量（加上文件前綴 "title: none | text: "）：讓搜尋字跟照片裡的文字比語意，
-- 例如搜「收據」找到文字寫著「電子發票」的照片。v 是空的 = 這段文字做不出向量（不再重試）
CREATE TABLE IF NOT EXISTS ocr_vec (
    unit_id INTEGER PRIMARY KEY,
    model   TEXT NOT NULL,
    v       BLOB NOT NULL                -- float16 × 768，little-endian
);
-- 手動標籤：只存在外掛資料庫，不改 Synology 的資料。搜尋字跟標籤完全相同（不分大小寫）就列入
CREATE TABLE IF NOT EXISTS tag (
    tag     TEXT NOT NULL,                -- 一律存小寫（casefold）
    unit_id INTEGER NOT NULL,
    PRIMARY KEY (tag, unit_id)
);
-- trigram：中文沒有空白分詞，用三字元片段做子字串比對（查詢少於 3 個字時改用 LIKE）
CREATE VIRTUAL TABLE IF NOT EXISTS ocr_fts USING fts5(text, content='ocr', content_rowid='unit_id', tokenize='trigram');
CREATE TRIGGER IF NOT EXISTS ocr_ai AFTER INSERT ON ocr BEGIN
    INSERT INTO ocr_fts(rowid, text) VALUES (new.unit_id, new.text);
END;
CREATE TRIGGER IF NOT EXISTS ocr_ad AFTER DELETE ON ocr BEGIN
    INSERT INTO ocr_fts(ocr_fts, rowid, text) VALUES ('delete', old.unit_id, old.text);
END;
CREATE TRIGGER IF NOT EXISTS ocr_au AFTER UPDATE ON ocr BEGIN
    INSERT INTO ocr_fts(ocr_fts, rowid, text) VALUES ('delete', old.unit_id, old.text);
    INSERT INTO ocr_fts(rowid, text) VALUES (new.unit_id, new.text);
END;
"""


def connect(path, readonly=False):
    if readonly:
        db = sqlite3.connect("file:%s?mode=ro" % path, uri=True, timeout=30, check_same_thread=False)
    else:
        os.makedirs(os.path.dirname(os.path.abspath(path)), exist_ok=True)
        new = not os.path.exists(path)
        db = sqlite3.connect(path, timeout=30, check_same_thread=False)
        if new:
            # 必須在建第一張表之前設定：向量一列 1.5 KB，預設 4 KB page 會浪費約三成空間
            db.execute("PRAGMA page_size=65536")
        db.execute("PRAGMA journal_mode=WAL")
        db.execute("PRAGMA synchronous=NORMAL")
        db.execute("PRAGMA wal_autocheckpoint=%d" % WAL_AUTOCHECKPOINT_PAGES)
        db.execute("PRAGMA journal_size_limit=%d" % WAL_SIZE_LIMIT)
        db.executescript(SCHEMA)
        ver = get_meta(db, "schema_version")
        if ver is None:
            set_meta(db, "schema_version", SCHEMA_VERSION)
            set_meta(db, "rev", 0)
            db.commit()
        elif int(ver) != SCHEMA_VERSION:
            raise RuntimeError("資料庫版本 %s 與程式版本 %s 不同" % (ver, SCHEMA_VERSION))
    # 讓 SQLite 直接用 OS 的 page cache 讀檔，不在自己的 cache 裡再存一份
    db.execute("PRAGMA mmap_size=268435456")
    db.execute("PRAGMA cache_size=-%d" % (READER_CACHE_KIB if readonly else WRITER_CACHE_KIB))
    db.execute("PRAGMA busy_timeout=30000")
    return db


def search_marker(db_path):
    """搜尋服務每次搜尋時更新這個檔的修改時間，索引器看到就先讓出 GPU。"""
    return os.path.join(os.path.dirname(os.path.abspath(db_path)), ".search-activity")


def file_bytes(path):
    """資料庫在硬碟上的總大小（主檔 + WAL + shm）。"""
    return sum(os.path.getsize(path + s) for s in ("", "-wal", "-shm") if os.path.exists(path + s))


def space_stats(db):
    """(主檔大小, 可回收的空間)：B-tree 頁裡沒用到的部分加上空閒頁。需要 dbstat（讀過每一頁，資料庫大時要幾秒）。"""
    page = db.execute("PRAGMA page_size").fetchone()[0]
    pages = db.execute("PRAGMA page_count").fetchone()[0]
    free = db.execute("PRAGMA freelist_count").fetchone()[0]
    unused = db.execute("SELECT sum(unused) FROM dbstat").fetchone()[0] or 0
    return pages * page, unused + free * page


def compact(db):
    """VACUUM：重寫整個資料庫，把頁填滿、回收空閒頁，然後把 WAL 縮回去。
    在 WAL 模式下搜尋服務照樣可以讀（讀到的是壓實前或壓實後的一致版本）。"""
    db.commit()
    db.execute("VACUUM")
    db.execute("PRAGMA wal_checkpoint(TRUNCATE)")


def requeue_failed(db):
    """把放棄的照片重新排入（只改資料庫，索引器在跑也可以用）。回傳 (embedding, OCR) 張數。"""
    with db:
        rev = next_rev(db)
        a = db.execute("UPDATE item SET emb_state=0, emb_tries=0, rev=? WHERE emb_state=2", (rev,)).rowcount
        b = db.execute("UPDATE item SET ocr_state=0, ocr_tries=0, rev=? WHERE ocr_state=2", (rev,)).rowcount
    return a, b


def get_meta(db, k, default=None):
    row = db.execute("SELECT v FROM meta WHERE k=?", (k,)).fetchone()
    return row[0] if row else default


def set_meta(db, k, v):
    db.execute("INSERT INTO meta(k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", (k, str(v)))


def next_rev(db):
    """在同一個 transaction 裡取得下一個版本號。"""
    rev = int(get_meta(db, "rev", 0)) + 1
    set_meta(db, "rev", rev)
    return rev


def stats(db):
    q = lambda sql: db.execute(sql).fetchone()[0]  # noqa: E731
    return {
        "items": q("SELECT count(*) FROM item WHERE deleted=0"),
        "deleted": q("SELECT count(*) FROM item WHERE deleted=1"),
        "embedded": q("SELECT count(*) FROM item WHERE deleted=0 AND emb_state=1"),
        "embed_pending": q("SELECT count(*) FROM item WHERE deleted=0 AND emb_state=0"),
        "embed_failed": q("SELECT count(*) FROM item WHERE deleted=0 AND emb_state=2"),
        "ocr_done": q("SELECT count(*) FROM item WHERE deleted=0 AND ocr_state=1"),
        "ocr_pending": q("SELECT count(*) FROM item WHERE deleted=0 AND ocr_state=0"),
        "ocr_failed": q("SELECT count(*) FROM item WHERE deleted=0 AND ocr_state=2"),
        "with_text": q("SELECT count(*) FROM ocr o JOIN item i USING(unit_id) WHERE i.deleted=0 AND o.text<>''"),
        "text_embedded": q("SELECT count(*) FROM ocr_vec t JOIN item i USING(unit_id) JOIN ocr o USING(unit_id) "
                           "WHERE i.deleted=0 AND o.text<>''"),
        "rev": int(get_meta(db, "rev", 0)),
        "owner_user_id": get_meta(db, "owner_user_id"),
    }
