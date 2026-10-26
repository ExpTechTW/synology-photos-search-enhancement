package wizard

import "syscall"

const ioctlGet, ioctlSet = syscall.TCGETS, syscall.TCSETS
