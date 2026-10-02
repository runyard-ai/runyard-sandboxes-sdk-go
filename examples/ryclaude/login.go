package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sdk"
)

// scopes is what ryclaude does with a key, and so all it asks for: making its
// sandbox and dropping it, writing claude's files in, and running claude. A
// key that could do more is one worth more to whoever takes this machine's.
const scopes = string(genv1.SandboxesRead + "," + genv1.SandboxesWrite + "," + genv1.FilesWrite + "," + genv1.Exec)

// approveWithin is how long a person is waited for. It is what a code lasts:
// an approval given later than this is one whose code has stopped working.
const approveWithin = 5 * time.Minute

// login is `ryclaude auth login`: a person approves a key for this machine
// in the daemon's console, and it is kept for every ryclaude after.
func login(ctx context.Context, args []string, m machine, stderr io.Writer) int {
	flags := flag.NewFlagSet("ryclaude auth login", flag.ContinueOnError)
	address := flags.String("url", "", "the daemon to log in to, e.g. https://sandboxes.example.com")
	noBrowser := flags.Bool("no-browser", false, "open no browser, and wait for the code the page shows to be pasted here")
	if code, done := parsed(flags, args, stderr); done {
		return code
	}
	daemon, err := daemonURL(*address)
	if err != nil {
		fmt.Fprintf(stderr, "ryclaude auth login: -url says which daemon to log in to, and %v\n", err)
		return 2
	}
	if err := signIn(ctx, daemon, *noBrowser, m, stderr); err != nil {
		fmt.Fprintf(stderr, "ryclaude: %v\n", err)
		return 1
	}
	return 0
}

// signIn is OAuth's authorization code with PKCE and a loopback redirect
// (RFC 7636, RFC 8252), as the daemon's contract describes it.
func signIn(ctx context.Context, daemon string, noBrowser bool, m machine, stderr io.Writer) error {
	// Before anybody is asked anything: an approval with nowhere to keep
	// its key is one made for nothing.
	path, err := grantPath()
	if err != nil {
		return err
	}
	client, err := dial(daemon, "")
	if err != nil {
		return err
	}
	if err := signsPeopleIn(ctx, client, daemon); err != nil {
		return err
	}

	// The verifier stays here until the redeem: what the browser carries is
	// its SHA-256, which a code is bound to and nobody can turn back.
	verifier, state := random(), random()
	challenge := sha256.Sum256([]byte(verifier))
	ask := url.Values{
		"client":         {asker(m.hostname)},
		"scopes":         {scopes},
		"sandboxes":      {string(genv1.KeyReachOwn)},
		"state":          {state},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
	}
	// Without a browser here there is nothing to send back to this machine:
	// the page is given no address, shows the code, and nothing listens.
	var answers <-chan answer
	hangUp := func() {}
	if !noBrowser {
		back, err := listen(ctx, state)
		if err != nil {
			return err
		}
		ask.Set("redirect_uri", back.uri)
		answers, hangUp = back.answers, back.close
	}
	page := daemon + "/console/authorize?" + ask.Encode()

	var pasted <-chan string
	switch {
	case noBrowser:
		fmt.Fprintf(stderr, "ryclaude: open this address, approve, and paste here the code it shows:\n\n  %s\n\n", page)
		pasted = paste(m)
	default:
		fmt.Fprintf(stderr, "ryclaude: approve the key in your browser, at:\n\n  %s\n\n", page)
		if err := m.browse(ctx, page); err != nil {
			fmt.Fprintf(stderr, "ryclaude: no browser was opened (%v): open that address yourself\n", err)
		}
		// A browser on another machine cannot come back to this one: the
		// code is in the address it failed to open, to be pasted.
		if m.interactive() {
			fmt.Fprintln(stderr, "ryclaude: a browser on another machine cannot come back here: paste the code from the address it ends at, after code=")
			pasted = paste(m)
		}
	}
	code, err := await(ctx, answers, pasted)
	// However the wait ended, nothing more is listened for: the port is
	// closed before the daemon is called, not when everything is over.
	hangUp()
	if err != nil {
		return err
	}
	key, err := redeem(ctx, client, daemon, code, verifier)
	if err != nil {
		return err
	}
	return keep(ctx, path, grant{
		URL: daemon, Key: key.Secret, KeyID: key.Id, Name: key.Name, Owner: string(key.Owner), ExpiresAt: key.ExpiresAt,
	}, stderr)
}

// signsPeopleIn refuses a daemon nobody can approve a key at, before a
// browser is opened on a page that could only say so.
func signsPeopleIn(ctx context.Context, client *sdk.Client, daemon string) error {
	ctx, cancel := context.WithTimeout(ctx, answerWithin)
	defer cancel()
	res, err := client.API().GetAuthWithResponse(ctx)
	switch {
	case err != nil:
		return fmt.Errorf("asking %s how people sign in: %w", daemon, err)
	case res.JSON200 == nil:
		return fmt.Errorf("asking %s how people sign in: %w", daemon, answered(res.StatusCode(), res.Body))
	case !res.JSON200.Google:
		return fmt.Errorf("%s signs nobody in, so nobody can approve a key there: mint one on its host with `runyard-sandboxes keys mint`, and give it with -key or RUNYARD_SANDBOXES_KEY", daemon)
	}
	return nil
}

// random is 32 bytes nobody can guess, as base64url without padding: the 43
// characters RFC 7636 asks of a verifier.
func random() string {
	bytes := make([]byte, 32)
	// crypto/rand does not fail: it ends the program when the kernel has
	// no randomness to give.
	_, _ = rand.Read(bytes)
	return base64.RawURLEncoding.EncodeToString(bytes)
}

