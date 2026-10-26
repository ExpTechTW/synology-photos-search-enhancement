// Package db：外掛資料庫（SQLite，純 Go 的實作，不需要 cgo）。
//
// 只存搜尋需要的最少資料：照片 id、時間、類型、縮圖版本、向量、OCR 文字。
// 不存 item JSON：注入時由 filter 用使用者自己的 session 向 DSM 取，格式與權限都由 DSM 決定。
// 向量存 f16 × 768（模型原始輸出），搜尋時再截短、量化，改格式不必重跑模型。
// 格式跟 Python 版完全相同，升級後沿用原本的索引。
package db

import (
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	_ "modernc.org/sqlite" // 註冊 "sqlite" driver
)

const SchemaVersion = 1

const (
	// SQLite 自己的 page cache（KiB）。page 64 KB，預設 2 MB 只放得下 31 頁：
	// 寫入端（索引器）交易中改過的頁要先放在這裡，太小會在交易進行中途就寫出去，多做 I/O
	writerCacheKiB = 16384
	// 讀取端（搜尋服務）主要靠 mmap 走 OS 的檔案快取，這裡放常用的 B-tree 內部頁就夠
	readerCacheKiB = 8192
	// WAL 累積到 128 頁（8 MB）就寫回主檔，寫回後暫存檔縮回 8 MB；預設 1000 頁 × 64 KB 會長到 64 MB
	walAutocheckpointPages = 128
	walSizeLimit           = 8 * 1024 * 1024
	mmapSize               = 268435456
)

// 狀態：0 待處理、1 完成、2 放棄（重試多次仍失敗）
const (
	Pending = 0
	Done    = 1
	Failed  = 2
)

const schema = `
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
`

// Q 是 *sql.DB 與 *sql.Tx 共同的方法。
type Q interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

func uri(path string, params ...string) string {
	r := strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23")
	return "file:" + r.Replace(path) + "?" + strings.Join(params, "&")
}

func pragmas(kv ...string) []string {
	out := make([]string, len(kv))
	for i, p := range kv {
		out[i] = "_pragma=" + p
	}
	return out
}

// Open 開啟寫入用的連線（索引器、指令列）。第一次會建立資料庫。
//
// 只用一條連線（跟 SQLite 的單一寫入者一致）：交易進行中只能用那個 *sql.Tx，不能再呼叫 db 的方法，否則會卡住。
func Open(path string) (*sql.DB, error) {
	path, _ = filepath.Abs(path)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
		if err := create(path); err != nil {
			return nil, err
		}
	}
	d, err := sql.Open("sqlite", uri(path, pragmas(
		"busy_timeout(30000)", "journal_mode(WAL)", "synchronous(NORMAL)",
		fmt.Sprintf("wal_autocheckpoint(%d)", walAutocheckpointPages),
		fmt.Sprintf("journal_size_limit(%d)", walSizeLimit),
		fmt.Sprintf("mmap_size(%d)", mmapSize), fmt.Sprintf("cache_size(-%d)", writerCacheKiB))...))
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(1)
	d.SetConnMaxIdleTime(0)
	if _, err := d.Exec(schema); err != nil {
		d.Close()
		return nil, err
	}
	ver, ok, err := GetMeta(d, "schema_version")
	if err != nil {
		d.Close()
		return nil, err
	}
	if !ok {
		if err := Tx(d, func(tx *sql.Tx) error {
			if err := SetMeta(tx, "schema_version", SchemaVersion); err != nil {
				return err
			}
			return SetMeta(tx, "rev", 0)
		}); err != nil {
			d.Close()
			return nil, err
		}
	} else if ver != strconv.Itoa(SchemaVersion) {
		d.Close()
		return nil, fmt.Errorf("資料庫版本 %s 與程式版本 %d 不同", ver, SchemaVersion)
	}
	return d, nil
}

