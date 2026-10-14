package fakedaemon

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// raw is one request to the fake, answered as its status and body.
func raw(t *testing.T, d *Daemon, method, path, key string) (int, string) {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, d.URL+path, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	return res.StatusCode, string(body)
}

// Every path from before namespaces answers 404 with the contract's error,
// naming where it went, whatever the method and whoever asks: an old client
// fails with a sentence rather than a guess.
func TestThePathsFromBeforeNamespacesSayWhereTheyWent(t *testing.T) {
	d := New(t, WithKey("k"))
	for _, c := range []struct{ method, path, now string }{
		{"GET", "/v1/sandboxes", "/v1/namespaces/{namespace}/sandboxes"},
		{"POST", "/v1/sandboxes", "/v1/namespaces/{namespace}/sandboxes"},
		{"GET", "/v1/sandboxes/0192f7a4-5b1e-7c3d-9a2f-4e6b8c1d0a53/events", "/v1/namespaces/{namespace}/sandboxes/0192f7a4-5b1e-7c3d-9a2f-4e6b8c1d0a53/events"},
		{"DELETE", "/v1/volumes/chat", "/v1/namespaces/{namespace}/volumes/chat"},
		{"GET", "/v1/volumes", "/v1/namespaces/{namespace}/volumes"},
		{"GET", "/v1/builds", "/v1/namespaces/{namespace}/builds"},
		{"GET", "/v1/builds/b1/log", "/v1/namespaces/{namespace}/builds/b1/log"},
		{"GET", "/v1/tunnels", "/v1/namespaces/{namespace}/tunnels"},
		{"GET", "/v1/counts", "/v1/namespaces/{namespace}/counts"},
	} {
		for _, key := range []string{"k", "", "wrong"} {
			status, body := raw(t, d, c.method, c.path, key)
			if status != http.StatusNotFound || !strings.Contains(body, `"not_found"`) || !strings.Contains(body, c.now) {
				t.Errorf("%s %s with key %q = %d %s, want 404 naming %s", c.method, c.path, key, status, body, c.now)
			}
		}
	}
	// What stayed where it was is still answered there.
	if status, body := raw(t, d, "GET", "/v1/info", "k"); status != http.StatusOK {
		t.Errorf("info = %d %s", status, body)
	}
}

// A key bound to one namespace finds every other — one that is there and one
// that is not — answered with the same 404 and the same body, before the
// route looks at anything else.
func TestAKeyBoundToANamespaceFindsNoOther(t *testing.T) {
	d := New(t, WithKey("k"), WithNamespaces("acme", "globex"))
	client := typed(t, d, "k")
	ctx := t.Context()

	made, err := client.CreateSandboxWithResponse(ctx, "acme", &genv1.CreateSandboxParams{IdempotencyKey: "one"}, genv1.SandboxSpec{Image: "alpine"})
	if err != nil || made.JSON202 == nil || *made.JSON202.Namespace != "acme" {
		t.Fatalf("a create in its own namespace = %v %d %s", err, made.StatusCode(), made.Body)
	}
	id := made.JSON202.Id.String()
	var bodies []string
	for _, namespace := range []string{"globex", "nope"} {
		for _, path := range []string{"/sandboxes", "/sandboxes/" + id, "/volumes", "/counts", "/tunnels", "/sandboxes/not-a-uuid"} {
			status, body := raw(t, d, "GET", "/v1/namespaces/"+namespace+path, "k")
			if status != http.StatusNotFound {
				t.Errorf("GET %s%s = %d %s, want 404", namespace, path, status, body)
			}
			bodies = append(bodies, strings.ReplaceAll(body, namespace, "<namespace>"))
		}
	}
	for _, body := range bodies[1:] {
		if body != bodies[0] {
			t.Errorf("one namespace is told apart from another:\n%s\n%s", bodies[0], body)
		}
	}
	if status, body := raw(t, d, "GET", "/v1/namespaces/acme/sandboxes/"+id, "k"); status != http.StatusOK {
		t.Errorf("its own = %d %s", status, body)
	}
	// Asked first and refused first: a wrong key is a 401 whatever the
	// namespace, as it is on every route.
	if status, _ := raw(t, d, "GET", "/v1/namespaces/globex/sandboxes", "wrong"); status != http.StatusUnauthorized {
		t.Errorf("a wrong key = %d, want 401", status)
	}
}

