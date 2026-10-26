package filter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/version"
)

const (
	chunkSize      = 65536
	captureMaxLine = 262144
)

// 一個連線兩個方向各用一塊 64 KB 的緩衝，用完放回去，記憶體不會隨流量成長。
var bufPool = sync.Pool{New: func() any { b := make([]byte, chunkSize); return &b }}

func getBuf() *[]byte  { return bufPool.Get().(*[]byte) }
func putBuf(b *[]byte) { bufPool.Put(b) }

// 狀態檔 stats 的欄位（順序同 Python 版）
const (
	stConnections = iota
	stActive
	stInspected
	stInjected
	stInjectSkipped
	stInjectErrors
	stUpstreamErrors
	stClientErrors
	stSubrequests
	stEngineErrors
	nStats
)

var statNames = [nStats]string{"connections", "active", "inspected", "injected", "inject_skipped", "inject_errors",
	"upstream_errors", "client_errors", "subrequests", "engine_errors"}

type lastError struct {
	At      int64  `json:"at"`
	Message string `json:"message"`
}

// Filter 是 SCGI filter 本體。
type Filter struct {
	cfg     *Config
	inj     injector // 純轉發模式是 nil
	started time.Time
	stats   [nStats]atomic.Int64
	dirty   atomic.Bool

	errMu     sync.Mutex
	lastError *lastError
	errStart  time.Time
	errN      int

	gateMu        sync.Mutex
	gateAt        time.Time
	gateChecked   bool
	gateActive    bool
	gateReason    string
	photosVersion *string

	capMu    sync.Mutex
	captured int

	conns  sync.WaitGroup
	nConns atomic.Int64
}

func newFilter(cfg *Config, cfgError string) *Filter {
	f := &Filter{cfg: cfg, started: time.Now()}
	f.dirty.Store(true)
	switch cfg.Mode {
	case modeDemo:
		f.inj = &demoInjector{cfg.Demo}
	case modeEngine:
		f.inj = newEngineInjector(f)
	}
	if cfgError != "" {
		f.noteError(cfgError)
	}
	return f
}

// noteError 記下錯誤（狀態檔的 last_error）；60 秒內超過 10 筆就不再逐筆寫 log。
func (f *Filter) noteError(msg string) {
	f.errMu.Lock()
	defer f.errMu.Unlock()
	f.lastError = &lastError{At: time.Now().Unix(), Message: msg}
	f.dirty.Store(true)
	now := time.Now()
	if now.Sub(f.errStart) > 60*time.Second {
		f.errStart, f.errN = now, 0
	}
	if f.errN < 10 {
		lg.error("%s", msg)
	} else if f.errN == 10 {
		lg.error("錯誤太多，接下來 60 秒不再逐筆記錄")
	}
	f.errN++
}

func (f *Filter) bump(i int, n int64) {
	f.stats[i].Add(n)
	f.dirty.Store(true)
}

// ── 診斷錄製 ────────────────────────────────────────────────────────────

type captureRec struct {
	T        int64   `json:"t"`
	Kind     string  `json:"kind"`
	Params   *params `json:"params"`
	Response any     `json:"response"`
}

type truncatedResp struct {
	TruncatedBytes int `json:"truncated_bytes"`
	Summary        any `json:"summary"`
}

type listSummary struct {
	Success json.RawMessage   `json:"success"`
	Count   int               `json:"count"`
	IDs     []json.RawMessage `json:"ids"`
}

type keysSummary struct {
	Success json.RawMessage `json:"success"`
	Keys    []string        `json:"keys"`
}

