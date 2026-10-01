// Package sdk is the Go client for runyard-sandboxes: spawn a microVM, do
// things in it, drop it.
//
// It is a thin, opinionated layer over the GENERATED client in
// [genv1], and thin is deliberate. The generated half is what the
// contract produces, so it cannot drift from the document: every request is
// built by it and every answer parsed by it, into the type the contract
// declares for that status. This half exists for the three things a generated
// client cannot know:
//
//   - **Waiting.** Creating a sandbox is asynchronous and a caller wants a
//     machine, not a `202`. `Create` follows its events until it is ready, and
//     returns the error the daemon recorded when it does not.
//   - **Closing.** A sandbox that outlives the program that made it is the
//     expensive mistake this API makes easy, so the handle has a `Close` and
//     the examples all defer it.
//   - **Ergonomics at the edge.** `Run` returns stdout, stderr and an exit
//     code; `WriteFile` takes bytes; `PutRule` opens a name to a sandbox by a
//     rule's name. The generated client returns a `*http.Response` and a
//     pointer to a struct with eleven optional fields.
//
// It adds no behaviour of its own. Anything it can do is something the contract
// describes, and anything it cannot is not there — a convenience that talked to
// a route the document does not have would be a second, undocumented API. The
// rest of the contract — processes, metrics, brokers, images, keys — is
// [Client.API]: the generated client itself, on the same address and key.
//
// It is in a module of its own, with the contract's generated code and nothing
// else, so a program that requires it does not require the daemon.
package sdk

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// Client talks to one daemon.
type Client struct {
	api *genv1.ClientWithResponses
	// WaitTimeout bounds Create's wait for a machine to answer. Zero uses three
	// minutes, which is a large image on a cold cache.
	WaitTimeout time.Duration
}

// Option configures a client.
type Option func(*options)

type options struct {
	httpClient *http.Client
	key        string
}

// WithKey attaches the API key every request carries.
func WithKey(key string) Option {
	return func(o *options) { o.key = key }
}

// WithHTTPClient replaces the transport, for a caller with its own timeouts,
// proxy or instrumentation.
func WithHTTPClient(client *http.Client) Option {
	return func(o *options) { o.httpClient = client }
}

// New points a client at a daemon.
//
// The address is checked here rather than on the first call: `127.0.0.1:8099`
// with no scheme parses as a URL, and every call made with it would fail with
// "unsupported protocol scheme", which names nothing the caller typed.
func New(baseURL string, opts ...Option) (*Client, error) {
	if parsed, err := url.Parse(baseURL); err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return nil, fmt.Errorf("runyard-sandboxes: %q is not a daemon's address: it takes http:// or https:// and a host", baseURL)
	}
	settings := &options{}
	for _, opt := range opts {
		opt(settings)
	}
	clientOpts := []genv1.ClientOption{}
	if settings.httpClient != nil {
		clientOpts = append(clientOpts, genv1.WithHTTPClient(settings.httpClient))
	} else {
		// No client-level timeout by default: this API streams — a followed
		// log, a tar going in, a command with a deadline of its own — and one
		// here would cut them. What bounds a call is the call's own context.
		clientOpts = append(clientOpts, genv1.WithHTTPClient(&http.Client{}))
	}
	if settings.key != "" {
		key := settings.key
		clientOpts = append(clientOpts, genv1.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
			req.Header.Set("Authorization", "Bearer "+key)
			return nil
		}))
	}
	// NewClient's error is its options', and none of these can fail.
	api, _ := genv1.NewClientWithResponses(strings.TrimSuffix(baseURL, "/"), clientOpts...)
	return &Client{api: api}, nil
}

// API is the generated client this one is built on, for every operation the
// contract has and this package does not wrap. It talks to the same daemon with
// the same key and transport, and each of its answers is parsed into the type
// the contract declares for that status.
//
// A followed stream — events, a process's output — is read through the
// embedded [genv1.ClientInterface] methods, which hand back the response
// unread; the `…WithResponse` methods read a body to its end before returning.
func (c *Client) API() *genv1.ClientWithResponses { return c.api }

// Info is what the host says about itself, including what brokers it declares.
func (c *Client) Info(ctx context.Context) (*genv1.HostInfo, error) {
	res, err := c.api.GetInfoWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200, nil
}

