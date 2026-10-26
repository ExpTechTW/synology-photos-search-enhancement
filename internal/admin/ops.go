package admin

import (
	"fmt"
	"os"
	"time"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/app"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/runas"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/state"
)

// ── apply：套用 config.yml ────────────────────────────────────────────────

const applyHelp = `用法：sse apply [--check]

  --check   先檢查 DSM 帳號與模型服務
`

func cmdApply(_ app.Global, args []string) int {
	var check bool
	f := newFlags()
	f.Bool(&check, "--check")
	noArgs(f.parse(args, applyHelp))
	requireRoot()
	defer locked()()
	st := needState()
	path := st.ConfigPath()
	cfg := loadConfig(path)
	ok("config.yml 格式正確")
	if cfg.Synology.Account != st.User {
		warn(fmt.Sprintf("synology.account（%s）跟執行服務的帳號（%s）不同：DSM 登入用前者，檔案權限用後者",
			cfg.Synology.Account, st.User))
	}
	if check {
		code, out := runas.Run(st.User, engineCmd(path, "check"), 600*time.Second)
		printIndented(out, "  ")
		if code != 0 {
			fail("檢查沒通過，沒有套用")
		}
	}
	data := renderFilterConfig(cfg, cfg.Filter.Mode)
	old, err := os.ReadFile(paths.FilterConfig)
	changed := err != nil || string(old) != string(data)
	must(writeAtomic(paths.FilterConfig, data, 0o644, nil))
	if changed || !sockAlive(paths.FilterSock) {
		if code, out := systemctl("restart", filterUnit); code != 0 {
			fail("重啟 filter 失敗：%s", out)
		}
	}
	if code, out := systemctl(append([]string{"restart"}, engineUnits...)...); code != 0 {
		fail("重啟索引器與搜尋服務失敗：%s", out)
	}
	waitEngine(cfg.Search.Socket)
	ok("索引器與搜尋服務已用新設定重新啟動")
	fst := waitInjection(cfg.Filter.Mode)
	ok(fmt.Sprintf("filter：模式 %s（%s）", fst.Mode, fst.InjectionReason))
	st.PhotosVersion = photosVersion()
	saveState(st)
	return 0
}

// ── 緊急開關 ─────────────────────────────────────────────────────────────

func cmdDisable(_ app.Global, args []string) int {
	noArgs(newFlags().parse(args, "用法：sse disable\n"))
	requireRoot()
	needState()
	must(writeAtomic(paths.KillSwitch, nil, 0o644, nil))
	ok("緊急開關已開啟：5 秒內停止注入，filter 繼續純轉發。恢復：sudo sse enable")
	return 0
}

func cmdEnable(_ app.Global, args []string) int {
	noArgs(newFlags().parse(args, "用法：sse enable\n"))
	requireRoot()
	needState()
	remove(paths.KillSwitch)
	ok("緊急開關已關閉")
	return 0
}

// ── nginx：重新套用、bypass、guard ───────────────────────────────────────

func applyNginxNow(st *state.State, probeURL string) {
	say("重新套用前檢查：")
	build := dsmInfo().build
	if good, out := nginxTest(paths.NginxConf); !good {
		fail("nginx -t 沒通過：%s", out)
	}
	ok("nginx -t 通過")
	kind, why := nginxClassify(readNginxConf())
	if kind == unknown {
		fail("%s", why)
	}
	ok("nginx upstream：" + why)
	if !sockAlive(paths.SynoscgiSock) {
		fail("連不上 synoscgi")
	}
	if !sockAlive(paths.FilterSock) {
		systemctl("start", filterUnit)
		waitFilterReady()
	}
	ok("filter 執行中")
	url := pickProbe(probeURL, st)
	if url == "" {
		fail("DSM 健康檢查失敗")
	}
	changed := nginxApply(url)
	verifyTraffic(url)
	if changed {
		ok("nginx 已重新套用，流量確認經過 filter")
	} else {
		ok("nginx 原本就是套用狀態，流量確認經過 filter")
	}
	st.DSMBuild, st.PhotosVersion, st.ProbeURL, st.AutoApply = build, photosVersion(), url, boolPtr(true)
	saveState(st)
}

func cmdReapply(_ app.Global, args []string) int {
	probeURL := probeFlag(args, "用法：sse reapply [--probe-url URL]\n")
	requireRoot()
	defer locked()()
	applyNginxNow(needState(), probeURL)
	return 0
}

