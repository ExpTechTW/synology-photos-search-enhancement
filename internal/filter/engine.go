package filter

import (
	"bufio"
	"container/list"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// skipError：放棄注入、原樣轉發（不算錯誤）。
type skipError struct{ msg string }

func (e *skipError) Error() string { return e.msg }

func skipf(format string, a ...any) error { return &skipError{fmt.Sprintf(format, a...)} }

type injector interface {
	wants(p *plan) bool
	// rewrite 回傳改寫後的回應 JSON；不需要改就回傳 nil。
	rewrite(ctx context.Context, p *plan, data []byte) ([]byte, error)
}

// ── demo ────────────────────────────────────────────────────────────────

type demoInjector struct{ demo *demoConfig }

func (d *demoInjector) wants(p *plan) bool { return strings.EqualFold(p.keyword, d.demo.keyword) }

func (d *demoInjector) rewrite(_ context.Context, p *plan, data []byte) ([]byte, error) {
	return injectDemo(p.method, p.params, data, d.demo.items), nil
}

// ── cache ───────────────────────────────────────────────────────────────

// ttlCache 是有存活時間與筆數上限的 cache（滿了丟掉最早放入的）。
type ttlCache[K comparable, V any] struct {
	mu    sync.Mutex
	ttl   time.Duration
	size  int
	order *list.List // 前面是最早放入的
	items map[K]*list.Element
}

type ttlEntry[K comparable, V any] struct {
	key K
	at  time.Time
	val V
}

func newTTLCache[K comparable, V any](ttl time.Duration, size int) *ttlCache[K, V] {
	return &ttlCache[K, V]{ttl: ttl, size: size, order: list.New(), items: map[K]*list.Element{}}
}

func (c *ttlCache[K, V]) get(k K) (V, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	var zero V
	el, ok := c.items[k]
	if !ok {
		return zero, false
	}
	e := el.Value.(*ttlEntry[K, V])
	if time.Since(e.at) > c.ttl {
		c.order.Remove(el)
		delete(c.items, k)
		return zero, false
	}
	return e.val, true
}

func (c *ttlCache[K, V]) put(k K, v V) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.items[k]; ok {
		el.Value = &ttlEntry[K, V]{k, time.Now(), v}
		c.order.MoveToBack(el)
	} else {
		c.items[k] = c.order.PushBack(&ttlEntry[K, V]{k, time.Now(), v})
	}
	for c.order.Len() > c.size {
		front := c.order.Front()
		c.order.Remove(front)
		delete(c.items, front.Value.(*ttlEntry[K, V]).key)
	}
}

// ── engine ──────────────────────────────────────────────────────────────

type engineKey struct{ keyword, itemType string }

type engineResult struct {
	owner   int64
	ownerOK bool
	hits    []int64
}

type basicKey struct{ auth, keyword string }

// basicCtx：完整的原生結果基本清單與驗證過的外掛結果。cache 共用，不能修改。
type basicCtx struct {
	native []basic
	ours   []basic
}

// engineInjector 向 sse-engine 查詢，再用搜尋者自己的 session 向 DSM 驗證、取得照片資料。
type engineInjector struct {
	f           *Filter
	cfg         *Config
	engineCache *ttlCache[engineKey, *engineResult]
	basicCache  *ttlCache[basicKey, *basicCtx]
}

func newEngineInjector(f *Filter) *engineInjector {
	return &engineInjector{
		f: f, cfg: f.cfg,
		engineCache: newTTLCache[engineKey, *engineResult](f.cfg.CacheTTL, 256),
		basicCache:  newTTLCache[basicKey, *basicCtx](f.cfg.CacheTTL, 256),
	}
}

func (e *engineInjector) wants(*plan) bool { return true }

// deadlineFor 回傳 now + d，但不超過 ctx 的期限。
func deadlineFor(ctx context.Context, d time.Duration) time.Time {
	t := time.Now().Add(d)
	if dl, ok := ctx.Deadline(); ok && dl.Before(t) {
		return dl
	}
	return t
}

func dialUnix(ctx context.Context, path string, timeout time.Duration) (*net.UnixConn, error) {
	d := net.Dialer{Timeout: timeout}
	c, err := d.DialContext(ctx, "unix", path)
	if err != nil {
		return nil, err
	}
	return c.(*net.UnixConn), nil
}

