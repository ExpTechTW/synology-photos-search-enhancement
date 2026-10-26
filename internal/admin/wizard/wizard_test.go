package wizard

import (
	"bufio"
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/config"
)

// feed 讓之後的輸入依序是 lines（每行一個答案）；回傳輸出的內容。
func feed(t *testing.T, lines ...string) *bytes.Buffer {
	t.Helper()
	var out bytes.Buffer
	oldIn, oldOut := In, Out
	In, Out = bufio.NewReader(strings.NewReader(strings.Join(lines, ""))), &out
	t.Cleanup(func() { In, Out = oldIn, oldOut })
	return &out
}

func secrets(t *testing.T, answers ...string) {
	t.Helper()
	old := ReadSecret
	ReadSecret = func(prompt string) ([]byte, error) {
		if len(answers) == 0 {
			return nil, ErrEOF
		}
		a := answers[0]
		answers = answers[1:]
		return []byte(a), nil
	}
	t.Cleanup(func() { ReadSecret = old })
}

func writeConfig(t *testing.T, values []Value) string {
	t.Helper()
	text, err := Render(values)
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := WriteConfig(p, text, -1, -1); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestTemplateNeedsFilling(t *testing.T) {
	_, err := config.Load(writeConfig(t, nil))
	if err == nil || !strings.Contains(err.Error(), "synology.account") {
		t.Fatalf("%v", err)
	}
}

func TestMinimalConfig(t *testing.T) {
	p := writeConfig(t, []Value{{"synology", "account", "alice"}, {"synology", "password", `p@ss: "x" #1`},
		{"embedding", "base_url", "http://gpu:8080/v1"}, {"ocr", "model", "gemma3"}})
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Synology.Password != `p@ss: "x" #1` { // 特殊字元
		t.Errorf("密碼：%q", c.Synology.Password)
	}
	if c.LLM.OCRBaseURL != "http://gpu:8080/v1" { // OCR 沿用 embedding 的服務
		t.Errorf("OCR：%q", c.LLM.OCRBaseURL)
	}
	if c.Index.DB != filepath.Join(filepath.Dir(p), "data", "index.db") || c.Filter.Mode != "engine" {
		t.Errorf("%q %q", c.Index.DB, c.Filter.Mode)
	}
	if st, _ := os.Stat(p); st.Mode().Perm() != 0o600 {
		t.Errorf("權限 %o", st.Mode().Perm())
	}
}

func TestValidation(t *testing.T) {
	base := []Value{{"synology", "account", "alice"}, {"synology", "password", "pw"},
		{"embedding", "base_url", "http://gpu/v1"}, {"ocr", "model", "m"}}
	with := func(extra ...Value) []Value { return append(append([]Value{}, base...), extra...) }
	// 開了 OCR 卻沒填模型
	if _, err := config.Load(writeConfig(t, with(Value{"ocr", "model", ""}))); err == nil || !strings.Contains(err.Error(), "ocr.model") {
		t.Errorf("%v", err)
	}
	p := writeConfig(t, with(Value{"ocr", "enabled", false}, Value{"ocr", "model", ""}))
	if c, err := config.Load(p); err != nil || c.Index.OCR {
		t.Errorf("%v", err)
	}
	data, _ := os.ReadFile(p)
	os.WriteFile(p, []byte(strings.Replace(string(data), "\nsearch:\n", "\nsearch:\n  typo_key: 1\n", 1)), 0o600)
	if _, err := config.Load(p); err == nil || !strings.Contains(err.Error(), "search.typo_key") {
		t.Errorf("%v", err)
	}
	if _, err := config.Load(writeConfig(t, with(Value{"filter", "mode", "inject"}))); err == nil {
		t.Error("filter.mode=inject 要拒絕")
	}
	p = writeConfig(t, base)
	os.Chmod(p, 0o644)
	if _, err := config.Load(p); err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("%v", err)
	}
}

