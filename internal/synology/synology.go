// Package synology：Synology Photos WebAPI client，只用讀取類 API。
//
// 用到的 API：
//
//	SYNO.API.Auth                    login / logout
//	SYNO.Foto.UserInfo               me（確認索引的是哪個使用者）
//	SYNO.Foto.Browse.Item            list、count（完整掃描）
//	SYNO.Foto.Browse.RecentlyAdded   list（快速掃描新照片）
//	SYNO.Foto.Thumbnail              get（只抓縮圖，不抓原檔）
package synology

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// session 失效、需要重新登入的錯誤碼
var sessionErrors = map[int]bool{105: true, 106: true, 107: true, 119: true}

var loginErrors = map[int]string{
	400: "帳號或密碼錯誤",
	401: "帳號已停用",
	402: "沒有權限",
	403: "需要兩步驟驗證（請改用沒有開 2FA 的帳號，或關閉 2FA）",
	404: "兩步驟驗證碼錯誤",
	407: "IP 被封鎖（登入失敗太多次）",
}

// MaxImage 是縮圖的大小上限。
const MaxImage = 12 * 1024 * 1024

// Error：Transient 表示整體性的問題（session、登入、DSM 暫時有狀況），不是某張照片的問題。
type Error struct {
	API, Method string
	Code        int // 0 = 沒有錯誤碼（例如 HTTP 錯誤）
	Detail      string
	Transient   bool
}

func (e *Error) Error() string {
	msg := e.API + "." + e.Method + " 失敗"
	if e.Code != 0 {
		msg += "（錯誤碼 " + strconv.Itoa(e.Code) + "）"
	}
	if e.Detail != "" {
		msg += "：" + e.Detail
	}
	return msg
}

func apiError(api, method string, code int, detail string) *Error {
	_, login := loginErrors[code]
	return &Error{API: api, Method: method, Code: code, Detail: detail, Transient: sessionErrors[code] || login}
}

// ImageMime 從檔頭判斷圖片格式；不認得回傳空字串。
func ImageMime(data []byte) string {
	switch {
	case bytes.HasPrefix(data, []byte("\xff\xd8\xff")):
		return "image/jpeg"
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	case bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")):
		return "image/gif" // 動圖：DSM 直接回原檔，不是縮圖
	case len(data) >= 12 && string(data[:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

// Client 是 DSM WebAPI 的 client（可以在多個 goroutine 裡同時使用）。
type Client struct {
	url, account, password string
	http                   *http.Client

	mu  sync.Mutex
	sid string
}

// New 建立 client。verifyTLS 為 false 時接受自簽憑證（連 127.0.0.1 的 DSM 時）。
func New(baseURL, account, password string, verifyTLS bool, timeout time.Duration) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if !verifyTLS {
		tr.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
	}
	return &Client{url: strings.TrimRight(baseURL, "/"), account: account, password: password,
		http: &http.Client{Transport: tr, Timeout: timeout}}
}

// post 回傳 JSON 解出的內容；raw 為 true 且回應不是 JSON 時回傳原始 bytes。
func (c *Client) post(params url.Values, raw bool) (map[string]any, []byte, error) {
	api, method := params.Get("api"), params.Get("method")
	resp, err := c.http.Post(c.url+"/webapi/entry.cgi", "application/x-www-form-urlencoded",
		strings.NewReader(params.Encode()))
	if err != nil {
		return nil, nil, &Error{API: api, Method: method, Detail: err.Error(), Transient: true}
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, MaxImage+1))
	if err != nil {
		return nil, nil, &Error{API: api, Method: method, Detail: err.Error(), Transient: true}
	}
	if resp.StatusCode >= 400 {
		// 4xx（例如某部影片沒有縮圖時的 404）是這個請求的問題；5xx 與 429 才是 DSM 暫時有狀況
		return nil, nil, &Error{API: api, Method: method, Detail: "HTTP " + strconv.Itoa(resp.StatusCode),
			Transient: resp.StatusCode >= 500 || resp.StatusCode == 429}
	}
	if raw && !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		return nil, data, nil
	}
	var d map[string]any
	if err := json.Unmarshal(data, &d); err != nil {
		return nil, nil, &Error{API: api, Method: method, Detail: "回應不是 JSON", Transient: true}
	}
	return d, nil, nil
}

func errorCode(d map[string]any) int {
	if e, ok := d["error"].(map[string]any); ok {
		if code, ok := e["code"].(float64); ok {
			return int(code)
		}
	}
	return 0
}

// Login 登入（session 名稱跟 Photos 相同）。
func (c *Client) Login() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loginLocked()
}

func (c *Client) loginLocked() error {
	d, _, err := c.post(url.Values{"api": {"SYNO.API.Auth"}, "version": {"6"}, "method": {"login"},
		"account": {c.account}, "passwd": {c.password}, "session": {"SynologyPhotos"}, "format": {"sid"}}, false)
	if err != nil {
		return err
	}
	if ok, _ := d["success"].(bool); !ok {
		code := errorCode(d)
		return apiError("SYNO.API.Auth", "login", code, loginErrors[code])
	}
	data, _ := d["data"].(map[string]any)
	sid, _ := data["sid"].(string)
	if sid == "" {
		return &Error{API: "SYNO.API.Auth", Method: "login", Detail: "回應裡沒有 sid", Transient: true}
	}
	c.sid = sid
	return nil
}

// Logout 登出（失敗也不管）。
func (c *Client) Logout() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sid != "" {
		c.post(url.Values{"api": {"SYNO.API.Auth"}, "version": {"6"}, "method": {"logout"},
			"session": {"SynologyPhotos"}, "_sid": {c.sid}}, false)
		c.sid = ""
	}
}