// readLine 讀一行（到 EOF 也算一行，跟 asyncio 的 readline 相同）；超過 limit 就是錯誤。
func readLine(r io.Reader, limit int) ([]byte, error) {
	br := bufio.NewReaderSize(r, chunkSize)
	var line []byte
	for {
		part, err := br.ReadSlice('\n')
		line = append(line, part...)
		if len(line) > limit {
			return nil, errors.New("engine 的回應超過長度上限")
		}
		switch err {
		case nil, io.EOF:
			return line, nil
		case bufio.ErrBufferFull:
			continue
		}
		return nil, err
	}
}

func engineRequest(keyword, itemType string) []byte {
	// 跟 Python 的 json.dumps 相同的格式
	b := []byte(`{"op": "search", "q": `)
	b = appendString(b, keyword)
	b = append(b, `, "item_type": `...)
	b = appendString(b, itemType)
	return append(b, "}\n"...)
}

func (e *engineInjector) engineSearch(ctx context.Context, keyword, itemType string) (*engineResult, error) {
	key := engineKey{keyword, itemType}
	if r, ok := e.engineCache.get(key); ok {
		return r, nil
	}
	conn, err := dialUnix(ctx, e.cfg.EngineSocket, e.cfg.EngineTimeout)
	if err != nil {
		return nil, err
	}
	conn.SetDeadline(deadlineFor(ctx, e.cfg.EngineTimeout))
	_, err = conn.Write(engineRequest(keyword, itemType))
	var line []byte
	if err == nil {
		line, err = readLine(conn, 1<<22)
	}
	conn.Close()
	if err != nil {
		return nil, err
	}
	r, err := parseEngineResponse(line, e.cfg.MaxInject)
	if err != nil {
		return nil, err
	}
	e.engineCache.put(key, r)
	return r, nil
}

func parseEngineResponse(line []byte, maxInject int) (*engineResult, error) {
	if !utf8.Valid(line) {
		return nil, errors.New("engine 的回應不是 UTF-8")
	}
	if !json.Valid(line) {
		var v any
		err := json.Unmarshal(line, &v)
		if err == nil {
			err = errors.New("不是合法的 JSON")
		}
		return nil, fmt.Errorf("engine 的回應看不懂：%w", err)
	}
	resp, ok := parseObject(line)
	if !ok {
		return nil, errors.New("engine 的回應不是物件")
	}
	if !pyTruthy(resp.get("ok")) {
		return nil, skipf("engine：%s", pyStr(resp.get("error")))
	}
	r := &engineResult{}
	r.owner, r.ownerOK = rawInt(resp.get("owner_user_id"))
	if raw, present := resp.vals["hits"]; present {
		arr, ok := parseArray(raw)
		if !ok {
			return nil, errors.New("engine 回應的 hits 不是陣列")
		}
		for _, h := range arr {
			o, ok := parseObject(h)
			if !ok {
				return nil, errors.New("engine 回應的 hits 格式不對")
			}
			id, ok1 := rawInt(o.get("id"))
			_, ok2 := rawInt(o.get("time"))
			if ok1 && ok2 {
				r.hits = append(r.hits, id)
			}
		}
	}
	if len(r.hits) > maxInject {
		r.hits = r.hits[:max(maxInject, 0)]
	}
	return r, nil
}

// call 對 synoscgi 發一個子請求，回傳 data；失敗回傳 skipError。
func (e *engineInjector) call(ctx context.Context, p *plan, q *params) (*object, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	raw := subrequestBytes(p, q)
	conn, err := dialUnix(ctx, e.cfg.Upstream, e.cfg.UpstreamConnectTimeout)
	if err != nil {
		return nil, err
	}
	buf, err := func() ([]byte, error) {
		defer conn.Close()
		if dl, ok := ctx.Deadline(); ok {
			conn.SetWriteDeadline(dl)
		}
		if _, err := conn.Write(raw); err != nil {
			return nil, err
		}
		conn.SetReadDeadline(deadlineFor(ctx, e.cfg.SubrequestTimeout))
		chunk := getBuf()
		defer putBuf(chunk)
		var out []byte
		for len(out) <= e.cfg.InjectMaxResponse {
			n, err := conn.Read(*chunk)
			out = append(out, (*chunk)[:n]...)
			if err == io.EOF {
				break
			}
			if err != nil {
				return nil, err
			}
		}
		return out, nil
	}()
	if err != nil {
		return nil, err
	}
	e.f.bump(stSubrequests, 1)
	api, _ := q.get("api")
	method, _ := q.get("method")
	parsed := parseCGIResponse(buf)
	if parsed != nil {
		e.f.capture("sub", q, json.RawMessage(parsed.body))
	} else {
		e.f.capture("sub", q, decodeReplace(buf[:min(len(buf), 2000)]))
		return nil, skipf("子請求 %s.%s 回應看不懂", api, method)
	}
	d, ok := parseObject(parsed.body)
	if ok && rawIsTrue(d.get("success")) {
		if data, ok := parseObject(d.get("data")); ok {
			return data, nil
		}
	}
	code := "None"
	if ok && pyTruthy(d.get("error")) {
		eo, ok := parseObject(d.get("error"))
		if !ok {
			return nil, fmt.Errorf("子請求 %s.%s 的 error 欄位格式不對", api, method)
		}
		code = pyStr(eo.get("code"))
	}
	return nil, skipf("子請求 %s.%s 失敗（%s）", api, method, code)
}

