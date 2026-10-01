//go:build unix

package main

import (
	"os"
	"syscall"
)

// windowChanged is the signal a terminal sends when its window is resized.
var windowChanged os.Signal = syscall.SIGWINCH
