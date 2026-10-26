// Package paths：系統上的路徑與外部指令。
//
// 測試時用 SSE_ROOT 把所有系統路徑移到沙盒；正式環境為空字串。
package paths

import "os"

var (
	Root    = os.Getenv("SSE_ROOT")
	Sandbox = Root != ""
)

// P 把系統路徑加上沙盒前綴。
func P(p string) string { return Root + p }

var (
	Prefix         = P("/usr/local/sse")
	Binary         = Prefix + "/sse" // 程式本體；/usr/local/bin/sse 指向它
	BinLink        = P("/usr/local/bin/sse")
	Etc            = P("/usr/local/etc/sse")
	FilterConfig   = Etc + "/filter.json"
	State          = Etc + "/state.json"
	KillSwitch     = Etc + "/DISABLE"
	UnitDir        = P("/etc/systemd/system")
	RunDir         = P("/run/sse")
	FilterSock     = RunDir + "/filter.sock"
	StatusFile     = RunDir + "/status.json"
	CaptureFile    = RunDir + "/capture.jsonl"
	LogDir         = P("/var/log/sse")
	SynoscgiSock   = P("/run/synoscgi.sock")
	NginxConf      = P("/etc/nginx/nginx.conf")
	NginxBackup    = NginxConf + ".sse-orig"
	NginxPid       = P("/run/nginx.pid")
	DSMVersionFile = P("/etc.defaults/VERSION")
	PhotosInfo     = P("/var/packages/SynologyPhotos/INFO")
	LockFile       = P("/run/sse-ctl.lock")
)

var (
	NginxBin  = env("SSE_NGINX", "/usr/bin/nginx")
	Systemctl = env("SSE_SYSTEMCTL", "systemctl")
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
