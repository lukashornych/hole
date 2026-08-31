//go:build linux || darwin

package engine

import (
	"os"
	"syscall"
	"unsafe"
)

// IsTerminal reports whether the file is a terminal.
//
// It issues the same terminal-attributes ioctl the container runtimes use, because the cheaper
// character-device test is not equivalent: /dev/null is a character device and is not a terminal,
// and /dev/null is exactly what a non-interactive `hole start` — a pipe, a CI job, `go test` —
// receives as stdin.
func IsTerminal(file *os.File) bool {
	var termios syscall.Termios
	_, _, errno := syscall.Syscall6(syscall.SYS_IOCTL, file.Fd(), ioctlGetTermios,
		uintptr(unsafe.Pointer(&termios)), 0, 0, 0)
	return errno == 0
}
