//go:build !unix

package main

import "os"

// windowChanged is nil where there is no such signal: the screen keeps the
// size it started with.
var windowChanged os.Signal