// Spec is what a sandbox should be. It is the contract's own type: a second
// spelling of it here would be a second thing to keep in step.
type Spec = genv1.SandboxSpec

// Sandbox is a machine, and the handle you work through.
type Sandbox struct {
	// ID is what the daemon minted: a UUID that names this machine and no
	// other, ever.
	ID genv1.SandboxID
	// Name is its `name` label, which is what a person reads and nothing
	// else. Not unique.
	Name string
	// State is what it was when this handle was last refreshed.
	State genv1.SandboxState
	// Brokers are the loopback ports inside the machine, by name.
	Brokers map[string]int
	// Identity is what an upstream sees when this sandbox calls it.
	Identity *genv1.Identity

	client *Client
}

// Create makes a sandbox and waits for it to answer.
//
// name becomes the `name` label, which is what the console shows; it is not
// the sandbox's id and need not be unique. Empty sets no label.
//
// The create is retried, with ONE idempotency key, when the request fails
// before an answer arrives: the daemon may have started the machine and lost
// the response, and the key is what makes the retry return that machine rather
// than make a second one.
//
// **Once the daemon has accepted the create, the handle comes back even with
// an error** — the sandbox failed, went away, did not answer in time, or ctx
// ended while it was being waited for. The machine exists either way, and a
// caller that got nil could not drop it: `if sandbox != nil { sandbox.Close }`
// belongs on the error path too.
func (c *Client) Create(ctx context.Context, name string, spec Spec) (*Sandbox, error) {
	if name != "" {
		labels := map[string]string{}
		if spec.Labels != nil {
			maps.Copy(labels, *spec.Labels)
		}
		labels["name"] = name
		spec.Labels = &labels
	}
	params := &genv1.CreateSandboxParams{IdempotencyKey: uuid.NewString()}

	// The raw call is what is retried, and the parse is outside the loop: a
	// retry is for an answer that never ARRIVED, and the generated
	// `…WithResponse` reports one that arrived and would not parse the same
	// way — which, retried, made a second attempt of a create the daemon had
	// already answered.
	var raw *http.Response
	var err error
	pause := backoff{first: 250 * time.Millisecond, most: time.Second}
	for attempt := 1; ; attempt++ {
		raw, err = c.api.CreateSandbox(ctx, params, spec)
		if err == nil || attempt == createAttempts || !pause.wait(ctx) {
			break
		}
	}
	if err != nil {
		return nil, err
	}
	// The parse closes it as well; closing twice is nothing, and a body whose
	// only close is inside generated code is one no reader here can see closed.
	defer closeBody(raw.Body)
	res, err := genv1.ParseCreateSandboxResponse(raw)
	if err != nil {
		return nil, fmt.Errorf("runyard-sandboxes: the create was answered %d with a body that does not parse: %w", raw.StatusCode, err)
	}
	// 202 is a machine this create started, and 200 one an earlier attempt
	// with the same key already had.
	created := res.JSON202
	if created == nil {
		created = res.JSON200
	}
	if created == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return c.wait(ctx, *created)
}

// createAttempts bounds Create's retries. Enough to ride out a daemon
// restarting or a connection reset; not so many that a daemon that is simply
// not there takes long to say so.
const createAttempts = 4

// Open returns a handle to a sandbox that already exists.
func (c *Client) Open(ctx context.Context, id genv1.SandboxID) (*Sandbox, error) {
	res, err := c.api.GetSandboxWithResponse(ctx, id)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return c.handle(*res.JSON200), nil
}

// List is every sandbox on the host.
func (c *Client) List(ctx context.Context) ([]genv1.Sandbox, error) {
	res, err := c.api.ListSandboxesWithResponse(ctx, &genv1.ListSandboxesParams{})
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200.Items, nil
}

func (c *Client) waitTimeout() time.Duration {
	if c.WaitTimeout > 0 {
		return c.WaitTimeout
	}
	return 3 * time.Minute
}

