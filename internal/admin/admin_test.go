package admin

import (
	"archive/tar"
	"bufio"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/admin/wizard"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/assets"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/config"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
)

// ── 解除安裝時詢問要不要刪資料目錄，預設保留 ─────────────────────────────

func askPurge(t *testing.T, answer string, purge, yes, tty bool, dataDir string) (bool, int) {
	t.Helper()
	var out bytes.Buffer
	oldIn, oldOut, oldTTY := wizard.In, wizard.Out, stdinIsTerminal
	wizard.In, wizard.Out = bufio.NewReader(strings.NewReader(answer+"\n")), &out
	stdinIsTerminal = func() bool { return tty }
	defer func() { wizard.In, wizard.Out, stdinIsTerminal = oldIn, oldOut, oldTTY }()
	got := wantPurge(purge, yes, dataDir)
	return got, strings.Count(out.String(), "要連資料目錄")
}

func TestUninstallDefaultKeeps(t *testing.T) {
	d := t.TempDir()
	for answer, want := range map[string]bool{"": false, "n": false, "y": true, "YES": true} {
		got, asked := askPurge(t, answer, false, false, true, d)
		if got != want || asked != 1 {
			t.Errorf("%q：%v（問了 %d 次）", answer, got, asked)
		}
	}
}

func TestUninstallNoQuestionWhenNotInteractive(t *testing.T) {
	d := t.TempDir()
	cases := []struct {
		purge, yes, tty bool
		dir             string
		want            bool
	}{
		{false, true, true, d, false},                         // --yes：保留
		{false, false, false, d, false},                       // 不是終端機：保留
		{true, false, true, d, true},                          // --purge：直接刪
		{false, false, true, filepath.Join(d, "nope"), false}, // 資料目錄不存在
		{false, false, true, "", false},
	}
	for _, c := range cases {
		if got, asked := askPurge(t, "y", c.purge, c.yes, c.tty, c.dir); got != c.want || asked != 0 {
			t.Errorf("%+v：%v（問了 %d 次）", c, got, asked)
		}
	}
}

// ── systemd 服務 ─────────────────────────────────────────────────────────

func TestRenderUnits(t *testing.T) {
	want := map[string]string{
		"sse-filter.service":      "ExecStart=/usr/local/sse/sse filter --config /usr/local/etc/sse/filter.json\n",
		"sse-engine.service":      "ExecStart=/usr/local/sse/sse --config /volume1/homes/alice/sse/config.yml serve\n",
		"sse-indexer.service":     "ExecStart=/usr/local/sse/sse --config /volume1/homes/alice/sse/config.yml index\n",
		"sse-nginx-guard.service": "ExecStart=/usr/local/sse/sse guard\n",
		"sse-nginx-guard.path":    "PathChanged=/etc/nginx/nginx.conf\n",
		"sse-nginx-guard.timer":   "OnUnitActiveSec=10min\n",
	}
	for _, u := range allUnits {
		data, err := renderUnit(u, "http", "http", "alice", "/volume1/homes/alice/sse/config.yml")
		if err != nil {
			t.Fatal(u, err)
		}
		text := string(data)
		if !strings.Contains(text, want[u]) || strings.Contains(text, "$") || strings.Contains(strings.ToLower(text), "python") {
			t.Errorf("%s：\n%s", u, text)
		}
	}
	data, _ := renderUnit("sse-filter.service", "http", "http", "alice", "")
	if !strings.Contains(string(data), "User=http\nGroup=http\n") {
		t.Error(string(data))
	}
	data, _ = renderUnit("sse-engine.service", "http", "web", "alice", "/volume1/My Photos/sse/config.yml")
	if !strings.Contains(string(data), "User=alice\nGroup=web\n") ||
		!strings.Contains(string(data), `ExecStart=/usr/local/sse/sse --config "/volume1/My Photos/sse/config.yml" serve`) {
		t.Error(string(data))
	}
	entries, _ := assets.Systemd.ReadDir("systemd")
	if len(entries) != len(allUnits) {
		t.Errorf("範本 %d 個，服務 %d 個", len(entries), len(allUnits))
	}
}

func TestSubstitute(t *testing.T) {
	out, err := substitute("a=$x ${y}z $$x", map[string]string{"x": "1", "y": "2"})
	if err != nil || out != "a=1 2z $x" {
		t.Errorf("%q %v", out, err)
	}
	if _, err := substitute("$nope", nil); err == nil {
		t.Error("沒有對應的值要回報")
	}
}

