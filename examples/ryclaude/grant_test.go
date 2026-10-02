package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// configHome is a configuration directory of the test's own, with nothing in
// it, and where ryclaude keeps its key under it.
func configHome(t *testing.T) (path string) {
	t.Helper()
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	path, err := grantPath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

// granted is a key as a login would have kept it, good for a week.
func granted() grant {
	return grant{
		URL: "https://sandboxes.example.com", Key: "rysk_k1234567_secret", KeyID: "k1234567",
		Name: "ryclaude on laptop", Owner: "ada@example.com", ExpiresAt: time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second),
	}
}

// keptAs writes what a credentials file holds, as something else than
// ryclaude might have, with a mode.
func keptAs(t *testing.T, path, body string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	// WriteFile's mode is under the umask, and what is tested is the mode.
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func modeOf(t *testing.T, path string) os.FileMode {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	return info.Mode().Perm()
}

func TestTheKeyIsKeptWhereItsOwnerAloneReadsIt(t *testing.T) {
	path := configHome(t)
	if want := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "ryclaude", "credentials.json"); path != want {
		t.Fatalf("kept at %s, want %s", path, want)
	}
	g := granted()
	if err := g.save(path); err != nil {
		t.Fatal(err)
	}
	if mode := modeOf(t, filepath.Dir(path)); mode != 0o700 {
		t.Errorf("the directory is %04o, want 0700", mode)
	}
	if mode := modeOf(t, path); mode != 0o600 {
		t.Errorf("the file is %04o, want 0600", mode)
	}

	// The file is the six fields, by these names.
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]string
	if err := json.Unmarshal(body, &fields); err != nil {
		t.Fatalf("%s: %v", body, err)
	}
	want := map[string]string{
		"url": g.URL, "key": g.Key, "keyId": g.KeyID, "name": g.Name, "owner": g.Owner, "expiresAt": g.ExpiresAt.Format(time.RFC3339),
	}
	if len(fields) != len(want) {
		t.Errorf("the file holds %v", fields)
	}
	for name, value := range want {
		if fields[name] != value {
			t.Errorf("%s is %q, want %q", name, fields[name], value)
		}
	}

	got, found, err := loadGrant(path)
	if err != nil || !found || got != g {
		t.Fatalf("read back %+v %v %v", got, found, err)
	}
	if got.expired() {
		t.Error("a key good for a week has expired")
	}
}

