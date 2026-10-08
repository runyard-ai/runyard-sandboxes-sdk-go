// Package fakedaemon is a runyard-sandboxes daemon made of maps: the WIRE of
// the real one, answering the way openapi/sandboxes.yaml says, with no
// hypervisor behind it.
//
// It is the double for everything that talks to a daemon through the SDK — the
// SDK itself and the example programs — so that each of them is tested against
// HTTP exchanges rather than against an interface invented for the purpose.
// The daemon's repository holds it to the contract with the same harness that
// holds the real daemon to it, so a shape it gets wrong is a failing test there
// rather than a false pass everywhere it is used. Its behaviour is tested here,
// beside it.
//
// A test is whatever the daemon would have behind it: Settle is the machine
// booting, Approve a person pressing a button, DialBroker the workload
// connecting to a broker the code under test serves.
//
// It is in the SDK's module so that the SDK's tests can use it — and so that a
// program built on the SDK can test against it too, without a daemon. Import
// it from tests only: it is a double, not a server.
package fakedaemon

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// Outcome is what a sandbox becomes once it is created.
//
// The zero value is `ready`. `creating` means it stays creating until the test
// calls Settle, which is how a test holds a create in the middle.
type Outcome struct {
	State genv1.SandboxState
	// Error is the daemon's sentence for a `failed` one.
	Error string
	// Console is what its serial port says.
	Console string
	// Progress is what the daemon says it is doing on the way there: an
	// image pulled, a disk made.
	Progress []string
}

// Interceptor answers an operation in place of the daemon, once. serve is the
// daemon's own handler, for an interceptor that wants the operation to happen
// and the answer to go astray.
type Interceptor func(w http.ResponseWriter, r *http.Request, serve http.HandlerFunc)

// Delete is one DELETE of a sandbox, as the daemon received it.
type Delete struct {
	ID   genv1.SandboxID
	Disk string
}

// Option configures a daemon.
type Option func(*Daemon)

// WithKey makes every request carry `Authorization: Bearer key`, as a real
// daemon does, and answers 401 to one that does not — or a key the daemon
// minted itself from an approval, until it is revoked or expires.
func WithKey(key string) Option { return func(d *Daemon) { d.key = key } }

// WithBrokers declares shared brokers on the host, by name.
func WithBrokers(names ...string) Option {
	return func(d *Daemon) {
		for _, name := range names {
			d.brokers = append(d.brokers, genv1.Broker{Name: name, Url: "https://" + name + ".invalid:8443/"})
		}
	}
}

// WithBoot decides what each created sandbox becomes.
func WithBoot(boot func(genv1.SandboxSpec) Outcome) Option {
	return func(d *Daemon) { d.boot = boot }
}

// WithRun decides what a command does. The context is the request's, so a
// command that blocks on it ends when the caller gives up.
func WithRun(run func(ctx context.Context, id genv1.SandboxID, request genv1.CommandRequest) genv1.CommandResult) Option {
	return func(d *Daemon) { d.run = run }
}

// WithNetwork is a host that gives sandboxes an interface, as `-network`
// does, and says so in its info.
func WithNetwork() Option { return func(d *Daemon) { d.network = true } }

// WithClock is the time it goes by — when a tunnel expires, what a record
// says it was made at — for a test about time that moves it rather than
// waiting for it to pass.
func WithClock(now func() time.Time) Option { return func(d *Daemon) { d.now = now } }

// Daemon is the fake. Its methods are safe to call while requests are served.
type Daemon struct {
	// URL is where it listens.
	URL string

	key     string
	brokers []genv1.Broker
	boot    func(genv1.SandboxSpec) Outcome
	run     func(context.Context, genv1.SandboxID, genv1.CommandRequest) genv1.CommandResult
	tty     func(context.Context, genv1.SandboxID, *Terminal) int
	network bool
	google  bool
	// brokerKeepAlive is how often a caller serving a broker is asked whether
	// it is still there.
	brokerKeepAlive time.Duration
	now             func() time.Time
	created         chan genv1.SandboxID

	mu           sync.Mutex
	sandboxes    map[genv1.SandboxID]*sandbox
	order        []genv1.SandboxID
	volumes      map[string]*volume
	idempotent   map[string]idempotent
	keys         []string
	deletes      []Delete
	commands     []genv1.CommandRequest
	calls        map[string]int
	intercepts   map[string][]Interceptor
	interceptAll map[string]Interceptor
	// approvals are the keys somebody approved and nobody has redeemed, by
	// their code; minted the keys redeemed and not revoked since.
	approvals map[string]approval
	minted    []genv1.KeyCreated
	// changed is closed, and made again, whenever a caller's broker is given,
	// served or taken away.
	changed chan struct{}
	// terminals is every terminal open, which the daemon's end hangs up.
	terminals map[*websocket.Conn]struct{}

	// serving is every terminal's handler and every client broker's, which
	// outlive the requests they were once: a WebSocket's connection is the
	// handler's, not the server's.
	serving sync.WaitGroup
}

type sandbox struct {
	record genv1.Sandbox
	// refused is what a test said the sandbox tried to reach and could not,
	// newest last.
	refused []genv1.EgressRefusal
	// open is what a test said the sandbox has open, as its report lists it.
	open    []genv1.Connection
	tunnels []*tunnel
	// callers is who holds each of its client brokers, by the broker's name:
	// a client broker is there for as long as its caller is.
	callers map[string]*caller
	files   map[string][]byte
	modes   map[string]string
	console string
	events  []genv1.SandboxEvent
	changed chan struct{}
}

