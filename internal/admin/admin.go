// Package admin：安裝、解除安裝與維運指令（以 root 執行）。
//
// 對 DSM 的變更只有這些，解除安裝會全部移除：
//
//	/usr/local/sse/            程式（sse 執行檔，root 擁有）
//	/usr/local/bin/sse         指令（指向 /usr/local/sse/sse 的 symlink）
//	/usr/local/etc/sse/        安裝紀錄、filter 設定（由 config.yml 產生）、緊急開關
//	/var/log/sse/              filter 與 sse 的 log
//	/etc/systemd/system/sse-*  四個服務
//	/etc/nginx/nginx.conf      upstream synoscgi 改成「filter 為主、synoscgi 為 backup」，前後有標記
//	/etc/nginx/nginx.conf.sse-orig   套用前的原廠設定備份
//
// 使用者的資料目錄（預設 ~/sse：config.yml、索引資料庫、log）解除安裝時會詢問要不要刪，預設保留。
package admin

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/admin/wizard"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/app"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/logs"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/version"
)

// bootstrap 與 connect-check 是內部用的指令（install.sh 與安裝前檢查呼叫），不列在說明裡。
var commands = map[string]func(app.Global, []string) int{
	"install": cmdInstall, "update": cmdUpdate, "apply": cmdApply, "uninstall": cmdUninstall,
	"status": cmdStatus, "doctor": cmdDoctor, "logs": cmdLogs, "disable": cmdDisable, "enable": cmdEnable,
	"bypass": cmdBypass, "reapply": cmdReapply, "capture": cmdCapture, "guard": cmdGuard,
	"bootstrap": cmdBootstrap, "connect-check": cmdConnectCheck,
}

// Handles：cmd 是不是這個套件的指令。
func Handles(cmd string) bool { return commands[cmd] != nil }

// 唯讀的指令不寫操作紀錄（sse.log）。
var readonly = map[string]bool{"status": true, "doctor": true, "logs": true, "connect-check": true}

// Main 執行管理指令。
func Main(g app.Global, cmd string, args []string) (rc int) {
	fn := commands[cmd]
	if fn == nil {
		fmt.Fprintf(os.Stderr, "錯誤：不認得的指令 %s\n", cmd)
		return 2
	}
	defer func() {
		switch r := recover().(type) {
		case nil:
		case Abort:
			bad(string(r))
			rc = 1
		case usageError:
			fmt.Fprintf(os.Stderr, "錯誤：%s（sse %s --help 看用法）\n", string(r), cmd)
			rc = 2
		case exitCode:
			rc = int(r)
		default:
			panic(r)
		}
	}()
	catchInterrupt()
	switch {
	case readonly[cmd] || (cmd == "capture" && len(args) > 0 && args[0] == "show") || (cmd == "install" && hasArg(args, "--dry-run")):
		stopAudit()
	case cmd != "install" && cmd != "update" && cmd != "bootstrap" && (os.Geteuid() == 0 || paths.Sandbox):
		startAudit(strings.Join(g.Argv, " "), 3)
	}
	return fn(g, args)
}

func hasArg(args []string, a string) bool {
	for _, x := range args {
		if x == a {
			return true
		}
	}
	return false
}

// Abort 中止指令：在任何地方 panic(Abort(…))，由 Main 印出原因、結束碼 1（跟 Python 版的 Abort 例外相同）。
type Abort string

func fail(format string, a ...any) { panic(Abort(fmt.Sprintf(format, a...))) }

// must：檔案操作之類的錯誤直接中止指令。
func must(err error) {
	if err != nil {
		panic(Abort(err.Error()))
	}
}

type usageError string

type exitCode int

// ── 中斷 ────────────────────────────────────────────────────────────────

// childRunning：正在執行子程序（新版的 sse）時，Ctrl-C 交給子程序處理，等它結束再決定要不要換回舊版。
var childRunning atomic.Bool

func catchInterrupt() {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt)
	go func() {
		for range ch {
			if childRunning.Load() {
				continue
			}
			wizard.RestoreTerminal()
			fmt.Println()
			bad("已中斷")
			os.Exit(130)
		}
	}()
}

// ── 輸出（有變更的指令也記到 /var/log/sse/sse.log） ──────────────────────

var audit struct {
	w       *logs.DailyWriter
	pending []string
	off     bool
}

func say(msg string) {
	fmt.Println(msg)
	switch {
	case audit.w != nil:
		auditLine(msg)
	case !audit.off:
		audit.pending = append(audit.pending, msg)
	}
}

