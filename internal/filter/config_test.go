package filter

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/logs"
)

func loadCfg(t *testing.T, data string) (*Config, string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "filter.json")
	if err := os.WriteFile(p, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, msg, _ := loadConfig(p)
	// 安裝程式用的 ValidateConfig 必須跟 filter 載入時的判斷一致
	if err := ValidateConfig([]byte(data)); (err != nil) != (msg != "") || err != nil && err.Error() != msg {
		t.Errorf("ValidateConfig 跟 loadConfig 不一致：%v / %q", err, msg)
	}
	return cfg, msg
}

const demoItemJSON = `{"id": 90001, "owner_user_id": 1, "time": 1750000000, "type": "photo"}`

func TestBadConfigsFallBackToPassthrough(t *testing.T) {
	for _, data := range []string{
		"{not json",
		`{"mode": "inject"}`,
		`{"mode": "demo"}`,
		`{"typo": 1}`,
		`{"mode": "demo", "demo": {"keyword": "", "items": [` + demoItemJSON + `]}}`,
		`{"mode": "demo", "demo": {"keyword": "x", "items": [{"id": "1"}]}}`,
	} {
		cfg, msg := loadCfg(t, data)
		if cfg.Mode != modePassthrough || msg == "" {
			t.Errorf("%s：mode %s，錯誤 %q", data, cfg.Mode, msg)
		}
	}
	cfg, msg := loadCfg(t, `{"mode": "demo", "demo": {"keyword": "test", "items": [`+demoItemJSON+`]}}`)
	if cfg.Mode != modeDemo || msg != "" {
		t.Errorf("mode %s，錯誤 %q", cfg.Mode, msg)
	}
}

func TestPruneKeepsOnlyRecentDays(t *testing.T) {
	d := t.TempDir()
	for _, name := range []string{"filter.log", "filter.log.2026-10-25", "filter.log.2026-10-24", "filter.log.2026-10-23",
		"filter.log.2026-10-01", "other.log.2026-10-01", "filter.log.junk"} {
		os.WriteFile(filepath.Join(d, name), nil, 0o644)
	}
	os.Chtimes(filepath.Join(d, "filter.log.junk"), time.Unix(0, 0), time.Unix(0, 0))
	logs.Prune(filepath.Join(d, "filter.log"), 3, time.Date(2026, 10, 26, 12, 0, 0, 0, time.Local))
	// 只留今天與前 2 天：最舊的紀錄不超過 3 天
	entries, _ := os.ReadDir(d)
	var got []string
	for _, e := range entries {
		got = append(got, e.Name())
	}
	sort.Strings(got)
	want := []string{"filter.log", "filter.log.2026-10-24", "filter.log.2026-10-25", "other.log.2026-10-01"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("%v", got)
	}
}

func TestFileLoggingAndBadConfig(t *testing.T) {
	d := t.TempDir()
	l := newLogger()
	path := filepath.Join(d, "x", "f.log")
	if err := l.setupFile(path, 3); err != nil {
		t.Fatal(err)
	}
	l.info("hello")
	// 寫不了：回傳錯誤、保留原本的輸出
	os.WriteFile(filepath.Join(d, "blocker"), nil, 0o644)
	if err := l.setupFile(filepath.Join(d, "blocker", "nope", "f.log"), 3); err == nil {
		t.Error("寫不了的路徑應該回傳錯誤")
	}
	l.warning("still here")
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 2 || !strings.HasSuffix(lines[0], " INFO hello") || !strings.HasSuffix(lines[1], " WARNING still here") {
		t.Errorf("%q", data)
	}
	if _, err := time.Parse("2006-01-02 15:04:05", lines[0][:19]); err != nil {
		t.Errorf("每行開頭要有時間：%q", lines[0])
	}
	for _, bad := range []string{`{"log_keep_days": 0}`, `{"log_keep_days": "3"}`, `{"log_file": 5}`} {
		if _, msg := loadCfg(t, bad); msg == "" {
			t.Errorf("%s：應該是錯的", bad)
		}
	}
	if _, msg := loadCfg(t, `{"log_file": "/var/log/sse/filter.log", "log_keep_days": 3}`); msg != "" {
		t.Error(msg)
	}
}

func TestLargeCaptureStaysValidJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "capture.jsonl")
	cfg := defaultConfig()
	cfg.CaptureFile = path
	f := newFilter(cfg, "")
	var big strings.Builder
	big.WriteString(`{"success":true,"data":{"list":[`)
	for i := range 2000 {
		if i > 0 {
			big.WriteByte(',')
		}
		big.WriteString(`{"id":` + itoa(i) + `,"filename":"` + strings.Repeat("x", 300) + `"}`)
	}
	big.WriteString(`]}}`)
	f.capture("native", qp("api", searchAPI, "_sid", "secret"), json.RawMessage(big.String()))
	f.capture("native", qp("api", searchAPI), json.RawMessage(`{"success":true,"data":{"count":3}}`))
	fh, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer fh.Close()
	var recs []map[string]any
	sc := bufio.NewScanner(fh)
	sc.Buffer(nil, 4<<20)
	for sc.Scan() {
		var r map[string]any
		if err := json.Unmarshal(sc.Bytes(), &r); err != nil { // 每一行都是完整的 JSON
			t.Fatal(err)
		}
		recs = append(recs, r)
	}
	summary := recs[0]["response"].(map[string]any)["summary"].(map[string]any)
	if summary["count"] != 2000.0 || !reflect.DeepEqual(summary["ids"].([]any)[:3], []any{0.0, 1.0, 2.0}) {
		t.Errorf("%v", summary)
	}
	if _, ok := recs[0]["params"].(map[string]any)["_sid"]; ok {
		t.Error("不能記 _sid")
	}
	if recs[1]["response"].(map[string]any)["data"].(map[string]any)["count"] != 3.0 {
		t.Errorf("%v", recs[1])
	}
	if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
		t.Errorf("權限 %v", st.Mode().Perm())
	}
}

func itoa(i int) string { return strings.TrimSpace(string(mustMarshal(i))) }
