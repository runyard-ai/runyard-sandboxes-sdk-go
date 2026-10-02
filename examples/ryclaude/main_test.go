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
	"time"

	"github.com/coder/websocket"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakedaemon"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	// No test reads this person's own key, or is told by their environment
	// which daemon to call: a test that needs either says so, and one that
	// forgot finds nothing.
	for name, value := range map[string]string{
		"XDG_CONFIG_HOME": "/nonexistent/ryclaude-tests", "RUNYARD_SANDBOXES_ADDR": "", "RUNYARD_SANDBOXES_KEY": "", "RYCLAUDE_IMAGE": "",
	} {
		if err := os.Setenv(name, value); err != nil {
			panic(err)
		}
	}
	goleak.VerifyTestMain(m)
}

// fakeConsole is a terminal that is not one: what is typed is given, the
// screen is kept, and the window was resized once.
type fakeConsole struct {
	in         io.Reader
	out        bytes.Buffer
	cols, rows int
	// typedAt is whether somebody is at it: a terminal, and not a pipe.
	typedAt  bool
	rawErr   error
	restored bool
	resized  chan os.Signal
	stopped  bool
}

func typing(keys string) *fakeConsole {
	c := &fakeConsole{in: strings.NewReader(keys), cols: 100, rows: 30, resized: make(chan os.Signal, 1)}
	c.resized <- os.Interrupt // which signal says so is the real console's business
	return c
}

func (c *fakeConsole) Read(p []byte) (int, error)  { return c.in.Read(p) }
func (c *fakeConsole) Write(p []byte) (int, error) { return c.out.Write(p) }
func (c *fakeConsole) size() (int, int)            { return c.cols, c.rows }
func (c *fakeConsole) interactive() bool           { return c.typedAt }

// at is a machine with this terminal, a name, and no browser: a test that
// logs in brings the person who has one.
func at(con *fakeConsole) machine {
	return machine{
		console:  con,
		hostname: func() (string, error) { return "laptop", nil },
		browse:   func(context.Context, string) error { return errors.New("this machine has no browser") },
	}
}

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
func daemon(t *testing.T, exit int, more ...fakedaemon.Option) (*fakedaemon.Daemon, *started) {
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
	d = fakedaemon.New(t, append([]fakedaemon.Option{fakedaemon.WithKey("k"), fakedaemon.WithNetwork(), fakedaemon.WithTerminal(terminal)}, more...)...)
	return d, seen
}

// What the daemon is doing while the sandbox is made is said as it says it,
// and a first pull of the image is said to be one: the long start is not a
// terminal saying nothing.
func TestTheMakingOfTheSandboxIsSaid(t *testing.T) {
	const once = "this daemon has not run this image before"
	for _, c := range []struct {
		name     string
		progress []string
		first    bool
	}{
		{"an image the daemon pulls", []string{"pulling example", "layer 1 of 2 (10MB)", "layer 2 of 2 (3MB)", "unpacked", "booting"}, true},
		{"an image the daemon has", []string{"pulling example", "already cached", "booting"}, false},
		{"a daemon that says nothing", nil, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, _ := daemon(t, 0, fakedaemon.WithBoot(func(genv1.SandboxSpec) fakedaemon.Outcome {
				return fakedaemon.Outcome{Progress: c.progress}
			}))
			var stderr bytes.Buffer
			if code := run(t.Context(), flags(t, d), at(typing("hello\n")), &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			said := stderr.String()
			// Each thing in its order, after the line that says a sandbox
			// is being made.
			from := strings.Index(said, "making a sandbox")
			if from < 0 {
				t.Fatalf("said: %s", said)
			}
			for _, message := range c.progress {
				at := strings.Index(said[from:], "ryclaude:   "+message+"\n")
				if at < 0 {
					t.Fatalf("%q is not said, or not in its place: %s", message, said)
				}
				from += at
			}
			// Said once, before the first layer, and only of an image that
			// is pulled.
			if got := strings.Count(said, once); got != map[bool]int{true: 1, false: 0}[c.first] {
				t.Errorf("the first pull is said %d times: %s", got, said)
			}
			if c.first && strings.Index(said, once) > strings.Index(said, "layer 1 of 2") {
				t.Errorf("the first pull is said after its first layer: %s", said)
			}
		})
	}
}

