package admin

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
)

// ── nginx.conf：判斷、套用、還原（純文字處理，方便測試） ──────────────────
//
// 標記的格式跟 Python 版逐字相同：Python 版套用過的 nginx.conf 升級後照樣認得、還原得回去。

const (
	markBegin = "# >>> synology-search-enhancement"
	markEnd   = "# <<< synology-search-enhancement <<<"
)

const (
	pristine = "pristine" // 原廠設定（直連 synoscgi）
	managed  = "managed"  // 已套用
	unknown  = "unknown"  // 格式跟預期不同：一律不自動修改
)

var (
	stmtRe     = regexp.MustCompile(`server[ \t]+unix:` + regexp.QuoteMeta(paths.SynoscgiSock) + `[ \t]*;`)
	stmtFullRe = regexp.MustCompile(`^(?:server[ \t]+unix:` + regexp.QuoteMeta(paths.SynoscgiSock) + `[ \t]*;)$`)
	snippetRe  = regexp.MustCompile(`(?s)` + regexp.QuoteMeta(markBegin) + ` original=("[^\n]*") >>>\n(.*?)` + regexp.QuoteMeta(markEnd))
	upstreamRe = regexp.MustCompile(`\bupstream\s+synoscgi\s*\{`)
	commentRe  = regexp.MustCompile(`#[^\n]*`)
	soleStmtRe = regexp.MustCompile(`^server\s+unix:` + regexp.QuoteMeta(paths.SynoscgiSock) + `$`)
	pidRe      = regexp.MustCompile(`(?m)^\s*pid\s`)
)

func stripComments(text string) string { return commentRe.ReplaceAllString(text, "") }

func upstreamSpan(text string) (start, end int, err error) {
	hits := upstreamRe.FindAllStringIndex(text, -1)
	if len(hits) != 1 {
		return 0, 0, fmt.Errorf("nginx.conf 裡有 %d 個 upstream synoscgi（預期 1 個）", len(hits))
	}
	start = hits[0][1]
	i := strings.Index(text[start:], "}")
	if i < 0 {
		return 0, 0, errors.New("upstream synoscgi 沒有結尾的 }")
	}
	return start, start + i, nil
}

func snippet(original, indent string) string {
	return fmt.Sprintf("%s original=%s >>>\n%sserver unix:%s max_fails=1 fail_timeout=5s;\n%sserver unix:%s backup;\n%s%s",
		markBegin, pyJSON(original), indent, paths.FilterSock, indent, paths.SynoscgiSock, indent, markEnd)
}