// wait returns once the sandbox is ready or has failed.
//
// It follows the sandbox's events, which say `ready` the moment the daemon
// knows it, and then asks the sandbox once: asking is what decides. A stream
// that ended before anything settled — a proxy cutting it, the daemon
// restarting — is followed again, after a pause that grows: what is across a
// network cannot say when it can be reached again. Asking every 200ms
// instead was up to a fifth of a second added to a create that takes under
// one.
//
// Every return after the create was accepted carries a handle, the last one
// the daemon described: see Create.
func (c *Client) wait(ctx context.Context, created genv1.Sandbox) (*Sandbox, error) {
	id := created.Id
	last := c.handle(created)
	waiting, cancel := context.WithTimeout(ctx, c.waitTimeout())
	defer cancel()
	pause := backoff{first: 50 * time.Millisecond, most: 2 * time.Second}
	for {
		c.untilSettled(waiting, id)
		// Asked in ctx rather than waiting: one that has run out is still
		// told what the sandbox is, to say what it was still.
		res, err := c.api.GetSandboxWithResponse(ctx, id)
		if err != nil {
			return last, err
		}
		if res.JSON200 == nil {
			return last, refused(res.HTTPResponse, res.Body)
		}
		sandbox := *res.JSON200
		last = c.handle(sandbox)
		switch sandbox.State {
		case genv1.SandboxStateReady:
			return last, nil
		case genv1.SandboxStateFailed:
			// The daemon's own sentence, which is the one that says the kernel
			// panicked mounting its root filesystem rather than "creation
			// failed".
			message := "the sandbox failed"
			if sandbox.Error != nil {
				message = *sandbox.Error
			}
			return last, errors.New(message)
		case genv1.SandboxStateGone, genv1.SandboxStateStopped, genv1.SandboxStatePaused:
			// None becomes ready by waiting: somebody stopped it or paused it,
			// or the machine left the host. Waiting out the timeout for either was
			// three minutes of a caller staring at nothing.
			return last, fmt.Errorf("sandbox %s is %s, and will not become ready", id, sandbox.State)
		case genv1.SandboxStateCreating, genv1.SandboxStateBooting, genv1.SandboxStateUnreachable:
			// On its way, or out of touch in a way a retry can fix: the
			// timeout decides how long that is worth.
		}
		if !pause.wait(waiting) {
			if err := ctx.Err(); err != nil {
				return last, err
			}
			return last, fmt.Errorf("sandbox %s was still %s after %s", id, sandbox.State, c.waitTimeout())
		}
	}
}

// untilSettled follows a sandbox's events until one says it has reached a
// state waiting will not change, or until they cannot be followed. It says
// nothing about which: the state is read after, from the sandbox itself.
func (c *Client) untilSettled(ctx context.Context, id genv1.SandboxID) {
	follow := genv1.FollowQuery(true)
	res, err := c.api.StreamSandboxEvents(ctx, id, &genv1.StreamSandboxEventsParams{Follow: &follow})
	if err != nil {
		return
	}
	defer closeBody(res.Body)
	if res.StatusCode != http.StatusOK {
		return
	}
	decoder := json.NewDecoder(res.Body)
	for {
		var event genv1.SandboxEvent
		if err := decoder.Decode(&event); err != nil {
			return
		}
		if event.State == nil {
			continue
		}
		switch *event.State {
		case genv1.SandboxStateReady, genv1.SandboxStateFailed, genv1.SandboxStateGone, genv1.SandboxStateStopped,
			genv1.SandboxStatePaused:
			return
		case genv1.SandboxStateCreating, genv1.SandboxStateBooting, genv1.SandboxStateUnreachable:
		}
	}
}

func (c *Client) handle(sandbox genv1.Sandbox) *Sandbox {
	handle := &Sandbox{ID: sandbox.Id, State: sandbox.State, client: c, Identity: sandbox.Identity}
	if sandbox.Labels != nil {
		handle.Name = (*sandbox.Labels)["name"]
	}
	if sandbox.Brokers != nil {
		handle.Brokers = map[string]int{}
		for _, broker := range *sandbox.Brokers {
			handle.Brokers[broker.Name] = broker.Port
		}
	}
	return handle
}

// Result is what a command did.
type Result struct {
	ExitCode int
	Stdout   string
	Stderr   string
	// Truncated says the output hit the host's cap, so what you have is the
	// beginning of it.
	Truncated  bool
	DurationMs int64
}

