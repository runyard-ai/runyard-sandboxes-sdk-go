package main

import (
	"bytes"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sdk"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakedaemon"
)

func TestAuthTakesOneOfItsCommands(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		exit int
	}{
		{"nothing", nil, 2},
		{"a command it does not have", []string{"whoami"}, 2},
		{"a command of another case", []string{"Login"}, 2},
		{"a flag before the command", []string{"-url", "https://sandboxes.example.com", "login"}, 2},
		{"an empty command", []string{""}, 2},
		{"help", []string{"-h"}, 0},
		{"help, spelled out", []string{"--help"}, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			configHome(t)
			p := &person{t: t}
			var stderr bytes.Buffer
			if code := run(t.Context(), append([]string{"auth"}, c.args...), p.at(typing("")), &stderr); code != c.exit {
				t.Fatalf("exit %d, want %d: %s", code, c.exit, stderr.String())
			}
			for _, command := range []string{"ryclaude auth login -url", "ryclaude auth logout", "ryclaude auth status"} {
				if !strings.Contains(stderr.String(), command) {
					t.Errorf("the usage does not say %q: %s", command, stderr.String())
				}
			}
			if p.opened != 0 {
				t.Error("a browser was opened")
			}
		})
	}
}

// loggedOut fails when a key is still kept.
func loggedOut(t *testing.T) {
	t.Helper()
	path, err := grantPath()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("the key is still kept: %v", err)
	}
}

func auth(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	con := typing("")
	var said bytes.Buffer
	code = run(t.Context(), append([]string{"auth"}, args...), at(con), &said)
	return code, con.out.String(), said.String()
}

func TestLoggingOutRevokesTheKeyAndForgetsIt(t *testing.T) {
	configHome(t)
	d := signingIn(t)
	g := logIn(t, d)
	code, _, said := auth(t, "logout")
	if code != 0 || !strings.Contains(said, "is revoked at "+d.URL) || !strings.Contains(said, g.KeyID) || !strings.Contains(said, "logged out") {
		t.Fatalf("exit %d: %s", code, said)
	}
	if strings.Contains(said, g.Key) {
		t.Errorf("the key is in what was said: %s", said)
	}
	if keys := d.Keys(); len(keys) != 0 {
		t.Errorf("the daemon still has %+v", keys)
	}
	loggedOut(t)

	// Twice is once.
	if code, _, said := auth(t, "logout"); code != 0 || !strings.Contains(said, "not logged in: there is nothing to log out of") || d.Calls("revokeKey") != 1 {
		t.Errorf("a second logout: exit %d, %d revokes: %s", code, d.Calls("revokeKey"), said)
	}
}

func TestLoggingOutWithNothingKept(t *testing.T) {
	configHome(t)
	for _, args := range [][]string{{"logout"}, {"logout", "-forget"}} {
		if code, _, said := auth(t, args...); code != 0 || !strings.Contains(said, "there is nothing to log out of") {
			t.Errorf("%v: exit %d: %s", args, code, said)
		}
	}
}