// ── filter.json ─────────────────────────────────────────────────────────

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	v := func(s, k, val string) wizard.Value { return wizard.Value{Section: s, Key: k, Val: val} }
	text, err := wizard.Render([]wizard.Value{v("synology", "account", "alice"), v("synology", "password", "pw"),
		v("embedding", "base_url", "http://gpu/v1"), v("ocr", "model", "m")})
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.yml")
	wizard.WriteConfig(p, text, -1, -1)
	cfg, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func TestRenderFilterConfig(t *testing.T) {
	if paths.Sandbox || exists(paths.FilterConfig) || exists(paths.PhotosInfo) {
		t.Skip("要在沒有安裝的環境跑")
	}
	// 欄位順序與格式跟 Python 版的 json.dumps(indent=2) 相同
	want := `{
  "listen": "/run/sse/filter.sock",
  "socket_mode": "0660",
  "upstream": "/run/synoscgi.sock",
  "status_file": "/run/sse/status.json",
  "kill_switch": "/usr/local/etc/sse/DISABLE",
  "photos_info": "/var/packages/SynologyPhotos/INFO",
  "mode": "passthrough",
  "inject_photos_version": null,
  "engine_socket": "/run/sse-engine/engine.sock",
  "max_inject": 500,
  "log_file": "/var/log/sse/filter.log",
  "log_keep_days": 3
}
`
	if got := string(renderFilterConfig(testConfig(t), "passthrough")); got != want {
		t.Errorf("%s", got)
	}
}

func TestFilterConfigEditKeepsOrder(t *testing.T) {
	// Python 版寫的 filter.json：改診斷錄製的設定時，其他欄位的順序與格式不變
	py := "{\n  \"listen\": \"/run/sse/filter.sock\",\n  \"mode\": \"engine\",\n  \"inject_photos_version\": \"1.9.0-10120\",\n" +
		"  \"demo\": {\n    \"keyword\": \"貓\",\n    \"items\": [\n      1,\n      2\n    ]\n  },\n  \"log_keep_days\": 3\n}\n"
	o, err := parseObject([]byte(py))
	if err != nil {
		t.Fatal(err)
	}
	if string(o.marshal()) != py {
		t.Errorf("%s", o.marshal())
	}
	o.set("capture_file", "/run/sse/capture.jsonl")
	o.set("capture_max", 200)
	o.set("mode", "passthrough")
	got := string(o.marshal())
	if !strings.HasSuffix(got, "  \"log_keep_days\": 3,\n  \"capture_file\": \"/run/sse/capture.jsonl\",\n  \"capture_max\": 200\n}\n") ||
		!strings.Contains(got, "\"mode\": \"passthrough\",\n  \"inject_photos_version\"") {
		t.Errorf("%s", got)
	}
	o.del("capture_file")
	o.del("capture_max")
	o.set("mode", "engine")
	if string(o.marshal()) != py {
		t.Errorf("%s", o.marshal())
	}
	if _, err := parseObject([]byte("[1]")); err == nil {
		t.Error("最外層不是物件要回報")
	}
}

// ── 更新 ────────────────────────────────────────────────────────────────

func rel(tag string, pre bool, assets ...string) release {
	r := release{Tag: tag, Prerelease: pre}
	for _, a := range assets {
		r.Assets = append(r.Assets, struct {
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		}{a, "https://example/" + tag + "/" + a})
	}
	return r
}

func TestPickRelease(t *testing.T) {
	a := assetName()
	rels := []release{
		rel("v2026.10.26+build.2", true, a, a+".sha256"),
		rel("v2026.10.26+build.10", true, a, a+".sha256"), // build 是數字：10 比 2 新
		rel("v2026.10.25+build.1", false, a, a+".sha256"),
		rel("v2026.10.24+build.1", false, "synology-photos-search-enhancement.tar.gz"),
		{Tag: "v2026.10.27+build.1", Draft: true},
	}
	if tag, url, sha, err := pickRelease(rels, "release", ""); err != nil || tag != "v2026.10.25+build.1" ||
		url != "https://example/v2026.10.25+build.1/"+a || sha != url+".sha256" {
		t.Errorf("%s %s %s %v", tag, url, sha, err)
	}
	if tag, _, _, _ := pickRelease(rels, "pre-release", ""); tag != "v2026.10.26+build.10" {
		t.Error(tag)
	}
	if tag, _, _, _ := pickRelease(rels, "release", "2026.10.26+build.2"); tag != "v2026.10.26+build.2" { // 指定版本：不分管道
		t.Error(tag)
	}
	if _, _, _, err := pickRelease(rels, "release", "v2026.10.24+build.1"); err == nil || !strings.Contains(err.Error(), "沒有 "+a) {
		t.Errorf("%v", err)
	}
	if _, _, _, err := pickRelease(rels, "release", "v1"); err == nil || !strings.Contains(err.Error(), "找不到版本 v1") {
		t.Errorf("%v", err)
	}
	if _, _, _, err := pickRelease(rels[:2], "release", ""); err == nil || !strings.Contains(err.Error(), "找不到 release 的版本") {
		t.Errorf("%v", err)
	}
}

