package main

import (
	"bytes"
	"context"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakedaemon"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// guest is how a sandbox answers curl: a host its rules allow is reached, and
// one they do not is refused by the host — which writes it down.
type guest int

const (
	// asTheRulesSay is a sandbox on a host that enforces its rules.
	asTheRulesSay guest = iota
	// reachingNothing is a sandbox on a host whose way out is broken.
	reachingNothing
	// reachingEverything is a sandbox on a host that enforces nothing.
	reachingEverything
	// failingUnrecorded is curl failing for a reason that is not the host's.
	failingUnrecorded
)

func daemon(t *testing.T, behaves guest, opts ...fakedaemon.Option) *fakedaemon.Daemon {
	t.Helper()
	var d *fakedaemon.Daemon
	curl := func(_ context.Context, id genv1.SandboxID, request genv1.CommandRequest) genv1.CommandResult {
		if request.Argv[0] != "curl" {
			return genv1.CommandResult{ExitCode: 127, Stderr: "not found"}
		}
		target, err := url.Parse(request.Argv[len(request.Argv)-1])
		if err != nil {
			return genv1.CommandResult{ExitCode: 3, Stderr: "curl: (3) URL rejected"}
		}
		host := target.Hostname()
		allowed := slices.ContainsFunc(d.Rules(id), func(rule genv1.EgressRule) bool {
			return rule.Domains != nil && slices.Contains(*rule.Domains, host)
		})
		switch {
		case behaves == reachingEverything, behaves == asTheRulesSay && allowed:
			if host == "httpbin.org" {
				return genv1.CommandResult{Stdout: "{\n  \"origin\": \"203.0.113.7\"\n}\n"}
			}
			return genv1.CommandResult{Stdout: "203.0.113.7"}
		case behaves == failingUnrecorded:
			return genv1.CommandResult{ExitCode: 28, Stderr: "curl: (28) Connection timed out"}
		}
		if behaves == asTheRulesSay {
			d.RecordRefusal(id, genv1.EgressRefusal{At: time.Now().UTC(), Target: host, Kind: new(genv1.Dns), Reason: new("no rule allows it")})
		}
		return genv1.CommandResult{ExitCode: 6, Stderr: "curl: (6) Could not resolve host: " + host}
	}
	d = fakedaemon.New(t, append([]fakedaemon.Option{fakedaemon.WithKey("k"), fakedaemon.WithNetwork(), fakedaemon.WithRun(curl)}, opts...)...)
	return d
}

func example(t *testing.T, d *fakedaemon.Daemon, extra ...string) (int, string, string) {
	t.Helper()
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"-addr", d.URL, "-key", "k"}, extra...), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

// Nothing is left on the host, whatever happened.
func requireDropped(t *testing.T, d *fakedaemon.Daemon) {
	t.Helper()
	if left := d.Sandboxes(); len(left) != 0 {
		t.Fatalf("%d sandboxes were left on the host", len(left))
	}
}

