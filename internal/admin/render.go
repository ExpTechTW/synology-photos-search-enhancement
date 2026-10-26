package admin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/assets"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/config"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/filter"
	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
)

// ── systemd 服務 ─────────────────────────────────────────────────────────

var placeholderRe = regexp.MustCompile(`\$(?:(\$)|([_a-zA-Z][_a-zA-Z0-9]*)|\{([_a-zA-Z][_a-zA-Z0-9]*)\})`)

// substitute 跟 Python 的 string.Template.substitute 一樣：$name、${name}、$$；沒有對應的值就是錯誤。
func substitute(tpl string, vars map[string]string) (string, error) {
	var missing []string
	out := placeholderRe.ReplaceAllStringFunc(tpl, func(m string) string {
		sm := placeholderRe.FindStringSubmatch(m)
		if sm[1] != "" {
			return "$"
		}
		name := sm[2] + sm[3]
		v, ok := vars[name]
		if !ok {
			missing = append(missing, name)
		}
		return v
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("範本裡有不認得的 $%s", missing[0])
	}
	return out, nil
}

// unitArg：路徑有空白時加上引號（systemd 的 ExecStart 跟 shell 一樣會切開）。
func unitArg(s string) string {
	if strings.ContainsAny(s, " \t\"'\\") {
		return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
	}
	return s
}

// renderUnit 產生 systemd 的設定檔。nginxUser/nginxGroup：filter 的身分；owner：索引與搜尋服務的身分。
func renderUnit(name, nginxUser, nginxGroup, owner, cfgPath string) ([]byte, error) {
	tpl, err := assets.Systemd.ReadFile("systemd/" + name)
	if err != nil {
		return nil, err
	}
	out, err := substitute(string(tpl), map[string]string{
		"user": nginxUser, "group": nginxGroup, "euser": owner, "egroup": nginxGroup,
		"binary": unitArg(paths.Binary), "filter_config": unitArg(paths.FilterConfig),
		"config": unitArg(cfgPath), "nginx_conf": paths.NginxConf,
	})
	return []byte(out), err
}

// ── filter 的設定（/usr/local/etc/sse/filter.json） ──────────────────────

// jsonObject 是保留欄位順序的 JSON 物件：改 filter.json 時不打亂原本的順序。
type jsonObject []jsonField

type jsonField struct {
	Key string
	Val json.RawMessage
}

func parseObject(data []byte) (jsonObject, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	if t, err := dec.Token(); err != nil || t != json.Delim('{') {
		return nil, errors.New("最外層必須是物件")
	}
	var obj jsonObject
	for dec.More() {
		t, err := dec.Token()
		if err != nil {
			return nil, err
		}
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			return nil, err
		}
		obj.set(t.(string), v)
	}
	if _, err := dec.Token(); err != nil {
		return nil, err
	}
	return obj, nil
}

func (o jsonObject) get(k string) (json.RawMessage, bool) {
	for _, f := range o {
		if f.Key == k {
			return f.Val, true
		}
	}
	return nil, false
}

// set 改值；原本沒有的欄位加在最後（跟 Python 的 dict 一樣）。
func (o *jsonObject) set(k string, v any) {
	raw, ok := v.(json.RawMessage)
	if !ok {
		raw = encodeJSON(v)
	}
	for i := range *o {
		if (*o)[i].Key == k {
			(*o)[i].Val = raw
			return
		}
	}
	*o = append(*o, jsonField{k, raw})
}

func (o *jsonObject) del(k string) {
	for i, f := range *o {
		if f.Key == k {
			*o = append((*o)[:i], (*o)[i+1:]...)
			return
		}
	}
}

func encodeJSON(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	enc.Encode(v)
	return json.RawMessage(bytes.TrimSuffix(b.Bytes(), []byte("\n")))
}

// marshal 寫成跟 Python 的 json.dumps(indent=2, ensure_ascii=False) + "\n" 相同的格式。
func (o jsonObject) marshal() []byte {
	if len(o) == 0 {
		return []byte("{}\n")
	}
	var b bytes.Buffer
	b.WriteString("{\n")
	for i, f := range o {
		var v bytes.Buffer
		if json.Indent(&v, f.Val, "  ", "  ") != nil {
			v.Write(f.Val)
		}
		fmt.Fprintf(&b, "  %s: %s", encodeJSON(f.Key), v.Bytes())
		if i < len(o)-1 {
			b.WriteByte(',')
		}
		b.WriteByte('\n')
	}
	b.WriteString("}\n")
	return b.Bytes()
}

// renderFilterConfig 由 config.yml 產生 filter 的設定（mode 可以跟 config.yml 不同：安裝時先以純轉發接上 nginx）。
// 診斷錄製的設定沿用現有的。
func renderFilterConfig(cfg *config.Config, mode string) []byte {
	var o jsonObject
	o.set("listen", paths.FilterSock)
	o.set("socket_mode", "0660")
	o.set("upstream", paths.SynoscgiSock)
	o.set("status_file", paths.StatusFile)
	o.set("kill_switch", paths.KillSwitch)
	o.set("photos_info", paths.PhotosInfo)
	o.set("mode", mode)
	// 記下現在的 Photos 版本：Photos 更新後回應格式可能改變，filter 會自動暫停注入，sse apply 確認後恢復
	var pv any
	if v := photosVersion(); mode == "engine" && v != "" {
		pv = v
	}
	o.set("inject_photos_version", pv)
	o.set("engine_socket", cfg.Search.Socket)
	o.set("max_inject", cfg.Filter.MaxInject)
	o.set("log_file", filepath.Join(paths.LogDir, "filter.log"))
	o.set("log_keep_days", cfg.Log.KeepDays)
	if data, err := os.ReadFile(paths.FilterConfig); err == nil {
		if old, err := parseObject(data); err == nil {
			for _, k := range []string{"capture_file", "capture_max"} {
				if v, ok := old.get(k); ok {
					o.set(k, v)
				}
			}
		}
	}
	data := o.marshal()
	validateFilterConfig(data)
	return data
}

// validateFilterConfig 用 filter 自己的解析規則檢查，避免兩邊規則不一致。
func validateFilterConfig(data []byte) {
	if err := filter.ValidateConfig(data); err != nil {
		fail("%v", err)
	}
}