type volume struct {
	createdAt  time.Time
	lastUsedAt time.Time
	holder     *genv1.SandboxID
	bytes      int64
	labels     map[string]string
	createdBy  *genv1.Creator
}

type idempotent struct {
	id   genv1.SandboxID
	spec []byte
}

// New starts a daemon for the length of the test.
func New(tb testing.TB, opts ...Option) *Daemon {
	tb.Helper()
	d := &Daemon{
		created:         make(chan genv1.SandboxID, 128),
		sandboxes:       map[genv1.SandboxID]*sandbox{},
		volumes:         map[string]*volume{},
		idempotent:      map[string]idempotent{},
		calls:           map[string]int{},
		intercepts:      map[string][]Interceptor{},
		interceptAll:    map[string]Interceptor{},
		approvals:       map[string]approval{},
		terminals:       map[*websocket.Conn]struct{}{},
		changed:         make(chan struct{}),
		brokerKeepAlive: brokerKeepAlive,
		now:             time.Now,
	}
	for _, opt := range opts {
		opt(d)
	}
	server := httptest.NewServer(d.routes())
	// Streams that follow a sandbox hold their connections open; closing the
	// clients' side first is what lets Close return rather than wait on them.
	tb.Cleanup(func() {
		d.takeAwayEvery()
		d.hangUpTerminals()
		server.CloseClientConnections()
		server.Close()
	})
	d.URL = server.URL
	return d
}

func (d *Daemon) routes() http.Handler {
	mux := http.NewServeMux()
	// open is a route the contract asks no key for; handle one it does.
	open := func(pattern, operation string, serve http.HandlerFunc) {
		mux.HandleFunc(pattern, func(w http.ResponseWriter, r *http.Request) {
			d.mu.Lock()
			d.calls[operation]++
			var intercept Interceptor
			if queued := d.intercepts[operation]; len(queued) > 0 {
				intercept, d.intercepts[operation] = queued[0], queued[1:]
			} else {
				intercept = d.interceptAll[operation]
			}
			d.mu.Unlock()
			if intercept != nil {
				intercept(w, r, serve)
				return
			}
			serve(w, r)
		})
	}
	handle := func(pattern, operation string, serve http.HandlerFunc) {
		open(pattern, operation, d.authorized(serve))
	}
	handle("GET /v1/info", "getInfo", d.getInfo)
	handle("GET /v1/counts", "getCounts", d.getCounts)
	handle("GET /v1/host/commitments", "getHostCommitments", d.getHostCommitments)
	handle("GET /v1/sandboxes", "listSandboxes", d.listSandboxes)
	handle("POST /v1/sandboxes", "createSandbox", d.createSandbox)
	handle("GET /v1/sandboxes/{id}", "getSandbox", d.getSandbox)
	handle("DELETE /v1/sandboxes/{id}", "deleteSandbox", d.deleteSandbox)
	handle("GET /v1/sandboxes/{id}/events", "streamSandboxEvents", d.streamEvents)
	handle("GET /v1/sandboxes/{id}/console", "getSandboxConsole", d.console)
	handle("GET /v1/sandboxes/{id}/terminal", "openTerminal", d.openTerminal)
	handle("GET /v1/sandboxes/{id}/brokers", "listSandboxBrokers", d.listSandboxBrokers)
	handle("PUT /v1/sandboxes/{id}/brokers/{broker}", "attachSandboxBroker", d.attachSandboxBroker)
	handle("DELETE /v1/sandboxes/{id}/brokers/{broker}", "detachSandboxBroker", d.detachSandboxBroker)
	handle("GET /v1/sandboxes/{id}/brokers/{broker}/serve", "serveSandboxBroker", d.serveSandboxBroker)
	handle("POST /v1/sandboxes/{id}/commands", "runCommand", d.runCommand)
	handle("GET /v1/sandboxes/{id}/files", "readFile", d.readFile)
	handle("PUT /v1/sandboxes/{id}/files", "writeFile", d.writeFile)
	handle("GET /v1/sandboxes/{id}/egress", "getSandboxEgress", d.getEgress)
	handle("PUT /v1/sandboxes/{id}/egress", "setSandboxEgress", d.setEgress)
	handle("GET /v1/sandboxes/{id}/egress/rules", "listEgressRules", d.listRules)
	handle("GET /v1/sandboxes/{id}/egress/rules/{rule}", "getEgressRule", d.getRule)
	handle("PUT /v1/sandboxes/{id}/egress/rules/{rule}", "putEgressRule", d.putRule)
	handle("DELETE /v1/sandboxes/{id}/egress/rules/{rule}", "deleteEgressRule", d.deleteRule)
	handle("POST /v1/sandboxes/{id}/egress/rules/{rule}/pause", "pauseEgressRule", d.pausingRule(true))
	handle("POST /v1/sandboxes/{id}/egress/rules/{rule}/resume", "resumeEgressRule", d.pausingRule(false))
	handle("POST /v1/sandboxes/{id}/egress/connections/kill", "killConnections", d.killConnections)
	handle("GET /v1/tunnels", "listHostTunnels", d.listHostTunnels)
	handle("GET /v1/sandboxes/{id}/tunnels", "listTunnels", d.listTunnels)
	handle("GET /v1/sandboxes/{id}/tunnels/{tunnel}", "getTunnel", d.getTunnel)
	handle("PUT /v1/sandboxes/{id}/tunnels/{tunnel}", "putTunnel", d.putTunnel)
	handle("DELETE /v1/sandboxes/{id}/tunnels/{tunnel}", "deleteTunnel", d.deleteTunnel)
	handle("POST /v1/sandboxes/{id}/tunnels/{tunnel}/pause", "pauseTunnel", d.pausingTunnel(true))
	handle("POST /v1/sandboxes/{id}/tunnels/{tunnel}/resume", "resumeTunnel", d.pausingTunnel(false))
	handle("POST /v1/sandboxes/{id}/tunnels/{tunnel}/tokens", "createTunnelToken", d.createTunnelToken)
	handle("DELETE /v1/sandboxes/{id}/tunnels/{tunnel}/tokens/{token}", "revokeTunnelToken", d.revokeTunnelToken)
	handle("GET /v1/sandboxes/{id}/tunnels/{tunnel}/activity", "getTunnelActivity", d.tunnelActivity)
	handle("GET /v1/sandboxes/{id}/tunnels/{tunnel}/guests", "listTunnelGuests", d.listTunnelGuests)
	handle("DELETE /v1/sandboxes/{id}/tunnels/{tunnel}/guests/{email}", "revokeTunnelGuest", d.revokeTunnelGuest)
	handle("GET /v1/volumes", "listVolumes", d.listVolumes)
	handle("DELETE /v1/volumes/{name}", "deleteVolume", d.deleteVolume)
	handle("PUT /v1/volumes/{name}/labels", "setVolumeLabels", d.setVolumeLabels)
	open("GET /v1/auth", "getAuth", d.getAuth)
	handle("GET /v1/me", "getMe", d.getMe)
	open("POST /v1/keys/authorizations/redeem", "redeemKeyAuthorization", d.redeemKeyAuthorization)
	handle("DELETE /v1/keys/{key}", "revokeKey", d.revokeKey)
	return mux
}