// getItems 用 Browse.Item.get 取照片資料：DSM 用搜尋者的權限回傳；沒權限、已刪除的不會出現在結果裡。
//
// 一張都取不到時回傳 skipError：多半是請求本身有問題（例如參數不被接受），
// 不能當成「這些照片都被刪了」而回傳空清單。
func (e *engineInjector) getItems(ctx context.Context, p *plan, ids []int64, additional *string) (map[int64]json.RawMessage, error) {
	out := map[int64]json.RawMessage{}
	lastError := "None"
	collect := func(data *object) {
		lst, _ := parseArray(data.get("list"))
		for _, it := range lst {
			if o, ok := parseObject(it); ok {
				if id, ok := rawInt(o.get("id")); ok {
					out[id] = it
				}
			}
		}
	}
	for i := 0; i < len(ids); i += 100 {
		chunk := ids[i:min(i+100, len(ids))]
		q := newParams()
		q.set("api", "SYNO.Foto.Browse.Item")
		q.set("version", strconv.Itoa(e.cfg.ItemGetVersion))
		q.set("method", "get")
		q.set("id", idList(chunk))
		if additional != nil {
			q.set("additional", *additional)
		}
		data, err := e.call(ctx, p, q)
		if err == nil {
			collect(data)
			continue
		}
		var sk *skipError
		if !errors.As(err, &sk) {
			return nil, err
		}
		lastError = sk.msg
		if len(chunk) > 1 {
			// 一整批失敗時逐張再試：可能只是其中一張被刪掉
			for _, id := range chunk {
				one := q.clone()
				one.set("id", idList([]int64{id}))
				data, err := e.call(ctx, p, one)
				if err != nil {
					if !errors.As(err, &sk) {
						return nil, err
					}
					lastError = sk.msg
					continue
				}
				collect(data)
			}
		}
	}
	if len(ids) > 0 && len(out) == 0 {
		return nil, skipf("一張照片資料都取不到：%s", lastError)
	}
	return out, nil
}

