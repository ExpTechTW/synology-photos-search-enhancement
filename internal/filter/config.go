package filter

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"
)

// 設定檔（filter.json）由 sse install / apply 產生。欄位名稱、預設值與檢查規則都跟 Python 版相同，
// 設定有問題時退回純轉發，而不是讓程式起不來（DSM 的每個請求都經過這裡）。

const (
	modePassthrough = "passthrough"
	modeDemo        = "demo"
	modeEngine      = "engine"
)

// configKeys 是所有欄位，順序同 Python 版的 DEFAULTS。
var configKeys = []string{
	"listen", "socket_mode", "upstream", "status_file", "kill_switch", "photos_info", "mode",
	"inject_photos_version", "inspect_max_header", "inspect_max_body", "inject_max_response", "inject_timeout",
	"upstream_connect_timeout", "idle_timeout", "demo", "engine_socket", "engine_timeout", "subrequest_timeout",
	"item_get_version", "max_inject", "cache_ttl", "capture_file", "capture_max", "log_file", "log_keep_days",
}

// configDefaults 是各欄位的預設值（JSON）。
var configDefaults = map[string]string{
	"listen":      `"/run/sse/filter.sock"`,
	"socket_mode": `"0660"`,
	"upstream":    `"/run/synoscgi.sock"`,
	"status_file": `"/run/sse/status.json"`,
	// 這個檔案存在時立即停止注入（不必重啟），由 `sse disable` 建立
	"kill_switch": `"/usr/local/etc/sse/DISABLE"`,
	"photos_info": `"/var/packages/SynologyPhotos/INFO"`,
	// passthrough：只轉發；demo：注入設定檔裡固定的 item（驗證用）；engine：向 sse-engine 查詢
	"mode": `"passthrough"`,
	// 啟用注入時記錄的 Photos 版本；Photos 更新後回應格式可能改變，版本不符就自動退回純轉發
	"inject_photos_version":    `null`,
	"inspect_max_header":       `262144`,
	"inspect_max_body":         `65536`,
	"inject_max_response":      `4194304`,
	"inject_timeout":           `15`,
	"upstream_connect_timeout": `5`,
	"idle_timeout":             `3600`,
	"demo":                     `null`,
	"engine_socket":            `"/run/sse-engine/engine.sock"`,
	"engine_timeout":           `5`,
	"subrequest_timeout":       `10`,
	// Browse.Item.get 的版本：v1、v2 不接受手機 App 要的額外欄位（motion_photo、mobile_cache_mtime…），
	// v3～v7 都接受，回傳內容與原生 list_item 逐欄相同（Photos 1.9.0 實測）
	"item_get_version": `4`,
	"max_inject":       `500`,
	"cache_ttl":        `60`,
	// 診斷用：把搜尋 API 的請求參數與回應記到檔案（不含 cookie 與 _sid），記滿 capture_max 筆自動停止
	"capture_file": `null`,
	"capture_max":  `200`,
	// log 檔：每天午夜換新檔，只留 log_keep_days 天內的紀錄（不設就寫到 stderr / journal）
	"log_file":      `null`,
	"log_keep_days": `3`,
}

// Config 是解析後的設定。
type Config struct {
	Listen                 string
	SocketMode             int64
	Upstream               string
	StatusFile             string
	KillSwitch             string
	PhotosInfo             string
	Mode                   string
	InjectPhotosVersion    json.RawMessage // 原始 JSON；null 或沒填時是 nil
	InspectMaxHeader       int
	InspectMaxBody         int
	InjectMaxResponse      int
	InjectTimeout          time.Duration
	UpstreamConnectTimeout time.Duration
	IdleTimeout            time.Duration
	Demo                   *demoConfig
	EngineSocket           string
	EngineTimeout          time.Duration
	SubrequestTimeout      time.Duration
	ItemGetVersion         int
	MaxInject              int
	CacheTTL               time.Duration
	CaptureFile            string // 空字串：不錄
	CaptureMax             float64
	captureMaxRaw          json.RawMessage // 狀態檔原樣顯示
	LogFile                string          // 空字串：寫到 stderr
	LogKeepDays            int
}

type demoItem struct {
	raw       json.RawMessage
	id, time  int64
	ownerUser int64
}

type demoConfig struct {
	keyword string
	items   []demoItem
}

// ValidateConfig 用 filter 自己的解析規則檢查 filter.json 的內容（sse install / apply 寫入前呼叫）。
// 錯誤訊息跟 filter 啟動時記錄的相同。
func ValidateConfig(data []byte) error {
	if _, msg, _ := parseConfig(data); msg != "" {
		return errors.New(msg)
	}
	return nil
}

