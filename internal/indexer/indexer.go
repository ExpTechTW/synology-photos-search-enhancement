// Package indexer：索引器。從 Synology Photos 讀縮圖 → embedding / OCR → 寫進外掛資料庫。
//
// 只呼叫讀取類 WebAPI，不寫任何 Synology 的資料。資料庫只由一個 goroutine 寫入。
// 模型服務一次只處理一張（見 llm 套件）；等模型回應的時候，先向 DSM 取下一張的縮圖。
package indexer

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/config"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/db"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/f16"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/llm"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/logs"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/synology"
)

var log = logs.Get("sse.indexer")

// ProbePNG 是 16×16 的純色 PNG：確認 OCR 服務是否正常時用。
var ProbePNG = []byte("\x89PNG\r\n\x1a\n\x00\x00\x00\rIHDR\x00\x00\x00\x10\x00\x00\x00\x10\x08\x02\x00\x00\x00\x90\x91h6" +
	"\x00\x00\x00\x15IDATx\x9cc\xfc\xcf\x80\x1f0\x8e*\x18U0\xaa`\x08\x0b\x00\x9a\xf7\x02\x1e\x81\xc0\x82" +
	"\xac\x00\x00\x00\x00IEND\xaeB`\x82")

// OwnerMismatchError：資料庫屬於另一個使用者，拒絕混在一起（systemd 不重啟）。
type OwnerMismatchError struct{ Msg string }

func (e *OwnerMismatchError) Error() string { return e.Msg }

// ServiceDownError：DSM 或模型服務暫時連不上。整體暫停、稍後再試，不算在個別照片的重試次數裡。
type ServiceDownError struct{ Err error }

func (e *ServiceDownError) Error() string { return e.Err.Error() }
func (e *ServiceDownError) Unwrap() error { return e.Err }

// Transient：看起來像服務性的錯誤（連不上、逾時、5xx、429、session），不是某張照片的問題。
func Transient(err error) bool {
	var le *llm.Error
	if errors.As(err, &le) {
		return le.Transient
	}
	var se *synology.Error
	if errors.As(err, &se) {
		return se.Transient
	}
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, syscall.ECONNREFUSED)
}

const (
	// TextPrepVersion：文字向量的前處理版本。改了前處理規則就加一，舊的文字向量會自動重做
	TextPrepVersion = 2
	// textMinLetters：字母或中文字少於這個數的文字不做向量（例如只有時間、數字的狀態列）：這種向量很空泛，跟什麼都有點像
	textMinLetters = 4

	compactMinBytes      = 8 * 1024 * 1024
	compactMinRatio      = 0.25
	CompactCheckInterval = 86400
)

// Meaningful：文字裡的字母或中文字夠多，值得做成向量。
func Meaningful(text string) bool {
	n := 0
	for _, r := range text {
		if unicode.IsLetter(r) {
			n++
			if n >= textMinLetters {
				return true
			}
		}
	}
	return false
}

// CompactIfWorthwhile：可回收空間達 25% 且至少 8 MB 才壓實（force 則一定做）。
// 回傳壓實前、後的硬碟用量；沒做時 done 為 false。
func CompactIfWorthwhile(d *sql.DB, dbPath string, force bool, progress *Progress) (before, after int64, done bool, err error) {
	if err := db.SetMeta(d, "last_compact_check", time.Now().Unix()); err != nil {
		return 0, 0, false, err
	}
	size, wasted, err := db.SpaceStats(d)
	if err != nil {
		log.Warnf("沒辦法估算可回收的空間（%v），不壓實", err)
		return 0, 0, false, nil
	}
	if !force && (wasted < compactMinBytes || float64(wasted) < float64(size)*compactMinRatio) {
		return 0, 0, false, nil
	}
	// VACUUM 會先在暫存目錄建一份壓實後的副本（engine 已把暫存目錄設成資料庫所在的目錄）
	tmp := os.Getenv("SQLITE_TMPDIR")
	if tmp == "" {
		tmp = os.TempDir()
	}
	need := float64(size-wasted) * 1.2
	var st syscall.Statfs_t
	if syscall.Statfs(tmp, &st) == nil && float64(st.Bavail)*float64(st.Bsize) < need {
		log.Warnf("暫存目錄 %s 的空間不足 %.0f MB，這次不壓實", tmp, need/1e6)
		return 0, 0, false, nil
	}
	if progress != nil {
		progress.Set("compacting", nil)
	}
	before = db.FileBytes(dbPath)
	t := time.Now()
	if err := db.Compact(d); err != nil {
		return 0, 0, false, err
	}
	after = db.FileBytes(dbPath)
	secs := math.Round(time.Since(t).Seconds()*10) / 10
	result, _ := json.Marshal(map[string]any{"before": before, "after": after, "seconds": secs})
	err = db.Tx(d, func(tx *sql.Tx) error {
		if err := db.SetMeta(tx, "last_compact", time.Now().Unix()); err != nil {
			return err
		}
		return db.SetMeta(tx, "last_compact_result", string(result))
	})
	log.Infof("壓實資料庫：%.1f MB → %.1f MB（%.1f 秒）", float64(before)/1e6, float64(after)/1e6, secs)
	return before, after, true, err
}

