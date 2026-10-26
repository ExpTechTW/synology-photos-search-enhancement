package admin

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/app"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/config"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/runas"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/state"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/version"
)

// ── status / doctor / logs ──────────────────────────────────────────────

type unitState struct{ name, active, enabled string }

type info struct {
	state       *state.State
	dsm         dsm
	photos      string
	units       []unitState
	filterAlive bool
	filter      *filterStatus
	killSwitch  bool
	engine      map[string]any
	cfg         *config.Config
	cfgError    string
	nginxKind   string
	nginxWhy    string
}

func collect() *info {
	in := &info{state: loadState(), dsm: dsmInfo(), photos: photosVersion(), filterAlive: sockAlive(paths.FilterSock),
		filter: readFilterStatus(), killSwitch: exists(paths.KillSwitch)}
	for _, u := range allUnits {
		if exists(filepath.Join(paths.UnitDir, u)) {
			_, active := systemctl("is-active", u)
			_, enabled := systemctl("is-enabled", u)
			if active == "" {
				active = "?"
			}
			if enabled == "" {
				enabled = "?"
			}
			in.units = append(in.units, unitState{u, active, enabled})
		}
	}
	if in.state != nil && in.state.DataDir != "" {
		cfg, err := config.Load(in.state.ConfigPath())
		if err != nil {
			in.cfgError = err.Error()
		} else {
			in.cfg = cfg
			in.engine = enginePing(cfg.Search.Socket, 5*time.Second)
		}
	}
	if data, err := os.ReadFile(paths.NginxConf); err != nil {
		in.nginxKind, in.nginxWhy = unknown, fmt.Sprintf("讀不到 nginx.conf：%v", err)
	} else {
		in.nginxKind, in.nginxWhy = nginxClassify(string(data))
	}
	return in
}

func show(in *info) {
	st := in.state
	say("synology-photos-search-enhancement " + version.Version)
	if st == nil {
		say("  還沒安裝")
	} else {
		sayf("  照片擁有者：%s，資料目錄：%s", orNone(st.User), orNone(st.DataDir))
		channel, source := st.Channel, st.Source
		if channel == "" {
			channel = "release"
		}
		from := "release " + source
		if source == "" || source == "local" {
			from = "從原始碼安裝"
		}
		sayf("  更新管道：%s（%s；檢查新版：sudo sse update --check）", channel, from)
	}
	sayf("  DSM %s（build %s），Photos %s", orNone(in.dsm.version), orNone(in.dsm.build), orNone(in.photos))
	say("  nginx：" + in.nginxWhy)
	for _, u := range in.units {
		sayf("  %-26s %s / %s", u.name, u.active, u.enabled)
	}
	f := in.filter
	if in.filterAlive && f != nil {
		s := f.Stats
		sayf("  filter：模式 %s，%s", f.Mode, f.InjectionReason)
		sayf("          連線 %d（進行中 %d）、注入 %d、略過 %d、注入錯誤 %d、synoscgi 錯誤 %d",
			s.Connections, s.Active, s.Injected, s.InjectSkipped, s.InjectErrors, s.UpstreamErrors)
		if f.LastError != nil {
			at := time.Unix(int64(f.LastError.At), 0).Format("2006-01-02 15:04:05")
			sayf("          最後錯誤：%s（%s）", f.LastError.Message, at)
		}
	} else {
		say("  filter：沒有在跑（nginx 會自動直連 synoscgi，DSM 照常運作）")
	}
	switch {
	case in.cfgError != "":
		say("  config.yml：" + in.cfgError)
	case st != nil && in.engine == nil:
		say("  搜尋服務：沒有回應")
	case in.engine != nil:
		sayf("  搜尋服務：索引 %d 張", pingItems(in.engine))
	}
	if f != nil && f.Capture != nil {
		sayf("  診斷錄製：%s（%d / %d 筆）", f.Capture.File, f.Capture.Count, f.Capture.Max)
	}
	if in.killSwitch {
		say("  緊急開關：開啟（不注入）")
	}
}

func probeFlag(args []string, help string) string {
	var probeURL string
	f := newFlags()
	f.String(&probeURL, "--probe-url")
	noArgs(f.parse(args, help))
	return probeURL
}

func cmdStatus(_ app.Global, args []string) int {
	probeURL := probeFlag(args, "用法：sse status [--probe-url URL]\n")
	in := collect()
	show(in)
	st := in.state
	if st != nil && st.DataDir != "" && in.cfg != nil {
		_, out := runas.Run(st.User, engineCmd(st.ConfigPath(), "progress"), 60*time.Second)
		say("")
		printIndented(out, "")
	}
	result := "失敗"
	if pickProbe(probeURL, st) != "" {
		result = "正常"
	}
	say("\n  DSM 健康檢查：" + result)
	return 0
}

