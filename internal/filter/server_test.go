package filter

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// 假 synoscgi：搜尋回固定的原生結果（只有 id 1），其他請求回 body 的長度與 sha256。
func fakeUpstream(t *testing.T, path string) {
	l, err := net.Listen("unix", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				n, err := r.ReadString(':')
				if err != nil {
					return
				}
				hlen, _ := strconv.Atoi(strings.TrimSuffix(n, ":"))
				head := make([]byte, hlen+1)
				if _, err := io.ReadFull(r, head); err != nil {
					return
				}
				parts := strings.Split(string(head[:hlen]), "\x00")
				h := map[string]string{}
				for i := 0; i+1 < len(parts); i += 2 {
					h[parts[i]] = parts[i+1]
				}
				clen, _ := strconv.Atoi(h["CONTENT_LENGTH"])
				sum := sha256.New()
				var small bytes.Buffer
				if _, err := io.CopyN(io.MultiWriter(sum, &limited{&small, 65536}), r, int64(clen)); err != nil {
					return
				}
				var body string
				if strings.Contains(small.String(), "SYNO.Foto.Search.Search") {
					body = `{"data":{"list":[{"id":1,"owner_user_id":1,"time":1700000000,"type":"photo"}]},"success":true}`
				} else {
					body = fmt.Sprintf(`{"data":{"len":%d,"sha256":"%s"},"success":true}`, clen, hex.EncodeToString(sum.Sum(nil)))
				}
				fmt.Fprintf(c, "Status: 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
			}()
		}
	}()
}

type limited struct {
	w io.Writer
	n int
}

func (l *limited) Write(p []byte) (int, error) {
	if l.n > 0 {
		k := min(l.n, len(p))
		l.w.Write(p[:k])
		l.n -= k
	}
	return len(p), nil
}

// 同時有很多連線（搜尋注入、上傳）時結果都正確；用 go test -race 跑可以抓到 goroutine 之間的競爭。
func TestConcurrentConnections(t *testing.T) {
	base := os.Getenv("SSE_TEST_TMP")
	if base == "" {
		base = "/tmp" // macOS 的 unix socket 路徑上限 104 bytes，t.TempDir() 太長
	}
	d, err := os.MkdirTemp(base, "sse")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	os.WriteFile(filepath.Join(d, "INFO"), []byte("version=\"1.9.0-1\"\n"), 0o644)
	cfgJSON := fmt.Sprintf(`{"listen": %q, "upstream": %q, "status_file": %q, "photos_info": %q, "kill_switch": %q,
		"mode": "demo", "inject_photos_version": "1.9.0-1", "capture_file": %q,
		"demo": {"keyword": "test", "items": [%s]}}`,
		d+"/f.sock", d+"/up.sock", d+"/status.json", d+"/INFO", d+"/DISABLE", d+"/cap.jsonl", demoItemJSON)
	cfg, msg, _ := parseConfig([]byte(cfgJSON))
	if msg != "" {
		t.Fatal(msg)
	}
	fakeUpstream(t, cfg.Upstream)
	f := newFilter(cfg, "")
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: cfg.Listen, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	go f.acceptLoop(l)
	defer l.Close()

	request := func(h []hdr, body string) (string, error) {
		c, err := net.Dial("unix", cfg.Listen)
		if err != nil {
			return "", err
		}
		defer c.Close()
		if _, err := c.Write(scgi(h, body)); err != nil {
			return "", err
		}
		out, err := io.ReadAll(c)
		_, b, _ := strings.Cut(string(out), "\r\n\r\n")
		return b, err
	}
	var wg sync.WaitGroup
	errs := make(chan error, 100)
	for i := range 40 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if i%2 == 0 {
				b, err := request(form(), search)
				if err == nil && !strings.HasPrefix(b, `{"data":{"list":[{"id":90001,`) {
					err = fmt.Errorf("沒有注入：%s", b)
				}
				errs <- err
				return
			}
			upload := strings.Repeat("x", 300000+i)
			sum := sha256.Sum256([]byte(upload))
			b, err := request([]hdr{{"REQUEST_METHOD", "POST"}, {"REQUEST_URI", "/webapi/entry.cgi"},
				{"CONTENT_TYPE", "application/octet-stream"}}, upload)
			want := fmt.Sprintf(`{"data":{"len":%d,"sha256":"%s"},"success":true}`, len(upload), hex.EncodeToString(sum[:]))
			if err == nil && b != want {
				err = fmt.Errorf("上傳內容不對：%s", b)
			}
			errs <- err
		}()
	}
	go func() {
		for range 20 {
			f.writeStatus()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	// 連線關掉之後才把 active 減一，等它歸零
	for end := time.Now().Add(2 * time.Second); f.stats[stActive].Load() != 0 && time.Now().Before(end); {
		time.Sleep(10 * time.Millisecond)
	}
	f.writeStatus()
	var st map[string]any
	data, _ := os.ReadFile(cfg.StatusFile)
	if err := json.Unmarshal(data, &st); err != nil {
		t.Fatal(err)
	}
	stats := st["stats"].(map[string]any)
	if stats["connections"] != 40.0 || stats["injected"] != 20.0 || stats["active"] != 0.0 {
		t.Errorf("%v", stats)
	}
}
