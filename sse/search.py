"""搜尋：向量常駐記憶體（numpy），OCR 文字與照片資料交給 SQLite。

向量搜尋每次查詢都要看過每一張照片的向量，沒有「熱門資料」可言，所以不靠 SQLite 的 page cache，
而是把截短、量化後的向量整份放在 numpy 陣列裡（512 維 int8，每張 512 bytes + 4 bytes 縮放係數）。
圖片一組、照片裡的文字（OCR）一組，分數範圍不同，門檻分開設。
OCR 文字比對、照片時間這些逐筆查詢才交給 SQLite（mmap + page cache）。

hub 校正：有些照片（例如某些截圖）跟什麼查詢都很像，會出現在各種不相關的搜尋裡。每張照片先算出它對一組
通用查詢的平均分數（hubness），比對時扣掉它高於整體平均的部分。在真實 NAS 上以原生搜尋結果當參考答案，
同樣門檻下精確率多約 10 個百分點、召回率多約 8 個百分點。
"""

import logging
import sqlite3
import threading

import numpy as np

from . import db as dbm

log = logging.getLogger("sse.search")

CHUNK = 32768

QUERY_BANK_PREFIX = "task: search result | query: "
# 通用查詢：只用來估計每張照片的 hubness，內容涵蓋常見主題即可
HUB_BANK = ["人", "朋友", "自拍", "食物", "咖啡", "貓", "狗", "車", "城市", "夜景", "街道", "建築", "海", "山", "森林",
            "公園", "天空", "花", "樹", "運動", "旅行", "演唱會", "生日", "辦公室", "電腦", "手機", "書", "文件", "截圖",
            "地圖", "衣服", "超市", "寺廟", "海灘", "聖誕節", "煙火", "鳥", "魚", "people", "food", "car", "city",
            "screenshot", "document", "室內", "戶外", "桌子", "椅子", "窗戶", "門"]


def prepare(v768, dims):
    """f16 × 768 → 前 dims 維、重新正規化、int8 量化。回傳 (int8 向量, 縮放係數)。"""
    v = np.frombuffer(v768, dtype="<f2").astype(np.float32)[:dims]
    n = float(np.linalg.norm(v))
    if not np.isfinite(n) or n == 0:
        return None, None
    v /= n
    scale = float(np.abs(v).max()) / 127.0
    if scale == 0:
        return None, None
    return np.round(v / scale).astype(np.int8), np.float32(scale)


def prepare_query(q, dims):
    v = np.asarray(q, dtype=np.float32)[:dims]
    return v / np.linalg.norm(v)


def select(scores, min_score, max_drop, max_results):
    """分數 → 要列入結果的列號。Synology 的搜尋結果是照時間排的，所以要決定「哪些算命中」，不是排名。

    不用「平均 + N 個標準差」：常見的主題（例如貓佔了一成以上的照片）會把平均與標準差一起拉高，
    結果所有照片都被刷掉（真實 NAS 上「貓」因此一張都沒選到）。
    """
    if scores.size == 0:
        return np.empty(0, dtype=np.int64)
    cut = max(min_score, float(scores.max()) - max_drop)
    idx = np.nonzero(scores >= cut)[0]
    if idx.size > max_results:
        idx = idx[np.argsort(-scores[idx])[:max_results]]
    return idx


