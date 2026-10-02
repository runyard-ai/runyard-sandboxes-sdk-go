package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// grant is what `ryclaude auth login` left on this machine: a daemon, and the
// key it gave. One at a time: logging in again, there or elsewhere, replaces
// it.
type grant struct {
	// URL is the daemon that issued the key, and the only one it is ever
	// sent to.
	URL       string    `json:"url"`
	Key       string    `json:"key"`
	KeyID     string    `json:"keyId"`
	Name      string    `json:"name"`
	Owner     string    `json:"owner"`
	ExpiresAt time.Time `json:"expiresAt"`
}

// grantPath is where the grant is kept: with this person's configuration,
// which is theirs alone to read.
func grantPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("finding where ryclaude keeps its key: %w", err)
	}
	return filepath.Join(dir, "ryclaude", "credentials.json"), nil
}

// loadGrant reads the grant, and says whether there is one. A file that is
// there and cannot be trusted is an error and never "not logged in": a key
// silently ignored is a person wondering why they are asked to log in again,
// and one read from a file others can read is one that is no longer theirs.
func loadGrant(path string) (grant, bool, error) {
	file, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return grant{}, false, nil
	}
	if err != nil {
		return grant{}, false, fmt.Errorf("reading ryclaude's key: %w", err)
	}
	defer func() { _ = file.Close() }()
	// Of the file that was opened, not of its name: what is checked is what
	// is read.
	info, err := file.Stat()
	if err != nil {
		return grant{}, false, fmt.Errorf("reading ryclaude's key: %w", err)
	}
	// Windows has no such bits, and says 0666 of every file.
	if mode := info.Mode().Perm(); mode&0o077 != 0 && runtime.GOOS != "windows" {
		return grant{}, false, fmt.Errorf("%s holds a key and others on this machine can read it (mode %04o), so it is not used. Fix it with: chmod 600 %s", path, mode, path)
	}

	var g grant
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	// A field this program does not know is a file it did not write, or one
	// a newer ryclaude did: either way not one to guess at.
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&g); err != nil {
		return grant{}, false, fmt.Errorf("%s is not a key ryclaude kept: %w. Remove it, and run `ryclaude auth login` again", path, err)
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return grant{}, false, fmt.Errorf("%s is not a key ryclaude kept: something follows it. Remove it, and run `ryclaude auth login` again", path)
	}
	if g.URL == "" || g.Key == "" || g.KeyID == "" || g.ExpiresAt.IsZero() {
		return grant{}, false, fmt.Errorf("%s is not a key ryclaude kept: it lacks a url, a key, a keyId or an expiresAt. Remove it, and run `ryclaude auth login` again", path)
	}
	if !g.text() {
		return grant{}, false, fmt.Errorf("%s is not a key ryclaude kept: it holds what is not text. Remove it, and run `ryclaude auth login` again", path)
	}
	// Held to what login would have written: the key goes where this says.
	if address, err := daemonURL(g.URL); err != nil || address != g.URL {
		return grant{}, false, fmt.Errorf("%s names a daemon ryclaude would not have logged in to. Remove it, and run `ryclaude auth login` again", path)
	}
	return g, true, nil
}

// save keeps the grant, in place of any other. The file is made 0600 under
// another name and renamed over the old one, so the key is never in a file
// others can read, not for a moment, and a reader finds the old grant or the
// new one and never half of either.
func (g grant) save(path string) error {
	// A daemon named the key and said whose it is, and every ryclaude after
	// this one would say so on a terminal.
	if !g.text() {
		return errors.New("keeping the key: the daemon gave it a name, an owner or an id that is not text")
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("making the directory ryclaude keeps its key in: %w", err)
	}
	body, err := json.MarshalIndent(g, "", "  ")
	if err != nil {
		return err
	}
	// CreateTemp makes it 0600.
	file, err := os.CreateTemp(dir, ".credentials-*")
	if err != nil {
		return fmt.Errorf("keeping the key: %w", err)
	}
	_, err = file.Write(append(body, '\n'))
	if err == nil {
		// On the disk before it has the name: a crash leaves the old file,
		// not an empty one.
		err = file.Sync()
	}
	if closeErr := file.Close(); err == nil {
		err = closeErr
	}
	if err == nil {
		err = os.Rename(file.Name(), path)
	}
	if err != nil {
		_ = os.Remove(file.Name())
		return fmt.Errorf("keeping the key: %w", err)
	}
	return nil
}

// text says whether everything the grant holds is text: none of it is a
// daemon's way to a terminal's control characters.
func (g grant) text() bool {
	return allText(g.URL) && allText(g.Key) && allText(g.KeyID) && allText(g.Name) && allText(g.Owner)
}

// forgetGrant removes the grant; one that is not there is removed already.
func forgetGrant(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("removing ryclaude's key: %w", err)
	}
	return nil
}

// expired says whether the key has outlived what it was approved for.
func (g grant) expired() bool { return !time.Now().Before(g.ExpiresAt) }

// when is a moment as a person reads one, in their own time zone.
func when(t time.Time) string { return t.Local().Format("2006-01-02 15:04 MST") }

// daemonURL is a daemon's address as a key may be sent to it: its scheme, its
// host and nothing else, over TLS unless the daemon is on this machine.
//
// What it refuses, it refuses without repeating: an address somebody put a
// password in is not one to print.
func daemonURL(raw string) (string, error) {
	parsed, err := url.Parse(raw)
	switch {
	case err != nil, parsed.Opaque != "", parsed.Hostname() == "", parsed.Scheme != "https" && parsed.Scheme != "http":
		return "", errors.New("it is not a daemon's address, which is https:// and a host, as https://sandboxes.example.com is")
	case parsed.User != nil:
		return "", errors.New("it names a user or a password, and a daemon's address has neither")
	case (parsed.Path != "" && parsed.Path != "/") || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "":
		return "", errors.New("it has a path, a query or a fragment, and a daemon's address is its host alone, as https://sandboxes.example.com is")
	case parsed.Scheme == "http" && !loopback(parsed.Hostname()):
		// The key is a bearer credential: over plain HTTP anybody on the
		// way reads it and has it.
		return "", fmt.Errorf("it is plain HTTP to another machine, where the key would cross the network for anybody on the way to read: use https://%s", parsed.Host)
	}
	return parsed.Scheme + "://" + strings.ToLower(parsed.Host), nil
}

// loopback says whether a host is this machine's own.
func loopback(host string) bool {
	ip := net.ParseIP(host)
	return strings.EqualFold(host, "localhost") || (ip != nil && ip.IsLoopback())
}
