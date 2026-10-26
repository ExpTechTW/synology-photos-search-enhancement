// Package engine：索引與搜尋相關的指令（以照片擁有者的身分執行）。
//
//	sse check                         檢查 DSM 帳號、模型服務、資料庫
//	sse index [--once [--full]]       索引器（由 systemd 執行）
//	sse serve                         搜尋服務（由 systemd 執行）
//	sse progress [--brief|--json]     索引進度
//	sse search <字> [--explain] [--limit N]
//	sse tag add|rm|list [標籤] [id…]
//	sse retry-failed | compact
//
// 以 root 執行時自動切換成照片擁有者：資料庫檔案必須由他擁有，root 建立的 -wal、-shm 他會讀寫不了。
package engine

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/app"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/config"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/db"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/indexer"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/llm"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/logs"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/runas"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/search"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/server"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/state"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/synology"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/version"
)

var commands = map[string]bool{"check": true, "index": true, "serve": true, "progress": true, "search": true,
	"tag": true, "retry-failed": true, "compact": true}

// 常駐的指令寫到 log 檔（每天換檔，只留 log.keep_days 天內的）；其他指令照常輸出到終端機
var logFiles = map[string]string{"index": "indexer.log", "serve": "engine.log"}

// Handles：cmd 是不是這個套件的指令。
func Handles(cmd string) bool { return commands[cmd] }

// options 是指令的參數（選項可以放在位置參數前後）。
type options struct {
	flags map[string]bool
	limit int
	args  []string
}

func parse(cmd string, argv []string) (options, error) {
	known := map[string][]string{"index": {"--once", "--full"}, "progress": {"--brief", "--json"},
		"search": {"--explain"}}
	o := options{flags: map[string]bool{}, limit: 30}
	for i := 0; i < len(argv); i++ {
		a := argv[i]
		switch {
		case cmd == "search" && (a == "--limit" || strings.HasPrefix(a, "--limit=")):
			v := strings.TrimPrefix(a, "--limit=")
			if a == "--limit" {
				if i+1 >= len(argv) {
					return o, errors.New("--limit 要指定數字")
				}
				i++
				v = argv[i]
			}
			n, err := strconv.Atoi(v)
			if err != nil || n < 1 {
				return o, errors.New("--limit 要指定正整數")
			}
			o.limit = n
		case a == "--":
			o.args = append(o.args, argv[i+1:]...)
			return o, nil
		case strings.HasPrefix(a, "-") && len(a) > 1 && !isNumber(a):
			ok := false
			for _, f := range known[cmd] {
				if a == f {
					o.flags[a], ok = true, true
				}
			}
			if !ok {
				return o, fmt.Errorf("不認得的選項 %s", a)
			}
		default:
			o.args = append(o.args, a)
		}
	}
	return o, nil
}

func isNumber(s string) bool { _, err := strconv.Atoi(s); return err == nil }

