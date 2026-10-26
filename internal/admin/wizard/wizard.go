// Package wizard：建立 config.yml（互動式設定精靈），以及讀取終端機的輸入。
//
// config.yml 都是把值填進 assets 的 config.example.yml（保留範本裡的說明），寫成權限 600 的檔案。
package wizard

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/assets"
)

// 輸入與輸出；測試時換掉。
var (
	In            = bufio.NewReader(os.Stdin)
	Out io.Writer = os.Stdout
	// ReadSecret 顯示 prompt 並讀一行不回顯的輸入（不含換行）。
	ReadSecret = readSecret
)

// ErrEOF：輸入已經結束（終端機斷線、stdin 被關閉），沒辦法再問。
var ErrEOF = errors.New("輸入已經結束")

// Value 是要填進範本的一個值。
type Value struct {
	Section, Key string
	Val          any // string、bool、int、float64 或 map[string]any
}

var sectionRe = regexp.MustCompile(`^[A-Za-z_]+:`)

// SetValue 把 section 底下的 key 改成 value；key 被註解掉（# key:）也會改成啟用。保留行尾的註解。
func SetValue(text, section, key string, value any) (string, error) {
	re := regexp.MustCompile(`^(\s+)(#\s*)?` + regexp.QuoteMeta(key) + `:(\s*)([^#]*?)(\s*#.*)?$`)
	lines := strings.Split(text, "\n")
	in := false
	for i, line := range lines {
		if sectionRe.MatchString(line) {
			in = strings.SplitN(line, ":", 2)[0] == section
			continue
		}
		if !in {
			continue
		}
		m := re.FindStringSubmatchIndex(line)
		if m == nil {
			continue
		}
		head := line[m[2]:m[3]] + key + ": " + YAMLScalar(value)
		if m[10] >= 0 {
			tail := line[m[10]:m[11]]
			if comment := strings.TrimSpace(tail); comment != "" {
				// 行尾說明盡量維持在原本的欄位（以字元數對齊，跟 Python 版相同）
				col := runes(line[:m[10]]) + runes(tail) - runes(strings.TrimLeftFunc(tail, unicode.IsSpace))
				w := max(runes(head)+1, col)
				head += strings.Repeat(" ", w-runes(head)) + comment
			}
		}
		lines[i] = head
		return strings.Join(lines, "\n"), nil
	}
	return "", fmt.Errorf("範本裡找不到 %s.%s", section, key)
}

func runes(s string) int { return utf8.RuneCountInString(s) }

// YAMLScalar 把值寫成 YAML（字串用 JSON 的雙引號字串，也是合法的 YAML）。
func YAMLScalar(v any) string {
	switch x := v.(type) {
	case bool:
		if x {
			return "true"
		}
		return "false"
	case int:
		return strconv.Itoa(x)
	case float64:
		s := strconv.FormatFloat(x, 'g', -1, 64)
		if !strings.ContainsAny(s, ".eEn") {
			s += ".0" // 跟 Python 的 str(float) 一樣，讀回來還是小數
		}
		return s
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, k := range keys {
			parts[i] = k + ": " + YAMLScalar(x[k])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	}
	return jsonString(fmt.Sprint(v))
}

func jsonString(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

// Render 把值填進範本。
func Render(values []Value) (string, error) {
	text := assets.ConfigTemplate
	for _, v := range values {
		var err error
		if text, err = SetValue(text, v.Section, v.Key, v.Val); err != nil {
			return "", err
		}
	}
	return text, nil
}

// WriteConfig 寫入權限 600 的 config.yml（先寫暫存檔再改名）。uid < 0 表示不改擁有者。
func WriteConfig(path, text string, uid, gid int) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	_, err = f.WriteString(text)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chmod(tmp, 0o600)
	}
	if err == nil && uid >= 0 && os.Geteuid() == 0 {
		err = os.Chown(tmp, uid, gid)
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}

// ── 讀取輸入 ─────────────────────────────────────────────────────────────

// readRaw 讀一行原始的位元組（不含換行）。讀到結尾且沒有內容時回傳 ErrEOF。
func readRaw(r *bufio.Reader) ([]byte, error) {
	line, err := r.ReadBytes('\n')
	if err != nil && len(line) == 0 {
		return nil, ErrEOF
	}
	line = bytes.TrimSuffix(line, []byte("\n"))
	return bytes.TrimSuffix(line, []byte("\r")), nil
}

var (
	restoreMu sync.Mutex
	restoreFn func()
)

// RestoreTerminal：輸入密碼時被中斷（Ctrl-C），把終端機的回顯開回來。
func RestoreTerminal() {
	restoreMu.Lock()
	defer restoreMu.Unlock()
	if restoreFn != nil {
		restoreFn()
		restoreFn = nil
	}
}

func readSecret(prompt string) ([]byte, error) {
	fmt.Fprint(Out, prompt)
	restore, err := noEcho(int(os.Stdin.Fd()))
	if err == nil {
		restoreMu.Lock()
		restoreFn = restore
		restoreMu.Unlock()
	}
	line, rerr := readRaw(In)
	if err == nil {
		RestoreTerminal()
		fmt.Fprintln(Out) // 使用者按的 Enter 沒有回顯
	}
	return line, rerr
}

// Halfwidth 把全形英數與全形空白（中文輸入法的全形模式）換成半形。
func Halfwidth(s string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 0xFF01 && r <= 0xFF5E:
			return r - 0xFEE0
		case r == 0x3000:
			return ' '
		}
		return r
	}, s)
}

