// Command ryclaude is Claude Code in your terminal, running in a sandbox.
//
// It looks and answers as `claude` does — the same screen, the same keys,
// Ctrl-C included — because it IS `claude`, in a PTY inside a microVM, with
// this terminal carried to it and back. Everything Claude does, it does
// there: the files it reads and edits are the sandbox's, the commands it runs
// are the sandbox's, and this machine is a screen and a keyboard.
//
//	ryclaude auth login -url https://sandboxes.example.com   # once
//	ryclaude -allow github.com
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
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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
	// login is the daemon `ryclaude auth login` was given, when the key is
	// the one it got: where to log in again once the key stops working.
	login string
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
	// Where a daemon listens on its own host, with nothing said.
	defaultAddr = "http://127.0.0.1:8099"

	// What each release of runyard-sandboxes publishes, in a namespace anyone
	// pulls from with no key. There, latest is the highest version published.
	defaultImage = "releases.runyard.ai/runyard-public/ryclaude:latest"

	workdir    = "/workspace"
	loginPath  = "/root/.claude/.credentials.json"
	configPath = "/root/.claude.json"
)

// machine is what ryclaude asks of the machine it runs on: its terminal, and
// to log in, its name and its browser. main's is this one; a test's is not.
type machine struct {
	console
	hostname func() (string, error)
	// browse opens an address in the person's browser.
	browse func(ctx context.Context, address string) error
}

func thisMachine() machine {
	return machine{console: terminal(), hostname: os.Hostname, browse: browserOf(runtime.GOOS).open}
}

func main() { os.Exit(run(context.Background(), os.Args[1:], thisMachine(), os.Stderr)) }

func run(ctx context.Context, args []string, m machine, stderr io.Writer) int {
	// What is said here is often what a daemon said, and a terminal obeys
	// what it is shown.
	stderr = plain{stderr}
	// Before the flags, which stop at the first word that is not one: `auth`
	// is this program's, and claude's own is `ryclaude -- auth`.
	if len(args) > 0 && args[0] == "auth" {
		return authenticate(ctx, args[1:], m, stderr)
	}
	var o options
	var allow string
	// Without a home there is no default, and -credentials says where.
	home, _ := os.UserHomeDir()
	flags := flag.NewFlagSet("ryclaude", flag.ContinueOnError)
	flags.SetOutput(stderr)
	// Neither defaults to what the environment says, which is read once the
	// flags are: a default is printed by -h, and one of these is a key.
	flags.StringVar(&o.addr, "addr", "", "the daemon, as https://host, or http:// on this machine. Otherwise RUNYARD_SANDBOXES_ADDR, then the one `ryclaude auth login` was given, then "+defaultAddr)
	flags.StringVar(&o.key, "key", "", "an API key. Otherwise RUNYARD_SANDBOXES_KEY, then the one `ryclaude auth login` got")
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
	if err := o.reach(); err != nil {
		fmt.Fprintf(stderr, "ryclaude: %v\n", err)
		return 1
	}

	// Ctrl-C is not here: the terminal is raw, and it reaches claude as the
	// key it is. These are the signals that are this program's own.
	ctx, stop := signal.NotifyContext(ctx, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	code, err := session(ctx, o, m, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "ryclaude: %v\n", err)
		if refusal, ok := errors.AsType[*sdk.Error](err); ok && refusal.Status == http.StatusUnauthorized && o.login != "" {
			fmt.Fprintf(stderr, "ryclaude: %s no longer accepts the key `ryclaude auth login` got, which was revoked or has expired: run `ryclaude auth login -url %s` again\n", o.login, o.login)
		}
		return 1
	}
	return code
}

// reach settles which daemon is called, and with which key: what the flags
// say, then the environment, then what `ryclaude auth login` kept.
//
// The kept key and its daemon are one thing and are never taken apart. The
// key is sent to the daemon that issued it and to no other, whatever -addr
// says; and a key given here is never sent to the daemon a login named, which
// did not issue it — it goes where it went before there was a login.
//
// Whichever key it is, it is sent over TLS, or to this machine.
func (o *options) reach() error {
	if o.addr == "" {
		o.addr = os.Getenv("RUNYARD_SANDBOXES_ADDR")
	}
	if o.key == "" {
		o.key = os.Getenv("RUNYARD_SANDBOXES_KEY")
	}
	if o.key != "" {
		if o.addr == "" {
			o.addr = defaultAddr
		}
		// Held to what a login is: a key is a key whoever gave it, and in
		// the clear across a network it is anybody's on the way.
		address, err := daemonURL(o.addr)
		if err != nil {
			return fmt.Errorf("the key is not sent to the daemon -addr or RUNYARD_SANDBOXES_ADDR names: %w", err)
		}
		o.addr = address
		return nil
	}
	const otherwise = "give one with -key or RUNYARD_SANDBOXES_KEY"
	path, err := grantPath()
	if err != nil {
		return fmt.Errorf("no key: %w; %s", err, otherwise)
	}
	g, found, err := loadGrant(path)
	switch {
	case err != nil:
		return err
	case !found:
		return errors.New("no key: get one with `ryclaude auth login -url https://…`, naming your daemon, or " + otherwise)
	}
	// The address is not repeated: it is somebody's to have put a password
	// in.
	if address, err := daemonURL(o.addr); o.addr != "" && (err != nil || address != g.URL) {
		return fmt.Errorf("the key `ryclaude auth login` got is for %s, and is not sent to the daemon -addr or RUNYARD_SANDBOXES_ADDR names, which did not issue it: log in to that one with `ryclaude auth login -url`, or %s", g.URL, otherwise)
	}
	// Before anything is made: a sandbox the daemon refuses to make is the
	// same failure, said less clearly.
	if g.expired() {
		return fmt.Errorf("the key for %s expired on %s: run `ryclaude auth login -url %s` again", g.URL, when(g.ExpiresAt), g.URL)
	}
	o.addr, o.key, o.login = g.URL, g.Key, g.URL
	return nil
}

func envOr(name, otherwise string) string {
	if value := os.Getenv(name); value != "" {
		return value
	}
	return otherwise
}

// making says what the daemon is doing while the sandbox is made, as the
// daemon says it, so that a start that takes long is not a terminal saying
// nothing.
//
// The long one is the first from an image: the daemon pulls it and unpacks
// it, once, and every start after is from what it kept. That is said when it
// is seen to be happening — the daemon names each layer it fetches, and none
// of an image it has. Should it word that otherwise one day, what is lost is
// this one sentence, and not what the daemon says.
func making(stderr io.Writer) func(message string) {
	pulling := false
	return func(message string) {
		if !pulling && strings.HasPrefix(message, "layer ") {
			pulling = true
			fmt.Fprintln(stderr, "ryclaude:   this daemon has not run this image before: it pulls and unpacks it once, which can take a minute. The next start is quick.")
		}
		fmt.Fprintf(stderr, "ryclaude:   %s\n", message)
	}
}

// session is one claude, in one sandbox, and its exit code.
func session(ctx context.Context, o options, con console, stderr io.Writer) (code int, err error) {
	client, err := dial(o.addr, o.key, sdk.WithProgress(making(stderr)))
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