func TestLoggingOutOfADaemonThatDoesNotRevoke(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer fakedaemon.Interceptor
		// exit is what logout exits with, and forgot whether the key is
		// gone from here, when -forget was not said.
		exit   int
		forgot bool
		said   string
	}{
		// The key is dead already, which is what was asked for.
		{"it no longer takes the key", fakedaemon.Refuse(http.StatusUnauthorized, genv1.ErrorCodeUnauthorized, "no key, or one this daemon will not accept"), 0, true, "had already stopped accepting the key"},
		// The file is all that can still revoke the key: it stays.
		{"it hangs up", fakedaemon.HangUp(), 1, false, "was not revoked: reaching DAEMON: "},
		{"it fails", fakedaemon.Refuse(http.StatusInternalServerError, genv1.ErrorCodeInternal, "the database is gone"), 1, false, "was not revoked: runyard-sandboxes: internal: the database is gone"},
		{"it refuses", fakedaemon.Refuse(http.StatusForbidden, genv1.ErrorCodeForbidden, "not yours"), 1, false, "was not revoked: runyard-sandboxes: forbidden: not yours"},
		{"it is a proxy with nothing behind it", fakedaemon.Answer(http.StatusBadGateway, "text/html", "<h1>Bad gateway</h1>"), 1, false, "was not revoked: it answered HTTP 502"},
	} {
		t.Run(c.name, func(t *testing.T) {
			configHome(t)
			d := signingIn(t)
			g := logIn(t, d)
			want := strings.ReplaceAll(c.said, "DAEMON", d.URL)

			d.Intercept("revokeKey", c.answer)
			code, _, said := auth(t, "logout")
			if code != c.exit || !strings.Contains(said, want) || strings.Contains(said, g.Key) {
				t.Fatalf("exit %d, want %d and %q: %s", code, c.exit, want, said)
			}
			if c.forgot {
				loggedOut(t)
				return
			}
			if !strings.Contains(said, "still logged in") || !strings.Contains(said, "ryclaude auth logout -forget") || strings.Contains(said, "logged out") {
				t.Errorf("said: %s", said)
			}
			if still := kept(t); still != g {
				t.Errorf("kept %+v, want it as it was", still)
			}

			// Told to forget it all the same, it does, and says what is
			// left behind.
			d.Intercept("revokeKey", c.answer)
			code, _, said = auth(t, "logout", "-forget")
			if code != 0 || !strings.Contains(said, "warning: ") || !strings.Contains(said, want) || !strings.Contains(said, "It works until "+when(g.ExpiresAt)) ||
				!strings.Contains(said, "logged out") || strings.Contains(said, g.Key) {
				t.Fatalf("with -forget: exit %d: %s", code, said)
			}
			loggedOut(t)
			if len(d.Keys()) != 1 {
				t.Errorf("the daemon has %d keys, and revoked none", len(d.Keys()))
			}
		})
	}
}

// The key is sent to the daemon that issued it, to be revoked, and to no
// other: a daemon that redirects is not followed.
func TestLoggingOutFollowsNoRedirect(t *testing.T) {
	configHome(t)
	d, elsewhere := signingIn(t), signingIn(t)
	logIn(t, d)
	d.Intercept("revokeKey", func(w http.ResponseWriter, r *http.Request, _ http.HandlerFunc) {
		http.Redirect(w, r, elsewhere.URL+r.URL.Path, http.StatusTemporaryRedirect)
	})
	if code, _, said := auth(t, "logout"); code != 1 || !strings.Contains(said, "it answered HTTP 307") {
		t.Fatalf("exit %d: %s", code, said)
	}
	if elsewhere.Calls("revokeKey") != 0 {
		t.Error("the key was sent where the redirect said")
	}
	kept(t)
}

func TestLoggingOutOfWhatCannotBeRead(t *testing.T) {
	path := configHome(t)
	keptAs(t, path, "{", 0o600)
	code, _, said := auth(t, "logout")
	if code != 1 || !strings.Contains(said, path+" is not a key ryclaude kept") || !strings.Contains(said, "`ryclaude auth logout -forget` removes it") {
		t.Fatalf("exit %d: %s", code, said)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("it was removed: %v", err)
	}
	code, _, said = auth(t, "logout", "-forget")
	if code != 0 || !strings.Contains(said, "warning: ") || !strings.Contains(said, "is not revoked") || !strings.Contains(said, "logged out") {
		t.Fatalf("with -forget: exit %d: %s", code, said)
	}
	loggedOut(t)
}

func TestLoggingOutThatCannotForget(t *testing.T) {
	// Something that is not a file, and holds something: it cannot be read,
	// and cannot be removed.
	path := configHome(t)
	if err := os.MkdirAll(filepath.Join(path, "in the way"), 0o700); err != nil {
		t.Fatal(err)
	}
	if code, _, said := auth(t, "logout", "-forget"); code != 1 || !strings.Contains(said, "removing ryclaude's key") {
		t.Fatalf("exit %d: %s", code, said)
	}
}

