// Package filter：SCGI filter，坐在 nginx 與 synoscgi 之間（sse filter，由 systemd 以 nginx 的帳號執行）。
//
//	nginx ──scgi──▶ /run/sse/filter.sock（本程式）──▶ /run/synoscgi.sock
//
// 安全原則：
//  1. request 一個 byte 都不改，邊收邊轉，不整包緩衝（上傳、下載不受影響）。
//  2. 只有 SYNO.Foto.Search.Search 的回應可能被改寫，且有大小與時間上限；超過就原樣轉發。
//  3. 任何解析或注入錯誤都原樣轉發（fail-open）。
//  4. 本程式整個掛掉或沒啟動時，nginx 的 backup upstream 直接連 synoscgi（由 sse install 設定）。
//  5. 不連任何資料庫、不寫任何 Synology 檔案；log 不記 cookie、request body 或搜尋關鍵字。
//  6. engine 模式下，要注入的照片一律先用「搜尋者自己的 session」向 DSM 取一次（Browse.Item.get）：
//     DSM 沒回的就不注入，所以權限完全由 DSM 判斷，其他帳號搜尋不會看到別人的照片。
package filter

import (
	"flag"
	"fmt"
	"io"
	"os"
)

// Main 是 sse filter 的進入點。
func Main(args []string) int {
	fs := flag.NewFlagSet("sse filter", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", "", "設定檔（JSON）")
	if err := fs.Parse(args); err != nil {
		if err == flag.ErrHelp {
			fmt.Println("用法：sse filter [--config FILE]")
			return 0
		}
		fmt.Fprintf(os.Stderr, "錯誤：%v\n用法：sse filter [--config FILE]\n", err)
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(os.Stderr, "錯誤：不認得的參數 %s\n用法：sse filter [--config FILE]\n", fs.Arg(0))
		return 2
	}
	cfg, cfgErr, warnings := loadConfig(*cfgPath)
	if cfg.LogFile != "" {
		if err := lg.setupFile(cfg.LogFile, cfg.LogKeepDays); err != nil {
			lg.error("沒辦法寫 log 檔 %s，改寫到 stderr：%s", cfg.LogFile, err)
		}
	}
	for _, w := range warnings {
		lg.warning("%s", w)
	}
	return newFilter(cfg, cfgErr).serve()
}
