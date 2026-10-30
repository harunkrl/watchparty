//go:build windows

package main

import "syscall"

// The console must speak UTF-8: a legacy conhost (cp857/cp1254 on a
// Turkish Windows) turns every box-drawing rune and every Turkish
// letter into mojibake. Stdlib only: the kernel32 entry points are
// loaded lazily, so a build without a console is harmless.
var (
	kernel32               = syscall.NewLazyDLL("kernel32.dll")
	procSetConsoleOutputCP = kernel32.NewProc("SetConsoleOutputCP")
	procSetConsoleCP       = kernel32.NewProc("SetConsoleCP")
)

// cpUTF8 is the Windows code page for UTF-8.
const cpUTF8 = 65001

// setConsoleUTF8 switches the console code pages to UTF-8: the output
// page for the dashboard glyphs, the input page for the friend flow
// (the nickname is read as bytes). A missing console (a GUI build) or
// an old Windows fails silently — the run continues with the old page.
func setConsoleUTF8() {
	procSetConsoleOutputCP.Call(uintptr(cpUTF8))
	procSetConsoleCP.Call(uintptr(cpUTF8))
}
