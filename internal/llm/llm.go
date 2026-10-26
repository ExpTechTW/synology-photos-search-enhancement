// Package llm：OpenAI 相容 API 的 client：圖片與文字 embedding、OCR。embedding 與 OCR 可以是不同的服務。
//
// 圖片 embedding 用的是 llama.cpp 的 multimodal prompt 格式（OpenAI 的 image_url 不適用於 /v1/embeddings）：
//
//	{"input": [{"prompt_string": "<__media__>", "multimodal_data": ["<base64>"]}]}
//
// llama-server 要用 LLAMA_MEDIA_MARKER 固定 marker，否則每次啟動都是隨機的。
//
// 不對模型服務併發：embedding 與 OCR 各自一次只送一個請求、一個請求只有一張圖或一段文字，
// 上一個完成才送下一個。索引器與搜尋服務是不同的 process，用資料目錄裡的鎖檔排隊。
package llm

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/config"
)

const (
	QueryPrefix = "task: search result | query: "
	// DocPrefix：文件（照片裡的文字）的前綴。EmbeddingGemma 的檢索是「查詢前綴 vs 文件前綴」，兩邊都要加
	DocPrefix   = "title: none | text: "
	DocMaxChars = 2000 // 文字很多的截圖只取前面一段：太長的文件向量會被稀釋，也比較慢
	OCRPrompt   = "Transcribe all legible text in this image exactly as written, line by line, keeping the original " +
		"language. Output only the text. If there is no legible text, output NONE."
)

// Error：Transient 表示服務暫時有問題（連不上、逾時、5xx、429），不是這張照片的問題。
type Error struct {
	Msg       string
	Transient bool
}

func (e *Error) Error() string { return e.Msg }

// Endpoint 是一個 OpenAI 相容的服務。
type Endpoint struct {
	Base, Key string
	http      *http.Client
}

func newEndpoint(base, key string, timeout int) *Endpoint {
	return &Endpoint{Base: strings.TrimRight(base, "/"), Key: key,
		http: &http.Client{Timeout: time.Duration(timeout) * time.Second}}
}

// Client 呼叫 embedding 與 OCR 服務。
type Client struct {
	Embed, OCREp *Endpoint
	EmbedModel   string
	OCRModel     string
	MediaMarker  string
	OCRExtraBody map[string]any
	OCRMaxTokens int
	LockDir      string // 空白 = 不用鎖檔（測試）
	// Ctx：每個 HTTP 請求都跟著它取消（nil 等同 context.Background()）。索引器收到 SIGTERM 時用來立刻停。
	Ctx context.Context
}

// FromConfig 依設定建立 client；鎖檔放在資料庫旁邊。
func FromConfig(cfg *config.Config) *Client {
	c := cfg.LLM
	return &Client{
		Embed:      newEndpoint(c.EmbedBaseURL, c.EmbedAPIKey, c.EmbedTimeout),
		OCREp:      newEndpoint(c.OCRBaseURL, c.OCRAPIKey, c.OCRTimeout),
		EmbedModel: c.EmbedModel, OCRModel: c.OCRModel, MediaMarker: c.MediaMarker,
		OCRExtraBody: c.OCRExtraBody, OCRMaxTokens: c.OCRMaxTokens,
		LockDir: filepath.Dir(cfg.Index.DB),
	}
}

// oneAtATime 讓同一種請求排隊送。每次開新的檔案描述子再 flock：同一個 process 的不同 goroutine 之間也會排隊。
func (c *Client) oneAtATime(kind string) func() {
	if c.LockDir == "" {
		return func() {}
	}
	os.MkdirAll(c.LockDir, 0o755)
	// 唯讀開啟：鎖檔萬一被 root 建立，照片擁有者照樣能開來上鎖
	f, err := os.OpenFile(filepath.Join(c.LockDir, kind+".lock"), os.O_RDONLY|os.O_CREATE, 0o644)
	if err != nil {
		return func() {}
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX)
		if err != syscall.EINTR {
			break
		}
	}
	return func() { f.Close() }
}

func (c *Client) ctx() context.Context {
	if c.Ctx != nil {
		return c.Ctx
	}
	return context.Background()
}

func (c *Client) post(ep *Endpoint, path string, body any) (map[string]any, error) {
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(c.ctx(), "POST", ep.Base+path, bytes.NewReader(data))
	if err != nil {
		return nil, &Error{Msg: fmt.Sprintf("%s 連線失敗：%v", path, err), Transient: true}
	}
	req.Header.Set("Content-Type", "application/json")
	if ep.Key != "" {
		req.Header.Set("Authorization", "Bearer "+ep.Key)
	}
	resp, err := ep.http.Do(req)
	if err != nil {
		return nil, &Error{Msg: fmt.Sprintf("%s 連線失敗：%v", path, err), Transient: true}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &Error{Msg: fmt.Sprintf("%s 連線失敗：%v", path, err), Transient: true}
	}
	if resp.StatusCode >= 400 {
		detail := raw
		if len(detail) > 500 {
			detail = detail[:500]
		}
		return nil, &Error{Msg: fmt.Sprintf("%s 回應 HTTP %d：%s", path, resp.StatusCode, strings.ToValidUTF8(string(detail), "?")),
			Transient: resp.StatusCode >= 500 || resp.StatusCode == 429}
	}
	var d map[string]any
	if err := json.Unmarshal(raw, &d); err != nil {
		// EmbeddingGemma 在 FP16 下會輸出 NaN（JSON 裡寫成 NaN，標準 JSON 不允許）
		if bytes.Contains(raw, []byte("NaN")) {
			return nil, nanError()
		}
		return nil, &Error{Msg: fmt.Sprintf("%s 回應不是 JSON：%v", path, err), Transient: true}
	}
	return d, nil
}

