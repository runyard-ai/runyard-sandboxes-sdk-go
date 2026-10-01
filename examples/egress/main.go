// Command egress shows a sandbox reaching what its rules allow, and nothing
// else — and the rules changing while it runs.
//
//  1. make a sandbox whose one egress rule, `ifconfig`, allows ifconfig.me
//  2. curl ifconfig.me from inside it: reached, and it says the address the
//     sandbox left through, which is its host's
//  3. curl httpbin.org: refused, because no rule allows it — the name does
//     not even resolve — and the host's report says so
//  4. add a rule, `httpbin`, and curl it again: reached
//  5. delete that rule by its name, and curl it again: refused
//  6. drop the sandbox
//
// Every refusal is the host's: the firewall and the resolver are outside the
// machine, where the workload — root inside it — cannot reach them.
//
//	go run ./examples/egress -addr http://127.0.0.1:8099 -key "$RUNYARD_SANDBOXES_KEY"
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

type options struct {
	addr, key, image string
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	var o options
	flags := flag.NewFlagSet("egress", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&o.addr, "addr", "http://127.0.0.1:8099", "the daemon")
	flags.StringVar(&o.key, "key", os.Getenv("RUNYARD_SANDBOXES_KEY"), "an API key with network.write. Mint one with: runyard-sandboxes keys mint")
	flags.StringVar(&o.image, "image", "curlimages/curl:8.11.1", "an OCI image with curl and a shell in it")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// Ctrl-C cancels rather than killing the program where it stands, so the
	// deferred drop still runs.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()

	if err := egress(ctx, o, stdout); err != nil {
		fmt.Fprintf(stderr, "egress: %v\n", err)
		return 1
	}
	return 0
}

// The rules this example writes, by name.
const (
	ifconfig = "ifconfig"
	httpbin  = "httpbin"
)

func egress(ctx context.Context, o options, stdout io.Writer) (err error) {
	client, err := sdk.New(o.addr, sdk.WithKey(o.key))
	if err != nil {
		return err
	}
	info, err := client.Info(ctx)
	if err != nil {
		return fmt.Errorf("asking the host what it is: %w", err)
	}
	if info.Network != nil && !info.Network.Enabled {
		return errors.New("this host gives sandboxes no network: its daemon was started with -network=false")
	}

	// No entrypoint: the image's is curl itself, which would run once and
	// exit. The commands below are the workload.
	none := []string{}
	spec := sdk.Spec{
		Image:      o.image,
		Entrypoint: &none,
		Egress: &genv1.EgressSpec{Rules: &[]genv1.EgressRule{
			{Name: ifconfig, Domains: &[]string{"ifconfig.me"}},
		}},
	}
	fmt.Fprintf(stdout, "making a sandbox from %s, with one egress rule: %s allows ifconfig.me\n", o.image, ifconfig)
	sandbox, err := client.Create(ctx, "egress-example", spec)
	if sandbox != nil {
		// Dropped whatever happens, on a context of its own: ctx may be the
		// one Ctrl-C cancelled, and a drop on it would not be sent.
		defer func() {
			drop, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
			defer cancel()
			if closeErr := sandbox.Close(drop); closeErr != nil {
				err = errors.Join(err, fmt.Errorf("dropping the sandbox: %w", closeErr))
				return
			}
			fmt.Fprintln(stdout, "dropped.")
		}()
	}
	if err != nil {
		return err
	}
	fmt.Fprintf(stdout, "ready: %s\n\n", sandbox.ID)

	// Allowed: it is reached, and says which address the sandbox left through.
	if err := reached(ctx, sandbox, stdout, "https://ifconfig.me/ip"); err != nil {
		return err
	}

	// Not allowed: refused, and the host says why.
	if err := refusedAs(ctx, sandbox, stdout, "https://httpbin.org/ip", "httpbin.org"); err != nil {
		return err
	}

	// A rule of its own, added while the machine runs, and it is reached.
	fmt.Fprintf(stdout, "\nadding the rule %s: httpbin.org, on the web ports\n", httpbin)
	if _, err := sandbox.PutRule(ctx, httpbin, sdk.Rule{Domains: &[]string{"httpbin.org"}}); err != nil {
		return fmt.Errorf("adding the rule %s: %w", httpbin, err)
	}
	if err := reached(ctx, sandbox, stdout, "https://httpbin.org/ip"); err != nil {
		return err
	}

	// That rule deleted by its name, and it is refused again; ifconfig's rule
	// is untouched.
	fmt.Fprintf(stdout, "\ndeleting the rule %s\n", httpbin)
	if err := sandbox.DeleteRule(ctx, httpbin); err != nil {
		return fmt.Errorf("deleting the rule %s: %w", httpbin, err)
	}
	if err := refusedAs(ctx, sandbox, stdout, "https://httpbin.org/ip", "httpbin.org"); err != nil {
		return err
	}
	fmt.Fprintln(stdout)
	return nil
}

// curl fetches a URL from inside the sandbox.
func curl(ctx context.Context, sandbox *sdk.Sandbox, url string) (*sdk.Result, error) {
	return sandbox.Run(ctx, "curl", "-sS", "--max-time", "15", url)
}

// reached is curl succeeding, as it must for a URL a rule allows.
func reached(ctx context.Context, sandbox *sdk.Sandbox, stdout io.Writer, url string) error {
	result, err := curl(ctx, sandbox, url)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("curl %s was refused, though a rule allows it: exit %d: %s", url, result.ExitCode, strings.TrimSpace(result.Stderr))
	}
	fmt.Fprintf(stdout, "$ curl %s\n%s\n", url, indent(result.Stdout))
	return nil
}

// refusedAs is curl failing, as it must for a URL no rule allows — and the
// sandbox's egress report saying the host refused that name.
func refusedAs(ctx context.Context, sandbox *sdk.Sandbox, stdout io.Writer, url, name string) error {
	result, err := curl(ctx, sandbox, url)
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return fmt.Errorf("curl %s was reached, though no rule allows it:\n%s", url, result.Stdout)
	}
	fmt.Fprintf(stdout, "$ curl %s\n%s\n", url, indent(result.Stderr))

	report, err := sandbox.Egress(ctx)
	if err != nil {
		return fmt.Errorf("reading its egress: %w", err)
	}
	for _, refusal := range report.Refusals {
		if refusal.Target != name {
			continue
		}
		by := "its firewall"
		if refusal.Kind != nil && *refusal.Kind == genv1.Dns {
			by = "its resolver"
		}
		reason := ""
		if refusal.Reason != nil {
			reason = ": " + *refusal.Reason
		}
		fmt.Fprintf(stdout, "  the host refused %s, by %s%s\n", name, by, reason)
		return nil
	}
	return fmt.Errorf("curl %s failed, and the host's report has no refusal of %s: it failed for another reason", url, name)
}

func indent(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return "  (nothing)"
	}
	return "  " + strings.ReplaceAll(text, "\n", "\n  ")
}
