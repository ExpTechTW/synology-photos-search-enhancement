package logs

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func names(t *testing.T, dir string) []string {
	t.Helper()
	entries, _ := os.ReadDir(dir)
	var out []string
	for _, e := range entries {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func TestPrune(t *testing.T) {
	d := t.TempDir()
	for _, day := range []string{"2026-10-20", "2026-10-23", "2026-10-24", "2026-10-25"} {
		os.WriteFile(filepath.Join(d, "indexer.log."+day), nil, 0o644)
	}
	os.WriteFile(filepath.Join(d, "other.log.2020-01-01"), nil, 0o644)
	Prune(filepath.Join(d, "indexer.log"), 3, time.Date(2026, 10, 26, 12, 0, 0, 0, time.Local))
	// 只留今天與前 2 天：最舊的紀錄不超過 3 天
	if got := strings.Join(names(t, d), " "); got != "indexer.log.2026-10-24 indexer.log.2026-10-25 other.log.2020-01-01" {
		t.Errorf("剩下 %s", got)
	}
}

func TestDailyRotation(t *testing.T) {
	d := t.TempDir()
	path := filepath.Join(d, "engine.log")
	now := time.Date(2026, 10, 24, 23, 59, 0, 0, time.Local)
	w, err := OpenDaily(path, 2)
	if err != nil {
		t.Fatal(err)
	}
	w.now = func() time.Time { return now }
	w.day = day(now)
	w.Write([]byte("第一天\n"))
	now = now.Add(2 * time.Minute) // 過了午夜
	w.Write([]byte("第二天\n"))
	now = now.Add(24 * time.Hour)
	w.Write([]byte("第三天\n"))
	w.Close()
	if got := strings.Join(names(t, d), " "); got != "engine.log engine.log.2026-10-25" {
		t.Errorf("檔案：%s", got) // 2026-10-24 超過 2 天已刪除
	}
	if b, _ := os.ReadFile(path); string(b) != "第三天\n" {
		t.Errorf("今天的檔案：%q", b)
	}
	if b, _ := os.ReadFile(path + ".2026-10-25"); string(b) != "第二天\n" {
		t.Errorf("昨天的檔案：%q", b)
	}
}

func TestSetup(t *testing.T) {
	d := t.TempDir()
	defer func() { out, stamped = os.Stderr, false }()
	if err := Setup(filepath.Join(d, "logs", "indexer.log"), 3, Info); err != nil {
		t.Fatal(err)
	}
	Get("sse.test").Debugf("看不到")
	Get("sse.test").Infof("寫進 log 檔 %d", 1)
	b, _ := os.ReadFile(filepath.Join(d, "logs", "indexer.log"))
	line := string(b)
	if !strings.Contains(line, " INFO sse.test 寫進 log 檔 1\n") || strings.Contains(line, "看不到") || len(line) < 24 || line[4] != '-' {
		t.Errorf("log：%q", line)
	}
	if err := Setup("/proc/nope/x.log", 3, Info); err == nil {
		t.Error("寫不了要回報")
	}
}