// create 建立新的資料庫：page_size 必須在建第一張表、切換 WAL 之前設定。
// 向量一列 1.5 KB，預設 4 KB page 會浪費約三成空間。
func create(path string) error {
	d, err := sql.Open("sqlite", uri(path))
	if err != nil {
		return err
	}
	defer d.Close()
	d.SetMaxOpenConns(1)
	for _, s := range []string{"PRAGMA page_size=65536", "PRAGMA journal_mode=WAL", schema} {
		if _, err := d.Exec(s); err != nil {
			return err
		}
	}
	return nil
}

// OpenReadOnly 開啟唯讀連線（搜尋服務）：主要靠 mmap 走 OS 的檔案快取。
func OpenReadOnly(path string) (*sql.DB, error) {
	path, _ = filepath.Abs(path)
	d, err := sql.Open("sqlite", uri(path, append([]string{"mode=ro"}, pragmas(
		"busy_timeout(30000)", fmt.Sprintf("mmap_size(%d)", mmapSize),
		fmt.Sprintf("cache_size(-%d)", readerCacheKiB))...)...))
	if err != nil {
		return nil, err
	}
	d.SetMaxOpenConns(4)
	return d, nil
}

// Tx 在交易裡執行 fn；fn 回傳錯誤就整個復原。
func Tx(d *sql.DB, fn func(tx *sql.Tx) error) error {
	tx, err := d.Begin()
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		tx.Rollback()
		return err
	}
	return tx.Commit()
}

// SearchMarker：搜尋服務每次搜尋時更新這個檔的修改時間，索引器看到就先讓出 GPU。
func SearchMarker(dbPath string) string {
	abs, _ := filepath.Abs(dbPath)
	return filepath.Join(filepath.Dir(abs), ".search-activity")
}

// FileBytes：資料庫在硬碟上的總大小（主檔 + WAL + shm）。
func FileBytes(path string) int64 {
	var n int64
	for _, s := range []string{"", "-wal", "-shm"} {
		if st, err := os.Stat(path + s); err == nil {
			n += st.Size()
		}
	}
	return n
}

// SpaceStats 回傳 (主檔大小, 可回收的空間)：B-tree 頁裡沒用到的部分加上空閒頁。
// 需要 dbstat（讀過每一頁，資料庫大時要幾秒）。
func SpaceStats(q Q) (size, wasted int64, err error) {
	var page, pages, free, unused int64
	if err = q.QueryRow("PRAGMA page_size").Scan(&page); err != nil {
		return
	}
	if err = q.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		return
	}
	if err = q.QueryRow("PRAGMA freelist_count").Scan(&free); err != nil {
		return
	}
	var u sql.NullInt64
	if err = q.QueryRow("SELECT sum(unused) FROM dbstat").Scan(&u); err != nil {
		return
	}
	unused = u.Int64
	return pages * page, unused + free*page, nil
}

// Compact：VACUUM 重寫整個資料庫，把頁填滿、回收空閒頁，然後把 WAL 縮回去。
// 在 WAL 模式下搜尋服務照樣可以讀（讀到的是壓實前或壓實後的一致版本）。
func Compact(d *sql.DB) error {
	if _, err := d.Exec("VACUUM"); err != nil {
		return err
	}
	_, err := d.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	return err
}

// RequeueFailed 把放棄的照片重新排入（只改資料庫，索引器在跑也可以用）。回傳 embedding、OCR 的張數。
func RequeueFailed(d *sql.DB) (emb, ocr int64, err error) {
	err = Tx(d, func(tx *sql.Tx) error {
		rev, err := NextRev(tx)
		if err != nil {
			return err
		}
		r, err := tx.Exec("UPDATE item SET emb_state=0, emb_tries=0, rev=? WHERE emb_state=2", rev)
		if err != nil {
			return err
		}
		emb, _ = r.RowsAffected()
		r, err = tx.Exec("UPDATE item SET ocr_state=0, ocr_tries=0, rev=? WHERE ocr_state=2", rev)
		if err != nil {
			return err
		}
		ocr, _ = r.RowsAffected()
		return nil
	})
	return
}

