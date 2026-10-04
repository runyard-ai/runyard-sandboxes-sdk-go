package fakedaemon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/spawn"
)

// brokered is a daemon with one ready sandbox that has the host's shared
// broker `claude` and a one-off, `billing`, a client for it, and the
// sandbox's id. A broker a caller serves is given by serving it.
func brokered(t *testing.T, opts ...Option) (*Daemon, *genv1.ClientWithResponses, genv1.SandboxID) {
	t.Helper()
	d := New(t, append([]Option{WithKey("k"), WithBrokers("claude")}, opts...)...)
	client := typed(t, d, "k")
	created, err := client.CreateSandboxWithResponse(t.Context(), &genv1.CreateSandboxParams{IdempotencyKey: "brokered"}, genv1.SandboxSpec{
		Image: "alpine",
		Brokers: &[]genv1.SandboxBrokerSpec{
			{Name: "claude"},
			{Name: "billing", Upstream: &genv1.BrokerUpstream{Url: "https://billing.example.com/"}},
		},
	})
	if err != nil || created.JSON202 == nil {
		t.Fatalf("create = %v %s", err, created.Body)
	}
	return d, client, created.JSON202.Id
}

func servePath(d *Daemon, id, name string) string {
	return d.URL + "/v1/sandboxes/" + id + "/brokers/" + name + "/serve"
}

// answer is what a handshake asking to serve a broker was answered: a socket
// and the port the broker is on, or the status and the code of the refusal.
type answer struct {
	conn   *websocket.Conn
	port   string
	status int
	code   genv1.ErrorCode
}

// handshake asks to serve a broker, as a caller does.
func handshake(t *testing.T, url string, options *websocket.DialOptions) answer {
	t.Helper()
	if options == nil {
		options = &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer k"}}, Subprotocols: []string{brokerProtocol}}
	}
	conn, res, err := websocket.Dial(t.Context(), url, options)
	if res == nil {
		t.Fatalf("no answer: %v", err)
	}
	var refusal genv1.Error
	if res.Body != nil {
		_ = json.NewDecoder(res.Body).Decode(&refusal)
		_ = res.Body.Close()
	}
	return answer{conn: conn, port: res.Header.Get(brokerPortHeader), status: res.StatusCode, code: refusal.Error.Code}
}

// serving serves a broker `llm` in the sandbox for the length of the test, as
// a caller does: the handshake, and a session whose streams this end accepts.
func serving(t *testing.T, d *Daemon, id genv1.SandboxID) *yamux.Session {
	t.Helper()
	session := serve(t, d, id)
	t.Cleanup(func() { _ = session.Close() })
	return session
}

// serve is serving, left to the test to end.
func serve(t *testing.T, d *Daemon, id genv1.SandboxID) *yamux.Session {
	t.Helper()
	got := handshake(t, servePath(d, id.String(), "llm"), nil)
	if got.conn == nil {
		t.Fatalf("serving llm was refused: %d %s", got.status, got.code)
	}
	if got.conn.Subprotocol() != brokerProtocol {
		t.Fatalf("agreed to %q", got.conn.Subprotocol())
	}
	config := yamux.DefaultConfig()
	config.EnableKeepAlive = false
	config.LogOutput = io.Discard
	session, err := yamux.Server(websocket.NetConn(context.Background(), got.conn, websocket.MessageBinary), config)
	if err != nil {
		t.Fatal(err)
	}
	return session
}

// wait is how long a test waits for something the daemon should have done
// before it says the daemon did not: never reached when it did.
const wait = 5 * time.Second

// hungUp fails the test unless the session is ended from the other side.
func hungUp(t *testing.T, session *yamux.Session) {
	t.Helper()
	select {
	case <-session.CloseChan():
	case <-time.After(wait):
		t.Fatalf("whoever serves the broker was not hung up on: its session is still open after %s", wait)
	}
}

// bare is a serving caller's bare WebSocket: nothing reads it but the test,
// so it answers no ping, and what it is closed with is the test's to read.
func bare(t *testing.T, d *Daemon, id genv1.SandboxID) *websocket.Conn {
	t.Helper()
	got := handshake(t, servePath(d, id.String(), "llm"), nil)
	if got.conn == nil {
		t.Fatalf("serving llm was refused: %d %s", got.status, got.code)
	}
	t.Cleanup(func() { _ = got.conn.CloseNow() })
	return got.conn
}

// closedWith is the status a caller's WebSocket is closed with by the daemon,
// read past whatever frames came before it.
func closedWith(t *testing.T, conn *websocket.Conn) websocket.StatusCode {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), wait)
	defer cancel()
	status, closed := closeStatus(ctx, conn)
	if !closed {
		t.Fatalf("the caller was not hung up on: its WebSocket is still open after %s", wait)
	}
	return status
}

// closeStatus reads a WebSocket until it is closed, and is what it was closed
// with; or false, when ctx ended first.
func closeStatus(ctx context.Context, conn *websocket.Conn) (websocket.StatusCode, bool) {
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return websocket.CloseStatus(err), ctx.Err() == nil
		}
	}
}

// gone waits until the sandbox has no broker `llm` a caller serves, and fails
// if it goes on having one.
func gone(t *testing.T, d *Daemon, id genv1.SandboxID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), wait)
	defer cancel()
	if err := d.BrokerServed(ctx, id.String(), "llm", false); err != nil {
		t.Fatalf("the broker is still there after %s: %v", wait, err)
	}
}