// summarizeResponse：大回應只留結構與照片 id。
func summarizeResponse(resp any) any {
	switch r := resp.(type) {
	case json.RawMessage:
		if o, ok := parseObject(r); ok {
			success := o.get("success")
			if data, ok := parseObject(o.get("data")); ok {
				if lst, ok := parseArray(data.get("list")); ok {
					s := listSummary{Success: success, Count: len(lst), IDs: []json.RawMessage{}}
					for _, x := range lst[:min(len(lst), 1000)] {
						if xo, ok := parseObject(x); ok {
							s.IDs = append(s.IDs, xo.get("id"))
						}
					}
					return s
				}
				keys := append([]string{}, data.keys...)
				sort.Strings(keys)
				return keysSummary{Success: success, Keys: keys}
			}
			return keysSummary{Success: success}
		}
		return truncRunes(string(r), 2000)
	case string:
		return truncRunes(r, 2000)
	case nil:
		return "None"
	}
	return truncRunes(fmt.Sprint(resp), 2000)
}

func truncRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// capture 把搜尋 API 的請求參數與回應記到診斷檔（不含 _sid 與 SynoToken；cookie 本來就不在參數裡）。
func (f *Filter) capture(kind string, p *params, response any) {
	path := f.cfg.CaptureFile
	if path == "" {
		return
	}
	f.capMu.Lock()
	defer f.capMu.Unlock()
	if float64(f.captured) >= f.cfg.CaptureMax {
		return
	}
	f.captured++
	f.dirty.Store(true)
	rec := captureRec{T: time.Now().Unix(), Kind: kind, Params: p.without(authParams...), Response: response}
	line, err := marshalPlain(rec)
	if err != nil {
		lg.warning("寫不了診斷紀錄：%s", err)
		return
	}
	if n := utf8.RuneCount(line); n > captureMaxLine {
		// 太大就把回應換成摘要（直接截斷字串會讓這一行變成壞掉的 JSON）
		rec.Response = truncatedResp{TruncatedBytes: n, Summary: summarizeResponse(response)}
		line, _ = marshalPlain(rec)
		if len(line) > captureMaxLine*2 {
			line = []byte(truncRunes(string(line), captureMaxLine*2))
		}
	}
	fh, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err == nil {
		_, err = fh.Write(append(line, '\n'))
		if cerr := fh.Close(); err == nil {
			err = cerr
		}
	}
	if err != nil {
		lg.warning("寫不了診斷紀錄：%s", err)
	}
}

// captureRequest：沒被當成搜尋的 Photos 請求也記下參數（不記回應），用來看用戶端實際送了什麼。
func (f *Filter) captureRequest(sn *sniffer) {
	if kind, p, resp, ok := sn.captureInfo(); ok {
		f.capture(kind, p, resp)
	}
}

// ── 注入開關 ────────────────────────────────────────────────────────────

func readPhotosVersion(path string) *string {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := decodeReplace(data)
	text = strings.ReplaceAll(text, "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(line, "version=") {
			v := strings.Trim(pyStrip(line[len("version="):]), `"`)
			return &v
		}
	}
	return nil
}

// injectionActive：模式、緊急開關、Photos 版本，每 5 秒重新確認一次。
func (f *Filter) injectionActive() bool {
	f.gateMu.Lock()
	defer f.gateMu.Unlock()
	now := time.Now()
	if f.gateChecked && now.Sub(f.gateAt) < 5*time.Second {
		return f.gateActive
	}
	active, reason := f.checkGate()
	if reason != f.gateReason {
		if active {
			lg.info("注入狀態：%s", reason)
		} else {
			lg.warning("注入狀態：%s", reason)
		}
		f.dirty.Store(true)
	}
	f.gateAt, f.gateChecked, f.gateActive, f.gateReason = now, true, active, reason
	return active
}

func (f *Filter) checkGate() (bool, string) {
	if f.inj == nil {
		return false, "純轉發模式"
	}
	if _, err := os.Stat(f.cfg.KillSwitch); err == nil {
		return false, fmt.Sprintf("緊急開關已開啟（%s）", f.cfg.KillSwitch)
	}
	f.photosVersion = readPhotosVersion(f.cfg.PhotosInfo)
	want := f.cfg.InjectPhotosVersion
	ws, isStr := rawString(want)
	if !pyTruthy(want) || f.photosVersion == nil || !isStr || *f.photosVersion != ws {
		have := "None"
		if f.photosVersion != nil {
			have = *f.photosVersion
		}
		return false, fmt.Sprintf("Photos 版本 %s 與啟用時的 %s 不同，暫停注入", have, pyStr(want))
	}
	return true, fmt.Sprintf("注入中（%s）", f.cfg.Mode)
}

