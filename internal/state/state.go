// Package state：安裝紀錄（/usr/local/etc/sse/state.json）。
package state

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
)

// State 的欄位名稱跟 Python 版相同，升級時可以直接沿用舊的紀錄。
type State struct {
	Version       string `json:"version,omitempty"`
	InstalledAt   int64  `json:"installed_at,omitempty"`
	UpdatedAt     int64  `json:"updated_at,omitempty"`
	DSMBuild      string `json:"dsm_build,omitempty"`
	Channel       string `json:"channel,omitempty"` // release / pre-release
	Source        string `json:"source,omitempty"`  // release 的 tag，或 local（從原始碼安裝）
	PhotosVersion string `json:"photos_version,omitempty"`
	NginxUser     string `json:"nginx_user,omitempty"`
	NginxGroup    string `json:"nginx_group,omitempty"`
	ProbeURL      string `json:"probe_url,omitempty"`
	AutoApply     *bool  `json:"auto_apply,omitempty"` // 沒有這個欄位 = true；bypass 時是 false
	User          string `json:"user,omitempty"`       // 照片擁有者（索引與搜尋服務以他的身分執行）
	DataDir       string `json:"data_dir,omitempty"`   // config.yml、索引、log
}

// AutoApplyOn：guard 可以自動重新套用 nginx 設定（不在 bypass 狀態）。
func (s *State) AutoApplyOn() bool { return s.AutoApply == nil || *s.AutoApply }

// ConfigPath：資料目錄裡的 config.yml。
func (s *State) ConfigPath() string { return filepath.Join(s.DataDir, "config.yml") }

// Load 讀安裝紀錄；還沒安裝回傳 (nil, nil)。
func Load() (*State, error) {
	data, err := os.ReadFile(paths.State)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

// Save 寫回安裝紀錄（先寫暫存檔再改名，不會留下寫一半的檔案）。
func Save(s *State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(paths.State), 0o755); err != nil {
		return err
	}
	tmp := paths.State + ".tmp"
	if err := os.WriteFile(tmp, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, paths.State)
}
