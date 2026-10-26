package admin

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/admin/wizard"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/app"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/config"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/runas"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/state"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/version"
)

const installHelp = `用法：sse install [選項]（通常由 install.sh 呼叫）

  --user NAME         照片擁有者的 DSM 帳號（預設：執行 sudo 的帳號）
  --data-dir DIR      放 config.yml 與索引的目錄（預設：~帳號/sse）
  --from-config FILE  用這份 config.yml，不跑設定精靈
  --dry-run           只檢查，不做任何變更
  --yes               不詢問直接執行
  --probe-url URL     DSM 健康檢查網址（預設自動偵測 5001/5000）
`

type installOpts struct {
	user, dataDir, importConfig, probeURL, channel, source string
	dryRun, yes                                            bool
}

func parseInstall(args []string) installOpts {
	var o installOpts
	f := newFlags()
	f.String(&o.user, "--user")
	f.String(&o.dataDir, "--data-dir")
	f.String(&o.importConfig, "--from-config")
	f.String(&o.probeURL, "--probe-url")
	f.String(&o.channel, "--channel") // install.sh / sse update 傳入
	f.String(&o.source, "--source")
	f.Bool(&o.dryRun, "--dry-run")
	f.Bool(&o.yes, "--yes")
	noArgs(f.parse(args, installHelp))
	if o.channel != "" {
		choice("--channel", o.channel, "release", "pre-release")
	}
	return o
}

// cmdInstall：sse install。
//
// 不是從 /usr/local/sse/sse 執行時（install.sh 下載的新版、sse update 下載的新版、開發時的 build/sse），
// 先把自己裝到 /usr/local/sse/sse，再用它執行安裝；安裝失敗就換回原本的程式。
func cmdInstall(g app.Global, args []string) int {
	o := parseInstall(args)
	requireRoot()
	if !o.dryRun && !sameFile(selfExe(), paths.Binary) {
		return selfInstall(g, args)
	}
	return install(o)
}

// ── 程式放到 /usr/local/sse ──────────────────────────────────────────────

// pythonLeftovers 是 Python 版留在 /usr/local/sse 的東西，換成 Go 版之後用不到了。
var pythonLeftovers = []string{"venv", "venv.new", "venv.prev", "python", "bin", "cache", "install.sh",
	"install.sh.tmp", "sse-filter.py", "ssectl.py", "systemd"}

// selfInstall 把正在執行的 sse 裝成 /usr/local/sse/sse（舊的留一份 sse.prev），再用它執行 sse install。
func selfInstall(g app.Global, args []string) int {
	exe := selfExe()
	fresh := !exists(paths.Prefix)
	must(os.MkdirAll(paths.Prefix, 0o755))
	// 程式要讓 filter（http）與照片擁有者都能執行；也修好在 umask 077 下裝出來的 700 目錄
	must(os.Chmod(paths.Prefix, 0o755))
	prev, next := paths.Binary+".prev", paths.Binary+".new"
	os.Remove(next) // 上次中斷留下的
	os.Remove(prev)
	if exists(paths.Binary) {
		if os.Link(paths.Binary, prev) != nil {
			must(copyFile(paths.Binary, prev, 0o755))
		}
	}
	must(copyFile(exe, next, 0o755))
	must(os.Rename(next, paths.Binary))

	childArgs := []string{"install"}
	if g.Verbose {
		childArgs = []string{"-v", "install"}
	}
	// 失敗時由這裡換回舊程式後只重啟一次服務；子程序的回復就不重啟（否則會重啟兩次，每次都要等舊的服務停下來）。
	// 子程序開始改動系統時會建立 changed；在安裝前檢查就失敗時服務沒動過，不必重啟
	changed := filepath.Join(paths.Prefix, ".install-changed")
	os.Remove(changed)
	os.Setenv(deferRestartEnv, changed)
	rc := runChild(paths.Binary, append(childArgs, args...)...)
	os.Unsetenv(deferRestartEnv)
	touched := exists(changed)
	os.Remove(changed)
	if rc == 0 {
		remove(prev)
		return 0
	}
	switch {
	case fresh:
		// 第一次安裝就失敗：sse install 已回復系統設定，程式也不留
		os.RemoveAll(paths.Prefix)
	default:
		// sse install 失敗時已經把系統設定回復成原本的樣子；這裡把程式也換回去，再重啟用到程式的服務
		say("Restoring the previous version / 換回原本的版本")
		if exists(prev) {
			os.Rename(prev, paths.Binary)
		} else {
			remove(paths.Binary) // 原本是 Python 版：服務已經改回用 Python 執行
		}
		if touched {
			for _, u := range binaryUnits {
				if exists(filepath.Join(paths.UnitDir, u)) {
					systemctl("restart", u)
				}
			}
		}
	}
	return rc
}

