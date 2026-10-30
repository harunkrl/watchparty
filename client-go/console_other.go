//go:build !windows

package main

// setConsoleUTF8 is a no-op outside Windows: the terminal speaks UTF-8.
func setConsoleUTF8() {}