// Main 執行索引與搜尋的指令。
func Main(g app.Global, cmd string, args []string) int {
	path := g.Config
	if path == "" {
		st, err := state.Load()
		if err != nil || st == nil || st.DataDir == "" {
			fmt.Fprintln(os.Stderr, "錯誤：還沒安裝，或用 --config 指定 config.yml")
			return 1
		}
		path = st.ConfigPath()
		// root 執行時切換成照片擁有者：資料庫檔案必須由他擁有，root 建立的 -wal、-shm 他會讀寫不了
		if os.Geteuid() == 0 && !paths.Sandbox && st.User != "" {
			self, err := os.Executable()
			if err != nil {
				self = paths.Binary
			}
			argv := []string{self, "--config", path}
			if g.Verbose {
				argv = append(argv, "-v")
			}
			return runas.Interactive(st.User, append(append(argv, cmd), args...))
		}
	}
	o, err := parse(cmd, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "錯誤：%v\n", err)
		return 2
	}
	level := logs.Info
	if g.Verbose {
		level = logs.Debug
	}
	logs.SetLevel(level)
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Fprintf(os.Stderr, "錯誤：%v\n", err)
		return 1
	}
	// SQLite 的暫存檔（例如壓實時的副本）放在資料庫旁邊：DSM 的 /tmp 是 tmpfs，會吃記憶體。
	// 要在第一次開資料庫之前設定，SQLite 只在初始化時讀一次
	if os.Getenv("SQLITE_TMPDIR") == "" {
		os.Setenv("SQLITE_TMPDIR", filepath.Dir(cfg.Index.DB))
	}
	os.MkdirAll(os.Getenv("SQLITE_TMPDIR"), 0o755)
	if name, ok := logFiles[cmd]; ok {
		if err := logs.Setup(filepath.Join(cfg.Log.Dir, name), cfg.Log.KeepDays, level); err != nil {
			logs.Get("sse").Errorf("%v", err)
		}
	}
	var code int
	switch cmd {
	case "check":
		code, err = cmdCheck(cfg)
	case "index":
		code, err = cmdIndex(cfg, o)
	case "serve":
		code, err = cmdServe(cfg)
	case "progress":
		code, err = cmdProgress(cfg, o)
	case "search":
		code, err = cmdSearch(cfg, o)
	case "tag":
		code, err = cmdTag(cfg, o)
	case "retry-failed":
		code, err = cmdRetryFailed(cfg)
	case "compact":
		code, err = cmdCompact(cfg)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "錯誤：%v\n", err)
		if code == 0 {
			code = 1
		}
	}
	return code
}

func makeDSM(cfg *config.Config) *synology.Client {
	c := cfg.Synology
	return synology.New(c.URL, c.Account, c.Password, c.VerifyTLS, time.Duration(c.Timeout)*time.Second)
}

// stopOnSignal：收到 SIGTERM / SIGINT 時關閉回傳的 channel。
func stopOnSignal() <-chan struct{} {
	stop := make(chan struct{})
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
	go func() {
		<-ch
		close(stop)
	}()
	return stop
}

// ── check ──

