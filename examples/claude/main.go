// Command claude makes a sandbox with the Claude CLI in it, gives it the
// host's shared `claude` broker, asks Claude one thing, prints the answer, and
// drops the sandbox.
//
// Nothing in the machine can reach Anthropic on its own, and nothing in it
// holds a key: the CLI talks to a port on its own loopback, the connection is
// carried out over vsock to the daemon, and the daemon carries it to the Claude
// broker under this sandbox's certificate. The broker puts the real credential
// on — on the host — and forwards what the CLI asked for.
//
// With -keep the sandbox stays up afterwards, to be used from the console's
// terminal. See README.md beside this file for the image and the broker it
// needs.
//
//	go run ./examples/claude -key "$RUNYARD_SANDBOXES_KEY" -prompt "…"
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sdk"
)

// broker is the shared broker this example gives its sandbox, and the name the
// image's profile script looks for.
const broker = "claude"

type options struct {
	addr, key, image, name, console, prompt string
	timeout                                 time.Duration
	keep                                    bool
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	var o options
	flags := flag.NewFlagSet("claude", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&o.addr, "addr", "http://127.0.0.1:8099", "the daemon")
	flags.StringVar(&o.key, "key", os.Getenv("RUNYARD_SANDBOXES_KEY"), "an API key with sandboxes.read, sandboxes.write and exec")
	flags.StringVar(&o.image, "image", "localhost:5001/runyard/claude-sandbox:latest", "the image with the Claude CLI in it (examples/claude/image)")
	flags.StringVar(&o.name, "name", "claude", "the sandbox's name label")
	flags.StringVar(&o.prompt, "prompt", "In two sentences: what is a microVM, and why would someone run an AI agent inside one?", "what to ask Claude, inside the sandbox")
	flags.DurationVar(&o.timeout, "timeout", 5*time.Minute, "how long Claude has to answer")
	flags.StringVar(&o.console, "console", "", "where you open the console, if not at -addr — through an SSH tunnel, say")
	flags.BoolVar(&o.keep, "keep", false, "leave the sandbox running afterwards, to use from the console's terminal")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := ask(ctx, o, stdout); err != nil {
		fmt.Fprintf(stderr, "claude: %v\n", err)
		return 1
	}
	return 0
}

func ask(ctx context.Context, o options, stdout io.Writer) (err error) {
	client, err := sdk.New(o.addr, sdk.WithKey(o.key))
	if err != nil {
		return err
	}
	info, err := client.Info(ctx)
	if err != nil {
		return fmt.Errorf("asking the host what it is: %w", err)
	}
	if !declares(info, broker) {
		// Said before a machine is made, rather than discovered from a create
		// that is refused — and with what to do about it.
		return fmt.Errorf("%s declares no shared broker %q; start the Claude broker and declare it (examples/claude/README.md)", info.Id, broker)
	}

	started := time.Now()
	fmt.Fprintf(stdout, "making %q from %s …\n", o.name, o.image)
	created, err := client.Create(ctx, o.name, sdk.Spec{
		Image:  o.image,
		Cpus:   new(2),
		Memory: new("4Gi"),
		// Nothing of the image's own runs: Claude is started by exec, or by the
		// person in the console's terminal.
		Entrypoint: &[]string{},
		Brokers:    &[]genv1.SandboxBrokerSpec{{Name: broker}},
	})
	if err != nil {
		// A create that failed, timed out or was interrupted still hands back
		// the machine the daemon made, and -keep is no reason to keep one
		// that never became usable. It used to be left on the host, with
		// nothing printed to say it was there.
		if created != nil {
			err = errors.Join(err, drop(created, stdout))
		}
		return err
	}
	fmt.Fprintf(stdout, "ready in %s: %s\n", time.Since(started).Round(time.Millisecond), created.ID)

	if !o.keep {
		// Deferred, so that a failed prompt or a Ctrl-C in the middle of one
		// drops the machine too.
		defer func() { err = errors.Join(err, drop(created, stdout)) }()
	}

	// Where the broker is inside the machine is in the create's answer, so it
	// is said without running anything there: checking it from inside was a
	// second Node start, and 150ms of every run. A sandbox that cannot find its
	// broker still says so, in what `claude` prints when it fails.
	address, ok := created.Broker(broker)
	if !ok {
		return fmt.Errorf("the sandbox was made without the %q broker it was given", broker)
	}
	fmt.Fprintf(stdout, "inside it, Claude talks to %s — a port on the machine's own loopback\n", address)

	if o.prompt != "" {
		fmt.Fprintf(stdout, "\n> %s\n\n", o.prompt)
		// The prompt is an argument to the shell, not part of its script: "$1"
		// is never parsed, so a quote in it is a quote. A login shell, because
		// that is where the image points the CLI at the broker.
		answer, err := created.RunWith(ctx, genv1.CommandRequest{
			Argv:           []string{"bash", "-lc", `exec claude -p "$1"`, "claude", o.prompt},
			TimeoutSeconds: seconds(o.timeout),
		})
		if err != nil {
			return fmt.Errorf("asking Claude: %w", err)
		}
		if answer.ExitCode != 0 {
			return fmt.Errorf("claude exited %d after %s: %s%s", answer.ExitCode,
				time.Duration(answer.DurationMs)*time.Millisecond, answer.Stdout, answer.Stderr)
		}
		fmt.Fprintf(stdout, "%s\n\n(%s)\n", strings.TrimSpace(answer.Stdout), time.Duration(answer.DurationMs)*time.Millisecond)
	}

	if o.keep {
		console := o.console
		if console == "" {
			console = o.addr
		}
		fmt.Fprintf(stdout, "\nopen   %s/console#/sandboxes/%s\n", strings.TrimSuffix(console, "/"), created.ID)
		fmt.Fprintln(stdout, "then   Terminal → Open a terminal, and type: claude")
		fmt.Fprintf(stdout, "it stays up until it is deleted — from the console, or DELETE /v1/sandboxes/%s\n", created.ID)
	}
	return nil
}

// drop deletes the sandbox on a context of its own, because the caller's may
// be the one Ctrl-C just cancelled.
func drop(sandbox *sdk.Sandbox, stdout io.Writer) error {
	dropping, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sandbox.Close(dropping); err != nil {
		return fmt.Errorf("dropping %s: %w", sandbox.ID, err)
	}
	fmt.Fprintln(stdout, "dropped.")
	return nil
}

// seconds is a command's timeout as the contract takes it, where zero means
// the host's default. Rounded up: truncated, a -timeout under a second became
// zero, which is not "very short" but the host's default of a minute or more.
func seconds(d time.Duration) *int {
	if d <= 0 {
		return nil
	}
	return new(int((d + time.Second - 1) / time.Second))
}

func declares(info *genv1.HostInfo, name string) bool {
	if info.Brokers == nil {
		return false
	}
	for _, declared := range *info.Brokers {
		if declared.Name == name {
			return true
		}
	}
	return false
}