// A second login replaces the first whole, and leaves nothing beside it.
func TestAKeyReplacesTheOneBeforeIt(t *testing.T) {
	path := configHome(t)
	first, second := granted(), granted()
	second.URL, second.Key, second.KeyID = "https://other.example.com", "rysk_k7654321_other", "k7654321"
	for _, g := range []grant{first, second} {
		if err := g.save(path); err != nil {
			t.Fatal(err)
		}
	}
	if got, _, err := loadGrant(path); err != nil || got != second {
		t.Fatalf("read back %+v %v", got, err)
	}
	if mode := modeOf(t, path); mode != 0o600 {
		t.Errorf("the file is %04o, want 0600", mode)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil || len(entries) != 1 {
		t.Errorf("beside the key: %v %v", entries, err)
	}
}

func TestAKeyThatCannotBeKeptIsSaidAndLeavesNothing(t *testing.T) {
	t.Run("the configuration directory is a file", func(t *testing.T) {
		home := filepath.Join(t.TempDir(), "config")
		keptAs(t, home, "", 0o600)
		t.Setenv("XDG_CONFIG_HOME", home)
		path, _ := grantPath()
		if err := granted().save(path); err == nil || !strings.Contains(err.Error(), "making the directory") {
			t.Fatalf("%v", err)
		}
	})
	// A rename that fails halfway: the old state is as it was, and the file
	// the key was written to is gone with the key in it.
	t.Run("something that is not a file has its name", func(t *testing.T) {
		path := configHome(t)
		if err := os.MkdirAll(filepath.Join(path, "in the way"), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := granted().save(path); err == nil || !strings.Contains(err.Error(), "keeping the key") {
			t.Fatalf("%v", err)
		}
		entries, err := os.ReadDir(filepath.Dir(path))
		if err != nil || len(entries) != 1 || entries[0].Name() != "credentials.json" {
			t.Errorf("left behind: %v %v", entries, err)
		}
	})
}

// What a daemon named is what every ryclaude after says on a terminal: a
// key whose name, owner or id is not text is not kept at all.
func TestAKeyThatIsNotTextIsNotKept(t *testing.T) {
	for name, spoil := range map[string]func(*grant){
		"an escape in its name":           func(g *grant) { g.Name = "ryclaude\x1b[2J" },
		"a carriage return in its owner":  func(g *grant) { g.Owner = "mallory@example.com\rada@example.com" },
		"a zero-width space in its owner": func(g *grant) { g.Owner = "ada\u200b@example.com" },
		"a line break in its id":          func(g *grant) { g.KeyID = "k1234567\n" },
		"a tab in its id":                 func(g *grant) { g.KeyID = "k123\t4567" },
		"a bell in the key":               func(g *grant) { g.Key = "rysk_k1234567_\a" },
		"bytes that are no rune":          func(g *grant) { g.Name = "ryclaude\xff" },
	} {
		t.Run(name, func(t *testing.T) {
			path := configHome(t)
			g := granted()
			spoil(&g)
			if err := g.save(path); err == nil || !strings.Contains(err.Error(), "a name, an owner or an id that is not text") {
				t.Fatalf("%v", err)
			}
			if _, found, err := loadGrant(path); found || err != nil {
				t.Errorf("something was kept: %v %v", found, err)
			}
		})
	}
	// Text of any script is text.
	path := configHome(t)
	g := granted()
	g.Name, g.Owner = "ryclaude on Zoë’s laptop — 鍵", "zoë@example.com"
	if err := g.save(path); err != nil {
		t.Fatal(err)
	}
	if got, _, err := loadGrant(path); err != nil || got != g {
		t.Errorf("read back %+v %v", got, err)
	}
}

func TestNoKeyKeptIsNotAnError(t *testing.T) {
	if _, found, err := loadGrant(configHome(t)); found || err != nil {
		t.Fatalf("%v %v", found, err)
	}
}

func TestAKeyThatCannotBeTrustedIsRefusedAndNotIgnored(t *testing.T) {
	whole, err := json.Marshal(granted())
	if err != nil {
		t.Fatal(err)
	}
	with := func(field, value string) string {
		var fields map[string]any
		if err := json.Unmarshal(whole, &fields); err != nil {
			t.Fatal(err)
		}
		fields[field] = value
		if value == "" {
			delete(fields, field)
		}
		body, err := json.Marshal(fields)
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	for _, c := range []struct {
		name, body string
		mode       os.FileMode
		said       string
	}{
		{"readable by its group", string(whole), 0o640, "others on this machine can read it (mode 0640)"},
		{"readable by everybody", string(whole), 0o604, "chmod 600 "},
		{"writable by its group", string(whole), 0o620, "chmod 600 "},
		{"empty", "", 0o600, "is not a key ryclaude kept"},
		{"cut short", string(whole[:len(whole)/2]), 0o600, "is not a key ryclaude kept"},
		{"not JSON", "rysk_k1234567_secret\n", 0o600, "is not a key ryclaude kept"},
		{"an unknown field", with("scopes", "exec"), 0o600, `unknown field "scopes"`},
		{"something after it", string(whole) + "{}", 0o600, "something follows it"},
		{"no url", with("url", ""), 0o600, "it lacks a url"},
		{"no key", with("key", ""), 0o600, "it lacks a url"},
		{"no key id", with("keyId", ""), 0o600, "it lacks a url"},
		{"no expiry", with("expiresAt", ""), 0o600, "it lacks a url"},
		{"an expiry that is not a time", with("expiresAt", "next week"), 0o600, "is not a key ryclaude kept"},
		// What a daemon named, and a terminal would obey.
		{"an escape in the key's name", with("name", "ryclaude\x1b[2J"), 0o600, "it holds what is not text"},
		{"a carriage return in its owner", with("owner", "mallory@example.com\rada@example.com"), 0o600, "it holds what is not text"},
		{"a bidi override in its owner", with("owner", "ada\u202egro.elpmaxe@"), 0o600, "it holds what is not text"},
		{"a line break in its id", with("keyId", "k1234567\nstatus:  accepted by the daemon"), 0o600, "it holds what is not text"},
		{"a NUL in the key", with("key", "rysk_k1234567_\x00"), 0o600, "it holds what is not text"},
		{"a daemon reached in the clear", with("url", "http://sandboxes.example.com"), 0o600, "names a daemon ryclaude would not have logged in to"},
		{"a daemon's address login would have tidied", with("url", "https://sandboxes.example.com/"), 0o600, "names a daemon ryclaude would not have logged in to"},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := configHome(t)
			keptAs(t, path, c.body, c.mode)
			_, found, err := loadGrant(path)
			if err == nil || found || !strings.Contains(err.Error(), c.said) || !strings.Contains(err.Error(), path) {
				t.Fatalf("%v %v, want it to say %q and name %s", found, err, c.said, path)
			}
			if strings.Contains(err.Error(), "rysk_") {
				t.Errorf("the key is in what was said: %v", err)
			}
		})
	}
}

func TestAKeyThatCannotBeReadIsNotNoKey(t *testing.T) {
	t.Run("its directory is a file", func(t *testing.T) {
		path := configHome(t)
		keptAs(t, filepath.Dir(path), "", 0o600)
		if _, found, err := loadGrant(path); err == nil || found || !strings.Contains(err.Error(), path) {
			t.Fatalf("%v %v", found, err)
		}
	})
	t.Run("it is a directory", func(t *testing.T) {
		path := configHome(t)
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatal(err)
		}
		if _, found, err := loadGrant(path); err == nil || found || !strings.Contains(err.Error(), path) {
			t.Fatalf("%v %v", found, err)
		}
	})
}

