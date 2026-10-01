package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
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

// guest answers the tour's commands the way an alpine machine would.
func guest(_ context.Context, id genv1.SandboxID, request genv1.CommandRequest) genv1.CommandResult {
	script := strings.Join(request.Argv, " ")
	switch {
	case strings.Contains(script, "wget"):
		return genv1.CommandResult{Stdout: "you are runyard-sandbox://fake-host/" + id.String() + "\n"}
	case strings.Contains(script, "hostname"):
		return genv1.CommandResult{Stdout: "I am a sandbox, running 6.12.0\n"}
	}
	return genv1.CommandResult{ExitCode: 127, Stderr: "not found"}
}

func daemon(t *testing.T, opts ...fakedaemon.Option) *fakedaemon.Daemon {
	t.Helper()
	return fakedaemon.New(t, append([]fakedaemon.Option{fakedaemon.WithKey("k"), fakedaemon.WithBrokers("example"), fakedaemon.WithRun(guest)}, opts...)...)
}

func tour(t *testing.T, d *fakedaemon.Daemon, extra ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"-addr", d.URL, "-key", "k"}, extra...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// Nothing a failure or a success leaves behind: both machines are gone, and
// each was dropped with its disk.
func requireAllDropped(t *testing.T, d *fakedaemon.Daemon) {
	t.Helper()
	if left := d.Sandboxes(); len(left) != 0 {
		t.Fatalf("%d sandboxes were left on the host: %+v", len(left), left)
	}
	for _, drop := range d.Deletes() {
		if drop.Disk != "delete" {
			t.Fatalf("%s was dropped keeping its disk", drop.ID)
		}
	}
}

func TestTheTourMakesTwoSandboxesWorksInThemAndDropsBoth(t *testing.T) {
	var mu sync.Mutex
	hostnames := map[string]bool{}
	d := daemon(t, fakedaemon.WithBoot(func(spec genv1.SandboxSpec) fakedaemon.Outcome {
		mu.Lock()
		defer mu.Unlock()
		hostnames[*spec.Hostname] = true
		return fakedaemon.Outcome{}
	}))
	code, stdout, stderr := tour(t, d, "-broker", "example")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	for _, want := range []string{
		"host fake-host — cloud-hypervisor v53.0",
		"broker example → https://example.invalid:8443/",
		"two sandboxes ready in",
		"I am a sandbox, running 6.12.0",
		"/work/note.txt is 43 bytes and says: written from the host, read by the sandbox",
		"the host will attach runyard-sandbox://fake-host/",
		"the upstream answered: you are runyard-sandbox://fake-host/",
		"dropped hello-one",
		"dropped hello-two",
	} {
		if !strings.Contains(stdout, want) {
			t.Errorf("the tour did not say %q:\n%s", want, stdout)
		}
	}
	if !hostnames["hello-one"] || !hostnames["hello-two"] {
		t.Errorf("the hostnames sent were %v", hostnames)
	}
	commands := d.Commands()
	if len(commands) != 3 || commands[2].TimeoutSeconds == nil || *commands[2].TimeoutSeconds != 30 {
		t.Errorf("the commands run were %+v", commands)
	}
	requireAllDropped(t, d)
	if got := len(d.Deletes()); got != 2 {
		t.Fatalf("%d drops, want 2", got)
	}
}

func TestWithoutABrokerTheTourCallsNone(t *testing.T) {
	d := daemon(t)
	code, stdout, stderr := tour(t, d)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if strings.Contains(stdout, "calling") || len(d.Commands()) != 2 {
		t.Fatalf("a broker was called when none was named:\n%s", stdout)
	}
	requireAllDropped(t, d)
}

