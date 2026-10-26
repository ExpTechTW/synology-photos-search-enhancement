// Package app：各指令共用的全域選項。
package app

// Global 是放在子指令前面的選項：sse [--config FILE] [-v] <指令> …
type Global struct {
	Config  string   // --config：config.yml 的路徑（空白 = 安裝紀錄裡的資料目錄）
	Verbose bool     // -v：除錯訊息
	Argv    []string // 原始參數（不含程式名稱），寫進操作紀錄用
}