// ── 安裝前檢查 ───────────────────────────────────────────────────────────

type installCtx struct {
	dsmBuild, photosVersion, nginxKind, nginxUser, nginxGroup, probeURL, user string
}

func preflight(probeURL string, st *state.State) *installCtx {
	say("安裝前檢查：")
	var problems []string
	check := func(cond bool, good, why string) bool {
		if cond {
			ok(good)
		} else {
			bad(why)
			problems = append(problems, why)
		}
		return cond
	}
	ctx := &installCtx{}
	d := dsmInfo()
	major := "None"
	if d.major != "" {
		major = "'" + d.major + "'"
	}
	check(d.major == "7", fmt.Sprintf("DSM %s（build %s）", orNone(d.version), orNone(d.build)),
		fmt.Sprintf("需要 DSM 7（讀到 %s）", major))
	ctx.dsmBuild = d.build
	pv := photosVersion()
	check(pv != "", "Synology Photos "+pv, fmt.Sprintf("找不到 Synology Photos（%s）", paths.PhotosInfo))
	ctx.photosVersion = pv
	code, out := systemctl("--version")
	check(code == 0, "systemd 可用", "systemctl 無法執行："+out)
	if data, err := os.ReadFile(paths.NginxConf); err != nil {
		check(false, "", fmt.Sprintf("讀不到 %s：%v", paths.NginxConf, err))
	} else {
		check(!isLink(paths.NginxConf), "nginx.conf 是一般檔案", "nginx.conf 是 symlink，跟預期不同")
		good, out := nginxTest(paths.NginxConf)
		check(good, "nginx -t 目前的設定通過", "nginx -t 目前就不通過，請先處理："+out)
		kind, why := nginxClassify(string(data))
		check(kind == pristine || kind == managed, "nginx upstream："+why, why)
		ctx.nginxKind = kind
	}
	check(sockAlive(paths.SynoscgiSock), "synoscgi socket 正常", "連不上 "+paths.SynoscgiSock)
	ctx.nginxUser, ctx.nginxGroup = nginxWorkerUser()
	if check(ctx.nginxUser != "", fmt.Sprintf("nginx worker 身分：%s:%s", ctx.nginxUser, ctx.nginxGroup),
		"判斷不出 nginx worker 的身分") {
		err := connectErrorAs(paths.SynoscgiSock, ctx.nginxUser)
		check(err == "", ctx.nginxUser+" 可以執行 sse、連得上 synoscgi（filter 會用這個身分執行）",
			ctx.nginxUser+" 連不上 synoscgi："+err)
	}
	ctx.probeURL = pickProbe(probeURL, st)
	check(ctx.probeURL != "", "DSM 健康檢查："+ctx.probeURL, "DSM 健康檢查網址都失敗，請用 --probe-url 指定")
	if len(problems) > 0 {
		fail("有 %d 項檢查沒通過，沒有做任何變更。", len(problems))
	}
	return ctx
}

