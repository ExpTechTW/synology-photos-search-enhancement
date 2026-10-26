package filter

import (
	"bytes"
	"encoding/json"
	"strconv"
	"strings"
	"unicode/utf16"
)

// JSON 小工具。改寫回應時只動需要改的欄位，其餘欄位原樣保留 DSM 回傳的位元組
// （順序、數字寫法都不變），也不必把整份回應解成 map。
//
// 這裡的函式都假設輸入是已經檢查過的合法 JSON（json.Valid），但仍會檢查邊界，不會 panic。

// kindOf 回傳 JSON 值的第一個字元：'{'、'['、'"'、't'、'f'、'n'，數字則是 '-' 或數字；空的回傳 0。
func kindOf(raw []byte) byte {
	i := skipWS(raw, 0)
	if i >= len(raw) {
		return 0
	}
	return raw[i]
}

func skipWS(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

func trimRaw(raw []byte) []byte {
	i := skipWS(raw, 0)
	j := len(raw)
	for j > i && (raw[j-1] == ' ' || raw[j-1] == '\t' || raw[j-1] == '\n' || raw[j-1] == '\r') {
		j--
	}
	return raw[i:j]
}

// valueEnd 回傳 b[i] 開始的 JSON 值結束的位置；格式不對時回傳 -1。
func valueEnd(b []byte, i int) int {
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		for i++; i < len(b); i++ {
			switch b[i] {
			case '\\':
				i++
			case '"':
				return i + 1
			}
		}
		return -1
	case '{', '[':
		depth := 0
		for ; i < len(b); i++ {
			switch b[i] {
			case '"':
				j := valueEnd(b, i)
				if j < 0 {
					return -1
				}
				i = j - 1
			case '{', '[':
				depth++
			case '}', ']':
				depth--
				if depth == 0 {
					return i + 1
				}
			}
		}
		return -1
	default:
		j := i
		for ; j < len(b); j++ {
			switch b[j] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				if j == i {
					return -1
				}
				return j
			}
		}
		return j
	}
}

// object 是保留欄位順序的 JSON 物件；值是原始 JSON。重複的欄位跟 Python 的 dict 一樣：位置在前、值取後者。
type object struct {
	keys []string
	vals map[string]json.RawMessage
}

func (o *object) get(k string) json.RawMessage {
	if o == nil {
		return nil
	}
	return o.vals[k]
}

func (o *object) set(k string, v json.RawMessage) {
	if _, ok := o.vals[k]; !ok {
		o.keys = append(o.keys, k)
	}
	o.vals[k] = v
}

func (o *object) marshal() []byte {
	n := 2
	for _, k := range o.keys {
		n += len(k) + len(o.vals[k]) + 4
	}
	b := make([]byte, 0, n)
	b = append(b, '{')
	for i, k := range o.keys {
		if i > 0 {
			b = append(b, ',')
		}
		b = appendString(b, k)
		b = append(b, ':')
		b = append(b, o.vals[k]...)
	}
	return append(b, '}')
}

func parseObject(raw []byte) (*object, bool) {
	b := raw
	i := skipWS(b, 0)
	if i >= len(b) || b[i] != '{' {
		return nil, false
	}
	o := &object{vals: map[string]json.RawMessage{}}
	i = skipWS(b, i+1)
	if i < len(b) && b[i] == '}' {
		return o, true
	}
	for {
		if i >= len(b) || b[i] != '"' {
			return nil, false
		}
		j := valueEnd(b, i)
		if j < 0 {
			return nil, false
		}
		key, ok := decodeString(b[i:j])
		if !ok {
			return nil, false
		}
		i = skipWS(b, j)
		if i >= len(b) || b[i] != ':' {
			return nil, false
		}
		i = skipWS(b, i+1)
		j = valueEnd(b, i)
		if j < 0 {
			return nil, false
		}
		o.set(key, json.RawMessage(b[i:j]))
		i = skipWS(b, j)
		if i >= len(b) {
			return nil, false
		}
		switch b[i] {
		case ',':
			i = skipWS(b, i+1)
		case '}':
			return o, true
		default:
			return nil, false
		}
	}
}

func parseArray(raw []byte) ([]json.RawMessage, bool) {
	b := raw
	i := skipWS(b, 0)
	if i >= len(b) || b[i] != '[' {
		return nil, false
	}
	out := []json.RawMessage{}
	i = skipWS(b, i+1)
	if i < len(b) && b[i] == ']' {
		return out, true
	}
	for {
		j := valueEnd(b, i)
		if j < 0 {
			return nil, false
		}
		out = append(out, json.RawMessage(b[i:j]))
		i = skipWS(b, j)
		if i >= len(b) {
			return nil, false
		}
		switch b[i] {
		case ',':
			i = skipWS(b, i+1)
		case ']':
			return out, true
		default:
			return nil, false
		}
	}
}

