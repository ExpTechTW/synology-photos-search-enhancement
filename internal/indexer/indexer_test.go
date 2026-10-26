package indexer_test

// 索引器 + 搜尋服務的測試：假 DSM、假模型服務，資料庫是真的 SQLite（對照 Python 版的 tests/test_engine.py）。

import (
	"bufio"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/config"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/db"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/f16"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/indexer"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/llm"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/search"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/server"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/synology"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/testfake"
)

const T0 = testfake.T0

type env struct {
	t   *testing.T
	dir string
	dsm *testfake.DSM
	llm *testfake.LLM
	cfg *config.Config
}

func newEnv(t *testing.T) *env {
	t.Helper()
	dir, err := os.MkdirTemp(os.Getenv("SSE_TEST_TMP"), "sse-eng.")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, dsm: testfake.NewDSM(testfake.EngineLibrary()), llm: testfake.NewLLM()}
	t.Cleanup(func() {
		e.dsm.Close()
		e.llm.Close()
		os.RemoveAll(dir)
	})
	e.writeCfg("alice", "pw-alice", nil)
	return e
}

// writeCfg 寫 config.yml；extra 是 section → 額外的欄位。
func (e *env) writeCfg(account, password string, extra map[string]map[string]any) {
	e.t.Helper()
	doc := map[string]map[string]any{
		"synology":  {"url": e.dsm.URL(), "account": account, "password": password},
		"embedding": {"base_url": e.llm.URL(), "api_key": "sk-test"},
		"ocr":       {"model": "fake-vision"},
		"index":     {"page_size": 7},
		"search":    {"socket": filepath.Join(e.dir, "engine.sock"), "refresh_interval": 1},
	}
	for sec, kv := range extra {
		if doc[sec] == nil {
			doc[sec] = map[string]any{}
		}
		for k, v := range kv {
			doc[sec][k] = v
		}
	}
	data, _ := json.Marshal(doc) // JSON 也是合法的 YAML
	path := filepath.Join(e.dir, "config.yml")
	if err := os.WriteFile(path, data, 0o600); err != nil {
		e.t.Fatal(err)
	}
	cfg, err := config.Load(path)
	if err != nil {
		e.t.Fatal(err)
	}
	e.cfg = cfg
}

func (e *env) client() *llm.Client {
	c := llm.FromConfig(e.cfg)
	return c
}

func (e *env) indexer() (*indexer.Indexer, *sql.DB) {
	e.t.Helper()
	d, err := db.Open(e.cfg.Index.DB)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { d.Close() })
	c := e.cfg.Synology
	dsm := synology.New(c.URL, c.Account, c.Password, false, 10*time.Second)
	return indexer.New(e.cfg, dsm, e.client(), d), d
}

func (e *env) runOnce(full bool) (*indexer.Indexer, *sql.DB) {
	e.t.Helper()
	ix, d := e.indexer()
	if err := ix.RunOnce(full); err != nil {
		e.t.Fatal(err)
	}
	return ix, d
}

func (e *env) index() *search.Index {
	idx := search.NewIndex(e.cfg.Index.DB, e.cfg.Search.Dims)
	e.t.Cleanup(idx.Close)
	return idx
}

func (e *env) search(q string, idx *search.Index) []search.Hit {
	e.t.Helper()
	if idx == nil {
		idx = e.index()
	}
	if _, err := idx.Refresh(); err != nil {
		e.t.Fatal(err)
	}
	qv, err := e.client().EmbedQuery(q)
	if err != nil {
		e.t.Fatal(err)
	}
	return idx.Search(qv, q, e.cfg.Search, "all")
}

func ids(hits []search.Hit) []int64 {
	out := make([]int64, len(hits))
	for i, h := range hits {
		out[i] = h.ID
	}
	return out
}

func set(v []int64) map[int64]bool {
	out := map[int64]bool{}
	for _, x := range v {
		out[x] = true
	}
	return out
}

func sameSet(a, b []int64) bool {
	if len(a) != len(b) {
		return false
	}
	sa := set(a)
	for _, x := range b {
		if !sa[x] {
			return false
		}
	}
	return true
}