func (d *Daemon) authorized(serve http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		authorization := r.Header.Get("Authorization")
		if authorization == "" && strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
			// A browser cannot put a key in the Authorization of a WebSocket,
			// so a handshake — and nothing else — may offer it as a
			// subprotocol.
			for _, protocol := range offered(r) {
				if key, ok := strings.CutPrefix(protocol, bearerProtocol); ok {
					authorization = "Bearer " + key
				}
			}
		}
		if d.key != "" && authorization != "Bearer "+d.key && !d.mintedKey(authorization) {
			refuse(w, http.StatusUnauthorized, genv1.ErrorCodeUnauthorized, "no key, or one this daemon will not accept")
			return
		}
		serve(w, r)
	}
}

// bearerProtocol is what a WebSocket handshake offers its key as: the prefix
// of a subprotocol, which is never the one echoed back.
const bearerProtocol = "runyard.bearer."

// offered is the subprotocols a handshake offers.
func offered(r *http.Request) []string {
	var protocols []string
	for _, header := range r.Header.Values("Sec-WebSocket-Protocol") {
		for protocol := range strings.SplitSeq(header, ",") {
			protocols = append(protocols, strings.TrimSpace(protocol))
		}
	}
	return protocols
}

// Intercept answers the next call of an operation, by operationId, in the
// daemon's place. Several queue up in order.
func (d *Daemon) Intercept(operation string, intercept Interceptor) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.intercepts[operation] = append(d.intercepts[operation], intercept)
}

// InterceptAll answers every call of an operation that no queued Intercept
// answers.
func (d *Daemon) InterceptAll(operation string, intercept Interceptor) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.interceptAll[operation] = intercept
}

// Refuse answers with the contract's error envelope.
func Refuse(status int, code genv1.ErrorCode, message string) Interceptor {
	return func(w http.ResponseWriter, _ *http.Request, _ http.HandlerFunc) {
		refuse(w, status, code, message)
	}
}

// Answer answers with exactly these bytes: what a proxy in front of a daemon
// says, say, which is not the contract's shape at all.
func Answer(status int, contentType, body string) Interceptor {
	return func(w http.ResponseWriter, _ *http.Request, _ http.HandlerFunc) {
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}
}

// HangUp closes the connection without an answer.
func HangUp() Interceptor {
	return func(w http.ResponseWriter, _ *http.Request, _ http.HandlerFunc) { hangUp(w) }
}

// HangUpAfter does what was asked and then closes the connection without
// saying so: the daemon that made the machine and lost the response.
func HangUpAfter() Interceptor {
	return func(w http.ResponseWriter, r *http.Request, serve http.HandlerFunc) {
		serve(httptest.NewRecorder(), r)
		hangUp(w)
	}
}

func hangUp(w http.ResponseWriter) {
	conn, _, err := http.NewResponseController(w).Hijack()
	if err != nil {
		panic(fmt.Sprintf("fakedaemon: hanging up: %v", err))
	}
	_ = conn.Close()
}

// Created receives each sandbox's id as the daemon makes it.
func (d *Daemon) Created() <-chan genv1.SandboxID { return d.created }

// Settle moves a sandbox to a state, as a real one moves when its machine
// boots or does not.
func (d *Daemon) Settle(id genv1.SandboxID, state genv1.SandboxState, message string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.sandboxes[id]; ok {
		d.settle(s, state, message)
	}
}