// ── 轉發 ────────────────────────────────────────────────────────────────

func isTimeout(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

func (f *Filter) handle(c *net.UnixConn) {
	defer f.conns.Done()
	defer f.nConns.Add(-1)
	f.bump(stConnections, 1)
	f.bump(stActive, 1)
	defer f.bump(stActive, -1)
	var up *net.UnixConn
	pumpDone := make(chan struct{})
	pumping := false
	defer func() {
		if r := recover(); r != nil {
			f.bump(stClientErrors, 1)
			f.noteError(fmt.Sprintf("轉發錯誤：%v", r))
		}
		if up != nil {
			up.Close()
		}
		c.Close()
		if pumping {
			<-pumpDone
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), f.cfg.UpstreamConnectTimeout)
	up, err := dialUnix(ctx, f.cfg.Upstream, f.cfg.UpstreamConnectTimeout)
	cancel()
	if err != nil {
		f.bump(stUpstreamErrors, 1)
		f.noteError("連不上 synoscgi：" + err.Error())
		return
	}
	sn := newSniffer(f.cfg.InspectMaxHeader, f.cfg.InspectMaxBody)
	pumping = true
	go func() {
		defer close(pumpDone)
		f.pumpRequest(c, up, sn)
	}()
	if err := f.pumpResponse(up, c, sn); err != nil {
		f.bump(stClientErrors, 1)
	}
}

// pumpRequest 把 request 原樣轉給 synoscgi；先解析副本再轉發：synoscgi 收完 request 才會回應，
// 所以回應到達前一定已解析完。
func (f *Filter) pumpRequest(c, up *net.UnixConn, sn *sniffer) {
	defer func() { recover() }()
	buf := getBuf()
	defer putBuf(buf)
	for {
		n, err := c.Read(*buf)
		if n > 0 {
			sn.feed((*buf)[:n])
			if _, werr := up.Write((*buf)[:n]); werr != nil {
				return
			}
		}
		if err != nil {
			if err == io.EOF {
				// 照 nginx 的行為轉達 EOF，不自己多做或少做
				up.CloseWrite()
			}
			return
		}
	}
}

// readSome 讀一次（有資料就先回傳資料，錯誤留到下一次讀）。
func readSome(conn *net.UnixConn, buf []byte, deadline time.Time) (int, error) {
	conn.SetReadDeadline(deadline)
	for {
		n, err := conn.Read(buf)
		if n > 0 {
			return n, nil
		}
		if err != nil {
			return 0, err
		}
	}
}

func (f *Filter) relay(first []byte, up, c *net.UnixConn, buf []byte) error {
	if len(first) > 0 {
		if _, err := c.Write(first); err != nil {
			return err
		}
	}
	for {
		n, err := readSome(up, buf, time.Now().Add(f.cfg.IdleTimeout))
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		if _, err := c.Write(buf[:n]); err != nil {
			return err
		}
	}
}

func (f *Filter) pumpResponse(up, c *net.UnixConn, sn *sniffer) error {
	bp := getBuf()
	defer putBuf(bp)
	buf := *bp
	n, err := readSome(up, buf, time.Now().Add(f.cfg.IdleTimeout))
	if err == io.EOF {
		return nil
	}
	if err != nil {
		return err
	}
	first := buf[:n]
	capturing := f.cfg.CaptureFile != ""
	var p *plan
	if f.injectionActive() || capturing {
		p = sn.plan()
	}
	if p == nil && capturing {
		f.captureRequest(sn)
	}
	if p == nil || (!capturing && !f.inj.wants(p)) {
		return f.relay(first, up, c, buf)
	}
	f.bump(stInspected, 1)
	deadline := time.Now().Add(f.cfg.InjectTimeout)
	resp := append(make([]byte, 0, 4*chunkSize), first...)
	eof := false
	for len(resp) <= f.cfg.InjectMaxResponse {
		if !time.Now().Before(deadline) {
			break
		}
		n, err := readSome(up, buf, deadline)
		if err == io.EOF {
			eof = true
			break
		}
		if err != nil {
			if isTimeout(err) {
				break
			}
			return err
		}
		resp = append(resp, buf[:n]...)
	}
	if !eof { // 回應太大或太慢：放棄注入，已收到的部分原樣送出後繼續轉發
		f.bump(stInjectSkipped, 1)
		return f.relay(resp, up, c, buf)
	}
	out := f.tryInject(p, resp, deadline)
	if out == nil {
		out = resp
	}
	_, err = c.Write(out)
	return err
}

func (f *Filter) tryInject(p *plan, raw []byte, deadline time.Time) []byte {
	parsed := parseCGIResponse(raw)
	if parsed != nil {
		f.capture("native", p.params, json.RawMessage(parsed.body))
	} else {
		f.capture("native", p.params, decodeReplace(raw[:min(len(raw), 2000)]))
	}
	if parsed == nil || !f.injectionActive() || !f.inj.wants(p) {
		return nil
	}
	remaining := max(100*time.Millisecond, time.Until(deadline))
	ctx, cancel := context.WithTimeout(context.Background(), remaining)
	defer cancel()
	data, err := f.safeRewrite(ctx, p, parsed.body)
	var sk *skipError
	switch {
	case err == nil:
	case errors.As(err, &sk):
		f.bump(stInjectSkipped, 1)
		lg.info("略過注入：%s", sk.msg)
		return nil
	case isNetError(err):
		f.bump(stEngineErrors, 1)
		f.noteError("注入逾時或連線失敗，原樣轉發：" + err.Error())
		return nil
	default:
		f.bump(stInjectErrors, 1)
		f.noteError("注入失敗，原樣轉發：" + err.Error())
		return nil
	}
	if data == nil {
		return nil
	}
	f.bump(stInjected, 1)
	return buildCGIResponse(parsed.lines, data)
}

// safeRewrite：注入的程式出任何問題都當成錯誤處理，原樣轉發（fail-open）。
func (f *Filter) safeRewrite(ctx context.Context, p *plan, body []byte) (data []byte, err error) {
	defer func() {
		if r := recover(); r != nil {
			data, err = nil, fmt.Errorf("panic: %v", r)
		}
	}()
	return f.inj.rewrite(ctx, p, body)
}

// isNetError：逾時、連不上、連線中斷（Python 版的 TimeoutError、ConnectionError、OSError）。
func isNetError(err error) bool {
	var ne net.Error
	return errors.As(err, &ne) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
}

// ── 狀態檔 ──────────────────────────────────────────────────────────────

type orderedStats [nStats]int64

func (s orderedStats) MarshalJSON() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')
	for i, name := range statNames {
		if i > 0 {
			b.WriteByte(',')
		}
		fmt.Fprintf(&b, "%q:%d", name, s[i])
	}
	b.WriteByte('}')
	return b.Bytes(), nil
}

