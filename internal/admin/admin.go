// Package admin：安裝、解除安裝與維運指令（以 root 執行）。
package admin

import (
	"fmt"
	"os"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/app"
)

var commands = map[string]bool{"install": true, "update": true, "apply": true, "uninstall": true, "status": true,
	"doctor": true, "logs": true, "disable": true, "enable": true, "bypass": true, "reapply": true,
	"capture": true, "guard": true}

// Handles：cmd 是不是這個套件的指令。
func Handles(cmd string) bool { return commands[cmd] }

// Main 執行管理指令。
func Main(g app.Global, cmd string, args []string) int {
	fmt.Fprintf(os.Stderr, "sse %s：尚未實作\n", cmd)
	return 1
}
