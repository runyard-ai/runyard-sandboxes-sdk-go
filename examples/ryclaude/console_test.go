package main

import (
	"io"
	"os"
	"strings"
	"testing"
)

// This process's console, on pipes rather than a terminal: what it can say of
// one that is not a terminal is what is tested here.
func pipes(t *testing.T) (screen, *os.File, *os.File) {
	t.Helper()
	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, f := range []*os.File{inR, inW, outR, outW} {
			_ = f.Close()
		}
	})
	return screen{in: inR, out: outW}, inW, outR
}

func TestTheConsoleCarriesBytes(t *testing.T) {
	s, typed, shown := pipes(t)
	if _, err := typed.WriteString("keys"); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 4)
	if n, err := s.Read(buf); err != nil || string(buf[:n]) != "keys" {
		t.Fatalf("%q %v", buf[:n], err)
	}
	if _, err := s.Write([]byte("screen")); err != nil {
		t.Fatal(err)
	}
	_ = s.out.Close()
	if got, err := io.ReadAll(shown); err != nil || string(got) != "screen" {
		t.Fatalf("%q %v", got, err)
	}
}

func TestAConsoleThatIsNotATerminalSaysSo(t *testing.T) {
	s, _, _ := pipes(t)
	if _, err := s.raw(); err == nil || !strings.Contains(err.Error(), "not a terminal") {
		t.Fatalf("%v", err)
	}
	// Nobody is typing at a pipe: `auth login` waits there for no code.
	if s.interactive() {
		t.Error("a pipe is somebody typing")
	}
	if cols, rows := s.size(); cols != 0 || rows != 0 {
		t.Errorf("%dx%d", cols, rows)
	}
	resized, stop := s.resizes()
	stop()
	select {
	case got := <-resized:
		t.Errorf("a resize nobody made: %v", got)
	default:
	}
	if terminal() == nil {
		t.Error("no console")
	}
}