func TestCompareVersion(t *testing.T) {
	cases := [][3]string{
		{"2026.10.25+build.1", "v2026.10.26+build.1", "newer"},
		{"2026.10.26+build.1", "v2026.10.26+build.1", "same"},
		{"2026.10.26+build.10", "v2026.10.26+build.9", "older"},
		{"0.0.0+dev", "v2026.01.01+build.1", "newer"}, // 開發版比任何 release 舊
		{"", "v2026.01.01+build.1", "newer"},
	}
	for _, c := range cases {
		if got := compareVersion(c[0], c[1]); got != c[2] {
			t.Errorf("%v：%s", c, got)
		}
	}
	if want := "sse-" + runtime.GOOS + "-"; !strings.HasPrefix(assetName(), want) || !strings.HasSuffix(assetName(), ".tar.gz") {
		t.Error(assetName())
	}
}

func TestVerifyAndExtract(t *testing.T) {
	d := t.TempDir()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for name, body := range map[string]string{"LICENSE": "GPL", "sse": "#!/bin/sh\necho 1\n"} {
		tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg})
		tw.Write([]byte(body))
	}
	tw.Close()
	gz.Close()
	tarball := filepath.Join(d, "x.tar.gz")
	os.WriteFile(tarball, buf.Bytes(), 0o644)
	sum := sha256.Sum256(buf.Bytes())
	os.WriteFile(tarball+".sha256", []byte(hex.EncodeToString(sum[:])+"  x.tar.gz\n"), 0o644)
	if !verifySHA256(tarball, tarball+".sha256") {
		t.Error("sha256 應該相符")
	}
	os.WriteFile(tarball+".bad", []byte(strings.Repeat("0", 64)+"  x.tar.gz\n"), 0o644)
	if verifySHA256(tarball, tarball+".bad") || verifySHA256(tarball, filepath.Join(d, "nope")) {
		t.Error("sha256 不符要拒絕")
	}
	bin, err := extractBinary(tarball, d)
	if err != nil || bin != filepath.Join(d, "sse") {
		t.Fatal(bin, err)
	}
	if st, _ := os.Stat(bin); st.Mode().Perm() != 0o755 {
		t.Errorf("%o", st.Mode().Perm())
	}
	if _, err := extractBinary(tarball+".sha256", d); err == nil {
		t.Error("不是 tar.gz 要回報")
	}
}

// ── 其他 ────────────────────────────────────────────────────────────────

func TestSnapshotRestore(t *testing.T) {
	d := t.TempDir()
	file, link, none := filepath.Join(d, "f"), filepath.Join(d, "l"), filepath.Join(d, "n")
	os.WriteFile(file, []byte("#!/bin/sh\n"), 0o755)
	os.Symlink("/usr/local/sse/sse", link)
	saved := []saved{snapshot(file), snapshot(link), snapshot(none)}
	for _, p := range []string{file, link, none} {
		if err := symlinkAtomic("/elsewhere", p); err != nil {
			t.Fatal(err)
		}
	}
	for _, s := range saved {
		if err := s.restore(); err != nil {
			t.Fatal(err)
		}
	}
	if data, _ := os.ReadFile(file); string(data) != "#!/bin/sh\n" || isLink(file) {
		t.Error("一般檔案沒還原")
	}
	if st, _ := os.Stat(file); st.Mode().Perm() != 0o755 {
		t.Errorf("%o", st.Mode().Perm())
	}
	if target, _ := os.Readlink(link); target != "/usr/local/sse/sse" {
		t.Error(target)
	}
	if exists(none) {
		t.Error("原本不存在的要移除")
	}
}

func TestRealpath(t *testing.T) {
	d, _ := filepath.EvalSymlinks(t.TempDir())
	os.Symlink(d, filepath.Join(d, "link"))
	if got := realpath(filepath.Join(d, "link", "a", "b")); got != filepath.Join(d, "a", "b") {
		t.Error(got)
	}
}

func TestFlags(t *testing.T) {
	var s string
	var b bool
	var n int
	f := newFlags()
	f.String(&s, "--user")
	f.Bool(&b, "-f", "--follow")
	f.Int(&n, "-n")
	pos := f.parse([]string{"filter", "--user=alice", "-f", "-n", "5"}, "")
	if s != "alice" || !b || n != 5 || len(pos) != 1 || pos[0] != "filter" {
		t.Errorf("%q %v %d %v", s, b, n, pos)
	}
	for _, bad := range [][]string{{"--nope"}, {"--user"}, {"-n", "x"}} {
		func() {
			defer func() {
				if _, ok := recover().(usageError); !ok {
					t.Errorf("%v 要回報用法錯誤", bad)
				}
			}()
			f.parse(bad, "")
		}()
	}
}