// What a daemon says it is doing is text on this terminal, and nothing else.
func TestTheMakingOfTheSandboxIsOnlyText(t *testing.T) {
	d, _ := daemon(t, 0, fakedaemon.WithBoot(func(genv1.SandboxSpec) fakedaemon.Outcome {
		return fakedaemon.Outcome{Progress: []string{"layer \x1b[2Jone"}}
	}))
	var stderr bytes.Buffer
	if code := run(t.Context(), flags(t, d), at(typing("hello\n")), &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if said := stderr.String(); strings.Contains(said, "\x1b") || !strings.Contains(said, "layer \ufffd[2Jone") {
		t.Errorf("said: %q", said)
	}
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
	code := run(t.Context(), flags(t, d, "-allow", "github.com, ,proxy.golang.org", "--", "--model", "opus"), at(con), &stderr)
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
	if code := run(t.Context(), flags(t, d), at(typing("x\n")), io.Discard); code != 7 {
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
			if code := run(t.Context(), flags(t, d, c.args...), at(con), &stderr); code != 1 || !strings.Contains(stderr.String(), c.said) {
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
	if code := run(t.Context(), flags(t, d), at(typing("x\n")), &stderr); code != 1 || !strings.Contains(stderr.String(), "dropping the sandbox") {
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
	if code := run(t.Context(), []string{"-h"}, at(typing("")), &stderr); code != 0 || !strings.Contains(stderr.String(), "-credentials") {
		t.Errorf("-h: %d %s", code, stderr.String())
	}
	if code := run(t.Context(), []string{"-nope"}, at(typing("")), io.Discard); code != 2 {
		t.Errorf("-nope: %d", code)
	}
}

// What -h prints of a flag is its default, and the key is not one.
func TestHelpDoesNotSayTheKey(t *testing.T) {
	t.Setenv("RUNYARD_SANDBOXES_KEY", "rysk_k1234567_secret")
	t.Setenv("RUNYARD_SANDBOXES_ADDR", "https://ada:hunter2@sandboxes.example.com")
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"-h"}, at(typing("")), &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}
	for _, secret := range []string{"rysk_k1234567_secret", "hunter2"} {
		if strings.Contains(stderr.String(), secret) {
			t.Errorf("-h says %q: %s", secret, stderr.String())
		}
	}
}

func TestThisMachineIsWhatMainRunsOn(t *testing.T) {
	m := thisMachine()
	if m.console == nil || m.hostname == nil || m.browse == nil {
		t.Errorf("%+v", m)
	}
}

// ryclaude is what `ryclaude` with these arguments does at a terminal: its
// exit code, and what it said.
func ryclaude(t *testing.T, args ...string) (int, string) {
	t.Helper()
	credentials := filepath.Join(t.TempDir(), "credentials.json")
	if err := os.WriteFile(credentials, []byte(secret), 0o600); err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	code := run(t.Context(), append([]string{"-credentials", credentials}, args...), at(typing("x\n")), &stderr)
	return code, stderr.String()
}

// Logged in once, ryclaude needs to be told neither the daemon nor a key.
func TestTheKeyALoginKeptIsTheOneUsed(t *testing.T) {
	configHome(t)
	d, seen := daemon(t, 0, fakedaemon.WithGoogle())
	g := logIn(t, d)
	// Whichever way the same daemon is named, it is the same daemon.
	for _, c := range []struct {
		name, env string
		args      []string
	}{
		{"told nothing", "", nil},
		{"told the same daemon", "", []string{"-addr", d.URL}},
		{"told the same daemon, as it was typed", "", []string{"-addr", strings.ToUpper(d.URL[:4]) + d.URL[4:] + "/"}},
		{"told the same daemon by the environment", d.URL + "/", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv("RUNYARD_SANDBOXES_ADDR", c.env)
			if code, said := ryclaude(t, c.args...); code != 0 {
				t.Fatalf("exit %d: %s", code, said)
			}
			if seen.files[loginPath] != secret {
				t.Error("claude did not run in a sandbox")
			}
		})
	}
	// The sandboxes were made as the key's own, which only that key is let
	// in with here: the daemon's own is "k", and was not given.
	if keys := d.Keys(); len(keys) != 1 || keys[0].Id != g.KeyID || d.Calls("createSandbox") != 4 {
		t.Errorf("the daemon's keys %+v, and %d sandboxes made", keys, d.Calls("createSandbox"))
	}
}

// reached is what `reach` settles for flags, an environment and a login.
type reached struct{ addr, key, login, said string }

// The key a login kept goes to the daemon that issued it and to no other,
// and a key given here goes where it always did: never to the login's.
func TestWhichDaemonIsCalledAndWithWhichKey(t *testing.T) {
	const theirs = "https://sandboxes.example.com"
	g := granted()
	for _, c := range []struct {
		name              string
		flagAddr, flagKey string
		envAddr, envKey   string
		loggedIn          bool
		want              reached
	}{
		// Nothing kept: the flags, then the environment, then the default.
		{"a flag for each", "https://flag:1", "flag-key", "", "", false, reached{addr: "https://flag:1", key: "flag-key"}},
		{"the environment for each", "", "", "https://env:1", "env-key", false, reached{addr: "https://env:1", key: "env-key"}},
		{"flags over the environment", "https://flag:1", "flag-key", "https://env:1", "env-key", false, reached{addr: "https://flag:1", key: "flag-key"}},
		{"a flag's daemon, the environment's key", "https://flag:1", "", "https://env:1", "env-key", false, reached{addr: "https://flag:1", key: "env-key"}},
		{"the environment's daemon, a flag's key", "", "flag-key", "https://env:1", "", false, reached{addr: "https://env:1", key: "flag-key"}},
		{"a key and no daemon", "", "flag-key", "", "", false, reached{addr: defaultAddr, key: "flag-key"}},
		{"no key", "", "", "", "", false, reached{said: "no key: get one with `ryclaude auth login -url https://…`, naming your daemon, or give one with -key or RUNYARD_SANDBOXES_KEY"}},
		{"a daemon and no key", "https://flag:1", "", "https://env:1", "", false, reached{said: "no key: get one with `ryclaude auth login -url"}},

		// A key kept: it is used when none is given, at its own daemon.
		{"logged in, told nothing", "", "", "", "", true, reached{addr: theirs, key: g.Key, login: theirs}},
		{"logged in, a flag naming that daemon", theirs + "/", "", "", "", true, reached{addr: theirs, key: g.Key, login: theirs}},
		{"logged in, the environment naming that daemon", "", "", theirs, "", true, reached{addr: theirs, key: g.Key, login: theirs}},
		{"logged in, a flag naming that daemon over the environment naming another", theirs, "", "https://env:1", "", true, reached{addr: theirs, key: g.Key, login: theirs}},
		// And never sent to another, however it is named.
		{"logged in, a flag naming another daemon", "https://other.example.com", "", "", "", true, reached{said: "the key `ryclaude auth login` got is for " + theirs + ", and is not sent to the daemon -addr or RUNYARD_SANDBOXES_ADDR names"}},
		{"logged in, the environment naming another daemon", "", "", "https://other.example.com", "", true, reached{said: "is not sent to the daemon -addr or RUNYARD_SANDBOXES_ADDR names"}},
		{"logged in, a flag naming another daemon over the environment naming that one", "https://other.example.com", "", theirs, "", true, reached{said: "is not sent to the daemon -addr or RUNYARD_SANDBOXES_ADDR names"}},
		{"logged in, that daemon in the clear", "http://sandboxes.example.com", "", "", "", true, reached{said: "is not sent to the daemon -addr or RUNYARD_SANDBOXES_ADDR names"}},
		{"logged in, that daemon on another port", theirs + ":8443", "", "", "", true, reached{said: "is not sent to the daemon -addr or RUNYARD_SANDBOXES_ADDR names"}},
		{"logged in, a page of that daemon", theirs + "/console", "", "", "", true, reached{said: "is not sent to the daemon -addr or RUNYARD_SANDBOXES_ADDR names"}},
		{"logged in, a password in what names another", "https://ada:hunter2@other.example.com", "", "", "", true, reached{said: "is not sent to the daemon -addr or RUNYARD_SANDBOXES_ADDR names"}},
		{"logged in, what is no address", "nope", "", "", "", true, reached{said: "is not sent to the daemon -addr or RUNYARD_SANDBOXES_ADDR names"}},

		// A key given here is not the login's, and neither is its daemon:
		// it goes where it went before anybody logged in.
		{"logged in, a key and a daemon by flag", "https://flag:1", "flag-key", "", "", true, reached{addr: "https://flag:1", key: "flag-key"}},
		{"logged in, a key and a daemon by the environment", "", "", "https://env:1", "env-key", true, reached{addr: "https://env:1", key: "env-key"}},
		{"logged in, a key by flag and no daemon", "", "flag-key", "", "", true, reached{addr: defaultAddr, key: "flag-key"}},
		{"logged in, a key by the environment and no daemon", "", "", "", "env-key", true, reached{addr: defaultAddr, key: "env-key"}},
		{"logged in, a key by flag and the login's daemon named", theirs, "flag-key", "", "", true, reached{addr: theirs, key: "flag-key"}},

		// A key given here is a key all the same: it crosses no network in
		// the clear, and goes to a daemon's address and nothing else.
		{"a key to this machine in the clear", "http://127.0.0.1:8099/", "flag-key", "", "", false, reached{addr: "http://127.0.0.1:8099", key: "flag-key"}},
		{"a key to this machine by name", "", "", "http://localhost:8099", "env-key", false, reached{addr: "http://localhost:8099", key: "env-key"}},
		{"a key over TLS, as it was typed", "HTTPS://Sandboxes.Example.com/", "flag-key", "", "", false, reached{addr: theirs, key: "flag-key"}},
		{"a key to another machine in the clear", "http://evil.example.com", "flag-key", "", "", false, reached{said: "the key is not sent to the daemon -addr or RUNYARD_SANDBOXES_ADDR names: it is plain HTTP to another machine, where the key would cross the network for anybody on the way to read: use https://evil.example.com"}},
		{"the environment's key to another machine in the clear", "", "", "http://10.0.0.7:8099", "env-key", false, reached{said: "use https://10.0.0.7:8099"}},
		{"a flag's key to the environment's machine in the clear", "", "flag-key", "http://evil.example.com", "", true, reached{said: "it is plain HTTP to another machine"}},
		{"a key to an address with a password", "https://ada:hunter2@sandboxes.example.com", "flag-key", "", "", false, reached{said: "it names a user or a password"}},
		{"a key to a page of a daemon", "https://sandboxes.example.com/console", "flag-key", "", "", false, reached{said: "it has a path, a query or a fragment"}},
		{"a key to what is no address", "nope", "flag-key", "", "", false, reached{said: "it is not a daemon's address"}},
		{"a key over something that is not HTTP", "ws://127.0.0.1:8099", "flag-key", "", "", false, reached{said: "it is not a daemon's address"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := configHome(t)
			if c.loggedIn {
				if err := g.save(path); err != nil {
					t.Fatal(err)
				}
			}
			t.Setenv("RUNYARD_SANDBOXES_ADDR", c.envAddr)
			t.Setenv("RUNYARD_SANDBOXES_KEY", c.envKey)
			o := options{addr: c.flagAddr, key: c.flagKey}
			err := o.reach()
			if c.want.said != "" {
				if err == nil || !strings.Contains(err.Error(), c.want.said) {
					t.Fatalf("%+v %v, want it refused as %q", o, err, c.want.said)
				}
				if strings.Contains(err.Error(), g.Key) || strings.Contains(err.Error(), "hunter2") ||
					strings.Contains(err.Error(), "flag-key") || strings.Contains(err.Error(), "env-key") {
					t.Errorf("a secret is in what was said: %v", err)
				}
				return
			}
			if got := (reached{addr: o.addr, key: o.key, login: o.login}); err != nil || got != c.want {
				t.Fatalf("%+v %v, want %+v", got, err, c.want)
			}
		})
	}
}

func TestAKeptKeyThatCannotBeUsedIsSaidBeforeAnythingIsMade(t *testing.T) {
	for _, c := range []struct {
		name string
		keep func(t *testing.T, path string, g grant)
		said string
	}{
		{"it has expired", func(t *testing.T, path string, g grant) {
			t.Helper()
			g.ExpiresAt = time.Now().Add(-time.Minute)
			if err := g.save(path); err != nil {
				t.Fatal(err)
			}
		}, "the key for DAEMON expired on "},
		{"it cannot be read", func(t *testing.T, path string, _ grant) {
			t.Helper()
			keptAs(t, path, "{", 0o600)
		}, "PATH is not a key ryclaude kept"},
		{"others can read it", func(t *testing.T, path string, _ grant) {
			t.Helper()
			if err := os.Chmod(path, 0o640); err != nil {
				t.Fatal(err)
			}
		}, "chmod 600 PATH"},
		{"it is kept nowhere that can be found", func(t *testing.T, _ string, _ grant) {
			t.Helper()
			t.Setenv("XDG_CONFIG_HOME", "")
			t.Setenv("HOME", "")
		}, "no key: finding where ryclaude keeps its key"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := configHome(t)
			d, _ := daemon(t, 0, fakedaemon.WithGoogle())
			g := logIn(t, d)
			c.keep(t, path, g)
			code, said := ryclaude(t)
			want := strings.NewReplacer("DAEMON", d.URL, "PATH", path).Replace(c.said)
			if code != 1 || !strings.Contains(said, want) || strings.Contains(said, g.Key) {
				t.Fatalf("exit %d, want 1 and %q: %s", code, want, said)
			}
			// Whatever is wrong with it, the way out is said.
			if again := "run `ryclaude auth login -url " + d.URL + "` again"; strings.Contains(c.said, "expired") && !strings.Contains(said, again) {
				t.Errorf("it did not say %q: %s", again, said)
			}
			if d.Calls("createSandbox") != 0 {
				t.Error("a sandbox was made")
			}
			// With a key given, what is kept is not looked at.
			if code, said := ryclaude(t, "-addr", d.URL, "-key", "k"); code != 0 {
				t.Errorf("with a key of its own: exit %d: %s", code, said)
			}
		})
	}
}