// resolveOwner 是照片擁有者（索引與搜尋服務以他的身分執行）與資料目錄。
func resolveOwner(o installOpts, st *state.State) (*runas.Account, string) {
	name := o.user
	if name == "" && st != nil {
		name = st.User
	}
	if name == "" {
		name = os.Getenv("SUDO_USER")
	}
	if name == "" || name == "root" {
		fail("要指定照片擁有者的帳號：%s -s -- --user <DSM 帳號>", installCmd)
	}
	acct, err := runas.Lookup(name)
	if err != nil {
		fail("這台 NAS 沒有 %s 這個帳號", name)
	}
	if acct.UID == 0 {
		fail("索引與搜尋服務不能用 root 執行，請指定照片擁有者的帳號")
	}
	dataDir := o.dataDir
	if dataDir == "" && st != nil {
		dataDir = st.DataDir
	}
	if dataDir == "" {
		if !isDir(acct.Home) {
			fail("%s 沒有家目錄（%s）：請到 DSM 控制台 > 使用者 > 進階設定 啟用家目錄服務，"+
				"或用 --data-dir 指定共用資料夾裡的目錄", name, acct.Home)
		}
		dataDir = filepath.Join(acct.Home, "sse")
	}
	return acct, realpath(dataDir)
}

func prepareDataDir(acct *runas.Account, dataDir string) {
	if fi, err := os.Stat(dataDir); err == nil && !paths.Sandbox {
		if statOwner(fi) != acct.UID {
			fail("%s 的擁有者不是 %s", dataDir, acct.Name)
		}
	}
	for _, d := range []string{dataDir, filepath.Join(dataDir, "data"), filepath.Join(dataDir, "logs")} {
		must(os.MkdirAll(d, 0o700))
		// 只有照片擁有者能進入：裡面有 config.yml 與照片裡讀出的文字。DSM 的 homes 有 ACL，新目錄可能繼承成所有人可寫
		must(os.Chmod(d, 0o700))
		if os.Geteuid() == 0 {
			must(os.Chown(d, int(acct.UID), int(acct.GID)))
		}
	}
}

// ensureConfig 確保 dataDir/config.yml 存在且正確。回傳 (路徑, 說明)。
func ensureConfig(o installOpts, acct *runas.Account, dataDir string, dryRun bool) (string, string) {
	path := filepath.Join(dataDir, "config.yml")
	if exists(path) && o.importConfig == "" {
		loadConfig(path)
		return path, "沿用 " + path
	}
	var text, how string
	switch {
	case o.importConfig != "":
		data, err := os.ReadFile(o.importConfig)
		if err != nil {
			fail("讀不到 %s：%v", o.importConfig, err)
		}
		text, how = string(data), "從 "+o.importConfig+" 匯入"
	case dryRun:
		return path, "會建立 " + path
	case stdinIsTerminal():
		var err error
		if text, err = wizard.Interactive(acct.Name); err != nil {
			fail("%v", err)
		}
		how = "設定精靈"
	default:
		text, err := wizard.Render([]wizard.Value{{Section: "synology", Key: "account", Val: acct.Name}})
		must(err)
		must(wizard.WriteConfig(path, text, int(acct.UID), int(acct.GID)))
		fail("已建立設定檔範本 %s：填好 DSM 密碼與模型服務後，再執行一次安裝：\n    %s", path, installCmd)
	}
	if dryRun {
		if _, err := config.Parse([]byte(text)); err != nil { // 實際檢查過，只是不寫入
			fail("config.yml 有誤：%v", err)
		}
		return path, fmt.Sprintf("會%s，寫到 %s", how, path)
	}
	must(wizard.WriteConfig(path, text, int(acct.UID), int(acct.GID)))
	loadConfig(path)
	return path, fmt.Sprintf("%s，寫到 %s", how, path)
}

// oldVersionLeftovers 是 0.x 版留下的東西（不影響運作，確認不需要後可以刪）。
func oldVersionLeftovers(st *state.State) []string {
	var found []string
	for _, p := range []string{filepath.Join(paths.Prefix, "ssectl.py"), filepath.Join(paths.Prefix, "systemd")} {
		if exists(p) {
			found = append(found, p)
		}
	}
	if st != nil && st.DataDir != "" {
		for _, n := range []string{"venv", "src", "python", "cache", "bin", "secrets", "config.toml"} {
			if p := filepath.Join(st.DataDir, n); exists(p) {
				found = append(found, p)
			}
		}
	}
	return found
}

