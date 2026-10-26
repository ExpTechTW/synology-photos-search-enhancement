//go:build !linux && !darwin

package wizard

import "errors"

// IsTerminal：其他平台不支援，一律當成不是終端機。
func IsTerminal(fd int) bool { return false }

func noEcho(fd int) (func(), error) { return nil, errors.New("不支援") }