func (e *env) concept(c string) []int64 {
	var out []int64
	e.dsm.Edit(func(items map[int64]*testfake.Item) {
		for _, it := range items {
			if it.Concept == c && it.Owner == 1 {
				out = append(out, it.ID)
			}
		}
	})
	return out
}

func stats(t *testing.T, d *sql.DB) db.Stats {
	t.Helper()
	s, err := db.GetStats(d)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestIndexAndSearch(t *testing.T) {
	e := newEnv(t)
	ix, d := e.runOnce(true)
	st := stats(t, d)
	if st.Items != 20 || st.Embedded != 20 || st.OCRDone != 20 || st.WithText != 2 || st.OwnerUserID != "1" {
		t.Fatalf("stats = %+v", st)
	}
	if got := ids(e.search("cat", nil)); !sameSet(got, e.concept("cat")) { // 不含 bob 的 300
		t.Errorf("cat = %v", got)
	}
	if got := ids(e.search("beach", nil)); !sameSet(got, e.concept("beach")) {
		t.Errorf("beach = %v", got)
	}
	if got := e.search("volcano", nil); len(got) != 0 {
		t.Errorf("volcano = %v", got)
	}
	// OCR：長字串用 FTS、短字串用 LIKE
	if got := e.search("38475629", nil); len(got) != 1 || got[0].ID != 200 || got[0].Src != "ocr" {
		t.Errorf("38475629 = %+v", got)
	}
	// 某張照片的文字裡剛好有「cat」：OCR 命中是加進來，不能讓向量找到的貓照片消失
	e.dsm.Set(testfake.MakeItem(202, 1, T0+202*3600, "receipt", "cat food 2kg"))
	e.runOnce(false)
	if got := ids(e.search("cat", nil)); !sameSet(got, append(e.concept("cat"), 202)) {
		t.Errorf("cat + OCR = %v", got)
	}
	if got := ids(e.search("停車", nil)); len(got) != 1 || got[0] != 201 {
		t.Errorf("停車 = %v", got)
	}
	hits := e.search("dog", nil)
	if !sort.SliceIsSorted(hits, func(a, b int) bool { return hits[a].Time > hits[b].Time }) {
		t.Error("結果要依時間新到舊")
	}
	// 再跑一次不會重做
	before := e.llm.Count("embed_image")
	if err := ix.RunOnce(true); err != nil {
		t.Fatal(err)
	}
	if e.llm.Count("embed_image") != before {
		t.Error("再跑一次不應該重做 embedding")
	}
}

func TestChangesDeletesAndSafety(t *testing.T) {
	e := newEnv(t)
	ix, d := e.runOnce(true)
	idx := e.index()
	idx.Refresh()
	// 刪掉一張、改一張縮圖（cache_key 變了）、新增一張
	e.dsm.Delete(101)
	e.dsm.Edit(func(items map[int64]*testfake.Item) {
		items[102].MTime++
		items[102].Concept = "beach"
	})
	e.dsm.Set(testfake.MakeItem(400, 1, T0+999999, "dog", ""))
	if err := ix.RunOnce(true); err != nil {
		t.Fatal(err)
	}
	if set(ids(e.search("cat", idx)))[101] {
		t.Error("刪掉的照片不能出現")
	}
	if !set(ids(e.search("beach", idx)))[102] || !set(ids(e.search("dog", idx)))[400] {
		t.Error("改過、新增的照片要出現")
	}
	if idx.Items() != 20 {
		t.Errorf("增量更新後索引 %d 張", idx.Items())
	}
	// API 突然只回很少照片：不能把其他照片當成被刪除
	var saved map[int64]*testfake.Item
	e.dsm.Edit(func(items map[int64]*testfake.Item) {
		saved = map[int64]*testfake.Item{}
		for k, v := range items {
			saved[k] = v
			if k != 103 && k != 300 {
				delete(items, k)
			}
		}
	})
	if _, _, _, err := ix.FullScan(); err != nil {
		t.Fatal(err)
	}
	if st := stats(t, d); st.Deleted != 1 {
		t.Errorf("deleted = %d", st.Deleted)
	}
}

func TestSettingsChangeRequeues(t *testing.T) {
	e := newEnv(t)
	e.runOnce(true)
	calls := e.llm.Count("embed_image")
	e.writeCfg("alice", "pw-alice", map[string]map[string]any{"embedding": {"image_size": "xl"}})
	ix, d := e.runOnce(false)
	if n := e.llm.Count("embed_image") - calls; n != 20 { // 每張用新尺寸重做一次
		t.Errorf("重做 %d 張", n)
	}
	var models []string
	rows, _ := d.Query("SELECT DISTINCT model FROM vec")
	for rows.Next() {
		var m string
		rows.Scan(&m)
		models = append(models, m)
	}
	rows.Close()
	if len(models) != 1 || models[0] != "embeddinggemma-2@xl" {
		t.Errorf("models = %v", models)
	}
	ix.RunOnce(false)
	if n := e.llm.Count("embed_image") - calls; n != 20 {
		t.Errorf("之後不該再重做（%d）", n)
	}
}

func TestProgressFile(t *testing.T) {
	e := newEnv(t)
	e.runOnce(true)
	data, err := os.ReadFile(filepath.Join(e.dir, "data", "indexer-status.json"))
	if err != nil {
		t.Fatal(err)
	}
	var prog map[string]any
	json.Unmarshal(data, &prog)
	processed := prog["processed"].(map[string]any)
	if prog["phase"] != "done" || processed["emb"] != 20.0 || processed["ocr"] != 20.0 || prog["pid"] != float64(os.Getpid()) {
		t.Errorf("progress = %v", prog)
	}
}

func TestProgressRateAndPause(t *testing.T) {
	p := indexer.NewProgress(filepath.Join(t.TempDir(), "p.json"))
	now := time.Now()
	if p.Rate("emb", now) != nil {
		t.Error("沒有資料時沒有速度")
	}
	p.Set("paused", map[string]any{"paused_until": now.Unix() + 60, "pause_reason": "LLM 連不上"})
	read := func() map[string]any {
		data, _ := os.ReadFile(p.Path)
		var m map[string]any
		json.Unmarshal(data, &m)
		return m
	}
	if read()["pause_reason"] != "LLM 連不上" {
		t.Error("暫停原因要寫進檔案")
	}
	p.Set("embedding", nil)
	if read()["pause_reason"] != nil {
		t.Error("離開暫停要清掉原因")
	}
}

func TestTextSemanticSearch(t *testing.T) {
	e := newEnv(t)
	e.writeCfg("alice", "pw-alice", map[string]map[string]any{"search": {"text_min_score": 0.4}})
	// 文字寫著 invoice，搜 receipt：逐字比對找不到，只能靠照片文字的向量
	e.dsm.Set(testfake.MakeItem(700, 1, T0+700*3600, "screen", "invoice 2026-03"))
	ix, d := e.runOnce(true)
	if st := stats(t, d); st.WithText != 3 || st.TextEmbedded != 3 {
		t.Fatalf("stats = %+v", st)
	}
	var model string
	d.QueryRow("SELECT DISTINCT model FROM ocr_vec").Scan(&model)
	if model != "embeddinggemma-2@text2" {
		t.Errorf("model = %s", model)
	}
	got := map[int64]string{}
	for _, h := range e.search("receipt", nil) {
		got[h.ID] = h.Src
	}
	// 200 的圖片本身就是收據（圖片向量），700 是文字寫著 invoice 的截圖（文字向量）
	if len(got) != 2 || got[700] != "text" || got[200] != "vec" {
		t.Errorf("receipt = %v", got)
	}
	if !sameSet(ids(e.search("cat", nil)), e.concept("cat")) {
		t.Error("圖片搜尋不受影響")
	}
	// 照片換了：舊的文字與文字向量都作廢，重做
	e.dsm.Edit(func(items map[int64]*testfake.Item) {
		items[700].MTime++
		items[700].Text = "parking"
	})
	ix.RunOnce(true)
	if got := ids(e.search("receipt", nil)); len(got) != 1 || got[0] != 200 {
		t.Errorf("換了之後 receipt = %v", got)
	}
	// 照片刪了：文字向量一起刪
	e.dsm.Delete(700)
	ix.RunOnce(true)
	var n int
	d.QueryRow("SELECT count(*) FROM ocr_vec WHERE unit_id=700").Scan(&n)
	if n != 0 {
		t.Error("刪掉的照片的文字向量要一起刪")
	}
}

func TestShortTextNotEmbedded(t *testing.T) {
	e := newEnv(t)
	e.writeCfg("alice", "pw-alice", map[string]map[string]any{"search": {"text_min_score": 0.1}})
	e.dsm.Set(testfake.MakeItem(710, 1, T0, "statusbar", "12:30 / 88")) // 只有時間、數字
	ix, d := e.runOnce(true)
	var n int
	d.QueryRow("SELECT length(v) FROM ocr_vec WHERE unit_id=710").Scan(&n)
	if n != 0 {
		t.Error("只有數字的文字不做向量")
	}
	if c, _ := ix.TextEmbedPending(); c != 0 {
		t.Error("不會一直重試")
	}
	if set(ids(e.search("anything", nil)))[710] {
		t.Error("710 不該出現")
	}
}

func TestOldDatabaseWithoutOCRVec(t *testing.T) {
	e := newEnv(t)
	_, d := e.runOnce(true)
	d.Exec("DROP TABLE ocr_vec")
	idx := e.index()
	if n, err := idx.Refresh(); err != nil || n != 20 { // 搜尋服務照常載入圖片向量
		t.Fatalf("refresh = %d %v", n, err)
	}
	if !sameSet(ids(e.search("cat", idx)), e.concept("cat")) {
		t.Error("沒有 ocr_vec 時圖片搜尋照常")
	}
}

func TestManualTag(t *testing.T) {
	e := newEnv(t)
	_, d := e.runOnce(true)
	d.Exec("INSERT INTO tag(tag, unit_id) VALUES ('test', 103)")
	hits := e.search("TEST", nil) // 不分大小寫
	if len(hits) != 1 || hits[0].ID != 103 || hits[0].Src != "tag" {
		t.Errorf("tag = %+v", hits)
	}
	if !sameSet(ids(e.search("cat", nil)), e.concept("cat")) {
		t.Error("不影響其他搜尋")
	}
	e.dsm.Delete(103)
	e.runOnce(true)
	if got := e.search("test", nil); len(got) != 0 {
		t.Errorf("照片刪了就不再出現：%v", got)
	}
}

func TestIndexerYieldsToSearch(t *testing.T) {
	e := newEnv(t)
	ix, d := e.indexer()
	ix.YieldToSearch = 2
	os.MkdirAll(filepath.Dir(ix.Marker()), 0o755)
	os.WriteFile(ix.Marker(), nil, 0o644) // 剛有人搜尋
	start := time.Now()
	if err := ix.RunOnce(true); err != nil {
		t.Fatal(err)
	}
	if time.Since(start) < 1500*time.Millisecond {
		t.Error("要先等搜尋結束才送圖片")
	}
	if stats(t, d).Embedded != 20 {
		t.Error("embedding 沒做完")
	}
	os.Chtimes(ix.Marker(), time.Unix(0, 0), time.Unix(0, 0))
	if ix.SearchedRecently() {
		t.Error("很久以前的搜尋不算")
	}
}

func TestCompact(t *testing.T) {
	e := newEnv(t)
	ix, d := e.runOnce(true)
	// 模擬大量補做：照新到舊（id 由大到小）插入幾千筆向量，頁只會填一半
	rnd := rand.New(rand.NewSource(5))
	err := db.Tx(d, func(tx *sql.Tx) error {
		for uid := 6000; uid > 1000; uid-- {
			if _, err := tx.Exec("INSERT INTO item(unit_id, owner_user_id, time, type, cache_key, seen_scan, emb_state, rev) "+
				"VALUES (?,1,?,'photo','k',1,1,1)", uid, T0+uid); err != nil {
				return err
			}
			v := make([]float64, 768)
			for i := range v {
				v[i] = rnd.NormFloat64()
			}
			if _, err := tx.Exec("INSERT INTO vec VALUES (?,?,?)", uid, "m", f16.Encode(v)); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	beforeHits := ids(e.search("cat", nil))
	size, wasted, _ := db.SpaceStats(d)
	if float64(wasted) <= float64(size)*0.25 {
		t.Fatalf("測試資料沒有浪費空間：%d / %d", wasted, size)
	}
	before, after, done, err := ix.MaybeCompact()
	if err != nil || !done || float64(after) >= float64(before)*0.75 {
		t.Fatalf("compact = %d → %d %v %v", before, after, done, err)
	}
	var res map[string]any
	v, _, _ := db.GetMeta(d, "last_compact_result")
	json.Unmarshal([]byte(v), &res)
	if res["after"] != float64(after) {
		t.Errorf("last_compact_result = %v", res)
	}
	size2, wasted2, _ := db.SpaceStats(d)
	if float64(wasted2) >= float64(size2)*0.15 {
		t.Errorf("壓實後還浪費 %d / %d", wasted2, size2)
	}
	if st, err := os.Stat(e.cfg.Index.DB + "-wal"); err == nil && st.Size() >= 9*1024*1024 {
		t.Errorf("WAL 沒縮回去：%d", st.Size())
	}
	if got := ids(e.search("cat", nil)); fmt.Sprint(got) != fmt.Sprint(beforeHits) {
		t.Errorf("資料變了：%v → %v", beforeHits, got)
	}
	if got := ids(e.search("38475629", nil)); len(got) != 1 || got[0] != 200 {
		t.Error("OCR 全文索引要照常")
	}
	if _, _, done, _ := ix.MaybeCompact(); done {
		t.Error("一天最多檢查一次")
	}
	db.SetMeta(d, "last_compact_check", 0)
	if _, _, done, _ := ix.MaybeCompact(); done {
		t.Error("沒什麼好回收就不做")
	}
}

func TestQuickScanPicksUpNewPhoto(t *testing.T) {
	e := newEnv(t)
	ix, _ := e.runOnce(true)
	e.dsm.Set(testfake.MakeItem(500, 1, T0-99999, "mountain", "")) // 舊照片、剛加入
	if n, err := ix.QuickScan(); err != nil || n != 1 {
		t.Fatalf("quick scan = %d %v", n, err)
	}
	ix.RunOnce(false)
	if !set(ids(e.search("mountain", nil)))[500] {
		t.Error("快速掃描要找到新照片")
	}
}

func TestOwnerMismatchRefused(t *testing.T) {
	e := newEnv(t)
	e.runOnce(true)
	e.writeCfg("bob", "pw-bob", nil)
	ix, _ := e.indexer()
	err := ix.RunOnce(true)
	if _, ok := err.(*indexer.OwnerMismatchError); !ok {
		t.Errorf("要拒絕：%v", err)
	}
}

func TestTruncatedOCRKeepsPartialText(t *testing.T) {
	e := newEnv(t)
	e.dsm.Set(testfake.MakeItem(890, 1, T0, "screenshot", "LONG:第一行\n第二行 hello\n第二行 hello\n第三"))
	_, d := e.runOnce(true)
	if stats(t, d).OCRFailed != 0 {
		t.Error("被截斷不算失敗")
	}
	var text string
	d.QueryRow("SELECT text FROM ocr WHERE unit_id=890").Scan(&text)
	if text != "第一行\n第二行 hello\n第三" {
		t.Errorf("text = %q", text)
	}
	if got := ids(e.search("hello", nil)); len(got) != 1 || got[0] != 890 {
		t.Errorf("hello = %v", got)
	}
}

func TestServiceOutageDoesNotBurnRetries(t *testing.T) {
	e := newEnv(t)
	e.llm.SetFailOCR(true) // 模型服務回 500：服務問題
	ix, d := e.indexer()
	err := ix.RunOnce(true)
	if _, ok := err.(*indexer.ServiceDownError); !ok {
		t.Fatalf("要整體暫停：%v", err)
	}
	if st := stats(t, d); st.Embedded != 20 || st.OCRFailed != 0 || st.OCRPending != 20 {
		t.Errorf("stats = %+v", st)
	}
	e.llm.SetFailOCR(false)
	if err := ix.RunOnce(false); err != nil {
		t.Fatal(err)
	}
	if stats(t, d).OCRDone != 20 {
		t.Error("恢復後要做完")
	}
}

func TestGIFAndUnknownFormat(t *testing.T) {
	e := newEnv(t)
	for _, it := range []*testfake.Item{
		testfake.MakeItem(880, 1, T0, "gif", ""),         // 動圖：照樣 embed，跳過 OCR
		testfake.MakeItem(881, 1, T0, "weird", ""),       // 不認得的格式：只算這張的錯誤
		testfake.MakeItem(883, 1, T0, "undecodable", ""), // 模型服務回 500 但服務正常：也只算這張
		testfake.MakeItem(884, 1, T0, "heic-no-m", ""),   // 沒有 m 縮圖：改用 xl，照樣成功
	} {
		e.dsm.Set(it)
	}
	nothumb := testfake.MakeItem(882, 1, T0, "nothumb", "") // 縮圖 HTTP 404：也只算這張
	nothumb.Type = "video"
	e.dsm.Set(nothumb)
	_, d := e.runOnce(true) // 不會因為這幾張而整體暫停
	if st := stats(t, d); st.Embedded != 22 || st.EmbedFailed != 3 {
		t.Errorf("stats = %+v", st)
	}
	var text sql.NullString
	d.QueryRow("SELECT text FROM ocr WHERE unit_id=880").Scan(&text)
	if !text.Valid || text.String != "" {
		t.Errorf("GIF 的 OCR 要是空字串：%v", text)
	}
	if !set(ids(e.search("gif", nil)))[880] {
		t.Error("動圖要找得到")
	}
}

func TestBrokenPhotoRetriesThenGivesUp(t *testing.T) {
	e := newEnv(t)
	e.dsm.Set(testfake.MakeItem(777, 1, T0, "broken", "")) // 縮圖永遠拿不到
	_, d := e.runOnce(true)
	if st := stats(t, d); st.Embedded != 20 || st.EmbedFailed != 1 {
		t.Errorf("stats = %+v", st)
	}
	var tries int
	d.QueryRow("SELECT emb_tries FROM item WHERE unit_id=777").Scan(&tries)
	if tries != 3 {
		t.Errorf("tries = %d", tries)
	}
	if a, b, _ := db.RequeueFailed(d); a != 1 || b != 0 {
		t.Errorf("requeue = %d %d", a, b)
	}
}

// ── 搜尋服務 ──

func ask(t *testing.T, sock string, req string) map[string]any {
	t.Helper()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte(req + "\n"))
	line, err := bufio.NewReader(c).ReadBytes('\n')
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(line, &out); err != nil {
		t.Fatalf("%v：%s", err, line)
	}
	return out
}

func startServer(t *testing.T, e *env) (*server.Server, string) {
	t.Helper()
	srv := server.New(e.cfg, e.client())
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(stop) }()
	t.Cleanup(func() {
		close(stop)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("搜尋服務沒有停止")
		}
	})
	select {
	case <-srv.Ready():
	case err := <-done:
		t.Fatal(err)
	case <-time.After(10 * time.Second):
		t.Fatal("搜尋服務沒有啟動")
	}
	// 啟動後會逐一送出 hub 校正用的通用查詢：等送完，才不會跟下面的快取計數混在一起
	for end := time.Now().Add(10 * time.Second); !srv.Index.HasBank() && time.Now().Before(end); {
		time.Sleep(20 * time.Millisecond)
	}
	return srv, e.cfg.Search.Socket
}

func hitIDs(resp map[string]any) []int64 {
	var out []int64
	for _, h := range resp["hits"].([]any) {
		out = append(out, int64(h.(map[string]any)["id"].(float64)))
	}
	return out
}

func TestServerProtocolAndQueryCache(t *testing.T) {
	e := newEnv(t)
	e.runOnce(true)
	_, sock := startServer(t, e)
	if st, _ := os.Stat(sock); st.Mode().Perm() != 0o660 {
		t.Errorf("socket mode = %o", st.Mode().Perm())
	}
	ping := ask(t, sock, `{"op":"ping"}`)
	if ping["ok"] != true || ping["owner_user_id"] != 1.0 || ping["items"] != 20.0 {
		t.Errorf("ping = %v", ping)
	}
	r := ask(t, sock, `{"op":"search","q":"cat"}`)
	if !sameSet(hitIDs(r), e.concept("cat")) {
		t.Errorf("search = %v", r)
	}
	if st, err := os.Stat(db.SearchMarker(e.cfg.Index.DB)); err != nil || time.Since(st.ModTime()) > 5*time.Second {
		t.Error("要通知索引器有人在搜尋")
	}
	calls := e.llm.Count("embed_text")
	ask(t, sock, `{"op":"search","q":"cat"}`)
	if e.llm.Count("embed_text") != calls {
		t.Error("第二次要走快取")
	}
	for _, bad := range []string{`{"op":"search","q":""}`, `not json`, `[1]`, `{"op":"nope"}`} {
		if ask(t, sock, bad)["ok"] != false {
			t.Errorf("%s 要回 ok=false", bad)
		}
	}
	// 新照片在 refresh_interval 內出現在搜尋結果
	e.dsm.Set(testfake.MakeItem(600, 1, T0+10_000_000, "cat", ""))
	e.runOnce(false)
	time.Sleep(2500 * time.Millisecond)
	if !set(hitIDs(ask(t, sock, `{"op":"search","q":"cat"}`)))[600] {
		t.Error("新照片要在 refresh_interval 內出現")
	}
}

func TestServerStopsAndRemovesSocket(t *testing.T) {
	e := newEnv(t)
	srv := server.New(e.cfg, e.client())
	stop := make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- srv.Serve(stop) }()
	<-srv.Ready()
	close(stop)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(e.cfg.Search.Socket); err == nil {
		t.Error("停止時要移除 socket")
	}
}

