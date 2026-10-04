package fakedaemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/hashicorp/yamux"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// The broker plane, as much of it as a caller of the API sees: the brokers a
// sandbox has, given and taken away while it runs, and a client broker, which
// is a caller's connection and lasts as long as it does. Nothing is dialled
// here — a shared or a one-off broker is a name and a port — and there is no
// workload to connect to a client broker: a test is the workload, and
// DialBroker is it connecting.

// brokerName is what a broker may be called, as the contract has it.
var brokerName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

const notABrokerName = "a broker name is lower-case letters, digits and dashes, starting and ending with a letter or digit"

const (
	brokerProtocol = "runyard.broker.v1"
	// brokerPortHeader is where the answer to a serve says the port the
	// broker is on inside the sandbox.
	brokerPortHeader = "Runyard-Broker-Port"
)

// firstBrokerPort is the port the first broker of a sandbox is on; each one
// after it is on the next nothing of the sandbox's has.
const firstBrokerPort = 4100

// brokerKeepAlive is how often a caller serving a broker is asked whether it
// is still there, as the daemon asks unless it is told otherwise.
const brokerKeepAlive = 5 * time.Second

// WithBrokerKeepAlive is how often a caller serving a broker is asked whether
// it is still there, and it is given twice that to answer: by the keepalives
// of its session, and when a second caller asks for its broker's name. It is
// the daemon's `brokers.keepAlive`, for a test about a caller that has gone
// silent which does not want to wait a quarter of a minute to see it noticed.
func WithBrokerKeepAlive(every time.Duration) Option {
	return func(d *Daemon) { d.brokerKeepAlive = every }
}

// caller is whoever holds a client broker: the broker is theirs from the
// moment their handshake is let in, before it is answered, and gone when
// they are.
type caller struct {
	// session is nil until the WebSocket is one.
	session *yamux.Session
	// left is done once the broker has been taken from this caller, and leave
	// makes it so: the broker detached, the sandbox stopped or deleted, the
	// daemon closed, or the name another caller's once this one did not
	// answer for it. Its handler hangs up when it is.
	left  context.Context
	leave context.CancelFunc
}

// newCaller is the caller whose handshake r is.
func newCaller(r *http.Request) *caller {
	// Not ended with the request: the connection stops being the server's
	// when it becomes a WebSocket.
	left, leave := context.WithCancel(context.WithoutCancel(r.Context()))
	return &caller{left: left, leave: leave}
}

// brokersOf is the brokers a sandbox has, never nil.
func brokersOf(s *sandbox) []genv1.SandboxBroker {
	if s.record.Brokers == nil {
		return []genv1.SandboxBroker{}
	}
	return *s.record.Brokers
}

// has is where a sandbox's broker of a name is among its brokers, or -1.
func has(s *sandbox, name string) int {
	return slices.IndexFunc(brokersOf(s), func(b genv1.SandboxBroker) bool { return b.Name == name })
}

// nextPort is a port nothing of the sandbox's has.
func nextPort(s *sandbox) int {
	port := firstBrokerPort
	for _, broker := range brokersOf(s) {
		port = max(port, broker.Port+1)
	}
	return port
}

// brokerOf is a spec's broker as the sandbox has it, on a port, or what is
// wrong with it. unknown is a shared broker this host does not declare, which
// is refused as a spec this host cannot give rather than as one that is wrong.
func (d *Daemon) brokerOf(name string, upstream *genv1.BrokerUpstream, port int) (broker genv1.SandboxBroker, wrong string, unknown bool) {
	broker = genv1.SandboxBroker{Name: name, Port: port}
	switch {
	case !brokerName.MatchString(name):
		return broker, fmt.Sprintf("%q: %s", name, notABrokerName), false
	case upstream != nil:
		broker.Kind, broker.Url, broker.Description = genv1.BrokerKindOneOff, &upstream.Url, upstream.Description
	default:
		at := slices.IndexFunc(d.brokers, func(b genv1.Broker) bool { return b.Name == name })
		if at < 0 {
			return broker, fmt.Sprintf("this host declares no broker %q", name), true
		}
		broker.Kind, broker.Url = genv1.BrokerKindShared, &d.brokers[at].Url
	}
	return broker, "", false
}