func TestAKeyIsForgotten(t *testing.T) {
	path := configHome(t)
	if err := forgetGrant(path); err != nil {
		t.Fatalf("forgetting what was never kept: %v", err)
	}
	if err := granted().save(path); err != nil {
		t.Fatal(err)
	}
	if err := forgetGrant(path); err != nil {
		t.Fatal(err)
	}
	if _, found, err := loadGrant(path); found || err != nil {
		t.Fatalf("still there: %v %v", found, err)
	}
	// What cannot be removed is said.
	if err := os.MkdirAll(filepath.Join(path, "in the way"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := forgetGrant(path); err == nil || !strings.Contains(err.Error(), "removing ryclaude's key") {
		t.Fatalf("%v", err)
	}
}

func TestTheKeyIsKeptWithThisPersonsConfiguration(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)
	if path, err := grantPath(); err != nil || path != filepath.Join(home, ".config", "ryclaude", "credentials.json") {
		t.Errorf("with a home alone: %s %v", path, err)
	}
	t.Setenv("HOME", "")
	if path, err := grantPath(); err == nil || !strings.Contains(err.Error(), "finding where ryclaude keeps its key") {
		t.Errorf("with nowhere: %s %v", path, err)
	}
}

func TestAKeyExpiresAtTheMomentItSays(t *testing.T) {
	g := granted()
	g.ExpiresAt = time.Now().Add(-time.Second)
	if !g.expired() {
		t.Error("a key a second past its expiry has not expired")
	}
	g.ExpiresAt = time.Now().Add(time.Hour)
	if g.expired() {
		t.Error("a key with an hour left has expired")
	}
	at := time.Date(2026, 10, 8, 21, 6, 29, 0, time.UTC)
	if got := when(at); got != at.Local().Format("2006-01-02 15:04 MST") {
		t.Errorf("said as %q", got)
	}
}

func TestADaemonsAddress(t *testing.T) {
	for raw, want := range map[string]string{
		"https://sandboxes.example.com":       "https://sandboxes.example.com",
		"https://sandboxes.example.com/":      "https://sandboxes.example.com",
		"HTTPS://Sandboxes.Example.COM:8443/": "https://sandboxes.example.com:8443",
		"https://10.0.0.7:8099":               "https://10.0.0.7:8099",
		// Plain HTTP, to this machine and no further.
		"http://127.0.0.1:8099":  "http://127.0.0.1:8099",
		"http://127.0.0.1:8099/": "http://127.0.0.1:8099",
		"http://localhost:8099":  "http://localhost:8099",
		"http://LOCALHOST":       "http://localhost",
		"http://[::1]:8099":      "http://[::1]:8099",
		"http://127.8.9.10":      "http://127.8.9.10",
	} {
		if got, err := daemonURL(raw); err != nil || got != want {
			t.Errorf("%s is %q %v, want %q", raw, got, err, want)
		}
	}
	for raw, said := range map[string]string{
		"":                                          "is not a daemon's address",
		"sandboxes.example.com":                     "is not a daemon's address",
		"sandboxes.example.com:8099":                "is not a daemon's address",
		"https://":                                  "is not a daemon's address",
		"https://:8099":                             "is not a daemon's address",
		"https:sandboxes.example.com":               "is not a daemon's address",
		"ftp://sandboxes.example.com":               "is not a daemon's address",
		"ws://127.0.0.1:8099":                       "is not a daemon's address",
		"https://sandboxes.example.com:port":        "is not a daemon's address",
		"https://sandboxes.example.com/%zz":         "is not a daemon's address",
		"https://ada:hunter2@sandboxes.example.com": "a user or a password",
		"https://ada@sandboxes.example.com":         "a user or a password",
		"https://sandboxes.example.com/console":     "a path, a query or a fragment",
		"https://sandboxes.example.com//":           "a path, a query or a fragment",
		"https://sandboxes.example.com/?next=x":     "a path, a query or a fragment",
		"https://sandboxes.example.com?":            "a path, a query or a fragment",
		"https://sandboxes.example.com#top":         "a path, a query or a fragment",
		// The key would cross the network for anybody to read.
		"http://sandboxes.example.com":      "use https://sandboxes.example.com",
		"http://10.0.0.7:8099":              "use https://10.0.0.7:8099",
		"http://127.0.0.1.example.com":      "plain HTTP to another machine",
		"http://localhost.example.com:8099": "plain HTTP to another machine",
		"http://[2001:db8::1]:8099":         "plain HTTP to another machine",
		"http://0.0.0.0:8099":               "plain HTTP to another machine",
	} {
		got, err := daemonURL(raw)
		if err == nil || got != "" || !strings.Contains(err.Error(), said) {
			t.Errorf("%s is %q %v, want it refused as %q", raw, got, err, said)
			continue
		}
		if strings.Contains(err.Error(), "hunter2") {
			t.Errorf("the password is in what was said: %v", err)
		}
	}
}
