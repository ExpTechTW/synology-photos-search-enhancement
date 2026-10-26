// Package search：搜尋。向量常駐記憶體，OCR 文字與照片資料交給 SQLite。
//
// 向量搜尋每次查詢都要看過每一張照片的向量，沒有「熱門資料」可言，所以不靠 SQLite 的 page cache，
// 而是把截短、量化後的向量整份放在記憶體（512 維 int8，每張 512 bytes + 4 bytes 縮放係數），比對時分給多個 CPU 核心。
// 圖片一組、照片裡的文字（OCR）一組，分數範圍不同，門檻分開設。
// OCR 文字比對、照片時間這些逐筆查詢才交給 SQLite（mmap + page cache）。
//
// hub 校正：有些照片（例如某些截圖）跟什麼查詢都很像，會出現在各種不相關的搜尋裡。每張照片先算出它對一組
// 通用查詢的平均分數（hubness），比對時扣掉它高於整體平均的部分。在真實 NAS 上以原生搜尋結果當參考答案，
// 同樣門檻下精確率多約 10 個百分點、召回率多約 8 個百分點。
// 對一組查詢的平均分數 = 跟這組查詢的平均向量的內積，所以每張照片只要算一次內積。
package search

import (
	"database/sql"
	"math"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/config"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/db"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/f16"
)

// HubBank 是通用查詢：只用來估計每張照片的 hubness，內容涵蓋常見主題即可。
var HubBank = []string{"人", "朋友", "自拍", "食物", "咖啡", "貓", "狗", "車", "城市", "夜景", "街道", "建築", "海", "山", "森林",
	"公園", "天空", "花", "樹", "運動", "旅行", "演唱會", "生日", "辦公室", "電腦", "手機", "書", "文件", "截圖",
	"地圖", "衣服", "超市", "寺廟", "海灘", "聖誕節", "煙火", "鳥", "魚", "people", "food", "car", "city",
	"screenshot", "document", "室內", "戶外", "桌子", "椅子", "窗戶", "門"}

// Prepared 是量化後的向量。
type Prepared struct {
	Vec   []int8
	Scale float32
}

// Prepare：f16 × 768 → 前 dims 維、重新正規化、int8 量化。向量不能用（長度為 0、有 NaN）時回傳 false。
func Prepare(blob []byte, dims int) (Prepared, bool) {
	if len(blob) < 2*dims {
		return Prepared{}, false
	}
	v := make([]float32, dims)
	f16.DecodeInto(v, blob)
	var sum float64
	for _, x := range v {
		sum += float64(x) * float64(x)
	}
	n := float32(math.Sqrt(sum))
	if n == 0 || math.IsNaN(float64(n)) || math.IsInf(float64(n), 0) {
		return Prepared{}, false
	}
	var peak float32
	for i := range v {
		v[i] /= n
		if a := float32(math.Abs(float64(v[i]))); a > peak {
			peak = a
		}
	}
	scale := peak / 127
	if scale == 0 {
		return Prepared{}, false
	}
	out := make([]int8, dims)
	for i, x := range v {
		out[i] = int8(math.RoundToEven(float64(x / scale)))
	}
	return Prepared{out, scale}, true
}

// PrepareQuery：查詢向量取前 dims 維並正規化。
func PrepareQuery(q []float64, dims int) []float32 {
	if len(q) < dims {
		dims = len(q)
	}
	var sum float64
	for _, x := range q[:dims] {
		sum += x * x
	}
	n := math.Sqrt(sum)
	out := make([]float32, dims)
	for i := range out {
		if n > 0 {
			out[i] = float32(q[i] / n)
		}
	}
	return out
}

