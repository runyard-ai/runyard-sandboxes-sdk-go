package sdk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/hashicorp/yamux"
)

// brokerProtocol is the WebSocket subprotocol of the route a client broker is
// served on.
const brokerProtocol = "runyard.broker.v1"

// brokerKeepAlive is how often the daemon is asked whether it is still there
// while a broker is served, and it is given twice that to answer: a daemon
// gone without a word — a network that dropped — is noticed within a quarter
// of a minute, which is what the daemon gives a caller.
const brokerKeepAlive = 5 * time.Second

// brokerPortHeader is where the daemon's answer to a serve says the port the
// broker is on, inside the sandbox.
const brokerPortHeader = "Runyard-Broker-Port"

// ServeBroker gives the sandbox a broker of this name and makes this program
// its far end, in one act: a port opens inside the machine — the listener
// says where — and every connection the workload makes to it comes out of
// Accept, where what is written back is what the workload reads. So
// `http.Serve(listener, handler)` answers the workload from here, with
// whatever this program holds and the sandbox must not.
//
// Nothing declares the broker beforehand, and nothing of it is kept: it is
// there for as long as the listener is, and gone — its port closed, its
// connections cut — when the listener is closed or lost.
//
// ctx bounds the handshake only: once ServeBroker has returned, ending ctx
// ends nothing. What can refuse refuses here, as an *Error: no such sandbox,
// a sandbox that is not running or one that already has a broker of this
// name (both `conflict`), a machine that cannot be told to open the port
// (`sandbox_unreachable`).
//
// Close takes the broker away, and cuts the connections still open. When the
// daemon hangs up — the sandbox was stopped, paused or deleted, the broker
// detached, the daemon stopped — or the line to it is lost, Accept returns an
// error and the listener is done: there is no reconnecting inside it, and a
// caller that wants the broker back serves it again, on whatever port it is
// given then.
//
// **Close does not wait for what is being answered, and so neither does
// `http.Server.Shutdown`.** Shutdown closes its listeners and then waits for
// its connections, and on a listener of the network that leaves them open.
// Here they are streams of the one session Close ends, and the broker itself
// is that session: there is no closing the listener and keeping it. A request
// in flight is cut, the workload reads an answer that ends short, and
// Shutdown still returns nil. To stop without cutting anything, stop what
// asks — the workload — first, and Close once it has ended.
//
// Accept is to be called as connections arrive, as a server does: at most 256
// wait to be accepted, one made past that is closed as soon as it is made,
// and one the daemon has waited 75 seconds for ends the serve. A connection
// has a CloseWrite as a TCP one does, for an answer that ends before the
// workload has finished asking; five minutes after either end has finished
// writing, the connection is closed altogether.
func (s *Sandbox) ServeBroker(ctx context.Context, name string) (*BrokerListener, error) {
	address := s.client.baseURL + "/v1/sandboxes/" + url.PathEscape(s.ID.String()) + "/brokers/" + url.PathEscape(name) + "/serve"
	socket, answer, err := s.client.upgrade(ctx, address, brokerProtocol)
	if err != nil {
		if _, refusal := errors.AsType[*Error](err); refusal {
			return nil, err
		}
		return nil, fmt.Errorf("serving broker %s of %s: %w", name, s.ID, err)
	}
	if socket.Subprotocol() != brokerProtocol {
		// Whatever answered is not speaking this protocol, and its frames
		// would be read as a session's.
		_ = socket.CloseNow()
		return nil, fmt.Errorf("serving broker %s of %s: the daemon did not agree to %s", name, s.ID, brokerProtocol)
	}
	// The port is the whole of what the workload needs of the broker, and
	// only the daemon knows: one that did not say has given this program a
	// broker nothing can be pointed at.
	port, err := strconv.Atoi(answer.Get(brokerPortHeader))
	if err != nil || port < 1 || port > 65535 {
		_ = socket.CloseNow()
		return nil, fmt.Errorf("serving broker %s of %s: the daemon did not say which port the broker is on (%s: %q)", name, s.ID, brokerPortHeader, answer.Get(brokerPortHeader))
	}

	listener := &BrokerListener{addr: brokerAddr(s.ID.String() + "/" + name), port: port}
	// This end accepts, and the daemon opens. The error is the
	// configuration's, and this one cannot be wrong.
	listener.session, _ = yamux.Server(line{
		// Not ctx, which bounds the handshake: the session lasts until Close.
		// NetConn lifts the socket's read limit, which a session's frames —
		// as large as a stream's window — are over.
		Conn:     websocket.NetConn(context.WithoutCancel(ctx), socket, websocket.MessageBinary),
		socket:   socket,
		listener: listener,
		remote:   brokerAddr(s.ID.String()),
	}, sessionConfig(s.client.brokerKeepAlive))
	return listener, nil
}

