//go:build !windows

package main

import (
	"syscall"
	"unsafe"
)

// termWidthFd queries the terminal width via ioctl TIOCGWINSZ.
// Returns 0 if the fd is not a terminal or the syscall fails.
func termWidthFd(fd uintptr) int {
	type winsize struct {
		Row, Col, Xpixel, Ypixel uint16
	}
	var ws winsize
	_, _, err := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGWINSZ, uintptr(unsafe.Pointer(&ws)))
	if err != 0 || ws.Col == 0 {
		return 0
	}
	return int(ws.Col)
}