func (d *Daemon) settle(s *sandbox, state genv1.SandboxState, message string) {
	s.record.State = state
	if state != genv1.SandboxStateReady {
		// A broker a caller serves is a running machine's: stopped or
		// paused, the caller is hung up on and the broker goes.
		d.takeAwayAll(s)
	}
	if state == genv1.SandboxStateFailed && message != "" {
		s.record.Error = &message
	}
	if state == genv1.SandboxStateReady {
		now := d.now().UTC()
		s.record.ReadyAt = &now
	}
	d.publish(s, genv1.SandboxEvent{Type: genv1.SandboxEventTypeState, State: &state})
	if message != "" {
		d.publish(s, genv1.SandboxEvent{Type: genv1.SandboxEventTypeError, Message: &message})
	}
}

func (d *Daemon) publish(s *sandbox, event genv1.SandboxEvent) {
	event.Seq = int64(len(s.events) + 1)
	event.At = d.now().UTC()
	s.events = append(s.events, event)
	close(s.changed)
	s.changed = make(chan struct{})
}

// Calls is how often an operation was asked for, intercepted or not.
func (d *Daemon) Calls(operation string) int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.calls[operation]
}

// IdempotencyKeys is the key each create carried, in order.
func (d *Daemon) IdempotencyKeys() []string {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]string(nil), d.keys...)
}

// Deletes is every sandbox DELETE the daemon carried out, in order.
func (d *Daemon) Deletes() []Delete {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]Delete(nil), d.deletes...)
}

// Commands is every command a sandbox was asked to run, in order.
func (d *Daemon) Commands() []genv1.CommandRequest {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]genv1.CommandRequest(nil), d.commands...)
}

// Sandboxes is every sandbox on the host now.
func (d *Daemon) Sandboxes() []genv1.Sandbox {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := []genv1.Sandbox{}
	for _, id := range d.order {
		if s, ok := d.sandboxes[id]; ok {
			out = append(out, s.record)
		}
	}
	return out
}

// File is what a sandbox holds at a path, and whether it holds anything.
func (d *Daemon) File(id genv1.SandboxID, path string) ([]byte, string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sandboxes[id]
	if !ok {
		return nil, "", false
	}
	body, ok := s.files[path]
	return body, s.modes[path], ok
}

// Volume says whether a volume exists, and which sandbox holds it.
func (d *Daemon) Volume(name string) (exists bool, holder *genv1.SandboxID) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.volumes[name]
	if !ok {
		return false, nil
	}
	return true, v.holder
}

// PutVolume makes a volume, as an earlier run would have left it.
func (d *Daemon) PutVolume(name string, bytes int64) {
	d.PutVolumeWith(name, bytes, nil, nil, time.Time{})
}

// PutVolumeWith makes a volume, as an earlier run would have left it: with
// these labels, made by this caller — nobody, when nil — and last used then,
// or now when that is zero.
func (d *Daemon) PutVolumeWith(name string, bytes int64, labels map[string]string, by *genv1.Creator, used time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now().UTC()
	if used.IsZero() {
		used = now
	}
	d.volumes[name] = &volume{createdAt: now, lastUsedAt: used.UTC(), bytes: bytes, labels: maps.Clone(labels), createdBy: by}
}

// VolumeLabels are a volume's labels, and whether there is one.
func (d *Daemon) VolumeLabels(name string) (map[string]string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.volumes[name]
	if !ok {
		return nil, false
	}
	return maps.Clone(v.labels), true
}

func (d *Daemon) getInfo(w http.ResponseWriter, _ *http.Request) {
	var info genv1.HostInfo
	info.Id = "fake-host"
	info.Instance = "fake"
	info.Build = "runyard-sandboxes (fake)"
	info.Protocol = 1
	info.Hypervisor.Name = "cloud-hypervisor"
	info.Hypervisor.Version = "cloud-hypervisor v53.0"
	if len(d.brokers) > 0 {
		brokers := append([]genv1.Broker(nil), d.brokers...)
		info.Brokers = &brokers
	}
	info.Network = &genv1.HostNetwork{Enabled: d.network, Forwarding: d.network, Warnings: []string{}}
	reply(w, http.StatusOK, info)
}

func (d *Daemon) listSandboxes(w http.ResponseWriter, _ *http.Request) {
	items := d.Sandboxes()
	reply(w, http.StatusOK, genv1.SandboxPage{Items: items, Total: len(items)})
}

