//go:build !linux

package main

func pickerHasTerminal() bool { return false }
