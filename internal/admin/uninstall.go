package admin

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/admin/wizard"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/app"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
)

const uninstallHelp = `用法：sse uninstall [--purge] [--yes]

  --purge           連資料目錄（索引資料庫、config.yml）一起刪，不詢問
  --yes             不詢問直接執行（資料目錄保留，除非加 --purge）
  --probe-url URL   DSM 健康檢查網址
`

// wantPurge：解除安裝時要不要連資料目錄一起刪。預設保留：重新安裝就不必重建索引（照片多的話要好幾個小時）。
func wantPurge(purge, yes bool, dataDir string) bool {
	if purge {
		return true
	}
	if yes || dataDir == "" || !exists(dataDir) || !stdinIsTerminal() {
		return false
	}
	return wizard.AskYes(fmt.Sprintf("要連資料目錄 %s 一起刪除嗎？（索引資料庫、config.yml、log；保留的話重新安裝不必重建索引）",
		dataDir), false)
}

func cmdUninstall(_ app.Global, args []string) int {
	var purge, yes bool
	var probeURL string
	f := newFlags()
	f.Bool(&purge, "--purge")
	f.Bool(&yes, "--yes")
	f.String(&probeURL, "--probe-url")
	noArgs(f.parse(args, uninstallHelp))
	requireRoot()
	defer locked()()
	st := loadState()
	url := pickProbe(probeURL, st)
	dataDir := ""
	if st != nil {
		dataDir = st.DataDir
	}
	purge = wantPurge(purge, yes, dataDir)
	sayf("將會：停用並移除所有服務、把 nginx.conf 還原成原廠設定、移除 %s。", paths.Prefix)
	if dataDir != "" {
		what := "保留"
		if purge {
			what = "一併刪除"
		}
		sayf("資料目錄 %s（索引資料庫、config.yml）：%s", dataDir, what)
	}
	say("不會動：Synology Photos 的資料庫與照片。")
	confirm(yes, "確定要解除安裝嗎？")
	say("\n解除安裝中：")
	// 1. 先停 guard，免得它在還原後又把設定套回去
	disableNow(guardTriggers...)
	ok("guard 已停用")
	// 2. 還原 nginx；還原不了就停在這裡，不移除 filter
	data, err := os.ReadFile(paths.NginxConf)
	if err != nil {
		fail("讀不到 %s：%v", paths.NginxConf, err)
	}
	text := string(data)
	switch kind, why := nginxClassify(text); {
	case kind == managed:
		nginxRevert()
		ok("nginx.conf 已還原為原廠設定")
	case kind == pristine:
		ok("nginx.conf 已經是原廠設定")
	case strings.Contains(text, paths.FilterSock):
		bad(why)
		say("nginx.conf 仍指向 filter 但格式跟預期不同，為了安全，停在這裡不移除 filter。")
		say("請照 README 的「手動還原」處理 nginx.conf 後，再執行一次 uninstall。")
		return 1
	default:
		warn("nginx.conf 的 upstream 跟預期不同，但沒有指向本外掛，略過：" + why)
	}
	if strings.Contains(readNginxConf(), paths.FilterSock) {
		bad("nginx.conf 仍然指向 filter，停止解除安裝")
		return 1
	}
	// 3. 停服務、移除 systemd 設定
	disableNow(append([]string{filterUnit}, engineUnits...)...)
	for _, u := range allUnits {
		remove(filepath.Join(paths.UnitDir, u))
	}
	systemctl("daemon-reload")
	systemctl(append([]string{"reset-failed"}, allUnits...)...)
	ok("服務已移除")
	// 4. 移除設定、log、備份、程式（程式最後刪：這個指令本身就是 /usr/local/sse 裡的 sse；
	//    檔案刪掉後正在執行的程式照樣跑得完）
	for _, p := range []string{paths.State, paths.KillSwitch, paths.FilterConfig, paths.NginxBackup, paths.BinLink,
		paths.FilterSock, paths.StatusFile, paths.CaptureFile} {
		remove(p)
	}
	for _, d := range []string{paths.Etc, paths.RunDir} {
		os.Remove(d)
	}
	os.RemoveAll(paths.LogDir)
	if purge && dataDir != "" && exists(filepath.Join(dataDir, "config.yml")) {
		os.RemoveAll(dataDir)
		ok("資料目錄已刪除：" + dataDir)
	}
	os.RemoveAll(paths.Prefix)
	ok("程式與設定已移除")
	var left []string
	for _, u := range allUnits {
		if p := filepath.Join(paths.UnitDir, u); exists(p) {
			left = append(left, p)
		}
	}
	for _, p := range []string{paths.Prefix, paths.Etc, paths.NginxBackup, paths.BinLink, paths.FilterSock, paths.LogDir} {
		if exists(p) {
			left = append(left, p)
		}
	}
	if len(left) > 0 {
		warn("還有殘留：" + strings.Join(left, ", "))
	}
	if url != "" {
		if probeOK(url) {
			ok("DSM 健康檢查正常")
		} else {
			bad("DSM 健康檢查失敗")
		}
	}
	if dataDir != "" && !purge && exists(dataDir) {
		sayf("\n資料目錄保留在 %s；不需要的話：rm -rf %s", dataDir, dataDir)
	}
	if len(left) == 0 {
		say("\n解除安裝完成。")
	} else {
		say("\n解除安裝完成，但有殘留，見上方。")
	}
	say("搜尋已回到 Synology Photos 原本的結果：只找得到 Photos 自己認得的（物件分類、人物、地點、標籤），\n" +
		"照片裡的文字與 AI 相似搜尋找到的照片不會再出現。已經開著的 Photos 網頁請重新整理，App 請重新開啟。")
	if len(left) > 0 {
		return 1
	}
	return 0
}
