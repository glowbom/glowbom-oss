package main

import (
	"os"

	"golang.org/x/sys/unix"
)

func pickerHasTerminal() bool {
	_, err := unix.IoctlGetTermios(int(os.Stdin.Fd()), unix.TCGETS)
	return err == nil
}