func (d *Daemon) createSandbox(w http.ResponseWriter, r *http.Request) {
	key := r.Header.Get("Idempotency-Key")
	body, _ := io.ReadAll(r.Body)
	var spec genv1.SandboxSpec
	if err := json.Unmarshal(body, &spec); err != nil || spec.Image == "" {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "the spec needs an image")
		return
	}
	if key == "" {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "Idempotency-Key is required")
		return
	}
	canonical, err := json.Marshal(spec)
	if err != nil {
		refuse(w, http.StatusInternalServerError, genv1.ErrorCodeInternal, "re-encoding the spec: "+err.Error())
		return
	}

	d.mu.Lock()
	defer d.mu.Unlock()
	d.keys = append(d.keys, key)
	if earlier, ok := d.idempotent[key]; ok {
		s, alive := d.sandboxes[earlier.id]
		switch {
		case !bytes.Equal(earlier.spec, canonical):
			refuse(w, http.StatusConflict, genv1.ErrorCodeSpecConflict, "this key made a sandbox with a different spec")
		case !alive:
			refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "this key made a sandbox that has since been dropped")
		default:
			reply(w, http.StatusOK, s.record)
		}
		return
	}

	id := uuid.Must(uuid.NewV7())
	record := genv1.Sandbox{Id: id, Spec: spec, State: genv1.SandboxStateCreating, CreatedAt: d.now().UTC(), Labels: spec.Labels}
	if spec.Brokers != nil {
		attached := []genv1.SandboxBroker{}
		for i, wanted := range *spec.Brokers {
			broker, wrong, unknown := d.brokerOf(wanted.Name, wanted.Upstream, firstBrokerPort+i)
			switch {
			case unknown:
				refuse(w, http.StatusUnprocessableEntity, genv1.ErrorCodeUnsupportedSpec, wrong)
				return
			case wrong != "":
				refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, wrong)
				return
			}
			attached = append(attached, broker)
		}
		record.Brokers = &attached
	}
	// Every sandbox's disk is a volume: one the spec did not name is given a
	// name, and the spec answered says which.
	if spec.Disk == nil || spec.Disk.Volume == nil {
		disk := genv1.DiskSpec{}
		if spec.Disk != nil {
			disk = *spec.Disk
		}
		name := "vol_" + strings.ReplaceAll(id.String(), "-", "")[16:]
		disk.Volume = &name
		record.Spec.Disk = &disk
	}
	volumeName := *record.Spec.Disk.Volume
	v, ok := d.volumes[volumeName]
	if ok && v.holder != nil {
		refuse(w, http.StatusConflict, genv1.ErrorCodeVolumeInUse, fmt.Sprintf("volume %s is held by sandbox %s", volumeName, *v.holder))
		return
	}
	if !ok {
		v = &volume{createdAt: d.now().UTC(), createdBy: d.creatorOf(r)}
		if record.Spec.Disk.Labels != nil {
			v.labels = maps.Clone(*record.Spec.Disk.Labels)
		}
		d.volumes[volumeName] = v
	}
	v.lastUsedAt = d.now().UTC()
	// The size the guest sees is the last sandbox's to say.
	if record.Spec.Disk.Size != nil {
		v.bytes = sizeOf(*record.Spec.Disk.Size)
	}
	holder := id
	v.holder = &holder
	record.Identity = &genv1.Identity{
		Uri:         "runyard-sandbox://fake-host/" + id.String(),
		Subject:     "CN=" + id.String(),
		Fingerprint: "sha256:00",
		NotAfter:    d.now().Add(24 * time.Hour).UTC(),
	}
	s := &sandbox{record: record, callers: map[string]*caller{}, files: map[string][]byte{}, modes: map[string]string{}, changed: make(chan struct{})}
	d.sandboxes[id] = s
	d.order = append(d.order, id)
	d.idempotent[key] = idempotent{id: id, spec: canonical}
	creating := genv1.SandboxStateCreating
	d.publish(s, genv1.SandboxEvent{Type: genv1.SandboxEventTypeState, State: &creating})

	// Answered as it was when the create was accepted, and settled after:
	// the caller sees `creating` in the 202 the way it does from a real one.
	accepted := s.record
	outcome := Outcome{}
	if d.boot != nil {
		outcome = d.boot(spec)
	}
	s.console = outcome.Console
	for _, message := range outcome.Progress {
		d.publish(s, genv1.SandboxEvent{Type: genv1.SandboxEventTypeProgress, Message: &message})
	}
	switch outcome.State {
	case genv1.SandboxStateCreating:
	case "":
		d.settle(s, genv1.SandboxStateReady, "")
	default:
		d.settle(s, outcome.State, outcome.Error)
	}
	reply(w, http.StatusAccepted, accepted)
	d.created <- id
}

// lookup is the sandbox a path names, or an answer saying there is none.
func (d *Daemon) lookup(w http.ResponseWriter, r *http.Request) (*sandbox, bool) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "a sandbox id is a UUID")
		return nil, false
	}
	s, ok := d.sandboxes[id]
	if !ok {
		refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, "no sandbox "+id.String())
		return nil, false
	}
	return s, true
}

func (d *Daemon) getSandbox(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.lookup(w, r); ok {
		reply(w, http.StatusOK, s.record)
	}
}

func (d *Daemon) deleteSandbox(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.lookup(w, r)
	if !ok {
		return
	}
	// Whoever serves a broker of its own in it is hung up on.
	d.takeAwayAll(s)
	disk := r.URL.Query().Get("disk")
	if disk == "" {
		disk = "keep"
	}
	id := s.record.Id
	d.deletes = append(d.deletes, Delete{ID: id, Disk: disk})
	if s.record.Spec.Disk != nil && s.record.Spec.Disk.Volume != nil {
		name := *s.record.Spec.Disk.Volume
		if v, ok := d.volumes[name]; ok && v.holder != nil && *v.holder == id {
			v.holder = nil
			v.lastUsedAt = d.now().UTC()
			v.bytes += 7_500_000
			if disk == "delete" {
				delete(d.volumes, name)
			}
		}
	}
	delete(d.sandboxes, id)
	// The stream of a sandbox that is gone ends, the way a followed stream
	// ends "when the thing being read ends".
	close(s.changed)
	s.changed = nil
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) streamEvents(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	s, ok := d.lookup(w, r)
	d.mu.Unlock()
	if !ok {
		return
	}
	follow := r.URL.Query().Get("follow") == "true"
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(http.StatusOK)
	sent := 0
	for {
		d.mu.Lock()
		pending := append([]genv1.SandboxEvent(nil), s.events[sent:]...)
		changed := s.changed
		d.mu.Unlock()
		for _, event := range pending {
			line, err := json.Marshal(event)
			if err != nil {
				// The status is sent: ending the stream short is all that is left.
				return
			}
			_, _ = w.Write(append(line, '\n'))
		}
		sent += len(pending)
		_ = http.NewResponseController(w).Flush()
		if !follow || changed == nil {
			return
		}
		select {
		case <-changed:
		case <-r.Context().Done():
			return
		}
	}
}