// Indexer 把照片做成索引。
type Indexer struct {
	cfg      *config.Config
	ic       config.Index
	dsm      *synology.Client
	llm      *llm.Client
	db       *sql.DB
	owner    int64
	embedTag string
	ocrTag   string
	textTag  string
	Progress *Progress
	marker   string

	// YieldToSearch 可以在測試裡調整（秒）
	YieldToSearch int
	yieldLimit    time.Duration
}

// New 建立索引器；d 是 db.Open 開的寫入連線。
func New(cfg *config.Config, dsm *synology.Client, client *llm.Client, d *sql.DB) *Indexer {
	return &Indexer{
		cfg: cfg, ic: cfg.Index, dsm: dsm, llm: client, db: d,
		embedTag: client.EmbedModel + "@" + cfg.Synology.ThumbSize,
		ocrTag:   client.OCRModel + "@" + cfg.Synology.OCRThumbSize,
		textTag:  fmt.Sprintf("%s@text%d", client.EmbedModel, TextPrepVersion),
		Progress: NewProgress(filepath.Join(filepath.Dir(cfg.Index.DB), "indexer-status.json")),
		marker:   db.SearchMarker(cfg.Index.DB), YieldToSearch: cfg.Index.YieldToSearch,
		yieldLimit: 120 * time.Second,
	}
}

// Marker 是搜尋服務每次搜尋時更新的檔案。
func (ix *Indexer) Marker() string { return ix.marker }

// ── 使用者 ──

// BindOwner 確認資料庫屬於目前登入的使用者（第一次就記下來）。
func (ix *Indexer) BindOwner() (int64, string, error) {
	uid, name, err := ix.dsm.Me()
	if err != nil {
		return 0, "", err
	}
	stored, ok, err := db.GetMeta(ix.db, "owner_user_id")
	if err != nil {
		return 0, "", err
	}
	if ok && stored != fmt.Sprint(uid) {
		return 0, "", &OwnerMismatchError{fmt.Sprintf("資料庫是使用者 %s 的索引，但目前登入的是 %s（id %d），拒絕混在一起",
			stored, name, uid)}
	}
	if !ok {
		if err := db.Tx(ix.db, func(tx *sql.Tx) error {
			if err := db.SetMeta(tx, "owner_user_id", uid); err != nil {
				return err
			}
			return db.SetMeta(tx, "owner_name", name)
		}); err != nil {
			return 0, "", err
		}
	}
	ix.owner = uid
	if _, _, err := ix.requeueStale(); err != nil {
		return 0, "", err
	}
	return uid, name, nil
}