// sessionConfig is how the session with the daemon is run.
func sessionConfig(keepAlive time.Duration) *yamux.Config {
	config := yamux.DefaultConfig()
	// Said, although it is the default: the keepalives are how this end
	// learns the daemon is gone, when it went without closing anything.
	config.EnableKeepAlive = true
	config.KeepAliveInterval = keepAlive
	config.ConnectionWriteTimeout = 2 * keepAlive
	// yamux writes what goes wrong with a session to standard error, which is
	// the program's: here it is what Accept returns.
	config.LogOutput = io.Discard
	return config
}

// brokerAddr is an end of a client broker's connection: the sandbox the
// workload dialled from, or `<sandbox>/<broker>`, which is what it dialled.
// There is no host and port to give: the connection came through the daemon.
type brokerAddr string

func (brokerAddr) Network() string  { return "runyard-broker" }
func (a brokerAddr) String() string { return string(a) }

// line is the WebSocket as the session holds it: its frames, addresses that
// say what its streams are rather than the library's "unknown", and why it
// ended, which a session tells Accept and not its streams.
type line struct {
	net.Conn
	socket   *websocket.Conn
	listener *BrokerListener
	remote   brokerAddr
}

func (l line) LocalAddr() net.Addr  { return l.listener.addr }
func (l line) RemoteAddr() net.Addr { return l.remote }

func (l line) Read(p []byte) (int, error) {
	n, err := l.Conn.Read(p)
	if err != nil {
		l.listener.ended.CompareAndSwap(nil, &err)
	}
	return n, err
}

func (l line) Write(p []byte) (int, error) {
	n, err := l.Conn.Write(p)
	if err != nil {
		l.listener.ended.CompareAndSwap(nil, &err)
	}
	return n, err
}

// Close says goodbye before it hangs up, and waits to be answered: the daemon
// reads that this end stopped serving, rather than finding it gone. Not the
// net.Conn's own Close, which cancels the read in flight as it says so, and
// leaves which of the two the daemon sees to chance.
func (l line) Close() error {
	return l.socket.Close(websocket.StatusNormalClosure, "")
}

// session is what a listener needs of its yamux session.
type session interface {
	AcceptStream() (*yamux.Stream, error)
	IsClosed() bool
	Close() error
}

// BrokerListener is a broker this program gave a sandbox by serving it: a
// net.Listener whose connections are the workload's, for as long as the
// broker lasts, which is as long as this does.
type BrokerListener struct {
	session session
	addr    brokerAddr
	// port is where the broker is inside the sandbox, on its loopback.
	port int
	// closed is whether Close was called here, which is what Accept and the
	// connections say afterwards rather than that the daemon hung up.
	closed atomic.Bool
	// ended is what first went wrong with the line, once something has.
	ended atomic.Pointer[error]
}

func (l *BrokerListener) Accept() (net.Conn, error) {
	stream, err := l.session.AcceptStream()
	if err == nil && l.session.IsClosed() {
		// One that was waiting when the session ended, which yamux hands
		// over as readily as it says the session has: it is cut already.
		err = yamux.ErrSessionShutdown
	}
	if err != nil {
		// A session that could not answer one stream is not one to wait on
		// for the next: the listener is done, whatever went wrong. One that
		// has ended already is not closed again, which would wait for its
		// goodbye to a daemon that may not be there to hear it.
		if !l.session.IsClosed() {
			_ = l.session.Close()
		}
		return nil, l.over("accept", err)
	}
	return &brokerConn{Stream: stream, listener: l}, nil
}