// hangUpOn hangs up on a command whose argv mentions word, and serves the
// rest.
func hangUpOn(word string) fakedaemon.Interceptor {
	return func(w http.ResponseWriter, r *http.Request, serve http.HandlerFunc) {
		body, _ := io.ReadAll(r.Body)
		if strings.Contains(string(body), word) {
			fakedaemon.HangUp()(w, r, serve)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		serve(w, r)
	}
}

// withoutBrokers answers a sandbox as though it had been made with none.
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

// Wherever the tour stops, both machines are dropped on the way out —
// including one whose create failed, which used to come back as nil and stay
// on the host.
func TestEveryWayTheTourFailsDropsBothSandboxes(t *testing.T) {
	failTwo := fakedaemon.WithBoot(func(spec genv1.SandboxSpec) fakedaemon.Outcome {
		if *spec.Hostname == "hello-two" {
			return fakedaemon.Outcome{State: genv1.SandboxStateFailed, Error: "the image has no /sbin/init"}
		}
		return fakedaemon.Outcome{}
	})
	exitOne := fakedaemon.WithRun(func(context.Context, genv1.SandboxID, genv1.CommandRequest) genv1.CommandResult {
		return genv1.CommandResult{ExitCode: 1, Stderr: "sh: hostname: not found"}
	})
	brokerFails := fakedaemon.WithRun(func(ctx context.Context, id genv1.SandboxID, request genv1.CommandRequest) genv1.CommandResult {
		if strings.Contains(strings.Join(request.Argv, " "), "wget") {
			return genv1.CommandResult{ExitCode: 1, Stderr: "wget: can't connect to remote host: Connection refused"}
		}
		return guest(ctx, id, request)
	})
	for name, tc := range map[string]struct {
		opts      []fakedaemon.Option
		intercept map[string]fakedaemon.Interceptor
		want      string
		created   int
	}{
		"a create fails":            {opts: []fakedaemon.Option{failTwo}, want: "creating hello-two: the image has no /sbin/init", created: 2},
		"a command is refused":      {intercept: map[string]fakedaemon.Interceptor{"runCommand": fakedaemon.Refuse(503, genv1.ErrorCodeSandboxUnreachable, "the agent is not answering")}, want: "running in", created: 2},
		"a command exits non-zero":  {opts: []fakedaemon.Option{exitOne}, want: "exit 1: sh: hostname: not found", created: 2},
		"the file is not written":   {intercept: map[string]fakedaemon.Interceptor{"writeFile": fakedaemon.Refuse(403, genv1.ErrorCodeForbidden, "no files.write")}, want: "writing a file", created: 2},
		"the file is not read back": {intercept: map[string]fakedaemon.Interceptor{"readFile": fakedaemon.Refuse(403, genv1.ErrorCodeForbidden, "no files.read")}, want: "reading it back", created: 2},
		"the broker is not given":   {intercept: map[string]fakedaemon.Interceptor{"getSandbox": withoutBrokers}, want: `was not given broker "example"`, created: 2},
		"the brokered call fails":   {opts: []fakedaemon.Option{brokerFails}, want: "the brokered call failed: wget", created: 2},
		"the brokered call is lost": {intercept: map[string]fakedaemon.Interceptor{"runCommand": hangUpOn("wget")}, want: "calling the broker", created: 2},
		"the host cannot be asked":  {intercept: map[string]fakedaemon.Interceptor{"getInfo": fakedaemon.Refuse(403, genv1.ErrorCodeForbidden, "no")}, want: "asking the host what it is", created: 0},
	} {
		t.Run(name, func(t *testing.T) {
			d := daemon(t, tc.opts...)
			for op, intercept := range tc.intercept {
				d.InterceptAll(op, intercept)
			}
			code, _, stderr := tour(t, d, "-broker", "example")
			if code != 1 || !strings.Contains(stderr, tc.want) {
				t.Fatalf("exit %d, stderr %q; want 1 and %q", code, stderr, tc.want)
			}
			requireAllDropped(t, d)
			if got := len(d.Deletes()); got != tc.created {
				t.Fatalf("%d sandboxes were dropped, want %d", got, tc.created)
			}
		})
	}
}

// A drop that fails is said, per sandbox, and does not stop the other one
// being dropped.
func TestADropThatFailsIsSaidAndTheOtherIsStillDropped(t *testing.T) {
	d := daemon(t)
	d.Intercept("deleteSandbox", fakedaemon.Refuse(500, genv1.ErrorCodeInternal, "the ruleset would not come down"))
	code, stdout, stderr := tour(t, d)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if !strings.Contains(stderr, "dropping hello-one") || !strings.Contains(stderr, "the ruleset would not come down") {
		t.Fatalf("the failed drop was not said: %q", stderr)
	}
	if !strings.Contains(stdout, "dropped hello-two") || len(d.Sandboxes()) != 1 {
		t.Fatalf("the second sandbox was not dropped: %s", stdout)
	}
}

// Ctrl-C in the middle of the tour cancels it, and both machines are still
// dropped — on a context Ctrl-C did not cancel.
func TestACancelledTourStillDropsBoth(t *testing.T) {
	running := make(chan struct{}, 2)
	d := daemon(t, fakedaemon.WithRun(func(ctx context.Context, _ genv1.SandboxID, _ genv1.CommandRequest) genv1.CommandResult {
		running <- struct{}{}
		<-ctx.Done()
		return genv1.CommandResult{}
	}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	spawn.Go(t, func() {
		done <- hello(ctx, options{addr: d.URL, key: "k", image: "alpine"}, io.Discard, io.Discard)
	})
	select {
	case <-running:
	case err := <-done:
		t.Fatalf("the tour ended before a command ran: %v", err)
	}
	cancel()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a cancelled tour said it succeeded")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the tour did not stop when it was cancelled")
	}
	requireAllDropped(t, d)
	if got := len(d.Deletes()); got != 2 {
		t.Fatalf("%d sandboxes were dropped, want 2", got)
	}
}

func TestTheFlagsAreTheProgramsInterface(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-h"}, &stdout, &stderr); code != 0 || !strings.Contains(stderr.String(), "-broker") {
		t.Errorf("-h: exit %d, %s", code, stderr.String())
	}
	if code := run([]string{"-no-such-flag"}, &stdout, &stderr); code != 2 {
		t.Errorf("an unknown flag: exit %d, want 2", code)
	}
	stderr.Reset()
	if code := run([]string{"-addr", "127.0.0.1:8099"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "not a daemon's address") {
		t.Errorf("an address with no scheme: exit %d, %s", code, stderr.String())
	}
}
