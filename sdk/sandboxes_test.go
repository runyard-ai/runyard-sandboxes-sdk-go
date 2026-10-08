package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakedaemon"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/spawn"
	"go.uber.org/goleak"
)

// This module cannot import the task package the rest of the repository
// starts goroutines on, so goleak holds its tests directly: a goroutine left
// running once they are done fails them.
func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// A daemon that makes one sandbox and keeps it `creating` until the test says
// it is ready — and counts how often it is asked.
type slowDaemon struct {
	id    string
	ready chan struct{}
	once  sync.Once
	// following receives once each follow of the events has sent what it
	// had, and before it ends or waits.
	following chan struct{}
	// cut is how many follows end before the sandbox is ready, as a proxy
	// cutting the stream does.
	cut     atomic.Int32
	queries atomic.Int32
}

func newSlowDaemon(t *testing.T, cut int32, opts ...Option) (*slowDaemon, *Client) {
	t.Helper()
	d := &slowDaemon{id: "0192f7a4-5b1e-7c3d-9a2f-4e6b8c1d0a53", ready: make(chan struct{}), following: make(chan struct{}, 16)}
	d.cut.Store(cut)
	server := httptest.NewServer(d)
	t.Cleanup(server.Close)
	client, err := New(server.URL, append([]Option{WithKey("k")}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return d, client
}

func (d *slowDaemon) makeReady() { d.once.Do(func() { close(d.ready) }) }

func (d *slowDaemon) state() string {
	select {
	case <-d.ready:
		return "ready"
	default:
		return "creating"
	}
}

func (d *slowDaemon) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	sandbox := func(status int) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprintf(w, `{"id":%q,"state":%q}`, d.id, d.state())
	}
	switch {
	case r.Method == http.MethodPost && r.URL.Path == "/v1/sandboxes":
		sandbox(http.StatusAccepted)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/"+d.id:
		d.queries.Add(1)
		sandbox(http.StatusOK)
	case r.Method == http.MethodGet && r.URL.Path == "/v1/sandboxes/"+d.id+"/events":
		w.Header().Set("Content-Type", "application/x-ndjson")
		fmt.Fprintln(w, `{"seq":1,"type":"state","state":"creating","at":"2026-09-26T00:00:00Z"}`)
		fmt.Fprintln(w, `{"seq":2,"type":"progress","message":"forking a disk","at":"2026-09-26T00:00:00Z"}`)
		if http.NewResponseController(w).Flush() != nil {
			return
		}
		d.following <- struct{}{}
		if d.cut.Add(-1) >= 0 {
			return
		}
		select {
		case <-d.ready:
		case <-r.Context().Done():
			return
		}
		fmt.Fprintln(w, `{"seq":3,"type":"state","state":"ready","at":"2026-09-26T00:00:01Z"}`)
		if http.NewResponseController(w).Flush() != nil {
			return
		}
		<-r.Context().Done()
	default:
		http.NotFound(w, r)
	}
}

func create(t *testing.T, client *Client) <-chan error {
	t.Helper()
	done := make(chan error, 1)
	spawn.Go(t, func() {
		created, err := client.Create(t.Context(), "", Spec{Image: "example"})
		if err == nil && created.State != "ready" {
			err = fmt.Errorf("Create returned a sandbox that is %s", created.State)
		}
		done <- err
	})
	return done
}

// Create returns when the daemon says `ready`, and asks nothing while it
// waits: it follows the events, and asks once they say it is time.
func TestCreateReturnsWhenTheEventsSayReady(t *testing.T) {
	daemon, client := newSlowDaemon(t, 0)
	done := create(t, client)

	<-daemon.following
	select {
	case err := <-done:
		t.Fatalf("Create returned while the sandbox was creating: %v", err)
	default:
	}
	if got := daemon.queries.Load(); got != 0 {
		t.Fatalf("the sandbox was asked about %d times while its events were being followed", got)
	}

	daemon.makeReady()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := daemon.queries.Load(); got != 1 {
		t.Fatalf("the sandbox was asked about %d times, want once: after the events said ready", got)
	}
}

// A stream that ends before the sandbox settled is followed again, and the
// sandbox is asked once after each — never every so often in between.
func TestCreateFollowsTheEventsAgainWhenTheyEndEarly(t *testing.T) {
	daemon, client := newSlowDaemon(t, 2)
	done := create(t, client)

	for range 3 {
		<-daemon.following
	}
	daemon.makeReady()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if got := daemon.queries.Load(); got != 3 {
		t.Fatalf("the sandbox was asked about %d times, want once after each of the three follows", got)
	}
}

// What the daemon says a sandbox is doing on the way is passed on while
// Create waits, in order, and once: a stream that was cut and followed again
// says it all again, and none of that is told twice.
func TestCreateTellsWhatTheDaemonSaysItIsDoingOnce(t *testing.T) {
	for _, cut := range []int32{0, 2} {
		var told []string
		daemon, client := newSlowDaemon(t, cut, WithProgress(func(message string) { told = append(told, message) }))
		done := create(t, client)
		for range cut + 1 {
			<-daemon.following
		}
		daemon.makeReady()
		if err := <-done; err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(told, []string{"forking a disk"}) {
			t.Errorf("cut %d times: told %q", cut, told)
		}
	}
}