// 索引器與搜尋服務同時在跑：embedding 與 OCR 各自一次只有一個請求、一個請求只有一筆
func TestOneRequestAtATime(t *testing.T) {
	e := newEnv(t)
	e.llm.Delay = 20 * time.Millisecond
	d, _ := db.Open(e.cfg.Index.DB)
	d.Close()
	_, sock := startServer(t, e)
	ix, d := e.indexer()
	ix.YieldToSearch = 0
	var wg sync.WaitGroup
	wg.Add(1)
	var runErr error
	go func() {
		defer wg.Done()
		runErr = ix.RunOnce(true)
	}()
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	n := 0
loop:
	for {
		select {
		case <-finished:
			break loop
		default:
			ask(t, sock, fmt.Sprintf(`{"op":"search","q":"query %d"}`, n))
			n++
		}
	}
	if runErr != nil {
		t.Fatal(runErr)
	}
	if n <= 3 {
		t.Errorf("只搜尋了 %d 次", n)
	}
	if stats(t, d).OCRDone != 20 {
		t.Error("OCR 沒做完")
	}
	if e.llm.MaxInflight["embed"] != 1 || e.llm.MaxInflight["ocr"] != 1 || e.llm.MaxInputs != 1 {
		t.Errorf("同時處理：%v，一個請求最多 %d 筆", e.llm.MaxInflight, e.llm.MaxInputs)
	}
}

