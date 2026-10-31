//go:build !linux

package main

import (
	"errors"
	"os"
)

func mountResctrl(string) error    { return errors.New("resctrl sampling requires Linux") }
func unmountResctrl(string)        {}
func terminationSignal() os.Signal { return os.Interrupt }
