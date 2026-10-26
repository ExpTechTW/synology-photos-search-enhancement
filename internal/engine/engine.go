// Package engine：索引與搜尋相關的指令（以照片擁有者的身分執行）。
package engine

import (
	"fmt"
	"os"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/app"
)

var commands = map[string]bool{"check": true, "index": true, "serve": true, "progress": true, "search": true,
	"tag": true, "retry-failed": true, "compact": true}

// Handles：cmd 是不是這個套件的指令。
func Handles(cmd string) bool { return commands[cmd] }

// Main 執行索引與搜尋的指令。
func Main(g app.Global, cmd string, args []string) int {
	fmt.Fprintf(os.Stderr, "sse %s：尚未實作\n", cmd)
	return 1
}
