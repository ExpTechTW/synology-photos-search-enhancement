// Package config：config.yml，所有設定都在這一個檔案（DSM 帳密、模型端點、搜尋門檻…）。
//
// 檔案裡有密碼與 API key，所以權限必須是 600（只有擁有者能讀）。
// 使用者看到的欄位見 assets/config.example.yml；這裡把它轉成程式內部用的結構。
package config

import (
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Error 是設定檔的問題（訊息直接給使用者看）。
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func errorf(format string, a ...any) error { return &Error{fmt.Sprintf(format, a...)} }

type kind int

const (
	kBool kind = iota
	kInt
	kFloat
	kString
	kMap // 原樣保留的區塊（ocr.extra_body）
)

type field struct {
	name string
	kind kind
	def  any
}

type section struct {
	name   string
	fields []field
}

// schema 的順序就是預設值的順序；欄位名稱跟 config.yml 相同。
var schema = []section{
	{"synology", []field{
		{"account", kString, ""},
		{"password", kString, ""},
		{"url", kString, "https://127.0.0.1:5001"},
		{"verify_tls", kBool, false},
		{"timeout", kInt, 60},
	}},
	{"embedding", []field{
		{"base_url", kString, ""},
		{"api_key", kString, ""},
		{"model", kString, "embeddinggemma-2"},
		{"media_marker", kString, "<__media__>"},
		{"image_size", kString, "m"},
		{"timeout", kInt, 120},
	}},
	{"ocr", []field{
		{"enabled", kBool, true},
		{"base_url", kString, ""},
		{"api_key", kString, ""},
		{"model", kString, ""},
		{"image_size", kString, "xl"},
		{"max_tokens", kInt, 4096},
		{"extra_body", kMap, map[string]any{}},
		{"text_embedding", kBool, true},
		{"timeout", kInt, 120},
	}},
	{"search", []field{
		{"min_score", kFloat, 0.71},
		{"max_drop", kFloat, 0.08},
		{"text_min_score", kFloat, 0.75},
		{"text_max_drop", kFloat, 0.08},
		{"max_results", kInt, 500},
		{"dims", kInt, 512},
		{"ocr_match", kBool, true},
		{"refresh_interval", kInt, 15},
		{"query_cache", kInt, 2048},
		{"socket", kString, "/run/sse-engine/engine.sock"},
	}},
	{"index", []field{
		{"page_size", kInt, 500},
		{"quick_scan_interval", kInt, 600},
		{"full_scan_interval", kInt, 21600},
		{"max_tries", kInt, 3},
		{"yield_to_search", kInt, 15},
		{"auto_compact", kBool, true},
	}},
	{"filter", []field{
		{"mode", kString, "engine"},
		{"max_inject", kInt, 500},
	}},
	{"log", []field{
		{"keep_days", kInt, 3},
	}},
}

// ImageSizes 是 DSM 縮圖的尺寸。
var ImageSizes = []string{"sm", "m", "xl"}

// Values 是 config.yml 補上預設值、檢查過後的內容：section → 欄位 → 值（bool、int、float64、string、map）。
type Values map[string]map[string]any

func (v Values) str(s, k string) string         { return v[s][k].(string) }
func (v Values) num(s, k string) int            { return v[s][k].(int) }
func (v Values) flt(s, k string) float64        { return v[s][k].(float64) }
func (v Values) flag(s, k string) bool          { return v[s][k].(bool) }
func (v Values) obj(s, k string) map[string]any { return v[s][k].(map[string]any) }

func merge(user map[string]any) (Values, error) {
	known := map[string]bool{}
	for _, s := range schema {
		known[s.name] = true
	}
	if err := unknown(user, known, ""); err != nil {
		return nil, err
	}
	out := Values{}
	for _, s := range schema {
		var sec map[string]any
		switch raw := user[s.name].(type) {
		case nil:
			sec = map[string]any{}
		case map[string]any:
			sec = raw
		default:
			return nil, errorf("%s 必須是一個區塊（key: value）", s.name)
		}
		names := map[string]bool{}
		for _, f := range s.fields {
			names[f.name] = true
		}
		if err := unknown(sec, names, s.name+"."); err != nil {
			return nil, err
		}
		vals := map[string]any{}
		for _, f := range s.fields {
			v, err := coerce(s.name+"."+f.name, f, sec[f.name])
			if err != nil {
				return nil, err
			}
			vals[f.name] = v
		}
		out[s.name] = vals
	}
	return out, nil
}

func unknown(m map[string]any, known map[string]bool, prefix string) error {
	var bad []string
	for k := range m {
		if !known[k] {
			bad = append(bad, prefix+k)
		}
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return errorf("不認得的設定：%s", strings.Join(bad, ", "))
	}
	return nil
}

func coerce(name string, f field, v any) (any, error) {
	if v == nil {
		return f.def, nil
	}
	switch f.kind {
	case kBool:
		if b, ok := v.(bool); ok {
			return b, nil
		}
		return nil, errorf("%s 必須是 true 或 false", name)
	case kInt:
		if i, ok := v.(int); ok {
			return i, nil
		}
		if _, ok := v.(float64); ok {
			return nil, errorf("%s 必須是整數", name)
		}
		return nil, errorf("%s 必須是數字", name)
	case kFloat:
		switch n := v.(type) {
		case int:
			return float64(n), nil
		case float64:
			return n, nil
		}
		return nil, errorf("%s 必須是數字", name)
	case kString:
		switch s := v.(type) {
		case string:
			return s, nil
		case int:
			return strconv.Itoa(s), nil // 例如純數字的密碼
		case float64:
			return strconv.FormatFloat(s, 'f', -1, 64), nil
		}
		return nil, errorf("%s 必須是文字", name)
	case kMap:
		if m, ok := v.(map[string]any); ok {
			return m, nil
		}
		return nil, errorf("%s 必須是一個區塊", name)
	}
	return nil, errorf("%s：未知的型別", name)
}

// CheckPermissions：config.yml 裡有密碼，其他人不能讀。
func CheckPermissions(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if mode := st.Mode().Perm(); mode&0o077 != 0 {
		return errorf("%s 裡有密碼，權限不能讓其他人讀（目前 %o），請執行：chmod 600 %s", path, mode, path)
	}
	return nil
}

// Read 讀 config.yml 並檢查，回傳補上預設值的內容。
func Read(path string) (Values, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, errorf("讀不到設定檔 %s：%v", path, err)
	}
	return Parse(data)
}

