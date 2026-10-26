package admin

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/config"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/state"
)

const (
	filterUnit   = "sse-filter.service"
	guardService = "sse-nginx-guard.service"
)

var (
	guardTriggers = []string{"sse-nginx-guard.path", "sse-nginx-guard.timer"}
	engineUnits   = []string{"sse-engine.service", "sse-indexer.service"}
	allUnits      = append(append([]string{filterUnit, guardService}, guardTriggers...), engineUnits...)
	// 用到 sse 執行檔的常駐服務：換回舊版的程式後要重新啟動
	binaryUnits = append([]string{filterUnit}, engineUnits...)
)

const probePath = "/webapi/query.cgi?api=SYNO.API.Info&version=1&method=query&query=SYNO.API.Info"

var probeCandidates = []string{"https://127.0.0.1:5001" + probePath, "http://127.0.0.1:5000" + probePath}

// ── 執行外部指令 ─────────────────────────────────────────────────────────

// run 執行指令，回傳結束碼與輸出（stdout + stderr，去掉前後空白）；執行不了是 127。
func run(argv []string, timeout time.Duration) (int, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	text := strings.TrimSpace(out.String())
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, text
	case ctx.Err() != nil:
		return 127, fmt.Sprintf("執行超過 %s：%s", timeout, strings.Join(argv, " "))
	case errors.As(err, &ee):
		return ee.ExitCode(), text
	}
	return 127, err.Error()
}

func systemctl(args ...string) (int, string) {
	return run(append([]string{paths.Systemctl}, args...), 60*time.Second)
}

// DSM 7.3 是 systemd 219，還沒有 `enable --now` / `disable --now`（220 才有），所以分成兩步
func enableNow(units ...string) (int, string) {
	if code, out := systemctl(append([]string{"enable"}, units...)...); code != 0 {
		return code, out
	}
	return systemctl(append([]string{"start"}, units...)...)
}

func disableNow(units ...string) (int, string) {
	systemctl(append([]string{"stop"}, units...)...)
	return systemctl(append([]string{"disable"}, units...)...)
}

// runChild 執行另一個 sse（新版），沿用目前的終端機；Ctrl-C 交給它處理。
func runChild(bin string, args ...string) int {
	cmd := exec.Command(bin, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	childRunning.Store(true)
	defer childRunning.Store(false)
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &ee):
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ee.ExitCode()
	}
	bad(fmt.Sprintf("執行不了 %s：%v", bin, err))
	return 127
}

// ── 檔案 ────────────────────────────────────────────────────────────────

func exists(p string) bool {
	_, err := os.Lstat(p)
	return err == nil
}

func isDir(p string) bool {
	st, err := os.Stat(p)
	return err == nil && st.IsDir()
}

func isLink(p string) bool {
	st, err := os.Lstat(p)
	return err == nil && st.Mode()&fs.ModeSymlink != 0
}

// writeAtomic 寫到同目錄的暫存檔、fsync，再 rename 蓋過去：不會留下寫一半的檔案。
// like 不是 nil 時沿用它的權限與擁有者。
func writeAtomic(path string, data []byte, mode os.FileMode, like os.FileInfo) error {
	d := filepath.Dir(path)
	if err := os.MkdirAll(d, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(d, "."+filepath.Base(path)+".sse-tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		if like != nil {
			err = os.Chmod(tmp, like.Mode().Perm()|like.Mode()&(fs.ModeSetuid|fs.ModeSetgid|fs.ModeSticky))
			if st, ok := like.Sys().(*syscall.Stat_t); ok && err == nil && os.Geteuid() == 0 {
				err = os.Chown(tmp, int(st.Uid), int(st.Gid))
			}
		} else {
			err = os.Chmod(tmp, mode)
		}
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// remove 刪除檔案；本來就不存在回傳 false。
func remove(p string) bool {
	return os.Remove(p) == nil
}

// copyFile 複製檔案（fsync 後才算完成）。
func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(dst, mode)
	}
	if err != nil {
		os.Remove(dst)
	}
	return err
}

// realpath 跟 Python 的 os.path.realpath 一樣：還不存在的部分原樣接在後面。
func realpath(p string) string {
	p, _ = filepath.Abs(p)
	rest := ""
	for cur := p; ; {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			return filepath.Join(r, rest)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		rest = filepath.Join(filepath.Base(cur), rest)
		cur = parent
	}
}

// selfExe 是正在執行的 sse 執行檔（解開 symlink）。
func selfExe() string {
	exe, err := os.Executable()
	if err != nil {
		return paths.Binary
	}
	if r, err := filepath.EvalSymlinks(exe); err == nil {
		return r
	}
	return exe
}

