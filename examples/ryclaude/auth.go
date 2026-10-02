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
	"syscall"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sdk"
)

const authUsage = `usage:
  ryclaude auth login -url https://sandboxes.example.com [-no-browser]
        get a key for this machine, approved by you in the daemon's console
  ryclaude auth logout [-forget]
        revoke that key, and forget it
  ryclaude auth status
        which daemon, whose key, until when, and whether it still works
`

// answerWithin bounds one question to a daemon. None of them waits on
// anything but the daemon itself, so one that takes longer is one that is
// not going to answer.
const answerWithin = 30 * time.Second

// errKeyRefused is a daemon that no longer takes the key: revoked there, or
// expired.
var errKeyRefused = errors.New("the daemon no longer accepts the key")

// authenticate is `ryclaude auth …`.
func authenticate(ctx context.Context, args []string, m machine, stderr io.Writer) int {
	// Ctrl-C is this program's here, as it is not once claude has the
	// terminal: nothing is raw, and somebody waiting for a browser stops
	// with it.
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(args) == 0 {
		fmt.Fprint(stderr, authUsage)
		return 2
	}
	switch command, rest := args[0], args[1:]; command {
	case "login":
		return login(ctx, rest, m, stderr)
	case "logout":
		return logout(ctx, rest, stderr)
	case "status":
		return status(ctx, rest, m, stderr)
	case "-h", "-help", "--help":
		fmt.Fprint(stderr, authUsage)
		return 0
	}
	fmt.Fprint(stderr, authUsage)
	return 2
}

// parsed reads a command's flags, and says what to exit with when that is
// all there is to do: 0 for -h, 2 for anything the command does not take.
func parsed(flags *flag.FlagSet, args []string, stderr io.Writer) (code int, done bool) {
	flags.SetOutput(stderr)
	if err := flags.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0, true
		}
		return 2, true
	}
	if flags.NArg() > 0 {
		fmt.Fprintf(stderr, "%s: it takes no %q\n", flags.Name(), flags.Arg(0))
		flags.Usage()
		return 2, true
	}
	return 0, false
}

// logout is `ryclaude auth logout`: the key is handed back to the daemon
// that gave it, and forgotten here.
func logout(ctx context.Context, args []string, stderr io.Writer) int {
	flags := flag.NewFlagSet("ryclaude auth logout", flag.ContinueOnError)
	forget := flags.Bool("forget", false, "forget the key here even when the daemon cannot be told to revoke it")
	if code, done := parsed(flags, args, stderr); done {
		return code
	}
	path, err := grantPath()
	if err != nil {
		fmt.Fprintf(stderr, "ryclaude: %v\n", err)
		return 1
	}
	g, found, err := loadGrant(path)
	switch {
	case err != nil && !*forget:
		fmt.Fprintf(stderr, "ryclaude: %v\nryclaude: nothing was revoked. `ryclaude auth logout -forget` removes it as it is\n", err)
		return 1
	case err != nil:
		fmt.Fprintf(stderr, "ryclaude: warning: %v\nryclaude: warning: the key it held, if any, is not revoked\n", err)
	case !found:
		fmt.Fprintln(stderr, "ryclaude: not logged in: there is nothing to log out of")
		return 0
	default:
		err := revoke(ctx, g)
		switch {
		case err == nil:
			fmt.Fprintf(stderr, "ryclaude: the key %q (%s) is revoked at %s\n", g.Name, g.KeyID, g.URL)
		case errors.Is(err, errKeyRefused):
			// Revoked there already, or expired: what was asked for is so.
			fmt.Fprintf(stderr, "ryclaude: %s had already stopped accepting the key %q (%s)\n", g.URL, g.Name, g.KeyID)
		case !*forget:
			// Kept: the file is the only thing here that can still revoke
			// the key, and forgetting it leaves one that works with nobody
			// holding it.
			fmt.Fprintf(stderr, "ryclaude: the key %q (%s) was not revoked: %v\nryclaude: still logged in. Try again, or `ryclaude auth logout -forget` to forget the key here and leave it to expire on %s\n",
				g.Name, g.KeyID, err, when(g.ExpiresAt))
			return 1
		default:
			fmt.Fprintf(stderr, "ryclaude: warning: the key %q (%s) was not revoked: %v. It works until %s, unless it is revoked in the console of %s\n",
				g.Name, g.KeyID, err, when(g.ExpiresAt), g.URL)
		}
	}
	if err := forgetGrant(path); err != nil {
		fmt.Fprintf(stderr, "ryclaude: %v\n", err)
		return 1
	}
	fmt.Fprintln(stderr, "ryclaude: logged out")
	return 0
}