// Parse 檢查 config.yml 的內容。
func Parse(data []byte) (Values, error) {
	var user map[string]any
	if err := yaml.Unmarshal(data, &user); err != nil {
		return nil, errorf("設定檔格式錯誤：%v", err)
	}
	c, err := merge(normalize(user).(map[string]any))
	if err != nil {
		return nil, err
	}
	if c.str("synology", "account") == "" {
		return nil, errorf("synology.account 沒有填（照片擁有者的 DSM 帳號）")
	}
	if c.str("synology", "password") == "" {
		return nil, errorf("synology.password 沒有填")
	}
	if c.str("embedding", "base_url") == "" {
		return nil, errorf("embedding.base_url 沒有填（embedding 服務的網址，例如 http://192.168.1.10:8080/v1）")
	}
	for _, k := range []string{"embedding", "ocr"} {
		if !contains(ImageSizes, c.str(k, "image_size")) {
			return nil, errorf("%s.image_size 只能是 %s", k, strings.Join(ImageSizes, "、"))
		}
	}
	if c.flag("ocr", "enabled") && c.str("ocr", "model") == "" {
		return nil, errorf("ocr.model 沒有填（要能看圖的模型）；不用 OCR 的話把 ocr.enabled 設成 false")
	}
	switch c.num("search", "dims") {
	case 128, 256, 512, 768:
	default:
		return nil, errorf("search.dims 只能是 128、256、512、768")
	}
	if m := c.str("filter", "mode"); m != "engine" && m != "passthrough" {
		return nil, errorf("filter.mode 只能是 engine 或 passthrough")
	}
	if d := c.num("log", "keep_days"); d < 1 || d > 365 {
		return nil, errorf("log.keep_days 必須是 1～365")
	}
	return c, nil
}

// normalize：yaml.v3 解出的整數可能是 int64、巢狀區塊是 map[string]any；統一成 int、float64、map[string]any。
func normalize(v any) any {
	switch x := v.(type) {
	case nil:
		return map[string]any{}
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normalizeValue(e)
		}
		return out
	}
	return v
}

func normalizeValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, e := range x {
			out[k] = normalizeValue(e)
		}
		return out
	case int64:
		return int(x)
	case uint64:
		if x <= math.MaxInt64 {
			return int(x)
		}
		return float64(x)
	case []any:
		for i := range x {
			x[i] = normalizeValue(x[i])
		}
	}
	return v
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Config 是程式內部用的設定。
type Config struct {
	Path    string // config.yml 的絕對路徑
	DataDir string // config.yml 所在的目錄：資料庫與 log 放在底下
	Values  Values // 使用者的設定（補上預設值）

	Synology Synology
	LLM      LLM
	Index    Index
	Search   Search
	Log      Log
	Filter   Filter
}

type Synology struct {
	URL, Account, Password string
	VerifyTLS              bool
	Timeout                int    // 秒
	ThumbSize              string // embedding 用的縮圖
	OCRThumbSize           string // OCR 用的縮圖
}