func brokersOn(t *testing.T, client *genv1.ClientWithResponses, id genv1.SandboxID) map[string]genv1.SandboxBroker {
	t.Helper()
	listed, err := client.ListSandboxBrokersWithResponse(t.Context(), id)
	if err != nil || listed.JSON200 == nil {
		t.Fatalf("its brokers = %v %s", err, listed.Body)
	}
	got, err := client.GetSandboxWithResponse(t.Context(), id)
	if err != nil || got.JSON200 == nil || got.JSON200.Brokers == nil {
		t.Fatalf("the sandbox = %v %s", err, got.Body)
	}
	// The list is the sandbox's own, on a path of its own.
	if !reflect.DeepEqual(listed.JSON200.Items, *got.JSON200.Brokers) {
		t.Fatalf("the list says %s and the sandbox %s", listed.Body, got.Body)
	}
	out := map[string]genv1.SandboxBroker{}
	for _, broker := range listed.JSON200.Items {
		out[broker.Name] = broker
	}
	return out
}

func TestASandboxIsCreatedWithTheBrokersItsSpecNames(t *testing.T) {
	_, client, id := brokered(t)
	brokers := brokersOn(t, client, id)
	claude, billing := brokers["claude"], brokers["billing"]
	if len(brokers) != 2 || claude.Kind != genv1.BrokerKindShared || valueOf(claude.Url) != "https://claude.invalid:8443/" {
		t.Errorf("the shared broker is %+v, of %d", claude, len(brokers))
	}
	if billing.Kind != genv1.BrokerKindOneOff || valueOf(billing.Url) != "https://billing.example.com/" {
		t.Errorf("the one-off broker is %+v", billing)
	}
	if claude.Port == 0 || claude.Port == billing.Port {
		t.Errorf("ports %d and %d", claude.Port, billing.Port)
	}
}

func TestACreateIsRefusedABrokerItCannotBeGiven(t *testing.T) {
	d := New(t, WithKey("k"))
	client := typed(t, d, "k")
	for name, c := range map[string]struct {
		broker genv1.SandboxBrokerSpec
		status int
		code   genv1.ErrorCode
	}{
		"a name that is not one": {genv1.SandboxBrokerSpec{Name: "Not_A_Name"}, http.StatusBadRequest, genv1.ErrorCodeBadRequest},
		// Nothing wrong with what was asked: this host has none to give.
		"a shared broker not declared": {genv1.SandboxBrokerSpec{Name: "nobody"}, http.StatusUnprocessableEntity, genv1.ErrorCodeUnsupportedSpec},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := client.CreateSandboxWithResponse(t.Context(), &genv1.CreateSandboxParams{IdempotencyKey: name}, genv1.SandboxSpec{Image: "x", Brokers: &[]genv1.SandboxBrokerSpec{c.broker}})
			if err != nil {
				t.Fatal(err)
			}
			var refusal genv1.Error
			if err := json.Unmarshal(res.Body, &refusal); err != nil || res.StatusCode() != c.status || refusal.Error.Code != c.code {
				t.Fatalf("%d %s, want %d %s", res.StatusCode(), res.Body, c.status, c.code)
			}
		})
	}
	if made := d.Sandboxes(); len(made) != 0 {
		t.Errorf("a refused create made %d sandboxes", len(made))
	}
}