// loadConfig 回傳 (設定, 錯誤訊息, 警告)。設定有問題時退回純轉發，而不是讓程式起不來。
func loadConfig(path string) (*Config, string, []string) {
	if path == "" {
		return defaultConfig(), "", nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return defaultConfig(), "讀不到設定檔，改用預設值純轉發：" + err.Error(), nil
	}
	return parseConfig(data)
}

func defaultConfig() *Config {
	c, _ := buildConfig(map[string]json.RawMessage{})
	return c
}

func parseConfig(data []byte) (*Config, string, []string) {
	user, err := decodeUserConfig(data)
	if err != nil {
		return defaultConfig(), "讀不到設定檔，改用預設值純轉發：" + err.Error(), nil
	}
	cfg, warnings := buildConfig(user)
	if msg := validateConfig(user); msg != "" {
		cfg.Mode, cfg.Demo = modePassthrough, nil
		return cfg, "設定檔有誤，改為純轉發：" + msg, warnings
	}
	return cfg, "", warnings
}

func decodeUserConfig(data []byte) (map[string]json.RawMessage, error) {
	if !utf8.Valid(data) {
		return nil, errors.New("設定檔不是 UTF-8")
	}
	if !json.Valid(data) {
		var v any
		if err := json.Unmarshal(data, &v); err != nil {
			return nil, err
		}
		return nil, errors.New("設定檔不是合法的 JSON")
	}
	if kindOf(data) != '{' {
		return nil, errors.New("設定檔最外層必須是物件")
	}
	user := map[string]json.RawMessage{}
	if err := json.Unmarshal(data, &user); err != nil {
		return nil, err
	}
	return user, nil
}

// buildConfig 把 JSON 轉成 Config。Python 版沒有檢查型別的欄位（例如 listen、inject_timeout）型別不對時
// 改用預設值並回傳警告：Python 版會在處理請求時出錯，這裡寧可保守。
func buildConfig(user map[string]json.RawMessage) (*Config, []string) {
	var warnings []string
	get := func(k string) json.RawMessage {
		if v, ok := user[k]; ok {
			return v
		}
		return json.RawMessage(configDefaults[k])
	}
	def := func(k string) json.RawMessage { return json.RawMessage(configDefaults[k]) }
	str := func(k string) string {
		if s, ok := rawString(get(k)); ok {
			return s
		}
		warnings = append(warnings, fmt.Sprintf("設定欄位 %s 必須是字串，改用預設值", k))
		s, _ := rawString(def(k))
		return s
	}
	optStr := func(k string, warn bool) string {
		v := get(k)
		if s, ok := rawString(v); ok {
			return s
		}
		if kindOf(v) != 'n' && warn {
			warnings = append(warnings, fmt.Sprintf("設定欄位 %s 必須是路徑字串，改為不使用", k))
		}
		return ""
	}
	num := func(k string, warn bool) float64 {
		if f, ok := rawNumber(get(k)); ok {
			return f
		}
		if warn {
			warnings = append(warnings, fmt.Sprintf("設定欄位 %s 必須是數字，改用預設值", k))
		}
		f, _ := rawNumber(def(k))
		return f
	}
	// 有檢查的欄位：不合規定時整份設定會退回純轉發，這裡只要給個能用的值
	positive := func(k string) float64 {
		if f := num(k, false); f > 0 {
			return f
		}
		f, _ := rawNumber(def(k))
		return f
	}

	c := &Config{
		Listen:                 str("listen"),
		Upstream:               str("upstream"),
		StatusFile:             str("status_file"),
		KillSwitch:             str("kill_switch"),
		PhotosInfo:             str("photos_info"),
		InspectMaxHeader:       sizeInt(num("inspect_max_header", true)),
		InspectMaxBody:         sizeInt(num("inspect_max_body", true)),
		InjectMaxResponse:      sizeInt(num("inject_max_response", true)),
		InjectTimeout:          seconds(num("inject_timeout", true)),
		UpstreamConnectTimeout: seconds(num("upstream_connect_timeout", true)),
		IdleTimeout:            seconds(num("idle_timeout", true)),
		EngineTimeout:          seconds(positive("engine_timeout")),
		SubrequestTimeout:      seconds(positive("subrequest_timeout")),
		ItemGetVersion:         sizeInt(positive("item_get_version")),
		MaxInject:              sizeInt(positive("max_inject")),
		CacheTTL:               seconds(positive("cache_ttl")),
		CaptureFile:            optStr("capture_file", true),
		CaptureMax:             positive("capture_max"),
		LogFile:                optStr("log_file", false),
		LogKeepDays:            3,
	}
	if v := get("capture_max"); validPositive(v) {
		c.captureMaxRaw = compactRaw(v)
	} else {
		c.captureMaxRaw = def("capture_max")
	}
	if m, ok := rawString(get("mode")); ok {
		c.Mode = m
	}
	if m, ok := parseSocketMode(get("socket_mode")); ok && m >= 0 && m <= math.MaxUint32 {
		c.SocketMode = m
	} else {
		c.SocketMode = 0o660
	}
	if v := get("inject_photos_version"); kindOf(v) != 'n' {
		c.InjectPhotosVersion = compactRaw(v)
	}
	if s, ok := rawString(get("engine_socket")); ok {
		c.EngineSocket = s
	}
	if n, ok := rawInt(get("log_keep_days")); ok && n >= 1 && n <= 365 {
		c.LogKeepDays = int(n)
	}
	if c.Mode == modeDemo {
		c.Demo, _ = parseDemo(get("demo"))
	}
	return c, warnings
}

