package filter

import (
	"fmt"
	"io"
	"os"
	"sync"
	"time"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/logs"
)

// logger：跟 Python 版相同的格式。stderr 是「等級 訊息」（journal 自己有時間），
// log 檔是「時間 等級 訊息」，每天午夜換新檔、只留 log_keep_days 天。
type logger struct {
	mu    sync.Mutex
	w     io.Writer
	file  bool
	close func() error
}

func newLogger() *logger { return &logger{w: os.Stderr} }

var lg = newLogger()

func (l *logger) log(level, format string, a ...any) {
	msg := fmt.Sprintf(format, a...)
	l.mu.Lock()
	defer l.mu.Unlock()
	line := level + " " + msg + "\n"
	if l.file {
		line = logs.Stamp(time.Now()) + " " + line
	}
	io.WriteString(l.w, line)
}

func (l *logger) info(format string, a ...any)    { l.log("INFO", format, a...) }
func (l *logger) warning(format string, a ...any) { l.log("WARNING", format, a...) }
func (l *logger) error(format string, a ...any)   { l.log("ERROR", format, a...) }

// toFile 是不是寫到 log 檔。
func (l *logger) toFile() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.file
}

// setupFile 改寫到 path。失敗就保留原本的輸出，回傳錯誤。
func (l *logger) setupFile(path string, keepDays int) error {
	w, err := logs.OpenDaily(path, keepDays)
	if err != nil {
		return err
	}
	l.mu.Lock()
	old := l.close
	l.w, l.file, l.close = w, true, w.Close
	l.mu.Unlock()
	if old != nil {
		old()
	}
	return nil
}