// Close takes the broker away: the daemon is told, every connection still
// open is cut, and the port inside the sandbox closes. It
// returns once the socket is closed and the session's reading and writing
// have ended. Closing again waits for the same, and is nothing.
func (l *BrokerListener) Close() error {
	l.closed.Store(true)
	return l.session.Close()
}

func (l *BrokerListener) Addr() net.Addr { return l.addr }

// Port is the port the broker is on inside the sandbox, on `127.0.0.1`: the
// daemon chose it when the broker was given, and it is the broker's until
// the listener is closed.
func (l *BrokerListener) Port() int { return l.port }

// URL is the broker as a workload reaches it over HTTP, inside the sandbox:
// `http://127.0.0.1:<port>`. What makes a call to it work is this program,
// answering — the sandbox holds no credential — so it is an address, not a
// capability.
func (l *BrokerListener) URL() string { return fmt.Sprintf("http://127.0.0.1:%d", l.port) }

// errHungUp is the daemon ending the session: the sandbox was stopped, paused
// or deleted, the broker detached, or the daemon stopped.
var errHungUp = errors.New("the daemon hung up")

// over is the error of a listener that is done, or of a connection of one:
// that it was closed here, or else why the line ended, which err is when
// nothing said more.
func (l *BrokerListener) over(op string, err error) error {
	if ended := l.ended.Load(); ended != nil {
		err = *ended
	}
	switch {
	case l.closed.Load():
		err = net.ErrClosed
	//nolint:errorlint // io.EOF itself is the socket closed with a goodbye; one wrapped is a line that broke mid-frame, which is the next case
	case err == io.EOF && l.ended.Load() != nil:
		// And not said as io.EOF, which on a connection is the workload
		// having finished.
		err = errHungUp
	default:
		err = fmt.Errorf("the line to the daemon was lost: %w", err)
	}
	return &net.OpError{Op: op, Net: l.addr.Network(), Addr: l.addr, Err: err}
}

// brokerConn is one connection the workload made: a stream of the session.
type brokerConn struct {
	*yamux.Stream
	listener *BrokerListener
	closed   atomic.Bool
}

func (c *brokerConn) Read(p []byte) (int, error) {
	n, err := c.Stream.Read(p)
	if err != nil {
		err = c.failed("read", err)
	}
	return n, err
}

func (c *brokerConn) Write(p []byte) (int, error) {
	n, err := c.Stream.Write(p)
	if err != nil {
		err = c.failed("write", err)
	}
	return n, err
}

// failed is a stream's error, as a connection's. A stream of a session that
// has ended reads as one whose other end finished — io.EOF — and an answer
// cut short would pass for a whole one: it is the listener's error instead.
// That is said of a stream the workload had finished as well, when the
// session ended before its end was read: by then nothing can tell them apart.
func (c *brokerConn) failed(op string, err error) error {
	switch {
	case c.closed.Load():
		return &net.OpError{Op: op, Net: c.listener.addr.Network(), Addr: c.listener.addr, Err: net.ErrClosed}
	case c.Session().IsClosed():
		return c.listener.over(op, err)
	}
	// As yamux says it: io.EOF, and a timeout a caller of SetDeadline can
	// tell is one.
	return err
}

// CloseWrite is this end having finished writing: the workload reads the end
// of the answer, and what it still sends is still read. yamux keeps a stream
// closed at one end for five minutes, and then resets it.
func (c *brokerConn) CloseWrite() error { return c.Stream.Close() }

// Close ends the connection for good. A stream's own Close is the
// half-close, after which a Read goes on waiting for the workload: the
// deadline is what ends one in flight, and refuses the next.
func (c *brokerConn) Close() error {
	if c.closed.Swap(true) {
		return nil
	}
	_ = c.SetReadDeadline(time.Unix(1, 0))
	return c.Stream.Close()
}