// requeueStale：模型或縮圖尺寸改了，用舊設定做的向量 / OCR 重新排入（舊的先留著，重做完才覆蓋）。
func (ix *Indexer) requeueStale() (a, b int64, err error) {
	var c int64
	err = db.Tx(ix.db, func(tx *sql.Tx) error {
		rev, err := db.NextRev(tx)
		if err != nil {
			return err
		}
		r, err := tx.Exec("UPDATE item SET emb_state=0, emb_tries=0, rev=? WHERE emb_state=1 AND unit_id IN "+
			"(SELECT unit_id FROM vec WHERE model<>?)", rev, ix.embedTag)
		if err != nil {
			return err
		}
		a, _ = r.RowsAffected()
		r, err = tx.Exec("UPDATE item SET ocr_state=0, ocr_tries=0, rev=? WHERE ocr_state=1 AND unit_id IN "+
			"(SELECT unit_id FROM ocr WHERE model<>?)", rev, ix.ocrTag)
		if err != nil {
			return err
		}
		b, _ = r.RowsAffected()
		r, err = tx.Exec("DELETE FROM ocr_vec WHERE model<>?", ix.textTag)
		if err != nil {
			return err
		}
		c, _ = r.RowsAffected()
		return nil
	})
	if err == nil && (a > 0 || b > 0 || c > 0) {
		log.Infof("設定改變，重新排入：embedding %d、OCR %d、文字向量 %d", a, b, c)
	}
	return a, b, err
}

// ── 掃描 ──

func (ix *Indexer) upsert(tx *sql.Tx, items []synology.Item, scanID *int64, rev int64) (int, error) {
	changed := 0
	for _, it := range items {
		if it.OwnerUserID != ix.owner {
			continue
		}
		var ck, typ string
		var t, deleted int64
		err := tx.QueryRow("SELECT cache_key, time, type, deleted FROM item WHERE unit_id=?", it.UnitID).
			Scan(&ck, &t, &typ, &deleted)
		switch {
		case errors.Is(err, sql.ErrNoRows):
			if _, err := tx.Exec("INSERT INTO item(unit_id, owner_user_id, time, type, cache_key, seen_scan, rev) "+
				"VALUES (?,?,?,?,?,?,?)", it.UnitID, it.OwnerUserID, it.Time, it.Type, it.CacheKey, nullable(scanID), rev); err != nil {
				return 0, err
			}
			changed++
		case err != nil:
			return 0, err
		case ck != it.CacheKey:
			// 照片或縮圖變了：之前的向量與 OCR 都不再可信
			for _, table := range []string{"vec", "ocr", "ocr_vec"} {
				if _, err := tx.Exec("DELETE FROM "+table+" WHERE unit_id=?", it.UnitID); err != nil {
					return 0, err
				}
			}
			if _, err := tx.Exec("UPDATE item SET cache_key=?, time=?, type=?, deleted=0, seen_scan=?, emb_state=0, "+
				"emb_tries=0, ocr_state=0, ocr_tries=0, last_error=NULL, rev=? WHERE unit_id=?",
				it.CacheKey, it.Time, it.Type, nullable(scanID), rev, it.UnitID); err != nil {
				return 0, err
			}
			changed++
		case t != it.Time || typ != it.Type || deleted != 0:
			if _, err := tx.Exec("UPDATE item SET time=?, type=?, deleted=0, seen_scan=?, rev=? WHERE unit_id=?",
				it.Time, it.Type, nullable(scanID), rev, it.UnitID); err != nil {
				return 0, err
			}
			changed++
		case scanID != nil:
			if _, err := tx.Exec("UPDATE item SET seen_scan=? WHERE unit_id=?", *scanID, it.UnitID); err != nil {
				return 0, err
			}
		}
	}
	return changed, nil
}

func nullable(p *int64) any {
	if p == nil {
		return 0
	}
	return *p
}

func (ix *Indexer) page(fetch func(offset, limit int) ([]map[string]any, error), offset int) (int, []synology.Item, error) {
	raw, err := fetch(offset, ix.ic.PageSize)
	if err != nil {
		return 0, nil, err
	}
	items := make([]synology.Item, 0, len(raw))
	bad := 0
	for _, r := range raw {
		if it, ok := synology.ParseItem(r); ok {
			items = append(items, it)
		} else {
			bad++
		}
	}
	if bad > 0 {
		log.Warnf("有 %d 筆照片資料格式看不懂，略過", bad)
	}
	return len(raw), items, nil
}

