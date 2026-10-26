// sse：synology-photos-search-enhancement 唯一的執行檔。
//
//	sudo sse status                     整體狀態與索引進度
//	sudo sse update [--check]           更新到最新版
//	sudo sse apply [--check]            改完 config.yml 後套用
//	sudo sse doctor                     診斷並給建議
//	sudo sse logs [filter|engine|indexer|sse] [-n 50] [-f]
//	sse search <字> [--explain]         試搜尋、看分數
//	sse tag add|rm|list <標籤> [id…]     手動標籤
//	sse retry-failed | compact
//	sudo sse disable | enable           緊急開關：停止 / 恢復注入
//	sudo sse bypass on | off            nginx 直連 synoscgi（排查問題用）
//	sudo sse reapply                    DSM 更新後確認沒問題，重新套用
//	sudo sse capture on | show | off    錄下搜尋請求（排查用）
//	sudo sse uninstall [--purge]
//
// 由 systemd 執行的：sse filter（nginx 與 synoscgi 之間）、sse serve（搜尋服務）、sse index（索引器）、sse guard。
// 索引與搜尋的指令會以照片擁有者的身分執行：以 root 執行時自動切換，避免資料庫檔案變成 root 擁有。
package main

import (
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/admin"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/app"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/engine"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/filter"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/version"
)

const usageText = `synology-photos-search-enhancement %s

用法：sse [--config FILE] [-v] <指令> [選項]

  sudo sse status                     整體狀態與索引進度
  sudo sse update [--check]           更新到最新版
  sudo sse apply [--check]            改完 config.yml 後套用
  sudo sse doctor                     診斷並給建議
  sudo sse logs [filter|engine|indexer|sse] [-n 50] [-f]
  sse search <字> [--explain]         試搜尋、看分數
  sse tag add|rm|list <標籤> [id…]     手動標籤
  sse progress [--brief|--json]       索引進度
  sse check                           檢查 DSM 帳號、模型服務、資料庫
  sse retry-failed                    把放棄的照片重新排入
  sse compact                         壓實資料庫
  sudo sse disable | enable           緊急開關：停止 / 恢復注入
  sudo sse bypass on | off            nginx 直連 synoscgi（排查問題用）
  sudo sse reapply                    DSM 更新後確認沒問題，重新套用
  sudo sse capture on | show | off    錄下搜尋請求（排查用）
  sudo sse uninstall [--purge]        解除安裝

  --config FILE   config.yml 的路徑（預設：安裝時的資料目錄）
  -v              顯示除錯訊息
  --version       顯示版本
`

func usage(w io.Writer) { fmt.Fprintf(w, usageText, version.Version) }

func main() { os.Exit(run(os.Args[1:])) }

func run(argv []string) int {
	// DSM 的 /etc/profile 設了 umask 077：sudo 執行時建立的目錄會變成 700，filter（http）就讀不到設定
	syscall.Umask(0o022)
	g := app.Global{Argv: argv}
	i := 0
	for ; i < len(argv); i++ {
		a := argv[i]
		switch {
		case a == "--config":
			if i+1 >= len(argv) {
				fmt.Fprintln(os.Stderr, "錯誤：--config 要指定檔案")
				return 2
			}
			i++
			g.Config = argv[i]
		case strings.HasPrefix(a, "--config="):
			g.Config = strings.TrimPrefix(a, "--config=")
		case a == "-v" || a == "--verbose":
			g.Verbose = true
		case a == "--version":
			fmt.Println(version.Version)
			return 0
		case a == "-h" || a == "--help":
			usage(os.Stdout)
			return 0
		case strings.HasPrefix(a, "-"):
			fmt.Fprintf(os.Stderr, "錯誤：不認得的選項 %s\n\n", a)
			usage(os.Stderr)
			return 2
		default:
			goto command
		}
	}
command:
	if i >= len(argv) {
		usage(os.Stderr)
		return 2
	}
	cmd, rest := argv[i], argv[i+1:]
	switch {
	case cmd == "help":
		usage(os.Stdout)
		return 0
	case cmd == "filter":
		return filter.Main(rest)
	case cmd == "status" && os.Geteuid() != 0 && !paths.Sandbox:
		// 一般帳號看不到系統狀態（/run/sse 只有 root 與 nginx 能讀），只顯示索引進度
		return engine.Main(g, "progress", rest)
	case engine.Handles(cmd):
		return engine.Main(g, cmd, rest)
	case admin.Handles(cmd):
		return admin.Main(g, cmd, rest)
	}
	fmt.Fprintf(os.Stderr, "錯誤：不認得的指令 %s\n\n", cmd)
	usage(os.Stderr)
	return 2
}
