package ply

import (
	"os"
	"syscall"
	"unsafe"
)

func tty(f *os.File) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), syscall.TIOCGETA, uintptr(unsafe.Pointer(&t)))
	return e == 0
}