// FullScan 翻完整個相簿：新增、縮圖變了就重做、沒看到的標記刪除。回傳看到、變更、刪除的張數。
func (ix *Indexer) FullScan() (seen, changed, removed int, err error) {
	started := time.Now()
	scanID := db.MetaInt(ix.db, "scan_seq", 0) + 1
	var total *int64
	if n, err := ix.dsm.CountItems(); err == nil {
		total = &n
	}
	ix.Progress.Set("full_scan", map[string]any{"scan_done": 0, "scan_total": total})
	offset := 0
	for {
		n, items, err := ix.page(ix.dsm.ListItems, offset)
		if err != nil {
			return 0, 0, 0, err
		}
		if n == 0 {
			break
		}
		err = db.Tx(ix.db, func(tx *sql.Tx) error {
			rev, err := db.NextRev(tx)
			if err != nil {
				return err
			}
			c, err := ix.upsert(tx, items, &scanID, rev)
			changed += c
			return err
		})
		if err != nil {
			return 0, 0, 0, err
		}
		seen += len(items)
		offset += n
		ix.Progress.Update(map[string]any{"scan_done": offset})
	}
	var existing, indexed int
	if err := ix.db.QueryRow("SELECT count(*) FROM item WHERE deleted=0 AND seen_scan<>?", scanID).Scan(&existing); err != nil {
		return 0, 0, 0, err
	}
	if err := ix.db.QueryRow("SELECT count(*) FROM item WHERE deleted=0").Scan(&indexed); err != nil {
		return 0, 0, 0, err
	}
	switch {
	case existing > 0 && float64(seen) < float64(indexed)*0.5:
		// 一次少了一半以上，比較可能是 API 出問題而不是真的刪掉那麼多照片
		log.Warnf("完整掃描只看到 %d 張，但索引裡有 %d 張；這次不標記刪除", seen, indexed)
	case existing > 0:
		err := db.Tx(ix.db, func(tx *sql.Tx) error {
			rev, err := db.NextRev(tx)
			if err != nil {
				return err
			}
			for _, table := range []string{"vec", "ocr", "ocr_vec"} {
				if _, err := tx.Exec("DELETE FROM "+table+" WHERE unit_id IN "+
					"(SELECT unit_id FROM item WHERE deleted=0 AND seen_scan<>?)", scanID); err != nil {
					return err
				}
			}
			r, err := tx.Exec("UPDATE item SET deleted=1, rev=? WHERE deleted=0 AND seen_scan<>?", rev, scanID)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			removed = int(n)
			return nil
		})
		if err != nil {
			return 0, 0, 0, err
		}
	}
	result, _ := json.Marshal(map[string]any{"seen": seen, "changed": changed, "removed": removed,
		"seconds": math.Round(time.Since(started).Seconds()*10) / 10})
	err = db.Tx(ix.db, func(tx *sql.Tx) error {
		for k, v := range map[string]any{"scan_seq": scanID, "last_full_scan": time.Now().Unix(),
			"last_full_scan_result": string(result)} {
			if err := db.SetMeta(tx, k, v); err != nil {
				return err
			}
		}
		return nil
	})
	log.Infof("完整掃描：%d 張，新增或變更 %d，刪除 %d", seen, changed, removed)
	return seen, changed, removed, err
}

// QuickScan 看最近新增的照片，直到一整頁都沒有變化為止。回傳變更的張數。
func (ix *Indexer) QuickScan() (int, error) {
	const maxPages = 5
	scanID := db.MetaInt(ix.db, "scan_seq", 0)
	ix.Progress.Set("quick_scan", nil)
	offset, changed := 0, 0
	for i := 0; i < maxPages; i++ {
		n, items, err := ix.page(ix.dsm.ListRecent, offset)
		if err != nil {
			return changed, err
		}
		if n == 0 {
			break
		}
		var c int
		err = db.Tx(ix.db, func(tx *sql.Tx) error {
			rev, err := db.NextRev(tx)
			if err != nil {
				return err
			}
			c, err = ix.upsert(tx, items, &scanID, rev)
			return err
		})
		if err != nil {
			return changed, err
		}
		changed += c
		if c == 0 {
			break
		}
		offset += n
	}
	if err := db.SetMeta(ix.db, "last_quick_scan", time.Now().Unix()); err != nil {
		return changed, err
	}
	if changed > 0 {
		log.Infof("快速掃描：新增或變更 %d", changed)
	}
	return changed, nil
}

