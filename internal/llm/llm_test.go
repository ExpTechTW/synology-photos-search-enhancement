package llm

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCleanOCR(t *testing.T) {
	for in, want := range map[string]string{
		"NONE":                   "",
		"I cannot read this":     "",
		"```\nhello\nworld\n```": "hello\nworld",
		"第一行\n第二行\n第一行\n第三":           "第一行\n第二行\n第三",
		"哈\n哈\n哈\n哈":                  "",
		OCRPrompt:                     "",
		"text\n收據 38475629":           "收據 38475629",
		"The image shows a cat":       "",
		"The image\nshows\na receipt": "The image\nshows\na receipt", // 超過兩行就不是聊天式回答
	} {
		if got := CleanOCR(in); got != want {
			t.Errorf("CleanOCR(%q) = %q, want %q", in, got, want)
		}
	}
}

// embedding 與 OCR 各自一次只送一個請求；兩種可以同時各一個
func TestOneRequestAtATime(t *testing.T) {
	var inflight, maxEmbed, maxOCR, both int32
	var mu sync.Mutex
	active := map[string]int{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		kind := "ocr"
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			kind = "embed"
		}
		mu.Lock()
		active[kind]++
		if active["embed"] > 0 && active["ocr"] > 0 {
			atomic.StoreInt32(&both, 1)
		}
		if kind == "embed" && int32(active[kind]) > maxEmbed {
			maxEmbed = int32(active[kind])
		}
		if kind == "ocr" && int32(active[kind]) > maxOCR {
			maxOCR = int32(active[kind])
		}
		mu.Unlock()
		atomic.AddInt32(&inflight, 1)
		time.Sleep(20 * time.Millisecond)
		mu.Lock()
		active[kind]--
		mu.Unlock()
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		if kind == "embed" {
			if n := len(body["input"].([]any)); n != 1 {
				t.Errorf("一個請求帶了 %d 筆", n)
			}
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"index": 0, "embedding": []float64{0.1, 0.2}}}})
		} else {
			json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}}})
		}
	}))
	defer srv.Close()
	c := &Client{Embed: newEndpoint(srv.URL, "", 10), OCREp: newEndpoint(srv.URL, "", 10), EmbedModel: "e",
		OCRModel: "o", LockDir: t.TempDir()}
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); c.EmbedQuery("cat") }()
		go func() { defer wg.Done(); c.OCR([]byte("x"), "image/png") }()
	}
	wg.Wait()
	if maxEmbed != 1 || maxOCR != 1 {
		t.Errorf("同時處理的請求：embedding %d、OCR %d，都應該是 1", maxEmbed, maxOCR)
	}
	if both != 1 {
		t.Error("embedding 與 OCR 應該可以各有一個同時進行")
	}
}

func TestErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sk-1" {
			w.WriteHeader(401)
			return
		}
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			w.Write([]byte(`{"data":[{"index":0,"embedding":[NaN, 0.1]}]}`))
			return
		}
		w.WriteHeader(503)
		w.Write([]byte("busy"))
	}))
	defer srv.Close()
	c := &Client{Embed: newEndpoint(srv.URL, "sk-1", 10), OCREp: newEndpoint(srv.URL, "sk-1", 10)}
	if _, err := c.EmbedQuery("x"); err == nil || !strings.Contains(err.Error(), "NaN") {
		t.Errorf("NaN：%v", err)
	}
	_, err := c.OCR([]byte("x"), "image/png")
	if e, ok := err.(*Error); !ok || !e.Transient || !strings.Contains(e.Msg, "HTTP 503") {
		t.Errorf("503 要算服務暫時有問題：%v", err)
	}
	c.Embed.Key = ""
	_, err = c.EmbedQuery("x")
	if e, ok := err.(*Error); !ok || e.Transient {
		t.Errorf("401 不是暫時的問題：%v", err)
	}
	c.Embed.Base = "http://127.0.0.1:1"
	_, err = c.EmbedQuery("x")
	if e, ok := err.(*Error); !ok || !e.Transient {
		t.Errorf("連不上要算暫時的問題：%v", err)
	}
}
