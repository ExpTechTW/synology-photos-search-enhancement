// Package server：搜尋服務。unix socket，一行 JSON 請求、一行 JSON 回應，給 DSM 端的 sse filter 呼叫。
//
//	{"op": "ping"}
//	{"op": "search", "q": "貓", "item_type": "all"}
//	→ {"ok": true, "owner_user_id": 1, "hits": [{"id": 1, "time": 1700000000, "type": "photo", ...}]}
package server

import (
	"bufio"
	"bytes"
	"container/list"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/config"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/db"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/llm"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/logs"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/search"
)

var log = logs.Get("sse.server")

const maxLine = 65536

// queryCache 是查詢字向量的 LRU 快取。
type queryCache struct {
	mu    sync.Mutex
	size  int
	order *list.List
	items map[string]*list.Element
}

type cacheEntry struct {
	key string
	vec []float64
}

func newQueryCache(size int) *queryCache {
	return &queryCache{size: size, order: list.New(), items: map[string]*list.Element{}}
}

func (c *queryCache) get(k string) []float64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.order.MoveToFront(e)
		return e.Value.(*cacheEntry).vec
	}
	return nil
}

func (c *queryCache) put(k string, v []float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		e.Value.(*cacheEntry).vec = v
		c.order.MoveToFront(e)
		return
	}
	c.items[k] = c.order.PushFront(&cacheEntry{k, v})
	for c.order.Len() > c.size {
		last := c.order.Back()
		c.order.Remove(last)
		delete(c.items, last.Value.(*cacheEntry).key)
	}
}

// Server 是搜尋服務。
type Server struct {
	cfg     *config.Config
	llm     *llm.Client
	Index   *search.Index
	cache   *queryCache
	started time.Time
	ready   chan struct{}

	statsMu sync.Mutex
	stats   map[string]int64
}

// New 建立搜尋服務。
func New(cfg *config.Config, client *llm.Client) *Server {
	return &Server{cfg: cfg, llm: client, Index: search.NewIndex(cfg.Index.DB, cfg.Search.Dims),
		cache: newQueryCache(cfg.Search.QueryCache), started: time.Now(), ready: make(chan struct{}),
		stats: map[string]int64{}}
}

// Ready 在 socket 開始接受連線後關閉。
func (s *Server) Ready() <-chan struct{} { return s.ready }

func (s *Server) count(k string) {
	s.statsMu.Lock()
	s.stats[k]++
	s.statsMu.Unlock()
}

func (s *Server) statsCopy() map[string]int64 {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	out := make(map[string]int64, len(s.stats))
	for k, v := range s.stats {
		out[k] = v
	}
	return out
}

// markSearch：讓索引器先別送新的圖片，模型服務留給這次查詢。
func (s *Server) markSearch() {
	path := db.SearchMarker(s.cfg.Index.DB)
	now := time.Now()
	if err := os.Chtimes(path, now, now); err != nil {
		if f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE, 0o644); err == nil {
			f.Close()
		}
	}
}

func (s *Server) embedQuery(q string) ([]float64, error) {
	s.markSearch()
	key := s.llm.EmbedModel + "\x00" + q
	if v := s.cache.get(key); v != nil {
		s.count("cache_hits")
		return v, nil
	}
	s.count("embed_calls")
	v, err := s.llm.EmbedQuery(q)
	if err != nil {
		return nil, err
	}
	if len(v) > s.cfg.Search.Dims { // 搜尋只用前 dims 維，快取也只留這些
		v = append([]float64(nil), v[:s.cfg.Search.Dims]...)
	}
	s.cache.put(key, v)
	return v, nil
}

type request struct {
	q        any
	itemType string
}

func (s *Server) ownerJSON() any {
	if o := s.Index.Owner(); o != nil {
		return *o
	}
	return nil
}

func (s *Server) doSearch(req request) map[string]any {
	q, ok := req.q.(string)
	if !ok || strings.TrimSpace(q) == "" || utf8.RuneCountInString(q) > 200 {
		return map[string]any{"ok": false, "error": "q 必須是 1～200 字的字串"}
	}
	t0 := time.Now()
	q = strings.TrimSpace(q)
	qv, err := s.embedQuery(q)
	if err != nil {
		s.count("llm_errors")
		return map[string]any{"ok": false, "error": "查詢 embedding 失敗：" + err.Error()}
	}
	itemType := req.itemType
	if itemType == "" {
		itemType = "all"
	}
	hits := s.Index.Search(qv, q, s.cfg.Search, itemType)
	s.count("searches")
	return map[string]any{"ok": true, "owner_user_id": s.ownerJSON(), "hits": hits,
		"took_ms": time.Since(t0).Milliseconds()}
}