const logsHelp = `用法：sse logs [filter|engine|indexer|sse] [-n 50] [-f]

  -n N          最後幾行（預設 50）
  -f, --follow  持續顯示新的 log
`

func cmdLogs(_ app.Global, args []string) int {
	n, follow := 50, false
	f := newFlags()
	f.Int(&n, "-n")
	f.Bool(&follow, "-f", "--follow")
	pos := f.parse(args, logsHelp)
	source := "indexer"
	if len(pos) > 1 {
		noArgs(pos[1:])
	}
	if len(pos) == 1 {
		source = pos[0]
	}
	choice("log", source, "filter", "engine", "indexer", "sse")
	requireRoot()
	var path string
	if source == "filter" || source == "sse" {
		path = filepath.Join(paths.LogDir, source+".log")
	} else {
		path = filepath.Join(needState().DataDir, "logs", source+".log")
	}
	if !exists(path) {
		fail("還沒有 log：%s", path)
	}
	var older []string
	if entries, err := os.ReadDir(filepath.Dir(path)); err == nil {
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), filepath.Base(path)+".") {
				older = append(older, e.Name())
			}
		}
	}
	sort.Strings(older)
	extra := ""
	if len(older) > 0 {
		extra = "（另有：" + strings.Join(older, "、") + "）"
	}
	fmt.Printf("==> %s%s <==\n", path, extra)
	if follow {
		tail, err := exec.LookPath("tail")
		if err == nil {
			err = syscall.Exec(tail, []string{"tail", "-n", fmt.Sprint(n), "-F", path}, os.Environ())
		}
		fail("執行不了 tail：%v", err)
	}
	code, out := run([]string{"tail", "-n", fmt.Sprint(n), path}, 60*time.Second)
	fmt.Println(out)
	return code
}

func cmdDoctor(_ app.Global, args []string) int {
	probeURL := probeFlag(args, "用法：sse doctor [--probe-url URL]\n")
	in := collect()
	show(in)
	say("\n診斷：")
	st := in.state
	var advice []string
	if pickProbe(probeURL, st) != "" {
		ok("DSM 健康檢查正常")
	} else {
		bad("DSM 健康檢查失敗")
	}
	if good, out := nginxTest(paths.NginxConf); good {
		ok("nginx -t 通過")
	} else {
		bad("nginx -t 沒通過：" + out)
	}
	if sockAlive(paths.SynoscgiSock) {
		ok("synoscgi socket")
	} else {
		bad("synoscgi socket")
	}
	if st != nil {
		if in.dsm.build != st.DSMBuild {
			warn(fmt.Sprintf("DSM 更新過（build %s → %s），已暫停自動套用 nginx 設定", orNone(st.DSMBuild), orNone(in.dsm.build)))
			advice = append(advice, "確認 DSM 與 Photos 都正常後，執行 sudo sse reapply 重新套用")
		}
		if !st.AutoApplyOn() {
			warn("目前是 bypass 狀態（nginx 直連 synoscgi）")
			advice = append(advice, "要恢復時執行 sudo sse bypass off")
		} else if in.nginxKind == pristine && in.dsm.build == st.DSMBuild {
			warn("nginx 是原廠設定，guard 應該會在 10 分鐘內重新套用")
			advice = append(advice, "想立刻套用可執行 sudo sse reapply")
		}
		if in.nginxKind == unknown {
			bad("nginx.conf 的格式跟預期不同，本外掛不會自動修改它")
			advice = append(advice, "參考 README 的「手動還原」")
		}
		if !in.filterAlive {
			bad("filter 沒有在跑")
			advice = append(advice, "看 log：sudo sse logs filter")
		}
		if in.cfgError != "" {
			bad("config.yml 有誤：" + in.cfgError)
			advice = append(advice, "修正 config.yml 後執行 sudo sse apply")
		}
		f := in.filter
		if f == nil {
			f = &filterStatus{}
		}
		if f.Mode == "engine" && in.engine == nil {
			bad("搜尋服務沒有回應（搜尋結果暫時只有原生的）")
			advice = append(advice, "看 log：sudo sse logs engine")
		}
		if f.Mode == "engine" && !f.InjectionActive && !in.killSwitch {
			warn("注入暫停中：" + orNone(f.InjectionReason))
			advice = append(advice, "Photos 更新過的話，確認搜尋正常後執行 sudo sse apply 重新啟用")
		}
	}
	for _, p := range oldVersionLeftovers(st) {
		warn("舊版留下的檔案：" + p)
	}
	if len(advice) > 0 {
		say("\n建議：")
		for _, a := range advice {
			say("  - " + a)
		}
	} else {
		say("\n沒有發現問題。")
	}
	return 0
}