// Run runs one thing and collects what it printed.
//
// `argv` is already split and is not a shell line: nothing here splits it, so a
// path with a space in it is not a bug waiting to happen. A caller who wants
// shell syntax asks for a shell — `Run(ctx, "sh", "-lc", "…")` — and owns what
// that means.
//
// A non-zero exit is NOT an error: it is a successful exchange with an
// unsuccessful command, and a caller that cannot tell those apart cannot tell a
// broken machine from a failing test suite.
func (s *Sandbox) Run(ctx context.Context, argv ...string) (*Result, error) {
	return s.RunWith(ctx, genv1.CommandRequest{Argv: argv})
}

// RunWith is Run with the rest of the contract's options: a directory, an
// environment, a user, a deadline.
func (s *Sandbox) RunWith(ctx context.Context, request genv1.CommandRequest) (*Result, error) {
	res, err := s.client.api.RunCommandWithResponse(ctx, s.ID, request)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	result := res.JSON200
	return &Result{
		ExitCode:   result.ExitCode,
		Stdout:     result.Stdout,
		Stderr:     result.Stderr,
		Truncated:  result.Truncated,
		DurationMs: result.DurationMs,
	}, nil
}

// WriteFile puts bytes in the sandbox.
//
// **Whatever goes in here is disclosed to the workload**, immediately and by
// definition — there is no such thing as a file the sandbox holds and the code
// inside it cannot read. A credential belongs on the other side of the wire:
// that is what the broker plane is for.
func (s *Sandbox) WriteFile(ctx context.Context, path string, body []byte, mode string) error {
	params := &genv1.WriteFileParams{Path: path}
	if mode != "" {
		params.Mode = &mode
	}
	res, err := s.client.api.WriteFileWithBodyWithResponse(ctx, s.ID, params,
		"application/octet-stream", bytes.NewReader(body))
	if err != nil {
		return err
	}
	return want(http.StatusNoContent, res.HTTPResponse, res.Body)
}

// ReadFile takes bytes out. A directory comes back as a tar.
func (s *Sandbox) ReadFile(ctx context.Context, path string) ([]byte, error) {
	res, err := s.client.api.ReadFileWithResponse(ctx, s.ID, &genv1.ReadFileParams{Path: path})
	if err != nil {
		return nil, err
	}
	if err := want(http.StatusOK, res.HTTPResponse, res.Body); err != nil {
		return nil, err
	}
	return res.Body, nil
}

// Broker is the loopback address, inside the sandbox, of a brokered upstream.
//
// It is here because it is the one thing a caller writing a workload needs and
// cannot guess: what the port is. What makes a call to it work is on the host —
// the sandbox holds no credential — so this is an address, not a capability.
func (s *Sandbox) Broker(name string) (string, bool) {
	port, ok := s.Brokers[name]
	if !ok {
		return "", false
	}
	return fmt.Sprintf("http://127.0.0.1:%d", port), true
}

// Rule is what one egress rule opens: its domains and CIDRs, on its ports.
// No ports is the web ports, 80 and 443.
type Rule = genv1.EgressRuleSpec

// PutRule adds an egress rule to a running sandbox, or replaces the one with
// this name, and says whether it was added. It returns once the host's
// firewall and the sandbox's resolver enforce it. The other rules are left as
// they are, and sending the same rule again changes nothing.
func (s *Sandbox) PutRule(ctx context.Context, name string, rule Rule) (bool, error) {
	res, err := s.client.api.PutEgressRuleWithResponse(ctx, s.ID, name, rule)
	if err != nil {
		return false, err
	}
	switch {
	case res.JSON201 != nil:
		return true, nil
	case res.JSON200 != nil:
		return false, nil
	}
	return false, refused(res.HTTPResponse, res.Body)
}

// PauseRule pauses an egress rule, keeping it: what only it opens is closed
// until it is resumed. It answers with the rule as it is now; one already
// paused is answered as it is. A rule the sandbox does not have is an error
// with the code `not_found`.
func (s *Sandbox) PauseRule(ctx context.Context, name string) (*genv1.EgressRule, error) {
	res, err := s.client.api.PauseEgressRuleWithResponse(ctx, s.ID, name)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200, nil
}

// ResumeRule puts a paused egress rule back in force, as it was written.
func (s *Sandbox) ResumeRule(ctx context.Context, name string) (*genv1.EgressRule, error) {
	res, err := s.client.api.ResumeEgressRuleWithResponse(ctx, s.ID, name)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200, nil
}

