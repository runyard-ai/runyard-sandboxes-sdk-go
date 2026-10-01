package main

import (
	"bytes"
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakedaemon"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/spawn"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// claude answers each turn with the prompt it was given, which is $1 — the
// fifth argument — and never part of the script.
func claude(_ context.Context, _ genv1.SandboxID, request genv1.CommandRequest) genv1.CommandResult {
	return genv1.CommandResult{Stdout: "\nyou said: " + request.Argv[4] + "\n"}
}

func daemon(t *testing.T, opts ...fakedaemon.Option) *fakedaemon.Daemon {
	t.Helper()
	return fakedaemon.New(t, append([]fakedaemon.Option{fakedaemon.WithKey("k"), fakedaemon.WithBrokers("claude"), fakedaemon.WithRun(claude)}, opts...)...)
}

func converse(t *testing.T, d *fakedaemon.Daemon, input string, extra ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"-addr", d.URL, "-key", "k", "-volume", "talk"}, extra...), strings.NewReader(input), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// Nothing runs between turns: every turn's sandbox was released — keeping the
// volume — and none is left.
func requireNothingRunning(t *testing.T, d *fakedaemon.Daemon, turns int) {
	t.Helper()
	if left := d.Sandboxes(); len(left) != 0 {
		t.Fatalf("%d sandboxes are still running", len(left))
	}
	drops := d.Deletes()
	if len(drops) != turns {
		t.Fatalf("%d sandboxes were dropped, want %d", len(drops), turns)
	}
	for _, drop := range drops {
		if drop.Disk != "keep" {
			t.Fatalf("%s was dropped with disk=%s, which deletes the conversation", drop.ID, drop.Disk)
		}
	}
}

func TestEachTurnIsASandboxOnTheSameVolume(t *testing.T) {
	var mu sync.Mutex
	var volumes []string
	d := daemon(t, fakedaemon.WithBoot(func(spec genv1.SandboxSpec) fakedaemon.Outcome {
		mu.Lock()
		defer mu.Unlock()
		volumes = append(volumes, *spec.Disk.Volume)
		return fakedaemon.Outcome{}
	}))
	code, stdout, stderr := converse(t, d, "make a file\n  what did I ask?  \n")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{
		"a new conversation, on volume talk",
		"claude> you said: make a file",
		"claude> you said: what did I ask?",
		"· volume holds 7.5 MB]",
		"· volume holds 15.0 MB]",
		"the conversation is kept: -volume talk picks it up again",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("did not say %q:\n%s", want, stdout)
		}
	}
	if len(volumes) != 2 || volumes[0] != "talk" || volumes[1] != "talk" {
		t.Fatalf("the turns named volumes %v", volumes)
	}
	command := d.Commands()[0]
	if command.Argv[2] != turnScript || *command.TimeoutSeconds != 600 {
		t.Fatalf("the turn ran %+v", command)
	}
	requireNothingRunning(t, d, 2)
	if exists, _ := d.Volume("talk"); !exists {
		t.Fatal("the conversation's volume is gone")
	}
}

func TestAVolumeThatExistsIsPickedUpAndANewOneIsNamed(t *testing.T) {
	d := daemon(t)
	d.PutVolume("talk", 1<<20)
	if _, stdout, _ := converse(t, d, ""); !strings.Contains(stdout, "picking up the conversation on volume talk") {
		t.Fatalf("an existing volume was not picked up:\n%s", stdout)
	}

	var stdout bytes.Buffer
	if code := run([]string{"-addr", d.URL, "-key", "k"}, strings.NewReader("hi\n"), &stdout, io.Discard); code != 0 {
		t.Fatalf("exit %d", code)
	}
	var named string
	for line := range strings.SplitSeq(stdout.String(), "\n") {
		if rest, ok := strings.CutPrefix(line, "a new conversation, on volume "); ok {
			named = rest
		}
	}
	if len(named) != len("chat-")+8 || !strings.HasPrefix(named, "chat-") {
		t.Fatalf("a new conversation was put on volume %q", named)
	}
	if exists, _ := d.Volume(named); !exists {
		t.Fatalf("the turn did not use %s", named)
	}
}

