package engine

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/config"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/db"
)

// ── 格式 ──

// fmtN：千分位，例如 19,754。
func fmtN(n int64) string {
	s := strconv.FormatInt(n, 10)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteByte(',')
		}
		b.WriteRune(c)
	}
	if neg {
		return "-" + b.String()
	}
	return b.String()
}

func fmtDur(sec float64) string {
	s := int64(sec)
	if s < 60 {
		return "不到 1 分鐘"
	}
	if s < 3600 {
		return fmt.Sprintf("%d 分鐘", int64(math.RoundToEven(float64(s)/60)))
	}
	mins := int64(math.RoundToEven(float64(s) / 60))
	h, m := mins/60, mins%60
	if h < 48 {
		return fmt.Sprintf("%d 小時 %d 分", h, m)
	}
	return fmt.Sprintf("%.1f 天", float64(s)/86400)
}

func fmtTime(ts int64) string {
	if ts == 0 {
		return "—"
	}
	t := time.Unix(ts, 0)
	now := time.Now()
	if t.Year() == now.Year() && t.YearDay() == now.YearDay() {
		return t.Format("15:04")
	}
	return t.Format("01-02 15:04")
}

func bar(frac float64) string {
	const width = 20
	n := int(math.RoundToEven(frac * width))
	n = max(0, min(width, n))
	return strings.Repeat("█", n) + strings.Repeat("░", width-n)
}

// pyFloat：跟 Python 的 str(float) 一樣至少留一位小數（80.0、31.1）。
func pyFloat(f float64) string {
	s := strconv.FormatFloat(f, 'f', -1, 64)
	if !strings.Contains(s, ".") {
		s += ".0"
	}
	return s
}

// ── 索引進度 ──

// column 是 embedding 或 OCR 的進度。
type column struct {
	Done       int64    `json:"done"`
	Pending    int64    `json:"pending"`
	Failed     int64    `json:"failed"`
	Total      int64    `json:"total"`
	Percent    float64  `json:"percent"`
	Rate       *float64 `json:"rate"`
	ETASeconds *int64   `json:"eta_seconds"`
}

// report 是 progress --json 的內容（欄位名稱跟 Python 版相同）。
type report struct {
	db.Stats
	DBBytes            int64          `json:"db_bytes"`
	LastCompact        *int64         `json:"last_compact"`
	LastFullScan       *int64         `json:"last_full_scan"`
	LastQuickScan      *int64         `json:"last_quick_scan"`
	LastFullScanResult map[string]any `json:"last_full_scan_result"`
	OCREnabled         bool           `json:"ocr_enabled"`
	Indexer            map[string]any `json:"indexer"`
	IndexerRunning     bool           `json:"indexer_running"`
	Embedding          column         `json:"embedding"`
	OCR                column         `json:"ocr"`
}

func optTime(q db.Q, k string) *int64 {
	if v := db.MetaInt(q, k, 0); v != 0 {
		return &v
	}
	return nil
}

// indexerAlive：進度檔裡的 pid 是不是還在跑的索引器（避免 pid 被重複使用時誤判）。
func indexerAlive(prog map[string]any) bool {
	pidF, _ := prog["pid"].(float64)
	pid := int(pidF)
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil && err != syscall.EPERM {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return true // 沒有 /proc（macOS 測試）
	}
	args := strings.Split(strings.TrimRight(string(data), "\x00"), "\x00")
	if len(args) == 0 || !strings.HasPrefix(filepath.Base(args[0]), "sse") {
		return false
	}
	for _, a := range args[1:] {
		if a == "index" {
			return true
		}
	}
	return false
}

func progressReport(cfg *config.Config) (*report, error) {
	conn, err := db.OpenReadOnly(cfg.Index.DB)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	st, err := db.GetStats(conn)
	if err != nil {
		return nil, err
	}
	r := &report{Stats: st, DBBytes: db.FileBytes(cfg.Index.DB), OCREnabled: cfg.Index.OCR,
		LastCompact: optTime(conn, "last_compact"), LastFullScan: optTime(conn, "last_full_scan"),
		LastQuickScan: optTime(conn, "last_quick_scan")}
	if v, ok, _ := db.GetMeta(conn, "last_full_scan_result"); ok {
		json.Unmarshal([]byte(v), &r.LastFullScanResult)
	}
	if data, err := os.ReadFile(filepath.Join(filepath.Dir(cfg.Index.DB), "indexer-status.json")); err == nil {
		json.Unmarshal(data, &r.Indexer)
	}
	r.IndexerRunning = r.Indexer != nil && indexerAlive(r.Indexer)
	col := func(done, pending, failed int64, key string) column {
		c := column{Done: done, Pending: pending, Failed: failed, Total: st.Items, Percent: 100}
		if st.Items > 0 {
			c.Percent = math.Round(1000*float64(done+failed)/float64(st.Items)) / 10
		}
		if r.IndexerRunning {
			if rates, ok := r.Indexer["rate"].(map[string]any); ok {
				if v, ok := rates[key].(float64); ok {
					c.Rate = &v
				}
			}
		}
		if c.Rate != nil && *c.Rate > 0 && pending > 0 {
			eta := int64(float64(pending) / *c.Rate)
			c.ETASeconds = &eta
		}
		return c
	}
	r.Embedding = col(st.Embedded, st.EmbedPending, st.EmbedFailed, "emb")
	r.OCR = col(st.OCRDone, st.OCRPending, st.OCRFailed, "ocr")
	return r, nil
}

var phases = map[string]string{"starting": "啟動中", "full_scan": "完整掃描", "quick_scan": "快速掃描",
	"embedding": "embedding", "ocr": "OCR", "text_embedding": "照片文字向量", "idle": "全部做完，等待新照片",
	"compacting": "壓實資料庫", "paused": "暫停", "stopped": "已停止", "done": "完成", "error": "錯誤"}

