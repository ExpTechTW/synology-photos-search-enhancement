// Package testfake：測試用的假 DSM 與假模型服務（只給測試用）。
//
// 假圖片的內容是 "FAKE:<概念>|TEXT:<照片裡的文字>"。假模型服務的 embedding：每個詞對應一個固定的隨機方向，
// 圖片的向量就是它概念的方向；查詢「<詞>」是同方向加一點雜訊（相似度約 0.8），不相關的詞相似度接近 0。
// OCR 回傳圖片裡 TEXT: 後面的文字，沒有就回 NONE。
package testfake

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// T0 是測試照片的基準時間。
const T0 = 1700000000

// Item 是假 DSM 裡的一張照片。
type Item struct {
	ID, Owner, Time int64
	Type, Concept   string
	Text            string // 照片裡的文字（假 OCR 會讀出來）
	MTime           int64  // 縮圖版本（cache_key 的一部分）
}

// MakeItem 建立一張照片。
func MakeItem(id, owner, t int64, concept, text string) *Item {
	return &Item{ID: id, Owner: owner, Time: t, Type: "photo", Concept: concept, Text: text, MTime: t + 7}
}

// CacheKey 是這張照片目前的縮圖版本。
func (it *Item) CacheKey() string { return fmt.Sprintf("%d_%d", it.ID, it.MTime) }

// EngineLibrary：alice（id 1）有各種內容的照片，bob（id 2）有一張貓。
func EngineLibrary() []*Item {
	var items []*Item
	uid := int64(100)
	for _, c := range []struct {
		concept string
		n       int
	}{{"cat", 5}, {"dog", 4}, {"beach", 6}, {"mountain", 3}} {
		for k := 0; k < c.n; k++ {
			uid++
			items = append(items, MakeItem(uid, 1, T0+uid*3600, c.concept, ""))
		}
	}
	items = append(items, MakeItem(200, 1, T0+200*3600, "receipt", "全家便利商店\nAB-38475629\n總計 NT$91"))
	items = append(items, MakeItem(201, 1, T0+201*3600, "sign", "停車場入口"))
	items = append(items, MakeItem(300, 2, T0, "cat", ""))
	return items
}

// FakeImage 是假圖片的內容。
func FakeImage(concept, text string) []byte {
	return append([]byte("\xff\xd8\xff"), []byte("FAKE:"+concept+"|TEXT:"+text)...)
}

func parseFake(img []byte) (concept, text string) {
	skip := 3
	if bytes.HasPrefix(img, []byte("GIF")) {
		skip = 6
	}
	if len(img) < skip {
		return "", ""
	}
	s := string(img[skip:])
	concept, text, _ = strings.Cut(s, "|TEXT:")
	return strings.TrimPrefix(concept, "FAKE:"), text
}

var users = map[string]struct {
	id int64
	pw string
}{"alice": {1, "pw-alice"}, "bob": {2, "pw-bob"}}

// DSM 是假的 Synology Photos WebAPI。
type DSM struct {
	Srv *httptest.Server

	mu       sync.Mutex
	Items    map[int64]*Item
	sessions map[string]int64
}

// NewDSM 啟動假 DSM。
func NewDSM(items []*Item) *DSM {
	d := &DSM{Items: map[int64]*Item{}, sessions: map[string]int64{}}
	for _, it := range items {
		d.Items[it.ID] = it
	}
	d.Srv = httptest.NewServer(http.HandlerFunc(d.handle))
	return d
}

// URL 是假 DSM 的網址。
func (d *DSM) URL() string { return d.Srv.URL }

// Close 停止假 DSM。
func (d *DSM) Close() { d.Srv.Close() }

// Set 新增或取代一張照片。
func (d *DSM) Set(it *Item) {
	d.mu.Lock()
	d.Items[it.ID] = it
	d.mu.Unlock()
}

// Delete 刪掉一張照片。
func (d *DSM) Delete(id int64) {
	d.mu.Lock()
	delete(d.Items, id)
	d.mu.Unlock()
}

// Edit 在鎖裡修改照片。
func (d *DSM) Edit(fn func(items map[int64]*Item)) {
	d.mu.Lock()
	fn(d.Items)
	d.mu.Unlock()
}

func (d *DSM) owned(uid int64, byID bool) []*Item {
	var out []*Item
	for _, it := range d.Items {
		if it.Owner == uid {
			out = append(out, it)
		}
	}
	sort.Slice(out, func(a, b int) bool {
		if byID {
			return out[a].ID > out[b].ID
		}
		if out[a].Time != out[b].Time {
			return out[a].Time > out[b].Time
		}
		return out[a].ID > out[b].ID
	})
	return out
}