func TestABrokerIsAttachedReplacedAndDetached(t *testing.T) {
	ctx := t.Context()
	d, client, id := brokered(t)
	before := brokersOn(t, client, id)
	upstream := func(url string) genv1.SandboxBrokerAttach {
		return genv1.SandboxBrokerAttach{Upstream: &genv1.BrokerUpstream{Url: url, Description: new("somebody's tools")}}
	}

	// A new one is on a port nothing of the sandbox's has.
	attached, err := client.AttachSandboxBrokerWithResponse(ctx, id, "tools", upstream("https://tools.example.com/"))
	if err != nil || attached.JSON200 == nil || attached.JSON200.Kind != genv1.BrokerKindOneOff {
		t.Fatalf("attach = %v %s", err, attached.Body)
	}
	for name, broker := range before {
		if broker.Port == attached.JSON200.Port {
			t.Errorf("tools is on %s's port", name)
		}
	}
	// One it has keeps its port, wherever it goes now.
	again, err := client.AttachSandboxBrokerWithResponse(ctx, id, "tools", upstream("https://other.example.com/"))
	if err != nil || again.JSON200 == nil || valueOf(again.JSON200.Url) != "https://other.example.com/" || again.JSON200.Port != attached.JSON200.Port {
		t.Fatalf("attached again = %v %s", err, again.Body)
	}
	// An empty body is the host's shared broker of that name, if it has one.
	shared, err := client.AttachSandboxBrokerWithResponse(ctx, id, "claude", genv1.SandboxBrokerAttach{})
	if err != nil || shared.JSON200 == nil || shared.JSON200.Kind != genv1.BrokerKindShared || shared.JSON200.Port != before["claude"].Port {
		t.Fatalf("the shared broker, again = %v %s", err, shared.Body)
	}
	if nobody, _ := client.AttachSandboxBrokerWithResponse(ctx, id, "nobody", genv1.SandboxBrokerAttach{}); nobody.JSON422 == nil || nobody.JSON422.Error.Code != genv1.ErrorCodeUnsupportedSpec {
		t.Fatalf("a shared broker nobody declared = %d %s", nobody.StatusCode(), nobody.Body)
	}
	// A field the contract does not have is not read, as the daemon does not
	// read it: a body that misspells `upstream` is an empty one, and what it
	// gives is the shared broker of that name — or nothing, when there is
	// none.
	misspelt := func(name string) *genv1.AttachSandboxBrokerResponse {
		t.Helper()
		res, err := client.AttachSandboxBrokerWithBodyWithResponse(ctx, id, name, "application/json", strings.NewReader(`{"upstrem":{"url":"https://x.example.com/"}}`))
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := misspelt("tools"); res.JSON422 == nil || brokersOn(t, client, id)["tools"].Kind != genv1.BrokerKindOneOff {
		t.Errorf("`upstrem`, for a name the host declares no broker of = %d %s", res.StatusCode(), res.Body)
	}
	if res := misspelt("claude"); res.JSON200 == nil || res.JSON200.Kind != genv1.BrokerKindShared {
		t.Errorf("`upstrem`, for a name the host declares = %d %s", res.StatusCode(), res.Body)
	}
	for name, refused := range map[string]func() (*genv1.AttachSandboxBrokerResponse, error){
		"a name that is not one": func() (*genv1.AttachSandboxBrokerResponse, error) {
			return client.AttachSandboxBrokerWithResponse(ctx, id, "Not_A_Name", upstream("https://x.example.com/"))
		},
		"a body that is not JSON": func() (*genv1.AttachSandboxBrokerResponse, error) {
			return client.AttachSandboxBrokerWithBodyWithResponse(ctx, id, "tools", "application/json", strings.NewReader(`{"upstream":`))
		},
	} {
		if res, err := refused(); err != nil || res.JSON400 == nil {
			t.Errorf("%s = %v %d %s", name, err, res.StatusCode(), res.Body)
		}
	}

	// The spec is kept in step, as the daemon keeps it for the next boot.
	spec := map[string]genv1.SandboxBrokerSpec{}
	for _, broker := range *d.Sandboxes()[0].Spec.Brokers {
		spec[broker.Name] = broker
	}
	if len(spec) != 3 || spec["tools"].Upstream == nil || spec["billing"].Upstream == nil || spec["claude"].Upstream != nil {
		t.Errorf("the spec's brokers are %+v", spec)
	}

	if detached, err := client.DetachSandboxBrokerWithResponse(ctx, id, "tools"); err != nil || detached.StatusCode() != http.StatusNoContent {
		t.Fatalf("detach = %v %s", err, detached.Body)
	}
	if _, has := brokersOn(t, client, id)["tools"]; has || len(*d.Sandboxes()[0].Spec.Brokers) != 2 {
		t.Error("a detached broker is still the sandbox's")
	}
	// Detaching one it does not have succeeds.
	if again, err := client.DetachSandboxBrokerWithResponse(ctx, id, "tools"); err != nil || again.StatusCode() != http.StatusNoContent {
		t.Fatalf("detach, again = %v %s", err, again.Body)
	}
	if bad, _ := client.DetachSandboxBrokerWithResponse(ctx, id, "Not_A_Name"); bad.JSON400 == nil {
		t.Errorf("detaching a name that is not one = %d", bad.StatusCode())
	}
}

// What is refused, and in which order, is the daemon's: what is wrong with
// the request before anything about the sandbox, and whether the sandbox can
// be changed now only once there is something to change.
func TestABrokerChangeIsRefusedInTheDaemonsOrder(t *testing.T) {
	ctx := t.Context()
	held := func(t *testing.T, state genv1.SandboxState) (*genv1.ClientWithResponses, genv1.SandboxID) {
		t.Helper()
		d := New(t, WithKey("k"), WithBrokers("claude"), WithBoot(func(genv1.SandboxSpec) Outcome { return Outcome{State: genv1.SandboxStateCreating} }))
		client := typed(t, d, "k")
		created, err := client.CreateSandboxWithResponse(ctx, &genv1.CreateSandboxParams{IdempotencyKey: "held"}, genv1.SandboxSpec{
			Image: "x", Brokers: &[]genv1.SandboxBrokerSpec{{Name: "billing", Upstream: &genv1.BrokerUpstream{Url: "https://billing.example.com/"}}},
		})
		if err != nil || created.JSON202 == nil {
			t.Fatalf("%v %s", err, created.Body)
		}
		if state != genv1.SandboxStateCreating {
			d.Settle(created.JSON202.Id, state, "")
		}
		return client, created.JSON202.Id
	}
	oneOff := genv1.SandboxBrokerAttach{Upstream: &genv1.BrokerUpstream{Url: "https://x.example.com/"}}

	for _, state := range []genv1.SandboxState{genv1.SandboxStateCreating, genv1.SandboxStateBooting, genv1.SandboxStatePaused} {
		t.Run("while it is "+string(state), func(t *testing.T) {
			client, id := held(t, state)
			// Its brokers are read, whatever it is doing.
			if listed, _ := client.ListSandboxBrokersWithResponse(ctx, id); listed.JSON200 == nil || len(listed.JSON200.Items) != 1 {
				t.Errorf("its brokers = %d %s", listed.StatusCode(), listed.Body)
			}
			for name, c := range map[string]struct {
				broker string
				body   genv1.SandboxBrokerAttach
				status int
			}{
				"a broker that could be given":       {"tools", oneOff, http.StatusConflict},
				"the one it has, again":              {"billing", oneOff, http.StatusConflict},
				"a name that is not one":             {"Not_A_Name", oneOff, http.StatusBadRequest},
				"a shared broker nobody declared":    {"nobody", genv1.SandboxBrokerAttach{}, http.StatusUnprocessableEntity},
				"a shared broker the host does have": {"claude", genv1.SandboxBrokerAttach{}, http.StatusConflict},
			} {
				if res, err := client.AttachSandboxBrokerWithResponse(ctx, id, c.broker, c.body); err != nil || res.StatusCode() != c.status {
					t.Errorf("attaching %s = %v %d %s, want %d", name, err, res.StatusCode(), res.Body, c.status)
				}
			}
			// Taking away what it has is a change, and waits; taking away
			// what it does not have changes nothing, and is done.
			if res, _ := client.DetachSandboxBrokerWithResponse(ctx, id, "billing"); res.JSON409 == nil || res.JSON409.Error.Code != genv1.ErrorCodeConflict {
				t.Errorf("detaching the broker it has = %d %s, want 409 conflict", res.StatusCode(), res.Body)
			}
			if res, _ := client.DetachSandboxBrokerWithResponse(ctx, id, "tools"); res.StatusCode() != http.StatusNoContent {
				t.Errorf("detaching a broker it does not have = %d %s, want 204", res.StatusCode(), res.Body)
			}
			if listed, _ := client.ListSandboxBrokersWithResponse(ctx, id); listed.JSON200 == nil || len(listed.JSON200.Items) != 1 || listed.JSON200.Items[0].Name != "billing" {
				t.Errorf("after all that was refused, its brokers = %s", listed.Body)
			}
		})
	}

	t.Run("of a sandbox that is not there", func(t *testing.T) {
		client, _ := held(t, genv1.SandboxStateReady)
		missing := genv1.SandboxID{}
		if res, _ := client.ListSandboxBrokersWithResponse(ctx, missing); res.JSON404 == nil {
			t.Errorf("the brokers of no sandbox = %d", res.StatusCode())
		}
		if res, _ := client.AttachSandboxBrokerWithResponse(ctx, missing, "tools", oneOff); res.JSON404 == nil {
			t.Errorf("attaching to no sandbox = %d", res.StatusCode())
		}
		if res, _ := client.DetachSandboxBrokerWithResponse(ctx, missing, "tools"); res.JSON404 == nil {
			t.Errorf("detaching from no sandbox = %d", res.StatusCode())
		}
		// The name is judged before the sandbox is looked for.
		if res, _ := client.AttachSandboxBrokerWithResponse(ctx, missing, "Not_A_Name", oneOff); res.JSON400 == nil {
			t.Errorf("attaching a name that is not one to no sandbox = %d, want 400", res.StatusCode())
		}
		if res, _ := client.DetachSandboxBrokerWithResponse(ctx, missing, "Not_A_Name"); res.JSON400 == nil {
			t.Errorf("detaching a name that is not one from no sandbox = %d, want 400", res.StatusCode())
		}
		// And the sandbox before what it could be given.
		if res, _ := client.AttachSandboxBrokerWithResponse(ctx, missing, "nobody", genv1.SandboxBrokerAttach{}); res.JSON404 == nil {
			t.Errorf("attaching a broker nobody declared to no sandbox = %d, want 404", res.StatusCode())
		}
	})
}

// Serving a broker is what gives the sandbox one: on a port the answer says,
// in its list for as long as the caller is there, in no spec, and gone when
// the caller is.
func TestServingABrokerGivesTheSandboxOneForAsLongAsTheCallerStays(t *testing.T) {
	d, client, id := brokered(t)
	before := brokersOn(t, client, id)

	got := handshake(t, servePath(d, id.String(), "llm"), nil)
	if got.conn == nil {
		t.Fatalf("%d %s", got.status, got.code)
	}
	defer func() { _ = got.conn.CloseNow() }()
	port, err := strconv.Atoi(got.port)
	if err != nil || port == 0 || port == before["claude"].Port || port == before["billing"].Port {
		t.Fatalf("the answer says the port is %q, beside %d and %d", got.port, before["claude"].Port, before["billing"].Port)
	}
	llm, listed := brokersOn(t, client, id)["llm"]
	if !listed || llm.Kind != genv1.BrokerKindClient || llm.Port != port || llm.Url != nil {
		t.Errorf("its list has %+v, %v", llm, listed)
	}
	// Nothing of it is in the spec: it is its caller's connection.
	for _, broker := range *d.Sandboxes()[0].Spec.Brokers {
		if broker.Name == "llm" {
			t.Errorf("the spec names %+v", broker)
		}
	}

	// A second, by another caller, is on a port of its own.
	other := handshake(t, servePath(d, id.String(), "tools"), nil)
	if other.conn == nil || other.port == got.port || other.port == "" {
		t.Fatalf("a second broker: %d %s, on %q", other.status, other.code, other.port)
	}
	defer func() { _ = other.conn.CloseNow() }()

	// The caller leaves, and its broker with it: the other's stays.
	if err := got.conn.Close(websocket.StatusNormalClosure, ""); err != nil {
		t.Fatal(err)
	}
	gone(t, d, id)
	after := brokersOn(t, client, id)
	if _, still := after["llm"]; still || len(after) != 3 || after["tools"].Kind != genv1.BrokerKindClient {
		t.Errorf("once its caller left, the sandbox has %+v", after)
	}
	if _, err := d.DialBroker(id.String(), "llm"); err == nil || !strings.Contains(err.Error(), "no caller is serving") {
		t.Errorf("a connection to a broker that went with its caller: %v", err)
	}
}

// Everything that refuses, in the order the contract gives: the name, the
// sandbox, whether it is running, the handshake, and whether the name is
// free.
func TestServingIsRefusedWhatTheRouteRefusesInItsOrder(t *testing.T) {
	d, client, sandbox := brokered(t)
	id := sandbox.String()
	offering := func(header http.Header, protocols ...string) *websocket.DialOptions {
		return &websocket.DialOptions{HTTPHeader: header, Subprotocols: protocols}
	}
	key := http.Header{"Authorization": {"Bearer k"}}
	missing := "00000000-0000-0000-0000-000000000000"
	for _, c := range []struct {
		name, url string
		options   *websocket.DialOptions
		status    int
		code      genv1.ErrorCode
	}{
		{"a name that is not one", servePath(d, id, "Not_A_Name"), nil, http.StatusBadRequest, genv1.ErrorCodeBadRequest},
		{"a name that is not one, of no sandbox", servePath(d, missing, "Not_A_Name"), nil, http.StatusBadRequest, genv1.ErrorCodeBadRequest},
		{"an id that is not one", servePath(d, "pr-1234", "llm"), nil, http.StatusBadRequest, genv1.ErrorCodeBadRequest},
		{"no such sandbox", servePath(d, missing, "llm"), nil, http.StatusNotFound, genv1.ErrorCodeNotFound},
		{"no such sandbox, and no handshake's protocol", servePath(d, missing, "llm"), offering(key), http.StatusNotFound, genv1.ErrorCodeNotFound},
		{"a handshake that does not offer the protocol", servePath(d, id, "llm"), offering(key), http.StatusBadRequest, genv1.ErrorCodeBadRequest},
		{"a handshake that offers another", servePath(d, id, "llm"), offering(key, terminalProtocol), http.StatusBadRequest, genv1.ErrorCodeBadRequest},
		{"the name of its shared broker", servePath(d, id, "claude"), nil, http.StatusConflict, genv1.ErrorCodeConflict},
		{"the name of its one-off broker", servePath(d, id, "billing"), nil, http.StatusConflict, genv1.ErrorCodeConflict},
		{"a name it has, and no handshake's protocol", servePath(d, id, "claude"), offering(key), http.StatusBadRequest, genv1.ErrorCodeBadRequest},
		{"no key", servePath(d, id, "llm"), offering(nil, brokerProtocol), http.StatusUnauthorized, genv1.ErrorCodeUnauthorized},
		{"another key", servePath(d, id, "llm"), offering(http.Header{"Authorization": {"Bearer other"}}, brokerProtocol), http.StatusUnauthorized, genv1.ErrorCodeUnauthorized},
		{"another key, offered as a subprotocol", servePath(d, id, "llm"), offering(nil, brokerProtocol, bearerProtocol+"other"), http.StatusUnauthorized, genv1.ErrorCodeUnauthorized},
	} {
		t.Run(c.name, func(t *testing.T) {
			got := handshake(t, c.url, c.options)
			if got.conn != nil {
				_ = got.conn.CloseNow()
				t.Fatal("it was let in")
			}
			if got.status != c.status || got.code != c.code || got.port != "" {
				t.Fatalf("%d %s, port %q", got.status, got.code, got.port)
			}
		})
	}

	// A request that is not a handshake is refused as one, whatever it offers.
	t.Run("not a handshake", func(t *testing.T) {
		request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, servePath(d, id, "llm"), http.NoBody)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Authorization", "Bearer k")
		request.Header.Set("Sec-WebSocket-Protocol", brokerProtocol)
		res, err := http.DefaultClient.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = res.Body.Close() }()
		var refusal genv1.Error
		if err := json.NewDecoder(res.Body).Decode(&refusal); err != nil || res.StatusCode != http.StatusBadRequest || refusal.Error.Code != genv1.ErrorCodeBadRequest {
			t.Fatalf("%d %v %+v", res.StatusCode, err, refusal)
		}
	})

	// A sandbox that is not running has no machine to open a port in, and
	// that is said before the handshake is looked at.
	for _, state := range []genv1.SandboxState{genv1.SandboxStateStopped, genv1.SandboxStatePaused, genv1.SandboxStateBooting, genv1.SandboxStateUnreachable, genv1.SandboxStateFailed} {
		t.Run("a sandbox that is "+string(state), func(t *testing.T) {
			d.Settle(sandbox, state, "")
			defer d.Settle(sandbox, genv1.SandboxStateReady, "")
			for _, options := range []*websocket.DialOptions{nil, offering(key)} {
				if got := handshake(t, servePath(d, id, "llm"), options); got.conn != nil || got.status != http.StatusConflict || got.code != genv1.ErrorCodeConflict {
					t.Fatalf("%d %s", got.status, got.code)
				}
			}
		})
	}

	// Nothing above was let in, or left the sandbox a broker.
	if brokers := brokersOn(t, client, sandbox); len(brokers) != 2 {
		t.Errorf("after every refusal, the sandbox has %+v", brokers)
	}
}