// pyJSON 跟 Python 的 json.dumps(str) 一樣（非 ASCII 字元寫成 \uXXXX）：標記裡記錄原始設定的格式不能變。
func pyJSON(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch {
		case r == '"':
			b.WriteString(`\"`)
		case r == '\\':
			b.WriteString(`\\`)
		case r == '\n':
			b.WriteString(`\n`)
		case r == '\r':
			b.WriteString(`\r`)
		case r == '\t':
			b.WriteString(`\t`)
		case r == '\b':
			b.WriteString(`\b`)
		case r == '\f':
			b.WriteString(`\f`)
		case r >= 0x20 && r <= 0x7e:
			b.WriteRune(r)
		case r < 0x10000:
			fmt.Fprintf(&b, `\u%04x`, r)
		default:
			r1, r2 := utf16.EncodeRune(r)
			fmt.Fprintf(&b, `\u%04x\u%04x`, r1, r2)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// ownLine：text[a:b] 是否獨佔一行（前面只有縮排、後面只有空白）；回傳縮排。
func ownLine(text string, a, b int) (string, bool) {
	lineStart := strings.LastIndex(text[:a], "\n") + 1
	lineEnd := len(text)
	if i := strings.Index(text[b:], "\n"); i >= 0 {
		lineEnd = b + i
	}
	lead, tail := text[lineStart:a], text[b:lineEnd]
	if strings.TrimSpace(lead) != "" || strings.TrimSpace(tail) != "" {
		return "", false
	}
	return lead, true
}

// nginxClassify 回傳 (pristine | managed | unknown, 說明)。unknown 一律不自動修改。
func nginxClassify(text string) (string, string) {
	start, end, err := upstreamSpan(text)
	if err != nil {
		return unknown, err.Error()
	}
	body := text[start:end]
	if strings.Contains(text, markBegin) || strings.Contains(text, markEnd) {
		ms := snippetRe.FindAllStringSubmatchIndex(text, -1)
		if len(ms) != 1 || !(start <= ms[0][0] && ms[0][1] <= end) {
			return unknown, "nginx.conf 裡本外掛的標記區塊不完整，或不在 upstream synoscgi 內"
		}
		m := ms[0]
		whole := text[m[0]:m[1]]
		var original string
		if json.Unmarshal([]byte(text[m[2]:m[3]]), &original) != nil {
			return unknown, "標記裡記錄的原始設定無法解析"
		}
		firstLine, _, _ := strings.Cut(whole, "\n")
		_, own := ownLine(text, m[0], m[0]+len(firstLine))
		second, _, _ := strings.Cut(text[m[4]:m[5]], "\n")
		indent := second[:len(second)-len(strings.TrimLeftFunc(second, unicode.IsSpace))]
		if !own || !stmtFullRe.MatchString(original) || whole != snippet(original, indent) {
			return unknown, "本外掛的標記區塊被手動修改過，不自動處理"
		}
		if strings.TrimSpace(stripComments(text[start:m[0]]+text[m[1]:end])) != "" {
			return unknown, "upstream synoscgi 裡除了本外掛的區塊還有其他設定"
		}
		return managed, "已套用（filter 為主、synoscgi 為 backup）"
	}
	var stmts []string
	for _, s := range strings.Split(stripComments(body), ";") {
		if s = strings.TrimSpace(s); s != "" {
			stmts = append(stmts, s)
		}
	}
	if m := stmtRe.FindStringIndex(body); len(stmts) == 1 && m != nil && soleStmtRe.MatchString(stmts[0]) {
		if _, own := ownLine(text, start+m[0], start+m[1]); !own {
			return unknown, "upstream synoscgi 的 server 不是獨立一行，格式跟預期不同"
		}
		return pristine, "原廠設定（直連 synoscgi）"
	}
	if strings.Contains(body, "filter.sock") || strings.Contains(body, "synosse") {
		return unknown, "upstream synoscgi 指向其他 filter，請先還原 nginx.conf"
	}
	return unknown, "upstream synoscgi 的內容不是預期的單一 server：" + strings.Join(stmts, "; ")
}

// nginxPatch 把原廠設定改成 filter 為主、synoscgi 為 backup。
func nginxPatch(text string) (string, error) {
	if kind, why := nginxClassify(text); kind != pristine {
		return "", errors.New(why)
	}
	start, end, _ := upstreamSpan(text)
	m := stmtRe.FindStringIndex(text[start:end])
	a, b := start+m[0], start+m[1]
	indent, _ := ownLine(text, a, b)
	return text[:a] + snippet(text[a:b], indent) + text[b:], nil
}

// nginxUnpatch 還原成套用前的原廠設定（逐字相同）。
func nginxUnpatch(text string) (string, error) {
	kind, why := nginxClassify(text)
	if kind == pristine {
		return text, nil
	}
	if kind != managed {
		return "", errors.New(why)
	}
	m := snippetRe.FindStringSubmatchIndex(text)
	var original string
	json.Unmarshal([]byte(text[m[2]:m[3]]), &original)
	return text[:m[0]] + original + text[m[1]:], nil
}

func nginxTest(path string) (bool, string) {
	code, out := run([]string{paths.NginxBin, "-t", "-c", path}, 60*time.Second)
	return code == 0, out
}

func nginxReload() (bool, string) {
	cmd := []string{paths.NginxBin, "-c", paths.NginxConf}
	if data, err := os.ReadFile(paths.NginxConf); err == nil && !pidRe.Match(data) {
		// 跟 DSM 啟動 master 時一樣指定 pid 檔，reload 訊號才會送到正在跑的 master
		cmd = append(cmd, "-g", "pid "+paths.NginxPid+";")
	}
	code, out := run(append(cmd, "-s", "reload"), 60*time.Second)
	return code == 0, out
}

// nginxWrite：nginx -t → 原子替換 → reload → 健康檢查；任何一步失敗都還原成原本的檔案。
func nginxWrite(newText, probeURL string) {
	if isLink(paths.NginxConf) {
		fail("%s 是 symlink，跟預期不同，不自動修改", paths.NginxConf)
	}
	old, err := os.ReadFile(paths.NginxConf)
	must(err)
	st, err := os.Stat(paths.NginxConf)
	must(err)
	// 暫存檔放同一個目錄，nginx -t 解析相對路徑的 include 時才會跟正式設定一樣
	tmp := filepath.Join(filepath.Dir(paths.NginxConf), ".nginx.conf.sse-test")
	must(os.WriteFile(tmp, []byte(newText), 0o644))
	good, out := nginxTest(tmp)
	remove(tmp)
	if !good {
		fail("nginx -t 沒通過，nginx.conf 沒有變更：\n%s", out)
	}
	must(writeAtomic(paths.NginxConf, []byte(newText), 0, st))
	good, out = nginxReload()
	if good && probeURL != "" {
		time.Sleep(1500 * time.Millisecond) // 等新的 worker 接手
		if !probeOK(probeURL) {
			good, out = false, "DSM 健康檢查失敗"
		}
	}
	if !good {
		writeAtomic(paths.NginxConf, old, 0, st)
		nginxReload()
		fail("套用後 %s，已自動還原 nginx.conf", out)
	}
}

func readNginxConf() string {
	data, err := os.ReadFile(paths.NginxConf)
	must(err)
	return string(data)
}

// nginxApply 套用（原本就是套用狀態回傳 false）。
func nginxApply(probeURL string) bool {
	text := readNginxConf()
	kind, why := nginxClassify(text)
	if kind == managed {
		return false
	}
	if kind != pristine {
		fail("%s", why)
	}
	if !sockAlive(paths.FilterSock) {
		fail("filter 沒有在跑，不套用 nginx 設定")
	}
	patched, err := nginxPatch(text)
	must(err)
	st, err := os.Stat(paths.NginxConf)
	must(err)
	must(writeAtomic(paths.NginxBackup, []byte(text), 0, st))
	nginxWrite(patched, probeURL)
	return true
}

// nginxRevert 還原成原廠設定。還原後的狀態就是原生狀態，所以健康檢查失敗也不會再套回去。
func nginxRevert() bool {
	text := readNginxConf()
	kind, why := nginxClassify(text)
	if kind == pristine {
		return false
	}
	if kind != managed {
		fail("%s", why)
	}
	orig, err := nginxUnpatch(text)
	must(err)
	nginxWrite(orig, "")
	return true
}
