package engine

import (
	"encoding/json"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/app"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/config"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/db"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/indexer"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/llm"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/synology"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/testfake"
)

// 給 TestIndexerAlive 用：以 "sse index" 的名字執行、什麼都不做的子程序
func TestMain(m *testing.M) {
	if os.Getenv("SSE_TEST_SLEEP") == "1" {
		time.Sleep(time.Minute)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

type env struct {
	t    *testing.T
	dir  string
	path string
	dsm  *testfake.DSM
	llm  *testfake.LLM
}

func newEnv(t *testing.T) *env {
	dir, err := os.MkdirTemp(os.Getenv("SSE_TEST_TMP"), "sse-cli.")
	if err != nil {
		t.Fatal(err)
	}
	e := &env{t: t, dir: dir, path: filepath.Join(dir, "config.yml"), dsm: testfake.NewDSM(testfake.EngineLibrary()),
		llm: testfake.NewLLM()}
	t.Cleanup(func() { e.dsm.Close(); e.llm.Close(); os.RemoveAll(dir) })
	doc := map[string]map[string]any{
		"synology":  {"url": e.dsm.URL(), "account": "alice", "password": "pw-alice"},
		"embedding": {"base_url": e.llm.URL()},
		"ocr":       {"model": "fake-vision"},
		"index":     {"page_size": 7},
		"search":    {"socket": filepath.Join(dir, "engine.sock")},
	}
	data, _ := json.Marshal(doc)
	os.WriteFile(e.path, data, 0o600)
	return e
}

func (e *env) cfg() *config.Config {
	cfg, err := config.Load(e.path)
	if err != nil {
		e.t.Fatal(err)
	}
	return cfg
}

func (e *env) indexer() *indexer.Indexer {
	cfg := e.cfg()
	d, err := db.Open(cfg.Index.DB)
	if err != nil {
		e.t.Fatal(err)
	}
	e.t.Cleanup(func() { d.Close() })
	c := cfg.Synology
	return indexer.New(cfg, synology.New(c.URL, c.Account, c.Password, false, 10*time.Second), llm.FromConfig(cfg), d)
}

// run 執行 sse --config <path> <args>，回傳結束碼與輸出（stdout + stderr）。
func (e *env) run(args ...string) (int, string) {
	e.t.Helper()
	r, w, _ := os.Pipe()
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = w, w
	done := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		done <- string(b)
	}()
	code := Main(app.Global{Config: e.path}, args[0], args[1:])
	w.Close()
	os.Stdout, os.Stderr = stdout, stderr
	return code, <-done
}

func TestCLISetsSQLiteTmpdir(t *testing.T) {
	e := newEnv(t)
	saved, had := os.LookupEnv("SQLITE_TMPDIR")
	os.Unsetenv("SQLITE_TMPDIR")
	defer func() {
		if had {
			os.Setenv("SQLITE_TMPDIR", saved)
		} else {
			os.Unsetenv("SQLITE_TMPDIR")
		}
	}()
	code, out := e.run("progress")
	if code != 0 || !strings.Contains(out, "索引器還沒跑過") {
		t.Errorf("%d %s", code, out)
	}
	if got := os.Getenv("SQLITE_TMPDIR"); got != filepath.Join(e.dir, "data") {
		t.Errorf("SQLITE_TMPDIR = %s", got)
	}
}

func TestProgressReport(t *testing.T) {
	e := newEnv(t)
	ix := e.indexer()
	if err := ix.RunOnce(true); err != nil {
		t.Fatal(err)
	}
	code, out := e.run("progress", "--json")
	if code != 0 {
		t.Fatal(out)
	}
	var st map[string]any
	if err := json.Unmarshal([]byte(out), &st); err != nil {
		t.Fatalf("%v：%s", err, out)
	}
	emb, ocr := st["embedding"].(map[string]any), st["ocr"].(map[string]any)
	if emb["percent"] != 100.0 || ocr["percent"] != 100.0 {
		t.Errorf("percent: %v %v", emb, ocr)
	}
	if st["last_full_scan_result"].(map[string]any)["seen"] != 20.0 {
		t.Errorf("%v", st["last_full_scan_result"])
	}
	// 有待處理的照片：百分比與「OCR 等 embedding」
	for uid := int64(900); uid < 905; uid++ {
		e.dsm.Set(testfake.MakeItem(uid, 1, testfake.T0, "dog", ""))
	}
	if _, _, _, err := ix.FullScan(); err != nil {
		t.Fatal(err)
	}
	_, out = e.run("progress", "--brief")
	for _, want := range []string{"embedding 20 / 25（80.0%", "embedding 做完後開始"} {
		if !strings.Contains(out, want) {
			t.Errorf("--brief 少了 %q：\n%s", want, out)
		}
	}
	_, out = e.run("progress")
	for _, want := range []string{"████████████████░░░░", "完整掃描  上次", "索引進度：25 張照片", "20 / 25（ 80.0%）"} {
		if !strings.Contains(out, want) {
			t.Errorf("progress 少了 %q：\n%s", want, out)
		}
	}
}

func TestIndexerAlive(t *testing.T) {
	sleeper := func(name string, args ...string) *exec.Cmd {
		cmd := exec.Command(os.Args[0])
		cmd.Args = append([]string{name}, args...)
		cmd.Env = append(os.Environ(), "SSE_TEST_SLEEP=1")
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })
		return cmd
	}
	idx := sleeper("/usr/local/sse/sse", "--config", "x.yml", "index")
	if !indexerAlive(map[string]any{"pid": float64(idx.Process.Pid)}) {
		t.Error("在跑的索引器")
	}
	other := sleeper("/usr/bin/other")
	_, err := os.Stat("/proc")
	if got := indexerAlive(map[string]any{"pid": float64(other.Process.Pid)}); got != os.IsNotExist(err) {
		t.Errorf("pid 被別的程式用了：%v（沒有 /proc 時無法判斷，當成還在跑）", got)
	}
	idx.Process.Kill()
	idx.Wait()
	if indexerAlive(map[string]any{"pid": float64(idx.Process.Pid)}) {
		t.Error("已經結束的索引器")
	}
	if indexerAlive(map[string]any{}) {
		t.Error("沒有 pid")
	}
}