func cmdCheck(cfg *config.Config) (int, error) {
	ok := true
	step := func(name string, fn func() (string, error)) bool {
		msg, err := fn()
		if err != nil {
			ok = false
			fmt.Printf("  ✗ %s：%v\n", name, err)
			return false
		}
		if msg != "" {
			name += "：" + msg
		}
		fmt.Printf("  ✓ %s\n", name)
		return true
	}
	fmt.Printf("sse %s 檢查（%s）：\n", version.Version, cfg.Path)
	dsm := makeDSM(cfg)
	loggedIn := step(fmt.Sprintf("DSM 登入（%s，帳號 %s）", cfg.Synology.URL, cfg.Synology.Account), func() (string, error) {
		if err := dsm.Login(); err != nil {
			return "", err
		}
		uid, name, err := dsm.Me()
		if err != nil {
			return "", err
		}
		d, err := db.Open(cfg.Index.DB)
		if err != nil {
			return "", err
		}
		defer d.Close()
		if stored, has, _ := db.GetMeta(d, "owner_user_id"); has && stored != strconv.FormatInt(uid, 10) {
			return "", &indexer.OwnerMismatchError{Msg: fmt.Sprintf("資料庫屬於使用者 id %s，目前登入的是 %s（id %d）", stored, name, uid)}
		}
		return fmt.Sprintf("%s（id %d）", name, uid), nil
	})
	if loggedIn {
		step("讀取照片與縮圖", func() (string, error) {
			raw, err := dsm.ListItems(0, 5)
			if err != nil {
				return "", err
			}
			var items []synology.Item
			for _, r := range raw {
				if it, ok := synology.ParseItem(r); ok {
					items = append(items, it)
				}
			}
			if len(items) == 0 {
				return "還沒有照片，略過", nil
			}
			var sizes []string
			for _, size := range []string{cfg.Synology.ThumbSize, cfg.Synology.OCRThumbSize} {
				data, _, err := dsm.Thumbnail(items[0].UnitID, items[0].CacheKey, size)
				if err != nil {
					return "", err
				}
				sizes = append(sizes, fmt.Sprintf("%s %d KB", size, len(data)/1024))
			}
			n, err := dsm.CountItems()
			if err != nil {
				return "", err
			}
			return fmt.Sprintf("照片 %s 張，縮圖 %s", fmtN(n), strings.Join(sizes, "、")), nil
		})
		dsm.Logout()
	}
	client := llm.FromConfig(cfg)
	c := cfg.LLM
	step(fmt.Sprintf("文字 embedding（%s %s）", c.EmbedBaseURL, c.EmbedModel), func() (string, error) {
		v, err := client.EmbedQuery("測試")
		return fmt.Sprintf("%d 維", len(v)), err
	})
	step("圖片 embedding", func() (string, error) {
		v, err := client.EmbedImage(indexer.ProbePNG)
		return fmt.Sprintf("%d 維", len(v)), err
	})
	if cfg.Index.OCR {
		step(fmt.Sprintf("OCR（%s %s）", c.OCRBaseURL, c.OCRModel), func() (string, error) {
			text, err := client.OCR(indexer.ProbePNG, "image/png")
			r := []rune(text)
			if len(r) > 40 {
				r = r[:40]
			}
			return fmt.Sprintf("回應 %q", string(r)), err
		})
	}
	step("資料庫", func() (string, error) {
		d, err := db.Open(cfg.Index.DB)
		if err != nil {
			return "", err
		}
		defer d.Close()
		var n int
		if err := d.QueryRow(`SELECT count(*) FROM ocr_fts WHERE ocr_fts MATCH ?`, `"abc"`).Scan(&n); err != nil {
			return "", err
		}
		return cfg.Index.DB, nil
	})
	if ok {
		return 0, nil
	}
	return 1, nil
}

// ── index / serve ──

func cmdIndex(cfg *config.Config, o options) (int, error) {
	d, err := db.Open(cfg.Index.DB)
	if err != nil {
		return 1, err
	}
	defer d.Close()
	dsm := makeDSM(cfg)
	defer dsm.Logout()
	ix := indexer.New(cfg, dsm, llm.FromConfig(cfg), d)
	if o.flags["--once"] {
		_, hasFull, _ := db.GetMeta(d, "last_full_scan")
		if err := ix.RunOnce(o.flags["--full"] || !hasFull); err != nil {
			return ownerExit(err), err
		}
		st, err := db.GetStats(d)
		if err != nil {
			return 1, err
		}
		b, _ := json.Marshal(st)
		fmt.Println(string(b))
		return 0, nil
	}
	if err := ix.Loop(stopOnSignal()); err != nil {
		logs.Get("sse.indexer").Errorf("%v", err)
		return ownerExit(err), nil
	}
	return 0, nil
}

// ownerExit：資料庫屬於另一個帳號時結束碼是 2，systemd 不會重啟（RestartPreventExitStatus=2）。
func ownerExit(err error) int {
	var om *indexer.OwnerMismatchError
	if errors.As(err, &om) {
		return 2
	}
	return 1
}

func cmdServe(cfg *config.Config) (int, error) {
	if err := server.New(cfg, llm.FromConfig(cfg)).Serve(stopOnSignal()); err != nil {
		return 1, err
	}
	return 0, nil
}

// ── tag / retry-failed / compact ──

