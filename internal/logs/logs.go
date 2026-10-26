// Package logs：log 檔每天午夜換新檔（舊的改名成 <檔名>.YYYY-MM-DD），只留 keep_days 天內的紀錄。
//
// 沒有呼叫 Setup 之前寫到 stderr（systemd 會收進 journal）。
package logs

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Level 是 log 的等級。
type Level int

const (
	Debug Level = iota
	Info
	Warning
	Error
)

func (l Level) String() string {
	return [...]string{"DEBUG", "INFO", "WARNING", "ERROR"}[l]
}

var (
	mu       sync.Mutex
	out      io.Writer = os.Stderr
	minLevel           = Info
	stamped            = false // 寫到檔案時每行前面加時間（journal 自己有時間）
)

// Logger 是一個有名字的 log（例如 sse.indexer）。
type Logger struct{ name string }

// Get 取得名字為 name 的 log。
func Get(name string) *Logger { return &Logger{name} }

func (l *Logger) Debugf(format string, a ...any) { l.log(Debug, format, a...) }
func (l *Logger) Infof(format string, a ...any)  { l.log(Info, format, a...) }
func (l *Logger) Warnf(format string, a ...any)  { l.log(Warning, format, a...) }
func (l *Logger) Errorf(format string, a ...any) { l.log(Error, format, a...) }

func (l *Logger) log(level Level, format string, a ...any) {
	mu.Lock()
	defer mu.Unlock()
	if level < minLevel {
		return
	}
	msg := fmt.Sprintf(format, a...)
	line := level.String() + " " + l.name + " " + msg + "\n"
	if stamped {
		line = Stamp(time.Now()) + " " + line
	}
	io.WriteString(out, line)
}

// SetLevel 設定最低的 log 等級。
func SetLevel(level Level) {
	mu.Lock()
	minLevel = level
	mu.Unlock()
}

// Setup 把 log 改寫到 path（每天換檔、只留 keepDays 天）。失敗時維持寫到 stderr。
func Setup(path string, keepDays int, level Level) error {
	w, err := OpenDaily(path, keepDays)
	if err != nil {
		return fmt.Errorf("沒辦法寫 log 檔 %s：%v", path, err)
	}
	mu.Lock()
	out, stamped, minLevel = w, true, level
	mu.Unlock()
	return nil
}

// Stamp 是 log 每行開頭的時間（跟 Python logging 相同的格式）。
func Stamp(t time.Time) string {
	return t.Format("2006-01-02 15:04:05") + fmt.Sprintf(",%03d", t.Nanosecond()/1e6)
}

// DailyWriter 寫到 path，每天午夜把舊的改名成 path.YYYY-MM-DD，並刪掉 keepDays 天以前的。
type DailyWriter struct {
	mu       sync.Mutex
	path     string
	keepDays int
	f        *os.File
	day      string // 目前這個檔案是哪一天的
	now      func() time.Time
}

// OpenDaily 開啟（或接著寫）每天換檔的 log。
func OpenDaily(path string, keepDays int) (*DailyWriter, error) {
	w := &DailyWriter{path: path, keepDays: keepDays, now: time.Now}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	if err := w.open(); err != nil {
		return nil, err
	}
	Prune(path, keepDays, w.now())
	return w, nil
}

func (w *DailyWriter) open() error {
	f, err := os.OpenFile(w.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	w.f = f
	// 接著寫既有的檔案時，以它最後修改的那天為準：程式停了幾天才重啟，第一次寫入就會先換檔
	w.day = day(w.now())
	if st, err := f.Stat(); err == nil && st.Size() > 0 {
		w.day = day(st.ModTime())
	}
	return nil
}

// Write 實作 io.Writer。
func (w *DailyWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if today := day(w.now()); today != w.day {
		w.rotate()
	}
	return w.f.Write(p)
}

func (w *DailyWriter) rotate() {
	w.f.Close()
	target := w.path + "." + w.day
	if _, err := os.Stat(target); err == nil {
		// 同一天的檔案已經存在（例如時間被調回去）：接在後面
		if old, err := os.ReadFile(w.path); err == nil {
			if f, err := os.OpenFile(target, os.O_WRONLY|os.O_APPEND, 0o644); err == nil {
				f.Write(old)
				f.Close()
				os.Remove(w.path)
			}
		}
	} else {
		os.Rename(w.path, target)
	}
	if err := w.open(); err != nil {
		w.f, _ = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
	}
	w.day = day(w.now())
	Prune(w.path, w.keepDays, w.now())
}

// Close 關閉檔案。
func (w *DailyWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.f.Close()
}

func day(t time.Time) string { return t.Format("2006-01-02") }

// Prune 刪掉 keepDays 天以前的舊檔（檔名 <path>.YYYY-MM-DD）：只留今天與前 keepDays - 1 天。
// 程式停了好幾天才重啟時不會換檔，所以啟動時也要自己清一次。
func Prune(path string, keepDays int, today time.Time) {
	dir, base := filepath.Split(path)
	if dir == "" {
		dir = "."
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	y, m, d := today.Date()
	oldest := time.Date(y, m, d, 0, 0, 0, 0, time.Local).AddDate(0, 0, -(keepDays - 1))
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, base+".") {
			continue
		}
		p := filepath.Join(dir, name)
		var old bool
		if t, err := time.ParseInLocation("2006-01-02", name[len(base)+1:], time.Local); err == nil {
			old = t.Before(oldest)
		} else if info, err := e.Info(); err == nil {
			old = info.ModTime().Before(today.Add(-time.Duration(keepDays) * 24 * time.Hour))
		} else {
			continue
		}
		if old {
			os.Remove(p)
		}
	}
}