func TestADaemonThatNoLongerTakesTheKeptKeySaysToLogInAgain(t *testing.T) {
	configHome(t)
	d, _ := daemon(t, 0, fakedaemon.WithGoogle())
	g := logIn(t, d)
	if code, _, said := auth(t, "logout", "-forget"); code != 0 {
		t.Fatalf("logging out: %s", said)
	}
	// Revoked there, and still kept here.
	path, _ := grantPath()
	if err := g.save(path); err != nil {
		t.Fatal(err)
	}
	code, said := ryclaude(t)
	if code != 1 || !strings.Contains(said, d.URL+" no longer accepts the key `ryclaude auth login` got, which was revoked or has expired: run `ryclaude auth login -url "+d.URL+"` again") {
		t.Fatalf("exit %d: %s", code, said)
	}
	if strings.Contains(said, g.Key) || len(d.Sandboxes()) != 0 {
		t.Errorf("said: %s", said)
	}

	// A key given here that is refused is not one a login would replace.
	code, said = ryclaude(t, "-addr", d.URL, "-key", "not-the-key")
	if code != 1 || !strings.Contains(said, "unauthorized") || strings.Contains(said, "auth login") {
		t.Fatalf("with a key of its own: exit %d: %s", code, said)
	}
}

// Another daemon is never shown the key, and nothing is asked of it.
func TestTheKeptKeyIsNotSentToAnotherDaemon(t *testing.T) {
	configHome(t)
	d, _ := daemon(t, 0, fakedaemon.WithGoogle())
	other, _ := daemon(t, 0)
	logIn(t, d)
	code, said := ryclaude(t, "-addr", other.URL)
	if code != 1 || !strings.Contains(said, "the key `ryclaude auth login` got is for "+d.URL+", and is not sent to the daemon -addr or RUNYARD_SANDBOXES_ADDR names") {
		t.Fatalf("exit %d: %s", code, said)
	}
	if other.Calls("createSandbox") != 0 || d.Calls("createSandbox") != 0 {
		t.Error("a daemon was asked for a sandbox")
	}
}