func cmdTag(cfg *config.Config, o options) (int, error) {
	if len(o.args) == 0 || (o.args[0] != "add" && o.args[0] != "rm" && o.args[0] != "list") {
		return 2, errors.New("用法：sse tag add|rm|list [標籤] [照片 id…]")
	}
	action := o.args[0]
	tag := ""
	if len(o.args) > 1 {
		tag = search.Casefold(o.args[1])
	}
	var ids []int64
	for _, s := range o.args[min(2, len(o.args)):] {
		id, err := strconv.ParseInt(s, 10, 64)
		if err != nil {
			return 2, fmt.Errorf("照片 id 要是數字：%s", s)
		}
		ids = append(ids, id)
	}
	d, err := db.Open(cfg.Index.DB)
	if err != nil {
		return 1, err
	}
	defer d.Close()
	if action == "list" {
		q, args := "SELECT tag, unit_id FROM tag ORDER BY tag, unit_id", []any{}
		if tag != "" {
			q, args = "SELECT tag, unit_id FROM tag WHERE tag=? ORDER BY tag, unit_id", []any{tag}
		}
		rows, err := d.Query(q, args...)
		if err != nil {
			return 1, err
		}
		defer rows.Close()
		n := 0
		for rows.Next() {
			var t string
			var uid int64
			rows.Scan(&t, &uid)
			fmt.Printf("%s\t%d\n", t, uid)
			n++
		}
		if n == 0 {
			fmt.Println("沒有標籤")
		}
		return 0, nil
	}
	if tag == "" {
		return 1, errors.New("要指定標籤")
	}
	if action == "add" {
		if len(ids) == 0 {
			return 1, errors.New("要指定照片 id（sse search 會列出照片 id）")
		}
		var unknown []string
		for _, id := range ids {
			var n int
			d.QueryRow("SELECT count(*) FROM item WHERE deleted=0 AND unit_id=?", id).Scan(&n)
			if n == 0 {
				unknown = append(unknown, strconv.FormatInt(id, 10))
			}
		}
		if len(unknown) > 0 {
			return 1, fmt.Errorf("索引裡沒有這些照片（id 打錯，或還沒掃描到）：%s", strings.Join(unknown, ", "))
		}
		err := db.Tx(d, func(tx *sql.Tx) error {
			for _, id := range ids {
				if _, err := tx.Exec("INSERT OR IGNORE INTO tag(tag, unit_id) VALUES (?,?)", tag, id); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return 1, err
		}
		fmt.Printf("已標上「%s」：%s\n", tag, joinIDs(ids))
		return 0, nil
	}
	var removed int64
	err = db.Tx(d, func(tx *sql.Tx) error {
		if len(ids) == 0 {
			r, err := tx.Exec("DELETE FROM tag WHERE tag=?", tag)
			if err == nil {
				removed, _ = r.RowsAffected()
			}
			return err
		}
		for _, id := range ids {
			r, err := tx.Exec("DELETE FROM tag WHERE tag=? AND unit_id=?", tag, id)
			if err != nil {
				return err
			}
			n, _ := r.RowsAffected()
			removed += n
		}
		return nil
	})
	if err != nil {
		return 1, err
	}
	fmt.Printf("已移除 %d 筆「%s」\n", removed, tag)
	return 0, nil
}

func joinIDs(ids []int64) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = strconv.FormatInt(id, 10)
	}
	return strings.Join(s, ", ")
}

func cmdRetryFailed(cfg *config.Config) (int, error) {
	d, err := db.Open(cfg.Index.DB)
	if err != nil {
		return 1, err
	}
	defer d.Close()
	a, b, err := db.RequeueFailed(d)
	if err != nil {
		return 1, err
	}
	fmt.Printf("重新排入：embedding %d、OCR %d\n", a, b)
	return 0, nil
}