// Select：分數 → 要列入結果的列號。Synology 的搜尋結果是照時間排的，所以要決定「哪些算命中」，不是排名。
//
// 不用「平均 + N 個標準差」：常見的主題（例如貓佔了一成以上的照片）會把平均與標準差一起拉高，
// 結果所有照片都被刷掉（真實 NAS 上「貓」因此一張都沒選到）。
func Select(scores []float32, minScore, maxDrop float64, maxResults int) []int {
	if len(scores) == 0 {
		return nil
	}
	best := scores[0]
	for _, s := range scores {
		if s > best {
			best = s
		}
	}
	cut := float32(math.Max(minScore, float64(best)-maxDrop))
	var idx []int
	for i, s := range scores {
		if s >= cut {
			idx = append(idx, i)
		}
	}
	if len(idx) > maxResults {
		sort.SliceStable(idx, func(a, b int) bool { return scores[idx[a]] > scores[idx[b]] })
		idx = idx[:maxResults]
	}
	return idx
}

// vecSet 是一組常駐記憶體的向量：int8 × dims、每列一個縮放係數，加上 hub 校正。圖片與照片裡的文字各一組。
type vecSet struct {
	dims  int
	ids   []int64
	mat   []int8 // 每 dims 個一列
	scale []float32
	hub   []float32
	pos   map[int64]int
}

func newVecSet(dims int) *vecSet { return &vecSet{dims: dims, pos: map[int64]int{}} }

func (s *vecSet) row(k int) []int8 { return s.mat[k*s.dims : (k+1)*s.dims] }

func (s *vecSet) hubness(k int, bankMean []float32) float32 {
	if bankMean == nil {
		return 0
	}
	return dot(s.row(k), bankMean) * s.scale[k]
}

func (s *vecSet) rehub(bankMean []float32) {
	for k := range s.ids {
		s.hub[k] = s.hubness(k, bankMean)
	}
}

// update：changes 是 unit_id → 量化後的向量，nil 表示移除。
func (s *vecSet) update(changes map[int64]*Prepared, bankMean []float32) {
	removed := false
	for uid, p := range changes {
		k, ok := s.pos[uid]
		switch {
		case p == nil:
			if ok {
				s.ids[k] = -1 // 最後一起移除
				delete(s.pos, uid)
				removed = true
			}
		case ok:
			copy(s.row(k), p.Vec)
			s.scale[k] = p.Scale
			s.hub[k] = s.hubness(k, bankMean)
		default:
			s.pos[uid] = len(s.ids)
			s.ids = append(s.ids, uid)
			s.mat = append(s.mat, p.Vec...)
			s.scale = append(s.scale, p.Scale)
			s.hub = append(s.hub, 0)
			s.hub[len(s.hub)-1] = s.hubness(len(s.ids)-1, bankMean)
		}
	}
	if !removed {
		return
	}
	w := 0
	for k, uid := range s.ids {
		if uid < 0 {
			continue
		}
		if w != k {
			s.ids[w], s.scale[w], s.hub[w] = uid, s.scale[k], s.hub[k]
			copy(s.mat[w*s.dims:(w+1)*s.dims], s.row(k))
		}
		s.pos[uid] = w
		w++
	}
	s.ids, s.scale, s.hub, s.mat = s.ids[:w], s.scale[:w], s.hub[:w], s.mat[:w*s.dims]
}

// scores 回傳每張照片 hub 校正後的分數（跟 ids 對應）。呼叫端要持有讀鎖。
func (s *vecSet) scores(qv []float32, banked bool) []float32 {
	n := len(s.ids)
	out := make([]float32, n)
	workers := runtime.GOMAXPROCS(0)
	if n < 4096 || workers < 2 {
		s.scoreRange(qv, out, 0, n)
	} else {
		var wg sync.WaitGroup
		chunk := (n + workers - 1) / workers
		for lo := 0; lo < n; lo += chunk {
			hi := min(lo+chunk, n)
			wg.Add(1)
			go func() {
				defer wg.Done()
				s.scoreRange(qv, out, lo, hi)
			}()
		}
		wg.Wait()
	}
	if banked && n > 0 {
		var sum float64
		for _, h := range s.hub {
			sum += float64(h)
		}
		mean := float32(sum / float64(n))
		for k := range out {
			out[k] -= s.hub[k] - mean
		}
	}
	return out
}

func (s *vecSet) scoreRange(qv, out []float32, lo, hi int) {
	for k := lo; k < hi; k++ {
		out[k] = dot(s.row(k), qv) * s.scale[k]
	}
}