func (d *Daemon) console(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.lookup(w, r); ok {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = io.WriteString(w, s.console)
	}
}

func (d *Daemon) runCommand(w http.ResponseWriter, r *http.Request) {
	var request genv1.CommandRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || len(request.Argv) == 0 {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "argv is required")
		return
	}
	d.mu.Lock()
	s, ok := d.lookup(w, r)
	if !ok {
		d.mu.Unlock()
		return
	}
	id, state := s.record.Id, s.record.State
	d.commands = append(d.commands, request)
	d.mu.Unlock()
	if state != genv1.SandboxStateReady {
		refuse(w, http.StatusServiceUnavailable, genv1.ErrorCodeSandboxUnreachable, "the sandbox is "+string(state))
		return
	}
	result := genv1.CommandResult{}
	if d.run != nil {
		result = d.run(r.Context(), id, request)
	}
	reply(w, http.StatusOK, result)
}

func (d *Daemon) readFile(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.lookup(w, r)
	if !ok {
		return
	}
	body, ok := s.files[r.URL.Query().Get("path")]
	if !ok {
		refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, "no such file")
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	_, _ = w.Write(body)
}

func (d *Daemon) writeFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if !strings.HasPrefix(path, "/") {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "path must be absolute")
		return
	}
	body, _ := io.ReadAll(r.Body)
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.lookup(w, r)
	if !ok {
		return
	}
	s.files[path] = body
	s.modes[path] = r.URL.Query().Get("mode")
	w.WriteHeader(http.StatusNoContent)
}

// getCounts counts what the double holds. It has one key, which may read
// everything, and no image cache: its images are none.
func (d *Daemon) getCounts(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	count := func(n int) *int { return &n }
	tunnels := 0
	for _, s := range d.sandboxes {
		tunnels += len(s.tunnels)
	}
	reply(w, http.StatusOK, genv1.HostCounts{
		Sandboxes: count(len(d.sandboxes)), Tunnels: count(tunnels), Volumes: count(len(d.volumes)),
		Images: count(0), Brokers: count(len(d.brokers)),
	})
}

// getHostCommitments adds up the specs the double's running sandboxes were
// made with, and the sizes of its volumes, as the daemon does. The double has
// no defaults, so a size a spec left out is none.
func (d *Daemon) getHostCommitments(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	var out genv1.HostCommitments
	for _, v := range d.volumes {
		out.Volumes++
		out.DiskBytes += v.bytes
	}
	for _, s := range d.sandboxes {
		if !isRunning(s.record.State) {
			continue
		}
		spec := s.record.Spec
		out.Running++
		if spec.Cpus != nil {
			out.Cpus += *spec.Cpus
		}
		if spec.Memory != nil {
			out.MemoryBytes += sizeOf(*spec.Memory)
		}
	}
	reply(w, http.StatusOK, out)
}

// isRunning is whether a sandbox in state is running, as the daemon counts
// it: a paused one is, a stopped one is not.
func isRunning(state genv1.SandboxState) bool {
	switch state {
	case genv1.SandboxStateCreating, genv1.SandboxStateBooting, genv1.SandboxStateReady,
		genv1.SandboxStateUnreachable, genv1.SandboxStatePaused:
		return true
	case genv1.SandboxStateStopped, genv1.SandboxStateFailed, genv1.SandboxStateGone:
		return false
	}
	return false
}

// sizeOf reads the binary sizes a test writes a spec with — `512Mi`, `2Gi` —
// and anything else as nothing.
func sizeOf(size string) int64 {
	for _, unit := range []struct {
		suffix string
		scale  int64
	}{{"Ti", 1 << 40}, {"Gi", 1 << 30}, {"Mi", 1 << 20}, {"Ki", 1 << 10}} {
		if number, ok := strings.CutSuffix(size, unit.suffix); ok {
			n, err := strconv.ParseInt(number, 10, 64)
			if err != nil {
				return 0
			}
			return n * unit.scale
		}
	}
	return 0
}

func (d *Daemon) listVolumes(w http.ResponseWriter, r *http.Request) {
	wanted := map[string]string{}
	for _, pair := range r.URL.Query()["label"] {
		key, value, ok := strings.Cut(pair, "=")
		if !ok {
			refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "a label filter is written `key=value`")
			return
		}
		wanted[key] = value
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	names := make([]string, 0, len(d.volumes))
	for name := range d.volumes {
		names = append(names, name)
	}
	sort.Strings(names)
	list := genv1.VolumeList{Items: []genv1.Volume{}}
	for _, name := range names {
		v := d.volumes[name]
		labels := map[string]string{}
		maps.Copy(labels, v.labels)
		if !carries(labels, wanted) {
			continue
		}
		list.Items = append(list.Items, genv1.Volume{
			Name: name, CreatedAt: v.createdAt, LastUsedAt: v.lastUsedAt, SandboxId: v.holder, Labels: labels, CreatedBy: v.createdBy,
			Bytes: v.bytes, AllocatedBytes: v.bytes, Digest: "sha256:" + strings.Repeat("0", 64),
		})
	}
	reply(w, http.StatusOK, list)
}

