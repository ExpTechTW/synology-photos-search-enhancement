package db

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func pragma(t *testing.T, q Q, name string) any {
	t.Helper()
	var v any
	if err := q.QueryRow("PRAGMA " + name).Scan(&v); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	if b, ok := v.([]byte); ok {
		return string(b)
	}
	return v
}

func TestSettings(t *testing.T) {
	p := filepath.Join(t.TempDir(), "data", "index.db")
	w, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	for name, want := range map[string]any{"journal_mode": "wal", "cache_size": int64(-16384),
		"wal_autocheckpoint": int64(128), "journal_size_limit": int64(8388608), "page_size": int64(65536),
		"busy_timeout": int64(30000)} {
		if got := pragma(t, w, name); got != want {
			t.Errorf("writer %s = %v (%T), want %v", name, got, got, want)
		}
	}
	r, err := OpenReadOnly(p)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	for name, want := range map[string]any{"cache_size": int64(-8192), "mmap_size": int64(268435456)} {
		if got := pragma(t, r, name); got != want {
			t.Errorf("reader %s = %v, want %v", name, got, want)
		}
	}
	if _, err := r.Exec("INSERT INTO meta VALUES ('x', 'y')"); err == nil {
		t.Error("唯讀連線不能寫入")
	}
	if v, ok, _ := GetMeta(w, "schema_version"); !ok || v != "1" {
		t.Errorf("schema_version = %q", v)
	}
}

func TestMetaRevStats(t *testing.T) {
	d, err := Open(filepath.Join(t.TempDir(), "i.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	for i := 1; i <= 2; i++ {
		rev, err := NextRev(d)
		if err != nil || rev != int64(i) {
			t.Fatalf("NextRev = %d, %v", rev, err)
		}
	}
	d.Exec("INSERT INTO item(unit_id, owner_user_id, time, type, cache_key, seen_scan, emb_state, rev) VALUES (1,1,0,'photo','k',1,2,1)")
	d.Exec("INSERT INTO ocr VALUES (1, 'm', '電子發票 38475629')")
	var n int
	d.QueryRow("SELECT count(*) FROM ocr_fts WHERE ocr_fts MATCH '\"發票 3\"'").Scan(&n)
	if n != 1 {
		t.Error("FTS5 trigram 沒有作用")
	}
	s, err := GetStats(d)
	if err != nil || s.Items != 1 || s.EmbedFailed != 1 || s.WithText != 1 || s.Rev != 2 {
		t.Errorf("stats = %+v, %v", s, err)
	}
	if a, b, err := RequeueFailed(d); err != nil || a != 1 || b != 0 {
		t.Errorf("RequeueFailed = %d %d %v", a, b, err)
	}
	if _, wasted, err := SpaceStats(d); err != nil || wasted < 0 {
		t.Errorf("SpaceStats: %v", err)
	}
	if err := Compact(d); err != nil {
		t.Error(err)
	}
}

// Python 版建立的資料庫要能直接沿用
func TestOpensPythonDatabase(t *testing.T) {
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Skip("沒有 python3")
	}
	p := filepath.Join(t.TempDir(), "py.db")
	script := `
import sqlite3, sys
c = sqlite3.connect(sys.argv[1])
c.execute("PRAGMA page_size=65536"); c.execute("PRAGMA journal_mode=WAL")
c.executescript(sys.stdin.read())
c.execute("INSERT INTO meta VALUES ('schema_version', '1')"); c.execute("INSERT INTO meta VALUES ('rev', '7')")
c.execute("INSERT INTO ocr VALUES (5, 'g@xl', 'parking 停車')")
c.commit()`
	cmd := exec.Command(py, "-c", script, p)
	cmd.Stdin = stringsReader(schema)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	d, err := Open(p)
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	if rev := MetaInt(d, "rev", 0); rev != 7 {
		t.Errorf("rev = %d", rev)
	}
	var id int
	if err := d.QueryRow("SELECT rowid FROM ocr_fts WHERE ocr_fts MATCH '\"parking\"'").Scan(&id); err != nil || id != 5 {
		t.Errorf("FTS: %d %v", id, err)
	}
	os.Remove(p)
}

func stringsReader(s string) *os.File {
	r, w, _ := os.Pipe()
	go func() { w.WriteString(s); w.Close() }()
	return r
}