func dot(a []int8, b []float32) float32 {
	var s0, s1, s2, s3 float32
	i := 0
	for ; i+4 <= len(a); i += 4 {
		s0 += float32(a[i]) * b[i]
		s1 += float32(a[i+1]) * b[i+1]
		s2 += float32(a[i+2]) * b[i+2]
		s3 += float32(a[i+3]) * b[i+3]
	}
	for ; i < len(a); i++ {
		s0 += float32(a[i]) * b[i]
	}
	return s0 + s1 + s2 + s3
}

type meta struct {
	time int64
	typ  string
}

// Index 是搜尋服務的索引。可以同時搜尋；Refresh 只在套用變動時短暫獨佔。
type Index struct {
	dbPath string
	dims   int

	mu       sync.RWMutex
	rev      int64
	img, txt *vecSet
	bankMean []float32 // 通用查詢的平均向量，有了才做 hub 校正
	meta     map[int64]meta
	owner    *int64

	connMu sync.Mutex
	conn   *sql.DB
}

// NewIndex 建立空的索引（Refresh 才會載入）。
func NewIndex(dbPath string, dims int) *Index {
	return &Index{dbPath: dbPath, dims: dims, rev: -1, img: newVecSet(dims), txt: newVecSet(dims), meta: map[int64]meta{}}
}

func (x *Index) db() (*sql.DB, error) {
	x.connMu.Lock()
	defer x.connMu.Unlock()
	if x.conn == nil {
		c, err := db.OpenReadOnly(x.dbPath)
		if err != nil {
			return nil, err
		}
		x.conn = c
	}
	return x.conn, nil
}

// Close 關閉資料庫連線。
func (x *Index) Close() {
	x.connMu.Lock()
	defer x.connMu.Unlock()
	if x.conn != nil {
		x.conn.Close()
		x.conn = nil
	}
}

// Items 是有圖片向量的照片張數。
func (x *Index) Items() int {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return len(x.img.ids)
}

// Rev 是目前載入到的資料庫版本。
func (x *Index) Rev() int64 {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.rev
}

// Owner 是索引所屬的使用者 id（還沒有時回傳 nil）。
func (x *Index) Owner() *int64 {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.owner
}

// HasBank：hub 校正是否已啟用。
func (x *Index) HasBank() bool {
	x.mu.RLock()
	defer x.mu.RUnlock()
	return x.bankMean != nil
}