// A browser cannot put a key in the Authorization of a WebSocket: a handshake
// offers it as a subprotocol, and only the route's own is echoed back.
func TestAHandshakeMayOfferItsKeyAsASubprotocol(t *testing.T) {
	d, _, id := brokered(t, WithTerminal(func(ctx context.Context, _ genv1.SandboxID, _ *Terminal) int {
		<-ctx.Done()
		return 0
	}))
	for route, protocol := range map[string]string{
		servePath(d, id.String(), "llm"):                     brokerProtocol,
		d.URL + "/v1/sandboxes/" + id.String() + "/terminal": terminalProtocol,
	} {
		got := handshake(t, route, &websocket.DialOptions{Subprotocols: []string{protocol, bearerProtocol + "k"}})
		if got.conn == nil {
			t.Fatalf("%s: %d %s", protocol, got.status, got.code)
		}
		if got.conn.Subprotocol() != protocol {
			t.Errorf("agreed to %q", got.conn.Subprotocol())
		}
		_ = got.conn.Close(websocket.StatusNormalClosure, "")
	}
	// And nowhere but on a handshake: the same header on a plain request is
	// no key at all.
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, d.URL+"/v1/sandboxes", http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Sec-WebSocket-Protocol", bearerProtocol+"k")
	res, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusUnauthorized {
		t.Errorf("a key offered as a subprotocol of no handshake = %d", res.StatusCode)
	}
}