// ── 失敗處理 ──

func (ix *Indexer) fail(column string, uid int64, ck string, cause error) error {
	state, tries := column+"_state", column+"_tries"
	msg := cause.Error()
	if len(msg) > 500 {
		msg = strings.ToValidUTF8(msg[:500], "")
	}
	return db.Tx(ix.db, func(tx *sql.Tx) error {
		rev, err := db.NextRev(tx)
		if err != nil {
			return err
		}
		_, err = tx.Exec(fmt.Sprintf("UPDATE item SET %[1]s=%[1]s+1, last_error=?, rev=?, %[2]s=CASE WHEN %[1]s+1>=? THEN 2 ELSE %[2]s END "+
			"WHERE unit_id=? AND cache_key=?", tries, state), msg, rev, ix.ic.MaxTries, uid, ck)
		return err
	})
}

// healthy：看起來像服務問題的錯誤，是不是其實只跟這張照片有關。對同一個服務發一個很便宜的請求確認它正常
// （例如 llama.cpp 解不開某張圖時回 500、某部影片沒有縮圖時 DSM 回錯誤）。
func (ix *Indexer) healthy(cause error, column string) bool {
	var le *llm.Error
	switch {
	case !errors.As(cause, &le):
		_, _, err := ix.dsm.Me()
		return err == nil
	case column == "emb":
		_, err := ix.llm.EmbedQuery("ping")
		return err == nil
	default:
		_, err := ix.llm.OCR(ProbePNG, "image/png")
		return err == nil
	}
}

// recordFailure：照片本身的問題計入重試次數；服務真的有問題則回傳 ServiceDownError（整體暫停）。
func (ix *Indexer) recordFailure(column string, uid int64, ck string, cause error) error {
	if Transient(cause) && !ix.healthy(cause, column) {
		return &ServiceDownError{cause}
	}
	what := "embedding"
	if column == "ocr" {
		what = "OCR"
	}
	log.Warnf("%s 失敗 %d：%v", what, uid, cause)
	return ix.fail(column, uid, ck, cause)
}

// ── 縮圖 ──

// thumb 抓縮圖；那個尺寸 DSM 沒產生（HTTP 404，例如某些 HEIC 的 m 是 ame_defect）就換其他尺寸。
func (ix *Indexer) thumb(uid int64, ck, size string) ([]byte, string, error) {
	sizes := []string{size}
	others := []string{"xl", "m", "sm"}
	if size == "sm" {
		others = []string{"m", "xl"}
	}
	for _, s := range others {
		if s != size {
			sizes = append(sizes, s)
		}
	}
	var last error
	for _, s := range sizes {
		data, mime, err := ix.dsm.Thumbnail(uid, ck, s)
		if err == nil {
			return data, mime, nil
		}
		var se *synology.Error
		if !errors.As(err, &se) || se.Transient || !strings.Contains(err.Error(), "HTTP 404") {
			return nil, "", err
		}
		last = err
	}
	return nil, "", last
}

type job struct {
	uid int64
	ck  string
}

type fetched struct {
	job
	data []byte
	mime string
	err  error
}

// prefetch 依序抓縮圖，最多比使用的地方多抓一張：等模型回應的時候，下一張的縮圖已經在路上了。
// 呼叫端用完（或提早結束）要 close(stop)。
func (ix *Indexer) prefetch(jobs []job, size string, stop <-chan struct{}) <-chan fetched {
	out := make(chan fetched)
	go func() {
		defer close(out)
		for _, j := range jobs {
			data, mime, err := ix.thumb(j.uid, j.ck, size)
			select {
			case out <- fetched{j, data, mime, err}:
			case <-stop:
				return
			}
		}
	}()
	return out
}

// SearchedRecently：最近 YieldToSearch 秒內有人搜尋。
func (ix *Indexer) SearchedRecently() bool {
	st, err := os.Stat(ix.marker)
	return err == nil && time.Since(st.ModTime()) < time.Duration(ix.YieldToSearch)*time.Second
}