// setBrokers replaces a sandbox's brokers, in its record and in its spec,
// which has none of those a caller holds: they are in no spec. A new slice
// each time: an answer already given holds the old one.
func setBrokers(s *sandbox, brokers []genv1.SandboxBroker) {
	s.record.Brokers = &brokers
	spec := []genv1.SandboxBrokerSpec{}
	for _, broker := range brokers {
		switch broker.Kind {
		case genv1.BrokerKindOneOff:
			spec = append(spec, genv1.SandboxBrokerSpec{Name: broker.Name, Upstream: &genv1.BrokerUpstream{Url: valueOf(broker.Url), Description: broker.Description}})
		case genv1.BrokerKindShared:
			spec = append(spec, genv1.SandboxBrokerSpec{Name: broker.Name})
		case genv1.BrokerKindClient:
		}
	}
	// A spec that names none, and never did, goes on not mentioning them.
	if len(spec) > 0 || s.record.Spec.Brokers != nil {
		s.record.Spec.Brokers = &spec
	}
}

// takeAway takes a client broker from the sandbox, and from whoever held it,
// if anybody did. Their handler is what hangs up, and says so to them: with
// the lock held here, nothing waits for a caller to answer.
func (d *Daemon) takeAway(s *sandbox, name string) {
	holder, ok := s.callers[name]
	if !ok {
		return
	}
	delete(s.callers, name)
	setBrokers(s, slices.DeleteFunc(slices.Clone(brokersOf(s)), func(b genv1.SandboxBroker) bool { return b.Name == name }))
	holder.leave()
	d.brokersChanged()
}

// takeAwayAll takes every client broker a sandbox has: it is being stopped,
// paused or deleted, and a client broker is a running machine's.
func (d *Daemon) takeAwayAll(s *sandbox) {
	for name := range s.callers {
		d.takeAway(s, name)
	}
}

func (d *Daemon) listSandboxBrokers(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.lookup(w, r); ok {
		reply(w, http.StatusOK, genv1.SandboxBrokerPage{Items: brokersOf(s)})
	}
}

// brokersChangeable is whether a sandbox's brokers can change now, and the
// refusal when they cannot: not while its machine is being made from its spec
// as it was, nor while it is paused and its agent cannot be told.
func brokersChangeable(w http.ResponseWriter, s *sandbox) bool {
	switch s.record.State {
	case genv1.SandboxStateCreating, genv1.SandboxStateBooting, genv1.SandboxStatePaused:
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "it is "+string(s.record.State)+": its brokers change once it is ready")
		return false
	case genv1.SandboxStateReady, genv1.SandboxStateStopped, genv1.SandboxStateFailed, genv1.SandboxStateUnreachable, genv1.SandboxStateGone:
	}
	return true
}

// attachSandboxBroker gives a sandbox a broker, or replaces the one it has by
// that name, which keeps its port. A name a caller is serving is that
// caller's, and is refused.
//
// What is wrong with what was asked is said before anything about the
// sandbox, as the daemon says it: a name that is not one before whether the
// sandbox exists, and a broker that cannot be given before whether the
// sandbox can be changed now.
func (d *Daemon) attachSandboxBroker(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("broker")
	if !brokerName.MatchString(name) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, notABrokerName)
		return
	}
	// Not strictly: the daemon reads what it knows of a body and ignores the
	// rest, so one with a field misspelt is an empty one — the shared broker.
	var body genv1.SandboxBrokerAttach
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "the body is not a broker to attach: "+err.Error())
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.lookup(w, r)
	if !ok {
		return
	}
	brokers := slices.Clone(brokersOf(s))
	at, port := has(s, name), nextPort(s)
	if at >= 0 {
		port = brokers[at].Port
	}
	broker, wrong, unknown := d.brokerOf(name, body.Upstream, port)
	switch {
	case unknown:
		refuse(w, http.StatusUnprocessableEntity, genv1.ErrorCodeUnsupportedSpec, wrong)
		return
	case wrong != "":
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, wrong)
		return
	case s.callers[name] != nil:
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, fmt.Sprintf("broker %s is one a caller is serving, and is theirs while they do", name))
		return
	case !brokersChangeable(w, s):
		return
	}
	if at >= 0 {
		brokers[at] = broker
	} else {
		brokers = append(brokers, broker)
	}
	setBrokers(s, brokers)
	reply(w, http.StatusOK, broker)
}