// askerLimit is how long the daemon lets what is asking be, in characters.
const askerLimit = 128

// asker is what the consent page shows as asking, and what the key is named
// for: this program, and the machine, so that its owner reading a list of
// keys one day knows which laptop this one was.
//
// A machine is named whatever its administrator typed. The daemon refuses a
// name with control or format characters in it, or longer than askerLimit;
// what of the machine's name is not text is left out, and what is too long
// cut, since a login refused for the laptop's name is one nobody can fix
// from the page.
func asker(hostname func() (string, error)) string {
	const program = "ryclaude"
	name, err := hostname()
	if err != nil {
		name = ""
	}
	name = strings.TrimSpace(strings.Map(func(r rune) rune {
		if !text(r) || r == utf8.RuneError {
			return -1
		}
		return r
	}, name))
	if name == "" {
		return program
	}
	if on := []rune(program + " on " + name); len(on) > askerLimit {
		return strings.TrimSpace(string(on[:askerLimit]))
	}
	return program + " on " + name
}

// paste is the first line typed that is not empty, and is closed with none
// when what is typed ends.
func paste(typed io.Reader) <-chan string {
	pasted := make(chan string, 1)
	//task:unowned a read of standard input cannot be interrupted: it ends with the line, or with the process, which exits once the login has
	go func() {
		defer close(pasted)
		for lines := bufio.NewScanner(typed); lines.Scan(); {
			if code := strings.TrimSpace(lines.Text()); code != "" {
				pasted <- code
				return
			}
		}
	}()
	return pasted
}

// await is the code, from whichever brings it first: the browser coming
// back, or the person pasting it. Either channel may be nil, for a way the
// code is not expected.
func await(ctx context.Context, answers <-chan answer, pasted <-chan string) (string, error) {
	waiting, cancel := context.WithTimeout(ctx, approveWithin)
	defer cancel()
	for {
		select {
		case <-waiting.Done():
			if ctx.Err() != nil {
				return "", errors.New("interrupted before anything was approved: no key was made")
			}
			return "", fmt.Errorf("nothing was approved within %s, which is as long as an approval's code lasts: run `ryclaude auth login` again", approveWithin)
		case got := <-answers:
			if got.denied {
				return "", errors.New("the key was denied in the browser: no key was made")
			}
			return got.code, nil
		case code, typed := <-pasted:
			if typed {
				return code, nil
			}
			if answers == nil {
				return "", errors.New("no code was pasted: standard input ended")
			}
			// Nothing more will be typed, and the browser may still come
			// back.
			pasted = nil
		}
	}
}

// redeem trades the code and the verifier for the key, which the daemon
// mints then. Nothing of the three is ever in what this says went wrong.
func redeem(ctx context.Context, client *sdk.Client, daemon, code, verifier string) (*genv1.KeyCreated, error) {
	ctx, cancel := context.WithTimeout(ctx, answerWithin)
	defer cancel()
	res, err := client.API().RedeemKeyAuthorizationWithResponse(ctx, genv1.KeyAuthorizationRedemption{Code: code, CodeVerifier: verifier})
	switch {
	case err != nil:
		return nil, fmt.Errorf("collecting the key from %s: %w", daemon, err)
	case res.StatusCode() == http.StatusBadRequest:
		// The daemon does not say which, on purpose, and neither can this.
		return nil, errors.New("the approval was refused or has expired; run `ryclaude auth login` again")
	case res.JSON200 == nil:
		return nil, fmt.Errorf("collecting the key from %s: %w", daemon, answered(res.StatusCode(), res.Body))
	case res.JSON200.Secret == "" || res.JSON200.Id == "":
		return nil, fmt.Errorf("collecting the key from %s: it answered with no key", daemon)
	}
	return res.JSON200, nil
}

// keep saves the key in place of the one before it, says whose it is, and
// hands the old one back.
func keep(ctx context.Context, path string, g grant, stderr io.Writer) error {
	previous, had, unread := loadGrant(path)
	if err := g.save(path); err != nil {
		// A key nothing holds would live until it expired, with nobody
		// knowing to revoke it.
		if revokeErr := revoke(ctx, g); revokeErr != nil {
			return fmt.Errorf("%w; and the key %q (%s) that was made could not be revoked (%w): revoke it in the console of %s", err, g.Name, g.KeyID, revokeErr, g.URL)
		}
		return fmt.Errorf("%w; the key that was made has been revoked", err)
	}
	// Whose it is comes first: a key that is somebody else's is one a
	// program on this machine had approved in its own name, and brought
	// to the callback before the browser did.
	fmt.Fprintf(stderr, "ryclaude: logged in as %s, at %s, with the key %q (%s), which expires on %s\n", g.Owner, g.URL, g.Name, g.KeyID, when(g.ExpiresAt))

	// Only once the new one is safe, and never fatal: the login worked.
	switch {
	case unread != nil:
		fmt.Fprintf(stderr, "ryclaude: warning: what was kept before could not be read, so the key it held is not revoked: %v\n", unread)
	case had && !previous.expired():
		if err := revoke(ctx, previous); err != nil && !errors.Is(err, errKeyRefused) {
			fmt.Fprintf(stderr, "ryclaude: warning: the key %q (%s) kept before, for %s, could not be revoked: %v. It works until %s, unless it is revoked in the console there\n",
				previous.Name, previous.KeyID, previous.URL, err, when(previous.ExpiresAt))
		}
	}
	return nil
}