func TestABrokerCarriesAWorkloadsConnectionToTheCallerServingIt(t *testing.T) {
	d, _, id := brokered(t)
	session := serving(t, d, id)

	conn, err := d.DialBroker(id.String(), "llm")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	// From its first byte, with nothing before it.
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	half, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("%T has no CloseWrite", conn)
	}
	if err := half.CloseWrite(); err != nil {
		t.Fatal(err)
	}
	stream, err := session.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if asked, err := io.ReadAll(stream); err != nil || string(asked) != "GET / HTTP/1.1\r\n\r\n" {
		t.Fatalf("%q %v", asked, err)
	}
	// The workload finished writing, and is still answered.
	if _, err := io.WriteString(stream, "HTTP/1.1 204 No Content\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	_ = stream.Close()
	if answer, err := io.ReadAll(conn); err != nil || string(answer) != "HTTP/1.1 204 No Content\r\n\r\n" {
		t.Fatalf("%q %v", answer, err)
	}
}

func TestAWorkloadsConnectionIsClosedForGoodAndCutWithItsBroker(t *testing.T) {
	d, _, id := brokered(t)
	session := serving(t, d, id)
	dial := func() net.Conn {
		t.Helper()
		conn, err := d.DialBroker(id.String(), "llm")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn
	}

	// Closing it ends a read in flight, and the caller reads the end.
	closed := dial()
	reads := make(chan error, 1)
	spawn.Go(t, func() {
		_, err := closed.Read(make([]byte, 1))
		reads <- err
	})
	if err := closed.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-reads; !errors.Is(err, net.ErrClosed) {
		t.Errorf("a read in flight ended with %v", err)
	}
	if err := closed.Close(); err != nil {
		t.Errorf("close, again: %v", err)
	}
	stream, err := session.AcceptStream()
	if err != nil {
		t.Fatal(err)
	}
	if rest, err := io.ReadAll(stream); err != nil || len(rest) != 0 {
		t.Errorf("the caller read %q, %v", rest, err)
	}

	// One open when its caller leaves is cut: what it was reading is not a
	// whole answer.
	cut := dial()
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadAll(cut); !errors.Is(err, errCut) {
		t.Errorf("a read from a broker that went: %v", err)
	}
}

