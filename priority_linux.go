//go:build linux

package main

import "syscall"

func setLowPriority() {
	syscall.Setpriority(syscall.PRIO_PROCESS, 0, 19)
}
