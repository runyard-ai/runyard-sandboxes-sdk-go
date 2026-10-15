package sdk

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/google/uuid"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakedaemon"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/spawn"
)

// namespacedCalls is every call of a client that is about something in a
// namespace, each made once.
func namespacedCalls(ctx context.Context, client *Client) map[string]func() error {
	return map[string]func() error{
		"Create":  func() error { _, err := client.Create(ctx, "", Spec{Image: "alpine"}); return err },
		"Open":    func() error { _, err := client.Open(ctx, uuid.New()); return err },
		"List":    func() error { _, err := client.List(ctx); return err },
		"Counts":  func() error { _, err := client.Counts(ctx); return err },
		"Costs":   func() error { _, err := client.Costs(ctx, genv1.CostWindow24h); return err },
		"Brokers": func() error { _, err := client.Brokers(ctx); return err },
		"HostTunnels": func() error {
			_, err := client.HostTunnels(ctx)
			return err
		},
		"Volumes":      func() error { _, err := client.Volumes(ctx); return err },
		"DeleteVolume": func() error { return client.DeleteVolume(ctx, "chat") },
	}
}

// sent is how many requests about something in a namespace the daemon was
// asked, whatever they were answered.
func sent(d *fakedaemon.Daemon) int {
	n := 0
	for _, operation := range []string{"createSandbox", "getSandbox", "listSandboxes", "getCounts", "listHostTunnels", "listVolumes", "deleteVolume"} {
		n += d.Calls(operation)
	}
	return n
}