// yieldToSearch：有人在搜尋就等安靜下來再送下一張（最多等 yieldLimit，避免一直有人搜尋時索引完全停住）。
// 查詢字的 embedding 跟索引共用同一個模型服務。
func (ix *Indexer) yieldToSearch() {
	if ix.YieldToSearch <= 0 {
		return
	}
	end := time.Now().Add(ix.yieldLimit)
	for ix.SearchedRecently() && time.Now().Before(end) {
		time.Sleep(time.Second)
	}
}

func (ix *Indexer) pendingJobs(query string, args ...any) ([]job, error) {
	rows, err := ix.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.uid, &j.ck); err != nil {
			return nil, err
		}
		jobs = append(jobs, j)
	}
	return jobs, rows.Err()
}

// ── embedding ──

// EmbedPending 做一批還沒做 embedding 的照片（新到舊），回傳處理掉（成功或確定失敗）的張數；0 表示沒有待處理的。
func (ix *Indexer) EmbedPending() (int, error) {
	jobs, err := ix.pendingJobs("SELECT unit_id, cache_key FROM item WHERE deleted=0 AND emb_state=0 "+
		"ORDER BY time DESC LIMIT ?", 256)
	if err != nil || len(jobs) == 0 {
		return 0, err
	}
	ix.Progress.Set("embedding", nil)
	stop := make(chan struct{})
	defer close(stop)
	done := 0
	for f := range ix.prefetch(jobs, ix.cfg.Synology.ThumbSize, stop) {
		var v []float64
		err := f.err
		if err == nil {
			ix.yieldToSearch()
			v, err = ix.llm.EmbedImage(f.data)
		}
		if err != nil {
			if err := ix.recordFailure("emb", f.uid, f.ck, err); err != nil {
				return done, err
			}
		} else if err := db.Tx(ix.db, func(tx *sql.Tx) error {
			rev, err := db.NextRev(tx)
			if err != nil {
				return err
			}
			r, err := tx.Exec("UPDATE item SET emb_state=1, last_error=NULL, rev=? WHERE unit_id=? AND cache_key=? AND deleted=0",
				rev, f.uid, f.ck)
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n > 0 { // 處理期間照片沒有被改掉或刪掉才寫入
				_, err = tx.Exec("INSERT OR REPLACE INTO vec(unit_id, model, v) VALUES (?,?,?)", f.uid, ix.embedTag, f16.Encode(v))
			}
			return err
		}); err != nil {
			return done, err
		}
		done++
		ix.Progress.Add("emb", 1)
	}
	return done, nil
}

// ── OCR ──

// OCRPending 做一批還沒做 OCR 的照片（embedding 都做完的才做），回傳處理掉的張數。
func (ix *Indexer) OCRPending() (int, error) {
	if !ix.ic.OCR {
		return 0, nil
	}
	jobs, err := ix.pendingJobs("SELECT unit_id, cache_key FROM item WHERE deleted=0 AND emb_state=1 AND ocr_state=0 "+
		"ORDER BY time DESC LIMIT ?", 64)
	if err != nil || len(jobs) == 0 {
		return 0, err
	}
	ix.Progress.Set("ocr", nil)
	stop := make(chan struct{})
	defer close(stop)
	done := 0
	for f := range ix.prefetch(jobs, ix.cfg.Synology.OCRThumbSize, stop) {
		text, err := "", f.err
		if err == nil && f.mime != "image/gif" { // OCR 模型讀不了 GIF（實測會回提示詞本身）；動圖多半也沒有要搜的文字
			text, err = ix.llm.OCR(f.data, f.mime)
		}
		if err != nil {
			if err := ix.recordFailure("ocr", f.uid, f.ck, err); err != nil {
				return done, err
			}
		} else if err := db.Tx(ix.db, func(tx *sql.Tx) error {
			rev, err := db.NextRev(tx)
			if err != nil {
				return err
			}
			r, err := tx.Exec("UPDATE item SET ocr_state=1, last_error=NULL, rev=? WHERE unit_id=? AND cache_key=? AND deleted=0",
				rev, f.uid, f.ck)
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n > 0 {
				if _, err := tx.Exec("INSERT OR REPLACE INTO ocr(unit_id, model, text) VALUES (?,?,?)", f.uid, ix.ocrTag, text); err != nil {
					return err
				}
				_, err = tx.Exec("DELETE FROM ocr_vec WHERE unit_id=?", f.uid)
			}
			return err
		}); err != nil {
			return done, err
		}
		done++
		ix.Progress.Add("ocr", 1)
	}
	return done, nil
}