func TestTag(t *testing.T) {
	e := newEnv(t)
	if err := e.indexer().RunOnce(true); err != nil {
		t.Fatal(err)
	}
	if code, out := e.run("tag", "add", "TEST", "203", "300"); code != 1 || !strings.Contains(out, "203, 300") {
		t.Errorf("不在索引裡的照片要拒絕：%d %s", code, out) // 300 是 bob 的
	}
	if code, out := e.run("tag", "add", "TEST", "103"); code != 0 || !strings.Contains(out, "已標上「test」") {
		t.Errorf("%d %s", code, out)
	}
	if _, out := e.run("tag", "list"); !strings.Contains(out, "test\t103") {
		t.Errorf("list：%s", out)
	}
	if code, out := e.run("tag", "rm", "test"); code != 0 || !strings.Contains(out, "已移除 1 筆") {
		t.Errorf("%d %s", code, out)
	}
	if _, out := e.run("tag", "list"); !strings.Contains(out, "沒有標籤") {
		t.Errorf("list：%s", out)
	}
	if code, _ := e.run("tag", "add"); code != 1 {
		t.Error("沒有標籤要拒絕")
	}
}

func TestCheckAndSearch(t *testing.T) {
	e := newEnv(t)
	code, out := e.run("check")
	if code != 0 {
		t.Fatalf("check：%s", out)
	}
	for _, want := range []string{"✓ DSM 登入", "alice（id 1）", "照片 20 張，縮圖 m", "✓ 文字 embedding", "768 維", "✓ OCR", "✓ 資料庫"} {
		if !strings.Contains(out, want) {
			t.Errorf("check 少了 %q：\n%s", want, out)
		}
	}
	if err := e.indexer().RunOnce(true); err != nil {
		t.Fatal(err)
	}
	code, out = e.run("search", "cat", "--explain")
	if code != 0 || !strings.Contains(out, "「cat」：5 張") || !strings.Contains(out, "圖片：分數最高的") {
		t.Errorf("search：%d %s", code, out)
	}
	if code, out := e.run("search"); code != 2 || !strings.Contains(out, "用法") {
		t.Errorf("%d %s", code, out)
	}
	if code, out := e.run("retry-failed"); code != 0 || !strings.Contains(out, "重新排入：embedding 0、OCR 0") {
		t.Errorf("%d %s", code, out)
	}
	if code, out := e.run("compact"); code != 0 || !strings.Contains(out, "硬碟用量") {
		t.Errorf("%d %s", code, out)
	}
}

func TestCheckReportsFailures(t *testing.T) {
	e := newEnv(t)
	e.llm.Close()
	code, out := e.run("check")
	if code != 1 || !strings.Contains(out, "✗ 文字 embedding") || !strings.Contains(out, "✓ DSM 登入") {
		t.Errorf("%d %s", code, out)
	}
}

func TestFormat(t *testing.T) {
	for n, want := range map[int64]string{0: "0", 999: "999", 1000: "1,000", 19754: "19,754", 1234567: "1,234,567"} {
		if got := fmtN(n); got != want {
			t.Errorf("fmtN(%d) = %s", n, got)
		}
	}
	for s, want := range map[float64]string{30: "不到 1 分鐘", 90: "2 分鐘", 29160: "8 小時 6 分", 200000: "2.3 天"} {
		if got := fmtDur(s); got != want {
			t.Errorf("fmtDur(%v) = %s", s, got)
		}
	}
	if pyFloat(80) != "80.0" || pyFloat(31.1) != "31.1" {
		t.Error("pyFloat")
	}
	if bar(0.8) != "████████████████░░░░" {
		t.Error("bar")
	}
}
