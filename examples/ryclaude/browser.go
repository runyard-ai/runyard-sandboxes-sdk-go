package main

import (
	"context"
	"fmt"
	"os/exec"
)

// browser opens an address in the person's browser, with the program their
// system has for opening things.
type browser struct {
	// command is that program; empty where this system is not known to have
	// one.
	command string
	system  string
}

// browserOf is the browser of a system, by its GOOS.
func browserOf(system string) browser {
	return browser{system: system, command: map[string]string{"linux": "xdg-open", "darwin": "open"}[system]}
}

func (b browser) open(ctx context.Context, address string) error {
	if b.command == "" {
		return fmt.Errorf("ryclaude knows no way to open one on %s", b.system)
	}
	// Not ctx's to end: the browser is the person's, and outlives a login
	// that is interrupted. A context that cannot end is also what leaves
	// nothing watching it once this has returned.
	command := exec.CommandContext(context.WithoutCancel(ctx), b.command, address)
	if err := command.Start(); err != nil {
		return err
	}
	// Not waited for: what opens a browser may be the browser, and return
	// only when it is closed.
	return command.Process.Release()
}