type captureStatus struct {
	File  string          `json:"file"`
	Count int             `json:"count"`
	Max   json.RawMessage `json:"max"`
}

// statusJSON 的欄位與順序跟 Python 版相同（sse status / doctor 會讀）。
type statusJSON struct {
	Version         string         `json:"version"`
	PID             int            `json:"pid"`
	Started         int64          `json:"started"`
	Updated         int64          `json:"updated"`
	Mode            string         `json:"mode"`
	InjectionActive bool           `json:"injection_active"`
	InjectionReason string         `json:"injection_reason"`
	PhotosVersion   *string        `json:"photos_version"`
	Capture         *captureStatus `json:"capture"`
	Stats           orderedStats   `json:"stats"`
	LastError       *lastError     `json:"last_error"`
}

func (f *Filter) status() statusJSON {
	st := statusJSON{
		Version: version.Version,
		PID:     os.Getpid(),
		Started: f.started.Unix(),
		Updated: time.Now().Unix(),
		Mode:    f.cfg.Mode,
	}
	f.gateMu.Lock()
	st.InjectionActive, st.InjectionReason, st.PhotosVersion = f.gateActive, f.gateReason, f.photosVersion
	f.gateMu.Unlock()
	if f.cfg.CaptureFile != "" {
		f.capMu.Lock()
		st.Capture = &captureStatus{File: f.cfg.CaptureFile, Count: f.captured, Max: f.cfg.captureMaxRaw}
		f.capMu.Unlock()
	}
	for i := range f.stats {
		st.Stats[i] = f.stats[i].Load()
	}
	f.errMu.Lock()
	st.LastError = f.lastError
	f.errMu.Unlock()
	return st
}

