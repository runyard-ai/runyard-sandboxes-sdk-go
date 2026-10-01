// Command ryclaude is Claude Code in your terminal, running in a sandbox.
//
// It looks and answers as `claude` does — the same screen, the same keys,
// Ctrl-C included — because it IS `claude`, in a PTY inside a microVM, with
// this terminal carried to it and back. Everything Claude does, it does
// there: the files it reads and edits are the sandbox's, the commands it runs
// are the sandbox's, and this machine is a screen and a keyboard.
//
//	ryclaude -addr https://sandboxes.example.com -allow github.com
//	ryclaude -- --model opus        # what follows -- is claude's
//
// A sandbox is made from ryclaude's image (image/, which CI publishes where
// anyone can pull it), claude runs in it on this terminal until it exits, and
// the sandbox is dropped.
//
// Your Claude credentials are copied into the sandbox, which reaches
// Anthropic's domains itself. That is temporary: a broker on the host is to
// carry claude's calls, and keep the credential out of the sandbox. Until
// then it is no worse than running claude on your own laptop, where the same
// credentials are in reach of everything claude runs.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sdk"
)

type options struct {
	addr, key, image, credentials string
	// allow is what the sandbox may reach besides Anthropic.
	allow []string
	// What claude is given: everything after the flags.
	args []string
}

// anthropic is what claude reaches to work: the API, and where a session's
// token is renewed.
var anthropic = []string{"api.anthropic.com", "platform.claude.com", "console.anthropic.com", "claude.ai"}

// Where claude starts in the sandbox, and where what it needs goes: root's
// home, the image running as root — the machine is the boundary, not a
// user inside it.
const (
	// What each release of runyard-sandboxes publishes, in a namespace anyone
	// pulls from with no key. There, latest is the highest version published.
	defaultImage = "releases.runyard.ai/runyard-public/ryclaude:latest"

	workdir    = "/workspace"
	loginPath  = "/root/.claude/.credentials.json"
	configPath = "/root/.claude.json"
)

func main() { os.Exit(run(context.Background(), os.Args[1:], terminal(), os.Stderr)) }

func run(ctx context.Context, args []string, con console, stderr io.Writer) int {
	var o options
	var allow string
	// Without a home there is no default, and -credentials says where.
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("ryclaude", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&o.addr, "addr", envOr("RUNYARD_SANDBOXES_ADDR", "http://127.0.0.1:8099"), "the daemon")
	flags.StringVar(&o.key, "key", os.Getenv("RUNYARD_SANDBOXES_KEY"), "an API key. Mint one with: runyard-sandboxes keys mint")
	flags.StringVar(&o.image, "image", envOr("RYCLAUDE_IMAGE", defaultImage), "an image with claude in it")
	flags.StringVar(&o.credentials, "credentials", filepath.Join(home, ".claude", ".credentials.json"), "your Claude credentials, copied into the sandbox")
	flags.StringVar(&allow, "allow", "", "domains the sandbox may reach besides Anthropic's, comma-separated, e.g. github.com")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	for domain := range strings.SplitSeq(allow, ",") {
		if domain = strings.TrimSpace(domain); domain != "" {
			o.allow = append(o.allow, domain)
		}
	}
	o.args = flags.Args()

	// Ctrl-C is not here: the terminal is raw, and it reaches claude as the
	// key it is. These are the signals that are this program's own.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	code, err := session(ctx, o, con, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "ryclaude: %v\n", err)
		return 1
	}
	return code
}

func envOr(name, otherwise string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return otherwise
}