// A global key reaches every namespace the daemon has, and what is in one is
// not there in another.
func TestAGlobalKeyReachesEveryNamespaceAndEachHoldsItsOwn(t *testing.T) {
	d := New(t, WithKey("k"), WithNamespaces("acme", "globex"), WithGlobalKeys())
	client := typed(t, d, "k")
	ctx := t.Context()

	made, err := client.CreateSandboxWithResponse(ctx, "globex", &genv1.CreateSandboxParams{IdempotencyKey: "one"}, genv1.SandboxSpec{Image: "alpine"})
	if err != nil || made.JSON202 == nil || *made.JSON202.Namespace != "globex" {
		t.Fatalf("create in globex = %v %d %s", err, made.StatusCode(), made.Body)
	}
	id := made.JSON202.Id
	if got, err := client.GetSandboxWithResponse(ctx, "acme", id); err != nil || got.StatusCode() != http.StatusNotFound {
		t.Errorf("globex's sandbox, from acme = %d %s", got.StatusCode(), got.Body)
	}
	if got, err := client.DeleteSandboxWithResponse(ctx, "acme", id, &genv1.DeleteSandboxParams{}); err != nil || got.StatusCode() != http.StatusNotFound {
		t.Errorf("deleting globex's sandbox from acme = %d %s", got.StatusCode(), got.Body)
	}
	if status, body := raw(t, d, "GET", "/v1/namespaces/nope/sandboxes", "k"); status != http.StatusNotFound {
		t.Errorf("a namespace the daemon does not have = %d %s", status, body)
	}
	// The same idempotency key in another namespace is another create.
	other, err := client.CreateSandboxWithResponse(ctx, "acme", &genv1.CreateSandboxParams{IdempotencyKey: "one"}, genv1.SandboxSpec{Image: "debian"})
	if err != nil || other.JSON202 == nil || other.JSON202.Id == id {
		t.Fatalf("the same key in acme = %v %d %s", err, other.StatusCode(), other.Body)
	}
}

// A daemon made with no key lets every call into every namespace it has,
// and into none it has not.
func TestADaemonWithNoKeyLetsEveryCallIntoItsNamespaces(t *testing.T) {
	d := New(t, WithNamespaces("acme", "globex"))
	for namespace, want := range map[string]int{"acme": http.StatusOK, "globex": http.StatusOK, "nope": http.StatusNotFound} {
		if status, body := raw(t, d, "GET", "/v1/namespaces/"+namespace+"/sandboxes", ""); status != want {
			t.Errorf("%s = %d %s, want %d", namespace, status, body, want)
		}
	}
}

// A key offered as a WebSocket subprotocol is held to its namespace too, as
// one in the Authorization header is.
func TestAKeyOfferedAsASubprotocolIsHeldToItsNamespace(t *testing.T) {
	d := New(t, WithKey("k"), WithNamespaces("acme", "globex"), WithTerminal(func(context.Context, genv1.SandboxID, *Terminal) int { return 0 }))
	made, err := typed(t, d, "k").CreateSandboxWithResponse(t.Context(), "acme", &genv1.CreateSandboxParams{IdempotencyKey: "one"}, genv1.SandboxSpec{Image: "alpine"})
	if err != nil || made.JSON202 == nil {
		t.Fatalf("create = %v %s", err, made.Body)
	}
	for namespace, want := range map[string]int{"acme": http.StatusSwitchingProtocols, "globex": http.StatusNotFound} {
		ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
		conn, res, err := websocket.Dial(ctx, d.URL+"/v1/namespaces/"+namespace+"/sandboxes/"+made.JSON202.Id.String()+"/terminal",
			&websocket.DialOptions{Subprotocols: []string{terminalProtocol, bearerProtocol + "k"}})
		cancel()
		if res == nil || res.StatusCode != want {
			t.Errorf("%s: %v %v, want %d", namespace, res, err, want)
		}
		if conn != nil {
			_ = conn.Close(websocket.StatusNormalClosure, "")
		}
		if res != nil && res.Body != nil {
			_ = res.Body.Close()
		}
	}
}
