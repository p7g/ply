//go:build unix

package ply

import (
	"os"
	"os/signal"
	"syscall"
)

func watchSignals(c chan os.Signal) { signal.Notify(c, os.Interrupt, syscall.SIGTERM) }
