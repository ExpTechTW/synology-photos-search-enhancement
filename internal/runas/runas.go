// Package runas：以照片擁有者（或 nginx 的帳號）的身分執行程式。
//
// 索引資料庫要由照片擁有者擁有：root 建立的 -wal、-shm 他會讀寫不了。
package runas

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/ExpTechTW/synology-photos-search-enhancement/internal/paths"
)

// Account 是一個系統帳號。
type Account struct {
	Name     string
	UID, GID uint32
	Groups   []uint32 // 所屬的所有群組（跟 initgroups 一樣）
	Home     string
}

// Lookup 找系統帳號（/etc/passwd、/etc/group）。
func Lookup(name string) (*Account, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return nil, err
	}
	uid, _ := strconv.ParseUint(u.Uid, 10, 32)
	gid, _ := strconv.ParseUint(u.Gid, 10, 32)
	a := &Account{Name: u.Username, UID: uint32(uid), GID: uint32(gid), Home: u.HomeDir}
	ids, err := u.GroupIds()
	if err == nil {
		for _, s := range ids {
			if g, err := strconv.ParseUint(s, 10, 32); err == nil {
				a.Groups = append(a.Groups, uint32(g))
			}
		}
	}
	return a, nil
}

// Command 建立以 name 的身分執行的指令。以 root 執行時才切換身分；沙盒（測試）裡沿用目前的環境變數。
func Command(ctx context.Context, name string, argv []string) (*exec.Cmd, error) {
	a, err := Lookup(name)
	if err != nil {
		return nil, err
	}
	cmd := exec.CommandContext(ctx, argv[0], argv[1:]...)
	if paths.Sandbox {
		cmd.Env = os.Environ()
	} else {
		cmd.Env = []string{"HOME=" + a.Home, "USER=" + a.Name, "LOGNAME=" + a.Name, "PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	}
	if os.Geteuid() == 0 && a.UID != 0 {
		cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: a.UID, Gid: a.GID, Groups: a.Groups}}
	}
	cmd.Dir = "/"
	if st, err := os.Stat(a.Home); err == nil && st.IsDir() {
		cmd.Dir = a.Home
	}
	return cmd, nil
}

// Run 以 name 的身分執行，回傳結束碼與輸出（stdout + stderr）。timeout 為 0 表示不限時。
func Run(name string, argv []string, timeout time.Duration) (int, string) {
	ctx := context.Background()
	if timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
	}
	cmd, err := Command(ctx, name, argv)
	if err != nil {
		return 127, err.Error()
	}
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	return exitCode(cmd.Run(), ctx), strings.TrimSpace(out.String())
}

// Interactive 以 name 的身分執行，沿用目前的終端機（輸出直接給使用者看）。
func Interactive(name string, argv []string) int {
	cmd, err := Command(context.Background(), name, argv)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 127
	}
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return exitCode(cmd.Run(), context.Background())
}

func exitCode(err error, ctx context.Context) int {
	if err == nil {
		return 0
	}
	if ctx.Err() != nil {
		return 124
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return 127
}
