package main

import (
	"syscall"

	"golang.org/x/sys/unix"
)

// signalNumber maps a signal name from proc.Exit back to its number.
func signalNumber(name string) int {
	if s := unix.SignalNum("SIG" + name); s != 0 {
		return int(s)
	}
	for i := 1; i < 65; i++ {
		if syscall.Signal(i).String() == name {
			return i
		}
	}
	return 0
}