// DeleteRule takes an egress rule away: what only it allowed is closed at
// once. A rule the sandbox does not have is an error with the code
// `not_found`.
func (s *Sandbox) DeleteRule(ctx context.Context, name string) error {
	res, err := s.client.api.DeleteEgressRuleWithResponse(ctx, s.ID, name)
	if err != nil {
		return err
	}
	return want(http.StatusNoContent, res.HTTPResponse, res.Body)
}

// Kill is which of a sandbox's connections to kill: those that match
// everything it names, and at least a destination or a source.
type Kill = genv1.KillConnections

// KillConnections ends the connections the sandbox has open that match, and
// answers with what it ended — nothing, when nothing matched. A rule that
// still allows the destination lets the workload connect again: pause or
// delete the rule first to close it for good.
func (s *Sandbox) KillConnections(ctx context.Context, kill Kill) ([]genv1.Connection, error) {
	res, err := s.client.api.KillConnectionsWithResponse(ctx, s.ID, kill)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200.Killed, nil
}

// TunnelSpec is what a tunnel publishes: a port inside the sandbox, to the
// addresses and domains its access names.
type TunnelSpec = genv1.TunnelSpec

// Tunnel is a published port as the host reports it: its address, its tokens'
// records, and whether a visitor can reach it now.
type Tunnel = genv1.Tunnel

// PutTunnel publishes a port under a name, or replaces the tunnel with that
// name, and says whether it was made. A replaced tunnel keeps its address and
// its tokens; a tunnel made again after a delete is at a new address.
func (s *Sandbox) PutTunnel(ctx context.Context, name string, spec TunnelSpec) (*Tunnel, bool, error) {
	res, err := s.client.api.PutTunnelWithResponse(ctx, s.ID, name, spec)
	if err != nil {
		return nil, false, err
	}
	switch {
	case res.JSON201 != nil:
		return res.JSON201, true, nil
	case res.JSON200 != nil:
		return res.JSON200, false, nil
	}
	return nil, false, refused(res.HTTPResponse, res.Body)
}