// carries says whether labels has every one wanted.
func carries(labels, wanted map[string]string) bool {
	for key, value := range wanted {
		if have, ok := labels[key]; !ok || have != value {
			return false
		}
	}
	return true
}

func (d *Daemon) setVolumeLabels(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Labels *map[string]string `json:"labels"`
	}
	if err := strictly(r, &body); err != nil || body.Labels == nil {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "`labels` is required; `{\"labels\":{}}` is what clears them")
		return
	}
	name := r.PathValue("name")
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.volumes[name]
	if !ok {
		refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, "no volume "+name)
		return
	}
	v.labels = maps.Clone(*body.Labels)
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) deleteVolume(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	d.mu.Lock()
	defer d.mu.Unlock()
	if v, ok := d.volumes[name]; ok {
		if v.holder != nil {
			refuse(w, http.StatusConflict, genv1.ErrorCodeVolumeInUse, fmt.Sprintf("volume %s is held by sandbox %s", name, *v.holder))
			return
		}
		delete(d.volumes, name)
	}
	w.WriteHeader(http.StatusNoContent)
}

func reply(w http.ResponseWriter, status int, body any) {
	encoded, err := json.Marshal(body)
	if err != nil {
		// Encoded before the status is sent, so a body the double cannot
		// encode is a 500 that says so rather than a 200 cut short.
		http.Error(w, "fakedaemon: encoding the reply: "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(append(encoded, '\n'))
}

func refuse(w http.ResponseWriter, status int, code genv1.ErrorCode, message string) {
	var envelope genv1.Error
	envelope.Error.Code = code
	envelope.Error.Message = message
	reply(w, status, envelope)
}

// The egress plane: rules kept in the sandbox's spec, as the daemon keeps
// them, and a report whose refusals are what a test recorded — there is no
// firewall here to refuse anything.

// ruleName is what an egress rule may be called.
var ruleName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Rules is a sandbox's egress rules now.
func (d *Daemon) Rules(id genv1.SandboxID) []genv1.EgressRule {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sandboxes[id]
	if !ok || s.record.Spec.Egress == nil || s.record.Spec.Egress.Rules == nil {
		return nil
	}
	return append([]genv1.EgressRule(nil), *s.record.Spec.Egress.Rules...)
}

// RecordRefusal makes a sandbox's egress report say it was refused this, as a
// real sandbox's firewall or resolver would. A test's command double calls it
// when the command it answers is one the sandbox's rules would not let out.
func (d *Daemon) RecordRefusal(id genv1.SandboxID, refusal genv1.EgressRefusal) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.sandboxes[id]; ok {
		s.refused = append(s.refused, refusal)
	}
}

// OpenConnection makes a sandbox's egress report list this connection as
// open, as the kernel would, until it is killed.
func (d *Daemon) OpenConnection(id genv1.SandboxID, c genv1.Connection) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.sandboxes[id]; ok {
		s.open = append(s.open, c)
	}
}

// killConnections kills what matches everything the body names, as the
// daemon does, and answers with what it killed. The body is checked as far
// as the fake needs: one end is named, and nothing it does not know.
func (d *Daemon) killConnections(w http.ResponseWriter, r *http.Request) {
	var body genv1.KillConnections
	if err := strictly(r, &body); err != nil {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "the body is not a kill: "+err.Error())
		return
	}
	destination, source, protocol := valueOf(body.Destination), valueOf(body.Source), valueOf(body.Protocol)
	if destination == "" && source == "" {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "a destination or a source: killing every connection is asked for by naming them")
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.lookup(w, r)
	if !ok {
		return
	}
	killed := []genv1.Connection{}
	kept := s.open[:0:0]
	for _, c := range s.open {
		address, _, _ := strings.Cut(c.Destination, ":")
		matches := (destination == "" || destination == c.Destination || destination == address) &&
			(source == "" || source == c.Source) && (protocol == "" || protocol == c.Protocol)
		if matches {
			killed = append(killed, c)
		} else {
			kept = append(kept, c)
		}
	}
	s.open = kept
	reply(w, http.StatusOK, map[string]any{"killed": killed})
}

func valueOf(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func rulesOf(s *sandbox) []genv1.EgressRule {
	if s.record.Spec.Egress == nil || s.record.Spec.Egress.Rules == nil {
		return []genv1.EgressRule{}
	}
	return *s.record.Spec.Egress.Rules
}

func (d *Daemon) getEgress(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.lookup(w, r)
	if !ok {
		return
	}
	refusals := make([]genv1.EgressRefusal, 0, len(s.refused))
	for _, refusal := range slices.Backward(s.refused) {
		refusals = append(refusals, refusal)
	}
	rules := rulesOf(s)
	reply(w, http.StatusOK, genv1.EgressStatus{
		Policy:      genv1.EgressSpec{Rules: &rules},
		Interface:   genv1.Interface{Namespace: "runyard-fake-" + s.record.Id.String(), Tap: "tap0", Veth: "ryfake-1", Guest: "172.30.0.5", Host: "172.30.0.4", Mask: 30},
		Resolved:    []genv1.ResolvedName{},
		Rules:       []genv1.FirewallRule{},
		Connections: append([]genv1.Connection{}, s.open...),
		Refusals:    refusals,
	})
}

// changeable is the sandbox a path names, if its rules can change now: the
// daemon changes them on a running interface.
func (d *Daemon) changeable(w http.ResponseWriter, r *http.Request) (*sandbox, bool) {
	s, ok := d.lookup(w, r)
	if !ok {
		return nil, false
	}
	if s.record.State != genv1.SandboxStateReady {
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "it is "+string(s.record.State)+": a policy changes on a running interface")
		return nil, false
	}
	return s, true
}

