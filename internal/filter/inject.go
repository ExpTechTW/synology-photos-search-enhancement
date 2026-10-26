package filter

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strconv"
	"time"
	"unicode/utf8"
)

// 注入（純函式，方便測試）。

func utcDay(ts int64) (int, int, int) {
	y, m, d := time.Unix(ts, 0).UTC().Date()
	return y, int(m), d
}

// basic 是 list_basic_timeline_info 的一筆：{"id","owner_user_id","time"}。
type basic struct {
	id    int64
	owner json.RawMessage // 原樣保留
	time  int64
}

func (b basic) appendJSON(dst []byte) []byte {
	dst = append(dst, `{"id":`...)
	dst = strconv.AppendInt(dst, b.id, 10)
	dst = append(dst, `,"owner_user_id":`...)
	dst = append(dst, b.owner...)
	dst = append(dst, `,"time":`...)
	dst = strconv.AppendInt(dst, b.time, 10)
	return append(dst, '}')
}

func marshalBasics(items []basic) []byte {
	b := make([]byte, 0, 2+len(items)*64)
	b = append(b, '[')
	for i, x := range items {
		if i > 0 {
			b = append(b, ',')
		}
		b = x.appendJSON(b)
	}
	return append(b, ']')
}

type secEntry struct {
	Day       int `json:"day,omitempty"`
	ItemCount int `json:"item_count"`
	Month     int `json:"month"`
	Year      int `json:"year"`
}

type section struct {
	Limit  int        `json:"limit"`
	List   []secEntry `json:"list"`
	Offset int        `json:"offset"`
}

func mustMarshal(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

// injectDemo：demo 模式把固定的 items 併進原生搜尋回應。回傳改寫後的回應，不需要改就回傳 nil。
func injectDemo(method string, p *params, data []byte, items []demoItem) []byte {
	top, ok := parseObject(data)
	if !ok || !rawIsTrue(top.get("success")) {
		return nil
	}
	body, ok := parseObject(top.get("data"))
	if !ok {
		return nil
	}
	if method == "count_item" {
		c, ok := rawInt(body.get("count"))
		if !ok {
			return nil
		}
		body.set("count", json.RawMessage(strconv.FormatInt(c+int64(len(items)), 10)))
		top.set("data", body.marshal())
		return top.marshal()
	}
	// 只注入第一頁，避免往下捲時每一頁都重複出現
	if off, ok := toInt(p, "offset"); ok && off != 0 {
		return nil
	}
	switch method {
	case "list_item", "list_basic_timeline_info":
		lst, ok := parseArray(body.get("list"))
		if !ok {
			return nil
		}
		existing := map[int64]bool{}
		for _, x := range lst {
			if o, ok := parseObject(x); ok {
				if id, ok := rawInt(o.get("id")); ok {
					existing[id] = true
				}
			}
		}
		var add []json.RawMessage
		for _, it := range items {
			if existing[it.id] {
				continue
			}
			if method == "list_basic_timeline_info" {
				owner := json.RawMessage(strconv.FormatInt(it.ownerUser, 10))
				add = append(add, basic{it.id, owner, it.time}.appendJSON(nil))
			} else {
				add = append(add, it.raw)
			}
		}
		if len(add) == 0 {
			return nil
		}
		body.set("list", joinArray(append(add, lst...)))
	case "get_search_timeline":
		sec, ok := parseArray(body.get("section"))
		if !ok {
			return nil
		}
		type ymd struct{ y, m, d int }
		days := map[ymd]int{}
		for _, it := range items {
			y, m, d := utcDay(it.time)
			days[ymd{y, m, d}]++
		}
		keys := make([]ymd, 0, len(days))
		for k := range days {
			keys = append(keys, k)
		}
		sort.Slice(keys, func(i, j int) bool {
			a, b := keys[i], keys[j]
			if a.y != b.y {
				return a.y > b.y
			}
			if a.m != b.m {
				return a.m > b.m
			}
			return a.d > b.d
		})
		lst := make([]secEntry, 0, len(keys))
		for _, k := range keys {
			lst = append(lst, secEntry{Day: k.d, ItemCount: days[k], Month: k.m, Year: k.y})
		}
		first := mustMarshal(section{Limit: 100, List: lst, Offset: 0})
		body.set("section", joinArray(append([]json.RawMessage{first}, sec...)))
	default:
		return nil
	}
	top.set("data", body.marshal())
	return top.marshal()
}

// mergeBasic 把原生結果與外掛結果合併成一份清單，id 不重複。
//
// 原生只依時間新到舊排（同時間的順序是它自己的），所以用穩定排序：原生之間的相對順序不變，
// 外掛的照片插在同時間的原生照片後面。
func mergeBasic(native, ours []basic) ([]basic, error) {
	seen := make(map[int64]bool, len(native)+len(ours))
	out := make([]basic, 0, len(native)+len(ours))
	for _, list := range [][]basic{native, ours} {
		for _, x := range list {
			if x.owner == nil {
				return nil, errors.New("原生結果缺少 owner_user_id")
			}
			if seen[x.id] {
				continue
			}
			seen[x.id] = true
			out = append(out, x)
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].time > out[j].time })
	return out, nil
}