// detachSandboxBroker takes a broker away, and hangs up on the caller serving
// it when it is one a caller serves. One the sandbox does not have is
// detached all the same, whatever state the sandbox is in: there is nothing
// to change.
func (d *Daemon) detachSandboxBroker(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("broker")
	if !brokerName.MatchString(name) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, notABrokerName)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.lookup(w, r)
	if !ok {
		return
	}
	if has(s, name) >= 0 {
		if !brokersChangeable(w, s) {
			return
		}
		d.takeAway(s, name)
		setBrokers(s, slices.DeleteFunc(slices.Clone(brokersOf(s)), func(b genv1.SandboxBroker) bool { return b.Name == name }))
	}
	w.WriteHeader(http.StatusNoContent)
}

// serveSandboxBroker gives the sandbox a broker of the name asked for and
// makes the caller its far end, for as long as its WebSocket lasts: when
// that ends, the broker is gone. Everything that can refuse does so before
// the upgrade, as an answer the caller can read.
func (d *Daemon) serveSandboxBroker(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("broker")
	if !brokerName.MatchString(name) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, notABrokerName)
		return
	}
	s, holder, port, ok := d.give(w, r, name)
	if !ok {
		return
	}
	defer d.serving.Done()
	// However it ends, the broker goes with its caller — unless it was taken
	// from them first, and the name is by now somebody else's, or nothing's.
	leave := func() {
		d.mu.Lock()
		defer d.mu.Unlock()
		if s.callers[name] == holder {
			d.takeAway(s, name)
		}
	}
	defer leave()

	// Said before the handshake is answered: the port is open by then, and
	// the first connection made to it is carried.
	w.Header().Set(brokerPortHeader, strconv.Itoa(port))
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{brokerProtocol}})
	if err != nil {
		return // Accept has answered
	}
	// This end opens the streams, and the caller accepts them: the daemon's
	// session, with the daemon's times. Its keepalives are how a caller that
	// went silent is found out, and a stream nobody accepts for 75 seconds —
	// yamux's own time for one — ends the serve. The error is the
	// configuration's, and this one cannot be wrong.
	config := yamux.DefaultConfig()
	config.KeepAliveInterval = d.brokerKeepAlive
	config.ConnectionWriteTimeout = 2 * d.brokerKeepAlive
	config.LogOutput = io.Discard
	session, _ := yamux.Client(goingAway{
		// Not the request's context: the connection stopped being the
		// server's when it became a WebSocket, and what ends it is one end
		// hanging up.
		Conn:   websocket.NetConn(context.WithoutCancel(r.Context()), conn, websocket.MessageBinary),
		socket: conn,
	}, config)
	// Hangs up saying this end is going away, when nothing was said before
	// it: the caller reads that its serving was ended, not that the line
	// broke.
	defer func() { _ = session.Close() }()

	d.mu.Lock()
	holder.session = session
	d.brokersChanged()
	d.mu.Unlock()
	// Until the caller leaves or stops answering, which the session says;
	// until the broker is taken from it, which left says; or until it opens
	// a stream, which is not its to open.
	_, err = session.AcceptStreamWithContext(holder.left)
	// Before anything is said to the caller, which waits for its answer: the
	// broker is gone from the moment its caller is known to have, and one
	// that went silent answers nothing.
	leave()
	if err == nil {
		_ = conn.Close(websocket.StatusPolicyViolation, "a caller serving a broker opens no stream")
	}
}