func TestSetValueKeepsComments(t *testing.T) {
	text, err := Render([]Value{{"embedding", "base_url", "http://愛麗絲/v1"}, {"ocr", "image_size", "m"},
		{"ocr", "extra_body", map[string]any{"reasoning_effort": "none"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`  base_url: "http://愛麗絲/v1"          # e.g. http://192.168.1.10:8080/v1`,
		`  image_size: "m"                    # thumbnail sent for OCR`,
		`  extra_body: {reasoning_effort: "none"} # extra request fields`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("找不到 %q", want)
		}
	}
	// 只改指定區塊裡的欄位：embedding 也有 image_size
	if !strings.Contains(text, "  # image_size: m                    # thumbnail sent for embedding") {
		t.Error("embedding.image_size 不該被改")
	}
	if _, err := SetValue(text, "ocr", "nope", 1); err == nil {
		t.Error("範本裡沒有的欄位要回報")
	}
}

func TestInteractiveWizard(t *testing.T) {
	// 設定精靈填的每個欄位都要在範本裡找得到位置，填完要是合法的設定
	feed(t, "alice\n", "http://gpu:8080/v1\n", "\n", "y\n", "n\n", "http://ocr:11434/v1\n", "gemma3:12b\n")
	secrets(t, "pw 123", "sk-e", "sk-o")
	text, err := Interactive("alice")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "config.yml")
	WriteConfig(p, text, -1, -1)
	c, err := config.Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if c.Synology.Account != "alice" || c.Synology.Password != "pw 123" || c.LLM.EmbedModel != "embeddinggemma-2" {
		t.Errorf("%+v %+v", c.Synology, c.LLM)
	}
	if c.LLM.EmbedAPIKey != "sk-e" || c.LLM.OCRBaseURL != "http://ocr:11434/v1" || c.LLM.OCRAPIKey != "sk-o" ||
		c.LLM.OCRModel != "gemma3:12b" {
		t.Errorf("%+v", c.LLM)
	}
}

func TestInteractiveStopsAtEOF(t *testing.T) {
	feed(t) // 終端機斷線：不能一直問下去
	secrets(t)
	if _, err := Interactive(""); !errors.Is(err, ErrEOF) {
		t.Errorf("%v", err)
	}
}

// DSM 的終端機沒開 iutf8：用倒退鍵刪掉注音字只會刪一個位元組，送來殘缺的 UTF-8。
func TestBrokenUTF8AsksAgain(t *testing.T) {
	for _, bad := range []string{"\xe3\x84y", "�y", "\xff"} {
		out := feed(t, bad+"\n", "y\n")
		if !AskYes("確定？", false) {
			t.Errorf("%q", bad)
		}
		if !strings.Contains(out.String(), "無法辨識") {
			t.Errorf("%q：%s", bad, out)
		}
	}
	secrets(t, "\xe3\x84", "pw")
	out := feed(t)
	if v, _ := ReadLine("密碼", true); v != "pw" || !strings.Contains(out.String(), "無法辨識") {
		t.Errorf("%q %s", v, out)
	}
}

func TestAnswers(t *testing.T) {
	cases := []struct {
		lines []string
		def   bool
		want  bool
	}{
		{[]string{"\n"}, false, false}, {[]string{"\n"}, true, true}, {[]string{"ｙ\n"}, false, true},
		{[]string{" YES \n"}, false, true}, {[]string{"n\n"}, true, false}, {[]string{"好\n", "y\n"}, false, true},
		{nil, true, true}, {[]string{"y"}, false, true}, // 沒有換行就結束
	}
	for _, c := range cases {
		feed(t, c.lines...)
		if got := AskYes("確定？", c.def); got != c.want {
			t.Errorf("%q：%v", c.lines, got)
		}
	}
	feed(t, "ｈｔｔｐ://nas:8080/v1\n")
	if v, _ := Ask("網址", "", false); v != "http://nas:8080/v1" { // 全形換成半形
		t.Errorf("%q", v)
	}
	feed(t, "\n", "　x\n")
	if v, _ := Ask("網址", "", false); v != "x" { // 必填
		t.Errorf("%q", v)
	}
	feed(t, "\n")
	if v, _ := Ask("模型", "m1", false); v != "m1" {
		t.Errorf("%q", v)
	}
	secrets(t, " ｐｗ ")
	if v, _ := ReadLine("密碼", true); v != "ｐｗ" { // 密碼不改
		t.Errorf("%q", v)
	}
}