func statOwner(fi os.FileInfo) uint32 {
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		return st.Uid
	}
	return 0
}

func sameFile(a, b string) bool {
	sa, err1 := os.Stat(a)
	sb, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(sa, sb)
}

// ── DSM / Photos 資訊 ────────────────────────────────────────────────────

func readKV(path string) (map[string]string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if ok {
			out[strings.TrimSpace(k)] = strings.Trim(strings.TrimSpace(v), `"`)
		}
	}
	return out, nil
}

// dsm 是 /etc.defaults/VERSION 的內容；讀不到的欄位是空字串。
type dsm struct{ major, version, build string }

func dsmInfo() dsm {
	kv, err := readKV(paths.DSMVersionFile)
	if err != nil {
		return dsm{}
	}
	return dsm{kv["majorversion"], kv["productversion"], kv["buildnumber"]}
}

func photosVersion() string {
	kv, err := readKV(paths.PhotosInfo)
	if err != nil {
		return ""
	}
	return kv["version"]
}

// orNone：讀不到的值顯示成 None（跟 Python 版的輸出相同）。
func orNone(s string) string {
	if s == "" {
		return "None"
	}
	return s
}

func sockAlive(path string) bool {
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

var probeClient = &http.Client{Transport: &http.Transport{
	Proxy:           nil, // 打的是本機的 DSM，不要經過代理伺服器
	TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
}}

// probeOK 透過 nginx 打 DSM 的 SYNO.API.Info：整條 nginx → (filter) → synoscgi 是否正常。
func probeOK(url string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	resp, err := probeClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return false
	}
	var r struct {
		Success any `json:"success"`
	}
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	return json.Unmarshal(data, &r) == nil && r.Success == true
}

func pickProbe(explicit string, st *state.State) string {
	if explicit != "" {
		if probeOK(explicit) {
			return explicit
		}
		return ""
	}
	var urls []string
	if st != nil && st.ProbeURL != "" {
		urls = append(urls, st.ProbeURL)
	}
	for _, u := range append(urls, probeCandidates...) {
		if probeOK(u) {
			return u
		}
	}
	return ""
}

// filterStatus 是 filter 每秒寫一次的狀態檔（/run/sse/status.json）。
type filterStatus struct {
	Version         string `json:"version"`
	Mode            string `json:"mode"`
	InjectionActive bool   `json:"injection_active"`
	InjectionReason string `json:"injection_reason"`
	Capture         *struct {
		File  string `json:"file"`
		Count int    `json:"count"`
		Max   int    `json:"max"`
	} `json:"capture"`
	Stats struct {
		Connections    int64 `json:"connections"`
		Active         int64 `json:"active"`
		Injected       int64 `json:"injected"`
		InjectSkipped  int64 `json:"inject_skipped"`
		InjectErrors   int64 `json:"inject_errors"`
		UpstreamErrors int64 `json:"upstream_errors"`
	} `json:"stats"`
	LastError *struct {
		Message string  `json:"message"`
		At      float64 `json:"at"`
	} `json:"last_error"`
}

func readFilterStatus() *filterStatus {
	data, err := os.ReadFile(paths.StatusFile)
	if err != nil {
		return nil
	}
	var st filterStatus
	if json.Unmarshal(data, &st) != nil {
		return nil
	}
	return &st
}

// ── 安裝紀錄與設定 ───────────────────────────────────────────────────────

func loadState() *state.State {
	st, err := state.Load()
	if err != nil {
		warn(fmt.Sprintf("安裝紀錄 %s 損毀，當作沒有紀錄處理", paths.State))
		return nil
	}
	if st != nil && st.DataDir == "" {
		// 0.x 版的紀錄：照片擁有者與目錄記在 engine 底下
		var old struct {
			Engine *struct{ User, Home string } `json:"engine"`
		}
		if data, err := os.ReadFile(paths.State); err == nil && json.Unmarshal(data, &old) == nil && old.Engine != nil {
			st.User, st.DataDir = old.Engine.User, old.Engine.Home
		}
	}
	return st
}

func saveState(st *state.State) { must(state.Save(st)) }

func needState() *state.State {
	st := loadState()
	if st == nil || st.DataDir == "" {
		fail("還沒安裝：%s", installCmd)
	}
	return st
}

func loadConfig(path string) *config.Config {
	cfg, err := config.Load(path)
	if err != nil {
		fail("config.yml 有誤：%v", err)
	}
	return cfg
}

func boolPtr(b bool) *bool { return &b }