func (it *Item) serialize() map[string]any {
	return map[string]any{"id": it.ID, "owner_user_id": it.Owner, "time": it.Time, "type": it.Type,
		"filename": fmt.Sprintf("IMG_%d.jpg", it.ID),
		"additional": map[string]any{"thumbnail": map[string]any{"cache_key": it.CacheKey(), "m": "ready",
			"sm": "ready", "xl": "ready", "unit_id": it.ID}}}
}

func ok(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"data": data, "success": true})
}

func fail(w http.ResponseWriter, code int) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{"code": code}, "success": false})
}

func (d *DSM) handle(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	p := r.Form
	d.mu.Lock()
	defer d.mu.Unlock()
	api, method := p.Get("api"), p.Get("method")
	switch {
	case api == "SYNO.API.Auth" && method == "login":
		u, found := users[p.Get("account")]
		if !found || p.Get("passwd") != u.pw {
			fail(w, 400)
			return
		}
		sid := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s%d", p.Get("account"), len(d.sessions)))))[:20]
		d.sessions[sid] = u.id
		ok(w, map[string]any{"sid": sid})
		return
	case api == "SYNO.API.Auth" && method == "logout":
		ok(w, map[string]any{})
		return
	}
	uid, found := d.sessions[p.Get("_sid")]
	if !found {
		fail(w, 119)
		return
	}
	offset, _ := strconv.Atoi(p.Get("offset"))
	limit, _ := strconv.Atoi(p.Get("limit"))
	page := func(items []*Item) []any {
		out := []any{}
		for i := offset; i < len(items) && i < offset+limit; i++ {
			out = append(out, items[i].serialize())
		}
		return out
	}
	switch {
	case api == "SYNO.Foto.UserInfo" && method == "me":
		for name, u := range users {
			if u.id == uid {
				ok(w, map[string]any{"id": uid, "name": name})
			}
		}
	case api == "SYNO.Foto.Browse.Item" && method == "count":
		ok(w, map[string]any{"count": len(d.owned(uid, false))})
	case api == "SYNO.Foto.Browse.Item" && method == "list":
		ok(w, map[string]any{"list": page(d.owned(uid, false))})
	case api == "SYNO.Foto.Browse.RecentlyAdded" && method == "list":
		ok(w, map[string]any{"list": page(d.owned(uid, true))})
	case api == "SYNO.Foto.Thumbnail" && method == "get":
		id, _ := strconv.ParseInt(p.Get("id"), 10, 64)
		it := d.Items[id]
		if it == nil || it.Owner != uid || p.Get("cache_key") != it.CacheKey() || it.Concept == "broken" {
			fail(w, 803)
			return
		}
		if it.Concept == "nothumb" || (it.Concept == "heic-no-m" && p.Get("size") == "m") {
			http.NotFound(w, r)
			return
		}
		switch it.Concept {
		case "gif":
			w.Header().Set("Content-Type", "image/gif")
			w.Write(append([]byte("GIF89a"), FakeImage("gif", "")[3:]...))
		case "weird":
			w.Header().Set("Content-Type", "image/heic")
			w.Write([]byte("\x00\x00\x00\x18ftypheic"))
		default:
			w.Header().Set("Content-Type", "image/jpeg")
			w.Write(FakeImage(it.Concept, it.Text))
		}
	default:
		fail(w, 103)
	}
}

// ── 假模型服務 ──

const dim = 768

var directions sync.Map

// Direction 是一個詞固定的隨機方向（單位向量）。
func Direction(word string) []float64 {
	if v, ok := directions.Load(word); ok {
		return v.([]float64)
	}
	h := sha256.Sum256([]byte(word))
	rnd := rand.New(rand.NewSource(int64(binary.LittleEndian.Uint64(h[:8]))))
	v := make([]float64, dim)
	for i := range v {
		v[i] = rnd.NormFloat64()
	}
	v = unit(v)
	directions.Store(word, v)
	return v
}

func unit(v []float64) []float64 {
	var s float64
	for _, x := range v {
		s += x * x
	}
	n := math.Sqrt(s)
	out := make([]float64, len(v))
	for i, x := range v {
		out[i] = x / n
	}
	return out
}

// 讓假的語意比對有「意思相近但字不同」的例子：文字裡的 invoice 跟查詢 receipt 同方向
var synonyms = map[string]string{"invoice": "receipt"}

const (
	queryPrefix = "task: search result | query: "
	docPrefix   = "title: none | text: "
)

func embedOne(inp any) []float64 {
	if m, ok := inp.(map[string]any); ok {
		data, _ := m["multimodal_data"].([]any)
		b, _ := base64.StdEncoding.DecodeString(data[0].(string))
		concept, _ := parseFake(b)
		return Direction(concept)
	}
	s := inp.(string)
	if strings.HasPrefix(s, docPrefix) {
		// 文件（照片裡的文字）：每個詞的方向加起來，所以「cat food」跟查詢「cat」有一定相似度
		sum := make([]float64, dim)
		words := strings.Fields(strings.ReplaceAll(s[len(docPrefix):], "/", " "))
		if len(words) == 0 {
			words = []string{"empty"}
		}
		for _, w := range words {
			if syn, ok := synonyms[w]; ok {
				w = syn
			}
			for i, x := range Direction(w) {
				sum[i] += x
			}
		}
		return unit(sum)
	}
	q := strings.TrimPrefix(s, queryPrefix)
	d, n := Direction(q), Direction("noise:"+q)
	v := make([]float64, dim)
	for i := range v {
		v[i] = 0.8*d[i] + 0.6*n[i]
	}
	return unit(v)
}