// validateConfig 依 Python 版 load_config 的順序檢查，回傳第一個錯誤（空字串表示沒問題）。
func validateConfig(user map[string]json.RawMessage) string {
	get := func(k string) json.RawMessage {
		if v, ok := user[k]; ok {
			return v
		}
		return json.RawMessage(configDefaults[k])
	}
	var unknown []string
	for k := range user {
		if _, ok := configDefaults[k]; !ok {
			unknown = append(unknown, k)
		}
	}
	if len(unknown) > 0 {
		sort.Strings(unknown)
		return "未知的設定欄位：" + strings.Join(unknown, ", ")
	}
	mode, _ := rawString(get("mode"))
	if kindOf(get("mode")) != '"' || (mode != modePassthrough && mode != modeDemo && mode != modeEngine) {
		return "mode 只能是 passthrough、demo 或 engine"
	}
	if _, ok := parseSocketMode(get("socket_mode")); !ok {
		return fmt.Sprintf("invalid literal for int() with base 8: '%s'", pyStr(get("socket_mode")))
	}
	if mode == modeDemo {
		if _, msg := parseDemo(get("demo")); msg != "" {
			return msg
		}
	}
	if mode == modeEngine && kindOf(get("engine_socket")) != '"' {
		return "mode=engine 需要 engine_socket"
	}
	for _, k := range []string{"engine_timeout", "subrequest_timeout", "max_inject", "cache_ttl", "capture_max", "item_get_version"} {
		if !validPositive(get(k)) {
			return k + " 必須是正數"
		}
	}
	if v := get("log_file"); kindOf(v) != 'n' && kindOf(v) != '"' {
		return "log_file 必須是路徑字串"
	}
	if n, ok := rawInt(get("log_keep_days")); !ok || n < 1 || n > 365 {
		return "log_keep_days 必須是 1～365 的整數"
	}
	return ""
}

func validPositive(v json.RawMessage) bool {
	f, ok := rawNumber(v)
	return ok && f > 0
}

// parseSocketMode 照 Python 的 int(str(值), 8)：字串或整數都可以（"0660"、660、"0o660"）。
func parseSocketMode(v json.RawMessage) (int64, bool) {
	switch kindOf(v) {
	case '"':
		s, _ := rawString(v)
		return pyParseInt(s, 8)
	case '-', '0', '1', '2', '3', '4', '5', '6', '7', '8', '9':
		if _, ok := rawInt(v); ok {
			return pyParseInt(strings.TrimSpace(string(v)), 8)
		}
	}
	return 0, false
}

func parseDemo(v json.RawMessage) (*demoConfig, string) {
	demo, ok := parseObject(v)
	if !ok {
		return nil, "mode=demo 需要 demo 區塊"
	}
	kw, ok := rawString(demo.get("keyword"))
	if !ok || pyStrip(kw) == "" {
		return nil, "demo.keyword 必須是非空字串"
	}
	items, ok := parseArray(demo.get("items"))
	if !ok || len(items) == 0 {
		return nil, "demo.items 必須是非空陣列"
	}
	d := &demoConfig{keyword: pyStrip(kw)}
	for _, raw := range items {
		it, ok := parseObject(raw)
		if !ok {
			return nil, "demo.items 的每一筆都必須是物件"
		}
		var vals [3]int64
		for i, k := range []string{"id", "owner_user_id", "time"} {
			n, ok := rawInt(it.get(k))
			if !ok {
				return nil, "demo.items 的每一筆都需要整數欄位 " + k
			}
			vals[i] = n
		}
		d.items = append(d.items, demoItem{raw: compactRaw(raw), id: vals[0], ownerUser: vals[1], time: vals[2]})
	}
	return d, ""
}

// seconds 把設定裡的秒數（可以有小數）轉成 Duration。
func seconds(f float64) time.Duration {
	if f > 1e9 {
		f = 1e9
	}
	return time.Duration(f * float64(time.Second))
}

// sizeInt 把設定裡的數字轉成 int（小數無條件捨去；限制範圍，32 位元的機種也不會溢位）。
func sizeInt(f float64) int {
	const limit = 1 << 30
	switch {
	case f > limit:
		return limit
	case f < -limit:
		return -limit
	}
	return int(math.Floor(f))
}
