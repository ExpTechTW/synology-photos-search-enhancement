// Package assets：包進執行檔的檔案（設定檔範本、systemd 服務範本）。
package assets

import "embed"

//go:embed config.example.yml
var ConfigTemplate string

//go:embed systemd/*
var Systemd embed.FS