// status is `ryclaude auth status`: what is kept here, and whether the
// daemon still takes it. The key itself is never among what it says.
func status(ctx context.Context, args []string, m machine, stderr io.Writer) int {
	if code, done := parsed(flag.NewFlagSet("ryclaude auth status", flag.ContinueOnError), args, stderr); done {
		return code
	}
	path, err := grantPath()
	if err != nil {
		fmt.Fprintf(stderr, "ryclaude: %v\n", err)
		return 1
	}
	g, found, err := loadGrant(path)
	switch {
	case err != nil:
		fmt.Fprintf(stderr, "ryclaude: %v\n", err)
		return 1
	case !found:
		fmt.Fprintln(stderr, "ryclaude: not logged in: log in with `ryclaude auth login -url https://…`")
		return 1
	}
	// The terminal claude's screen goes to as it is; what is said here is
	// held to text, as everything else this program says is.
	out := plain{m}
	fmt.Fprintf(out, "daemon:  %s\nkey:     %s (%s)\nowner:   %s\nexpires: %s\n", g.URL, g.Name, g.KeyID, g.Owner, when(g.ExpiresAt))
	if g.expired() {
		fmt.Fprintf(out, "status:  expired: run `ryclaude auth login -url %s` again\n", g.URL)
		return 1
	}
	switch err := accepted(ctx, g); {
	case err == nil:
		fmt.Fprintln(out, "status:  accepted by the daemon")
		return 0
	case errors.Is(err, errKeyRefused):
		fmt.Fprintf(out, "status:  no longer accepted by the daemon, where it was revoked or has expired: run `ryclaude auth login -url %s` again\n", g.URL)
	default:
		fmt.Fprintf(out, "status:  unknown: %v\n", err)
	}
	return 1
}

// dial is a client of one daemon, which goes nowhere else: a redirect is
// not followed, since following one is sending what was meant for this
// daemon — a key, a verifier — wherever its answer says.
func dial(daemon, key string, more ...sdk.Option) (*sdk.Client, error) {
	stay := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return sdk.New(daemon, append([]sdk.Option{sdk.WithKey(key), sdk.WithHTTPClient(&http.Client{CheckRedirect: stay})}, more...)...)
}

// revoke hands a key back to the daemon that gave it: a key may revoke
// itself, whatever its scopes.
func revoke(ctx context.Context, g grant) error {
	client, err := dial(g.URL, g.Key)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, answerWithin)
	defer cancel()
	res, err := client.API().RevokeKeyWithResponse(ctx, g.KeyID)
	switch {
	case err != nil:
		return fmt.Errorf("reaching %s: %w", g.URL, err)
	case res.StatusCode() == http.StatusNoContent:
		return nil
	case res.StatusCode() == http.StatusUnauthorized:
		return errKeyRefused
	}
	return answered(res.StatusCode(), res.Body)
}

// accepted asks the daemon whether it still takes the key.
func accepted(ctx context.Context, g grant) error {
	client, err := dial(g.URL, g.Key)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, answerWithin)
	defer cancel()
	res, err := client.API().GetMeWithResponse(ctx)
	switch {
	case err != nil:
		return fmt.Errorf("reaching %s: %w", g.URL, err)
	case res.JSON200 != nil:
		return nil
	case res.StatusCode() == http.StatusUnauthorized:
		return errKeyRefused
	}
	return answered(res.StatusCode(), res.Body)
}

// answered is the error for an answer that is not the one an operation
// declares for success. It says the daemon's own code and message, when the
// answer is the contract's refusal, and otherwise only the status: a body
// this does not know the shape of is not one to print, since on these
// routes it might be a key.
func answered(status int, body []byte) error {
	var envelope genv1.Error
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Code != "" {
		return &sdk.Error{Status: status, Code: string(envelope.Error.Code), Message: envelope.Error.Message}
	}
	return fmt.Errorf("it answered HTTP %d, which is not what a runyard-sandboxes daemon answers", status)
}
