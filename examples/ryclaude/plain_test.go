package main

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
	"unicode"
)

// onlyText fails when what was said on a terminal holds anything a terminal
// would obey rather than show.
func onlyText(t *testing.T, said string) {
	t.Helper()
	for _, r := range said {
		if r != '\n' && r != '\t' && !unicode.IsGraphic(r) {
			t.Errorf("%U is in what was said: %q", r, said)
			return
		}
	}
}

func TestWhatIsSaidOnATerminalIsText(t *testing.T) {
	for name, c := range map[string]struct{ said, shown string }{
		"text":                     {"ryclaude: logged in as ada@example.com\n", "ryclaude: logged in as ada@example.com\n"},
		"text of every script":     {"clé – 鍵 — ключ … “key”\n", "clé – 鍵 — ключ … “key”\n"},
		"tabs, as flags are shown": {"  -url string\n    \tthe daemon\n", "  -url string\n    \tthe daemon\n"},
		"nothing":                  {"", ""},
		// Clearing the screen, and rewriting the line above.
		"an escape sequence":     {"ada\x1b[2J\x1b[1A@example.com", "ada\ufffd[2J\ufffd[1A@example.com"},
		"a window title":         {"\x1b]0;owned\x07", "\ufffd]0;owned\ufffd"},
		"a carriage return":      {"logged in as mallory\rlogged in as ada    ", "logged in as mallory\ufffdlogged in as ada    "},
		"a backspace":            {"mallory\b\b\b\b\b\b\bada", "mallory\ufffd\ufffd\ufffd\ufffd\ufffd\ufffd\ufffdada"},
		"a NUL and a DEL":        {"a\x00b\x7fc", "a\ufffdb\ufffdc"},
		"an 8-bit control":       {"a\u009bb", "a\ufffdb"},
		"a bidi override":        {"ada\u202egro.elpmaxe@", "ada\ufffdgro.elpmaxe@"},
		"a zero-width space":     {"ada\u200b@example.com", "ada\ufffd@example.com"},
		"a line separator":       {"one\u2028two", "one\ufffdtwo"},
		"bytes that are no rune": {"a\xffb\xc3", "a\ufffdb\ufffd"},
	} {
		var terminal bytes.Buffer
		n, err := fmt.Fprint(plain{&terminal}, c.said)
		if err != nil || n != len(c.said) || terminal.String() != c.shown {
			t.Errorf("%s: %q was shown as %q (%d, %v), want %q", name, c.said, terminal.String(), n, err, c.shown)
		}
		onlyText(t, terminal.String())
		// Text throughout is what was shown as it was said, a line break or
		// a tab apart: those are this program's own, and no part of a name.
		if want := terminal.String() == c.said && !strings.ContainsAny(c.said, "\n\t"); allText(c.said) != want {
			t.Errorf("%s: %q is text throughout: %v, want %v", name, c.said, allText(c.said), want)
		}
	}
}

// broken is a terminal that has gone.
type broken struct{}

func (broken) Write([]byte) (int, error) { return 0, errors.New("the terminal is gone") }

func TestATerminalThatIsGoneIsSaid(t *testing.T) {
	if n, err := fmt.Fprint(plain{broken{}}, "said"); err == nil || n != 0 {
		t.Errorf("%d %v", n, err)
	}
}