func num(m map[string]any, k string) int64 {
	f, _ := m[k].(float64)
	return int64(f)
}

func describeState(r *report) string {
	prog := r.Indexer
	if prog == nil {
		prog = map[string]any{}
	}
	if !r.IndexerRunning {
		if prog["phase"] == "error" {
			return fmt.Sprintf("沒有在跑：%v", prog["pause_reason"])
		}
		return "沒有在跑"
	}
	phase, _ := prog["phase"].(string)
	if phase == "paused" {
		return fmt.Sprintf("暫停中，%s 再試（%v）", fmtTime(num(prog, "paused_until")), prog["pause_reason"])
	}
	if phase == "full_scan" && num(prog, "scan_total") > 0 {
		return fmt.Sprintf("完整掃描中 %s / %s", fmtN(num(prog, "scan_done")), fmtN(num(prog, "scan_total")))
	}
	name := phases[phase]
	if name == "" {
		name = phase
	}
	return fmt.Sprintf("%s（%s 開始）", name, fmtTime(num(prog, "phase_since")))
}

func describeColumn(c column, waiting string) string {
	if c.Pending == 0 {
		return "完成"
	}
	if waiting != "" {
		return waiting
	}
	if c.ETASeconds != nil && *c.ETASeconds > 0 {
		eta := fmtDur(float64(*c.ETASeconds))
		if !strings.HasPrefix(eta, "不到") {
			eta = "約 " + eta
		}
		return fmt.Sprintf("%.1f 張/秒，%s", *c.Rate, eta)
	}
	return "計算速度中"
}

func cmdProgress(cfg *config.Config, o options) (int, error) {
	if _, err := os.Stat(cfg.Index.DB); err != nil {
		fmt.Println("還沒有索引資料庫：索引器還沒跑過")
		return 0, nil
	}
	r, err := progressReport(cfg)
	if err != nil {
		return 1, err
	}
	if o.flags["--json"] {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		enc.Encode(r)
		os.Stdout.Write(buf.Bytes())
		return 0, nil
	}
	e, oc := r.Embedding, r.OCR
	ocrWait := ""
	if e.Pending > 0 {
		ocrWait = "embedding 做完後開始"
	}
	if o.flags["--brief"] {
		line := fmt.Sprintf("embedding %s / %s（%s%%，%s）", fmtN(e.Done), fmtN(e.Total), pyFloat(e.Percent), describeColumn(e, ""))
		if r.OCREnabled {
			line += fmt.Sprintf("；OCR %s / %s（%s%%，%s）", fmtN(oc.Done), fmtN(oc.Total), pyFloat(oc.Percent),
				describeColumn(oc, ocrWait))
		}
		fmt.Println(line)
		fmt.Println("索引器：" + describeState(r))
		return 0, nil
	}
	prog := r.Indexer
	if prog == nil {
		prog = map[string]any{}
	}
	deleted := ""
	if r.Deleted > 0 {
		deleted = fmt.Sprintf("（另有 %s 張已刪除）", fmtN(r.Deleted))
	}
	fmt.Printf("索引進度：%s 張照片%s\n", fmtN(r.Items), deleted)
	fmt.Printf("  索引器    %s\n", describeState(r))
	fmt.Printf("  embedding %s %6s / %s（%5.1f%%）  %s\n", bar(e.Percent/100), fmtN(e.Done), fmtN(e.Total), e.Percent,
		describeColumn(e, ""))
	if r.OCREnabled {
		fmt.Printf("  OCR       %s %6s / %s（%5.1f%%）  %s\n", bar(oc.Percent/100), fmtN(oc.Done), fmtN(oc.Total), oc.Percent,
			describeColumn(oc, ocrWait))
		tv, wt := r.TextEmbedded, r.WithText
		more := ""
		if tv < wt {
			more = "（做完一批 OCR 就跟著做）"
		}
		fmt.Printf("  照片裡有文字：%s 張，文字向量 %s 張%s\n", fmtN(wt), fmtN(tv), more)
	} else {
		fmt.Println("  OCR       關閉")
	}
	if e.Failed > 0 || oc.Failed > 0 {
		fmt.Printf("  失敗：embedding %d、OCR %d（各試 %d 次都沒成功；sse retry-failed 重新排入）\n",
			e.Failed, oc.Failed, cfg.Index.MaxTries)
	}
	last := ""
	if res := r.LastFullScanResult; res != nil {
		secs, _ := res["seconds"].(float64)
		last = fmt.Sprintf("（%s 張、新增或變更 %s、刪除 %s，花 %s）", fmtN(num(res, "seen")), fmtN(num(res, "changed")),
			fmtN(num(res, "removed")), fmtDur(secs))
	}
	nextFull, nextQuick := "—", "—"
	if r.IndexerRunning {
		nextFull, nextQuick = fmtTime(num(prog, "next_full_scan")), fmtTime(num(prog, "next_quick_scan"))
	}
	fmt.Printf("  完整掃描  上次 %s%s，下次約 %s\n", fmtTime(deref(r.LastFullScan)), last, nextFull)
	fmt.Printf("  快速掃描  上次 %s，下次約 %s\n", fmtTime(deref(r.LastQuickScan)), nextQuick)
	fmt.Printf("  資料庫    %.1f MB（%s；上次壓實 %s）\n", float64(r.DBBytes)/1e6, cfg.Index.DB, fmtTime(deref(r.LastCompact)))
	return 0, nil
}

func deref(p *int64) int64 {
	if p == nil {
		return 0
	}
	return *p
}
