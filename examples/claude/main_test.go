package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakedaemon"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/spawn"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// claude answers a prompt the way the CLI prints an answer.
func claude(_ context.Context, _ genv1.SandboxID, _ genv1.CommandRequest) genv1.CommandResult {
	return genv1.CommandResult{Stdout: "\nA microVM is a small virtual machine.\n\n", DurationMs: 3471}
}

func daemon(t *testing.T, opts ...fakedaemon.Option) *fakedaemon.Daemon {
	t.Helper()
	return fakedaemon.New(t, append([]fakedaemon.Option{fakedaemon.WithKey("k"), fakedaemon.WithBrokers("claude"), fakedaemon.WithRun(claude)}, opts...)...)
}

func invoke(t *testing.T, d *fakedaemon.Daemon, extra ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"-addr", d.URL, "-key", "k"}, extra...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestAPromptIsAnsweredInASandboxThatIsThenDropped(t *testing.T) {
	d := daemon(t)
	code, stdout, stderr := invoke(t, d, "-prompt", `what's "quoted"?`, "-name", "asker")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{`making "asker" from`, "ready in", "inside it, Claude talks to http://127.0.0.1:", `> what's "quoted"?`, "A microVM is a small virtual machine.\n\n(3.471s)", "dropped."} {
		if !strings.Contains(stdout, want) {
			t.Errorf("did not say %q:\n%s", want, stdout)
		}
	}
	// The prompt is $1 — an argument — and never part of the script.
	command := d.Commands()[0]
	if got := command.Argv; len(got) != 5 || got[2] != `exec claude -p "$1"` || got[4] != `what's "quoted"?` {
		t.Errorf("argv = %q", got)
	}
	if *command.TimeoutSeconds != 300 {
		t.Errorf("timeout = %d, want the default five minutes", *command.TimeoutSeconds)
	}
	if left := d.Sandboxes(); len(left) != 0 || d.Deletes()[0].Disk != "delete" {
		t.Fatalf("left %+v, drops %+v", left, d.Deletes())
	}
}

// requireDropped is the property every failure below shares: whatever
// happened, the machine that was made is not on the host afterwards.
func requireDropped(t *testing.T, d *fakedaemon.Daemon, made int) {
	t.Helper()
	if left := d.Sandboxes(); len(left) != 0 {
		t.Fatalf("%d sandboxes were left on the host", len(left))
	}
	if got := len(d.Deletes()); got != made {
		t.Fatalf("%d sandboxes were dropped, want %d", got, made)
	}
}