// SetBank 設定通用查詢的向量並重算每張照片的 hubness。
func (x *Index) SetBank(vectors [][]float64) {
	if len(vectors) == 0 {
		return
	}
	mean := make([]float32, x.dims)
	for _, v := range vectors {
		for i, f := range PrepareQuery(v, x.dims) {
			mean[i] += f / float32(len(vectors))
		}
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	x.bankMean = mean
	x.img.rehub(mean)
	x.txt.rehub(mean)
}

// Refresh 讀取 rev 之後有變動的照片（第一次呼叫時載入全部），回傳讀到的筆數。
// 讀資料庫與量化都在鎖外面做，搜尋不會被擋住；一邊讀一邊量化，不會把原始向量全部放進記憶體。
func (x *Index) Refresh() (int, error) {
	c, err := x.db()
	if err != nil {
		return 0, err
	}
	rev := db.MetaInt(c, "rev", 0)
	if rev == x.Rev() {
		return 0, nil
	}
	ownerStr, hasOwner, _ := db.GetMeta(c, "owner_user_id")
	from := x.Rev()
	rows, err := c.Query("SELECT i.unit_id, i.time, i.type, i.deleted, i.emb_state, v.v, t.v FROM item i "+
		"LEFT JOIN vec v USING(unit_id) LEFT JOIN ocr_vec t USING(unit_id) WHERE i.rev > ?", from)
	withText := true
	if err != nil && strings.Contains(err.Error(), "no such table: ocr_vec") {
		// 舊版資料庫還沒有 ocr_vec（索引器下次啟動會建立）
		withText = false
		rows, err = c.Query("SELECT i.unit_id, i.time, i.type, i.deleted, i.emb_state, v.v FROM item i "+
			"LEFT JOIN vec v USING(unit_id) WHERE i.rev > ?", from)
	}
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	img, txt := map[int64]*Prepared{}, map[int64]*Prepared{}
	metas := map[int64]*meta{}
	n := 0
	for rows.Next() {
		var uid, t, deleted, emb int64
		var typ string
		var blob, tblob sql.RawBytes // 不複製：量化完就不需要原始向量
		dest := []any{&uid, &t, &typ, &deleted, &emb, &blob}
		if withText {
			dest = append(dest, &tblob)
		}
		if err := rows.Scan(dest...); err != nil {
			return 0, err
		}
		n++
		img[uid], txt[uid], metas[uid] = nil, nil, nil
		if deleted != 0 || emb != db.Done || len(blob) == 0 {
			continue
		}
		p, ok := Prepare(blob, x.dims)
		if !ok {
			continue
		}
		img[uid] = &p
		metas[uid] = &meta{t, typ}
		if len(tblob) > 0 {
			if tp, ok := Prepare(tblob, x.dims); ok {
				txt[uid] = &tp
			}
		}
	}
	if err := rows.Err(); err != nil {
		return 0, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	for uid, m := range metas {
		if m == nil {
			delete(x.meta, uid)
		} else {
			x.meta[uid] = *m
		}
	}
	x.img.update(img, x.bankMean)
	x.txt.update(txt, x.bankMean)
	x.rev = rev
	x.owner = nil
	if hasOwner {
		if id, ok := parseInt(ownerStr); ok {
			x.owner = &id
		}
	}
	return n, nil
}

// TextHits：OCR 文字裡出現搜尋字的照片。3 個字以上用 FTS（trigram），更短的用 LIKE。
func (x *Index) TextHits(q string, limit int) ([]int64, error) {
	q = strings.TrimSpace(q)
	if q == "" {
		return nil, nil
	}
	c, err := x.db()
	if err != nil {
		return nil, err
	}
	var rows *sql.Rows
	if utf8.RuneCountInString(q) >= 3 {
		rows, err = c.Query("SELECT rowid FROM ocr_fts WHERE ocr_fts MATCH ? LIMIT ?",
			`"`+strings.ReplaceAll(q, `"`, `""`)+`"`, limit)
	} else {
		esc := strings.NewReplacer(`\`, `\\`, "%", `\%`, "_", `\_`).Replace(q)
		rows, err = c.Query(`SELECT unit_id FROM ocr WHERE text LIKE ? ESCAPE '\' LIMIT ?`, "%"+esc+"%", limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// Casefold：標籤一律存小寫、比對時不分大小寫。
func Casefold(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

type tagHit struct {
	id, time int64
	typ      string
}

// tagHits：手動標籤完全相同的照片。不需要有向量（例如縮圖做不出來的照片也能標）。
func (x *Index) tagHits(q string) []tagHit {
	q = Casefold(q)
	if q == "" {
		return nil
	}
	c, err := x.db()
	if err != nil {
		return nil
	}
	rows, err := c.Query("SELECT t.unit_id, i.time, i.type FROM tag t JOIN item i USING(unit_id) "+
		"WHERE t.tag=? AND i.deleted=0", q)
	if err != nil {
		return nil // 舊版資料庫還沒有 tag 表（索引器下次啟動會建立）
	}
	defer rows.Close()
	var out []tagHit
	for rows.Next() {
		var h tagHit
		if rows.Scan(&h.id, &h.time, &h.typ) == nil {
			out = append(out, h)
		}
	}
	return out
}

// TagHits 回傳標了這個標籤的照片 id。
func (x *Index) TagHits(q string) []int64 {
	var out []int64
	for _, h := range x.tagHits(q) {
		out = append(out, h.id)
	}
	return out
}

// Hit 是一筆搜尋結果（JSON 欄位跟 Python 版相同，filter 會讀）。
type Hit struct {
	ID    int64   `json:"id"`
	Time  int64   `json:"time"`
	Type  string  `json:"type"`
	Score float64 `json:"score"`
	Src   string  `json:"src"` // vec 圖片、text 照片文字的語意、ocr 文字逐字、tag 標籤
}

// Search 回傳搜尋結果，依時間新到舊。
func (x *Index) Search(qvec []float64, keyword string, sc config.Search, itemType string) []Hit {
	tags := x.tagHits(keyword)
	var text []int64
	if sc.OCRMatch {
		text, _ = x.TextHits(keyword, sc.MaxResults)
	}
	// 標籤、OCR 文字命中只是「加進來」，不影響向量結果的門檻：常見的字幾乎都會出現在某張照片的文字裡
	// （例如一張寫著「貓乾糧」的包裝照片，會讓「貓」的向量結果幾乎全部消失）
	qv := PrepareQuery(qvec, x.dims)
	type hit struct {
		score float32
		src   string
		m     meta
		ok    bool
	}
	hits := map[int64]hit{}
	x.mu.RLock()
	banked := x.bankMean != nil
	scores := x.img.scores(qv, banked)
	for _, k := range Select(scores, sc.MinScore, sc.MaxDrop, sc.MaxResults) {
		uid := x.img.ids[k]
		m, ok := x.meta[uid]
		hits[uid] = hit{scores[k], "vec", m, ok}
	}
	// 照片裡文字的語意（例如搜「收據」找到文字寫著「電子發票」的截圖）
	scores = x.txt.scores(qv, banked)
	for _, k := range Select(scores, sc.TextMinScore, sc.TextMaxDrop, sc.MaxResults) {
		uid := x.txt.ids[k]
		if _, dup := hits[uid]; !dup {
			m, ok := x.meta[uid]
			hits[uid] = hit{scores[k], "text", m, ok}
		}
	}
	for _, uid := range text {
		if m, ok := x.meta[uid]; ok {
			hits[uid] = hit{1, "ocr", m, true}
		}
	}
	x.mu.RUnlock()
	for _, h := range tags {
		hits[h.id] = hit{1, "tag", meta{h.time, h.typ}, true}
	}
	out := make([]Hit, 0, len(hits))
	for uid, h := range hits {
		if !h.ok || (itemType != "" && itemType != "all" && h.m.typ != itemType) {
			continue
		}
		out = append(out, Hit{ID: uid, Time: h.m.time, Type: h.m.typ, Score: math.Round(float64(h.score)*1e4) / 1e4, Src: h.src})
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Time != out[b].Time {
			return out[a].Time > out[b].Time
		}
		return out[a].ID > out[b].ID
	})
	if len(out) > sc.MaxResults {
		out = out[:sc.MaxResults]
	}
	return out
}

// Scored 是一張照片的分數。
type Scored struct {
	ID    int64
	Score float64
}

// Explain 回傳分數最高的 top 張，以及全部分數的平均與標準差（sse search --explain 用）。
func (x *Index) Explain(qvec []float64, top int, text bool) ([]Scored, float64, float64) {
	qv := PrepareQuery(qvec, x.dims)
	x.mu.RLock()
	set := x.img
	if text {
		set = x.txt
	}
	scores := set.scores(qv, x.bankMean != nil)
	ids := append([]int64(nil), set.ids...)
	x.mu.RUnlock()
	if len(scores) == 0 {
		return nil, 0, 0
	}
	order := make([]int, len(scores))
	var sum float64
	for i, s := range scores {
		order[i] = i
		sum += float64(s)
	}
	mean := sum / float64(len(scores))
	var sq float64
	for _, s := range scores {
		sq += (float64(s) - mean) * (float64(s) - mean)
	}
	sort.Slice(order, func(a, b int) bool { return scores[order[a]] > scores[order[b]] })
	if len(order) > top {
		order = order[:top]
	}
	out := make([]Scored, len(order))
	for i, k := range order {
		out[i] = Scored{ids[k], float64(scores[k])}
	}
	return out, mean, math.Sqrt(sq / float64(len(scores)))
}

// DB 回傳唯讀連線（指令列查 OCR 文字用）。
func (x *Index) DB() (*sql.DB, error) { return x.db() }

func parseInt(s string) (int64, bool) {
	n, err := strconv.ParseInt(s, 10, 64)
	return n, err == nil
}