func TestDialBrokerIsRefusedWhenNoCallerServesTheName(t *testing.T) {
	d, _, id := brokered(t)
	for name, c := range map[string]struct{ sandbox, broker, want string }{
		"an id that is not one":     {"pr-1234", "llm", "is not a sandbox id"},
		"no such sandbox":           {"00000000-0000-0000-0000-000000000000", "llm", "no sandbox"},
		"a name nobody serves":      {id.String(), "llm", "no caller is serving"},
		"a broker with an upstream": {id.String(), "claude", "no caller is serving"},
	} {
		t.Run(name, func(t *testing.T) {
			conn, err := d.DialBroker(c.sandbox, c.broker)
			if err == nil {
				_ = conn.Close()
				t.Fatal("it connected")
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("%v", err)
			}
		})
	}
}

// A name is one broker's: a second caller is refused it while the first
// answers for it, and has it as soon as the first has left.
func TestANameIsOneCallersAtATime(t *testing.T) {
	d, _, id := brokered(t)
	first := serving(t, d, id)
	for range 2 {
		got := handshake(t, servePath(d, id.String(), "llm"), nil)
		if got.conn != nil {
			_ = got.conn.CloseNow()
			t.Fatal("a second caller was given the name")
		}
		if got.status != http.StatusConflict || got.code != genv1.ErrorCodeConflict {
			t.Fatalf("%d %s", got.status, got.code)
		}
	}
	// The first lost nothing by it.
	conn, err := d.DialBroker(id.String(), "llm")
	if err != nil {
		t.Fatal(err)
	}
	_ = conn.Close()
	if stream, err := first.AcceptStream(); err != nil {
		t.Fatal(err)
	} else {
		_ = stream.Close()
	}

	// No waiting to be told the first has gone: it is asked, and does not
	// answer.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second := serving(t, d, id)
	conn, err = d.DialBroker(id.String(), "llm")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := second.AcceptStream(); err != nil {
		t.Fatalf("the second caller was not given the connection: %v", err)
	}
}