class VecSet:
    """一組常駐記憶體的向量：int8 × dims、每列一個縮放係數，加上 hub 校正。圖片與照片裡的文字各一組。"""

    def __init__(self, dims):
        self.dims = dims
        self.ids = np.empty(0, dtype=np.int64)
        self.mat = np.empty((0, dims), dtype=np.int8)
        self.scale = np.empty(0, dtype=np.float32)
        self.hub = np.empty(0, dtype=np.float32)

    def hubness(self, mat, scale, bank):
        if bank is None or not len(mat):
            return np.zeros(len(mat), dtype=np.float32)
        with np.errstate(all="ignore"):
            return ((mat.astype(np.float32) * scale[:, None]) @ bank.T).mean(axis=1).astype(np.float32)

    def rehub(self, bank):
        self.hub = self.hubness(self.mat, self.scale, bank)

    def update(self, changes, bank):
        """changes：{unit_id: f16 向量的 bytes，或 None 表示移除}。"""
        pos = {int(u): k for k, u in enumerate(self.ids)}
        keep = np.ones(self.ids.size, dtype=bool)
        changed, add_ids, add_vec, add_scale = [], [], [], []
        for uid, blob in changes.items():
            k = pos.get(uid)
            vec, scale = prepare(blob, self.dims) if blob else (None, None)
            if vec is None:
                if k is not None:
                    keep[k] = False
            elif k is not None:
                self.mat[k], self.scale[k] = vec, scale
                changed.append(k)
            else:
                add_ids.append(uid)
                add_vec.append(vec)
                add_scale.append(scale)
        if changed and bank is not None:
            self.hub[changed] = self.hubness(self.mat[changed], self.scale[changed], bank)
        if not keep.all():
            self.ids, self.mat, self.scale, self.hub = self.ids[keep], self.mat[keep], self.scale[keep], self.hub[keep]
        if add_ids:
            add_mat, add_scale = np.stack(add_vec), np.asarray(add_scale, dtype=np.float32)
            self.ids = np.concatenate([self.ids, np.asarray(add_ids, dtype=np.int64)])
            self.mat = np.concatenate([self.mat, add_mat])
            self.scale = np.concatenate([self.scale, add_scale])
            self.hub = np.concatenate([self.hub, self.hubness(add_mat, add_scale, bank)])

    def scores(self, qv, banked):
        """回傳 (照片 id, hub 校正後的分數)。呼叫端要持有 Index.lock 或先複製一份參照。"""
        ids, mat, scale, hub = self.ids, self.mat, self.scale, self.hub
        out = np.empty(ids.size, dtype=np.float32)
        # macOS 的 Accelerate 在 matmul 會誤報浮點例外；向量在載入時已確認都是有限值
        with np.errstate(all="ignore"):
            for i in range(0, ids.size, CHUNK):
                out[i:i + CHUNK] = (mat[i:i + CHUNK].astype(np.float32) @ qv) * scale[i:i + CHUNK]
        if banked and ids.size:
            out -= hub - hub.mean()
        return ids, out


