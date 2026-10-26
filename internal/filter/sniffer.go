package filter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

// 解析 request（只看副本，不影響轉發）。

const searchAPI = "SYNO.Foto.Search.Search"

var (
	injectMethods = []string{"list_item", "list_basic_timeline_info", "get_search_timeline", "count_item"}
	authParams    = []string{"_sid", "SynoToken"}
	entryRE       = regexp.MustCompile(`/webapi/entry\.cgi(?:/|\?|$)`)
)

func inList(s string, list []string) bool {
	for _, x := range list {
		if s == x {
			return true
		}
	}
	return false
}

// params 是保留順序的參數（跟 Python 的 dict 相同：新的鍵加在最後，已有的鍵改值不改位置）。
type params struct {
	keys []string
	vals map[string]string
}

func newParams() *params { return &params{vals: map[string]string{}} }

func (p *params) get(k string) (string, bool) {
	if p == nil {
		return "", false
	}
	v, ok := p.vals[k]
	return v, ok
}

func (p *params) set(k, v string) {
	if _, ok := p.vals[k]; !ok {
		p.keys = append(p.keys, k)
	}
	p.vals[k] = v
}

func (p *params) clone() *params {
	c := &params{keys: append([]string(nil), p.keys...), vals: make(map[string]string, len(p.vals))}
	for k, v := range p.vals {
		c.vals[k] = v
	}
	return c
}

func (p *params) update(o *params) {
	for _, k := range o.keys {
		p.set(k, o.vals[k])
	}
}

// without 回傳拿掉 keys 之後的副本。
func (p *params) without(keys ...string) *params {
	c := newParams()
	if p == nil {
		return c
	}
	for _, k := range p.keys {
		if !inList(k, keys) {
			c.set(k, p.vals[k])
		}
	}
	return c
}

// encode 跟 Python 的 urlencode 相同（quote_plus：空白變 +，只有英數字與 _.-~ 不編碼）。
func (p *params) encode() string {
	var sb strings.Builder
	for i, k := range p.keys {
		if i > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(url.QueryEscape(k))
		sb.WriteByte('=')
		sb.WriteString(url.QueryEscape(p.vals[k]))
	}
	return sb.String()
}