// Blank lines are not turns, /exit ends the conversation where it is, and
// nothing after it is read as a prompt.
func TestBlankLinesAreSkippedAndExitLeaves(t *testing.T) {
	d := daemon(t)
	code, stdout, stderr := converse(t, d, "\n   \n\t\nhello\n  /exit  \nnot a turn\n")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if calls := d.Calls("createSandbox"); calls != 1 {
		t.Fatalf("%d turns were taken, want the one before /exit", calls)
	}
	if strings.Contains(stdout, "not a turn") {
		t.Fatalf("a line after /exit was answered:\n%s", stdout)
	}
	requireNothingRunning(t, d, 1)
}

func TestResetForgetsEverything(t *testing.T) {
	d := daemon(t)
	code, stdout, stderr := converse(t, d, "remember banana\n/reset\n")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stdout, "forgotten: the next turn starts from an empty talk") {
		t.Fatalf("reset was not said:\n%s", stdout)
	}
	if exists, _ := d.Volume("talk"); exists {
		t.Fatal("the volume survived /reset")
	}
}

// A /reset the daemon refuses leaves the volume as it was, and the
// conversation goes on. It used to end the program.
func TestARefusedResetDoesNotEndTheConversation(t *testing.T) {
	d := daemon(t)
	d.Intercept("deleteVolume", fakedaemon.Refuse(409, genv1.ErrorCodeVolumeInUse, "held by a sandbox"))
	code, stdout, stderr := converse(t, d, "/reset\nstill here?\n")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "nothing was forgotten") || !strings.Contains(stderr, "held by a sandbox") {
		t.Fatalf("the refusal was not said: %q", stderr)
	}
	if !strings.Contains(stdout, "claude> you said: still here?") {
		t.Fatalf("the conversation did not go on:\n%s", stdout)
	}
}

// One turn failing is not the conversation failing, however it fails, and
// its sandbox is released each time so the next turn can have the volume.
func TestAFailedTurnIsSaidAndTheNextOneStillRuns(t *testing.T) {
	failFirst := func() fakedaemon.Option {
		var once sync.Once
		return fakedaemon.WithBoot(func(genv1.SandboxSpec) fakedaemon.Outcome {
			failed := false
			once.Do(func() { failed = true })
			if failed {
				return fakedaemon.Outcome{State: genv1.SandboxStateFailed, Error: "the volume belongs to another image"}
			}
			return fakedaemon.Outcome{}
		})
	}
	exitFirst := func() fakedaemon.Option {
		var once sync.Once
		return fakedaemon.WithRun(func(ctx context.Context, id genv1.SandboxID, request genv1.CommandRequest) genv1.CommandResult {
			failed := false
			once.Do(func() { failed = true })
			if failed {
				return genv1.CommandResult{ExitCode: 1, Stdout: "API Error: ", Stderr: "overloaded"}
			}
			return claude(ctx, id, request)
		})
	}
	for name, tc := range map[string]struct {
		opts      []fakedaemon.Option
		intercept map[string]fakedaemon.Interceptor
		want      string
		drops     int
	}{
		// The failed sandbox holds the volume like any other, and used to be
		// left holding it: every turn after it was `volume_in_use`.
		"the sandbox fails":     {opts: []fakedaemon.Option{failFirst()}, want: "the volume belongs to another image", drops: 2},
		"the create is refused": {intercept: map[string]fakedaemon.Interceptor{"createSandbox": fakedaemon.Refuse(507, genv1.ErrorCodeCapacityExceeded, "eight of eight")}, want: "capacity_exceeded", drops: 1},
		"claude exits non-zero": {opts: []fakedaemon.Option{exitFirst()}, want: "claude exited 1: API Error: overloaded", drops: 2},
		"the command is lost":   {intercept: map[string]fakedaemon.Interceptor{"runCommand": fakedaemon.HangUp()}, want: "asking Claude", drops: 2},
	} {
		t.Run(name, func(t *testing.T) {
			d := daemon(t, tc.opts...)
			for op, intercept := range tc.intercept {
				d.Intercept(op, intercept)
			}
			code, stdout, stderr := converse(t, d, "first\nsecond\n")
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if !strings.Contains(stderr, "that turn failed: ") || !strings.Contains(stderr, tc.want) {
				t.Fatalf("stderr %q does not say %q", stderr, tc.want)
			}
			if !strings.Contains(stdout, "claude> you said: second") {
				t.Fatalf("the turn after the failure did not run:\n%s", stdout)
			}
			requireNothingRunning(t, d, tc.drops)
		})
	}
}