// cmdCompact：手動壓實（索引器在跑也可以用：WAL 模式下讀取不受影響，寫入會等壓實做完）。
func cmdCompact(cfg *config.Config) (int, error) {
	d, err := db.Open(cfg.Index.DB)
	if err != nil {
		return 1, err
	}
	defer d.Close()
	size, wasted, err := db.SpaceStats(d)
	if err != nil {
		return 1, err
	}
	fmt.Printf("主檔 %.1f MB，可回收約 %.1f MB\n", float64(size)/1e6, float64(wasted)/1e6)
	before, after, done, err := indexer.CompactIfWorthwhile(d, cfg.Index.DB, true, nil)
	if err != nil {
		return 1, err
	}
	if done {
		fmt.Printf("硬碟用量：%.1f MB → %.1f MB\n", float64(before)/1e6, float64(after)/1e6)
	}
	return 0, nil
}

// ── search ──

func cmdSearch(cfg *config.Config, o options) (int, error) {
	if len(o.args) != 1 {
		return 2, errors.New("用法：sse search <字> [--explain] [--limit N]")
	}
	query := o.args[0]
	if _, err := os.Stat(cfg.Index.DB); err != nil {
		fmt.Println("還沒有索引資料庫：索引器還沒跑過")
		return 1, nil
	}
	idx := search.NewIndex(cfg.Index.DB, cfg.Search.Dims)
	defer idx.Close()
	if _, err := idx.Refresh(); err != nil {
		return 1, err
	}
	client := llm.FromConfig(cfg)
	bank := make([][]float64, 0, len(search.HubBank))
	for _, q := range search.HubBank {
		v, err := client.EmbedQuery(q)
		if err != nil {
			return 1, err
		}
		bank = append(bank, v)
	}
	idx.SetBank(bank)
	t := time.Now()
	qv, err := client.EmbedQuery(query)
	if err != nil {
		return 1, err
	}
	hits := idx.Search(qv, query, cfg.Search, "all")
	fmt.Printf("「%s」：%d 張（%d ms，索引 %d 張）\n", query, len(hits), time.Since(t).Milliseconds(), idx.Items())
	labels := map[string]string{"vec": "圖片", "text": "照片文字", "ocr": "文字逐字", "tag": "標籤"}
	for i, h := range hits {
		if i >= o.limit {
			break
		}
		fmt.Printf("  %-10d %s  %-6s %.3f %s\n", h.ID, time.Unix(h.Time, 0).UTC().Format("2006-01-02"), h.Type, h.Score, labels[h.Src])
	}
	if !o.flags["--explain"] {
		return 0, nil
	}
	sc := cfg.Search
	top, mean, std := idx.Explain(qv, 20, false)
	fmt.Printf("\n圖片：分數最高的 %d 張（hub 校正後；平均 %.3f、標準差 %.3f；門檻 min_score=%.2f max_drop=%.2f）：\n",
		len(top), mean, std, sc.MinScore, sc.MaxDrop)
	for _, s := range top {
		fmt.Printf("  %-10d %.3f\n", s.ID, s.Score)
	}
	top, mean, std = idx.Explain(qv, 20, true)
	if len(top) == 0 {
		return 0, nil
	}
	texts := map[int64]string{}
	if c, err := idx.DB(); err == nil {
		ids := make([]string, len(top))
		for i, s := range top {
			ids[i] = strconv.FormatInt(s.ID, 10)
		}
		if rows, err := c.Query("SELECT unit_id, text FROM ocr WHERE unit_id IN (" + strings.Join(ids, ",") + ")"); err == nil {
			for rows.Next() {
				var id int64
				var text string
				rows.Scan(&id, &text)
				texts[id] = text
			}
			rows.Close()
		}
	}
	fmt.Printf("\n照片裡的文字：分數最高的 %d 張（平均 %.3f、標準差 %.3f；門檻 text_min_score=%.2f）：\n",
		len(top), mean, std, sc.TextMinScore)
	for _, s := range top {
		text := []rune(strings.ReplaceAll(texts[s.ID], "\n", " / "))
		if len(text) > 60 {
			text = text[:60]
		}
		fmt.Printf("  %-10d %.3f  %s\n", s.ID, s.Score, string(text))
	}
	return 0, nil
}
