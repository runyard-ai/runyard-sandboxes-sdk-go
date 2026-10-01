package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakedaemon"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// fakeConsole is a terminal that is not one: what is typed is given, the
// screen is kept, and the window was resized once.
type fakeConsole struct {
	in         io.Reader
	out        bytes.Buffer
	cols, rows int
	rawErr     error
	restored   bool
	resized    chan os.Signal
	stopped    bool
}

func typing(keys string) *fakeConsole {
	c := &fakeConsole{in: strings.NewReader(keys), cols: 100, rows: 30, resized: make(chan os.Signal, 1)}
	c.resized <- os.Interrupt // which signal says so is the real console's business
	return c
}

func (c *fakeConsole) Read(p []byte) (int, error)  { return c.in.Read(p) }
func (c *fakeConsole) Write(p []byte) (int, error) { return c.out.Write(p) }
func (c *fakeConsole) size() (int, int)            { return c.cols, c.rows }

func (c *fakeConsole) raw() (func(), error) {
	if c.rawErr != nil {
		return nil, c.rawErr
	}
	return func() { c.restored = true }, nil
}

func (c *fakeConsole) resizes() (<-chan os.Signal, func()) {
	return c.resized, func() { c.stopped = true }
}

// started is what the sandbox was when claude started in it.
type started struct {
	argv         []string
	cols, rows   int
	sandbox      genv1.Sandbox
	rules        []genv1.EgressRule
	files, modes map[string]string
}

// The credentials these tests copy in.
const secret = `{"claudeAiOauth":{"accessToken":"sk-test"}}`

// daemon is a daemon whose terminals run a claude that says what it heard,
// follows the window, and exits with exit.
func daemon(t *testing.T, exit int) (*fakedaemon.Daemon, *started) {
	t.Helper()
	var d *fakedaemon.Daemon
	seen := &started{files: map[string]string{}, modes: map[string]string{}}
	terminal := func(ctx context.Context, id genv1.SandboxID, tty *fakedaemon.Terminal) int {
		seen.argv, seen.cols, seen.rows = tty.Argv, tty.Cols, tty.Rows
		seen.sandbox, seen.rules = d.Sandboxes()[0], d.Rules(id)
		for _, path := range []string{loginPath, configPath} {
			body, mode, _ := d.File(id, path)
			seen.files[path], seen.modes[path] = string(body), mode
		}
		line, _ := bufio.NewReader(tty.In).ReadString('\n')
		var size [2]int
		select {
		case size = <-tty.Resizes:
		case <-ctx.Done():
			return 129 // hung up, as SIGHUP ends a process
		}
		_, _ = fmt.Fprintf(tty.Out, "claude heard %q at %dx%d", line, size[0], size[1])
		return exit
	}
	d = fakedaemon.New(t, fakedaemon.WithKey("k"), fakedaemon.WithNetwork(), fakedaemon.WithTerminal(terminal))
	return d, seen
}

// flags are the daemon's address and key, and credentials to copy.
func flags(t *testing.T, d *fakedaemon.Daemon, more ...string) []string {
	t.Helper()
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credentials, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	return append([]string{"-addr", d.URL, "-key", "k", "-credentials", credentials}, more...)
}