// MarshalJSON 依順序輸出成 JSON 物件（診斷錄製用）。
func (p *params) MarshalJSON() ([]byte, error) {
	if p == nil {
		return []byte("{}"), nil
	}
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range p.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, _ := marshalPlain(k)
		vb, _ := marshalPlain(p.vals[k])
		buf.Write(kb)
		buf.WriteByte(':')
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// headers 是 SCGI 的 header（保留順序；重複的名稱位置在前、值取後者）。
type headers struct {
	keys []string
	vals map[string]string
}

func newHeaders() *headers { return &headers{vals: map[string]string{}} }

func (h *headers) get(k string) (string, bool) {
	if h == nil {
		return "", false
	}
	v, ok := h.vals[k]
	return v, ok
}

func (h *headers) val(k string) string { v, _ := h.get(k); return v }

func (h *headers) set(k, v string) {
	if _, ok := h.vals[k]; !ok {
		h.keys = append(h.keys, k)
	}
	h.vals[k] = v
}

// plan 是要檢查回應的搜尋請求。
type plan struct {
	method, keyword string
	params          *params
	qparams         *params
	bparams         *params
	headers         *headers
}

const (
	snLen = iota
	snHead
	snBody
	snDone
	snSkip
)

// sniffer 邊轉發邊解析 SCGI request 的副本，只用來判斷要不要檢查回應。
//
// 任何看不懂的格式都放棄解析（skip），轉發照常進行。request 與回應在不同的 goroutine，所以要上鎖。
type sniffer struct {
	mu         sync.Mutex
	maxHeader  int
	maxBody    int
	buf        []byte
	state      int
	hlen, clen int
	headers    *headers
	params     *params
	qparams    *params
	bparams    *params
	skipReason string
}

func newSniffer(maxHeader, maxBody int) *sniffer {
	return &sniffer{maxHeader: maxHeader, maxBody: maxBody}
}

func (s *sniffer) feed(data []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == snDone || s.state == snSkip {
		return
	}
	defer func() {
		if r := recover(); r != nil {
			s.skip(fmt.Sprintf("解析失敗：%v", r))
		}
	}()
	s.buf = append(s.buf, data...)
	if err := s.advance(); err != nil {
		s.skip("解析失敗：" + err.Error())
	}
}

func (s *sniffer) skip(reason string) {
	s.state = snSkip
	if s.skipReason == "" {
		s.skipReason = reason
	}
	s.buf = nil
}

func (s *sniffer) advance() error {
	if s.state == snLen {
		i := bytes.IndexByte(s.buf, ':')
		if i < 0 {
			if len(s.buf) > 10 {
				return errors.New("netstring 長度欄位過長")
			}
			return nil
		}
		n := s.buf[:i]
		if !isDigits(n) {
			return errors.New("netstring 長度不是數字")
		}
		hlen := math.MaxInt32
		if len(n) <= 9 {
			hlen, _ = strconv.Atoi(string(n))
		}
		if hlen > s.maxHeader {
			return errors.New("header 太大")
		}
		s.hlen = hlen
		s.buf = s.buf[i+1:]
		s.state = snHead
	}
	if s.state == snHead {
		if len(s.buf) < s.hlen+1 {
			return nil
		}
		if s.buf[s.hlen] != ',' {
			return errors.New("netstring 結尾不是逗號")
		}
		parts := bytes.Split(s.buf[:s.hlen], []byte{0})
		s.buf = s.buf[s.hlen+1:]
		if len(parts)%2 != 1 || len(parts[len(parts)-1]) != 0 {
			return errors.New("header 格式錯誤")
		}
		h := newHeaders()
		for i := 0; i+1 < len(parts); i += 2 {
			h.set(string(parts[i]), string(parts[i+1]))
		}
		s.headers = h
		if !s.candidate() {
			s.skip("")
			return nil
		}
		s.state = snBody
	}
	if s.state == snBody {
		if len(s.buf) < s.clen {
			return nil
		}
		body := s.buf[:s.clen]
		s.state = snDone
		s.qparams = parseQS(decodeReplace([]byte(s.headers.val("QUERY_STRING"))))
		s.bparams = newParams()
		if len(body) > 0 {
			s.bparams = parseQS(decodeReplace(body))
		}
		s.params = s.qparams.clone()
		s.params.update(s.bparams)
		s.buf = nil
	}
	return nil
}

func isDigits(b []byte) bool {
	if len(b) == 0 {
		return false
	}
	for _, c := range b {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (s *sniffer) entryCGI() bool {
	uri := s.headers.val("REQUEST_URI")
	if uri == "" {
		uri = s.headers.val("SCRIPT_NAME")
	}
	path, _, _ := strings.Cut(uri, "?")
	return entryRE.MatchString(path + "?")
}

func (s *sniffer) candidate() bool {
	h := s.headers
	cl := h.val("CONTENT_LENGTH")
	if cl == "" {
		cl = "0"
	}
	clen, ok := pyParseIntASCII(cl)
	if !ok {
		s.skipReason = "CONTENT_LENGTH 不是數字"
		return false
	}
	s.clen = clampInt(clen)
	if !s.entryCGI() {
		s.skipReason = "不是 entry.cgi"
		return false
	}
	if clen < 0 || clen > int64(s.maxBody) {
		s.skipReason = fmt.Sprintf("body 太大（%d bytes）", clen)
		return false
	}
	if m := h.val("REQUEST_METHOD"); m != "GET" && m != "POST" {
		s.skipReason = "HTTP 方法 " + asciiReplace(m)
		return false
	}
	ctype := asciiLower(h.val("CONTENT_TYPE"))
	if clen != 0 && !strings.HasPrefix(ctype, "application/x-www-form-urlencoded") {
		s.skipReason = "Content-Type " + asciiReplace(ctype)
		return false
	}
	return true
}

func (s *sniffer) plan() *plan {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != snDone || s.params == nil {
		return nil
	}
	p := s.params
	api, _ := p.get("api")
	method, _ := p.get("method")
	if api != searchAPI || !inList(method, injectMethods) {
		return nil
	}
	kw := normalizeKeyword(p.get("keyword"))
	if kw == "" {
		return nil
	}
	return &plan{method: method, keyword: kw, params: p, qparams: s.qparams, bparams: s.bparams, headers: s.headers}
}

// 診斷錄製時不記的 API（量大、跟搜尋無關）
var captureSkipAPIs = []string{"SYNO.Foto.Thumbnail", "SYNO.FotoTeam.Thumbnail", "SYNO.Foto.Download",
	"SYNO.FotoTeam.Download", "SYNO.Foto.Upload", "SYNO.FotoTeam.Upload", "SYNO.Foto.Streaming",
	"SYNO.FotoTeam.Streaming"}

type skippedInfo struct {
	Reason        *string `json:"reason"`
	Method        string  `json:"method"`
	Path          string  `json:"path"`
	ContentType   string  `json:"content_type"`
	ContentLength string  `json:"content_length"`
}

// captureInfo：沒被當成搜尋的請求要不要錄、錄什麼（ok 為 false 表示不錄）。
func (s *sniffer) captureInfo() (kind string, p *params, resp any, ok bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == snDone && s.params != nil {
		api, _ := s.params.get("api")
		if (strings.HasPrefix(api, "SYNO.Foto.") || strings.HasPrefix(api, "SYNO.FotoTeam.")) && !hasAnyPrefix(api, captureSkipAPIs) {
			return "request", s.params, nil, true
		}
	} else if s.state == snSkip && s.headers != nil && s.entryCGI() {
		h := s.headers
		var reason *string
		if s.skipReason != "" {
			r := s.skipReason
			reason = &r
		}
		path, _, _ := strings.Cut(h.val("REQUEST_URI"), "?")
		return "skipped", nil, skippedInfo{
			Reason:        reason,
			Method:        asciiReplace(h.val("REQUEST_METHOD")),
			Path:          decodeReplace([]byte(path)),
			ContentType:   asciiReplace(h.val("CONTENT_TYPE")),
			ContentLength: asciiReplace(h.val("CONTENT_LENGTH")),
		}, true
	}
	return "", nil, nil, false
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

// normalizeKeyword 去掉搜尋字外面的引號（網頁版送 JSON 字串 "\"貓\""）。
func normalizeKeyword(v string, ok bool) string {
	if !ok {
		return ""
	}
	v = pyStrip(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		var s string
		if err := json.Unmarshal([]byte(v), &s); err == nil {
			v = s
		} else {
			v = v[1 : len(v)-1]
		}
	} else if len(v) >= 2 && v[0] == '\'' && v[len(v)-1] == '\'' {
		v = v[1 : len(v)-1]
	}
	return pyStrip(v)
}

// ── 跟 Python 標準庫行為相同的小函式 ──

// parseQS 跟 Python 的 parse_qs(keep_blank_values=True) 相同，每個鍵只取第一個值。
func parseQS(s string) *params {
	p := newParams()
	for _, nv := range strings.Split(s, "&") {
		if nv == "" {
			continue
		}
		name, value, _ := strings.Cut(nv, "=")
		name = pyUnquote(strings.ReplaceAll(name, "+", " "))
		value = pyUnquote(strings.ReplaceAll(value, "+", " "))
		if _, ok := p.vals[name]; !ok {
			p.set(name, value)
		}
	}
	return p
}

// pyUnquote 跟 Python 的 urllib.parse.unquote 相同：%XX 解碼，不合法的保留原樣，解出來不是 UTF-8 的換成 U+FFFD。
func pyUnquote(s string) string {
	if !strings.Contains(s, "%") {
		return s
	}
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		if s[i] == '%' && i+2 < len(s) && isHex(s[i+1]) && isHex(s[i+2]) {
			b = append(b, unhex(s[i+1])<<4|unhex(s[i+2]))
			i += 2
			continue
		}
		b = append(b, s[i])
	}
	return decodeReplace(b)
}

func isHex(c byte) bool {
	return c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F'
}

func unhex(c byte) byte {
	switch {
	case c >= 'a':
		return c - 'a' + 10
	case c >= 'A':
		return c - 'A' + 10
	}
	return c - '0'
}

// decodeReplace 跟 Python 的 bytes.decode("utf-8", "replace") 相同（不合法的序列換成一個 U+FFFD）。
func decodeReplace(b []byte) string {
	if utf8.Valid(b) {
		return string(b)
	}
	var sb strings.Builder
	for i := 0; i < len(b); {
		r, size := utf8.DecodeRune(b[i:])
		if r != utf8.RuneError || size > 1 {
			sb.Write(b[i : i+size])
			i += size
			continue
		}
		sb.WriteRune(utf8.RuneError)
		i += invalidLen(b[i:])
	}
	return sb.String()
}

// invalidLen：不合法的 UTF-8 序列中，要換成一個 U+FFFD 的長度（Unicode 建議的 maximal subpart，Python 的做法）。
func invalidLen(b []byte) int {
	c := b[0]
	n, lo, hi := 0, byte(0x80), byte(0xBF)
	switch {
	case c >= 0xC2 && c <= 0xDF:
		n = 2
	case c == 0xE0:
		n, lo = 3, 0xA0
	case c == 0xED:
		n, hi = 3, 0x9F
	case c >= 0xE1 && c <= 0xEF:
		n = 3
	case c == 0xF0:
		n, lo = 4, 0x90
	case c == 0xF4:
		n, hi = 4, 0x8F
	case c >= 0xF1 && c <= 0xF3:
		n = 4
	default:
		return 1
	}
	i := 1
	for ; i < n && i < len(b); i++ {
		if b[i] < lo || b[i] > hi {
			break
		}
		lo, hi = 0x80, 0xBF
	}
	return i
}

// asciiReplace 跟 Python 的 bytes.decode("ascii", "replace") 相同。
func asciiReplace(s string) string {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			var sb strings.Builder
			for j := 0; j < len(s); j++ {
				if s[j] >= 0x80 {
					sb.WriteRune(utf8.RuneError)
				} else {
					sb.WriteByte(s[j])
				}
			}
			return sb.String()
		}
	}
	return s
}

func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + 32
		}
	}
	return string(b)
}

func asciiSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f'
}

func asciiTrim(b []byte) []byte {
	i, j := 0, len(b)
	for i < j && asciiSpace(b[i]) {
		i++
	}
	for j > i && asciiSpace(b[j-1]) {
		j--
	}
	return b[i:j]
}

// pyIsSpace 跟 Python 的 str.isspace 相同（比 Go 多了 \x1c～\x1f）。
func pyIsSpace(r rune) bool { return unicode.IsSpace(r) || r >= 0x1c && r <= 0x1f }

func pyStrip(s string) string { return strings.TrimFunc(s, pyIsSpace) }

// pyParseInt 跟 Python 的 int(字串, base) 相同：可有前後空白、正負號、數字間的單一底線；base 8 可加 0o。
// 超出範圍時取 int64 的上下限（Python 的整數沒有上限，比大小的結果不變）。
func pyParseInt(s string, base int) (int64, bool) {
	// int() 去掉的空白跟 str.strip() 不同：不含 \x1c～\x1f，正好是 Go 的 unicode.IsSpace
	return parseIntDigits(strings.TrimFunc(s, unicode.IsSpace), base)
}

// pyParseIntASCII 是 Python 的 int(bytes)：只去掉 ASCII 空白。
func pyParseIntASCII(s string) (int64, bool) {
	return parseIntDigits(string(asciiTrim([]byte(s))), 10)
}

func parseIntDigits(s string, base int) (int64, bool) {
	neg := false
	if s != "" && (s[0] == '+' || s[0] == '-') {
		neg = s[0] == '-'
		s = s[1:]
	}
	if base == 8 && len(s) >= 2 && s[0] == '0' && (s[1] == 'o' || s[1] == 'O') {
		s = s[2:]
		if strings.HasPrefix(s, "_") {
			s = s[1:]
		}
	}
	if s == "" || s[0] == '_' || s[len(s)-1] == '_' || strings.Contains(s, "__") {
		return 0, false
	}
	digits := strings.ReplaceAll(s, "_", "")
	for _, c := range digits {
		if c < '0' || c > rune('0'+base-1) {
			return 0, false
		}
	}
	if neg {
		digits = "-" + digits
	}
	n, err := strconv.ParseInt(digits, base, 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
			return 0, false
		}
	}
	return n, true
}

// toInt 是 Python 版的 _to_int：參數不是整數時用預設值。
func toInt(p *params, k string) (int64, bool) {
	v, ok := p.get(k)
	if !ok {
		return 0, false
	}
	return pyParseInt(v, 10)
}

func clampInt(n int64) int {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	if n < math.MinInt32 {
		return math.MinInt32
	}
	return int(n)
}
