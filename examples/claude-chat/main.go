// Command claude-chat is a conversation with Claude in which every turn is a
// new sandbox.
//
// A turn makes a machine, lets Claude answer and do whatever it wants to the
// filesystem, and drops the machine. What it wrote stays on a volume — the
// sandbox's disk, kept by name — and the next turn's sandbox names the same
// volume, so it boots with everything the last one left: the files Claude
// made, what it installed, and its own record of the conversation, which is
// how `claude --continue` picks the thread up.
//
// Nothing runs between turns. A conversation left for a week costs its disk
// and no memory, and a turn costs what a create costs whatever the volume
// holds, because the volume is attached rather than copied.
//
// Lines are read from standard input, so a script can be piped in. `/reset`
// forgets everything and `/exit` leaves; the volume is kept either way unless
// -forget is given, and -volume picks a conversation up again.
//
//	go run ./examples/claude-chat -key "$RUNYARD_SANDBOXES_KEY"
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
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

// broker is the shared broker each turn's sandbox is given, and the name the
// image's profile script looks for.
const broker = "claude"

// turnScript is what runs in each sandbox. The prompt is "$1", an argument
// and never part of the script, so a quote in it is a quote.
//
// `--continue` is used when the volume already holds a conversation, which
// Claude keeps under ~/.claude/projects: asking to continue one that does not
// exist is an error, and asking the volume is what makes -volume resume a
// conversation this program did not start.
//
// The sandbox is the boundary, so Claude is not asked before it writes or
// runs anything — that is what makes a turn able to do "whatever it wants".
// The CLI refuses that as root unless told it is in a sandbox, which it is.
const turnScript = `
continue=
if ls "$HOME"/.claude/projects/*/*.jsonl >/dev/null 2>&1; then continue=--continue; fi
IS_SANDBOX=1 exec claude -p $continue --dangerously-skip-permissions "$1"
`

type options struct {
	addr, key, image, volume string
	timeout                  time.Duration
	forget                   bool
}

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	var o options
	flags := flag.NewFlagSet("claude-chat", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&o.addr, "addr", "http://127.0.0.1:8099", "the daemon")
	flags.StringVar(&o.key, "key", os.Getenv("RUNYARD_SANDBOXES_KEY"), "an API key with sandboxes.read, sandboxes.write and exec")
	flags.StringVar(&o.image, "image", "localhost:5001/runyard/claude-sandbox:latest", "the image with the Claude CLI in it (examples/claude/image)")
	flags.StringVar(&o.volume, "volume", "", "the conversation's volume, to pick one up again; a new one when empty")
	flags.DurationVar(&o.timeout, "timeout", 10*time.Minute, "how long one turn has")
	flags.BoolVar(&o.forget, "forget", false, "delete the volume on the way out")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := chat(ctx, o, stdin, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "claude-chat: %v\n", err)
		return 1
	}
	return 0
}

func chat(ctx context.Context, o options, input io.Reader, stdout, stderr io.Writer) (err error) {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	client, err := sdk.New(o.addr, sdk.WithKey(o.key))
	if err != nil {
		return err
	}
	info, err := client.Info(ctx)
	if err != nil {
		return fmt.Errorf("asking the host what it is: %w", err)
	}
	if !declares(info, broker) {
		return fmt.Errorf("%s declares no shared broker %q; start the Claude broker and declare it (examples/claude/README.md)", info.Id, broker)
	}

	if o.volume == "" {
		o.volume = "chat-" + randomHex(4)
	}
	if holds(ctx, client, o.volume) != "" {
		fmt.Fprintf(stdout, "picking up the conversation on volume %s\n", o.volume)
	} else {
		fmt.Fprintf(stdout, "a new conversation, on volume %s\n", o.volume)
	}
	defer func() {
		if !o.forget {
			fmt.Fprintf(stdout, "\nthe conversation is kept: -volume %s picks it up again\n", o.volume)
			return
		}
		forgetting, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if forgetErr := client.DeleteVolume(forgetting, o.volume); forgetErr != nil {
			err = errors.Join(err, fmt.Errorf("deleting volume %s: %w", o.volume, forgetErr))
			return
		}
		fmt.Fprintf(stdout, "\nvolume %s deleted\n", o.volume)
	}()
	fmt.Fprintln(stdout, "type a message; /reset forgets everything, /exit leaves")

	lines, reading := readLines(ctx, input)
	for {
		fmt.Fprint(stdout, "\nyou> ")
		var line string
		select {
		case <-ctx.Done():
			// Ctrl-C at the prompt. Reading the line itself is not
			// interruptible, so it is waited for here instead: waiting in
			// Scan, a Ctrl-C did nothing until the person also pressed Enter.
			fmt.Fprintln(stdout)
			return nil
		case next, ok := <-lines:
			if !ok {
				fmt.Fprintln(stdout)
				if err := <-reading; err != nil {
					return fmt.Errorf("reading a line: %w", err)
				}
				return nil
			}
			line = next
		}
		prompt := strings.TrimSpace(line)
		switch prompt {
		case "":
			continue
		case "/exit":
			return nil
		case "/reset":
			if err := client.DeleteVolume(ctx, o.volume); err != nil {
				if ctx.Err() != nil {
					return nil
				}
				// Refused — held by a sandbox, say — the volume is as it was,
				// and so is the conversation: it goes on rather than ending
				// over a command that did nothing.
				fmt.Fprintf(stderr, "nothing was forgotten: deleting volume %s: %v\n", o.volume, err)
				continue
			}
			fmt.Fprintf(stdout, "forgotten: the next turn starts from an empty %s\n", o.volume)
			continue
		}
		if err := turn(ctx, client, o, prompt, stdout); err != nil {
			if ctx.Err() != nil {
				return nil
			}
			// One turn failing is not the conversation failing: the volume
			// holds what the turns before it wrote, and the next one can
			// still have it.
			fmt.Fprintf(stderr, "that turn failed: %v\n", err)
		}
	}
}