// ── OCR 文字的向量 ──

// TextEmbedPending 把 OCR 讀出的文字做成向量。文字很短、模型服務的負擔很小，所以不讓給搜尋。
func (ix *Indexer) TextEmbedPending() (int, error) {
	if !ix.ic.OCR || !ix.ic.OCREmbed {
		return 0, nil
	}
	rows, err := ix.db.Query("SELECT o.unit_id, o.text FROM ocr o JOIN item i USING(unit_id) "+
		"LEFT JOIN ocr_vec t USING(unit_id) WHERE i.deleted=0 AND o.text<>'' "+
		"AND t.unit_id IS NULL ORDER BY i.time DESC LIMIT ?", 64)
	if err != nil {
		return 0, err
	}
	type textRow struct {
		uid  int64
		text string
	}
	var todo []textRow
	for rows.Next() {
		var r textRow
		if err := rows.Scan(&r.uid, &r.text); err != nil {
			rows.Close()
			return 0, err
		}
		todo = append(todo, r)
	}
	rows.Close()
	if len(todo) == 0 {
		return 0, nil
	}
	ix.Progress.Set("text_embedding", nil)
	out := map[int64][]byte{}
	for _, r := range todo {
		if !Meaningful(r.text) {
			out[r.uid] = []byte{} // 記成做過（空向量），不再重試
			continue
		}
		v, err := ix.llm.EmbedText(r.text)
		if err != nil {
			if Transient(err) && !ix.healthy(err, "emb") {
				return 0, &ServiceDownError{err}
			}
			log.Warnf("文字向量失敗 %d：%v", r.uid, err)
			out[r.uid] = []byte{}
			continue
		}
		out[r.uid] = f16.Encode(v)
	}
	err = db.Tx(ix.db, func(tx *sql.Tx) error {
		rev, err := db.NextRev(tx)
		if err != nil {
			return err
		}
		for uid, v := range out {
			if _, err := tx.Exec("INSERT OR REPLACE INTO ocr_vec(unit_id, model, v) VALUES (?,?,?)", uid, ix.textTag, v); err != nil {
				return err
			}
			if _, err := tx.Exec("UPDATE item SET rev=? WHERE unit_id=?", rev, uid); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	ix.Progress.Add("txt", len(out))
	return len(out), nil
}

// ── 主迴圈 ──

// MaybeCompact：大量補做索引之後，趁閒下來時壓實（每天最多檢查一次）。
func (ix *Indexer) MaybeCompact() (before, after int64, done bool, err error) {
	if !ix.ic.AutoCompact {
		return 0, 0, false, nil
	}
	if float64(time.Now().Unix())-db.MetaFloat(ix.db, "last_compact_check", 0) < CompactCheckInterval {
		return 0, 0, false, nil
	}
	return CompactIfWorthwhile(ix.db, ix.cfg.Index.DB, false, ix.Progress)
}

func drain(step func() (int, error)) error {
	for {
		n, err := step()
		if err != nil || n == 0 {
			return err
		}
	}
}

// RunOnce 掃描一次並把待處理的都做完（指令列的 --once 與測試用）。
func (ix *Indexer) RunOnce(full bool) error {
	if _, _, err := ix.BindOwner(); err != nil {
		return err
	}
	var err error
	if full {
		_, _, _, err = ix.FullScan()
	} else {
		_, err = ix.QuickScan()
	}
	if err != nil {
		return err
	}
	if err := drain(ix.EmbedPending); err != nil {
		return err
	}
	if err := drain(func() (int, error) {
		n, err := ix.OCRPending()
		if err != nil || n > 0 {
			return n, err
		}
		return ix.TextEmbedPending()
	}); err != nil {
		return err
	}
	ix.Progress.Set("done", nil)
	return nil
}

// Loop 常駐執行直到 stop 被關閉。資料庫屬於別的使用者時回傳 OwnerMismatchError。
func (ix *Indexer) Loop(stop <-chan struct{}) error {
	if _, _, err := ix.BindOwner(); err != nil {
		var om *OwnerMismatchError
		if errors.As(err, &om) {
			ix.Progress.Set("error", map[string]any{"pause_reason": err.Error()})
			return err
		}
		return ix.retryBind(stop, err)
	}
	return ix.loop(stop)
}

// retryBind：啟動時 DSM 連不上，等一下再試（不要讓 systemd 一直重啟）。
func (ix *Indexer) retryBind(stop <-chan struct{}, err error) error {
	backoff := 0
	for {
		backoff = min(600, max(30, backoff*2))
		log.Warnf("服務暫時連不上，%d 秒後再試：%v", backoff, err)
		ix.Progress.Set("paused", map[string]any{"paused_until": time.Now().Unix() + int64(backoff),
			"pause_reason": truncate(err.Error(), 300)})
		if wait(stop, time.Duration(backoff)*time.Second) {
			ix.Progress.Set("stopped", nil)
			return nil
		}
		if _, _, err = ix.BindOwner(); err == nil {
			return ix.loop(stop)
		}
		var om *OwnerMismatchError
		if errors.As(err, &om) {
			ix.Progress.Set("error", map[string]any{"pause_reason": err.Error()})
			return err
		}
	}
}

func (ix *Indexer) loop(stop <-chan struct{}) error {
	lastFull := db.MetaFloat(ix.db, "last_full_scan", 0)
	nextFull := lastFull + float64(ix.ic.FullScanInterval)
	nextQuick := 0.0
	backoff := 0
	for {
		select {
		case <-stop:
			ix.Progress.Set("stopped", nil)
			return nil
		default:
		}
		err := func() error {
			now := float64(time.Now().Unix())
			if now >= nextFull {
				if _, _, _, err := ix.FullScan(); err != nil {
					return err
				}
				nextFull = now + float64(ix.ic.FullScanInterval)
				nextQuick = now + float64(ix.ic.QuickScanInterval)
			} else if now >= nextQuick {
				if _, err := ix.QuickScan(); err != nil {
					return err
				}
				nextQuick = now + float64(ix.ic.QuickScanInterval)
			}
			ix.Progress.Update(map[string]any{"next_full_scan": int64(nextFull), "next_quick_scan": int64(nextQuick)})
			// 文字向量排在 OCR 前面：每做完一批 OCR，下一輪就先把那批文字做成向量
			worked := 0
			for _, step := range []func() (int, error){ix.EmbedPending, ix.TextEmbedPending, ix.OCRPending} {
				n, err := step()
				if err != nil {
					return err
				}
				if n > 0 {
					worked = n
					break
				}
			}
			backoff = 0
			if worked == 0 {
				// 大量補做索引之後，趁閒下來時壓實一次
				if _, _, _, err := ix.MaybeCompact(); err != nil {
					log.Errorf("壓實資料庫失敗：%v", err)
				}
				ix.Progress.Set("idle", nil)
				until := math.Min(nextFull, nextQuick) - float64(time.Now().Unix())
				wait(stop, time.Duration(math.Min(30, math.Max(1, until))*float64(time.Second)))
			}
			return nil
		}()
		if err == nil {
			continue
		}
		var om *OwnerMismatchError
		if errors.As(err, &om) {
			return err
		}
		backoff = min(600, max(30, backoff*2))
		var sd *ServiceDownError
		if errors.As(err, &sd) {
			log.Warnf("服務暫時連不上，%d 秒後再試：%v", backoff, err)
		} else {
			log.Errorf("索引出錯，%d 秒後重試：%v", backoff, err)
		}
		ix.Progress.Set("paused", map[string]any{"paused_until": time.Now().Unix() + int64(backoff),
			"pause_reason": truncate(err.Error(), 300)})
		wait(stop, time.Duration(backoff)*time.Second)
	}
}

// wait 等 d 或直到 stop 被關閉；被關閉時回傳 true。
func wait(stop <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-stop:
		return true
	case <-t.C:
		return false
	}
}

func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