// give gives the sandbox a broker of the name a handshake asks to serve, on a
// port, held by the caller while its handshake is answered — or is the
// refusal: no such sandbox, one that is not running, a request that is not
// the handshake, a name the sandbox already has. In that order, which is the
// contract's.
func (d *Daemon) give(w http.ResponseWriter, r *http.Request, name string) (*sandbox, *caller, int, bool) {
	for {
		d.mu.Lock()
		s, ok := d.lookup(w, r)
		if !ok {
			d.mu.Unlock()
			return nil, nil, 0, false
		}
		refusal := func(status int, code genv1.ErrorCode, message string) (*sandbox, *caller, int, bool) {
			d.mu.Unlock()
			refuse(w, status, code, message)
			return nil, nil, 0, false
		}
		at := has(s, name)
		switch {
		case s.record.State != genv1.SandboxStateReady:
			return refusal(http.StatusConflict, genv1.ErrorCodeConflict, "the sandbox is "+string(s.record.State)+": a broker a caller serves is a running machine's")
		case !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") || !slices.Contains(offered(r), brokerProtocol):
			return refusal(http.StatusBadRequest, genv1.ErrorCodeBadRequest, "this route is a WebSocket: send the handshake, offering "+brokerProtocol)
		case at >= 0 && s.callers[name] == nil:
			return refusal(http.StatusConflict, genv1.ErrorCodeConflict, fmt.Sprintf("this sandbox already has a broker %s, which is %s", name, brokersOf(s)[at].Kind))
		}
		holder, held := s.callers[name]
		if !held {
			// The broker is there from here, before the handshake is
			// answered: once it has been, its port is open and a connection
			// made to it is carried, with no moment in between for a test to
			// fall into.
			port := nextPort(s)
			holder = newCaller(r)
			s.callers[name] = holder
			setBrokers(s, append(slices.Clone(brokersOf(s)), genv1.SandboxBroker{Name: name, Port: port, Kind: genv1.BrokerKindClient}))
			d.brokersChanged()
			d.serving.Add(1)
			d.mu.Unlock()
			return s, holder, port, true
		}
		session, changed := holder.session, d.changed
		d.mu.Unlock()
		if session == nil {
			// Still being let in, with nobody to ask yet: once it serves, or
			// has gone, this is looked at again.
			select {
			case <-changed:
				continue
			case <-r.Context().Done():
				return nil, nil, 0, false
			}
		}
		// The caller that holds the name is asked whether it is still there,
		// and only one that answers keeps it. A caller that closed a moment
		// ago, and this end not yet told; one that died, and its keepalives
		// not yet missed: refusing the newcomer for either would be refusing
		// the one program that can serve the broker, on the word of one that
		// no longer does. One that answers is never moved.
		if _, err := session.Ping(); err == nil {
			refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, fmt.Sprintf("this sandbox already has a broker %s, which another caller is serving", name))
			return nil, nil, 0, false
		}
		d.mu.Lock()
		if s.callers[name] == holder {
			d.takeAway(s, name)
		}
		d.mu.Unlock()
	}
}

// brokersChanged tells whoever waits in BrokerServed or DialBroker, or for a
// caller that left to have gone, to look again. Called with the lock held.
func (d *Daemon) brokersChanged() {
	close(d.changed)
	d.changed = make(chan struct{})
}