// strictly reads a body the way the daemon reads an egress one: a field it
// does not have is a refusal.
func strictly(r *http.Request, into any) error {
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	return decoder.Decode(into)
}

// checkRule says what the daemon would refuse a rule for, of what a double
// can tell.
func checkRule(name string, spec genv1.EgressRuleSpec) string {
	if !ruleName.MatchString(name) {
		return fmt.Sprintf("%q is not a rule name", name)
	}
	if (spec.Domains == nil || len(*spec.Domains) == 0) && (spec.Cidrs == nil || len(*spec.Cidrs) == 0) && (spec.Internet == nil || !*spec.Internet) {
		return fmt.Sprintf("rule %s names no domains, no cidrs and not the internet, so it opens nothing", name)
	}
	return ""
}

func (d *Daemon) setEgress(w http.ResponseWriter, r *http.Request) {
	var spec genv1.EgressSpec
	if err := strictly(r, &spec); err != nil {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "the body is not an egress policy: "+err.Error())
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.changeable(w, r)
	if !ok {
		return
	}
	rules := []genv1.EgressRule{}
	if spec.Rules != nil {
		rules = *spec.Rules
	}
	seen := map[string]bool{}
	for _, rule := range rules {
		wrong := checkRule(rule.Name, genv1.EgressRuleSpec{Domains: rule.Domains, Cidrs: rule.Cidrs, Internet: rule.Internet})
		if wrong == "" && seen[rule.Name] {
			wrong = fmt.Sprintf("two rules are called %q", rule.Name)
		}
		if wrong != "" {
			refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "egress: "+wrong)
			return
		}
		seen[rule.Name] = true
	}
	s.record.Spec.Egress = &genv1.EgressSpec{Rules: &rules}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) listRules(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.lookup(w, r); ok {
		reply(w, http.StatusOK, map[string][]genv1.EgressRule{"items": rulesOf(s)})
	}
}

func (d *Daemon) getRule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("rule")
	if !ruleName.MatchString(name) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, fmt.Sprintf("%q is not a rule name", name))
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.lookup(w, r)
	if !ok {
		return
	}
	for _, rule := range rulesOf(s) {
		if rule.Name == name {
			reply(w, http.StatusOK, rule)
			return
		}
	}
	refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, fmt.Sprintf("this sandbox has no egress rule %q", name))
}

func (d *Daemon) putRule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("rule")
	var spec genv1.EgressRuleSpec
	if err := strictly(r, &spec); err != nil {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "the body is not an egress rule: "+err.Error())
		return
	}
	if wrong := checkRule(name, spec); wrong != "" {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "egress: "+wrong)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.changeable(w, r)
	if !ok {
		return
	}
	rule := genv1.EgressRule{Name: name, Domains: spec.Domains, Cidrs: spec.Cidrs, Internet: spec.Internet, Ports: spec.Ports, PinTtl: spec.PinTtl, AllowPrivate: spec.AllowPrivate}
	if spec.Paused != nil && *spec.Paused {
		rule.Paused = new(true)
	}
	rules := append([]genv1.EgressRule(nil), rulesOf(s)...)
	status := http.StatusCreated
	for i := range rules {
		if rules[i].Name == name {
			rules[i], status = rule, http.StatusOK
		}
	}
	if status == http.StatusCreated {
		rules = append(rules, rule)
	}
	s.record.Spec.Egress = &genv1.EgressSpec{Rules: &rules}
	reply(w, status, rule)
}

// pausingRule pauses a rule, or resumes it, as the daemon does: kept where it
// is, answered as it is now, and answered the same when it already was.
func (d *Daemon) pausingRule(paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("rule")
		if !ruleName.MatchString(name) {
			refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, fmt.Sprintf("%q is not a rule name", name))
			return
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		s, ok := d.changeable(w, r)
		if !ok {
			return
		}
		rules := append([]genv1.EgressRule(nil), rulesOf(s)...)
		for i := range rules {
			if rules[i].Name != name {
				continue
			}
			rules[i].Paused = nil
			if paused {
				rules[i].Paused = new(true)
			}
			s.record.Spec.Egress = &genv1.EgressSpec{Rules: &rules}
			reply(w, http.StatusOK, rules[i])
			return
		}
		refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, fmt.Sprintf("this sandbox has no egress rule %q", name))
	}
}

func (d *Daemon) deleteRule(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("rule")
	if !ruleName.MatchString(name) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, fmt.Sprintf("%q is not a rule name", name))
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.changeable(w, r)
	if !ok {
		return
	}
	rules := rulesOf(s)
	kept := make([]genv1.EgressRule, 0, len(rules))
	for _, rule := range rules {
		if rule.Name != name {
			kept = append(kept, rule)
		}
	}
	if len(kept) == len(rules) {
		refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, fmt.Sprintf("this sandbox has no egress rule %q", name))
		return
	}
	s.record.Spec.Egress = &genv1.EgressSpec{Rules: &kept}
	w.WriteHeader(http.StatusNoContent)
}