func TestAReleaseThatFailsIsTheTurnsFailure(t *testing.T) {
	d := daemon(t)
	d.Intercept("deleteSandbox", fakedaemon.Refuse(500, genv1.ErrorCodeInternal, "the ruleset would not come down"))
	code, _, stderr := converse(t, d, "hi\n")
	if code != 0 || !strings.Contains(stderr, "that turn failed: dropping") || !strings.Contains(stderr, "the ruleset would not come down") {
		t.Fatalf("exit %d: %q", code, stderr)
	}
}

func TestForgetDeletesTheVolumeOnTheWayOut(t *testing.T) {
	d := daemon(t)
	code, stdout, stderr := converse(t, d, "hi\n", "-forget")
	if code != 0 || !strings.Contains(stdout, "volume talk deleted") {
		t.Fatalf("exit %d: %s %s", code, stdout, stderr)
	}
	if exists, _ := d.Volume("talk"); exists {
		t.Fatal("-forget kept the volume")
	}

	refused := daemon(t)
	refused.Intercept("deleteVolume", fakedaemon.Refuse(409, genv1.ErrorCodeVolumeInUse, "held"))
	if code, _, stderr := converse(t, refused, "hi\n", "-forget"); code != 1 || !strings.Contains(stderr, "deleting volume talk") {
		t.Fatalf("a refused -forget: exit %d: %s", code, stderr)
	}
}

// Ctrl-C at the prompt ends the conversation. It used to do nothing until
// the person also pressed Enter: the read of the line was not interruptible.
func TestCtrlCAtThePromptLeaves(t *testing.T) {
	d := daemon(t)
	input, typing := io.Pipe()
	defer func() { _ = typing.Close() }()
	ctx, cancel := context.WithCancel(t.Context())
	stdout := &lockedBuffer{written: make(chan struct{}, 1)}
	done := make(chan error, 1)
	spawn.Go(t, func() {
		done <- chat(ctx, options{addr: d.URL, key: "k", image: "claude", volume: "talk"}, input, stdout, io.Discard)
	})
	// The prompt is written, and nothing is typed.
	for !strings.Contains(stdout.String(), "you> ") {
		select {
		case err := <-done:
			t.Fatalf("the conversation ended before the prompt: %v", err)
		case <-stdout.written:
		}
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Ctrl-C at the prompt = %v, want a quiet exit", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Ctrl-C at the prompt did not end the conversation")
	}
}

