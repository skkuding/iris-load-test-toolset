//go:build linux

package main

import (
	"os"
	"syscall"
)

func mountResctrl(path string) error { return syscall.Mount("resctrl", path, "resctrl", 0, "") }
func unmountResctrl(path string)     { _ = syscall.Unmount(path, 0) }
func terminationSignal() os.Signal   { return syscall.SIGTERM }