// ── Select 與 hub 校正 ──

func TestSelectThresholdRules(t *testing.T) {
	s := []float32{0.72, 0.70, 0.66, 0.63, 0.55}
	check := func(got []int, want string) {
		t.Helper()
		sort.Ints(got)
		if fmt.Sprint(got) != want {
			t.Errorf("got %v, want %s", got, want)
		}
	}
	check(search.Select(s, 0.64, 0.06, 500), "[0 1 2]")
	check(search.Select(s, 0.64, 0.03, 500), "[0 1]")
	scaled := make([]float32, len(s))
	for i, x := range s {
		scaled[i] = x * 0.8
	}
	check(search.Select(scaled, 0.64, 0.06, 500), "[]") // 沒有夠像的就不回傳
	big := make([]float32, 5000)
	for i := range big {
		big[i] = 0.55
	}
	big = append(big, s...)
	check(search.Select(big, 0.64, 0.06, 2), "[5000 5001]") // max_results
	common := make([]float32, 0, 10000)
	for i := 0; i < 3000; i++ {
		common = append(common, 0.74)
	}
	for i := 0; i < 7000; i++ {
		common = append(common, 0.62)
	}
	if n := len(search.Select(common, 0.71, 0.08, 5000)); n != 3000 { // 常見主題不會被整體分布刷掉
		t.Errorf("common = %d", n)
	}
}

