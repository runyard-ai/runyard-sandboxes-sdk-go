package sdk

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakedaemon"
)

// terminalOn is a ready sandbox on a daemon whose terminals do what run does.
func terminalOn(t *testing.T, run func(context.Context, genv1.SandboxID, *fakedaemon.Terminal) int) (*fakedaemon.Daemon, *Sandbox) {
	t.Helper()
	d := fakedaemon.New(t, fakedaemon.WithKey("k"), fakedaemon.WithTerminal(run))
	client, err := New(d.URL, WithKey("k"))
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := client.Create(t.Context(), "tty", Spec{Image: "debian:bookworm-slim"})
	if err != nil {
		t.Fatal(err)
	}
	return d, sandbox
}

// echo answers each line typed with what it heard, and exits with 3 on "bye".
func echo(_ context.Context, _ genv1.SandboxID, tty *fakedaemon.Terminal) int {
	lines := bufio.NewScanner(tty.In)
	for lines.Scan() {
		if lines.Text() == "bye" {
			return 3
		}
		_, _ = io.WriteString(tty.Out, "heard "+lines.Text()+"\r\n")
	}
	if lines.Err() != nil {
		return 1
	}
	return 0
}

func TestATerminalCarriesKeysInAndTheScreenOut(t *testing.T) {
	_, sandbox := terminalOn(t, echo)
	tty, err := sandbox.Terminal(t.Context(), TerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tty.Close() }()
	if _, ok := tty.ExitCode(); ok {
		t.Fatal("an exit code before it exited")
	}
	for _, typed := range []string{"hello\n", "again\nbye\n"} {
		if _, err := tty.Write([]byte(typed)); err != nil {
			t.Fatal(err)
		}
	}
	screen, err := io.ReadAll(tty)
	if err != nil {
		t.Fatal(err)
	}
	if string(screen) != "heard hello\r\nheard again\r\n" {
		t.Errorf("the screen: %q", screen)
	}
	if code, ok := tty.ExitCode(); !ok || code != 3 {
		t.Errorf("exit %d, %v", code, ok)
	}
	// Closing one that exited is nothing.
	if err := tty.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
}