// session is one claude, in one sandbox, and its exit code.
func session(ctx context.Context, o options, con console, stderr io.Writer) (code int, err error) {
	client, err := sdk.New(o.addr, sdk.WithKey(o.key))
	if err != nil {
		return 0, err
	}
	// Read before anything is made: a sandbox claude cannot log in from is one
	// made for nothing.
	credentials, err := os.ReadFile(o.credentials)
	if err != nil {
		return 0, fmt.Errorf("reading your credentials: %w: log in here first, with claude, or say where they are with -credentials", err)
	}

	reach := append(slices.Clone(anthropic), o.allow...)
	spec := sdk.Spec{
		Image:  o.image,
		Egress: &genv1.EgressSpec{Rules: &[]genv1.EgressRule{{Name: "allowed", Domains: &reach}}},
		Env: &map[string]string{
			// It is one: this is what lets claude, as root, be told to skip
			// its permission prompts.
			"IS_SANDBOX": "1",
			// The version is the image's, and nothing but the API is reachable:
			// saying so stops claude trying, and waiting, on every start.
			"DISABLE_AUTOUPDATER":                      "1",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
			"TERM":      envOr("TERM", "xterm-256color"),
			"COLORTERM": os.Getenv("COLORTERM"),
			"LANG":      "C.UTF-8",
		},
	}
	fmt.Fprintf(stderr, "ryclaude: making a sandbox from %s …\n", o.image)
	sandbox, err := client.Create(ctx, "ryclaude", spec)
	if sandbox != nil {
		// Dropped whatever happens, on a context of its own: ctx may be the
		// one a signal ended.
		defer func() {
			drop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if closeErr := sandbox.Close(drop); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("dropping the sandbox %s: %w", sandbox.ID, closeErr))
			}
		}()
	}
	if err != nil {
		return 0, err
	}
	if err := install(ctx, sandbox, credentials); err != nil {
		return 0, err
	}
	return attach(ctx, sandbox, o.args, con)
}

// install puts in the sandbox the credentials claude logs in with, and a
// configuration that does not ask what a person already answered here: the
// onboarding, and whether to trust the directory claude starts in.
//
// The credentials are yours, copied in, and whatever runs in the sandbox can
// read them. That is temporary: a broker on the host is to carry claude's
// calls and hold the credential there. Until then it is no worse than running
// claude on your own laptop, where the same file is in reach of everything
// claude runs.
func install(ctx context.Context, sandbox *sdk.Sandbox, credentials []byte) error {
	config, err := json.Marshal(map[string]any{
		"hasCompletedOnboarding": true,
		"theme":                  "dark",
		"projects":               map[string]any{workdir: map[string]any{"hasTrustDialogAccepted": true}},
	})
	if err != nil {
		return err
	}
	if err := sandbox.WriteFile(ctx, configPath, config, "0600"); err != nil {
		return fmt.Errorf("writing claude's configuration: %w", err)
	}
	if err := sandbox.WriteFile(ctx, loginPath, credentials, "0600"); err != nil {
		return fmt.Errorf("copying your credentials in: %w", err)
	}
	result, err := sandbox.Run(ctx, "mkdir", "-p", workdir)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("mkdir -p %s: exit %d: %s", workdir, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	return nil
}

// attach runs claude in a terminal in the sandbox, with this one carried to it,
// until it exits; and is its exit code.
func attach(ctx context.Context, sandbox *sdk.Sandbox, args []string, con console) (int, error) {
	cols, rows := con.size()
	// A shell, to start in the directory; then claude in its place, with what
	// it was given.
	argv := append([]string{"bash", "-c", `cd ` + workdir + ` && exec claude "$@"`, "claude"}, args...)
	// Its own context: a terminal is closed by ending the context it was
	// opened on, which is how a signal to this program hangs claude up.
	ctx, hangUp := context.WithCancel(ctx)
	defer hangUp()
	tty, err := sandbox.Terminal(ctx, sdk.TerminalOptions{Argv: argv, Cols: cols, Rows: rows})
	if err != nil {
		return 0, fmt.Errorf("starting claude: %w", err)
	}
	defer func() { _ = tty.Close() }()

	restore, err := con.raw()
	if err != nil {
		return 0, err
	}
	defer restore()

	resized, stopResizes := con.resizes()
	var carrying sync.WaitGroup
	carrying.Go(func() { //task:unowned joined by carrying.Wait, deferred below
		for {
			select {
			case <-ctx.Done():
				return
			case <-resized:
				// A resize lost is a screen drawn at the old size until the
				// next one: not worth ending claude over.
				_ = tty.Resize(con.size())
			}
		}
	})
	defer carrying.Wait()
	defer stopResizes()
	defer hangUp()

	//task:unowned a read of standard input cannot be interrupted: it ends with the process, which exits once claude has
	go func() { _, _ = io.Copy(tty, con) }()

	if _, err := io.Copy(con, tty); err != nil {
		return 0, err
	}
	code, ok := tty.ExitCode()
	if !ok {
		return 0, errors.New("the sandbox hung up before claude exited")
	}
	return code, nil
}
