package indexer

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// Progress 是 indexer-status.json（放在資料庫旁邊）：目前在做什麼、處理速度、暫停原因，給 progress 指令看。
// 處理量的總數在資料庫裡；這裡只記資料庫裡沒有的即時狀態。欄位名稱跟 Python 版相同。
type Progress struct {
	Path string

	mu        sync.Mutex
	data      map[string]any
	processed map[string]int64
	samples   map[string][]sample
	lastWrite time.Time
	now       func() time.Time
}

type sample struct {
	t     time.Time
	total int64
}

const (
	rateWindow  = 600 * time.Second // 用最近 10 分鐘的處理量算速度
	rateMinSpan = 60 * time.Second  // 至少累積 1 分鐘才算速度
)

var columns = []string{"emb", "ocr", "txt"}

// NewProgress 建立進度檔（第一次寫入時才會產生檔案）。
func NewProgress(path string) *Progress {
	now := time.Now().Unix()
	return &Progress{
		Path: path,
		data: map[string]any{"pid": os.Getpid(), "started": now, "phase": "starting", "phase_since": now,
			"paused_until": nil, "pause_reason": nil, "scan_done": nil, "scan_total": nil,
			"next_full_scan": nil, "next_quick_scan": nil},
		processed: map[string]int64{"emb": 0, "ocr": 0, "txt": 0},
		samples:   map[string][]sample{},
		now:       time.Now,
	}
}

// Set 切換階段（並馬上寫檔）。kv 是要一起更新的欄位。
func (p *Progress) Set(phase string, kv map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if phase != p.data["phase"] {
		p.data["phase"], p.data["phase_since"] = phase, p.now().Unix()
	}
	if phase != "paused" {
		p.data["paused_until"], p.data["pause_reason"] = nil, nil
	}
	if phase != "full_scan" {
		p.data["scan_done"], p.data["scan_total"] = nil, nil
	}
	for k, v := range kv {
		p.data[k] = v
	}
	p.write(true)
}

// Update 更新欄位，不切換階段（最多 5 秒寫一次檔）。
func (p *Progress) Update(kv map[string]any) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for k, v := range kv {
		p.data[k] = v
	}
	p.write(false)
}

// Add 記下處理了 n 張（用來算速度）。
func (p *Progress) Add(column string, n int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if n > 0 {
		p.processed[column] += int64(n)
		p.samples[column] = append(p.samples[column], sample{p.now(), p.processed[column]})
	}
	p.write(false)
}

// Rate 是最近 10 分鐘的處理速度（張/秒）；資料不夠時回傳 nil。
func (p *Progress) Rate(column string, now time.Time) *float64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.rate(column, now)
}

func (p *Progress) rate(column string, now time.Time) *float64 {
	q := p.samples[column]
	for len(q) > 1 && q[0].t.Before(now.Add(-rateWindow)) {
		q = q[1:]
	}
	p.samples[column] = q
	if len(q) < 2 || now.Sub(q[0].t) < rateMinSpan {
		return nil
	}
	// 分母用「現在」而不是最後一筆：停下來時速度會跟著往下掉，不會一直顯示舊的速度
	r := float64(q[len(q)-1].total-q[0].total) / now.Sub(q[0].t).Seconds()
	return &r
}

// Data 回傳目前的內容（測試用）。
func (p *Progress) Data() map[string]any {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := map[string]any{}
	for k, v := range p.data {
		out[k] = v
	}
	return out
}

func (p *Progress) write(force bool) {
	now := p.now()
	if !force && now.Sub(p.lastWrite) < 5*time.Second {
		return
	}
	p.lastWrite = now
	out := map[string]any{}
	for k, v := range p.data {
		out[k] = v
	}
	processed, rate := map[string]int64{}, map[string]any{}
	for _, c := range columns {
		processed[c] = p.processed[c]
		if r := p.rate(c, now); r != nil {
			rate[c] = *r
		} else {
			rate[c] = nil
		}
	}
	out["processed"], out["rate"], out["updated"] = processed, rate, now.Unix()
	data, err := json.Marshal(out)
	if err != nil {
		return
	}
	tmp := p.Path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		log.Warnf("寫不了進度檔 %s：%v", p.Path, err)
		return
	}
	if err := os.Rename(tmp, p.Path); err != nil {
		log.Warnf("寫不了進度檔 %s：%v", p.Path, err)
	}
}