// applyTimeRange 套用 start_time / end_time（Photos 1.9.0 只有 list_item 與 list_basic_timeline_info 會套用）。
// sort_direction、item_type 原生會忽略，這裡也不處理。
func applyTimeRange(items []basic, p *params) []basic {
	start, hasStart := toInt(p, "start_time")
	end, hasEnd := toInt(p, "end_time")
	if !hasStart && !hasEnd {
		return items
	}
	out := make([]basic, 0, len(items))
	for _, x := range items {
		if hasStart && x.time < start || hasEnd && x.time > end {
			continue
		}
		out = append(out, x)
	}
	return out
}

// buildSections 依合併後的清單重建 get_search_timeline 的 section（規則由 Photos 1.9.0 的原生回應反推）：
//
//   - 以天（或月）為一組，依清單順序累積，超過 chunk 張就成一段
//   - 單一組就超過 chunk 張時自己成段，平均拆成「張數 / chunk」段；
//     這些段的 list 都是那一整組（item_count 是整組張數），offset 是該段在組內的起點
//   - 遇到這種大組或清單結尾時，還沒滿的部分併入前一段（前一段是大組拆出來的就自成一段）
//   - 一般段的 limit 是該段張數，offset 是 0
func buildSections(items []basic, unit string) []section {
	const chunk = 100
	type key struct{ y, m, d int }
	type group struct {
		k key
		n int
	}
	var groups []group
	for _, x := range items {
		y, m, d := utcDay(x.time)
		k := key{y, m, d}
		if unit == "month" {
			k.d = 0
		}
		if len(groups) > 0 && groups[len(groups)-1].k == k {
			groups[len(groups)-1].n++
		} else {
			groups = append(groups, group{k, 1})
		}
	}
	type sec struct {
		big           bool
		gs            []group
		limit, offset int
	}
	var secs []sec
	var cur []group
	n := 0
	sum := func(gs []group) int {
		t := 0
		for _, g := range gs {
			t += g.n
		}
		return t
	}
	flush := func() {
		if len(cur) > 0 {
			if len(secs) > 0 && !secs[len(secs)-1].big {
				last := &secs[len(secs)-1]
				last.gs = append(last.gs, cur...)
				last.limit += sum(cur)
			} else {
				secs = append(secs, sec{false, append([]group(nil), cur...), sum(cur), 0})
			}
		}
		cur = nil
	}
	for _, g := range groups {
		c := g.n
		if c > chunk {
			flush()
			n = 0
			parts := c / chunk
			base, extra := c/parts, c%parts
			off := 0
			for i := 0; i < parts; i++ {
				size := base
				if i < extra {
					size++
				}
				secs = append(secs, sec{true, []group{g}, size, off})
				off += size
			}
			continue
		}
		cur = append(cur, g)
		n += c
		if n > chunk {
			secs = append(secs, sec{false, append([]group(nil), cur...), n, 0})
			cur = nil
			n = 0
		}
	}
	flush()
	out := make([]section, 0, len(secs))
	for _, s := range secs {
		lst := make([]secEntry, 0, len(s.gs))
		for _, g := range s.gs {
			e := secEntry{ItemCount: g.n, Month: g.k.m, Year: g.k.y}
			if unit != "month" {
				e.Day = g.k.d
			}
			lst = append(lst, e)
		}
		out = append(out, section{Limit: s.limit, List: lst, Offset: s.offset})
	}
	return out
}

// pageOf 跟 Python 的 items[offset:offset + limit] 相同（limit 沒給或是負數就取到最後）。
func pageOf(items []basic, p *params) []basic {
	off, _ := toInt(p, "offset")
	if off < 0 {
		off = 0
	}
	if off >= int64(len(items)) {
		return nil
	}
	rest := items[off:]
	if limit, ok := toInt(p, "limit"); ok && limit >= 0 && limit < int64(len(rest)) {
		return rest[:limit]
	}
	return rest
}

// cgiResponse 是 synoscgi 的 CGI 回應：header 行與 JSON body。
type cgiResponse struct {
	lines [][]byte
	body  []byte
}

