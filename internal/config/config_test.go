package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const minimal = `
synology:
  account: alice
  password: pw
embedding:
  base_url: http://gpu:8080/v1
ocr:
  model: gemma3
`

func write(t *testing.T, text string, mode os.FileMode) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.yml")
	if err := os.WriteFile(p, []byte(text), mode); err != nil {
		t.Fatal(err)
	}
	os.Chmod(p, mode)
	return p
}

func TestMinimalConfig(t *testing.T) {
	c, err := Load(write(t, minimal, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if c.Index.DB != filepath.Join(c.DataDir, "data", "index.db") || c.Log.Dir != filepath.Join(c.DataDir, "logs") {
		t.Errorf("路徑：%s %s", c.Index.DB, c.Log.Dir)
	}
	// OCR 沒填網址就沿用 embedding 的服務
	if c.LLM.OCRBaseURL != "http://gpu:8080/v1" || c.Synology.ThumbSize != "m" || c.Synology.OCRThumbSize != "xl" {
		t.Errorf("%+v %+v", c.LLM, c.Synology)
	}
	if c.Search.MinScore != 0.71 || c.Search.Dims != 512 || c.Filter.Mode != "engine" || c.Log.KeepDays != 3 ||
		!c.Index.OCR || !c.Index.OCREmbed || c.Search.Socket != "/run/sse-engine/engine.sock" {
		t.Errorf("預設值：%+v %+v %+v", c.Search, c.Filter, c.Index)
	}
}

func TestPermissions(t *testing.T) {
	_, err := Load(write(t, minimal, 0o644))
	if err == nil || !strings.Contains(err.Error(), "chmod 600") {
		t.Errorf("權限 644 要拒絕：%v", err)
	}
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yml")); err == nil || !strings.Contains(err.Error(), "找不到設定檔") {
		t.Errorf("%v", err)
	}
}

// doc 組出 config.yml：base 的三個必填區塊，加上每個區塊額外的設定（同一區塊不能出現兩次）
func doc(extra map[string]string) string {
	sections := map[string]string{
		"synology":  "  account: alice\n  password: pw\n",
		"embedding": "  base_url: http://gpu:8080/v1\n",
		"ocr":       "  model: gemma3\n",
	}
	order := []string{"synology", "embedding", "ocr", "search", "filter", "log", "searh"}
	var b strings.Builder
	for _, name := range order {
		body, base := sections[name]
		add, ok := extra[name]
		if !base && !ok {
			continue
		}
		if ok && !strings.HasPrefix(add, "  ") { // 整個區塊換成這個值（例如 synology: 5）
			b.WriteString(name + ": " + add + "\n")
			continue
		}
		b.WriteString(name + ":\n" + body + add)
	}
	return b.String()
}

func TestValidation(t *testing.T) {
	for _, tc := range []struct {
		section, extra, want string
	}{
		{"search", "  min_scor: 0.5\n", "不認得的設定：search.min_scor"},
		{"searh", "  min_score: 0.5\n", "不認得的設定：searh"},
		{"search", "  min_score: high\n", "search.min_score 必須是數字"},
		{"search", "  max_results: 1.5\n", "search.max_results 必須是整數"},
		{"search", "  ocr_match: 1\n", "search.ocr_match 必須是 true 或 false"},
		{"search", "  dims: 300\n", "search.dims 只能是"},
		{"embedding", "  image_size: huge\n", "embedding.image_size 只能是"},
		{"filter", "  mode: demo\n", "filter.mode 只能是"},
		{"log", "  keep_days: 0\n", "log.keep_days"},
		{"ocr", "  extra_body: none\n", "ocr.extra_body 必須是一個區塊"},
		{"synology", "5", "synology 必須是一個區塊"},
		{"synology", "  url: [a]\n", "synology.url 必須是文字"},
		{"synology", "{account: a}", "synology.password 沒有填"},
		{"ocr", "{enabled: true}", "ocr.model 沒有填"},
		{"synology", "  url: [unclosed\n", "設定檔格式錯誤"},
		{"synology", "  account: dup\n", "設定檔格式錯誤"}, // 同一個欄位出現兩次
	} {
		_, err := Parse([]byte(doc(map[string]string{tc.section: tc.extra})))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s %q：%v，預期含 %q", tc.section, tc.extra, err, tc.want)
		}
	}
	if _, err := Parse([]byte(doc(nil))); err != nil {
		t.Errorf("基本設定：%v", err)
	}
}

func TestValues(t *testing.T) {
	c, err := Load(write(t, `
synology: {account: bob, password: 123456}
embedding: {base_url: "http://e/v1", api_key: sk-e, image_size: xl}
ocr: {enabled: false, model: ""}
search: {min_score: 1, text_min_score: 0.4}
`, 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if c.Synology.Password != "123456" { // YAML 把純數字當成整數
		t.Errorf("password = %q", c.Synology.Password)
	}
	if c.Index.OCR || c.Index.OCREmbed || c.LLM.OCRAPIKey != "sk-e" || c.Synology.ThumbSize != "xl" {
		t.Errorf("%+v %+v", c.Index, c.LLM)
	}
	if c.Search.MinScore != 1.0 || c.Search.TextMinScore != 0.4 {
		t.Errorf("%+v", c.Search)
	}
	if _, err = Parse([]byte(minimal + "ocr:\n  base_url: http://o/v1\n")); err == nil {
		t.Error("同一個區塊出現兩次是 YAML 錯誤")
	}
	c, err = Load(write(t, strings.Replace(minimal, "  model: gemma3", "  model: gemma3\n  base_url: http://o/v1\n  extra_body: {reasoning_effort: none}", 1), 0o600))
	if err != nil {
		t.Fatal(err)
	}
	if c.LLM.OCRBaseURL != "http://o/v1" || c.LLM.OCRAPIKey != "" || c.LLM.OCRExtraBody["reasoning_effort"] != "none" {
		t.Errorf("OCR 用自己的服務時不沿用 embedding 的 key：%+v", c.LLM)
	}
}

// 範本本身（填好必填欄位後）要是合法的設定
func TestTemplateIsValid(t *testing.T) {
	data, err := os.ReadFile("../assets/config.example.yml")
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(data), `account: ""`, `account: "a"`, 1)
	text = strings.Replace(text, `password: ""`, `password: "p"`, 1)
	text = strings.Replace(text, `base_url: ""                       # e.g.`, `base_url: "http://x/v1"           # e.g.`, 1)
	text = strings.Replace(text, `model: ""  `, `model: "m"  `, 1)
	if _, err := Parse([]byte(text)); err != nil {
		t.Errorf("範本：%v", err)
	}
}