func (s *Server) handle(conn net.Conn) {
	defer conn.Close()
	conn.SetDeadline(time.Now().Add(10 * time.Second))
	line, err := readLine(bufio.NewReaderSize(conn, 4096))
	if err != nil {
		return
	}
	var resp map[string]any
	var raw map[string]any
	if err := json.Unmarshal(line, &raw); err != nil || raw == nil {
		resp = map[string]any{"ok": false, "error": "請求不是 JSON 物件"}
	} else {
		op, _ := raw["op"].(string)
		req := request{q: raw["q"]}
		req.itemType, _ = raw["item_type"].(string)
		switch op {
		case "ping":
			resp = map[string]any{"ok": true, "owner_user_id": s.ownerJSON(), "items": s.Index.Items(),
				"rev": s.Index.Rev(), "uptime": int64(time.Since(s.started).Seconds()), "stats": s.statsCopy()}
		case "search":
			// 查詢字的 embedding 可能要等模型服務，放寬期限
			conn.SetDeadline(time.Now().Add(time.Duration(s.cfg.LLM.EmbedTimeout+10) * time.Second))
			resp = s.doSearch(req)
		default:
			resp = map[string]any{"ok": false, "error": "不支援的 op"}
		}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(resp); err != nil {
		s.count("errors")
		log.Errorf("處理請求失敗：%v", err)
		return
	}
	if _, err := conn.Write(buf.Bytes()); err != nil {
		s.count("errors")
	}
}

// readLine 讀一行（不含換行），超過 maxLine 就放棄。
func readLine(r *bufio.Reader) ([]byte, error) {
	var line []byte
	for {
		chunk, err := r.ReadSlice('\n')
		line = append(line, chunk...)
		if len(line) > maxLine {
			return nil, errors.New("請求太長")
		}
		switch {
		case err == nil, errors.Is(err, io.EOF) && len(line) > 0:
			return bytes.TrimRight(line, "\r\n"), nil
		case !errors.Is(err, bufio.ErrBufferFull):
			return nil, err
		}
	}
}

// ensureBank 取得通用查詢的向量（hub 校正用）。模型服務暫時連不上就先不校正，下一輪再試。
func (s *Server) ensureBank() {
	if s.Index.HasBank() {
		return
	}
	vecs := make([][]float64, 0, len(search.HubBank))
	for _, q := range search.HubBank {
		v, err := s.llm.EmbedQuery(q)
		if err != nil {
			log.Warnf("取不到通用查詢的向量，暫時不做 hub 校正：%v", err)
			return
		}
		vecs = append(vecs, v)
	}
	s.Index.SetBank(vecs)
	log.Infof("hub 校正已啟用")
}

func (s *Server) refreshLoop(stop <-chan struct{}) {
	t := time.NewTicker(time.Duration(max(1, s.cfg.Search.RefreshInterval)) * time.Second)
	defer t.Stop()
	for {
		s.ensureBank()
		if n, err := s.Index.Refresh(); err != nil {
			log.Errorf("讀取索引失敗：%v", err)
		} else if n > 0 {
			log.Infof("索引更新 %d 筆，共 %d 張（rev %d）", n, s.Index.Items(), s.Index.Rev())
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

// listen 建立 unix socket；舊的 socket 沒人在聽就刪掉。
func listen(path string) (net.Listener, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s 已存在且不是 socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s 已有其他程序在監聽", path)
		}
		os.Remove(path)
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	// 0660：只有服務本身與 nginx 群組（sse filter）能連
	if err := os.Chmod(path, 0o660); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Serve 提供搜尋服務直到 stop 被關閉。
func (s *Server) Serve(stop <-chan struct{}) error {
	path := s.cfg.Search.Socket
	if _, err := s.Index.Refresh(); err != nil {
		// 索引器還沒建立資料庫：先用空的索引啟動，之後定期重新讀取
		log.Warnf("讀取索引失敗（之後會再試）：%v", err)
	}
	log.Infof("載入 %d 張照片的向量（rev %d）", s.Index.Items(), s.Index.Rev())
	debug.FreeOSMemory() // 第一次載入時讀過整個資料庫，把用完的記憶體還給系統
	ln, err := listen(path)
	if err != nil {
		return err
	}
	ln.(*net.UnixListener).SetUnlinkOnClose(true)
	log.Infof("搜尋服務啟動：%s", path)
	close(s.ready)
	done := make(chan struct{})
	go s.refreshLoop(done)
	go func() {
		<-stop
		close(done)
		ln.Close()
	}()
	var wg sync.WaitGroup
	for {
		conn, err := ln.Accept()
		if err != nil {
			select {
			case <-done:
				wg.Wait()
				s.Index.Close()
				log.Infof("搜尋服務已停止")
				return nil
			default:
			}
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			time.Sleep(100 * time.Millisecond)
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.handle(conn)
		}()
	}
}