func TestATerminalRunsWhatItIsAskedAtItsSize(t *testing.T) {
	var asked fakedaemon.Terminal
	_, sandbox := terminalOn(t, func(_ context.Context, _ genv1.SandboxID, tty *fakedaemon.Terminal) int {
		asked = *tty
		size := <-tty.Resizes
		_, _ = io.WriteString(tty.Out, strings.Repeat("x", size[0]*size[1]))
		return 0
	})
	tty, err := sandbox.Terminal(t.Context(), TerminalOptions{Argv: []string{"top", "-d", "1"}, Cols: 120, Rows: 40, User: "1000:1000"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tty.Close() }()
	if err := tty.Resize(3, 2); err != nil {
		t.Fatal(err)
	}
	screen, err := io.ReadAll(tty)
	if err != nil || string(screen) != "xxxxxx" {
		t.Fatalf("%q %v", screen, err)
	}
	if !slices.Equal(asked.Argv, []string{"top", "-d", "1"}) || asked.Cols != 120 || asked.Rows != 40 || asked.User != "1000:1000" {
		t.Errorf("asked %+v", asked)
	}
}

func TestATerminalOfNoSizeIsTheHostsDefault(t *testing.T) {
	var cols, rows int
	_, sandbox := terminalOn(t, func(_ context.Context, _ genv1.SandboxID, tty *fakedaemon.Terminal) int {
		cols, rows = tty.Cols, tty.Rows
		return 0
	})
	tty, err := sandbox.Terminal(t.Context(), TerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tty.Close() }()
	if _, err := io.ReadAll(tty); err != nil {
		t.Fatal(err)
	}
	if cols != 80 || rows != 24 {
		t.Errorf("%dx%d", cols, rows)
	}
}

// Closing it is hanging up, which ends what runs.
func TestClosingATerminalHangsItUp(t *testing.T) {
	ended := make(chan struct{})
	_, sandbox := terminalOn(t, func(ctx context.Context, _ genv1.SandboxID, _ *fakedaemon.Terminal) int {
		<-ctx.Done()
		close(ended)
		return 129
	})
	tty, err := sandbox.Terminal(t.Context(), TerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if err := tty.Close(); err != nil {
		t.Fatal(err)
	}
	<-ended
	if _, err := tty.Write([]byte("x")); err == nil {
		t.Error("a write after closing")
	}
	if err := tty.Resize(1, 1); err == nil {
		t.Error("a resize after closing")
	}
}

func TestATerminalTheDaemonRefusesIsItsError(t *testing.T) {
	t.Run("not running", func(t *testing.T) {
		// Created, and held creating: Create would wait for it, so the
		// generated client makes it, and the handle is made of its answer.
		d := fakedaemon.New(t, fakedaemon.WithKey("k"), fakedaemon.WithTerminal(echo), fakedaemon.WithBoot(func(genv1.SandboxSpec) fakedaemon.Outcome {
			return fakedaemon.Outcome{State: genv1.SandboxStateCreating}
		}))
		client, err := New(d.URL, WithKey("k"))
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.API().CreateSandboxWithResponse(t.Context(), &genv1.CreateSandboxParams{IdempotencyKey: "held"}, genv1.SandboxSpec{Image: "x"})
		if err != nil || res.JSON202 == nil {
			t.Fatalf("%v %s", err, res.Body)
		}
		_, err = client.handle(*res.JSON202).Terminal(t.Context(), TerminalOptions{})
		if CodeOf(err) != "conflict" {
			t.Fatalf("%v", err)
		}
	})
	t.Run("without terminals", func(t *testing.T) {
		d := fakedaemon.New(t, fakedaemon.WithKey("k"))
		client, err := New(d.URL, WithKey("k"))
		if err != nil {
			t.Fatal(err)
		}
		sandbox, err := client.Create(t.Context(), "tty", Spec{Image: "x"})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := sandbox.Terminal(t.Context(), TerminalOptions{}); CodeOf(err) != "not_implemented" {
			t.Fatalf("%v", err)
		}
	})
	t.Run("another key", func(t *testing.T) {
		_, sandbox := terminalOn(t, echo)
		sandbox.client.key = "other"
		var refused *Error
		if _, err := sandbox.Terminal(t.Context(), TerminalOptions{}); !errors.As(err, &refused) || refused.Status != http.StatusUnauthorized {
			t.Fatalf("%v", err)
		}
	})
}

func TestATerminalNobodyAnswersIsAnError(t *testing.T) {
	_, sandbox := terminalOn(t, echo)
	sandbox.client.baseURL = "http://127.0.0.1:1"
	if _, err := sandbox.Terminal(t.Context(), TerminalOptions{}); err == nil || !strings.Contains(err.Error(), "opening a terminal in") {
		t.Fatalf("%v", err)
	}
}

// A sandbox that hangs up says no exit code: what ran in it did not say how it
// ended.
func TestATerminalTheSandboxHangsUpEndsWithoutAnExitCode(t *testing.T) {
	d, sandbox := terminalOn(t, echo)
	d.Intercept("openTerminal", func(w http.ResponseWriter, r *http.Request, _ http.HandlerFunc) {
		conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{terminalProtocol}})
		if err != nil {
			return
		}
		_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"news"}`))
		_ = conn.Close(websocket.StatusGoingAway, "the sandbox hung up")
	})
	tty, err := sandbox.Terminal(t.Context(), TerminalOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tty.Close() }()
	if screen, err := io.ReadAll(tty); err != nil || len(screen) != 0 {
		t.Fatalf("%q %v", screen, err)
	}
	if _, ok := tty.ExitCode(); ok {
		t.Error("an exit code nobody sent")
	}
}

func TestATerminalThatSpeaksNonsenseIsAnError(t *testing.T) {
	for _, c := range []struct {
		name string
		say  func(context.Context, *websocket.Conn)
		err  string
	}{
		{"a control message that is not JSON", func(ctx context.Context, conn *websocket.Conn) {
			_ = conn.Write(ctx, websocket.MessageText, []byte(`{`))
		}, "not JSON"},
		{"a close that is not an ending", func(_ context.Context, conn *websocket.Conn) {
			_ = conn.Close(websocket.StatusPolicyViolation, "no")
		}, "reading the terminal"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, sandbox := terminalOn(t, echo)
			d.Intercept("openTerminal", func(w http.ResponseWriter, r *http.Request, _ http.HandlerFunc) {
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{terminalProtocol}})
				if err != nil {
					return
				}
				c.say(r.Context(), conn)
				_ = conn.Close(websocket.StatusNormalClosure, "")
			})
			tty, err := sandbox.Terminal(t.Context(), TerminalOptions{})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = tty.Close() }()
			if _, err := io.ReadAll(tty); err == nil || !strings.Contains(err.Error(), c.err) {
				t.Fatalf("%v", err)
			}
		})
	}
}