// BrokerServed returns once a caller is serving a broker of that name in the
// sandbox — or, with served false, once none is, which is true of a sandbox
// that is not there. It is what a test waits on before DialBroker, where a
// workload would try again: a program under test starts serving when it gets
// to it, and its broker is gone a moment after it closed. The error is ctx's,
// when that ends first.
func (d *Daemon) BrokerServed(ctx context.Context, sandboxID, name string, served bool) error {
	for {
		d.mu.Lock()
		is := false
		if id, err := uuid.Parse(sandboxID); err == nil {
			if s, ok := d.sandboxes[id]; ok {
				_, is = s.callers[name]
			}
		}
		changed := d.changed
		d.mu.Unlock()
		if is == served {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// goingAway is the WebSocket as the session holds it, which says it is going
// before it hangs up — 1001, as the daemon does — so the caller reads that
// its broker was taken away, not that the line broke. Not the net.Conn's own
// Close, which says 1000 and cancels the read in flight as it does.
type goingAway struct {
	net.Conn
	socket *websocket.Conn
}

func (g goingAway) Close() error {
	return g.socket.Close(websocket.StatusGoingAway, "this caller no longer serves the broker")
}

// DialBroker is a connection as a workload inside the sandbox would make it
// to the broker's port: what is written to it comes out of the Accept of
// whoever serves the broker, and what they write back is read from it. Its
// CloseWrite is the workload having finished writing, as `nc -N` does.
//
// An error when no caller is serving a broker of that name: there is no port
// for a workload to connect to.
func (d *Daemon) DialBroker(sandboxID, name string) (net.Conn, error) {
	id, err := uuid.Parse(sandboxID)
	if err != nil {
		return nil, fmt.Errorf("fakedaemon: %q is not a sandbox id", sandboxID)
	}
	var session *yamux.Session
	for session == nil {
		d.mu.Lock()
		s, ok := d.sandboxes[id]
		var letIn <-chan struct{}
		switch {
		case !ok:
			err = fmt.Errorf("fakedaemon: no sandbox %s", id)
		case s.callers[name] == nil:
			err = fmt.Errorf("fakedaemon: no caller is serving a broker %s in sandbox %s", name, id)
		case s.callers[name].session == nil:
			// Its handshake was answered and its session is being made: a
			// caller whose ServeBroker has returned is serving, and a
			// connection made the moment after is one it gets.
			letIn = d.changed
		default:
			session = s.callers[name].session
		}
		d.mu.Unlock()
		if err != nil {
			return nil, err
		}
		if letIn != nil {
			<-letIn
		}
	}
	stream, err := session.OpenStream()
	if err != nil {
		return nil, fmt.Errorf("fakedaemon: connecting to broker %s of sandbox %s: %w", name, id, err)
	}
	return &workloadConn{Stream: stream}, nil
}

// workloadConn is a workload's connection to a client broker: a stream of
// the session its server holds.
type workloadConn struct {
	*yamux.Stream
	closed atomic.Bool
}

// errCut is a connection whose broker went with its caller: the daemon cuts
// it.
var errCut = errors.New("fakedaemon: the broker's connection was cut: its caller has gone, and the broker with it")

// Read is the stream's, but for the end of its session: yamux reads that as
// the other end having finished — io.EOF — and an answer cut short would pass
// for a whole one.
func (c *workloadConn) Read(p []byte) (int, error) {
	n, err := c.Stream.Read(p)
	switch {
	case err == nil:
	case c.closed.Load():
		err = net.ErrClosed
	case c.Session().IsClosed():
		err = errCut
	}
	return n, err
}

// CloseWrite is the workload having finished writing: what the server still
// answers is still read.
func (c *workloadConn) CloseWrite() error { return c.Stream.Close() }

// Close ends the connection for good. A stream's own Close is the
// half-close, after which a Read goes on waiting for the server: the deadline
// is what ends one in flight.
func (c *workloadConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	_ = c.SetReadDeadline(time.Unix(1, 0))
	return c.Stream.Close()
}

// takeAwayEvery ends every client broker, as a daemon going down does. Their
// handlers are waited for with the terminals'.
func (d *Daemon) takeAwayEvery() {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, s := range d.sandboxes {
		d.takeAwayAll(s)
	}
}
