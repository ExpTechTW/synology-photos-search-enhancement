// Package filter：SCGI filter，坐在 nginx 與 synoscgi 之間（sse filter，由 systemd 以 nginx 的帳號執行）。
package filter

import (
	"fmt"
	"os"
)

// Main 是 sse filter 的進入點。
func Main(args []string) int {
	fmt.Fprintln(os.Stderr, "sse filter：尚未實作")
	return 1
}