func TestTheExampleReachesWhatItsRulesAllowAndIsRefusedTheRest(t *testing.T) {
	var spec genv1.SandboxSpec
	d := daemon(t, asTheRulesSay, fakedaemon.WithBoot(func(asked genv1.SandboxSpec) fakedaemon.Outcome {
		spec = asked
		return fakedaemon.Outcome{}
	}))
	code, stdout, stderr := example(t, d)
	if code != 0 {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	// Made with the one rule, and no entrypoint of the image's.
	if spec.Image != "curlimages/curl:8.11.1" || spec.Entrypoint == nil || len(*spec.Entrypoint) != 0 ||
		spec.Egress == nil || len(*spec.Egress.Rules) != 1 || (*spec.Egress.Rules)[0].Name != "ifconfig" {
		t.Fatalf("made with %+v", spec)
	}
	for _, said := range []string{
		"one egress rule: ifconfig allows ifconfig.me",
		"$ curl https://ifconfig.me/ip\n  203.0.113.7\n",
		"$ curl https://httpbin.org/ip\n  curl: (6) Could not resolve host: httpbin.org\n  the host refused httpbin.org, by its resolver: no rule allows it\n",
		"adding the rule httpbin: httpbin.org, on the web ports\n$ curl https://httpbin.org/ip\n  {\n    \"origin\": \"203.0.113.7\"\n  }\n",
		"deleting the rule httpbin\n$ curl https://httpbin.org/ip\n  curl: (6)",
		"dropped.\n",
	} {
		if !strings.Contains(stdout, said) {
			t.Errorf("it does not say %q:\n%s", said, stdout)
		}
	}
	// Every curl is one command, with a deadline of its own.
	for _, command := range d.Commands() {
		if !slices.Contains(command.Argv, "--max-time") {
			t.Errorf("curl without a deadline: %q", command.Argv)
		}
	}
	if len(d.Commands()) != 4 {
		t.Errorf("%d commands, want 4", len(d.Commands()))
	}
	requireDropped(t, d)
}

func TestAHostThatGivesNoNetworkIsSaidAndNothingIsMade(t *testing.T) {
	d := fakedaemon.New(t, fakedaemon.WithKey("k"))
	code, _, stderr := example(t, d)
	if code != 1 || !strings.Contains(stderr, "-network=false") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	if d.Calls("createSandbox") != 0 {
		t.Fatal("a sandbox was asked for on a host that gives it no network")
	}
}

// The example checks what it shows: a rule that does not open its name, or
// a name opened with no rule, is a failure — never a tour that looks right.
func TestWhatTheRulesDoNotExplainIsAFailure(t *testing.T) {
	for behaves, want := range map[guest]string{
		reachingNothing:    "curl https://ifconfig.me/ip was refused, though a rule allows it: exit 6",
		reachingEverything: "curl https://httpbin.org/ip was reached, though no rule allows it",
		failingUnrecorded:  "curl https://ifconfig.me/ip was refused, though a rule allows it: exit 28",
	} {
		d := daemon(t, behaves)
		code, _, stderr := example(t, d)
		if code != 1 || !strings.Contains(stderr, want) {
			t.Errorf("exit %d: %s, want %q", code, stderr, want)
		}
		requireDropped(t, d)
	}
}

// A refusal the host has no record of is curl failing for some other reason.
func TestARefusalTheHostDidNotMakeIsSaid(t *testing.T) {
	d := daemon(t, asTheRulesSay)
	d.InterceptAll("getSandboxEgress", func(w http.ResponseWriter, _ *http.Request, _ http.HandlerFunc) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"policy":{},"interface":{"tap":"t","guest":"g","host":"h","mask":30},"resolved":[],"rules":[],"connections":[],"refusals":[{"at":"2026-09-28T12:00:00Z","target":"other.example"}],"truncated":false}`))
	})
	code, _, stderr := example(t, d)
	if code != 1 || !strings.Contains(stderr, "has no refusal of httpbin.org: it failed for another reason") {
		t.Fatalf("exit %d: %s", code, stderr)
	}
	requireDropped(t, d)
}

func TestEachRefusalOfTheDaemonsIsTheExamplesFailure(t *testing.T) {
	for operation, want := range map[string]string{
		"getInfo":          "asking the host what it is",
		"putEgressRule":    "adding the rule httpbin",
		"deleteEgressRule": "deleting the rule httpbin",
		"getSandboxEgress": "reading its egress",
		"runCommand":       "sandbox_unreachable",
		"deleteSandbox":    "dropping the sandbox",
	} {
		t.Run(operation, func(t *testing.T) {
			d := daemon(t, asTheRulesSay)
			d.InterceptAll(operation, fakedaemon.Refuse(http.StatusServiceUnavailable, "sandbox_unreachable", "it stopped answering"))
			code, _, stderr := example(t, d)
			if code != 1 || !strings.Contains(stderr, want) {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if operation != "deleteSandbox" {
				requireDropped(t, d)
			}
		})
	}
}

func TestASandboxThatFailsToBootIsDroppedAndSaid(t *testing.T) {
	d := daemon(t, asTheRulesSay, fakedaemon.WithBoot(func(genv1.SandboxSpec) fakedaemon.Outcome {
		return fakedaemon.Outcome{State: genv1.SandboxStateFailed, Error: "pulling curlimages/curl: not found"}
	}))
	code, stdout, stderr := example(t, d)
	if code != 1 || !strings.Contains(stderr, "not found") || !strings.Contains(stdout, "dropped.") {
		t.Fatalf("exit %d: %s\n%s", code, stderr, stdout)
	}
	requireDropped(t, d)
}

func TestFlagsAreReadAndRefused(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if code := run([]string{"-h"}, &stdout, &stderr); code != 0 {
		t.Errorf("-h = %d", code)
	}
	if code := run([]string{"-nope"}, &stdout, &stderr); code != 2 {
		t.Errorf("an unknown flag = %d", code)
	}
	stderr.Reset()
	if code := run([]string{"-addr", "127.0.0.1:8099"}, &stdout, &stderr); code != 1 || !strings.Contains(stderr.String(), "http://") {
		t.Errorf("an address with no scheme = %d: %s", code, stderr.String())
	}
	var spec genv1.SandboxSpec
	d := daemon(t, asTheRulesSay, fakedaemon.WithBoot(func(asked genv1.SandboxSpec) fakedaemon.Outcome {
		spec = asked
		return fakedaemon.Outcome{}
	}))
	if code, _, stderr := example(t, d, "-image", "registry.example/curl:1"); code != 0 || spec.Image != "registry.example/curl:1" {
		t.Errorf("-image = %d %s, made from %q", code, stderr, spec.Image)
	}
}

func TestIndentSaysNothingForNothing(t *testing.T) {
	if got := indent(" \n"); got != "  (nothing)" {
		t.Fatalf("%q", got)
	}
}
