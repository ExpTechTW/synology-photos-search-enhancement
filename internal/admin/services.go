package admin

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/app"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/runas"
)

// ── 索引與搜尋服務（以照片擁有者的身分執行 sse 本身） ─────────────────────

// engineCmd 是以 config.yml 執行索引與搜尋指令的參數，例如 sse --config … check。
func engineCmd(cfgPath string, args ...string) []string {
	return append([]string{selfExe(), "--config", cfgPath}, args...)
}

// printIndented 把子程序的輸出縮排後印出（也記到操作紀錄）。
func printIndented(out, indent string) {
	if out == "" {
		return
	}
	for _, line := range strings.Split(out, "\n") {
		say(indent + line)
	}
}

// enginePing 問搜尋服務 {"op":"ping"}，回傳它的回應（ok 才算數）。
func enginePing(sock string, timeout time.Duration) map[string]any {
	c, err := net.DialTimeout("unix", sock, timeout)
	if err != nil {
		return nil
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(timeout))
	if _, err := c.Write([]byte("{\"op\":\"ping\"}\n")); err != nil {
		return nil
	}
	line, err := bufio.NewReaderSize(c, 65536).ReadSlice('\n')
	if err != nil && len(line) == 0 {
		return nil
	}
	var r map[string]any
	if json.Unmarshal(line, &r) != nil || r["ok"] != true {
		return nil
	}
	return r
}

func pingItems(r map[string]any) int {
	n, _ := r["items"].(float64)
	return int(n)
}

// waitEngine 等搜尋服務回應。第一次啟動要建資料庫、載入向量；SSE_ENGINE_WAIT 給測試縮短。
func waitEngine(sock string) map[string]any {
	timeout := 120
	if v, err := strconv.Atoi(os.Getenv("SSE_ENGINE_WAIT")); err == nil && v > 0 {
		timeout = v
	}
	end := time.Now().Add(time.Duration(timeout) * time.Second)
	for time.Now().Before(end) {
		if r := enginePing(sock, 5*time.Second); r != nil {
			return r
		}
		time.Sleep(time.Second)
	}
	fail("搜尋服務沒有在 %d 秒內回應（sudo sse logs engine）", timeout)
	return nil
}

// ── filter ─────────────────────────────────────────────────────────────

func waitFilterReady() *filterStatus {
	const timeout = 20
	end := time.Now().Add(timeout * time.Second)
	for time.Now().Before(end) {
		if st := readFilterStatus(); sockAlive(paths.FilterSock) && st != nil && st.Version != "" {
			return st
		}
		time.Sleep(300 * time.Millisecond)
	}
	fail("filter 沒有在 %d 秒內啟動（sudo sse logs filter）", timeout)
	return nil
}

func waitInjection(mode string) *filterStatus {
	st := waitFilterReady()
	end := time.Now().Add(10 * time.Second)
	for time.Now().Before(end) && (st.Mode != mode || (mode != "passthrough" && !st.InjectionActive)) {
		time.Sleep(500 * time.Millisecond)
		if s := readFilterStatus(); s != nil {
			st = s
		}
	}
	return st
}

// verifyTraffic 確認 DSM 正常，而且流量真的經過 filter 並成功轉給 synoscgi。
//
// 只看健康檢查不夠：filter 連不上 synoscgi 時，nginx 會把 GET 重試到 backup，健康檢查照樣通過。
func verifyTraffic(probeURL string) {
	var conns, errs int64
	if s0 := readFilterStatus(); s0 != nil {
		conns, errs = s0.Stats.Connections, s0.Stats.UpstreamErrors
	}
	end := time.Now().Add(15 * time.Second)
	for time.Now().Before(end) {
		if !probeOK(probeURL) {
			fail("DSM 健康檢查失敗")
		}
		time.Sleep(1200 * time.Millisecond)
		s := readFilterStatus()
		if s != nil && s.Stats.UpstreamErrors > errs {
			msg := ""
			if s.LastError != nil {
				msg = s.LastError.Message
			}
			fail("filter 連不上 synoscgi：%s", msg)
		}
		if s != nil && s.Stats.Connections > conns {
			return
		}
	}
	fail("nginx 沒有把流量送進 filter（設定沒生效）")
}

// ── 身分 ────────────────────────────────────────────────────────────────

var nginxUserRe = regexp.MustCompile(`(?m)^\s*user\s+([^\s;]+)(?:\s+([^\s;]+))?\s*;`)

// nginxWorkerUser 是 nginx worker 的帳號與群組（filter 以這個身分執行）。
func nginxWorkerUser() (string, string) {
	if !paths.Sandbox {
		if entries, err := os.ReadDir("/proc"); err == nil {
			for _, e := range entries {
				if _, err := strconv.Atoi(e.Name()); err != nil {
					continue
				}
				cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
				if err != nil || !strings.Contains(string(cmdline), "nginx: worker process") {
					continue
				}
				fi, err := os.Stat(filepath.Join("/proc", e.Name()))
				if err != nil {
					continue
				}
				st := fi.Sys().(*syscall.Stat_t)
				u, err1 := user.LookupId(strconv.Itoa(int(st.Uid)))
				g, err2 := user.LookupGroupId(strconv.Itoa(int(st.Gid)))
				if err1 == nil && err2 == nil {
					return u.Username, g.Name
				}
			}
		}
	}
	data, err := os.ReadFile(paths.NginxConf)
	if err != nil {
		return "", ""
	}
	m := nginxUserRe.FindStringSubmatch(string(data))
	if m == nil {
		return "", ""
	}
	u, err := user.Lookup(m[1])
	if err != nil {
		return "", ""
	}
	if m[2] != "" {
		return u.Username, m[2]
	}
	g, err := user.LookupGroupId(u.Gid)
	if err != nil {
		return "", ""
	}
	return u.Username, g.Name
}

// connectErrorAs 以 name 的身分連 unix socket：連得上回傳空字串，否則回傳原因。
// 執行的是 sse 本身，所以也一併確認了這個帳號執行得了它（filter 就是以這個身分執行同一個檔案）。
func connectErrorAs(sock, name string) string {
	code, out := runas.Run(name, []string{selfExe(), "connect-check", sock}, 10*time.Second)
	if code == 0 {
		return ""
	}
	if lines := strings.Split(strings.TrimSpace(out), "\n"); out != "" {
		return lines[len(lines)-1]
	}
	return fmt.Sprintf("結束碼 %d", code)
}

// cmdConnectCheck：sse connect-check <socket>（內部用，由 connectErrorAs 以別的帳號執行）。
func cmdConnectCheck(_ app.Global, args []string) int {
	if len(args) != 1 {
		panic(usageError("用法：sse connect-check <socket>"))
	}
	c, err := net.DialTimeout("unix", args[0], 3*time.Second)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	c.Close()
	return 0
}