// A name a caller is serving is theirs while they do: it is not given an
// upstream under them.
func TestANameACallerServesIsNotAttached(t *testing.T) {
	d, client, id := brokered(t)
	session := serving(t, d, id)
	for name, body := range map[string]genv1.SandboxBrokerAttach{
		"an upstream":       {Upstream: &genv1.BrokerUpstream{Url: "https://llm.example.com/"}},
		"the shared broker": {},
	} {
		res, err := client.AttachSandboxBrokerWithResponse(t.Context(), id, "llm", body)
		// The shared one of that name is not declared, which is said first:
		// what cannot be given is judged before the sandbox is.
		want := http.StatusConflict
		if body.Upstream == nil {
			want = http.StatusUnprocessableEntity
		}
		if err != nil || res.StatusCode() != want {
			t.Errorf("attaching %s over it = %v %d %s, want %d", name, err, res.StatusCode(), res.Body, want)
		}
	}
	if llm := brokersOn(t, client, id)["llm"]; llm.Kind != genv1.BrokerKindClient {
		t.Errorf("it is now %+v", llm)
	}
	conn, err := d.DialBroker(id.String(), "llm")
	if err != nil {
		t.Fatalf("its caller was hung up on: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := session.AcceptStream(); err != nil {
		t.Fatalf("its caller was hung up on: %v", err)
	}
}

// takings is what takes a broker from the caller serving it.
var takings = map[string]func(*testing.T, *Daemon, *genv1.ClientWithResponses, genv1.SandboxID){
	"the sandbox is deleted": func(t *testing.T, _ *Daemon, client *genv1.ClientWithResponses, id genv1.SandboxID) {
		t.Helper()
		if res, err := client.DeleteSandboxWithResponse(t.Context(), id, &genv1.DeleteSandboxParams{}); err != nil || res.StatusCode() != http.StatusNoContent {
			t.Fatalf("%v %s", err, res.Body)
		}
	},
	"the broker is detached": func(t *testing.T, _ *Daemon, client *genv1.ClientWithResponses, id genv1.SandboxID) {
		t.Helper()
		if res, err := client.DetachSandboxBrokerWithResponse(t.Context(), id, "llm"); err != nil || res.StatusCode() != http.StatusNoContent {
			t.Fatalf("%v %s", err, res.Body)
		}
	},
	"the sandbox is stopped": func(_ *testing.T, d *Daemon, _ *genv1.ClientWithResponses, id genv1.SandboxID) {
		d.Settle(id, genv1.SandboxStateStopped, "")
	},
	"the sandbox is paused": func(_ *testing.T, d *Daemon, _ *genv1.ClientWithResponses, id genv1.SandboxID) {
		d.Settle(id, genv1.SandboxStatePaused, "")
	},
}

// Each hangs up on the caller, cuts what it was carrying, and leaves the
// sandbox without the broker.
func TestACallerIsHungUpOnAndItsBrokerGoneWhenItIsTakenAway(t *testing.T) {
	for name, take := range takings {
		t.Run(name, func(t *testing.T) {
			d, client, id := brokered(t)
			session := serving(t, d, id)
			conn, err := d.DialBroker(id.String(), "llm")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			if _, err := session.AcceptStream(); err != nil {
				t.Fatal(err)
			}

			take(t, d, client, id)
			// Gone from the moment it is taken, before its caller has been
			// told.
			if _, err := d.DialBroker(id.String(), "llm"); err == nil {
				t.Error("a connection to a broker that is gone")
			}
			if listed, _ := client.ListSandboxBrokersWithResponse(t.Context(), id); listed.JSON200 != nil {
				for _, broker := range listed.JSON200.Items {
					if broker.Name == "llm" {
						t.Errorf("the sandbox still lists %+v", broker)
					}
				}
			}
			gone(t, d, id)
			hungUp(t, session)
			if _, err := session.AcceptStream(); err == nil {
				t.Error("a session that was hung up on accepted a stream")
			}
			if _, err := io.ReadAll(conn); !errors.Is(err, errCut) {
				t.Errorf("the workload's connection ended with %v", err)
			}
		})
	}
}

// A caller this daemon hangs up on is told it is going away — 1001 — which is
// how it tells being hung up on from a line that was lost.
func TestACallerHungUpOnIsToldTheDaemonIsGoingAway(t *testing.T) {
	for name, take := range takings {
		t.Run(name, func(t *testing.T) {
			d, client, id := brokered(t)
			serving := bare(t, d, id)
			take(t, d, client, id)
			if status := closedWith(t, serving); status != websocket.StatusGoingAway {
				t.Errorf("closed with %d, want 1001", status)
			}
		})
	}
	t.Run("the daemon is closed", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), wait)
		defer cancel()
		type ending struct {
			status websocket.StatusCode
			closed bool
		}
		ended := make(chan ending, 1)
		t.Run("with a caller serving", func(inner *testing.T) {
			d, _, id := brokered(inner)
			got := handshake(inner, servePath(d, id.String(), "llm"), nil)
			if got.conn == nil {
				inner.Fatalf("%d %s", got.status, got.code)
			}
			// Read while the daemon closes, which is this subtest ending:
			// so on the outer test, whose end does not wait for the daemon's.
			spawn.Go(t, func() {
				defer func() { _ = got.conn.CloseNow() }()
				status, closed := closeStatus(ctx, got.conn)
				ended <- ending{status, closed}
			})
		})
		if got := <-ended; !got.closed || got.status != websocket.StatusGoingAway {
			t.Errorf("hung up on: %v, with %d; want 1001", got.closed, got.status)
		}
	})
}