func nanError() *Error {
	return &Error{Msg: "embedding 含 NaN：EmbeddingGemma 不能用 FP16 執行，請改用 BF16 或 F32（見 docs/models.md）"}
}

// embed 回傳一張圖或一段文字的向量。
func (c *Client) embed(input any) ([]float64, error) {
	unlock := c.oneAtATime("embed")
	d, err := c.post(c.Embed, "/embeddings", map[string]any{"model": c.EmbedModel, "input": []any{input}})
	unlock()
	if err != nil {
		return nil, err
	}
	data, _ := d["data"].([]any)
	if len(data) != 1 {
		return nil, &Error{Msg: fmt.Sprintf("embedding 回傳 %d 筆，預期 1 筆", len(data))}
	}
	first, _ := data[0].(map[string]any)
	raw, _ := first["embedding"].([]any)
	if len(raw) == 0 {
		return nil, &Error{Msg: "embedding 回應裡沒有向量"}
	}
	v := make([]float64, len(raw))
	for i, x := range raw {
		f, ok := x.(float64)
		if !ok || math.IsNaN(f) {
			return nil, nanError()
		}
		v[i] = f
	}
	return v, nil
}

// EmbedImage：圖片的向量。
func (c *Client) EmbedImage(image []byte) ([]float64, error) {
	return c.embed(map[string]any{"prompt_string": c.MediaMarker,
		"multimodal_data": []string{base64.StdEncoding.EncodeToString(image)}})
}

// EmbedQuery：搜尋字的向量。
func (c *Client) EmbedQuery(text string) ([]float64, error) { return c.embed(QueryPrefix + text) }

// EmbedText：照片裡文字的向量（只取前 DocMaxChars 個字）。
func (c *Client) EmbedText(text string) ([]float64, error) {
	if utf8.RuneCountInString(text) > DocMaxChars {
		text = string([]rune(text)[:DocMaxChars])
	}
	return c.embed(DocPrefix + text)
}

// OCR 讀出圖片裡的文字（已清掉雜訊）。
func (c *Client) OCR(image []byte, mime string) (string, error) {
	body := map[string]any{
		"model": c.OCRModel, "temperature": 0, "max_tokens": c.OCRMaxTokens,
		"messages": []any{map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{
				"url": "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(image)}},
			map[string]any{"type": "text", "text": OCRPrompt},
		}}},
	}
	for k, v := range c.OCRExtraBody { // 例如會思考的模型要 {"reasoning_effort": "none"}，否則又慢又貴
		body[k] = v
	}
	unlock := c.oneAtATime("ocr")
	d, err := c.post(c.OCREp, "/chat/completions", body)
	unlock()
	if err != nil {
		return "", err
	}
	choices, _ := d["choices"].([]any)
	if len(choices) == 0 {
		return "", ocrFormatError(d)
	}
	choice, _ := choices[0].(map[string]any)
	msg, ok := choice["message"].(map[string]any)
	if !ok {
		return "", ocrFormatError(d)
	}
	text, _ := msg["content"].(string)
	// 文字很多的截圖可能超過上限：保留已經讀到的部分（對搜尋一樣有用），不當成失敗
	return CleanOCR(strings.TrimSpace(text)), nil
}

func ocrFormatError(d map[string]any) error {
	b, _ := json.Marshal(d)
	s := string(b)
	if len(s) > 200 {
		s = s[:200]
	}
	return &Error{Msg: "OCR 回應格式不對：" + s}
}

// 模型偶爾不轉錄，而是回一句話（例如圖片太小或全白時）
var chatter = regexp.MustCompile(`(?i)^(please provide|i (can(no|')t|am unable|'m unable|'m sorry)|sorry|there (is|are) no|` +
	`no (legible )?text|the image|this image|none\b)`)

// CleanOCR 清掉 OCR 的雜訊：聊天式回答、回傳提示詞本身、程式碼區塊標記、重複的行。
//
// 重複的行只留第一次出現的：模型有時會陷入迴圈重複同一段直到字數上限（幻覺），
// 而搜尋是子字串比對，同一行出現幾次都一樣。
func CleanOCR(text string) string {
	if strings.Contains(text, "Transcribe all legible text") {
		return ""
	}
	var lines []string
	for _, ln := range strings.Split(strings.Trim(strings.TrimSpace(text), "`"), "\n") {
		ln = strings.TrimSpace(ln)
		if ln != "" && strings.ToLower(ln) != "text" && strings.ToLower(ln) != "plaintext" {
			lines = append(lines, ln)
		}
	}
	if len(lines) == 0 || (len(lines) <= 2 && chatter.MatchString(lines[0])) {
		return ""
	}
	var out []string
	seen := map[string]bool{}
	for _, ln := range lines {
		if !seen[ln] {
			seen[ln] = true
			out = append(out, ln)
		}
	}
	if len(out) == 1 && len(lines) >= 3 && utf8.RuneCountInString(out[0]) <= 2 {
		return "" // 同一個字重複好幾行，多半是幻覺
	}
	return strings.Join(out, "\n")
}