// context 回傳原生結果與驗證過的外掛結果；同一個身分與搜尋字在 cache_ttl 內共用。
//
// 原生的清單不帶時間範圍去取：count_item 與 get_search_timeline 本來就不理會時間範圍，
// list_item 與 list_basic_timeline_info 再自己套用，四個 method 才會來自同一份清單。
func (e *engineInjector) context(ctx context.Context, p *plan) (*basicCtx, error) {
	r, err := e.engineSearch(ctx, p.keyword, "all") // 原生忽略 item_type，這裡也一樣
	if err != nil {
		return nil, err
	}
	if len(r.hits) == 0 {
		return &basicCtx{}, nil
	}
	key := basicKey{authKey(p), p.keyword}
	if c, ok := e.basicCache.get(key); ok {
		return c, nil
	}
	got, err := e.getItems(ctx, p, r.hits, nil)
	if err != nil {
		return nil, err
	}
	var ours []basic
	for _, id := range r.hits {
		raw, ok := got[id]
		if !ok {
			continue
		}
		it, _ := parseObject(raw)
		owner := it.get("owner_user_id")
		o, isInt := rawInt(owner)
		t, hasTime := rawInt(it.get("time"))
		if !r.ownerOK || !isInt || o != r.owner || !hasTime {
			continue
		}
		ours = append(ours, basic{id, compactRaw(owner), t})
	}
	if len(ours) == 0 {
		c := &basicCtx{}
		e.basicCache.put(key, c)
		return c, nil
	}
	q := newParams()
	for _, k := range p.params.keys {
		if inList(k, authParams) || inList(k, []string{"method", "offset", "limit", "additional",
			"timeline_group_unit", "start_time", "end_time"}) {
			continue
		}
		q.set(k, p.params.vals[k])
	}
	q.set("method", "list_basic_timeline_info")
	data, err := e.call(ctx, p, q)
	if err != nil {
		return nil, err
	}
	lst, ok := parseArray(data.get("list"))
	if !ok {
		return nil, skipf("原生 list_basic_timeline_info 格式看不懂")
	}
	native := make([]basic, 0, len(lst))
	for _, x := range lst {
		o, ok := parseObject(x)
		if !ok {
			return nil, skipf("原生 list_basic_timeline_info 格式看不懂")
		}
		id, ok1 := rawInt(o.get("id"))
		t, ok2 := rawInt(o.get("time"))
		if !ok1 || !ok2 {
			return nil, skipf("原生 list_basic_timeline_info 格式看不懂")
		}
		var owner json.RawMessage
		if v := o.get("owner_user_id"); v != nil {
			owner = compactRaw(v)
		}
		native = append(native, basic{id, owner, t})
	}
	c := &basicCtx{native, ours}
	e.basicCache.put(key, c)
	return c, nil
}

func (e *engineInjector) rewrite(ctx context.Context, p *plan, data []byte) ([]byte, error) {
	top, ok := parseObject(data)
	if !ok || !rawIsTrue(top.get("success")) {
		return nil, nil
	}
	body, ok := parseObject(top.get("data"))
	if !ok {
		return nil, nil
	}
	c, err := e.context(ctx, p)
	if err != nil {
		return nil, err
	}
	if len(c.ours) == 0 {
		return nil, nil
	}
	nativeIDs := make(map[int64]bool, len(c.native))
	for _, x := range c.native {
		nativeIDs[x.id] = true
	}
	allNative := true
	for _, x := range c.ours {
		if !nativeIDs[x.id] {
			allNative = false
			break
		}
	}
	if allNative {
		return nil, nil // 原生已經全部找得到，不必改
	}
	merged, err := mergeBasic(c.native, c.ours)
	if err != nil {
		return nil, err
	}
	if p.method == "list_item" || p.method == "list_basic_timeline_info" {
		merged = applyTimeRange(merged, p.params)
	}
	switch p.method {
	case "count_item":
		if _, ok := rawInt(body.get("count")); !ok {
			return nil, nil
		}
		body.set("count", json.RawMessage(strconv.Itoa(len(merged))))
	case "list_basic_timeline_info":
		body.set("list", marshalBasics(merged))
	case "get_search_timeline":
		if kindOf(body.get("section")) != '[' {
			return nil, nil
		}
		unit := normalizeKeyword(p.params.get("timeline_group_unit"))
		if unit == "" {
			unit = "day"
		}
		body.set("section", mustMarshal(buildSections(merged, unit)))
	case "list_item":
		lst, ok := parseArray(body.get("list"))
		if !ok {
			return nil, nil
		}
		page := pageOf(merged, p.params)
		// 原生回應裡已經有的照片直接沿用，其餘才向 DSM 取
		got := map[int64]json.RawMessage{}
		for _, x := range lst {
			if o, ok := parseObject(x); ok {
				if id, ok := rawInt(o.get("id")); ok {
					got[id] = x
				}
			}
		}
		var missing []int64
		for _, x := range page {
			if _, ok := got[x.id]; !ok {
				missing = append(missing, x.id)
			}
		}
		if len(missing) > 0 {
			var additional *string
			if v, ok := p.params.get("additional"); ok {
				additional = &v
			}
			fetched, err := e.getItems(ctx, p, missing, additional)
			if err != nil {
				return nil, err
			}
			for id, it := range fetched {
				got[id] = it
			}
		}
		out := make([]json.RawMessage, 0, len(page))
		for _, x := range page {
			if it, ok := got[x.id]; ok {
				out = append(out, it)
			}
		}
		body.set("list", joinArray(out))
	default:
		return nil, nil
	}
	top.set("data", body.marshal())
	return top.marshal(), nil
}