// A key given by hand is held to what a login is: it is not sent in the
// clear to another machine, and nothing is sent at all.
func TestAKeyGivenHereIsNotSentInTheClear(t *testing.T) {
	for _, tell := range [][]string{{"-addr", "http://sandboxes.example.com", "-key", "rysk_k1234567_secret"}, {"-key", "rysk_k1234567_secret"}} {
		if tell[0] == "-key" {
			t.Setenv("RUNYARD_SANDBOXES_ADDR", "http://sandboxes.example.com")
		}
		code, said := ryclaude(t, tell...)
		if code != 1 || !strings.Contains(said, "the key is not sent to the daemon -addr or RUNYARD_SANDBOXES_ADDR names") || !strings.Contains(said, "use https://sandboxes.example.com") {
			t.Errorf("%v: exit %d: %s", tell, code, said)
		}
		if strings.Contains(said, "rysk_") || strings.Contains(said, "making a sandbox") {
			t.Errorf("%v: said: %s", tell, said)
		}
	}
}

// A daemon that answers a key with a redirect is not followed with it.
func TestTheKeyDoesNotFollowARedirect(t *testing.T) {
	d, _ := daemon(t, 0)
	other, _ := daemon(t, 0)
	d.Intercept("createSandbox", func(w http.ResponseWriter, r *http.Request, _ http.HandlerFunc) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	if code, said := ryclaude(t, "-addr", d.URL, "-key", "k"); code != 1 || !strings.Contains(said, "HTTP 307") {
		t.Fatalf("exit %d: %s", code, said)
	}
	if other.Calls("createSandbox") != 0 {
		t.Error("the key was sent where the redirect said")
	}
}

// `auth` is ryclaude's own word; after --, it is claude's.
func TestAuthAfterTheDashesIsClaudes(t *testing.T) {
	d, seen := daemon(t, 0)
	if code, said := ryclaude(t, "-addr", d.URL, "-key", "k", "--", "auth", "login"); code != 0 {
		t.Fatalf("exit %d: %s", code, said)
	}
	if got := seen.argv[len(seen.argv)-2:]; !slices.Equal(got, []string{"auth", "login"}) {
		t.Errorf("claude was given %q", seen.argv)
	}
}

// The daemon's address and the image are the environment's, when it says.
func TestTheEnvironmentSaysWhereTheDaemonAndTheImageAre(t *testing.T) {
	d, seen := daemon(t, 0)
	t.Setenv("RUNYARD_SANDBOXES_ADDR", d.URL)
	t.Setenv("RUNYARD_SANDBOXES_KEY", "k")
	t.Setenv("RYCLAUDE_IMAGE", "registry.example.com/claude:1")
	if code := run(t.Context(), flags(t, d)[4:], at(typing("x\n")), io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if seen.sandbox.Spec.Image != "registry.example.com/claude:1" {
		t.Errorf("image %s", seen.sandbox.Spec.Image)
	}
}
