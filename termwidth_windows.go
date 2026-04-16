//go:build windows

package main

// termWidthFd is a no-op on Windows; termWidth falls back to $COLUMNS or 80.
func termWidthFd(fd uintptr) int {
	return 0
}