func (f *Filter) writeStatus() {
	path := f.cfg.StatusFile
	tmp := path + ".tmp"
	f.dirty.Store(false)
	data, err := marshalPlain(f.status())
	if err == nil {
		err = os.WriteFile(tmp, data, 0o644)
	}
	if err == nil {
		err = os.Chmod(tmp, 0o644)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		f.dirty.Store(true)
		lg.warning("寫不了狀態檔 %s：%s", path, err)
	}
}

func (f *Filter) statusLoop(ctx context.Context) {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for {
		f.injectionActive()
		if f.dirty.Load() {
			f.writeStatus()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ── 伺服器 ──────────────────────────────────────────────────────────────

// prepareSocketPath 清掉上次沒清的 socket；已有其他程序在監聽或不是 socket 時拒絕啟動。
func prepareSocketPath(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o777); err != nil {
		return err
	}
	st, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if st.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s 已存在且不是 socket，拒絕覆蓋", path)
	}
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		os.Remove(path) // 上次沒清掉的殘留 socket
		return nil
	}
	c.Close()
	return fmt.Errorf("%s 已有其他程序在監聽，拒絕啟動", path)
}

// fatal：啟動失敗。寫進 log（有 log 檔時也寫 stderr，systemd 的 journal 看得到）。
func fatal(err error) int {
	lg.error("%s", err)
	if lg.toFile() {
		fmt.Fprintln(os.Stderr, err)
	}
	return 1
}

func (f *Filter) serve() int {
	path := f.cfg.Listen
	if err := prepareSocketPath(path); err != nil {
		return fatal(err)
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	defer signal.Stop(sig)
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return fatal(err)
	}
	if err := syscall.Chmod(path, uint32(f.cfg.SocketMode)); err != nil {
		l.Close()
		return fatal(err)
	}
	lg.info("sse-filter %s 啟動：%s → %s（模式 %s）", version.Version, path, f.cfg.Upstream, f.cfg.Mode)

	ctx, cancel := context.WithCancel(context.Background())
	statusDone := make(chan struct{})
	go func() {
		defer close(statusDone)
		f.statusLoop(ctx)
	}()
	acceptDone := make(chan struct{})
	go func() {
		defer close(acceptDone)
		f.acceptLoop(l)
	}()

	<-sig
	// 先移除 socket：nginx 會立刻改走 backup 直連 synoscgi，不必等 fail_timeout
	l.Close()
	os.Remove(path)
	<-acceptDone
	if n := f.nConns.Load(); n > 0 {
		lg.info("等待 %d 個進行中的連線結束", n)
		done := make(chan struct{})
		go func() {
			f.conns.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(25 * time.Second):
		}
	}
	cancel()
	<-statusDone
	f.writeStatus()
	lg.info("sse-filter 已停止")
	return 0
}

func (f *Filter) acceptLoop(l *net.UnixListener) {
	var backoff time.Duration
	for {
		c, err := l.AcceptUnix()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			// 暫時性的錯誤（例如 fd 用完）：稍等再接
			f.noteError("接受連線失敗：" + err.Error())
			backoff = min(max(2*backoff, 5*time.Millisecond), time.Second)
			time.Sleep(backoff)
			continue
		}
		backoff = 0
		f.conns.Add(1)
		f.nConns.Add(1)
		go f.handle(c)
	}
}