func sayf(format string, a ...any) { say(fmt.Sprintf(format, a...)) }

func auditLine(msg string) {
	audit.w.Write([]byte(logs.Stamp(time.Now()) + " " + msg + "\n"))
}

// startAudit 開始寫操作紀錄；之前輸出的內容（例如安裝前檢查）也一併寫進去。還沒安裝（沒有 log 目錄）就不寫。
func startAudit(cmd string, keepDays int) {
	if audit.w != nil || !isDir(paths.LogDir) {
		return
	}
	w, err := logs.OpenDaily(filepath.Join(paths.LogDir, "sse.log"), keepDays)
	if err != nil {
		return
	}
	audit.w = w
	auditLine(fmt.Sprintf("── sse %s %s ──", version.Version, cmd))
	for _, line := range audit.pending {
		auditLine(line)
	}
	audit.pending = nil
}

func stopAudit() { audit.off, audit.pending = true, nil }

func ok(msg string)   { say("  ✓ " + msg) }
func warn(msg string) { say("  ! " + msg) }
func bad(msg string)  { say("  ✗ " + msg) }

// ── 選項 ────────────────────────────────────────────────────────────────

// flags 是一個指令的選項：--name VALUE、--name=VALUE、布林的 --name；其餘是位置參數。
type flags struct {
	bools map[string]*bool
	strs  map[string]*string
	ints  map[string]*int
}

func newFlags() *flags {
	return &flags{bools: map[string]*bool{}, strs: map[string]*string{}, ints: map[string]*int{}}
}

func (f *flags) Bool(p *bool, names ...string) {
	for _, n := range names {
		f.bools[n] = p
	}
}

func (f *flags) String(p *string, names ...string) {
	for _, n := range names {
		f.strs[n] = p
	}
}

func (f *flags) Int(p *int, names ...string) {
	for _, n := range names {
		f.ints[n] = p
	}
}

// parse 解析選項，回傳位置參數。--help 印出 help 後結束。
func (f *flags) parse(args []string, help string) []string {
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "-h" || a == "--help" {
			fmt.Print(help)
			panic(exitCode(0))
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(a, "=")
		if p := f.bools[name]; p != nil && !hasVal {
			*p = true
			continue
		}
		sp, ip := f.strs[name], f.ints[name]
		if sp == nil && ip == nil {
			panic(usageError("不認得的選項 " + a))
		}
		if !hasVal {
			if i+1 >= len(args) {
				panic(usageError(name + " 要指定值"))
			}
			i++
			val = args[i]
		}
		if sp != nil {
			*sp = val
		} else {
			n, err := strconv.Atoi(val)
			if err != nil {
				panic(usageError(name + " 必須是整數"))
			}
			*ip = n
		}
	}
	return pos
}

func choice(name, v string, allowed ...string) {
	for _, a := range allowed {
		if v == a {
			return
		}
	}
	panic(usageError(fmt.Sprintf("%s 只能是 %s", name, strings.Join(allowed, "、"))))
}

func noArgs(pos []string) {
	if len(pos) > 0 {
		panic(usageError("多餘的參數 " + strings.Join(pos, " ")))
	}
}

// ── 共用 ────────────────────────────────────────────────────────────────

const installCmd = "curl -fsSL https://raw.githubusercontent.com/ExpTechTW/synology-photos-search-enhancement/main/install.sh | sudo sh"

func requireRoot() {
	if os.Geteuid() != 0 && !paths.Sandbox {
		fail("請用 root 執行：sudo sse ...")
	}
}

// locked 取得 sse 指令之間的鎖（同時只能有一個在改系統），回傳釋放的函式。
func locked() func() {
	must(os.MkdirAll(filepath.Dir(paths.LockFile), 0o755))
	f, err := os.OpenFile(paths.LockFile, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	must(err)
	if syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB) != nil {
		say("另一個 sse 指令正在執行，等待中…")
		must(syscall.Flock(int(f.Fd()), syscall.LOCK_EX))
	}
	return func() { f.Close() }
}

// stdinIsTerminal：可以互動詢問（測試時換掉）。
var stdinIsTerminal = func() bool { return wizard.IsTerminal(int(os.Stdin.Fd())) }

func confirm(yes bool, question string) {
	if yes {
		return
	}
	if !stdinIsTerminal() {
		fail("非互動模式請加 --yes")
	}
	if !wizard.AskYes(question, false) {
		fail("已取消，沒有做任何變更")
	}
}