func withoutBrokers(w http.ResponseWriter, r *http.Request, serve http.HandlerFunc) {
	recorder := httptest.NewRecorder()
	serve(recorder, r)
	var sandbox genv1.Sandbox
	_ = json.Unmarshal(recorder.Body.Bytes(), &sandbox)
	sandbox.Brokers = nil
	body, err := json.Marshal(sandbox)
	if err != nil {
		http.Error(w, "re-encoding the sandbox: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(recorder.Code)
	_, _ = w.Write(body)
}

func TestEveryWayItFailsSaysWhyAndLeavesNothingBehind(t *testing.T) {
	for name, tc := range map[string]struct {
		opts      []fakedaemon.Option
		intercept map[string]fakedaemon.Interceptor
		args      []string
		want      string
		made      int
	}{
		"the host cannot be asked": {
			intercept: map[string]fakedaemon.Interceptor{"getInfo": fakedaemon.Refuse(403, genv1.ErrorCodeForbidden, "no")},
			want:      "asking the host what it is",
		},
		"the create is refused": {
			intercept: map[string]fakedaemon.Interceptor{"createSandbox": fakedaemon.Refuse(507, genv1.ErrorCodeCapacityExceeded, "eight of eight")},
			want:      "capacity_exceeded: eight of eight",
		},
		// It used to be left on the host: the error came back, the handle
		// was not looked at, and nothing said a machine was there.
		"the sandbox fails": {
			opts: []fakedaemon.Option{fakedaemon.WithBoot(func(genv1.SandboxSpec) fakedaemon.Outcome {
				return fakedaemon.Outcome{State: genv1.SandboxStateFailed, Error: "the kernel panicked"}
			})},
			want: "the kernel panicked",
			made: 1,
		},
		"the sandbox fails, with -keep": {
			opts: []fakedaemon.Option{fakedaemon.WithBoot(func(genv1.SandboxSpec) fakedaemon.Outcome {
				return fakedaemon.Outcome{State: genv1.SandboxStateFailed, Error: "the kernel panicked"}
			})},
			args: []string{"-keep"},
			want: "the kernel panicked",
			made: 1,
		},
		"the broker is not in the answer": {
			intercept: map[string]fakedaemon.Interceptor{"getSandbox": withoutBrokers},
			want:      `made without the "claude" broker`,
			made:      1,
		},
		"the command is refused": {
			intercept: map[string]fakedaemon.Interceptor{"runCommand": fakedaemon.Refuse(503, genv1.ErrorCodeSandboxUnreachable, "the agent is not answering")},
			want:      "asking Claude: runyard-sandboxes: sandbox_unreachable",
			made:      1,
		},
		"claude exits non-zero": {
			opts: []fakedaemon.Option{fakedaemon.WithRun(func(context.Context, genv1.SandboxID, genv1.CommandRequest) genv1.CommandResult {
				return genv1.CommandResult{ExitCode: 1, Stdout: "API Error: ", Stderr: "401 invalid x-api-key", DurationMs: 900}
			})},
			want: "claude exited 1 after 900ms: API Error: 401 invalid x-api-key",
			made: 1,
		},
	} {
		t.Run(name, func(t *testing.T) {
			d := daemon(t, tc.opts...)
			for op, intercept := range tc.intercept {
				d.InterceptAll(op, intercept)
			}
			code, _, stderr := invoke(t, d, tc.args...)
			if code != 1 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit %d, stderr %q; want 1 and %q", code, stderr, tc.want)
			}
			requireDropped(t, d, tc.made)
		})
	}
}

func TestAHostWithoutTheBrokerIsToldBeforeAnythingIsMade(t *testing.T) {
	d := fakedaemon.New(t, fakedaemon.WithKey("k"), fakedaemon.WithBrokers("github"))
	code, _, stderr := invoke(t, d)
	if code != 1 || !strings.Contains(stderr, `declares no shared broker "claude"`) || !strings.Contains(stderr, "examples/claude/README.md") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if calls := d.Calls("createSandbox"); calls != 0 {
		t.Fatalf("%d creates were sent to a host that cannot serve them", calls)
	}
	bare := fakedaemon.New(t, fakedaemon.WithKey("k"))
	if code, _, stderr := invoke(t, bare); code != 1 || !strings.Contains(stderr, "declares no shared broker") {
		t.Fatalf("a host with no brokers at all: exit %d: %s", code, stderr)
	}
}

func TestADropThatFailsIsTheProgramsFailure(t *testing.T) {
	d := daemon(t)
	d.InterceptAll("deleteSandbox", fakedaemon.Refuse(500, genv1.ErrorCodeInternal, "the ruleset would not come down"))
	code, stdout, stderr := invoke(t, d)
	if code != 1 || !strings.Contains(stderr, "dropping") || !strings.Contains(stderr, "the ruleset would not come down") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "A microVM is") {
		t.Fatalf("the answer was not printed before the drop failed: %s", stdout)
	}
}

// -keep leaves a working sandbox up and says where to open it.
func TestKeepLeavesTheSandboxUpAndSaysWhereItIs(t *testing.T) {
	d := daemon(t)
	code, stdout, stderr := invoke(t, d, "-keep", "-prompt", "", "-console", "http://localhost:9000/")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	left := d.Sandboxes()
	if len(left) != 1 || len(d.Deletes()) != 0 {
		t.Fatalf("-keep left %d sandboxes and dropped %d", len(left), len(d.Deletes()))
	}
	if !strings.Contains(stdout, "open   http://localhost:9000/console#/sandboxes/"+left[0].Id.String()) {
		t.Fatalf("the console link is wrong:\n%s", stdout)
	}
	if len(d.Commands()) != 0 || strings.Contains(stdout, "> ") {
		t.Fatalf("an empty prompt was asked:\n%s", stdout)
	}

	other := daemon(t)
	if _, stdout, _ := invoke(t, other, "-keep", "-prompt", ""); !strings.Contains(stdout, "open   "+other.URL+"/console#/sandboxes/") {
		t.Fatalf("without -console the link is not at -addr:\n%s", stdout)
	}
}