func TestLogoutAndStatusTakeNothingElse(t *testing.T) {
	for _, c := range []struct {
		args []string
		exit int
		said string
	}{
		{[]string{"logout", "now"}, 2, `it takes no "now"`},
		{[]string{"logout", "-url", "https://sandboxes.example.com"}, 2, "flag provided but not defined: -url"},
		{[]string{"logout", "-h"}, 0, "-forget"},
		{[]string{"status", "now"}, 2, `it takes no "now"`},
		{[]string{"status", "-forget"}, 2, "flag provided but not defined: -forget"},
		{[]string{"status", "-h"}, 0, "ryclaude auth status"},
	} {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			configHome(t)
			d := signingIn(t)
			g := logIn(t, d)
			code, stdout, said := auth(t, c.args...)
			if code != c.exit || !strings.Contains(said, c.said) || stdout != "" {
				t.Fatalf("exit %d, want %d and %q: %s%s", code, c.exit, c.said, stdout, said)
			}
			if still := kept(t); still != g || len(d.Keys()) != 1 {
				t.Error("the key was touched")
			}
		})
	}
}

func TestWithNowhereAKeyIsKeptThereIsNothingToSay(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	for _, command := range []string{"logout", "status"} {
		if code, stdout, said := auth(t, command); code != 1 || !strings.Contains(said, "finding where ryclaude keeps its key") || stdout != "" {
			t.Errorf("%s: exit %d: %s%s", command, code, stdout, said)
		}
	}
}

func TestStatusSaysWhatIsKeptAndWhetherItStillWorks(t *testing.T) {
	configHome(t)
	d := signingIn(t)
	g := logIn(t, d)
	code, stdout, said := auth(t, "status")
	want := "daemon:  " + d.URL + "\n" +
		"key:     ryclaude on laptop (" + g.KeyID + ")\n" +
		"owner:   ada@example.com\n" +
		"expires: " + when(g.ExpiresAt) + "\n" +
		"status:  accepted by the daemon\n"
	if code != 0 || stdout != want || said != "" {
		t.Fatalf("exit %d:\n%s%s\nwant:\n%s", code, stdout, said, want)
	}
	if d.Calls("getMe") != 1 {
		t.Errorf("the daemon was asked %d times", d.Calls("getMe"))
	}
}

func TestStatusOfAKeyThatDoesNotWork(t *testing.T) {
	for _, c := range []struct {
		name string
		// breaking makes the key stop working, there or here.
		breaking func(t *testing.T, d *fakedaemon.Daemon, g grant)
		status   string
		// asked is whether the daemon was: how often is the transport's,
		// which asks again a question that changes nothing when its
		// connection is hung up.
		asked bool
	}{
		{"revoked at the daemon", func(t *testing.T, d *fakedaemon.Daemon, g grant) {
			t.Helper()
			operator, err := sdk.New(d.URL, sdk.WithKey("admin"))
			if err != nil {
				t.Fatal(err)
			}
			if res, err := operator.API().RevokeKeyWithResponse(t.Context(), g.KeyID); err != nil || res.StatusCode() != http.StatusNoContent {
				t.Fatalf("revoking: %v", err)
			}
		}, "status:  no longer accepted by the daemon, where it was revoked or has expired: run `ryclaude auth login -url DAEMON` again\n", true},
		// Not asked: an expired key is not sent anywhere again.
		{"expired", func(t *testing.T, _ *fakedaemon.Daemon, g grant) {
			t.Helper()
			g.ExpiresAt = time.Now().Add(-time.Minute)
			path, _ := grantPath()
			if err := g.save(path); err != nil {
				t.Fatal(err)
			}
		}, "status:  expired: run `ryclaude auth login -url DAEMON` again\n", false},
		{"a daemon that hangs up", func(_ *testing.T, d *fakedaemon.Daemon, _ grant) {
			d.InterceptAll("getMe", fakedaemon.HangUp())
		}, "status:  unknown: reaching DAEMON: ", true},
		{"a daemon that fails", func(_ *testing.T, d *fakedaemon.Daemon, _ grant) {
			d.Intercept("getMe", fakedaemon.Refuse(http.StatusInternalServerError, genv1.ErrorCodeInternal, "the database is gone"))
		}, "status:  unknown: runyard-sandboxes: internal: the database is gone\n", true},
		{"a proxy with nothing behind it", func(_ *testing.T, d *fakedaemon.Daemon, _ grant) {
			d.Intercept("getMe", fakedaemon.Answer(http.StatusBadGateway, "text/html", "<h1>Bad gateway</h1>"))
		}, "status:  unknown: it answered HTTP 502, which is not what a runyard-sandboxes daemon answers\n", true},
		{"a daemon that sends the key elsewhere", func(_ *testing.T, d *fakedaemon.Daemon, _ grant) {
			d.Intercept("getMe", func(w http.ResponseWriter, r *http.Request, _ http.HandlerFunc) {
				http.Redirect(w, r, "http://127.0.0.1:1/v1/me", http.StatusFound)
			})
		}, "status:  unknown: it answered HTTP 302", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			configHome(t)
			d := signingIn(t)
			g := logIn(t, d)
			c.breaking(t, d, g)
			code, stdout, said := auth(t, "status")
			want := strings.ReplaceAll(c.status, "DAEMON", d.URL)
			if code != 1 || !strings.Contains(stdout, want) || !strings.HasPrefix(stdout, "daemon:  "+d.URL+"\nkey:     ryclaude on laptop ("+g.KeyID+")\nowner:   ada@example.com\nexpires: ") {
				t.Fatalf("exit %d, want 1 and %q:\n%s%s", code, want, stdout, said)
			}
			if strings.Contains(stdout+said, g.Key) {
				t.Errorf("the key is in what was said: %s%s", stdout, said)
			}
			if asked := d.Calls("getMe"); (asked > 0) != c.asked {
				t.Errorf("the daemon was asked %d times", asked)
			}
			// Saying what is kept changes nothing of it.
			if still := kept(t); still.KeyID != g.KeyID {
				t.Error("the key was touched")
			}
		})
	}
}