func TestClaudeRunsInTheSandboxOnThisTerminal(t *testing.T) {
	d, seen := daemon(t, 0)
	con := typing("hello\n")
	var stderr bytes.Buffer
	code := run(t.Context(), flags(t, d, "-allow", "github.com, ,proxy.golang.org", "--", "--model", "opus"), con, &stderr)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if got := con.out.String(); got != `claude heard "hello\n" at 100x30` {
		t.Errorf("the screen: %q", got)
	}
	if !slices.Equal(seen.argv, []string{"bash", "-c", `cd /workspace && exec claude "$@"`, "claude", "--model", "opus"}) {
		t.Errorf("argv %q", seen.argv)
	}
	if seen.cols != 100 || seen.rows != 30 {
		t.Errorf("opened at %dx%d", seen.cols, seen.rows)
	}
	if !con.restored || !con.stopped {
		t.Errorf("the terminal: restored %v, resizes stopped %v", con.restored, con.stopped)
	}

	// The credentials are in the sandbox, for nobody else there to read.
	if seen.files[loginPath] != secret || seen.modes[loginPath] != "0600" {
		t.Errorf("credentials: %q, mode %s", seen.files[loginPath], seen.modes[loginPath])
	}
	var config struct {
		HasCompletedOnboarding bool `json:"hasCompletedOnboarding"`
		Projects               map[string]struct {
			HasTrustDialogAccepted bool `json:"hasTrustDialogAccepted"`
		} `json:"projects"`
	}
	if err := json.Unmarshal([]byte(seen.files[configPath]), &config); err != nil || !config.HasCompletedOnboarding || !config.Projects["/workspace"].HasTrustDialogAccepted {
		t.Errorf("config %q: %v", seen.files[configPath], err)
	}
	if commands := d.Commands(); len(commands) != 1 || !slices.Equal(commands[0].Argv, []string{"mkdir", "-p", "/workspace"}) {
		t.Errorf("commands %+v", commands)
	}

	// The sandbox reaches Anthropic and what was allowed, and nothing else.
	spec := seen.sandbox.Spec
	want := append(slices.Clone(anthropic), "github.com", "proxy.golang.org")
	if len(seen.rules) != 1 || !slices.Equal(*seen.rules[0].Domains, want) {
		t.Errorf("rules %+v", seen.rules)
	}
	if spec.Image != "releases.runyard.ai/runyard-public/ryclaude:latest" || spec.Entrypoint != nil || spec.Brokers != nil {
		t.Errorf("spec %+v", spec)
	}
	if env := *spec.Env; env["IS_SANDBOX"] != "1" || env["DISABLE_AUTOUPDATER"] != "1" || env["TERM"] == "" {
		t.Errorf("env %v", env)
	}
	if left := d.Sandboxes(); len(left) != 0 {
		t.Errorf("%d sandboxes left", len(left))
	}
}

func TestClaudesExitCodeIsRyclaudes(t *testing.T) {
	d, _ := daemon(t, 7)
	if code := run(t.Context(), flags(t, d), typing("x\n"), io.Discard); code != 7 {
		t.Fatalf("exit %d", code)
	}
}

