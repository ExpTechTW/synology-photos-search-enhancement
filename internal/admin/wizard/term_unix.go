//go:build linux || darwin

package wizard

import (
	"syscall"
	"unsafe"
)

func tcget(fd int) (*syscall.Termios, error) {
	var t syscall.Termios
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlGet, uintptr(unsafe.Pointer(&t))); e != 0 {
		return nil, e
	}
	return &t, nil
}

func tcset(fd int, t *syscall.Termios) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), ioctlSet, uintptr(unsafe.Pointer(t))); e != 0 {
		return e
	}
	return nil
}

// IsTerminal：fd 是不是終端機。
func IsTerminal(fd int) bool {
	_, err := tcget(fd)
	return err == nil
}

// noEcho 關掉終端機的回顯（輸入密碼用），回傳還原的函式。
func noEcho(fd int) (func(), error) {
	old, err := tcget(fd)
	if err != nil {
		return nil, err
	}
	t := *old
	t.Lflag &^= syscall.ECHO
	if err := tcset(fd, &t); err != nil {
		return nil, err
	}
	return func() { tcset(fd, old) }, nil
}