// readLines hands over input a line at a time until it ends, then closes
// lines and says on done why it ended.
//
// The one goroutine in this repository nobody ends. A read of a terminal is
// a blocking system call that neither a context nor closing the file
// interrupts, so an owner that waited for it would hold Ctrl-C at the prompt
// until the person also pressed Enter — the bug the channel is here to fix.
// It ends at the next line or the end of input, or with the process, which
// is what reading standard input means; whatever it reads once ctx has ended
// is dropped rather than acted on.
func readLines(ctx context.Context, input io.Reader) (lines <-chan string, done <-chan error) {
	out := make(chan string)
	ended := make(chan error, 1)
	//task:unowned a read of standard input cannot be interrupted, and ends with the process; see above
	go func() {
		defer close(out)
		scanner := bufio.NewScanner(input)
		scanner.Buffer(make([]byte, 64<<10), 1<<20)
		for scanner.Scan() {
			select {
			case out <- scanner.Text():
			case <-ctx.Done():
				ended <- nil
				return
			}
		}
		ended <- scanner.Err()
	}()
	return out, ended
}

// turn is one sandbox, from nothing to dropped, on the conversation's volume.
func turn(ctx context.Context, client *sdk.Client, o options, prompt string, stdout io.Writer) (err error) {
	started := time.Now()
	sandbox, err := client.Create(ctx, o.volume, sdk.Spec{
		Image:      o.image,
		Cpus:       new(2),
		Memory:     new("4Gi"),
		Entrypoint: &[]string{},
		Brokers:    &[]genv1.SandboxBrokerSpec{{Name: broker}},
		Disk:       &genv1.DiskSpec{Volume: new(o.volume)},
	})
	if err != nil {
		// A sandbox that failed holds its volume like any other, so one left
		// here made every later turn a `volume_in_use`: the conversation
		// could not go on, and nothing said why.
		if sandbox != nil {
			err = errors.Join(err, release(sandbox))
		}
		return err
	}
	ready := time.Since(started)

	var answered time.Duration
	// Released, not closed: Close would delete the volume along with the
	// sandbox. Deferred so that a failed turn or a Ctrl-C still lets the
	// volume go.
	defer func() {
		mark := time.Now()
		if releaseErr := release(sandbox); releaseErr != nil {
			err = errors.Join(err, releaseErr)
			return
		}
		fmt.Fprintf(stdout, "\n[sandbox %s · ready in %s · answered in %s · dropped in %s%s]\n",
			sandbox.ID, ms(ready), ms(answered), ms(time.Since(mark)), holds(ctx, client, o.volume))
	}()

	mark := time.Now()
	answer, err := sandbox.RunWith(ctx, genv1.CommandRequest{
		Argv:           []string{"bash", "-lc", turnScript, "claude", prompt},
		TimeoutSeconds: seconds(o.timeout),
	})
	answered = time.Since(mark)
	if err != nil {
		return fmt.Errorf("asking Claude: %w", err)
	}
	if answer.ExitCode != 0 {
		return fmt.Errorf("claude exited %d: %s%s", answer.ExitCode, answer.Stdout, answer.Stderr)
	}
	fmt.Fprintf(stdout, "\nclaude> %s\n", strings.TrimSpace(answer.Stdout))
	return nil
}

// release drops the sandbox and keeps the volume, on a context of its own:
// the turn's may be the one Ctrl-C just cancelled.
func release(sandbox *sdk.Sandbox) error {
	releasing, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := sandbox.Release(releasing); err != nil {
		return fmt.Errorf("dropping %s: %w", sandbox.ID, err)
	}
	return nil
}

// holds is what the volume has on the host now, which is what the
// conversation costs while nothing runs.
func holds(ctx context.Context, client *sdk.Client, name string) string {
	volumes, err := client.Volumes(ctx)
	if err != nil {
		return ""
	}
	for _, volume := range volumes {
		if volume.Name == name {
			return fmt.Sprintf(" · volume holds %.1f MB", float64(volume.AllocatedBytes)/1e6)
		}
	}
	return ""
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

// seconds is a turn's timeout as the contract takes it, where zero means the
// host's default. Rounded up: truncated, a -timeout under a second became
// zero, which is not "very short" but the host's default.
func seconds(d time.Duration) *int {
	if d <= 0 {
		return nil
	}
	return new(int((d + time.Second - 1) / time.Second))
}

func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func ms(d time.Duration) string {
	if d >= 10*time.Second {
		return d.Round(100 * time.Millisecond).String()
	}
	return d.Round(time.Millisecond).String()
}