// LLM 是假的 OpenAI 相容服務。
type LLM struct {
	Srv *httptest.Server

	mu          sync.Mutex
	Calls       map[string]int // embed_text、embed_image、ocr
	FailOCR     bool
	Delay       time.Duration
	inflight    map[string]int
	MaxInflight map[string]int // 同時處理的請求數的最大值
	MaxInputs   int            // 一個 embedding 請求最多帶了幾筆
}

// NewLLM 啟動假模型服務。
func NewLLM() *LLM {
	l := &LLM{Calls: map[string]int{}, inflight: map[string]int{}, MaxInflight: map[string]int{}}
	l.Srv = httptest.NewServer(http.HandlerFunc(l.handle))
	return l
}

// URL 是 OpenAI 相容的 base URL。
func (l *LLM) URL() string { return l.Srv.URL + "/v1" }

// Close 停止假模型服務。
func (l *LLM) Close() { l.Srv.Close() }

// Count 回傳某種請求的次數。
func (l *LLM) Count(kind string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Calls[kind]
}

// SetFailOCR 讓 OCR 回 HTTP 500（模擬服務掛掉）。
func (l *LLM) SetFailOCR(v bool) {
	l.mu.Lock()
	l.FailOCR = v
	l.mu.Unlock()
}

func (l *LLM) handle(w http.ResponseWriter, r *http.Request) {
	kind := "ocr"
	if strings.HasSuffix(r.URL.Path, "/embeddings") {
		kind = "embed"
	}
	var body map[string]any
	json.NewDecoder(r.Body).Decode(&body)
	l.mu.Lock()
	l.inflight[kind]++
	l.MaxInflight[kind] = max(l.MaxInflight[kind], l.inflight[kind])
	if inputs, ok := body["input"].([]any); ok {
		l.MaxInputs = max(l.MaxInputs, len(inputs))
	}
	delay := l.Delay
	l.mu.Unlock()
	defer func() {
		l.mu.Lock()
		l.inflight[kind]--
		l.mu.Unlock()
	}()
	time.Sleep(delay)
	out, ok := l.respond(kind, body)
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		w.WriteHeader(500)
		json.NewEncoder(w).Encode(map[string]any{"error": "fake failure"})
		return
	}
	json.NewEncoder(w).Encode(out)
}

func (l *LLM) respond(kind string, body map[string]any) (any, bool) {
	if kind == "embed" {
		inputs, _ := body["input"].([]any)
		for _, x := range inputs {
			if m, ok := x.(map[string]any); ok {
				data, _ := m["multimodal_data"].([]any)
				b, _ := base64.StdEncoding.DecodeString(data[0].(string))
				if c, _ := parseFake(b); c == "undecodable" {
					return nil, false // 跟 llama.cpp 解不開圖片時一樣回 500，但服務本身正常
				}
			}
		}
		var data []any
		l.mu.Lock()
		for i, x := range inputs {
			if _, img := x.(map[string]any); img {
				l.Calls["embed_image"]++
			} else {
				l.Calls["embed_text"]++
			}
			data = append(data, map[string]any{"index": i, "embedding": embedOne(x)})
		}
		l.mu.Unlock()
		return map[string]any{"data": data, "model": body["model"]}, true
	}
	l.mu.Lock()
	l.Calls["ocr"]++
	failing := l.FailOCR
	l.mu.Unlock()
	if failing {
		return nil, false
	}
	msgs, _ := body["messages"].([]any)
	content := msgs[0].(map[string]any)["content"].([]any)
	u := content[0].(map[string]any)["image_url"].(map[string]any)["url"].(string)
	b, _ := base64.StdEncoding.DecodeString(u[strings.Index(u, ",")+1:])
	_, text := parseFake(b)
	if strings.HasPrefix(text, "LONG:") { // 文字很多：輸出到上限被截斷
		return map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": text[5:]},
			"finish_reason": "length"}}}, true
	}
	if text == "" {
		text = "NONE"
	}
	return map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": text},
		"finish_reason": "stop"}}}, true
}

// Values 是給 url.Values 用的小工具（測試裡組 DSM 請求）。
func Values(kv ...string) url.Values {
	v := url.Values{}
	for i := 0; i+1 < len(kv); i += 2 {
		v.Set(kv[i], kv[i+1])
	}
	return v
}