// Ctrl-C while the machine boots, and while Claude answers: the machine is
// dropped either way. The first used to leave it on the host.
func TestACancelledRunDropsTheSandbox(t *testing.T) {
	t.Run("while it boots", func(t *testing.T) {
		d := daemon(t, fakedaemon.WithBoot(func(genv1.SandboxSpec) fakedaemon.Outcome {
			return fakedaemon.Outcome{State: genv1.SandboxStateCreating}
		}))
		following := make(chan struct{})
		d.Intercept("streamSandboxEvents", func(w http.ResponseWriter, r *http.Request, serve http.HandlerFunc) {
			close(following)
			serve(w, r)
		})
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		spawn.Go(t, func() {
			done <- ask(ctx, options{addr: d.URL, key: "k", image: "claude-sandbox", prompt: "hi"}, io.Discard)
		})
		awaitOrFail(t, following, done)
		cancel()
		requireReturns(t, done)
		requireDropped(t, d, 1)
	})
	t.Run("while claude answers", func(t *testing.T) {
		asked := make(chan struct{})
		d := daemon(t, fakedaemon.WithRun(func(ctx context.Context, _ genv1.SandboxID, _ genv1.CommandRequest) genv1.CommandResult {
			close(asked)
			<-ctx.Done()
			return genv1.CommandResult{}
		}))
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		spawn.Go(t, func() {
			done <- ask(ctx, options{addr: d.URL, key: "k", image: "claude-sandbox", prompt: "hi"}, io.Discard)
		})
		awaitOrFail(t, asked, done)
		cancel()
		requireReturns(t, done)
		requireDropped(t, d, 1)
	})
}

// awaitOrFail waits for the moment to cancel at, and fails rather than
// hanging when the run ended before it came.
func awaitOrFail(t *testing.T, moment <-chan struct{}, done <-chan error) {
	t.Helper()
	select {
	case <-moment:
	case err := <-done:
		t.Fatalf("it ended before the moment to cancel it: %v", err)
	}
}

func requireReturns(t *testing.T, done <-chan error) {
	t.Helper()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled run said it succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("it did not stop when it was cancelled")
	}
}

// A -timeout under a second used to be sent as zero, which the contract
// reads as the host's default: "very short" became "a minute or more".
func TestTheTimeoutIsRoundedUpToWholeSeconds(t *testing.T) {
	for timeout, want := range map[time.Duration]int{
		500 * time.Millisecond:  1,
		time.Second:             1,
		1500 * time.Millisecond: 2,
		5 * time.Minute:         300,
	} {
		if got := seconds(timeout); got == nil || *got != want {
			t.Errorf("seconds(%s) = %v, want %d", timeout, got, want)
		}
	}
	for _, timeout := range []time.Duration{0, -time.Second} {
		if got := seconds(timeout); got != nil {
			t.Errorf("seconds(%s) = %d, want nil: the host's default", timeout, *got)
		}
	}
	d := daemon(t)
	if code, _, stderr := invoke(t, d, "-timeout", "300ms"); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if got := d.Commands()[0].TimeoutSeconds; got == nil || *got != 1 {
		t.Fatalf("-timeout 300ms was sent as %v", got)
	}
}

func TestTheFlagsAreTheProgramsInterface(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-h"}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "-keep") {
		t.Errorf("-h: exit %d, %s", code, stderr.String())
	}
	if code := run([]string{"-timeout", "soon"}, &stdout, &stderr); code != 2 {
		t.Errorf("a bad duration: exit %d, want 2", code)
	}
	stderr.Reset()
	if code := run([]string{"-addr", "localhost:8099"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "not a daemon's address") {
		t.Errorf("an address with no scheme: exit %d, %s", code, stderr.String())
	}
}