// Tunnels is every tunnel the sandbox has, in the order they were made.
func (s *Sandbox) Tunnels(ctx context.Context) ([]Tunnel, error) {
	res, err := s.client.api.ListTunnelsWithResponse(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200.Items, nil
}

// Tunnel is one of the sandbox's tunnels. One it does not have is an error
// with the code `not_found`.
func (s *Sandbox) Tunnel(ctx context.Context, name string) (*Tunnel, error) {
	res, err := s.client.api.GetTunnelWithResponse(ctx, s.ID, name)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200, nil
}

// PauseTunnel stops serving a tunnel and keeps it, address and tokens; the
// connections open through it are ended.
func (s *Sandbox) PauseTunnel(ctx context.Context, name string) (*Tunnel, error) {
	res, err := s.client.api.PauseTunnelWithResponse(ctx, s.ID, name)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200, nil
}

// ResumeTunnel serves a paused tunnel again, at the address it had.
func (s *Sandbox) ResumeTunnel(ctx context.Context, name string) (*Tunnel, error) {
	res, err := s.client.api.ResumeTunnelWithResponse(ctx, s.ID, name)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200, nil
}

// DeleteTunnel takes a tunnel away: its address answers nothing and the
// connections through it are ended.
func (s *Sandbox) DeleteTunnel(ctx context.Context, name string) error {
	res, err := s.client.api.DeleteTunnelWithResponse(ctx, s.ID, name)
	if err != nil {
		return err
	}
	return want(http.StatusNoContent, res.HTTPResponse, res.Body)
}

// MintTunnelToken draws a token that opens one tunnel from a command line, as
// `Authorization: Bearer <token>`. The answer is the only time the token
// exists outside the caller.
func (s *Sandbox) MintTunnelToken(ctx context.Context, name, comment string) (*genv1.TunnelTokenCreated, error) {
	res, err := s.client.api.CreateTunnelTokenWithResponse(ctx, s.ID, name, genv1.TunnelTokenRequest{Comment: comment})
	if err != nil {
		return nil, err
	}
	if res.JSON201 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON201, nil
}

// RevokeTunnelToken withdraws a token and keeps its record, with why.
func (s *Sandbox) RevokeTunnelToken(ctx context.Context, name, tokenID, reason string) (*genv1.TunnelToken, error) {
	params := &genv1.RevokeTunnelTokenParams{}
	if reason != "" {
		params.Reason = &reason
	}
	res, err := s.client.api.RevokeTunnelTokenWithResponse(ctx, s.ID, name, tokenID, params)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200, nil
}

// TunnelActivity is a page of who reached a tunnel and what they asked for,
// newest first, as the gateway recorded it. cursor is the last page's
// NextCursor, or empty for the first.
func (s *Sandbox) TunnelActivity(ctx context.Context, name string, limit int, cursor string) (*genv1.TunnelActivityPage, error) {
	params := &genv1.GetTunnelActivityParams{}
	if limit > 0 {
		params.Limit = &limit
	}
	if cursor != "" {
		params.Cursor = &cursor
	}
	res, err := s.client.api.GetTunnelActivityWithResponse(ctx, s.ID, name, params)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200, nil
}

// TunnelGuest is a person one of a tunnel's admins — the addresses in its
// access.emails — let in from its share page. They may reach it, and share
// nothing.
type TunnelGuest = genv1.TunnelGuest

// TunnelGuests is who a tunnel's admins let in, oldest first. They are kept
// by the gateway and read through the host's relay, so while either is away
// this is an error with the code `upstream_error` that says which.
func (s *Sandbox) TunnelGuests(ctx context.Context, name string) ([]TunnelGuest, error) {
	res, err := s.client.api.ListTunnelGuestsWithResponse(ctx, s.ID, name)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200.Items, nil
}

// RevokeTunnelGuest takes a guest back out: they reach nothing from their
// next request on. An admin is not a guest; take them out of the tunnel's
// access.emails with PutTunnel instead.
func (s *Sandbox) RevokeTunnelGuest(ctx context.Context, name, email string) error {
	res, err := s.client.api.RevokeTunnelGuestWithResponse(ctx, s.ID, name, email)
	if err != nil {
		return err
	}
	return want(http.StatusNoContent, res.HTTPResponse, res.Body)
}

// Counts is how many sandboxes, tunnels, volumes, images and shared brokers
// the host has — each only if this client's key may read the list it counts,
// and nil otherwise.
func (c *Client) Counts(ctx context.Context) (*genv1.HostCounts, error) {
	res, err := c.api.GetCountsWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200, nil
}

// HostCommitments is what the host's sandboxes were made with, added up: the
// vCPUs and memory of those with a machine, and the disks of all of them.
func (c *Client) HostCommitments(ctx context.Context) (*genv1.HostCommitments, error) {
	res, err := c.api.GetHostCommitmentsWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200, nil
}

// HostTunnels is every tunnel on the host, and the relay that serves them.
func (c *Client) HostTunnels(ctx context.Context) (*genv1.HostTunnels, error) {
	return c.HostTunnelsPage(ctx, 0, "")
}

// HostTunnelsPage is a page of the host's tunnels, oldest first, and the relay
// that serves them. limit 0 is every one that is left; cursor is the last
// page's NextCursor, or empty for the first.
func (c *Client) HostTunnelsPage(ctx context.Context, limit int, cursor string) (*genv1.HostTunnels, error) {
	params := &genv1.ListHostTunnelsParams{}
	if limit > 0 {
		params.Limit = &limit
	}
	if cursor != "" {
		params.Cursor = &cursor
	}
	res, err := c.api.ListHostTunnelsWithResponse(ctx, params)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200, nil
}

// Egress is the sandbox's network as its host enforces it: its rules, what
// its names are pinned to, what it has open, and what it tried to reach and
// was refused.
func (s *Sandbox) Egress(ctx context.Context) (*genv1.EgressStatus, error) {
	res, err := s.client.api.GetSandboxEgressWithResponse(ctx, s.ID)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200, nil
}

// Close drops the sandbox and its disk — a volume included, if it has one.
//
// `defer sandbox.Close(ctx)` is the point of this method existing: a machine
// that outlives the program that made it costs memory on somebody's host until
// a person notices.
func (s *Sandbox) Close(ctx context.Context) error {
	return s.drop(ctx, "delete")
}

// Release drops the sandbox and keeps its disk: on a volume, for the next
// sandbox that names it. It returns once the machine is down and the volume
// is free, so a create that follows it can have the volume at once.
func (s *Sandbox) Release(ctx context.Context) error {
	return s.drop(ctx, "keep")
}

func (s *Sandbox) drop(ctx context.Context, what string) error {
	disk := genv1.DeleteSandboxParamsDisk(what)
	res, err := s.client.api.DeleteSandboxWithResponse(ctx, s.ID, &genv1.DeleteSandboxParams{Disk: &disk})
	if err != nil {
		return err
	}
	return want(http.StatusNoContent, res.HTTPResponse, res.Body)
}

// Volumes is every volume on the host, with the sandbox holding each.
func (c *Client) Volumes(ctx context.Context) ([]genv1.Volume, error) {
	res, err := c.api.ListVolumesWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200.Items, nil
}

// DeleteVolume lets a volume go, and what was written on it. Deleting one that
// is not there succeeds; one a sandbox holds is refused with `volume_in_use`.
func (c *Client) DeleteVolume(ctx context.Context, name string) error {
	res, err := c.api.DeleteVolumeWithResponse(ctx, name)
	if err != nil {
		return err
	}
	return want(http.StatusNoContent, res.HTTPResponse, res.Body)
}

// Console is what the guest wrote to its serial port: the whole diagnosis when
// a machine never reaches its agent.
func (s *Sandbox) Console(ctx context.Context) (string, error) {
	res, err := s.client.api.GetSandboxConsoleWithResponse(ctx, s.ID, &genv1.GetSandboxConsoleParams{})
	if err != nil {
		return "", err
	}
	// Without this, a refusal came back as the console: a `404` for a sandbox
	// that is gone read as a guest that printed `{"error":…}` and nothing else.
	if err := want(http.StatusOK, res.HTTPResponse, res.Body); err != nil {
		return "", err
	}
	return string(res.Body), nil
}

// want is nil for the one status an operation answers with when it did what it
// was asked, and the refusal for any other. For the operations whose success
// has no JSON body — a 204, a file's bytes, a console — where the generated
// response has no typed field to find empty.
func want(status int, res *http.Response, body []byte) error {
	if res.StatusCode == status {
		return nil
	}
	return refused(res, body)
}

// Error is a refusal, in the shape every failure in this contract has.
//
// The code is a closed set, so a caller branches on it rather than on a string:
// `image_unknown`, `unsupported_spec` and `capacity_exceeded` each have their
// own next move, and a shared `bad_request` cannot tell them apart.
type Error struct {
	Status  int
	Code    string
	Message string
}

func (e *Error) Error() string {
	if e.Code == "" {
		// Not the contract's shape — a proxy's page, a load balancer's
		// sentence — and what it says is the only diagnosis there is.
		if e.Message != "" {
			return fmt.Sprintf("runyard-sandboxes: HTTP %d: %s", e.Status, e.Message)
		}
		return fmt.Sprintf("runyard-sandboxes: HTTP %d", e.Status)
	}
	return fmt.Sprintf("runyard-sandboxes: %s: %s", e.Code, e.Message)
}

// refused is the error for an answer that is not the success an operation
// declares. The generated client has already read the body; what is here is
// turning it into the contract's error — every refusal the document describes
// has the one shape — or, for one that is not in it, keeping what it said.
//
// It is also what a success status becomes when its body was not the JSON the
// contract declares, which the generated client leaves with no typed field
// filled in: that is a daemon answering outside its contract, and saying so
// beats handing a caller a zero-valued struct.
func refused(res *http.Response, body []byte) error {
	if len(body) > 1<<20 {
		body = body[:1<<20]
	}
	var envelope genv1.Error
	if json.Unmarshal(body, &envelope) == nil && envelope.Error.Code != "" {
		return &Error{Status: res.StatusCode, Code: string(envelope.Error.Code), Message: envelope.Error.Message}
	}
	message := strings.TrimSpace(string(body))
	if res.StatusCode/100 == 2 {
		message = fmt.Sprintf("the daemon answered %d with a body the contract does not describe: %q", res.StatusCode, message)
	}
	return &Error{Status: res.StatusCode, Message: message}
}

// CodeOf is the contract's error code out of an error, for a caller that wants
// to branch without a type assertion.
func CodeOf(err error) string {
	if apiError, ok := errors.AsType[*Error](err); ok {
		return apiError.Code
	}
	return ""
}

// closeBody closes a response the SDK has finished with. Its error is not
// worth returning: the answer has already been read, and a failure to close
// changes nothing about it.
func closeBody(body io.Closer) { _ = body.Close() }