// A key bound to a namespace binds the client to it: every call goes there,
// with nothing more said, and who the key is was asked once for all of them.
func TestAKeyBoundToANamespaceBindsTheClient(t *testing.T) {
	d, client := fake(t, fakedaemon.WithNamespaces("acme", "globex"))
	ctx := t.Context()
	sandbox, err := client.Create(ctx, "one", Spec{Image: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.Namespace != "acme" {
		t.Errorf("the sandbox is in %q, want acme", sandbox.Namespace)
	}
	if _, err := sandbox.Run(ctx, "true"); err != nil {
		t.Fatal(err)
	}
	opened, err := client.Open(ctx, sandbox.ID)
	if err != nil || opened.Namespace != "acme" {
		t.Fatalf("open = %v %+v", err, opened)
	}
	for name, call := range namespacedCalls(ctx, client) {
		if err := call(); errors.Is(err, ErrNamespaceRequired) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if got, err := client.ResolveNamespace(ctx); err != nil || got != "acme" {
		t.Errorf("ResolveNamespace = %q %v", got, err)
	}
	if got, err := client.Namespaces(ctx); err != nil || !slices.Equal(got, []string{"acme"}) {
		t.Errorf("Namespaces = %v %v", got, err)
	}
	if asked := d.Calls("getMe"); asked != 1 {
		t.Errorf("who the key is was asked %d times, want once", asked)
	}
}

// A global key has no namespace to infer: every namespaced call fails
// without sending anything, until the client is told one.
func TestAGlobalKeyNamesItsNamespace(t *testing.T) {
	d, client := fake(t, fakedaemon.WithNamespaces("acme", "globex"), fakedaemon.WithGlobalKeys())
	ctx := t.Context()
	for name, call := range namespacedCalls(ctx, client) {
		if err := call(); !errors.Is(err, ErrNamespaceRequired) {
			t.Errorf("%s = %v, want ErrNamespaceRequired", name, err)
		}
	}
	if n := sent(d); n != 0 {
		t.Errorf("%d requests were sent that could name no namespace", n)
	}
	// What is about no namespace is still answered.
	if _, err := client.Info(ctx); err != nil {
		t.Error(err)
	}
	if got, err := client.Namespaces(ctx); err != nil || !slices.Equal(got, []string{"acme", "globex"}) {
		t.Errorf("Namespaces = %v %v", got, err)
	}

	acme, globex := client.Namespace("acme"), client.Namespace("globex")
	sandbox, err := acme.Create(ctx, "one", Spec{Image: "alpine"})
	if err != nil || sandbox.Namespace != "acme" {
		t.Fatalf("create in acme = %v %+v", err, sandbox)
	}
	if listed, err := acme.List(ctx); err != nil || len(listed) != 1 {
		t.Errorf("acme's = %v %v", listed, err)
	}
	if listed, err := globex.List(ctx); err != nil || len(listed) != 0 {
		t.Errorf("globex's = %v %v", listed, err)
	}
	if _, err := globex.Open(ctx, sandbox.ID); CodeOf(err) != "not_found" {
		t.Errorf("acme's sandbox, opened in globex = %v", err)
	}
	// Bound to acme, the derived client is the parent's otherwise: its key,
	// and the one answer to who that is.
	if _, err := sandbox.Run(ctx, "true"); err != nil {
		t.Error(err)
	}
	if asked := d.Calls("getMe"); asked != 1 {
		t.Errorf("who the key is was asked %d times, want once across the clients made from one", asked)
	}
	// The parent is as it was.
	if _, err := client.List(ctx); !errors.Is(err, ErrNamespaceRequired) {
		t.Errorf("the parent after Namespace = %v", err)
	}
}

// Naming a namespace other than the one a key is bound to fails in the SDK,
// naming both, and sends nothing a 404 would be the only answer to.
func TestANamespaceAKeyIsNotBoundToFailsAtOnce(t *testing.T) {
	d, client := fake(t, fakedaemon.WithNamespaces("acme", "globex"))
	ctx := t.Context()
	globex := client.Namespace("globex")
	for name, call := range namespacedCalls(ctx, globex) {
		err := call()
		if err == nil || !strings.Contains(err.Error(), "acme") || !strings.Contains(err.Error(), "globex") {
			t.Errorf("%s = %v, want an error naming acme and globex", name, err)
		}
	}
	if n := sent(d); n != 0 {
		t.Errorf("%d requests were sent to a namespace the key is not bound to", n)
	}
	// Its own, named, is as good as inferred; and empty is the key's.
	for _, name := range []string{"acme", ""} {
		if _, err := client.Namespace(name).List(ctx); err != nil {
			t.Errorf("Namespace(%q).List = %v", name, err)
		}
	}
}

// WithNamespace spares the question, and the daemon still holds the key to
// the namespace: it is the daemon that refuses one the key does not reach.
func TestWithNamespaceAsksNothingOfTheKey(t *testing.T) {
	d := fakedaemon.New(t, fakedaemon.WithKey("k"), fakedaemon.WithNamespaces("acme", "globex"))
	ctx := t.Context()
	acme, err := New(d.URL, WithKey("k"), WithNamespace("acme"))
	if err != nil {
		t.Fatal(err)
	}
	sandbox, err := acme.Create(ctx, "", Spec{Image: "alpine"})
	if err != nil || sandbox.Namespace != "acme" {
		t.Fatalf("create = %v %+v", err, sandbox)
	}
	globex, err := New(d.URL, WithKey("k"), WithNamespace("globex"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := globex.List(ctx); CodeOf(err) != "not_found" {
		t.Errorf("a namespace the key is not bound to, named = %v, want the daemon's not_found", err)
	}
	if asked := d.Calls("getMe"); asked != 0 {
		t.Errorf("who the key is was asked %d times, want never", asked)
	}
	// Named again with Namespace, it is checked like any other.
	if _, err := acme.Namespace("globex").List(ctx); err == nil || CodeOf(err) != "" {
		t.Errorf("Namespace(globex) on a client told acme = %v, want the SDK's own error", err)
	}
	if got, err := acme.Namespaces(ctx); err != nil || !slices.Equal(got, []string{"acme"}) {
		t.Errorf("Namespaces = %v %v", got, err)
	}
}

// A daemon that will not say who the key is fails the call that needed to
// know, with its refusal — and is asked again by the next, rather than the
// client failing for life on what went wrong once.
func TestWhoTheKeyIsIsAskedAgainUntilItIsAnswered(t *testing.T) {
	d, client := fake(t)
	ctx := t.Context()
	d.Intercept("getMe", fakedaemon.Refuse(http.StatusServiceUnavailable, genv1.ErrorCodeUpstreamError, "the database is not answering"))
	if _, err := client.List(ctx); CodeOf(err) != "upstream_error" {
		t.Fatalf("list = %v, want the refusal", err)
	}
	if _, err := client.Namespaces(ctx); err != nil {
		t.Fatalf("namespaces after = %v", err)
	}
	if _, err := client.List(ctx); err != nil {
		t.Fatalf("list after = %v", err)
	}
	if asked := d.Calls("getMe"); asked != 2 {
		t.Errorf("who the key is was asked %d times, want twice: once refused, once answered", asked)
	}

	for name, intercept := range map[string]fakedaemon.Interceptor{
		"a body that is not the contract's": fakedaemon.Answer(http.StatusOK, "application/json", "[]"),
		"a hang-up":                         fakedaemon.HangUp(),
	} {
		t.Run(name, func(t *testing.T) {
			d, client := fake(t)
			d.Intercept("getMe", intercept)
			if _, err := client.ResolveNamespace(t.Context()); err == nil {
				t.Error("resolved a namespace from no answer")
			}
			if _, err := client.Namespaces(t.Context()); err != nil {
				t.Errorf("namespaces after = %v", err)
			}
		})
	}
}

// A key that reaches no namespace — an answer without the field — is told
// so as an empty list, not nil.
func TestNamespacesOfAnAnswerWithoutThemIsEmpty(t *testing.T) {
	d, client := fake(t)
	d.Intercept("getMe", fakedaemon.Answer(http.StatusOK, "application/json",
		`{"admin":false,"email":"ada@example.com","scopes":[],"via":"key","key":{"id":"k1","name":"k","sandboxes":"all"}}`))
	got, err := client.Namespaces(t.Context())
	if err != nil || got == nil || len(got) != 0 {
		t.Fatalf("Namespaces = %#v %v", got, err)
	}
	if _, err := client.ResolveNamespace(t.Context()); !errors.Is(err, ErrNamespaceRequired) {
		t.Errorf("a key bound to nothing = %v", err)
	}
}

// Concurrent first calls on one client, and on the clients made from it,
// ask who the key is once between them.
func TestConcurrentFirstCallsAskOnce(t *testing.T) {
	d, client := fake(t)
	const callers = 16
	start := make(chan struct{})
	failed := make(chan error, callers)
	waits := make([]func(), 0, callers)
	for i := range callers {
		caller := client
		if i%2 == 1 {
			caller = client.Namespace("")
		}
		waits = append(waits, spawn.Go(t, func() {
			<-start
			_, err := caller.List(t.Context())
			failed <- err
		}))
	}
	close(start)
	for _, wait := range waits {
		wait()
	}
	for range callers {
		if err := <-failed; err != nil {
			t.Error(err)
		}
	}
	if asked := d.Calls("getMe"); asked != 1 {
		t.Errorf("who the key is was asked %d times by %d callers at once, want once", asked, callers)
	}
}

// The callers that arrive while one asks wait for its answer rather than
// asking too, and all of them are given it.
func TestWhoamiAsksOnceForEveryoneWaiting(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var w whoami
		var asks atomic.Int32
		release := make(chan struct{})
		answer := &genv1.Me{Email: "ada@example.com"}
		ask := func(ctx context.Context) (*genv1.Me, error) {
			asks.Add(1)
			<-release
			return answer, ctx.Err()
		}
		const callers = 8
		got := make(chan *genv1.Me, callers)
		for range callers {
			spawn.Go(t, func() {
				me, err := w.get(t.Context(), ask)
				if err != nil {
					t.Error(err)
				}
				got <- me
			})
		}
		synctest.Wait() // every caller is either asking or waiting on the one that is
		if n := asks.Load(); n != 1 {
			t.Fatalf("%d asked at once, want 1", n)
		}
		close(release)
		for range callers {
			if me := <-got; me != answer {
				t.Errorf("a caller was given %v", me)
			}
		}
		// Kept: the next caller asks nothing.
		if me, err := w.get(t.Context(), ask); err != nil || me != answer || asks.Load() != 1 {
			t.Errorf("after = %v %v, asked %d times", me, err, asks.Load())
		}
	})
}

// When the one asking fails, a caller that was waiting asks in turn; one
// whose own context ends while it waits stops waiting.
func TestWhoamiAsksAgainWhenTheAskerFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var w whoami
		var asks atomic.Int32
		release := make(chan struct{})
		answer := &genv1.Me{Email: "ada@example.com"}
		lost := errors.New("the line was lost")
		ask := func(context.Context) (*genv1.Me, error) {
			if asks.Add(1) == 1 {
				<-release
				return nil, lost
			}
			return answer, nil
		}
		first := make(chan error, 1)
		spawn.Go(t, func() {
			_, err := w.get(t.Context(), ask)
			first <- err
		})
		synctest.Wait()

		impatient, cancel := context.WithCancel(t.Context())
		gaveUp := make(chan error, 1)
		spawn.Go(t, func() {
			_, err := w.get(impatient, ask)
			gaveUp <- err
		})
		second := make(chan *genv1.Me, 1)
		spawn.Go(t, func() {
			me, err := w.get(t.Context(), ask)
			if err != nil {
				t.Error(err)
			}
			second <- me
		})
		synctest.Wait()
		cancel()
		if err := <-gaveUp; !errors.Is(err, context.Canceled) {
			t.Errorf("a waiter whose context ended = %v", err)
		}

		close(release)
		if err := <-first; !errors.Is(err, lost) {
			t.Errorf("the asker = %v, want its own failure", err)
		}
		if me := <-second; me != answer {
			t.Errorf("the waiter that asked in turn was given %v", me)
		}
		if n := asks.Load(); n != 2 {
			t.Errorf("asked %d times, want twice", n)
		}
	})
}

// A sandbox's handle is in the namespace its body says, and in the one it was
// asked of when the body does not say.
func TestAHandleIsInTheNamespaceItsBodySays(t *testing.T) {
	id := "0192f7a4-5b1e-7c3d-9a2f-4e6b8c1d0a53"
	for name, c := range map[string]struct{ body, want string }{
		"one that says":       {`{"id":"` + id + `","state":"ready","namespace":"elsewhere"}`, "elsewhere"},
		"one that does not":   {`{"id":"` + id + `","state":"ready"}`, "acme"},
		"one that says empty": {`{"id":"` + id + `","state":"ready","namespace":""}`, "acme"},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/v1/namespaces/acme/sandboxes/"+id {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(c.body))
			}))
			t.Cleanup(server.Close)
			client, err := New(server.URL, WithNamespace("acme"))
			if err != nil {
				t.Fatal(err)
			}
			sandbox, err := client.Open(t.Context(), uuid.MustParse(id))
			if err != nil || sandbox.Namespace != c.want {
				t.Fatalf("open = %v %+v, want it in %s", err, sandbox, c.want)
			}
		})
	}
}