func TestWhatGoesWrongIsSaidAndTheSandboxDropped(t *testing.T) {
	failing := `{"exitCode":1,"stderr":"mkdir: read-only file system","stdout":"","durationMs":1,"truncated":false}`
	for _, c := range []struct {
		name    string
		prepare func(*fakedaemon.Daemon, *fakeConsole)
		args    []string
		said    string
	}{
		{"no daemon", nil, []string{"-addr", "nope"}, "is not a daemon's address"},
		{"no credentials", nil, []string{"-credentials", "/nonexistent/credentials.json"}, "log in here first"},
		{"a sandbox that does not start", func(d *fakedaemon.Daemon, _ *fakeConsole) {
			d.Intercept("createSandbox", fakedaemon.Refuse(http.StatusBadRequest, genv1.ErrorCodeImageUnknown, "no such image"))
		}, nil, "no such image"},
		{"a configuration the sandbox refuses", func(d *fakedaemon.Daemon, _ *fakeConsole) {
			d.Intercept("writeFile", fakedaemon.Refuse(http.StatusInsufficientStorage, genv1.ErrorCodeInternal, "the disk is full"))
		}, nil, "writing claude's configuration"},
		{"credentials the sandbox refuses", func(d *fakedaemon.Daemon, _ *fakeConsole) {
			d.Intercept("writeFile", nil) // the configuration, as it is
			d.Intercept("writeFile", fakedaemon.Refuse(http.StatusInsufficientStorage, genv1.ErrorCodeInternal, "the disk is full"))
		}, nil, "copying your credentials in"},
		{"a directory it cannot make", func(d *fakedaemon.Daemon, _ *fakeConsole) {
			d.Intercept("runCommand", fakedaemon.Answer(http.StatusOK, "application/json", failing))
		}, nil, "mkdir -p /workspace: exit 1: mkdir: read-only file system"},
		{"a sandbox that stopped answering", func(d *fakedaemon.Daemon, _ *fakeConsole) {
			d.Intercept("runCommand", fakedaemon.Refuse(http.StatusServiceUnavailable, genv1.ErrorCodeSandboxUnreachable, "gone"))
		}, nil, "gone"},
		{"a terminal it refuses", func(d *fakedaemon.Daemon, _ *fakeConsole) {
			d.Intercept("openTerminal", fakedaemon.Refuse(http.StatusConflict, genv1.ErrorCodeConflict, "paused"))
		}, nil, "starting claude: runyard-sandboxes: conflict: paused"},
		{"no terminal here", func(_ *fakedaemon.Daemon, con *fakeConsole) {
			con.rawErr = errors.New("standard input is not a terminal")
		}, nil, "standard input is not a terminal"},
		// A sandbox that hangs up before claude has exited is not claude
		// succeeding.
		{"a sandbox that hangs up", func(d *fakedaemon.Daemon, _ *fakeConsole) {
			d.Intercept("openTerminal", hangUpTerminal)
		}, nil, "hung up before claude exited"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, _ := daemon(t, 0)
			con := typing("x\n")
			if c.prepare != nil {
				c.prepare(d, con)
			}
			var stderr bytes.Buffer
			if code := run(t.Context(), flags(t, d, c.args...), con, &stderr); code != 1 || !strings.Contains(stderr.String(), c.said) {
				t.Fatalf("%d: %s", code, stderr.String())
			}
			if left := d.Sandboxes(); len(left) != 0 {
				t.Errorf("%d sandboxes left", len(left))
			}
		})
	}
}

func TestASandboxThatCannotBeDroppedIsSaid(t *testing.T) {
	d, _ := daemon(t, 0)
	d.Intercept("deleteSandbox", fakedaemon.Refuse(http.StatusInternalServerError, genv1.ErrorCodeInternal, "stuck"))
	var stderr bytes.Buffer
	if code := run(t.Context(), flags(t, d), typing("x\n"), &stderr); code != 1 || !strings.Contains(stderr.String(), "dropping the sandbox") {
		t.Fatalf("%d: %s", code, stderr.String())
	}
}

// hangUpTerminal opens a terminal and hangs it up without an exit code.
func hangUpTerminal(w http.ResponseWriter, r *http.Request, _ http.HandlerFunc) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"runyard.terminal.v1"}})
	if err != nil {
		return
	}
	_ = conn.Close(websocket.StatusGoingAway, "the sandbox hung up")
}

func TestFlags(t *testing.T) {
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"-h"}, typing(""), &stderr); code != 0 || !strings.Contains(stderr.String(), "-credentials") {
		t.Errorf("-h: %d %s", code, stderr.String())
	}
	if code := run(t.Context(), []string{"-nope"}, typing(""), io.Discard); code != 2 {
		t.Errorf("-nope: %d", code)
	}
}

// The daemon's address and the image are the environment's, when it says.
func TestTheEnvironmentSaysWhereTheDaemonAndTheImageAre(t *testing.T) {
	d, seen := daemon(t, 0)
	t.Setenv("RUNYARD_SANDBOXES_ADDR", d.URL)
	t.Setenv("RUNYARD_SANDBOXES_KEY", "k")
	t.Setenv("RYCLAUDE_IMAGE", "registry.example.com/claude:1")
	if code := run(t.Context(), flags(t, d)[4:], typing("x\n"), io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if seen.sandbox.Spec.Image != "registry.example.com/claude:1" {
		t.Errorf("image %s", seen.sandbox.Spec.Image)
	}
}