const bypassHelp = `用法：sse bypass on|off [--probe-url URL]

  on    nginx 直連 synoscgi（filter 不在路徑上，排查問題用）
  off   恢復經過 filter
`

func cmdBypass(_ app.Global, args []string) int {
	var probeURL string
	f := newFlags()
	f.String(&probeURL, "--probe-url")
	pos := f.parse(args, bypassHelp)
	if len(pos) != 1 {
		panic(usageError("要指定 on 或 off"))
	}
	choice("bypass", pos[0], "on", "off")
	requireRoot()
	defer locked()()
	st := needState()
	if pos[0] == "on" {
		st.AutoApply = boolPtr(false)
		saveState(st)
		changed := nginxRevert()
		note := ""
		if !changed {
			note = "（原本就是）"
		}
		ok("nginx 已直連 synoscgi" + note + "；filter 仍在跑但不在路徑上")
		say("恢復：sudo sse bypass off")
	} else {
		applyNginxNow(st, probeURL)
	}
	return 0
}

// cmdGuard 由 systemd 觸發（nginx.conf 被改寫、開機、每 10 分鐘）。條件不安全就什麼都不做。
func cmdGuard(_ app.Global, args []string) int {
	noArgs(newFlags().parse(args, "用法：sse guard（由 systemd 執行）\n"))
	defer locked()()
	st := loadState()
	if st == nil || !st.AutoApplyOn() {
		return 0
	}
	if build := dsmInfo().build; build != st.DSMBuild {
		sayf("DSM 已更新（build %s → %s），暫停自動套用；確認正常後執行 sudo sse reapply", orNone(st.DSMBuild), orNone(build))
		return 0
	}
	data, err := os.ReadFile(paths.NginxConf)
	if err != nil {
		sayf("讀不到 nginx.conf：%v", err)
		return 1
	}
	kind, why := nginxClassify(string(data))
	switch {
	case kind == managed:
		return 0
	case kind == unknown:
		say("nginx.conf 格式跟預期不同，不自動套用：" + why)
		return 1
	case !sockAlive(paths.FilterSock):
		say("filter 沒有在跑，不套用")
		return 0
	}
	if err := runStep(func() error { nginxApply(st.ProbeURL); return nil }); err != nil {
		say("重新套用失敗（nginx 維持原廠設定）：" + err.Error())
		return 1
	}
	say("nginx.conf 被重新產生過，已重新套用")
	return 0
}

// ── 診斷錄製 ─────────────────────────────────────────────────────────────

const captureHelp = `用法：sse capture on|off|show [--max 200]

  on      開始記錄搜尋 API 的請求與回應（不含 cookie、_sid、SynoToken）
  show    顯示紀錄
  off     停止記錄並刪除紀錄
  --max N 最多記幾筆（預設 200）
`

func cmdCapture(_ app.Global, args []string) int {
	max := 200
	f := newFlags()
	f.Int(&max, "--max")
	pos := f.parse(args, captureHelp)
	if len(pos) != 1 {
		panic(usageError("要指定 on、off 或 show"))
	}
	action := pos[0]
	choice("capture", action, "on", "off", "show")
	requireRoot()
	defer locked()()
	needState()
	if action == "show" {
		data, err := os.ReadFile(paths.CaptureFile)
		if err != nil {
			say("沒有診斷紀錄")
		} else {
			os.Stdout.Write(data)
		}
		return 0
	}
	data, err := os.ReadFile(paths.FilterConfig)
	must(err)
	fc, err := parseObject(data)
	if err != nil {
		fail("%s：%v", paths.FilterConfig, err)
	}
	if action == "on" {
		fc.set("capture_file", paths.CaptureFile)
		fc.set("capture_max", max)
	} else {
		fc.del("capture_file")
		fc.del("capture_max")
		remove(paths.CaptureFile)
	}
	out := fc.marshal()
	validateFilterConfig(out)
	must(writeAtomic(paths.FilterConfig, out, 0o644, nil))
	if code, msg := systemctl("restart", filterUnit); code != 0 {
		fail("重啟 filter 失敗：%s", msg)
	}
	waitFilterReady()
	if action == "on" {
		ok(fmt.Sprintf("開始記錄搜尋 API（最多 %d 筆，存在 %s，重開機就會消失）", max, paths.CaptureFile))
		say("  記錄內容含搜尋字與照片檔名，但不含 cookie、_sid、SynoToken。看完請執行 sudo sse capture off")
	} else {
		ok("已停止記錄並刪除紀錄")
	}
	return 0
}