// Every progress the daemon gives is told, in the order it gave it, and
// nothing else is: not a state, not an error's sentence.
func TestCreateTellsEveryProgressInOrderAndNothingElse(t *testing.T) {
	said := []string{"pulling example", "layer 1 of 2 (10MB)", "layer 2 of 2 (3MB)", "booting"}
	var told []string
	d := fakedaemon.New(t, fakedaemon.WithKey("k"), fakedaemon.WithBoot(func(genv1.SandboxSpec) fakedaemon.Outcome {
		return fakedaemon.Outcome{Progress: said}
	}))
	client, err := New(d.URL, WithKey("k"), WithProgress(func(message string) { told = append(told, message) }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Create(t.Context(), "", Spec{Image: "example"}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(told, said) {
		t.Errorf("told %q", told)
	}

	// One that fails says why in an error, which is Create's to return and
	// not progress.
	told = nil
	failing := fakedaemon.New(t, fakedaemon.WithKey("k"), fakedaemon.WithBoot(func(genv1.SandboxSpec) fakedaemon.Outcome {
		return fakedaemon.Outcome{State: genv1.SandboxStateFailed, Error: "no such image", Progress: []string{"pulling example"}}
	}))
	client, err = New(failing.URL, WithKey("k"), WithProgress(func(message string) { told = append(told, message) }))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Create(t.Context(), "", Spec{Image: "example"}); err == nil || !strings.Contains(err.Error(), "no such image") {
		t.Fatalf("Create: %v", err)
	}
	if !slices.Equal(told, []string{"pulling example"}) {
		t.Errorf("of one that failed, told %q", told)
	}
}

// fake is a daemon that answers the way the contract says, and a client
// pointed at it with the key it wants.
func fake(t *testing.T, opts ...fakedaemon.Option) (*fakedaemon.Daemon, *Client) {
	t.Helper()
	d := fakedaemon.New(t, append([]fakedaemon.Option{fakedaemon.WithKey("k")}, opts...)...)
	client, err := New(d.URL+"/", WithKey("k"))
	if err != nil {
		t.Fatal(err)
	}
	return d, client
}

func holding(genv1.SandboxSpec) fakedaemon.Outcome {
	return fakedaemon.Outcome{State: genv1.SandboxStateCreating}
}

type createResult struct {
	sandbox *Sandbox
	err     error
}

// createAsync is Create on its own goroutine, for a test that has to move the
// sandbox while it is being waited for.
func createAsync(ctx context.Context, t *testing.T, client *Client, spec Spec) <-chan createResult {
	t.Helper()
	done := make(chan createResult, 1)
	spawn.Go(t, func() {
		sandbox, err := client.Create(ctx, "", spec)
		done <- createResult{sandbox, err}
	})
	return done
}

func within(t *testing.T, done <-chan createResult) createResult {
	t.Helper()
	select {
	case got := <-done:
		return got
	case <-time.After(10 * time.Second):
		t.Fatal("Create did not return")
		return createResult{}
	}
}

func TestNewRefusesAnAddressThatIsNotADaemons(t *testing.T) {
	for _, address := range []string{"", "127.0.0.1:8099", "localhost", "ftp://host", "http://", "http://%zz", "unix:///run/sock"} {
		if client, err := New(address); err == nil || client != nil {
			t.Errorf("New(%q) = %v, %v; want a refusal", address, client, err)
		} else if !strings.Contains(err.Error(), "http://") {
			t.Errorf("New(%q) said %q, which does not say what it wants", address, err)
		}
	}
	for _, address := range []string{"http://127.0.0.1:8099", "https://sandboxes.example/", "http://host/prefix/"} {
		if _, err := New(address); err != nil {
			t.Errorf("New(%q): %v", address, err)
		}
	}
}

func TestInfoSaysWhatTheHostIsAndWhatItDeclares(t *testing.T) {
	_, client := fake(t, fakedaemon.WithBrokers("claude", "github"))
	info, err := client.Info(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.Id != "fake-host" || info.Brokers == nil || len(*info.Brokers) != 2 || (*info.Brokers)[0].Name != "claude" {
		t.Fatalf("info = %+v", info)
	}
}

// A daemon that wants a key says so in the contract's words when it is not
// given one.
func TestAMissingKeyIsRefusedWithTheContractsCode(t *testing.T) {
	d, _ := fake(t)
	anonymous, err := New(d.URL)
	if err != nil {
		t.Fatal(err)
	}
	_, err = anonymous.Info(context.Background())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Status != 401 || CodeOf(err) != "unauthorized" {
		t.Fatalf("Info without a key = %v", err)
	}
	if !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("the error %q does not name its code", err)
	}
}

type countingTransport struct{ calls atomic.Int32 }

func (c *countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	c.calls.Add(1)
	return http.DefaultTransport.RoundTrip(r)
}

func TestWithHTTPClientIsTheTransportEveryCallUses(t *testing.T) {
	d, _ := fake(t)
	transport := &countingTransport{}
	client, err := New(d.URL, WithKey("k"), WithHTTPClient(&http.Client{Transport: transport}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Info(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.List(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := transport.calls.Load(); got != 2 {
		t.Fatalf("the caller's transport carried %d requests, want 2", got)
	}
}

func TestCreateReturnsAReadyHandleWithItsNameBrokersAndIdentity(t *testing.T) {
	d, client := fake(t, fakedaemon.WithBrokers("claude"))
	labels := map[string]string{"team": "infra", "name": "overwritten"}
	sandbox, err := client.Create(context.Background(), "hello-one", Spec{
		Image:   "alpine:3.20",
		Labels:  &labels,
		Brokers: &[]genv1.SandboxBrokerSpec{{Name: "claude"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.State != genv1.SandboxStateReady || sandbox.Name != "hello-one" || sandbox.Identity == nil {
		t.Fatalf("handle = %+v", sandbox)
	}
	address, ok := sandbox.Broker("claude")
	if !ok || sandbox.Brokers["claude"] == 0 || address != fmt.Sprintf("http://127.0.0.1:%d", sandbox.Brokers["claude"]) {
		t.Fatalf("Broker(claude) = %q, %v", address, ok)
	}
	if _, ok := sandbox.Broker("github"); ok {
		t.Fatal("a broker the sandbox was not given has an address")
	}
	made := d.Sandboxes()[0]
	if got := *made.Labels; got["name"] != "hello-one" || got["team"] != "infra" {
		t.Fatalf("the daemon was sent labels %v", got)
	}
	// The caller's map is the caller's: Create adds the name to a copy.
	if labels["name"] != "overwritten" || len(labels) != 2 {
		t.Fatalf("Create changed the caller's labels to %v", labels)
	}
}

func TestCreateWithNoNameSetsNoLabel(t *testing.T) {
	d, client := fake(t)
	sandbox, err := client.Create(context.Background(), "", Spec{Image: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	if sandbox.Name != "" || d.Sandboxes()[0].Labels != nil {
		t.Fatalf("a nameless create was labelled: %q, %v", sandbox.Name, d.Sandboxes()[0].Labels)
	}
}

// The daemon made the machine and the answer was lost. The retry carries the
// same key, and gets that machine back rather than a second one.
func TestARetriedCreateCarriesOneKeyAndMakesOneMachine(t *testing.T) {
	d, client := fake(t)
	d.Intercept("createSandbox", fakedaemon.HangUpAfter())
	sandbox, err := client.Create(context.Background(), "once", Spec{Image: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	if got := len(d.Sandboxes()); got != 1 {
		t.Fatalf("%d sandboxes were made, want 1", got)
	}
	if sandbox.ID != d.Sandboxes()[0].Id {
		t.Fatalf("Create returned %s, and the daemon made %s", sandbox.ID, d.Sandboxes()[0].Id)
	}
	keys := d.IdempotencyKeys()
	if len(keys) < 2 {
		t.Fatalf("the create was sent %d times, want a retry", len(keys))
	}
	for _, key := range keys {
		if key != keys[0] || key == "" {
			t.Fatalf("the attempts carried keys %v, want one key", keys)
		}
	}
}

// A daemon that never answers is not asked forever, and every attempt is the
// same create.
func TestCreateGivesUpOnADaemonThatNeverAnswers(t *testing.T) {
	d, client := fake(t)
	d.InterceptAll("createSandbox", fakedaemon.HangUp())
	sandbox, err := client.Create(context.Background(), "", Spec{Image: "alpine"})
	if err == nil || sandbox != nil {
		t.Fatalf("Create = %v, %v; want an error and no handle", sandbox, err)
	}
	// At least createAttempts: net/http itself resends a request carrying an
	// Idempotency-Key once when a reused connection is hung up on.
	if calls := d.Calls("createSandbox"); calls < createAttempts || calls > 2*createAttempts {
		t.Fatalf("the create was attempted %d times, want %d", calls, createAttempts)
	}
	// The interceptor answered before the daemon read the header, so the
	// keys are the transport's to prove: every request is the same create.
	if len(d.Sandboxes()) != 0 {
		t.Fatal("a create nobody answered made a machine")
	}
}

// Ctrl-C between attempts is not waited out.
func TestACancelledCreateStopsRetrying(t *testing.T) {
	d, client := fake(t)
	ctx, cancel := context.WithCancel(context.Background())
	d.InterceptAll("createSandbox", func(w http.ResponseWriter, r *http.Request, _ http.HandlerFunc) {
		cancel()
		fakedaemon.HangUp()(w, r, nil)
	})
	got := within(t, createAsync(ctx, t, client, Spec{Image: "alpine"}))
	if got.err == nil || got.sandbox != nil {
		t.Fatalf("Create = %v, %v; want an error and no handle", got.sandbox, got.err)
	}
	if calls := d.Calls("createSandbox"); calls > 2 {
		t.Fatalf("the create was attempted %d times, though it was cancelled during the first", calls)
	}
}

// A refusal is an answer, and answers are not retried: a second attempt at a
// create the daemon refused would be refused the same way.
func TestARefusedCreateIsNotRetriedAndSaysWhy(t *testing.T) {
	d, client := fake(t)
	d.PutVolume("busy", 0)
	if _, err := client.Create(context.Background(), "", Spec{Image: "alpine", Disk: &genv1.DiskSpec{Volume: new("busy")}}); err != nil {
		t.Fatal(err)
	}
	sandbox, err := client.Create(context.Background(), "", Spec{Image: "alpine", Disk: &genv1.DiskSpec{Volume: new("busy")}})
	if CodeOf(err) != "volume_in_use" || sandbox != nil {
		t.Fatalf("Create on a held volume = %v, %v", sandbox, err)
	}
	if calls := d.Calls("createSandbox"); calls != 2 {
		t.Fatalf("two creates were sent %d times", calls)
	}
}

// An answer that arrived and does not parse is an error, and it is NOT
// retried: a retry is for an answer that never came. Through the generated
// `…WithResponse` the two were one error, and the second attempt quietly made
// the machine the first one had been answered about.
func TestACreateAnsweredWithGarbageIsAnError(t *testing.T) {
	d, client := fake(t)
	d.Intercept("createSandbox", fakedaemon.Answer(202, "application/json", "{not json"))
	sandbox, err := client.Create(context.Background(), "", Spec{Image: "alpine"})
	if err == nil || sandbox != nil || !strings.Contains(err.Error(), "answered 202 with a body that does not parse") {
		t.Fatalf("Create = %v, %v", sandbox, err)
	}
	if got := d.Calls("createSandbox"); got != 1 {
		t.Fatalf("the create was sent %d times, want once", got)
	}
}

// A sandbox that fails says why in the daemon's own sentence, and the handle
// comes back with the error so the caller can drop what failed.
func TestAFailedCreateSaysWhyAndCanBeDropped(t *testing.T) {
	d, client := fake(t, fakedaemon.WithBoot(func(genv1.SandboxSpec) fakedaemon.Outcome {
		return fakedaemon.Outcome{State: genv1.SandboxStateFailed, Error: "the kernel panicked mounting its root filesystem"}
	}))
	sandbox, err := client.Create(context.Background(), "doomed", Spec{Image: "alpine"})
	if err == nil || err.Error() != "the kernel panicked mounting its root filesystem" {
		t.Fatalf("err = %v", err)
	}
	if sandbox == nil || sandbox.State != genv1.SandboxStateFailed || sandbox.Name != "doomed" {
		t.Fatalf("handle = %+v", sandbox)
	}
	if err := sandbox.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := len(d.Sandboxes()); got != 0 {
		t.Fatalf("%d sandboxes left after dropping the failed one", got)
	}
}

func TestAFailedCreateWithNoSentenceStillSaysItFailed(t *testing.T) {
	_, client := fake(t, fakedaemon.WithBoot(func(genv1.SandboxSpec) fakedaemon.Outcome {
		return fakedaemon.Outcome{State: genv1.SandboxStateFailed}
	}))
	if _, err := client.Create(context.Background(), "", Spec{Image: "alpine"}); err == nil || err.Error() != "the sandbox failed" {
		t.Fatalf("err = %v", err)
	}
}

// `gone`, `stopped` and `paused` do not become `ready` by waiting. Create used
// to wait out the whole WaitTimeout — three minutes by default — for either of
// the first two.
func TestCreateReturnsAtOnceWhenTheSandboxWillNeverBeReady(t *testing.T) {
	for _, state := range []genv1.SandboxState{genv1.SandboxStateGone, genv1.SandboxStateStopped, genv1.SandboxStatePaused} {
		t.Run(string(state), func(t *testing.T) {
			d, client := fake(t, fakedaemon.WithBoot(holding))
			done := createAsync(t.Context(), t, client, Spec{Image: "alpine"})
			id := <-d.Created()
			d.Settle(id, state, "")
			got := within(t, done)
			if got.err == nil || !strings.Contains(got.err.Error(), string(state)) {
				t.Fatalf("err = %v", got.err)
			}
			if got.sandbox == nil || got.sandbox.ID != id || got.sandbox.State != state {
				t.Fatalf("handle = %+v", got.sandbox)
			}
		})
	}
}

// Ctrl-C while the machine boots. The daemon is making it regardless, so the
// handle comes back — it used to be nil, and the machine was nobody's to drop.
func TestACreateCancelledWhileWaitingReturnsWhatToDrop(t *testing.T) {
	d, client := fake(t, fakedaemon.WithBoot(holding))
	ctx, cancel := context.WithCancel(context.Background())
	// Cancelled once the caller follows the events, which is after it has the
	// 202: before that, it has no id to hand back.
	following := make(chan struct{})
	d.Intercept("streamSandboxEvents", func(w http.ResponseWriter, r *http.Request, serve http.HandlerFunc) {
		close(following)
		serve(w, r)
	})
	done := createAsync(ctx, t, client, Spec{Image: "alpine"})
	id := <-d.Created()
	<-following
	cancel()
	got := within(t, done)
	if !errors.Is(got.err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", got.err)
	}
	if got.sandbox == nil || got.sandbox.ID != id {
		t.Fatalf("handle = %+v, want the sandbox being made", got.sandbox)
	}
	if err := got.sandbox.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left := len(d.Sandboxes()); left != 0 {
		t.Fatalf("%d sandboxes left after dropping the cancelled create", left)
	}
}

func TestCreateGivesUpAfterWaitTimeoutAndSaysWhatItWasStill(t *testing.T) {
	d, client := fake(t, fakedaemon.WithBoot(holding))
	client.WaitTimeout = 50 * time.Millisecond
	sandbox, err := client.Create(context.Background(), "", Spec{Image: "alpine"})
	if err == nil || !strings.Contains(err.Error(), "still creating after 50ms") {
		t.Fatalf("err = %v", err)
	}
	if sandbox == nil || sandbox.ID != d.Sandboxes()[0].Id {
		t.Fatalf("handle = %+v", sandbox)
	}
}

func TestTheDefaultWaitIsThreeMinutes(t *testing.T) {
	if got := (&Client{}).waitTimeout(); got != 3*time.Minute {
		t.Fatalf("waitTimeout = %s", got)
	}
}

// A stream that ends before it says anything settled — a proxy cutting it, a
// daemon restarting — is not an answer: the sandbox is asked instead.
func TestCreateFallsBackToAskingWhenTheEventsStopShort(t *testing.T) {
	for name, intercept := range map[string]fakedaemon.Interceptor{
		"ends":    fakedaemon.Answer(200, "application/x-ndjson", `{"seq":1,"type":"state","state":"creating","at":"2026-09-26T00:00:00Z"}`+"\n"),
		"garbage": fakedaemon.Answer(200, "application/x-ndjson", "{this is not an event\n"),
		"refused": fakedaemon.Refuse(404, genv1.ErrorCodeNotFound, "no"),
		"hung up": fakedaemon.HangUp(),
	} {
		t.Run(name, func(t *testing.T) {
			d, client := fake(t, fakedaemon.WithBoot(holding))
			d.InterceptAll("streamSandboxEvents", intercept)
			done := createAsync(t.Context(), t, client, Spec{Image: "alpine"})
			id := <-d.Created()
			d.Settle(id, genv1.SandboxStateReady, "")
			if got := within(t, done); got.err != nil || got.sandbox.State != genv1.SandboxStateReady {
				t.Fatalf("Create = %+v, %v", got.sandbox, got.err)
			}
		})
	}
}

// The question that decides can itself fail; the handle still comes back.
func TestCreateSaysSoWhenTheSandboxCannotBeAskedAbout(t *testing.T) {
	for name, intercept := range map[string]fakedaemon.Interceptor{
		"refused": fakedaemon.Refuse(500, genv1.ErrorCodeInternal, "the state file is unreadable"),
		"garbage": fakedaemon.Answer(200, "application/json", "[]"),
		"hung up": fakedaemon.HangUp(),
	} {
		t.Run(name, func(t *testing.T) {
			d, client := fake(t)
			d.InterceptAll("getSandbox", intercept)
			sandbox, err := client.Create(context.Background(), "", Spec{Image: "alpine"})
			if err == nil {
				t.Fatal("Create succeeded without being able to ask")
			}
			if sandbox == nil || sandbox.ID != d.Sandboxes()[0].Id {
				t.Fatalf("handle = %+v", sandbox)
			}
		})
	}
}

// Several creates at once each get their own machine: one key per Create,
// not one per client.
func TestConcurrentCreatesEachMakeTheirOwnMachine(t *testing.T) {
	d, client := fake(t)
	const n = 8
	ids := make([]genv1.SandboxID, n)
	waits := make([]func(), n)
	for i := range n {
		waits[i] = spawn.Go(t, func() {
			sandbox, err := client.Create(t.Context(), fmt.Sprint("s", i), Spec{Image: "alpine"})
			if err != nil {
				t.Error(err)
				return
			}
			ids[i] = sandbox.ID
		})
	}
	for _, wait := range waits {
		wait()
	}
	seen := map[genv1.SandboxID]bool{}
	for _, id := range ids {
		seen[id] = true
	}
	keys := map[string]bool{}
	for _, key := range d.IdempotencyKeys() {
		keys[key] = true
	}
	if len(seen) != n || len(d.Sandboxes()) != n || len(keys) != n {
		t.Fatalf("%d creates made %d handles, %d sandboxes and used %d keys", n, len(seen), len(d.Sandboxes()), len(keys))
	}
}

func TestOpenAndListSeeWhatIsThere(t *testing.T) {
	d, client := fake(t)
	made, err := client.Create(context.Background(), "listed", Spec{Image: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	opened, err := client.Open(context.Background(), made.ID)
	if err != nil || opened.ID != made.ID || opened.Name != "listed" || opened.State != genv1.SandboxStateReady {
		t.Fatalf("Open = %+v, %v", opened, err)
	}
	all, err := client.List(context.Background())
	if err != nil || len(all) != 1 || all[0].Id != made.ID {
		t.Fatalf("List = %+v, %v", all, err)
	}
	if _, err := client.Open(context.Background(), uuid.New()); CodeOf(err) != "not_found" {
		t.Fatalf("Open of a sandbox that is not there = %v", err)
	}
	d.Intercept("listSandboxes", fakedaemon.Refuse(403, genv1.ErrorCodeForbidden, "no sandboxes.read"))
	if _, err := client.List(context.Background()); CodeOf(err) != "forbidden" {
		t.Fatalf("a refused List = %v", err)
	}
}

func ready(t *testing.T, client *Client) *Sandbox {
	t.Helper()
	sandbox, err := client.Create(context.Background(), "", Spec{Image: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	return sandbox
}

// A non-zero exit is an answer, not an error: the exchange worked.
func TestRunReturnsWhatTheCommandDidIncludingFailing(t *testing.T) {
	d, client := fake(t, fakedaemon.WithRun(func(_ context.Context, _ genv1.SandboxID, request genv1.CommandRequest) genv1.CommandResult {
		return genv1.CommandResult{ExitCode: 2, Stdout: "out:" + request.Argv[0], Stderr: "err", Truncated: true, DurationMs: 12}
	}))
	sandbox := ready(t, client)
	result, err := sandbox.Run(context.Background(), "ls", "/no such dir")
	if err != nil {
		t.Fatal(err)
	}
	if *result != (Result{ExitCode: 2, Stdout: "out:ls", Stderr: "err", Truncated: true, DurationMs: 12}) {
		t.Fatalf("result = %+v", result)
	}
	// argv is not a shell line: the space stays inside its argument.
	if got := d.Commands()[0].Argv; len(got) != 2 || got[1] != "/no such dir" {
		t.Fatalf("argv arrived as %q", got)
	}
	if _, err := sandbox.RunWith(context.Background(), genv1.CommandRequest{Argv: []string{"pwd"}, Dir: new("/work"), TimeoutSeconds: new(5)}); err != nil {
		t.Fatal(err)
	}
	if got := d.Commands()[1]; *got.Dir != "/work" || *got.TimeoutSeconds != 5 {
		t.Fatalf("RunWith sent %+v", got)
	}
}

func TestRunIsRefusedInTheDaemonsWords(t *testing.T) {
	d, client := fake(t)
	sandbox := ready(t, client)
	if _, err := sandbox.Run(context.Background()); CodeOf(err) != "bad_request" {
		t.Fatalf("Run with no argv = %v", err)
	}
	d.Settle(sandbox.ID, genv1.SandboxStateUnreachable, "")
	if _, err := sandbox.Run(context.Background(), "true"); CodeOf(err) != "sandbox_unreachable" {
		t.Fatalf("Run on an unreachable sandbox = %v", err)
	}
}

func TestFilesGoInAndComeOut(t *testing.T) {
	d, client := fake(t)
	sandbox := ready(t, client)
	if err := sandbox.WriteFile(context.Background(), "/work/a.txt", []byte("hello"), "0600"); err != nil {
		t.Fatal(err)
	}
	if err := sandbox.WriteFile(context.Background(), "/work/empty", nil, ""); err != nil {
		t.Fatal(err)
	}
	if body, mode, ok := d.File(sandbox.ID, "/work/a.txt"); !ok || string(body) != "hello" || mode != "0600" {
		t.Fatalf("the daemon holds %q mode %q (%v)", body, mode, ok)
	}
	if body, mode, ok := d.File(sandbox.ID, "/work/empty"); !ok || len(body) != 0 || mode != "" {
		t.Fatalf("an empty file with no mode arrived as %q mode %q (%v)", body, mode, ok)
	}
	back, err := sandbox.ReadFile(context.Background(), "/work/a.txt")
	if err != nil || string(back) != "hello" {
		t.Fatalf("ReadFile = %q, %v", back, err)
	}
	if _, err := sandbox.ReadFile(context.Background(), "/work/none"); CodeOf(err) != "not_found" {
		t.Fatalf("reading a file that is not there = %v", err)
	}
	if err := sandbox.WriteFile(context.Background(), "relative", []byte("x"), ""); CodeOf(err) != "bad_request" {
		t.Fatalf("writing a relative path = %v", err)
	}
}

// Close takes the disk with it and Release keeps it; that is the whole
// difference, and it is in the parameter the daemon receives.
func TestCloseDeletesTheDiskAndReleaseKeepsIt(t *testing.T) {
	d, client := fake(t)
	closed, released := ready(t, client), ready(t, client)
	if err := closed.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := released.Release(context.Background()); err != nil {
		t.Fatal(err)
	}
	deletes := d.Deletes()
	if len(deletes) != 2 || deletes[0] != (fakedaemon.Delete{ID: closed.ID, Disk: "delete"}) || deletes[1] != (fakedaemon.Delete{ID: released.ID, Disk: "keep"}) {
		t.Fatalf("the daemon was asked %+v", deletes)
	}
	// A deferred Close after an explicit one says what happened rather than
	// pretending it dropped something.
	if err := closed.Close(context.Background()); CodeOf(err) != "not_found" {
		t.Fatalf("closing twice = %v", err)
	}
}

func TestVolumesAreListedAndLetGo(t *testing.T) {
	d, client := fake(t)
	d.PutVolume("kept", 1<<20)
	sandbox, err := client.Create(context.Background(), "", Spec{Image: "alpine", Disk: &genv1.DiskSpec{Volume: new("held")}})
	if err != nil {
		t.Fatal(err)
	}
	volumes, err := client.Volumes(context.Background())
	if err != nil || len(volumes) != 2 || volumes[0].Name != "held" || volumes[0].SandboxId == nil || *volumes[0].SandboxId != sandbox.ID || volumes[1].SandboxId != nil {
		t.Fatalf("Volumes = %+v, %v", volumes, err)
	}
	if err := client.DeleteVolume(context.Background(), "held"); CodeOf(err) != "volume_in_use" {
		t.Fatalf("deleting a held volume = %v", err)
	}
	if err := client.DeleteVolume(context.Background(), "kept"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeleteVolume(context.Background(), "never-was"); err != nil {
		t.Fatalf("deleting a volume that is not there = %v, want success", err)
	}
	d.Intercept("listVolumes", fakedaemon.Refuse(403, genv1.ErrorCodeForbidden, "no"))
	if _, err := client.Volumes(context.Background()); CodeOf(err) != "forbidden" {
		t.Fatalf("a refused Volumes = %v", err)
	}
}

// Volumes are listed by label, and their labels set.
func TestVolumesAreFoundAndLabelledByLabel(t *testing.T) {
	d, client := fake(t)
	d.PutVolumeWith("chat", 1, map[string]string{"runyard.harness": "ryclaude"}, nil, time.Time{})
	d.PutVolumeWith("other", 1, nil, nil, time.Time{})
	volumes, err := client.Volumes(context.Background(), "runyard.harness=ryclaude")
	if err != nil || len(volumes) != 1 || volumes[0].Name != "chat" {
		t.Fatalf("Volumes = %+v, %v", volumes, err)
	}
	if err := client.SetVolumeLabels(context.Background(), "chat", map[string]string{"runyard.title": "x"}); err != nil {
		t.Fatal(err)
	}
	if labels, _ := d.VolumeLabels("chat"); labels["runyard.title"] != "x" || len(labels) != 1 {
		t.Errorf("labels %v", labels)
	}
	if err := client.SetVolumeLabels(context.Background(), "never-was", nil); CodeOf(err) != "not_found" {
		t.Errorf("labelling a volume that is not there = %v", err)
	}
	d.Intercept("setVolumeLabels", fakedaemon.HangUp())
	if err := client.SetVolumeLabels(context.Background(), "chat", nil); err == nil {
		t.Error("a hang-up was a success")
	}
}

func TestConsoleIsWhatTheGuestWrote(t *testing.T) {
	_, client := fake(t, fakedaemon.WithBoot(func(genv1.SandboxSpec) fakedaemon.Outcome {
		return fakedaemon.Outcome{Console: "[    0.000000] Linux version 6.12\n"}
	}))
	sandbox := ready(t, client)
	if got, err := sandbox.Console(context.Background()); err != nil || got != "[    0.000000] Linux version 6.12\n" {
		t.Fatalf("Console = %q, %v", got, err)
	}
}

// A refusal is not the console. It used to be returned as though the guest
// had printed it.
func TestConsoleOfASandboxThatIsGoneIsAnError(t *testing.T) {
	_, client := fake(t)
	sandbox := ready(t, client)
	if err := sandbox.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	got, err := sandbox.Console(context.Background())
	if CodeOf(err) != "not_found" || got != "" {
		t.Fatalf("Console of a dropped sandbox = %q, %v", got, err)
	}
}

// Something between the caller and the daemon — a proxy, a load balancer —
// answers in its own shape, and what it said is the only diagnosis there is.
// Error() used to drop it and say only the status.
func TestAnErrorThatIsNotTheContractsShapeKeepsWhatItSaid(t *testing.T) {
	for name, tc := range map[string]struct {
		intercept fakedaemon.Interceptor
		want      string
	}{
		"html page":         {fakedaemon.Answer(502, "text/html", "<h1>502 Bad Gateway</h1>\n"), "runyard-sandboxes: HTTP 502: <h1>502 Bad Gateway</h1>"},
		"json without code": {fakedaemon.Answer(500, "application/json", `{"message":"boom"}`), `runyard-sandboxes: HTTP 500: {"message":"boom"}`},
		"empty":             {fakedaemon.Answer(503, "", ""), "runyard-sandboxes: HTTP 503"},
		"the contract's":    {fakedaemon.Refuse(507, genv1.ErrorCodeCapacityExceeded, "eight of eight"), "runyard-sandboxes: capacity_exceeded: eight of eight"},
	} {
		t.Run(name, func(t *testing.T) {
			d, client := fake(t)
			d.Intercept("getInfo", tc.intercept)
			_, err := client.Info(context.Background())
			if err == nil || err.Error() != tc.want {
				t.Fatalf("err = %v, want %s", err, tc.want)
			}
		})
	}
}

func TestCodeOfFindsTheCodeThroughWrapping(t *testing.T) {
	refusal := &Error{Status: 409, Code: "volume_in_use", Message: "held"}
	if got := CodeOf(fmt.Errorf("creating: %w", refusal)); got != "volume_in_use" {
		t.Fatalf("CodeOf(wrapped) = %q", got)
	}
	if got := CodeOf(errors.New("plain")); got != "" {
		t.Fatalf("CodeOf(plain) = %q", got)
	}
	if got := CodeOf(nil); got != "" {
		t.Fatalf("CodeOf(nil) = %q", got)
	}
}

// Every call says so when the daemon hangs up, rather than returning a zero
// value that looks like an answer.
func TestEveryCallFailsWhenTheDaemonHangsUp(t *testing.T) {
	d, client := fake(t)
	sandbox := ready(t, client)
	for _, op := range []string{"getInfo", "getSandbox", "listSandboxes", "runCommand", "writeFile", "readFile", "deleteSandbox", "listVolumes", "deleteVolume", "getSandboxConsole"} {
		d.InterceptAll(op, fakedaemon.HangUp())
	}
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"Info":         func() error { _, err := client.Info(ctx); return err },
		"Open":         func() error { _, err := client.Open(ctx, sandbox.ID); return err },
		"List":         func() error { _, err := client.List(ctx); return err },
		"Run":          func() error { _, err := sandbox.Run(ctx, "true"); return err },
		"WriteFile":    func() error { return sandbox.WriteFile(ctx, "/a", nil, "") },
		"ReadFile":     func() error { _, err := sandbox.ReadFile(ctx, "/a"); return err },
		"Close":        func() error { return sandbox.Close(ctx) },
		"Volumes":      func() error { _, err := client.Volumes(ctx); return err },
		"DeleteVolume": func() error { return client.DeleteVolume(ctx, "v") },
		"Console":      func() error { _, err := sandbox.Console(ctx); return err },
	} {
		if err := call(); err == nil {
			t.Errorf("%s succeeded against a daemon that hung up", name)
		}
	}
}

// An answer whose body is not what the contract says is an error, not a zero
// value.
func TestAnAnswerThatDoesNotDecodeIsAnError(t *testing.T) {
	d, client := fake(t)
	sandbox := ready(t, client)
	for _, op := range []string{"getInfo", "getSandbox", "listSandboxes", "runCommand", "listVolumes"} {
		d.InterceptAll(op, fakedaemon.Answer(200, "application/json", "{"))
	}
	ctx := context.Background()
	if _, err := client.Info(ctx); err == nil {
		t.Error("Info decoded garbage")
	}
	if _, err := client.Open(ctx, sandbox.ID); err == nil {
		t.Error("Open decoded garbage")
	}
	if _, err := client.List(ctx); err == nil {
		t.Error("List decoded garbage")
	}
	if _, err := sandbox.Run(ctx, "true"); err == nil {
		t.Error("Run decoded garbage")
	}
	if _, err := client.Volumes(ctx); err == nil {
		t.Error("Volumes decoded garbage")
	}
}

// A success status whose body is not the JSON the contract declares leaves
// the generated response with no typed field filled in. That is a daemon — or
// something in front of one — answering outside the contract, and it is an
// error that says so rather than a zero-valued struct that reads as an answer.
func TestASuccessTheContractDoesNotDescribeIsAnError(t *testing.T) {
	d, client := fake(t)
	sandbox := ready(t, client)
	for _, op := range []string{"getInfo", "getSandbox", "listSandboxes", "runCommand", "listVolumes"} {
		d.InterceptAll(op, fakedaemon.Answer(200, "text/html", "<h1>welcome</h1>"))
	}
	d.InterceptAll("createSandbox", fakedaemon.Answer(202, "text/plain", "accepted"))
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"Info":    func() error { _, err := client.Info(ctx); return err },
		"Open":    func() error { _, err := client.Open(ctx, sandbox.ID); return err },
		"List":    func() error { _, err := client.List(ctx); return err },
		"Run":     func() error { _, err := sandbox.Run(ctx, "true"); return err },
		"Volumes": func() error { _, err := client.Volumes(ctx); return err },
		"Create":  func() error { _, err := client.Create(ctx, "", Spec{Image: "alpine"}); return err },
	} {
		err := call()
		var refusal *Error
		if !errors.As(err, &refusal) || refusal.Status/100 != 2 || refusal.Code != "" ||
			!strings.Contains(err.Error(), "with a body the contract does not describe") {
			t.Errorf("%s = %v, want a refusal naming the undescribed body", name, err)
		}
	}
}

// The operations whose success has no JSON body are held to their status: a
// 200 where the contract says 204 is not a delete that happened.
func TestAnOperationAnsweredWithTheWrongSuccessIsAnError(t *testing.T) {
	d, client := fake(t)
	sandbox := ready(t, client)
	for _, op := range []string{"writeFile", "deleteSandbox", "deleteVolume"} {
		d.InterceptAll(op, fakedaemon.Answer(200, "application/json", "{}"))
	}
	d.InterceptAll("readFile", fakedaemon.Answer(204, "", ""))
	d.InterceptAll("getSandboxConsole", fakedaemon.Answer(202, "text/plain", "later"))
	ctx := context.Background()
	for name, call := range map[string]func() error{
		"WriteFile":    func() error { return sandbox.WriteFile(ctx, "/a", []byte("x"), "") },
		"Close":        func() error { return sandbox.Close(ctx) },
		"Release":      func() error { return sandbox.Release(ctx) },
		"DeleteVolume": func() error { return client.DeleteVolume(ctx, "v") },
		"ReadFile":     func() error { _, err := sandbox.ReadFile(ctx, "/a"); return err },
		"Console":      func() error { _, err := sandbox.Console(ctx); return err },
	} {
		var refusal *Error
		if err := call(); !errors.As(err, &refusal) || refusal.Status/100 != 2 {
			t.Errorf("%s = %v, want a refusal carrying the status it got", name, err)
		}
	}
}

// A refusal is quoted, and capped: an HTML page the size of a novel from a
// proxy is not an error message anybody reads to the end.
func TestARefusalIsQuotedUpToAMebibyte(t *testing.T) {
	d, client := fake(t)
	d.Intercept("getInfo", fakedaemon.Answer(500, "text/plain", strings.Repeat("x", 3<<20)))
	_, err := client.Info(context.Background())
	var refusal *Error
	if !errors.As(err, &refusal) || refusal.Status != 500 || len(refusal.Message) != 1<<20 {
		t.Fatalf("err = %T, message of %d bytes", err, len(refusal.Message))
	}
}

// API is the generated client on the same daemon, with the same key: an
// operation this package does not wrap is one call away, not a second client
// to configure.
func TestAPIIsTheGeneratedClientOnTheSameDaemonAndKey(t *testing.T) {
	d, client := fake(t)
	sandbox := ready(t, client)
	ctx := context.Background()

	got, err := client.API().GetSandboxWithResponse(ctx, sandbox.ID)
	if err != nil || got.JSON200 == nil || got.JSON200.Id != sandbox.ID {
		t.Fatalf("GetSandbox through API() = %v %d %s", err, got.StatusCode(), got.Body)
	}
	info, err := client.API().GetInfoWithResponse(ctx)
	if err != nil || info.JSON200 == nil {
		t.Fatalf("the key did not travel: %v %d %s", err, info.StatusCode(), info.Body)
	}
	if got := d.Calls("getSandbox"); got < 1 {
		t.Fatalf("the fake never saw the call: %d", got)
	}
}

// A followed stream through API() is read as it is written, through the raw
// methods the typed client embeds — which is what its doc comment promises
// and what a `…WithResponse` method, reading to the end first, could not do.
func TestAFollowedStreamThroughAPIIsNotBuffered(t *testing.T) {
	d, client := fake(t, fakedaemon.WithBoot(holding))
	created, err := client.API().CreateSandboxWithResponse(context.Background(), &genv1.CreateSandboxParams{IdempotencyKey: "k"}, Spec{Image: "alpine"})
	if err != nil || created.JSON202 == nil {
		t.Fatalf("create = %v", err)
	}
	id := created.JSON202.Id

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	follow := genv1.FollowQuery(true)
	res, err := client.API().StreamSandboxEvents(ctx, id, &genv1.StreamSandboxEventsParams{Follow: &follow})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	decoder := json.NewDecoder(res.Body)
	var first genv1.SandboxEvent
	if err := decoder.Decode(&first); err != nil || first.State == nil || *first.State != genv1.SandboxStateCreating {
		t.Fatalf("the first event, while the sandbox is still creating = %+v, %v", first, err)
	}
	d.Settle(id, genv1.SandboxStateReady, "")
	for {
		var event genv1.SandboxEvent
		if err := decoder.Decode(&event); err != nil {
			t.Fatalf("the stream ended before it said ready: %v", err)
		}
		if event.State != nil && *event.State == genv1.SandboxStateReady {
			return
		}
	}
}

// A rule is put by its name — added, then replaced — and deleted by it, and
// the others are left as they were.
func TestARuleIsPutAndDeletedByItsName(t *testing.T) {
	d, client := fake(t)
	sandbox := ready(t, client)
	ctx := context.Background()
	domains := []string{"ifconfig.me"}
	if added, err := sandbox.PutRule(ctx, "ifconfig", Rule{Domains: &domains}); err != nil || !added {
		t.Fatalf("added %v, %v", added, err)
	}
	ssh := []string{"tcp/22"}
	if added, err := sandbox.PutRule(ctx, "ifconfig", Rule{Domains: &domains, Ports: &ssh}); err != nil || added {
		t.Fatalf("a replacement: added %v, %v", added, err)
	}
	cidrs := []string{"192.0.2.0/24"}
	if _, err := sandbox.PutRule(ctx, "mirror", Rule{Cidrs: &cidrs}); err != nil {
		t.Fatal(err)
	}
	rules := d.Rules(sandbox.ID)
	if len(rules) != 2 || rules[0].Name != "ifconfig" || (*rules[0].Ports)[0] != "tcp/22" || rules[1].Name != "mirror" {
		t.Fatalf("the daemon has %+v", rules)
	}

	if err := sandbox.DeleteRule(ctx, "ifconfig"); err != nil {
		t.Fatal(err)
	}
	if rules := d.Rules(sandbox.ID); len(rules) != 1 || rules[0].Name != "mirror" {
		t.Fatalf("after deleting one, the daemon has %+v", rules)
	}
	if err := sandbox.DeleteRule(ctx, "ifconfig"); CodeOf(err) != "not_found" {
		t.Fatalf("deleting it again = %v", err)
	}
}

// A rule is paused and resumed by its name, kept where it was and as it was
// written; either, asked again, is answered as it is.
func TestARuleIsPausedAndResumedByItsName(t *testing.T) {
	d, client := fake(t)
	sandbox := ready(t, client)
	ctx := context.Background()
	domains, ssh := []string{"github.com"}, []string{"tcp/22"}
	if _, err := sandbox.PutRule(ctx, "github-ssh", Rule{Domains: &domains, Ports: &ssh}); err != nil {
		t.Fatal(err)
	}
	cidrs := []string{"192.0.2.0/24"}
	if _, err := sandbox.PutRule(ctx, "mirror", Rule{Cidrs: &cidrs}); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		paused, err := sandbox.PauseRule(ctx, "github-ssh")
		if err != nil || paused.Paused == nil || !*paused.Paused || paused.Name != "github-ssh" || (*paused.Ports)[0] != "tcp/22" {
			t.Fatalf("paused = %+v, %v", paused, err)
		}
	}
	rules := d.Rules(sandbox.ID)
	if len(rules) != 2 || rules[0].Name != "github-ssh" || rules[0].Paused == nil || rules[1].Paused != nil {
		t.Fatalf("the daemon has %+v", rules)
	}
	for range 2 {
		resumed, err := sandbox.ResumeRule(ctx, "github-ssh")
		if err != nil || resumed.Paused != nil || (*resumed.Domains)[0] != "github.com" {
			t.Fatalf("resumed = %+v, %v", resumed, err)
		}
	}
	if _, err := sandbox.PauseRule(ctx, "nope"); CodeOf(err) != "not_found" {
		t.Fatalf("pausing a rule that is not there = %v", err)
	}
	if _, err := sandbox.ResumeRule(ctx, "nope"); CodeOf(err) != "not_found" {
		t.Fatalf("resuming a rule that is not there = %v", err)
	}
	// Put paused, it is paused; put again without it, it is not.
	paused := true
	if _, err := sandbox.PutRule(ctx, "mirror", Rule{Cidrs: &cidrs, Paused: &paused}); err != nil {
		t.Fatal(err)
	}
	if rules := d.Rules(sandbox.ID); rules[1].Paused == nil || !*rules[1].Paused {
		t.Fatalf("put paused: %+v", rules[1])
	}
	if _, err := sandbox.PutRule(ctx, "mirror", Rule{Cidrs: &cidrs}); err != nil {
		t.Fatal(err)
	}
	if rules := d.Rules(sandbox.ID); rules[1].Paused != nil {
		t.Fatalf("put without paused: %+v", rules[1])
	}

	d.Settle(sandbox.ID, genv1.SandboxStateStopped, "")
	if _, err := sandbox.PauseRule(ctx, "mirror"); CodeOf(err) != "conflict" {
		t.Fatalf("pausing on a stopped sandbox = %v", err)
	}
	if _, err := sandbox.ResumeRule(ctx, "mirror"); CodeOf(err) != "conflict" {
		t.Fatalf("resuming on a stopped sandbox = %v", err)
	}
}

func TestARuleIsRefusedInTheDaemonsWords(t *testing.T) {
	d, client := fake(t)
	sandbox := ready(t, client)
	ctx := context.Background()
	if _, err := sandbox.PutRule(ctx, "empty", Rule{}); CodeOf(err) != "bad_request" || !strings.Contains(err.Error(), "opens nothing") {
		t.Fatalf("a rule that opens nothing = %v", err)
	}
	domains := []string{"a.example"}
	if _, err := sandbox.PutRule(ctx, "Not-A-Name", Rule{Domains: &domains}); CodeOf(err) != "bad_request" {
		t.Fatalf("a name that cannot be one = %v", err)
	}
	d.Settle(sandbox.ID, genv1.SandboxStateStopped, "")
	if _, err := sandbox.PutRule(ctx, "a", Rule{Domains: &domains}); CodeOf(err) != "conflict" {
		t.Fatalf("a stopped sandbox = %v", err)
	}
	// An answer outside the contract is said to be, rather than read as a
	// rule that was added.
	d.Intercept("putEgressRule", fakedaemon.Answer(http.StatusOK, "text/html", "<html>proxy</html>"))
	if _, err := sandbox.PutRule(ctx, "a", Rule{Domains: &domains}); err == nil || !strings.Contains(err.Error(), "does not describe") {
		t.Fatalf("a body the contract does not describe = %v", err)
	}
}

// What the sandbox tried to reach and was refused, as its report has it.
func TestEgressIsTheSandboxsNetworkAsTheHostHasIt(t *testing.T) {
	d, client := fake(t)
	sandbox := ready(t, client)
	ctx := context.Background()
	domains := []string{"ifconfig.me"}
	if _, err := sandbox.PutRule(ctx, "ifconfig", Rule{Domains: &domains}); err != nil {
		t.Fatal(err)
	}
	d.RecordRefusal(sandbox.ID, genv1.EgressRefusal{At: time.Now().UTC(), Target: "httpbin.org", Kind: new(genv1.Dns), Reason: new("no rule allows it")})
	egress, err := sandbox.Egress(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if rules := *egress.Policy.Rules; len(rules) != 1 || rules[0].Name != "ifconfig" {
		t.Fatalf("policy %+v", egress.Policy)
	}
	if len(egress.Refusals) != 1 || egress.Refusals[0].Target != "httpbin.org" {
		t.Fatalf("refusals %+v", egress.Refusals)
	}
	if err := sandbox.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.Egress(ctx); CodeOf(err) != "not_found" {
		t.Fatalf("a dropped sandbox's egress = %v", err)
	}
	if err := sandbox.DeleteRule(ctx, "ifconfig"); CodeOf(err) != "not_found" {
		t.Fatalf("a dropped sandbox's rule = %v", err)
	}
}

// Connections are killed by what is named, and what was killed is the
// answer; they are gone from the report after.
func TestConnectionsAreKilledByWhatIsNamed(t *testing.T) {
	d, client := fake(t)
	sandbox := ready(t, client)
	ctx := context.Background()
	for _, c := range []genv1.Connection{
		{Protocol: "tcp", Source: "172.30.0.5:40000", Destination: "140.82.121.4:443"},
		{Protocol: "udp", Source: "172.30.0.5:5353", Destination: "140.82.121.4:3478"},
		{Protocol: "tcp", Source: "172.30.0.5:40001", Destination: "192.0.2.1:443"},
	} {
		d.OpenConnection(sandbox.ID, c)
	}
	destination, protocol := "140.82.121.4", "tcp"
	killed, err := sandbox.KillConnections(ctx, Kill{Destination: &destination, Protocol: &protocol})
	if err != nil || len(killed) != 1 || killed[0].Source != "172.30.0.5:40000" {
		t.Fatalf("killed %+v, %v", killed, err)
	}
	source := "172.30.0.5:40001"
	if killed, err := sandbox.KillConnections(ctx, Kill{Source: &source}); err != nil || len(killed) != 1 {
		t.Fatalf("killed %+v, %v", killed, err)
	}
	// Again: nothing left, and that is no error.
	if killed, err := sandbox.KillConnections(ctx, Kill{Source: &source}); err != nil || len(killed) != 0 {
		t.Fatalf("killed %+v, %v", killed, err)
	}
	egress, err := sandbox.Egress(ctx)
	if err != nil || len(egress.Connections) != 1 || egress.Connections[0].Protocol != "udp" {
		t.Fatalf("left %+v, %v", egress, err)
	}
	if _, err := sandbox.KillConnections(ctx, Kill{}); CodeOf(err) != "bad_request" || !strings.Contains(err.Error(), "a destination or a source") {
		t.Fatalf("naming neither = %v", err)
	}
	if err := sandbox.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.KillConnections(ctx, Kill{Source: &source}); CodeOf(err) != "not_found" {
		t.Fatalf("a dropped sandbox = %v", err)
	}
}

// A transport that fails is the error, for each of them.
func TestRuleCallsThatNeverReachTheDaemonSaySo(t *testing.T) {
	_, client := fake(t)
	sandbox := ready(t, client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	domains := []string{"a.example"}
	if _, err := sandbox.PutRule(ctx, "a", Rule{Domains: &domains}); !errors.Is(err, context.Canceled) {
		t.Errorf("PutRule = %v", err)
	}
	if err := sandbox.DeleteRule(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("DeleteRule = %v", err)
	}
	if _, err := sandbox.PauseRule(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("PauseRule = %v", err)
	}
	if _, err := sandbox.ResumeRule(ctx, "a"); !errors.Is(err, context.Canceled) {
		t.Errorf("ResumeRule = %v", err)
	}
	destination := "192.0.2.1"
	if _, err := sandbox.KillConnections(ctx, Kill{Destination: &destination}); !errors.Is(err, context.Canceled) {
		t.Errorf("KillConnections = %v", err)
	}
	if _, err := sandbox.Egress(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Egress = %v", err)
	}
}

func TestATunnelIsPublishedPausedTokenedAndTakenAwayByItsName(t *testing.T) {
	d, client := fake(t)
	sandbox := ready(t, client)
	ctx := context.Background()
	emails := []openapi_types.Email{"ada@example.com"}
	made, created, err := sandbox.PutTunnel(ctx, "web", TunnelSpec{Port: 3000, Access: &genv1.TunnelAccess{Emails: &emails}})
	if err != nil || !created || made.Slug == "" || made.Url == nil || made.Status.State != genv1.Served {
		t.Fatalf("PutTunnel = %+v, %v, %v", made, created, err)
	}
	replaced, created, err := sandbox.PutTunnel(ctx, "web", TunnelSpec{Port: 3001})
	if err != nil || created || replaced.Slug != made.Slug || replaced.Port != 3001 {
		t.Fatalf("replaced = %+v, %v, %v", replaced, created, err)
	}
	all, err := sandbox.Tunnels(ctx)
	if err != nil || len(all) != 1 {
		t.Fatalf("Tunnels = %+v, %v", all, err)
	}
	if paused, err := sandbox.PauseTunnel(ctx, "web"); err != nil || !paused.Paused {
		t.Fatalf("PauseTunnel = %+v, %v", paused, err)
	}
	if resumed, err := sandbox.ResumeTunnel(ctx, "web"); err != nil || resumed.Paused {
		t.Fatalf("ResumeTunnel = %+v, %v", resumed, err)
	}
	token, err := sandbox.MintTunnelToken(ctx, "web", "ci")
	if err != nil || !strings.HasPrefix(token.Token, "tgt_"+token.Id+"_") {
		t.Fatalf("MintTunnelToken = %+v, %v", token, err)
	}
	revoked, err := sandbox.RevokeTunnelToken(ctx, "web", token.Id, "done")
	if err != nil || !revoked.Revoked || *revoked.RevokedReason != "done" {
		t.Fatalf("RevokeTunnelToken = %+v, %v", revoked, err)
	}
	if again, err := sandbox.RevokeTunnelToken(ctx, "web", token.Id, ""); err != nil || !again.Revoked {
		t.Fatalf("revoked again with no reason = %+v, %v", again, err)
	}
	ada := "ada@example.com"
	d.RecordVisit(sandbox.ID, "web", genv1.TunnelRequest{ActorKind: genv1.User, Actor: &ada, Method: "GET", Path: "/", Status: 200})
	page, err := sandbox.TunnelActivity(ctx, "web", 10, "")
	if err != nil || len(page.Items) != 1 || *page.Items[0].Actor != ada {
		t.Fatalf("TunnelActivity = %+v, %v", page, err)
	}
	if _, err := sandbox.TunnelActivity(ctx, "web", 0, "0000000000000001"); err != nil {
		t.Fatalf("TunnelActivity from a cursor = %v", err)
	}
	host, err := client.HostTunnels(ctx)
	if err != nil || !host.Relay.Docked || len(host.Items) != 1 || host.Items[0].SandboxId == nil {
		t.Fatalf("HostTunnels = %+v, %v", host, err)
	}
	counted, err := client.Counts(ctx)
	if err != nil || counted.Sandboxes == nil || *counted.Sandboxes < 1 || *counted.Tunnels != 1 {
		t.Fatalf("Counts = %+v, %v", counted, err)
	}
	promised, err := client.HostCommitments(ctx)
	if err != nil || promised.Volumes < 1 || promised.Running < 1 {
		t.Fatalf("HostCommitments = %+v, %v", promised, err)
	}
	paged, err := client.HostTunnelsPage(ctx, 1, "")
	if err != nil || len(paged.Items) != 1 || paged.NextCursor != nil || !paged.Relay.Docked {
		t.Fatalf("HostTunnelsPage = %+v, %v", paged, err)
	}
	if _, err := client.HostTunnelsPage(ctx, 1, "forged"); CodeOf(err) != "bad_request" {
		t.Fatalf("HostTunnelsPage from a forged cursor = %v", err)
	}
	one, err := sandbox.Tunnel(ctx, "web")
	if err != nil || one.Slug != made.Slug || len(one.Tokens) != 1 {
		t.Fatalf("Tunnel = %+v, %v", one, err)
	}
	if none, err := sandbox.TunnelGuests(ctx, "web"); err != nil || len(none) != 0 {
		t.Fatalf("TunnelGuests before any = %+v, %v", none, err)
	}
	d.AddTunnelGuest(sandbox.ID, "web", "grace@example.com", ada)
	d.AddTunnelGuest(sandbox.ID, "web", "linus@example.com", ada)
	guests, err := sandbox.TunnelGuests(ctx, "web")
	if err != nil || len(guests) != 2 || guests[0].Email != "grace@example.com" || guests[0].By != ada {
		t.Fatalf("TunnelGuests = %+v, %v", guests, err)
	}
	if err := sandbox.RevokeTunnelGuest(ctx, "web", "grace@example.com"); err != nil {
		t.Fatalf("RevokeTunnelGuest = %v", err)
	}
	if err := sandbox.RevokeTunnelGuest(ctx, "web", "grace@example.com"); CodeOf(err) != "not_found" {
		t.Errorf("a guest revoked twice = %v", err)
	}
	if left, err := sandbox.TunnelGuests(ctx, "web"); err != nil || len(left) != 1 || left[0].Email != "linus@example.com" {
		t.Fatalf("TunnelGuests after a revoke = %+v, %v", left, err)
	}
	if err := sandbox.DeleteTunnel(ctx, "web"); err != nil {
		t.Fatal(err)
	}
	if _, err := sandbox.Tunnel(ctx, "web"); CodeOf(err) != "not_found" {
		t.Errorf("a deleted tunnel = %v", err)
	}
}

func TestATunnelRefusedByTheDaemonIsTheDaemonsWords(t *testing.T) {
	_, client := fake(t)
	sandbox := ready(t, client)
	ctx := context.Background()
	if _, _, err := sandbox.PutTunnel(ctx, "web", TunnelSpec{Port: 0}); CodeOf(err) != "bad_request" {
		t.Errorf("PutTunnel = %v", err)
	}
	for name, err := range map[string]error{
		"Tunnel":            func() error { _, err := sandbox.Tunnel(ctx, "nope"); return err }(),
		"PauseTunnel":       func() error { _, err := sandbox.PauseTunnel(ctx, "nope"); return err }(),
		"ResumeTunnel":      func() error { _, err := sandbox.ResumeTunnel(ctx, "nope"); return err }(),
		"DeleteTunnel":      sandbox.DeleteTunnel(ctx, "nope"),
		"MintTunnelToken":   func() error { _, err := sandbox.MintTunnelToken(ctx, "nope", "x"); return err }(),
		"RevokeTunnelToken": func() error { _, err := sandbox.RevokeTunnelToken(ctx, "nope", "abcdefgh", ""); return err }(),
		"TunnelActivity":    func() error { _, err := sandbox.TunnelActivity(ctx, "nope", 0, ""); return err }(),
		"TunnelGuests":      func() error { _, err := sandbox.TunnelGuests(ctx, "nope"); return err }(),
		"RevokeTunnelGuest": sandbox.RevokeTunnelGuest(ctx, "nope", "grace@example.com"),
	} {
		if CodeOf(err) != "not_found" {
			t.Errorf("%s = %v", name, err)
		}
	}
	gone, err := client.Open(ctx, sandbox.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := gone.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := gone.Tunnels(ctx); CodeOf(err) != "not_found" {
		t.Errorf("Tunnels of a sandbox that is gone = %v", err)
	}
}

func TestTunnelCallsThatNeverReachTheDaemonSaySo(t *testing.T) {
	_, client := fake(t)
	sandbox := ready(t, client)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, err := range map[string]error{
		"PutTunnel":         func() error { _, _, err := sandbox.PutTunnel(ctx, "web", TunnelSpec{Port: 1}); return err }(),
		"Tunnels":           func() error { _, err := sandbox.Tunnels(ctx); return err }(),
		"Tunnel":            func() error { _, err := sandbox.Tunnel(ctx, "web"); return err }(),
		"PauseTunnel":       func() error { _, err := sandbox.PauseTunnel(ctx, "web"); return err }(),
		"ResumeTunnel":      func() error { _, err := sandbox.ResumeTunnel(ctx, "web"); return err }(),
		"DeleteTunnel":      sandbox.DeleteTunnel(ctx, "web"),
		"MintTunnelToken":   func() error { _, err := sandbox.MintTunnelToken(ctx, "web", "x"); return err }(),
		"RevokeTunnelToken": func() error { _, err := sandbox.RevokeTunnelToken(ctx, "web", "abcdefgh", ""); return err }(),
		"TunnelActivity":    func() error { _, err := sandbox.TunnelActivity(ctx, "web", 0, ""); return err }(),
		"TunnelGuests":      func() error { _, err := sandbox.TunnelGuests(ctx, "web"); return err }(),
		"RevokeTunnelGuest": sandbox.RevokeTunnelGuest(ctx, "web", "grace@example.com"),
		"HostTunnels":       func() error { _, err := client.HostTunnels(ctx); return err }(),
		"HostTunnelsPage":   func() error { _, err := client.HostTunnelsPage(ctx, 1, "c"); return err }(),
		"Counts":            func() error { _, err := client.Counts(ctx); return err }(),
		"HostCommitments":   func() error { _, err := client.HostCommitments(ctx); return err }(),
	} {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s = %v", name, err)
		}
	}
}

func TestATunnelsGuestsWhileTheGatewayIsAwayAreTheDaemonsWords(t *testing.T) {
	d, client := fake(t)
	sandbox := ready(t, client)
	ctx := context.Background()
	away := fakedaemon.Refuse(http.StatusServiceUnavailable, genv1.ErrorCodeUpstreamError, "the gateway is not answering")
	d.Intercept("listTunnelGuests", away)
	d.Intercept("revokeTunnelGuest", away)
	if _, err := sandbox.TunnelGuests(ctx, "web"); CodeOf(err) != "upstream_error" || !strings.Contains(err.Error(), "gateway") {
		t.Errorf("TunnelGuests = %v", err)
	}
	if err := sandbox.RevokeTunnelGuest(ctx, "web", "grace@example.com"); CodeOf(err) != "upstream_error" {
		t.Errorf("RevokeTunnelGuest = %v", err)
	}
}

func TestATunnelsShareButtonIsPutAndReadBack(t *testing.T) {
	_, client := fake(t)
	sandbox := ready(t, client)
	ctx := context.Background()
	made, _, err := sandbox.PutTunnel(ctx, "web", TunnelSpec{Port: 1})
	if err != nil || *made.Overlay.Hidden || *made.Overlay.Corner != genv1.BottomRight {
		t.Fatalf("PutTunnel without one = %+v, %v", made.Overlay, err)
	}
	corner, hidden := genv1.TopRight, true
	if _, _, err := sandbox.PutTunnel(ctx, "web", TunnelSpec{Port: 1, Overlay: &genv1.TunnelOverlay{Hidden: &hidden, Corner: &corner}}); err != nil {
		t.Fatal(err)
	}
	one, err := sandbox.Tunnel(ctx, "web")
	if err != nil || !*one.Overlay.Hidden || *one.Overlay.Corner != genv1.TopRight {
		t.Fatalf("Tunnel = %+v, %v", one.Overlay, err)
	}
	wrong := genv1.TunnelOverlayCorner("middle")
	if _, _, err := sandbox.PutTunnel(ctx, "web", TunnelSpec{Port: 1, Overlay: &genv1.TunnelOverlay{Corner: &wrong}}); CodeOf(err) != "bad_request" {
		t.Errorf("a corner that is not one = %v", err)
	}
}

func TestTheHostsTunnelsRefusedAreTheDaemonsWords(t *testing.T) {
	d, client := fake(t)
	d.Intercept("listHostTunnels", fakedaemon.Refuse(http.StatusForbidden, genv1.ErrorCodeForbidden, "no scope"))
	if _, err := client.HostTunnels(context.Background()); CodeOf(err) != "forbidden" {
		t.Errorf("HostTunnels = %v", err)
	}
}
