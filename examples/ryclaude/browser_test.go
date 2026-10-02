package main

import (
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestTheBrowserIsOpenedByWhatTheSystemOpensThingsWith(t *testing.T) {
	for system, command := range map[string]string{"linux": "xdg-open", "darwin": "open", "plan9": ""} {
		if got := browserOf(system).command; got != command {
			t.Errorf("on %s it is %q, want %q", system, got, command)
		}
	}
	if err := browserOf("plan9").open(t.Context(), "https://sandboxes.example.com"); err == nil || !strings.Contains(err.Error(), "on plan9") {
		t.Errorf("a system with nothing to open one with: %v", err)
	}
}

// No browser is opened here: what stands in for the system's opener is a
// program that does nothing, and one that is not there.
func TestOpeningABrowserStartsItAndDoesNotWaitForIt(t *testing.T) {
	nothing, err := exec.LookPath("true")
	if err != nil {
		t.Fatalf("this test runs `true` in place of a browser: %v", err)
	}
	if err := (browser{command: nothing}).open(t.Context(), "https://sandboxes.example.com"); err != nil {
		t.Errorf("opening: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "xdg-open")
	if err := (browser{command: missing}).open(t.Context(), "https://sandboxes.example.com"); err == nil {
		t.Error("an opener that is not installed opened something")
	}
}