// parseCGIResponse：不是 200、有壓縮或分塊、不是 JSON 就回傳 nil（不碰）。
func parseCGIResponse(raw []byte) *cgiResponse {
	sep := bytes.Index(raw, []byte("\r\n\r\n"))
	if sep < 0 {
		return nil
	}
	lines := bytes.Split(raw[:sep], []byte("\r\n"))
	hdrs := map[string]string{}
	for _, ln := range lines {
		k, v, _ := bytes.Cut(ln, []byte(":"))
		hdrs[asciiLower(string(asciiTrim(k)))] = asciiLower(string(asciiTrim(v)))
	}
	if st := hdrs["status"]; st != "" && !bytes.HasPrefix([]byte(st), []byte("200")) {
		return nil
	}
	if ce, ok := hdrs["content-encoding"]; ok && ce != "identity" {
		return nil
	}
	if _, ok := hdrs["transfer-encoding"]; ok {
		return nil
	}
	body := raw[sep+4:]
	if !utf8.Valid(body) || !json.Valid(body) {
		return nil
	}
	return &cgiResponse{lines: lines, body: body}
}

// buildCGIResponse 換上新的 body，Content-Length 跟著改。
func buildCGIResponse(lines [][]byte, body []byte) []byte {
	var out bytes.Buffer
	out.Grow(len(body) + 256)
	for i, ln := range lines {
		if i > 0 {
			out.WriteString("\r\n")
		}
		name, _, _ := bytes.Cut(ln, []byte(":"))
		if asciiLower(string(asciiTrim(name))) == "content-length" {
			out.Write(name)
			out.WriteString(": ")
			out.WriteString(strconv.Itoa(len(body)))
			continue
		}
		out.Write(ln)
	}
	out.WriteString("\r\n\r\n")
	out.Write(body)
	return out.Bytes()
}

// buildSCGI 組 SCGI request：SCGI 規定 CONTENT_LENGTH 必須是第一個 header。
func buildSCGI(h *headers, body []byte) []byte {
	var hb bytes.Buffer
	hb.WriteString("CONTENT_LENGTH\x00" + strconv.Itoa(len(body)) + "\x00")
	scgi, ok := h.get("SCGI")
	if !ok {
		scgi = "1"
	}
	hb.WriteString("SCGI\x00" + scgi + "\x00")
	for _, k := range h.keys {
		if k != "CONTENT_LENGTH" && k != "SCGI" {
			hb.WriteString(k + "\x00" + h.vals[k] + "\x00")
		}
	}
	out := make([]byte, 0, hb.Len()+len(body)+16)
	out = strconv.AppendInt(out, int64(hb.Len()), 10)
	out = append(out, ':')
	out = append(out, hb.Bytes()...)
	out = append(out, ',')
	return append(out, body...)
}

// 子請求不沿用這些 header：內容、長度、路徑都是新的；不要壓縮，才能直接解析
var subDrop = map[string]bool{"CONTENT_LENGTH": true, "CONTENT_TYPE": true, "HTTP_CONTENT_LENGTH": true,
	"HTTP_CONTENT_TYPE": true, "QUERY_STRING": true, "REQUEST_URI": true, "DOCUMENT_URI": true, "SCRIPT_NAME": true,
	"PATH_INFO": true, "REQUEST_METHOD": true, "HTTP_ACCEPT_ENCODING": true, "HTTP_TRANSFER_ENCODING": true,
	"HTTP_EXPECT": true}

// subrequestBytes 用原請求的身分（cookie、SynoToken、_sid、來源 IP）組一個新的 WebAPI 請求。
func subrequestBytes(p *plan, q *params) []byte {
	h := newHeaders()
	for _, k := range p.headers.keys {
		if !subDrop[k] {
			h.set(k, p.headers.vals[k])
		}
	}
	query := newParams()
	b := q.clone()
	for _, k := range authParams {
		if v, ok := p.qparams.get(k); ok {
			query.set(k, v)
		}
	}
	for _, k := range authParams {
		if v, ok := p.bparams.get(k); ok {
			b.set(k, v)
		}
	}
	body := []byte(b.encode())
	qs := query.encode()
	h.set("REQUEST_METHOD", "POST")
	h.set("CONTENT_TYPE", "application/x-www-form-urlencoded")
	h.set("QUERY_STRING", qs)
	uri := "/webapi/entry.cgi"
	if len(query.keys) > 0 {
		uri += "?" + qs
	}
	h.set("REQUEST_URI", uri)
	h.set("DOCUMENT_URI", "/webapi/entry.cgi")
	h.set("SCRIPT_NAME", "/webapi/entry.cgi")
	return buildSCGI(h, body)
}

// authKey 是搜尋者身分的雜湊（cache 的鍵，不保存 cookie 本身）。
func authKey(p *plan) string {
	h := sha256.New()
	h.Write([]byte(p.headers.val("HTTP_COOKIE")))
	for _, k := range authParams {
		v, _ := p.params.get(k)
		h.Write([]byte("\x00" + v))
	}
	h.Write([]byte("\x00" + p.headers.val("HTTP_X_SYNO_TOKEN")))
	return hex.EncodeToString(h.Sum(nil))
}