func joinArray(items []json.RawMessage) []byte {
	n := 2
	for _, it := range items {
		n += len(it) + 1
	}
	b := make([]byte, 0, n)
	b = append(b, '[')
	for i, it := range items {
		if i > 0 {
			b = append(b, ',')
		}
		b = append(b, it...)
	}
	return append(b, ']')
}

func decodeString(raw []byte) (string, bool) {
	raw = trimRaw(raw)
	if len(raw) < 2 || raw[0] != '"' || raw[len(raw)-1] != '"' {
		return "", false
	}
	if bytes.IndexByte(raw, '\\') < 0 {
		return string(raw[1 : len(raw)-1]), true
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// rawString：值是 JSON 字串時回傳內容。
func rawString(raw []byte) (string, bool) {
	if kindOf(raw) != '"' {
		return "", false
	}
	return decodeString(raw)
}

// rawInt：值是 JSON 的整數（Python 的 int，不含 true / false 與 1.0 這種小數）時回傳數值。
func rawInt(raw []byte) (int64, bool) {
	s := trimRaw(raw)
	if len(s) == 0 {
		return 0, false
	}
	for i, c := range s {
		if !(c >= '0' && c <= '9' || c == '-' && i == 0) {
			return 0, false
		}
	}
	n, err := strconv.ParseInt(string(s), 10, 64)
	return n, err == nil
}

// rawNumber：值是 JSON 的數字（整數或小數，不含 true / false）時回傳數值。
func rawNumber(raw []byte) (float64, bool) {
	k := kindOf(raw)
	if k != '-' && (k < '0' || k > '9') {
		return 0, false
	}
	f, err := strconv.ParseFloat(string(trimRaw(raw)), 64)
	if err != nil {
		if ne, ok := err.(*strconv.NumError); !ok || ne.Err != strconv.ErrRange {
			return 0, false
		}
	}
	return f, true
}

func rawIsTrue(raw []byte) bool { return string(trimRaw(raw)) == "true" }

// pyTruthy 是 Python 對 JSON 值做 bool() 的結果（沒有值等於 None）。
func pyTruthy(raw []byte) bool {
	switch kindOf(raw) {
	case 0, 'n', 'f':
		return false
	case 't':
		return true
	case '"':
		s, _ := decodeString(raw)
		return s != ""
	case '[':
		a, _ := parseArray(raw)
		return len(a) > 0
	case '{':
		o, _ := parseObject(raw)
		return o != nil && len(o.keys) > 0
	}
	f, _ := rawNumber(raw)
	return f != 0
}

// pyStr 近似 Python 的 str(值)，給錯誤訊息用（沒有值是 None）。
func pyStr(raw []byte) string {
	switch kindOf(raw) {
	case 0, 'n':
		return "None"
	case 't':
		return "True"
	case 'f':
		return "False"
	case '"':
		s, _ := decodeString(raw)
		return s
	}
	return string(compactRaw(raw))
}

func compactRaw(raw []byte) json.RawMessage {
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		return json.RawMessage(trimRaw(raw))
	}
	return json.RawMessage(buf.Bytes())
}

// appendString 把字串寫成 JSON 字串，非 ASCII 字元一律寫成 \uXXXX（跟 Python 的 ensure_ascii=True 相同）。
func appendString(b []byte, s string) []byte {
	const hex = "0123456789abcdef"
	b = append(b, '"')
	for _, r := range s {
		switch {
		case r == '"':
			b = append(b, '\\', '"')
		case r == '\\':
			b = append(b, '\\', '\\')
		case r == '\n':
			b = append(b, '\\', 'n')
		case r == '\r':
			b = append(b, '\\', 'r')
		case r == '\t':
			b = append(b, '\\', 't')
		case r == '\b':
			b = append(b, '\\', 'b')
		case r == '\f':
			b = append(b, '\\', 'f')
		case r >= 0x20 && r < 0x7f:
			b = append(b, byte(r))
		case r < 0x10000:
			b = append(b, '\\', 'u', hex[r>>12&0xf], hex[r>>8&0xf], hex[r>>4&0xf], hex[r&0xf])
		default:
			r1, r2 := utf16.EncodeRune(r)
			for _, x := range []rune{r1, r2} {
				b = append(b, '\\', 'u', hex[x>>12&0xf], hex[x>>8&0xf], hex[x>>4&0xf], hex[x&0xf])
			}
		}
	}
	return append(b, '"')
}

// marshalPlain 用 encoding/json 輸出，不把 < > & 轉成 <（跟 Python 的 json.dumps 一樣）。
func marshalPlain(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// idList 跟 Python 的 json.dumps(整數清單) 相同："[1, 2, 3]"。
func idList(ids []int64) string {
	var sb strings.Builder
	sb.WriteByte('[')
	for i, id := range ids {
		if i > 0 {
			sb.WriteString(", ")
		}
		sb.WriteString(strconv.FormatInt(id, 10))
	}
	sb.WriteByte(']')
	return sb.String()
}