// GetMeta 讀 meta 表；沒有這個 key 時 ok 為 false。
func GetMeta(q Q, k string) (v string, ok bool, err error) {
	var s sql.NullString
	err = q.QueryRow("SELECT v FROM meta WHERE k=?", k).Scan(&s)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	return s.String, err == nil, err
}

// MetaInt 讀數字型的 meta，沒有或格式不對時回傳 def。
func MetaInt(q Q, k string, def int64) int64 {
	v, ok, err := GetMeta(q, k)
	if err != nil || !ok {
		return def
	}
	n, err := strconv.ParseInt(v, 10, 64)
	if err != nil {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return int64(f)
		}
		return def
	}
	return n
}

// MetaFloat 讀小數型的 meta（例如時間戳），沒有或格式不對時回傳 def。
func MetaFloat(q Q, k string, def float64) float64 {
	v, ok, err := GetMeta(q, k)
	if err != nil || !ok {
		return def
	}
	f, err := strconv.ParseFloat(v, 64)
	if err != nil {
		return def
	}
	return f
}

// SetMeta 寫 meta 表（值一律存成文字）。
func SetMeta(q Q, k string, v any) error {
	_, err := q.Exec("INSERT INTO meta(k, v) VALUES (?, ?) ON CONFLICT(k) DO UPDATE SET v=excluded.v", k, fmt.Sprint(v))
	return err
}

// NextRev 在同一個交易裡取得下一個版本號。
func NextRev(q Q) (int64, error) {
	rev := MetaInt(q, "rev", 0) + 1
	return rev, SetMeta(q, "rev", rev)
}

// Stats 是索引的統計（progress --json 的欄位名稱跟 Python 版相同）。
type Stats struct {
	Items        int64  `json:"items"`
	Deleted      int64  `json:"deleted"`
	Embedded     int64  `json:"embedded"`
	EmbedPending int64  `json:"embed_pending"`
	EmbedFailed  int64  `json:"embed_failed"`
	OCRDone      int64  `json:"ocr_done"`
	OCRPending   int64  `json:"ocr_pending"`
	OCRFailed    int64  `json:"ocr_failed"`
	WithText     int64  `json:"with_text"`
	TextEmbedded int64  `json:"text_embedded"`
	Rev          int64  `json:"rev"`
	OwnerUserID  string `json:"owner_user_id"`
}

// GetStats 統計索引。
func GetStats(q Q) (Stats, error) {
	var s Stats
	for _, x := range []struct {
		dst *int64
		sql string
	}{
		{&s.Items, "SELECT count(*) FROM item WHERE deleted=0"},
		{&s.Deleted, "SELECT count(*) FROM item WHERE deleted=1"},
		{&s.Embedded, "SELECT count(*) FROM item WHERE deleted=0 AND emb_state=1"},
		{&s.EmbedPending, "SELECT count(*) FROM item WHERE deleted=0 AND emb_state=0"},
		{&s.EmbedFailed, "SELECT count(*) FROM item WHERE deleted=0 AND emb_state=2"},
		{&s.OCRDone, "SELECT count(*) FROM item WHERE deleted=0 AND ocr_state=1"},
		{&s.OCRPending, "SELECT count(*) FROM item WHERE deleted=0 AND ocr_state=0"},
		{&s.OCRFailed, "SELECT count(*) FROM item WHERE deleted=0 AND ocr_state=2"},
		{&s.WithText, "SELECT count(*) FROM ocr o JOIN item i USING(unit_id) WHERE i.deleted=0 AND o.text<>''"},
		{&s.TextEmbedded, "SELECT count(*) FROM ocr_vec t JOIN item i USING(unit_id) JOIN ocr o USING(unit_id) " +
			"WHERE i.deleted=0 AND o.text<>''"},
	} {
		if err := q.QueryRow(x.sql).Scan(x.dst); err != nil {
			return s, err
		}
	}
	s.Rev = MetaInt(q, "rev", 0)
	s.OwnerUserID, _, _ = GetMeta(q, "owner_user_id")
	return s, nil
}