type LLM struct {
	EmbedBaseURL, EmbedAPIKey, EmbedModel, MediaMarker string
	EmbedTimeout                                       int
	OCRBaseURL, OCRAPIKey, OCRModel                    string
	OCRExtraBody                                       map[string]any
	OCRMaxTokens, OCRTimeout                           int
}

type Index struct {
	DB                                  string // data/index.db
	PageSize                            int
	QuickScanInterval, FullScanInterval int // 秒
	MaxTries                            int
	YieldToSearch                       int // 秒；0 = 不讓
	AutoCompact                         bool
	OCR                                 bool // 要不要做 OCR
	OCREmbed                            bool // OCR 文字也做成向量
}

type Search struct {
	MinScore, MaxDrop, TextMinScore, TextMaxDrop float64
	MaxResults, Dims                             int
	OCRMatch                                     bool
	RefreshInterval                              int // 秒
	QueryCache                                   int
	Socket                                       string
}

type Log struct {
	Dir      string
	KeepDays int
}

type Filter struct {
	Mode      string
	MaxInject int
}

// Load 讀 config.yml（並確認權限是 600），回傳程式內部用的設定。資料庫與 log 放在 config.yml 所在的目錄底下。
func Load(path string) (*Config, error) {
	return load(path, true)
}

// LoadNoPermCheck 跟 Load 一樣，但不檢查權限（例如 root 檢查別人的設定檔時）。
func LoadNoPermCheck(path string) (*Config, error) {
	return load(path, false)
}

func load(path string, checkPerms bool) (*Config, error) {
	if strings.HasPrefix(path, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, path[2:])
		}
	}
	path, _ = filepath.Abs(path)
	if checkPerms {
		if err := CheckPermissions(path); err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil, errorf("找不到設定檔 %s", path)
			}
			return nil, err
		}
	}
	c, err := Read(path)
	if err != nil {
		return nil, err
	}
	dataDir := filepath.Dir(path)
	ocrBase, ocrKey := c.str("ocr", "base_url"), c.str("ocr", "api_key")
	if ocrBase == "" {
		// OCR 沒填網址就跟 embedding 用同一個服務（連 API key 一起沿用）
		ocrBase = c.str("embedding", "base_url")
		if ocrKey == "" {
			ocrKey = c.str("embedding", "api_key")
		}
	}
	cfg := &Config{
		Path: path, DataDir: dataDir, Values: c,
		Synology: Synology{
			URL: c.str("synology", "url"), Account: c.str("synology", "account"),
			Password: c.str("synology", "password"), VerifyTLS: c.flag("synology", "verify_tls"),
			Timeout: c.num("synology", "timeout"), ThumbSize: c.str("embedding", "image_size"),
			OCRThumbSize: c.str("ocr", "image_size"),
		},
		LLM: LLM{
			EmbedBaseURL: c.str("embedding", "base_url"), EmbedAPIKey: c.str("embedding", "api_key"),
			EmbedModel: c.str("embedding", "model"), MediaMarker: c.str("embedding", "media_marker"),
			EmbedTimeout: c.num("embedding", "timeout"),
			OCRBaseURL:   ocrBase, OCRAPIKey: ocrKey, OCRModel: c.str("ocr", "model"),
			OCRExtraBody: c.obj("ocr", "extra_body"), OCRMaxTokens: c.num("ocr", "max_tokens"),
			OCRTimeout: c.num("ocr", "timeout"),
		},
		Index: Index{
			DB: filepath.Join(dataDir, "data", "index.db"), PageSize: c.num("index", "page_size"),
			QuickScanInterval: c.num("index", "quick_scan_interval"),
			FullScanInterval:  c.num("index", "full_scan_interval"), MaxTries: c.num("index", "max_tries"),
			YieldToSearch: c.num("index", "yield_to_search"), AutoCompact: c.flag("index", "auto_compact"),
			OCR:      c.flag("ocr", "enabled"),
			OCREmbed: c.flag("ocr", "enabled") && c.flag("ocr", "text_embedding"),
		},
		Search: Search{
			MinScore: c.flt("search", "min_score"), MaxDrop: c.flt("search", "max_drop"),
			TextMinScore: c.flt("search", "text_min_score"), TextMaxDrop: c.flt("search", "text_max_drop"),
			MaxResults: c.num("search", "max_results"), Dims: c.num("search", "dims"),
			OCRMatch: c.flag("search", "ocr_match"), RefreshInterval: c.num("search", "refresh_interval"),
			QueryCache: c.num("search", "query_cache"), Socket: c.str("search", "socket"),
		},
		Log:    Log{Dir: filepath.Join(dataDir, "logs"), KeepDays: c.num("log", "keep_days")},
		Filter: Filter{Mode: c.str("filter", "mode"), MaxInject: c.num("filter", "max_inject")},
	}
	return cfg, nil
}
