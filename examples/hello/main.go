// Command hello makes two microVMs, works in them, and drops them.
//
// It is the smallest honest tour of what this service is for:
//
//  1. spawn two sandboxes from an ordinary OCI image, in parallel
//  2. run a command in each and read what it printed
//  3. put a file in one and read it back out
//  4. call a brokered upstream FROM INSIDE a sandbox, and watch the upstream
//     say which sandbox called it — with no credential anywhere in the machine
//  5. drop both, and their disks
//
// Everything it does goes through sdk/, which goes through the generated
// client, which is produced from openapi/sandboxes.yaml. There is no path from
// this program to the daemon that the contract does not describe.
//
//	go run ./examples/hello -addr http://127.0.0.1:8099 -key "$RUNYARD_KEY"
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sdk"
)

type options struct {
	addr, key, image, broker string
}

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr io.Writer) int {
	var o options
	flags := flag.NewFlagSet("hello", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&o.addr, "addr", "http://127.0.0.1:8099", "the daemon")
	flags.StringVar(&o.key, "key", os.Getenv("RUNYARD_SANDBOXES_KEY"), "an API key. Mint one with: runyard-sandboxes keys mint")
	flags.StringVar(&o.image, "image", "alpine:3.20", "the OCI image each sandbox boots")
	flags.StringVar(&o.broker, "broker", "", "a broker this host declares, to call from inside a sandbox")
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}

	// Ctrl-C cancels, rather than killing the program where it stands: killed,
	// it never reached the deferred drop, and both machines stayed up.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()

	if err := hello(ctx, o, stdout, stderr); err != nil {
		fmt.Fprintf(stderr, "hello: %v\n", err)
		return 1
	}
	return 0
}

func hello(ctx context.Context, o options, stdout, stderr io.Writer) error {
	addr, key, image, broker := o.addr, o.key, o.image, o.broker
	client, err := sdk.New(addr, sdk.WithKey(key))
	if err != nil {
		return err
	}

	info, err := client.Info(ctx)
	if err != nil {
		return fmt.Errorf("asking the host what it is: %w", err)
	}
	fmt.Fprintf(stdout, "host %s — %s\n", info.Id, info.Hypervisor.Version)
	if info.Brokers != nil {
		for _, declared := range *info.Brokers {
			fmt.Fprintf(stdout, "  broker %s → %s\n", declared.Name, declared.Url)
		}
	}

	spec := sdk.Spec{Image: image}
	if broker != "" {
		spec.Brokers = &[]genv1.SandboxBrokerSpec{{Name: broker}}
	}

	// Two machines, made at the same time, because that is the interesting
	// case: they share an image, and on a filesystem with copy-on-write clones
	// they share every block of it until they write.
	names := []string{"hello-one", "hello-two"}
	sandboxes := make([]*sdk.Sandbox, len(names))
	started := time.Now()

	errs := make([]error, len(names))
	var creating sync.WaitGroup
	for i, name := range names {
		creating.Go(func() { //task:unowned joined by creating.Wait, below
			// The name is a label, and the hostname is sent with it rather
			// than derived from it: the daemon interprets no label, so a
			// caller that wants a shell prompt to say "hello-one" says so.
			own := spec
			own.Hostname = &name
			sandbox, err := client.Create(ctx, name, own)
			sandboxes[i], errs[i] = sandbox, err
		})
	}
	creating.Wait()

	// Dropped whatever happens, including on the way out of a failure — and a
	// create that failed still hands back the machine it made, so that one is
	// dropped too. A machine that outlives the program that made it costs
	// somebody memory until a person notices.
	//
	// On a context of its own, because ctx may be the one Ctrl-C cancelled;
	// and bounded, because a daemon that stopped answering would otherwise
	// hold the program open forever on the way out.
	defer func() {
		dropping, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
		defer cancel()
		for _, sandbox := range sandboxes {
			if sandbox == nil {
				continue
			}
			if err := sandbox.Close(dropping); err != nil {
				fmt.Fprintf(stderr, "  dropping %s (%s): %v\n", sandbox.Name, sandbox.ID, err)
				continue
			}
			fmt.Fprintf(stdout, "dropped %s (%s)\n", sandbox.Name, sandbox.ID)
		}
	}()

	for i, err := range errs {
		if err != nil {
			return fmt.Errorf("creating %s: %w", names[i], err)
		}
	}
	fmt.Fprintf(stdout, "two sandboxes ready in %s\n", time.Since(started).Round(time.Millisecond))
	for _, sandbox := range sandboxes {
		fmt.Fprintf(stdout, "  %s is %s\n", sandbox.Name, sandbox.ID)
	}

	for _, sandbox := range sandboxes {
		result, err := sandbox.Run(ctx, "/bin/sh", "-c", "echo I am $(hostname), running $(uname -r)")
		if err != nil {
			return fmt.Errorf("running in %s: %w", sandbox.ID, err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("%s: exit %d: %s", sandbox.ID, result.ExitCode, result.Stderr)
		}
		fmt.Fprintf(stdout, "  %s", result.Stdout)
	}

	// A file in, and the workload's own view of it out. Two directions of the
	// same wire, and the second is the one that proves the first.
	const note = "written from the host, read by the sandbox\n"
	if err := sandboxes[0].WriteFile(ctx, "/work/note.txt", []byte(note), "0644"); err != nil {
		return fmt.Errorf("writing a file: %w", err)
	}
	back, err := sandboxes[0].ReadFile(ctx, "/work/note.txt")
	if err != nil {
		return fmt.Errorf("reading it back: %w", err)
	}
	fmt.Fprintf(stdout, "  /work/note.txt is %d bytes and says: %s", len(back), back)

	if broker != "" {
		address, ok := sandboxes[0].Broker(broker)
		if !ok {
			return fmt.Errorf("sandbox %s was not given broker %q", sandboxes[0].ID, broker)
		}
		fmt.Fprintf(stdout, "\ncalling %s from inside %s, which holds no credential at all:\n", broker, sandboxes[0].ID)
		if sandboxes[0].Identity != nil {
			fmt.Fprintf(stdout, "  the host will attach %s\n", sandboxes[0].Identity.Uri)
		}
		result, err := sandboxes[0].RunWith(ctx, genv1.CommandRequest{
			Argv:           []string{"/bin/sh", "-c", "wget -q -O - " + address + "/whoami"},
			TimeoutSeconds: new(30),
		})
		if err != nil {
			return fmt.Errorf("calling the broker: %w", err)
		}
		if result.ExitCode != 0 {
			return fmt.Errorf("the brokered call failed: %s", result.Stderr)
		}
		fmt.Fprintf(stdout, "  the upstream answered: %s", result.Stdout)
	}

	fmt.Fprintln(stdout)
	return nil
}