class Index:
    def __init__(self, db_path, dims):
        self.db_path = db_path
        self.dims = dims
        self.lock = threading.Lock()
        self.rev = -1
        self.img = VecSet(dims)    # 圖片
        self.txt = VecSet(dims)    # 照片裡的文字（OCR）
        self.bank = None    # 通用查詢的向量（HUB_BANK），有了才做 hub 校正
        self.meta = {}      # unit_id → (time, type)
        self.owner = None
        self._conn = None

    @property
    def ids(self):
        return self.img.ids

    def conn(self):
        if self._conn is None:
            self._conn = dbm.connect(self.db_path, readonly=True)
        return self._conn

    def set_bank(self, vectors):
        bank = np.stack([prepare_query(v, self.dims) for v in vectors]).astype(np.float32)
        with self.lock:
            self.bank = bank
            self.img.rehub(bank)
            self.txt.rehub(bank)

    def refresh(self):
        """讀取 rev 之後有變動的照片。第一次呼叫時載入全部。"""
        c = self.conn()
        rev = int(dbm.get_meta(c, "rev", 0))
        if rev == self.rev:
            return 0
        owner = dbm.get_meta(c, "owner_user_id")
        try:
            rows = c.execute("SELECT i.unit_id, i.time, i.type, i.deleted, i.emb_state, v.v, t.v FROM item i "
                             "LEFT JOIN vec v USING(unit_id) LEFT JOIN ocr_vec t USING(unit_id) "
                             "WHERE i.rev > ?", (self.rev,)).fetchall()
        except sqlite3.OperationalError:   # 舊版資料庫還沒有 ocr_vec（索引器下次啟動會建立）
            rows = [r + (None,) for r in c.execute(
                "SELECT i.unit_id, i.time, i.type, i.deleted, i.emb_state, v.v FROM item i "
                "LEFT JOIN vec v USING(unit_id) WHERE i.rev > ?", (self.rev,))]
        img, txt = {}, {}
        with self.lock:
            for uid, t, typ, deleted, emb, blob, tblob in rows:
                ok = not deleted and emb == dbm.DONE and bool(blob)
                img[uid] = blob if ok else None
                txt[uid] = tblob if ok and tblob else None
                if ok:
                    self.meta[uid] = (t, typ)
                else:
                    self.meta.pop(uid, None)
            self.img.update(img, self.bank)
            self.txt.update(txt, self.bank)
            self.rev = rev
            self.owner = int(owner) if owner is not None else None
        return len(rows)

    def scores(self, qv):
        """圖片：回傳 (照片 id, hub 校正後的分數)。"""
        with self.lock:
            return self.img.scores(qv, self.bank is not None)

    def text_scores(self, qv):
        """照片裡的文字：回傳 (照片 id, hub 校正後的分數)。"""
        with self.lock:
            return self.txt.scores(qv, self.bank is not None)

    def text_hits(self, q, limit):
        """OCR 文字裡出現搜尋字的照片。3 個字以上用 FTS（trigram），更短的用 LIKE。"""
        q = q.strip()
        if not q:
            return []
        c = self.conn()
        if len(q) >= 3:
            phrase = '"%s"' % q.replace('"', '""')
            rows = c.execute("SELECT rowid FROM ocr_fts WHERE ocr_fts MATCH ? LIMIT ?", (phrase, limit)).fetchall()
        else:
            esc = q.replace("\\", "\\\\").replace("%", "\\%").replace("_", "\\_")
            rows = c.execute("SELECT unit_id FROM ocr WHERE text LIKE ? ESCAPE '\\' LIMIT ?",
                             ("%" + esc + "%", limit)).fetchall()
        return [r[0] for r in rows]

    def tag_hits(self, q):
        """手動標籤完全相同的照片：[(unit_id, time, type)]。不需要有向量（例如縮圖做不出來的照片也能標）。"""
        q = q.strip().casefold()
        if not q:
            return []
        try:
            return self.conn().execute("SELECT t.unit_id, i.time, i.type FROM tag t JOIN item i USING(unit_id) "
                                       "WHERE t.tag=? AND i.deleted=0", (q,)).fetchall()
        except sqlite3.OperationalError:
            return []  # 舊版資料庫還沒有 tag 表（索引器下次啟動會建立）

    def search(self, qvec, keyword, scfg, item_type="all"):
        """回傳 [{"id", "time", "type", "score", "src"}]，依時間新到舊。"""
        hits = {}
        tags = self.tag_hits(keyword)
        text = [u for u in self.text_hits(keyword, scfg["max_results"]) if u in self.meta] if scfg["ocr_match"] else []
        # 標籤、OCR 文字命中只是「加進來」，不影響向量結果的門檻：常見的字幾乎都會出現在某張照片的文字裡
        # （例如一張寫著「貓乾糧」的包裝照片，會讓「貓」的向量結果幾乎全部消失）
        qv = prepare_query(qvec, self.dims)
        ids, scores = self.scores(qv)
        for k in select(scores, scfg["min_score"], scfg["max_drop"], scfg["max_results"]):
            uid = int(ids[k])
            hits[uid] = (float(scores[k]), "vec") + self.meta.get(uid, (None, None))
        # 照片裡文字的語意（例如搜「收據」找到文字寫著「電子發票」的截圖）
        ids, scores = self.text_scores(qv)
        for k in select(scores, scfg["text_min_score"], scfg["text_max_drop"], scfg["max_results"]):
            uid = int(ids[k])
            if uid not in hits:
                hits[uid] = (float(scores[k]), "text") + self.meta.get(uid, (None, None))
        for uid in text:
            hits[uid] = (1.0, "ocr") + self.meta[uid]
        for uid, t, typ in tags:
            hits[uid] = (1.0, "tag", t, typ)
        out = []
        for uid, (score, src, t, typ) in hits.items():
            if t is None or (item_type not in ("all", None) and typ != item_type):
                continue
            out.append({"id": uid, "time": t, "type": typ, "score": round(score, 4), "src": src})
        out.sort(key=lambda x: (-x["time"], -x["id"]))
        return out[:scfg["max_results"]]

    def explain(self, qvec, top=20, text=False):
        qv = prepare_query(qvec, self.dims)
        ids, scores = self.text_scores(qv) if text else self.scores(qv)
        order = np.argsort(-scores)[:top]
        return [(int(ids[k]), float(scores[k])) for k in order], (float(scores.mean()) if scores.size else 0.0,
                                                                 float(scores.std()) if scores.size else 0.0)