// A caller serving a broker accepts streams and opens none: one that does is
// hung up on, as breaking the rules — 1008 — and its broker goes.
func TestACallerThatOpensAStreamIsHungUpOn(t *testing.T) {
	d, _, id := brokered(t)
	serving := bare(t, d, id)
	// A stream opened from the accepting side, as yamux frames it: a window
	// update carrying SYN, on an even id.
	open := []byte{0, 1, 0, 1, 0, 0, 0, 2, 0, 0, 0, 0}
	if err := serving.Write(t.Context(), websocket.MessageBinary, open); err != nil {
		t.Fatal(err)
	}
	if status := closedWith(t, serving); status != websocket.StatusPolicyViolation {
		t.Errorf("closed with %d, want 1008", status)
	}
	gone(t, d, id)
}

// The caller that holds a name is asked whether it is still there when a
// second arrives, and only one that answers keeps it.
func TestACallerThatDoesNotAnswerForItsNameLosesItToTheNext(t *testing.T) {
	// Long enough that the second caller has asked before the session's own
	// keepalive has: it is the asking that is tested here.
	d, client, id := brokered(t, WithBrokerKeepAlive(100*time.Millisecond))
	silent := bare(t, d, id)
	second := serving(t, d, id)
	if status := closedWith(t, silent); status != websocket.StatusGoingAway {
		t.Errorf("the caller that did not answer was closed with %d, want 1001", status)
	}
	if brokers := brokersOn(t, client, id); len(brokers) != 3 || brokers["llm"].Kind != genv1.BrokerKindClient {
		t.Errorf("the sandbox has %+v", brokers)
	}
	conn, err := d.DialBroker(id.String(), "llm")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := second.AcceptStream(); err != nil {
		t.Fatalf("the second caller was not given the connection: %v", err)
	}
}

// A caller that goes silent without leaving is found out by its session's
// keepalives, and its broker goes.
func TestACallerThatGoesSilentLosesItsBroker(t *testing.T) {
	d, client, id := brokered(t, WithBrokerKeepAlive(10*time.Millisecond))
	bare(t, d, id)
	gone(t, d, id)
	if _, still := brokersOn(t, client, id)["llm"]; still {
		t.Error("the sandbox still lists the broker of a caller that went silent")
	}
	if _, err := d.DialBroker(id.String(), "llm"); err == nil || !strings.Contains(err.Error(), "no caller is serving") {
		t.Errorf("a connection to a broker whose caller went silent: %v", err)
	}
}

// A broker still served when the test ends is taken away by its daemon.
func TestACallerLeftAtTheEndIsHungUpOn(t *testing.T) {
	hungUpOn := make(chan bool, 1)
	t.Run("leaves it serving", func(inner *testing.T) {
		d, _, id := brokered(inner)
		session := serve(inner, d, id)
		// Watched from the outer test, whose end does not wait for the
		// daemon's: a daemon that hung up on nobody would wait for this
		// session for ever, so it is ended here once that is known.
		spawn.Go(t, func() {
			defer func() { _ = session.Close() }()
			select {
			case <-session.CloseChan():
				hungUpOn <- true
			case <-time.After(wait):
				hungUpOn <- false
			}
		})
	})
	if !<-hungUpOn {
		t.Fatalf("the daemon closed and did not hang up on the caller serving a broker: their session was still open after %s", wait)
	}
}

func TestBrokerServedWaitsForACallerToArriveAndToLeave(t *testing.T) {
	d, _, id := brokered(t)
	// No caller serves what is not there.
	for _, nowhere := range [][2]string{{"pr-1234", "llm"}, {"00000000-0000-0000-0000-000000000000", "llm"}, {id.String(), "llm"}, {id.String(), "claude"}} {
		if err := d.BrokerServed(t.Context(), nowhere[0], nowhere[1], false); err != nil {
			t.Errorf("%v: %v", nowhere, err)
		}
	}
	// It waits for a caller that has not arrived, as long as it is let.
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := d.BrokerServed(ctx, id.String(), "llm", true); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	arrived := make(chan error, 1)
	spawn.Go(t, func() { arrived <- d.BrokerServed(t.Context(), id.String(), "llm", true) })
	session := serving(t, d, id)
	if err := <-arrived; err != nil {
		t.Fatal(err)
	}
	// And for one that has not left.
	ctx, cancel = context.WithCancel(t.Context())
	cancel()
	if err := d.BrokerServed(ctx, id.String(), "llm", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("%v", err)
	}
	left := make(chan error, 1)
	spawn.Go(t, func() { left <- d.BrokerServed(t.Context(), id.String(), "llm", false) })
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-left; err != nil {
		t.Fatal(err)
	}
}