func TestPrepareMatchesPython(t *testing.T) {
	// 跟 numpy 版的 prepare 相同：前 dims 維、正規化、除以 max|v|/127 後四捨六入五成雙
	v := make([]float64, 768)
	for i := range v {
		v[i] = float64(i%7) - 3
	}
	p, ok := search.Prepare(f16.Encode(v), 4)
	if !ok {
		t.Fatal("prepare 失敗")
	}
	if fmt.Sprint(p.Vec) != "[-127 -85 -42 0]" {
		t.Errorf("vec = %v", p.Vec)
	}
	if _, ok := search.Prepare(f16.Encode(make([]float64, 768)), 512); ok {
		t.Error("全零的向量不能用")
	}
}

func TestHubCorrection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "i.db")
	d, err := db.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	rnd := rand.New(rand.NewSource(3))
	gauss := func() []float64 {
		v := make([]float64, 768)
		for i := range v {
			v[i] = rnd.NormFloat64()
		}
		return v
	}
	norm := func(v []float64) float64 {
		var s float64
		for _, x := range v {
			s += x * x
		}
		return math.Sqrt(s)
	}
	bank := [][]float64{gauss(), gauss(), gauss(), gauss(), gauss()}
	query := gauss()
	noise := gauss()
	hubby := make([]float64, 768) // 跟所有通用查詢都像
	target := make([]float64, 768)
	for i := range hubby {
		for _, b := range bank {
			hubby[i] += b[i] / 5
		}
		hubby[i] += 0.3 * noise[i]
		target[i] = query[i] + 0.6*gauss()[i]
	}
	qn, hn := norm(query), norm(hubby)
	for i := range hubby {
		hubby[i] += 0.55 * query[i] / qn * hn
	}
	db.Tx(d, func(tx *sql.Tx) error {
		for uid, v := range map[int64][]float64{1: hubby, 2: target} {
			n := norm(v)
			u := make([]float64, len(v))
			for i := range v {
				u[i] = v[i] / n
			}
			tx.Exec("INSERT INTO item(unit_id, owner_user_id, time, type, cache_key, seen_scan, emb_state, rev) VALUES (?,1,0,'photo','k',1,1,1)", uid)
			tx.Exec("INSERT INTO vec VALUES (?,?,?)", uid, "m", f16.Encode(u))
		}
		return db.SetMeta(tx, "rev", 1)
	})
	d.Close()
	idx := search.NewIndex(path, 512)
	defer idx.Close()
	idx.Refresh()
	scores := func() map[int64]float64 {
		top, _, _ := idx.Explain(query, 10, false)
		out := map[int64]float64{}
		for _, s := range top {
			out[s.ID] = s.Score
		}
		return out
	}
	raw := scores()
	idx.SetBank(bank)
	fixed := scores()
	if fixed[1]-raw[1] >= fixed[2]-raw[2] {
		t.Errorf("hub 照片要被扣分較多：raw %v fixed %v", raw, fixed)
	}
}