func (c *Client) session() (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sid == "" {
		if err := c.loginLocked(); err != nil {
			return "", err
		}
	}
	return c.sid, nil
}

func (c *Client) dropSession(sid string) {
	c.mu.Lock()
	if c.sid == sid {
		c.sid = ""
	}
	c.mu.Unlock()
}

// Call 呼叫 API；session 失效時重新登入再試一次。參數值不是字串的會轉成 JSON。
func (c *Client) Call(api string, version int, method string, params map[string]any, raw bool) (map[string]any, []byte, error) {
	p := url.Values{"api": {api}, "version": {strconv.Itoa(version)}, "method": {method}}
	for k, v := range params {
		if s, ok := v.(string); ok {
			p.Set(k, s)
		} else {
			b, _ := json.Marshal(v)
			p.Set(k, string(b))
		}
	}
	for attempt := 1; ; attempt++ {
		sid, err := c.session()
		if err != nil {
			return nil, nil, err
		}
		p.Set("_sid", sid)
		d, body, err := c.post(p, raw)
		if err != nil || body != nil {
			return nil, body, err
		}
		if ok, _ := d["success"].(bool); ok {
			data, _ := d["data"].(map[string]any)
			return data, nil, nil
		}
		code := errorCode(d)
		if sessionErrors[code] && attempt == 1 {
			c.dropSession(sid)
			continue
		}
		return nil, nil, apiError(api, method, code, "")
	}
}

// Me 回傳目前登入的使用者 id 與名稱。
func (c *Client) Me() (int64, string, error) {
	d, _, err := c.Call("SYNO.Foto.UserInfo", 1, "me", nil, false)
	if err != nil {
		return 0, "", err
	}
	id, ok := d["id"].(float64)
	if !ok {
		id, ok = d["user_id"].(float64)
	}
	if !ok {
		b, _ := json.Marshal(d)
		return 0, "", &Error{API: "SYNO.Foto.UserInfo", Method: "me", Detail: "回應裡沒有使用者 id：" + truncate(string(b), 200)}
	}
	name, _ := d["name"].(string)
	if name == "" {
		name = c.account
	}
	return int64(id), name, nil
}

// ListItems：完整掃描，依拍攝時間新到舊。
func (c *Client) ListItems(offset, limit int) ([]map[string]any, error) {
	d, _, err := c.Call("SYNO.Foto.Browse.Item", 1, "list", map[string]any{"offset": offset, "limit": limit,
		"additional": []string{"thumbnail"}, "sort_by": "takentime", "sort_direction": "desc"}, false)
	return list(d), err
}

// CountItems：照片總數。
func (c *Client) CountItems() (int64, error) {
	d, _, err := c.Call("SYNO.Foto.Browse.Item", 1, "count", nil, false)
	if err != nil {
		return 0, err
	}
	n, _ := d["count"].(float64)
	return int64(n), nil
}

// ListRecent：最近加入的照片（快速掃描）。
func (c *Client) ListRecent(offset, limit int) ([]map[string]any, error) {
	d, _, err := c.Call("SYNO.Foto.Browse.RecentlyAdded", 1, "list", map[string]any{"offset": offset, "limit": limit,
		"additional": []string{"thumbnail"}}, false)
	return list(d), err
}

func list(d map[string]any) []map[string]any {
	raw, _ := d["list"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, x := range raw {
		if m, ok := x.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// Thumbnail 回傳 (圖片, MIME)。格式不認得、太大都算這張照片的問題（不是服務問題）。
func (c *Client) Thumbnail(unitID int64, cacheKey, size string) ([]byte, string, error) {
	_, data, err := c.Call("SYNO.Foto.Thumbnail", 2, "get", map[string]any{"id": unitID, "cache_key": cacheKey,
		"type": "unit", "size": size}, true)
	if err != nil {
		return nil, "", err
	}
	if data == nil {
		return nil, "", &Error{API: "SYNO.Foto.Thumbnail", Method: "get", Detail: "回應不是圖片"}
	}
	mime := ImageMime(data)
	if mime == "" {
		head := data
		if len(head) > 8 {
			head = head[:8]
		}
		return nil, "", &Error{API: "SYNO.Foto.Thumbnail", Method: "get", Detail: fmt.Sprintf("不認得的圖片格式（%q）", head)}
	}
	if len(data) > MaxImage {
		return nil, "", &Error{API: "SYNO.Foto.Thumbnail", Method: "get", Detail: fmt.Sprintf("圖片太大（%d bytes）", len(data))}
	}
	return data, mime, nil
}

// Item 是索引需要的欄位。
type Item struct {
	UnitID, OwnerUserID, Time int64
	Type, CacheKey            string
	ThumbReady                bool
}

// ParseItem 把 Browse.Item.list 的一筆轉成 Item；格式不對回傳 false。
func ParseItem(raw map[string]any) (Item, bool) {
	id, ok1 := whole(raw["id"])
	t, ok2 := whole(raw["time"])
	owner, ok3 := whole(raw["owner_user_id"])
	add, _ := raw["additional"].(map[string]any)
	thumb, _ := add["thumbnail"].(map[string]any)
	ck, ok4 := thumb["cache_key"].(string)
	if !(ok1 && ok2 && ok3 && ok4) {
		return Item{}, false
	}
	typ, _ := raw["type"].(string)
	if typ == "" {
		typ = "photo"
	}
	return Item{UnitID: id, OwnerUserID: owner, Time: t, Type: typ, CacheKey: ck,
		ThumbReady: thumb["xl"] == "ready" || thumb["m"] == "ready"}, true
}

func whole(v any) (int64, bool) {
	f, ok := v.(float64)
	if !ok || f != float64(int64(f)) {
		return 0, false
	}
	return int64(f), true
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