func TestStatusWithNothingToSay(t *testing.T) {
	t.Run("not logged in", func(t *testing.T) {
		configHome(t)
		if code, stdout, said := auth(t, "status"); code != 1 || stdout != "" || !strings.Contains(said, "not logged in: log in with `ryclaude auth login -url") {
			t.Fatalf("exit %d: %s%s", code, stdout, said)
		}
	})
	t.Run("what is kept cannot be read", func(t *testing.T) {
		path := configHome(t)
		keptAs(t, path, `{"key":"rysk_k1234567_secret"`, 0o600)
		if code, stdout, said := auth(t, "status"); code != 1 || stdout != "" || !strings.Contains(said, path+" is not a key ryclaude kept") || strings.Contains(said, "rysk_") {
			t.Fatalf("exit %d: %s%s", code, stdout, said)
		}
	})
	t.Run("what is kept can be read by others", func(t *testing.T) {
		path := configHome(t)
		if err := granted().save(path); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(path, 0o644); err != nil {
			t.Fatal(err)
		}
		if code, stdout, said := auth(t, "status"); code != 1 || stdout != "" || !strings.Contains(said, "chmod 600 "+path) {
			t.Fatalf("exit %d: %s%s", code, stdout, said)
		}
	})
}

func TestWhatADaemonAnsweredIsSaidWithoutItsBody(t *testing.T) {
	refusal := answered(http.StatusForbidden, []byte(`{"error":{"code":"forbidden","message":"not yours"}}`))
	if got, ok := errors.AsType[*sdk.Error](refusal); !ok || got.Status != http.StatusForbidden || got.Code != "forbidden" || got.Message != "not yours" {
		t.Errorf("the contract's refusal: %v", refusal)
	}
	for name, body := range map[string]string{
		"a key":                     `{"id":"k1234567","secret":"rysk_k1234567_secret"}`,
		"an envelope with no code":  `{"error":{"message":"rysk_k1234567_secret"}}`,
		"what is not JSON":          "rysk_k1234567_secret",
		"nothing":                   "",
		"an envelope of other type": `{"error":"rysk_k1234567_secret"}`,
	} {
		err := answered(http.StatusOK, []byte(body))
		if err == nil || !strings.Contains(err.Error(), "it answered HTTP 200") || strings.Contains(err.Error(), "rysk_") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestADaemonIsDialledWhereItIsAndNowhereElse(t *testing.T) {
	if _, err := dial("sandboxes.example.com", "k"); err == nil || !strings.Contains(err.Error(), "is not a daemon's address") {
		t.Errorf("an address with no scheme: %v", err)
	}
	g := granted()
	g.URL = "nowhere"
	for name, err := range map[string]error{"revoking": revoke(t.Context(), g), "asking": accepted(t.Context(), g)} {
		if err == nil || !strings.Contains(err.Error(), "is not a daemon's address") || strings.Contains(err.Error(), g.Key) {
			t.Errorf("%s at an address that is not one: %v", name, err)
		}
	}
}
