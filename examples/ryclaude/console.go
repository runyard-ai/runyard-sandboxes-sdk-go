package main

import (
	"errors"
	"io"
	"os"
	"os/signal"

	"golang.org/x/term"
)

// console is the terminal ryclaude carries to the sandbox: what is typed, the
// screen, and its size. main's is this process's own; a test's is not one.
type console interface {
	io.Reader
	io.Writer
	// interactive says whether somebody is at it to type: what `auth login`
	// asks before it waits for a code to be pasted.
	interactive() bool
	// size is its columns and rows; zero when it cannot say, which leaves the
	// sandbox's terminal at the host's default.
	size() (cols, rows int)
	// raw hands every key to the program, Ctrl-C included, until restored.
	raw() (restore func(), err error)
	// resizes says when the window changed size, until stopped.
	resizes() (resized <-chan os.Signal, stop func())
}

// screen is this process's terminal.
type screen struct {
	in, out *os.File
}

func terminal() console { return screen{in: os.Stdin, out: os.Stdout} }

func (s screen) Read(p []byte) (int, error)  { return s.in.Read(p) }
func (s screen) Write(p []byte) (int, error) { return s.out.Write(p) }

func (s screen) interactive() bool { return term.IsTerminal(int(s.in.Fd())) }

func (s screen) size() (int, int) {
	cols, rows, err := term.GetSize(int(s.out.Fd()))
	if err != nil {
		return 0, 0
	}
	return cols, rows
}

func (s screen) raw() (func(), error) {
	fd := int(s.in.Fd())
	if !term.IsTerminal(fd) {
		return nil, errors.New("standard input is not a terminal: ryclaude is claude on a screen, and needs one")
	}
	state, err := term.MakeRaw(fd)
	if err != nil {
		return nil, err
	}
	return func() { _ = term.Restore(fd, state) }, nil
}

func (s screen) resizes() (<-chan os.Signal, func()) {
	resized := make(chan os.Signal, 1)
	if windowChanged != nil {
		signal.Notify(resized, windowChanged)
	}
	return resized, func() { signal.Stop(resized) }
}