// ── 安裝 ────────────────────────────────────────────────────────────────

// saved 是改動前的檔案（回復用）：不存在、一般檔案或 symlink。
type saved struct {
	path string
	kind int // 0 = 原本不存在，1 = 一般檔案，2 = symlink
	data []byte
	mode os.FileMode
	link string
}

func snapshot(path string) saved {
	s := saved{path: path}
	fi, err := os.Lstat(path)
	switch {
	case err != nil:
	case fi.Mode()&fs.ModeSymlink != 0:
		s.kind = 2
		s.link, err = os.Readlink(path)
		must(err)
	default:
		s.kind = 1
		s.mode = fi.Mode().Perm()
		s.data, err = os.ReadFile(path)
		must(err)
	}
	return s
}

func (s saved) restore() error {
	switch s.kind {
	case 1:
		return writeAtomic(s.path, s.data, s.mode, nil)
	case 2:
		return symlinkAtomic(s.link, s.path)
	}
	if err := os.Remove(s.path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

// symlinkAtomic 建立（或換掉）symlink：先建暫存的再改名蓋過去。
func symlinkAtomic(target, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".sse-tmp")
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

func install(o installOpts) int {
	defer locked()()
	st := loadState()
	fresh := st == nil || st.Version == ""
	ctx := preflight(o.probeURL, st)
	acct, dataDir := resolveOwner(o, st)
	ctx.user = acct.Name
	ok(fmt.Sprintf("照片擁有者：%s，資料目錄：%s", acct.Name, dataDir))
	if !o.dryRun {
		prepareDataDir(acct, dataDir)
	}
	cfgPath, how := ensureConfig(o, acct, dataDir, o.dryRun)
	ok("設定檔：" + how)
	var cfg *config.Config
	if exists(cfgPath) {
		sayf("\n檢查 DSM 帳號與模型服務（以 %s 的身分）：", acct.Name)
		code, out := runas.Run(acct.Name, engineCmd(cfgPath, "check"), 600*time.Second)
		printIndented(out, "  ")
		if code != 0 {
			fail("檢查沒通過（還沒改動系統）：修正 %s 後再執行一次安裝：\n    %s", cfgPath, installCmd)
		}
		cfg = loadConfig(cfgPath)
	}
	mode := "engine"
	if cfg != nil {
		mode = cfg.Filter.Mode
	}
	say("\n將會：")
	sayf("  - 安裝 filter 與服務（filter 以 %s 身分執行，索引與搜尋以 %s 身分執行）", ctx.nginxUser, acct.Name)
	if ctx.nginxKind == pristine {
		sayf("  - nginx upstream 改成 filter 為主、synoscgi 為 backup（原檔備份到 %s）", paths.NginxBackup)
	}
	if mode == "engine" {
		say("  - 模式：engine（AI 找到的照片加進原生搜尋結果）")
	} else {
		say("  - 模式：passthrough（只轉發）")
	}
	if o.dryRun {
		say("\n--dry-run：所有檢查都通過，沒有做任何變更。")
		return 0
	}
	verb := "升級"
	if fresh {
		verb = "安裝"
	}
	confirm(o.yes, fmt.Sprintf("\n確定要%s嗎？", verb))

	var written []saved
	nginxChanged := false
	var newDirs []string
	for _, d := range []string{paths.Etc, paths.LogDir} {
		if !exists(d) {
			newDirs = append(newDirs, d)
		}
	}
	// log 目錄給 filter（nginx worker 身分）寫；sse 自己以 root 寫 sse.log
	must(os.MkdirAll(paths.LogDir, 0o750))
	if os.Geteuid() == 0 {
		u, err := user.Lookup(ctx.nginxUser)
		must(err)
		g, err := user.LookupGroup(ctx.nginxGroup)
		must(err)
		uid, _ := strconv.Atoi(u.Uid)
		gid, _ := strconv.Atoi(g.Gid)
		must(os.Chown(paths.LogDir, uid, gid))
	}
	must(os.Chmod(paths.LogDir, 0o750))
	startAudit("install", cfg.Log.KeepDays)

	put := func(path string, data []byte, mode os.FileMode) {
		s := snapshot(path)
		must(writeAtomic(path, data, mode, nil))
		written = append(written, s)
	}
	sysctl := func(args ...string) {
		if code, out := systemctl(args...); code != 0 {
			fail("systemctl %s 失敗：%s", strings.Join(args, " "), out)
		}
	}

	sayf("\n%s中：", verb)
	if m := os.Getenv(deferRestartEnv); m != "" {
		os.WriteFile(m, nil, 0o644) // 告訴外層：系統已經動過，失敗時要重啟服務
	}
	var newState *state.State
	err := func() (err error) {
		defer func() {
			// 跟 Python 版的 except Exception 一樣：任何錯誤（包括沒料到的）都要回復
			if r := recover(); r != nil {
				if a, isAbort := r.(Abort); isAbort {
					err = errors.New(string(a))
				} else {
					err = fmt.Errorf("%v", r)
				}
			}
		}()
		// 先以純轉發模式接上 nginx，確認沒問題再開啟注入
		firstMode := "passthrough"
		if !fresh {
			if cur := readFilterStatus(); cur != nil && cur.Mode != "" {
				firstMode = cur.Mode
			}
		}
		put(paths.FilterConfig, renderFilterConfig(cfg, firstMode), 0o644)
		for _, unit := range allUnits {
			data, err := renderUnit(unit, ctx.nginxUser, ctx.nginxGroup, acct.Name, cfgPath)
			must(err)
			put(filepath.Join(paths.UnitDir, unit), data, 0o644)
		}
		s := snapshot(paths.BinLink)
		must(symlinkAtomic(paths.Binary, paths.BinLink))
		written = append(written, s)
		ok("檔案已就位")
		sysctl("daemon-reload")
		sysctl("enable", filterUnit)
		sysctl("restart", filterUnit)
		fst := waitFilterReady()
		ok(fmt.Sprintf("filter %s 已啟動", fst.Version))
		if ctx.nginxKind == pristine {
			written = append(written, snapshot(paths.NginxBackup))
		}
		nginxChanged = nginxApply(ctx.probeURL)
		if nginxChanged {
			ok("nginx 已改為 filter 為主、synoscgi 為 backup")
		} else {
			ok("nginx 設定原本就是套用狀態")
		}
		verifyTraffic(ctx.probeURL)
		ok("流量確認經過 filter，DSM 回應正常")
		if code, out := enableNow(guardTriggers...); code != 0 {
			fail("啟用 guard 失敗：%s", out)
		}
		ok("guard 已啟用（DSM 重新產生 nginx.conf 時自動重新套用）")
		sysctl(append([]string{"enable"}, engineUnits...)...)
		say("  … 重新啟動索引器與搜尋服務（舊的索引器要等手上的請求結束，最多約 20 秒）")
		sysctl(append([]string{"restart"}, engineUnits...)...)
		ping := waitEngine(cfg.Search.Socket)
		ok(fmt.Sprintf("索引器與搜尋服務已啟動（目前索引 %d 張）", pingItems(ping)))
		if e := connectErrorAs(cfg.Search.Socket, ctx.nginxUser); e != "" {
			fail("%s 連不上搜尋服務（%s）：%s", ctx.nginxUser, cfg.Search.Socket, e)
		}
		put(paths.FilterConfig, renderFilterConfig(cfg, cfg.Filter.Mode), 0o644)
		if code, out := systemctl("restart", filterUnit); code != 0 {
			fail("重啟 filter 失敗：%s", out)
		}
		fst = waitInjection(cfg.Filter.Mode)
		ok(fmt.Sprintf("模式：%s（%s）", fst.Mode, fst.InjectionReason))
		now := time.Now().Unix()
		newState = &state.State{}
		if st != nil {
			*newState = *st
		}
		if newState.InstalledAt == 0 {
			newState.InstalledAt = now
		}
		newState.Version, newState.UpdatedAt, newState.DSMBuild = version.Version, now, ctx.dsmBuild
		if o.channel != "" {
			newState.Channel = o.channel
		} else if newState.Channel == "" {
			newState.Channel = "release"
		}
		newState.Source = o.source
		if newState.Source == "" {
			newState.Source = "local"
		}
		newState.PhotosVersion, newState.NginxUser, newState.NginxGroup = ctx.photosVersion, ctx.nginxUser, ctx.nginxGroup
		newState.ProbeURL, newState.AutoApply, newState.User, newState.DataDir = ctx.probeURL, boolPtr(true), acct.Name, dataDir
		saveState(newState)
		return nil
	}()
	if err != nil {
		bad(err.Error())
		rollback(fresh, written, nginxChanged, ctx.probeURL, newDirs)
		return 1
	}
	// Python 版（與 0.x 版）的程式用不到了：服務都已經改成執行 /usr/local/sse/sse
	for _, n := range pythonLeftovers {
		os.RemoveAll(filepath.Join(paths.Prefix, n))
	}
	if left := oldVersionLeftovers(newState); len(left) > 0 {
		warn("舊版留下的檔案已經用不到，確認後可以刪除：" + strings.Join(left, "、"))
	}
	say("\n完成。索引器會在背景建立索引（照片多的話要幾個小時），做到哪、搜尋就加到哪。")
	say("  看進度：sudo sse status")
	sayf("  改設定：編輯 %s，再執行 sudo sse apply", cfgPath)
	return 0
}

func rollback(fresh bool, written []saved, nginxChanged bool, probeURL string, newDirs []string) {
	say("開始回復到安裝前的狀態：")
	type step struct {
		desc string
		fn   func() error
	}
	var steps []step
	ignore := func(f func()) func() error { return func() error { f(); return nil } }
	if fresh {
		steps = append(steps, step{"停用 guard", ignore(func() { disableNow(guardTriggers...) })})
	}
	if nginxChanged {
		steps = append(steps, step{"還原 nginx.conf", ignore(func() { nginxRevert() })})
	}
	if fresh {
		steps = append(steps, step{"停用服務", ignore(func() { disableNow(append([]string{filterUnit}, engineUnits...)...) })})
	}
	for i := len(written) - 1; i >= 0; i-- {
		s := written[i]
		desc := "還原 " + s.path
		if s.kind == 0 {
			desc = "移除 " + s.path
		}
		steps = append(steps, step{desc, s.restore})
	}
	for i := len(newDirs) - 1; i >= 0; i-- {
		d := newDirs[i]
		steps = append(steps, step{"移除目錄 " + d, func() error {
			if d == paths.LogDir {
				os.RemoveAll(d)
				return nil
			}
			if isDir(d) {
				return os.Remove(d)
			}
			return nil
		}})
	}
	steps = append(steps, step{"重新載入 systemd", ignore(func() { systemctl("daemon-reload") })})
	if !fresh && os.Getenv(deferRestartEnv) == "" {
		steps = append(steps, step{"用舊版重新啟動服務", ignore(func() {
			systemctl(append([]string{"restart", filterUnit}, engineUnits...)...)
		})})
	}
	for _, s := range steps {
		if err := runStep(s.fn); err != nil {
			bad(fmt.Sprintf("%s 失敗：%v", s.desc, err))
		} else {
			ok(s.desc)
		}
	}
	if probeURL != "" && probeOK(probeURL) {
		ok("回復完成，DSM 回應正常")
	} else {
		bad("回復後 DSM 健康檢查沒通過，請執行 sudo sse doctor，或照 README 的「手動還原」處理")
	}
}

// runStep 執行一個回復步驟；中止（Abort）也當成這一步失敗，繼續做下一步。
func runStep(fn func() error) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("%v", r)
		}
	}()
	return fn()
}