// Ctrl-C in the middle of a turn ends the conversation, and the turn's
// sandbox is still released.
func TestCtrlCInATurnReleasesItsSandbox(t *testing.T) {
	asked := make(chan struct{})
	d := daemon(t, fakedaemon.WithRun(func(ctx context.Context, _ genv1.SandboxID, _ genv1.CommandRequest) genv1.CommandResult {
		close(asked)
		<-ctx.Done()
		return genv1.CommandResult{}
	}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	spawn.Go(t, func() {
		done <- chat(ctx, options{addr: d.URL, key: "k", image: "claude", volume: "talk"}, strings.NewReader("think hard\n"), io.Discard, io.Discard)
	})
	select {
	case <-asked:
	case err := <-done:
		t.Fatalf("the conversation ended before the turn asked: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Ctrl-C in a turn = %v, want a quiet exit", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Ctrl-C in a turn did not end the conversation")
	}
	requireNothingRunning(t, d, 1)
}

// A line longer than the reader holds is an error that says what it was
// doing, rather than a bare "token too long".
func TestALineTooLongToReadEndsWithAnError(t *testing.T) {
	d := daemon(t)
	code, _, stderr := converse(t, d, strings.Repeat("x", 2<<20)+"\n")
	if code != 1 || !strings.Contains(stderr, "reading a line: bufio.Scanner: token too long") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if calls := d.Calls("createSandbox"); calls != 0 {
		t.Fatalf("%d turns were taken from a line that could not be read", calls)
	}
}

func TestAHostThatCannotServeTheConversationIsToldSo(t *testing.T) {
	noBroker := fakedaemon.New(t, fakedaemon.WithKey("k"))
	if code, _, stderr := converse(t, noBroker, "hi\n"); code != 1 || !strings.Contains(stderr, `declares no shared broker "claude"`) {
		t.Fatalf("a host with no claude broker: exit %d: %s", code, stderr)
	}
	refused := daemon(t)
	refused.Intercept("getInfo", fakedaemon.Refuse(403, genv1.ErrorCodeForbidden, "no"))
	if code, _, stderr := converse(t, refused, "hi\n"); code != 1 || !strings.Contains(stderr, "asking the host what it is") {
		t.Fatalf("a host that will not say what it is: exit %d: %s", code, stderr)
	}
	other := fakedaemon.New(t, fakedaemon.WithKey("k"), fakedaemon.WithBrokers("github"))
	if code, _, _ := converse(t, other, "hi\n"); code != 1 {
		t.Fatalf("a host whose brokers do not include claude: exit %d", code)
	}
}

// What the volume holds is said when it can be asked, and left out when it
// cannot, rather than failing the turn over a figure.
func TestTheVolumesSizeIsLeftOutWhenItCannotBeAsked(t *testing.T) {
	d := daemon(t)
	d.InterceptAll("listVolumes", fakedaemon.Refuse(403, genv1.ErrorCodeForbidden, "no"))
	code, stdout, stderr := converse(t, d, "hi\n")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.Contains(stdout, "volume holds") || !strings.Contains(stdout, "dropped in") {
		t.Fatalf("the turn's summary is wrong:\n%s", stdout)
	}
}

func TestDurationsAreShortWhereShortIsHonest(t *testing.T) {
	for d, want := range map[time.Duration]string{
		0:                          "0s",
		1234567 * time.Microsecond: "1.235s",
		12345 * time.Millisecond:   "12.3s",
		10 * time.Second:           "10s",
	} {
		if got := ms(d); got != want {
			t.Errorf("ms(%s) = %q, want %q", d, got, want)
		}
	}
	if got := seconds(1500 * time.Millisecond); got == nil || *got != 2 {
		t.Errorf("seconds(1.5s) = %v, want 2", got)
	}
	if got := seconds(0); got != nil {
		t.Errorf("seconds(0) = %d, want nil: the host's default", *got)
	}
}

func TestTheFlagsAreTheProgramsInterface(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-h"}, strings.NewReader(""), &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "-forget") {
		t.Errorf("-h: exit %d, %s", code, stderr.String())
	}
	if code := run([]string{"-forget=maybe"}, strings.NewReader(""), &stdout, &stderr); code != 2 {
		t.Errorf("a bad boolean: exit %d, want 2", code)
	}
	stderr.Reset()
	if code := run([]string{"-addr", "localhost:8099"}, strings.NewReader(""), &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "not a daemon's address") {
		t.Errorf("an address with no scheme: exit %d, %s", code, stderr.String())
	}
}

// lockedBuffer is stdout for a conversation on another goroutine, and says
// each time something is written to it.
type lockedBuffer struct {
	mu      sync.Mutex
	b       bytes.Buffer
	written chan struct{}
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case l.written <- struct{}{}:
	default:
	}
	return l.b.Write(p)
}

func (l *lockedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