// ReadLine 讀一行輸入，去掉前後空白。
//
// DSM 的終端機沒有開 iutf8：輸入法還在中文模式時打了字，用倒退鍵刪掉只會刪一個位元組，
// 畫面上看不出來，送出的卻是殘缺的 UTF-8。這種輸入請使用者重新輸入，不要當成答案。
// 輸入已經結束時回傳 ErrEOF。
func ReadLine(prompt string, secret bool) (string, error) {
	for {
		var raw []byte
		var err error
		if secret {
			raw, err = ReadSecret(prompt)
		} else {
			fmt.Fprint(Out, prompt)
			raw, err = readRaw(In)
		}
		if err != nil {
			return "", ErrEOF
		}
		if utf8.Valid(raw) && !bytes.ContainsRune(raw, utf8.RuneError) {
			s := string(raw)
			if !secret {
				s = Halfwidth(s) // 密碼原樣保留
			}
			return strings.TrimSpace(s), nil
		}
		fmt.Fprintln(Out, "  有無法辨識的字元（輸入法是不是還在中文模式？），請重新輸入")
	}
}

// Ask 問一個必填的問題；default 不是空白時直接按 Enter 就用它。
func Ask(prompt, def string, secret bool) (string, error) {
	shown := prompt + ": "
	if def != "" && !secret {
		shown = fmt.Sprintf("%s [%s]: ", prompt, def)
	}
	for {
		v, err := ReadLine(shown, secret)
		if err != nil {
			if def != "" {
				return def, nil
			}
			return "", err
		}
		if v == "" {
			v = def
		}
		if v != "" {
			return v, nil
		}
		fmt.Fprintln(Out, "  這一項必須填")
	}
}

// AskYes 問 y/n；直接按 Enter（或輸入已經結束）就用 def。
func AskYes(prompt string, def bool) bool {
	hint := "y/N"
	if def {
		hint = "Y/n"
	}
	for {
		v, err := ReadLine(fmt.Sprintf("%s [%s] ", prompt, hint), false)
		if err != nil {
			return def
		}
		switch strings.ToLower(v) {
		case "":
			return def
		case "y", "yes":
			return true
		case "n", "no":
			return false
		}
		fmt.Fprintln(Out, "  請輸入 y 或 n")
	}
}

// Interactive 是互動式的設定精靈，回傳 config.yml 的內容。
func Interactive(accountDefault string) (string, error) {
	fmt.Fprintln(Out, "\n建立設定檔（之後可以直接編輯 config.yml，再執行 sudo sse apply）")
	fmt.Fprintln(Out, "Creating config.yml (you can edit it later and run: sudo sse apply)")
	fmt.Fprintln(Out)
	var v []Value
	var err error
	ask := func(section, key, prompt, def string, secret bool) {
		if err != nil {
			return
		}
		var s string
		if s, err = Ask(prompt, def, secret); err == nil {
			v = append(v, Value{section, key, s})
		}
	}
	optional := func(section, key, prompt string) {
		if err != nil {
			return
		}
		var s string
		if s, err = ReadLine(prompt, true); err == nil {
			v = append(v, Value{section, key, s})
		}
	}
	ask("synology", "account", "照片擁有者的 DSM 帳號 / DSM account that owns the photos", accountDefault, false)
	ask("synology", "password", "DSM 密碼 / DSM password", "", true)
	if err == nil {
		fmt.Fprintln(Out, "\nEmbedding 服務（OpenAI 相容、提供 EmbeddingGemma 2 圖片 embedding；架設方式見 docs/models.md）")
	}
	ask("embedding", "base_url", "  網址 / base URL（例如 http://192.168.1.10:8080/v1）", "", false)
	optional("embedding", "api_key", "  API key（沒有就按 Enter / leave empty if none）: ")
	ask("embedding", "model", "  模型名稱 / model", "embeddinggemma-2", false)
	if err != nil {
		return "", err
	}
	fmt.Fprintln(Out)
	if AskYes("要讀照片裡的文字（OCR）嗎？需要一個能看圖的模型 / Enable OCR (needs a vision chat model)?", true) {
		if !AskYes("  OCR 跟 embedding 用同一個服務？/ Same server as embedding?", true) {
			ask("ocr", "base_url", "  OCR 網址 / OCR base URL", "", false)
			optional("ocr", "api_key", "  OCR API key（沒有就按 Enter）: ")
		}
		ask("ocr", "model", "  OCR 模型名稱 / OCR model（例如 gemma3:12b、gpt-4o-mini）", "", false)
	} else {
		v = append(v, Value{"ocr", "enabled", false})
	}
	if err != nil {
		return "", err
	}
	return Render(v)
}
