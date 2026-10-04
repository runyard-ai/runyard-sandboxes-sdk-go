package sdk

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/google/uuid"
	"github.com/hashicorp/yamux"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakedaemon"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/spawn"
)

// brokered is a ready sandbox with the host's shared broker `claude`, and
// the daemon it is on. The broker the tests serve, `llm`, is given by serving
// it.
func brokered(t *testing.T) (*fakedaemon.Daemon, *Sandbox) {
	t.Helper()
	d, client := fake(t, fakedaemon.WithBrokers("claude"))
	sandbox, err := client.Create(t.Context(), "served", Spec{
		Image:   "alpine",
		Brokers: &[]genv1.SandboxBrokerSpec{{Name: "claude"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return d, sandbox
}

// serve gives the sandbox the broker `llm`, served here. The test closes what
// it returns: a cleanup would run after those that wait for what Accepts on
// it.
func serve(t *testing.T, sandbox *Sandbox) *BrokerListener {
	t.Helper()
	listener, err := sandbox.ServeBroker(t.Context(), "llm")
	if err != nil {
		t.Fatal(err)
	}
	return listener
}

// workload is a connection the workload makes to the broker's port.
func workload(t *testing.T, d *fakedaemon.Daemon, sandbox *Sandbox) net.Conn {
	t.Helper()
	conn, err := d.DialBroker(sandbox.ID.String(), "llm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// finish is one end having finished writing, which the other reads as the
// end of what it was sent.
func finish(t *testing.T, conn net.Conn) {
	t.Helper()
	half, ok := conn.(interface{ CloseWrite() error })
	if !ok {
		t.Fatalf("%T has no CloseWrite", conn)
	}
	if err := half.CloseWrite(); err != nil {
		t.Fatal(err)
	}
}

// answering hands every connection the listener accepts to answer, each on a
// goroutine of its own, until the listener is done.
func answering(t *testing.T, listener net.Listener, answer func(net.Conn)) {
	t.Helper()
	spawn.Go(t, func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			spawn.Go(t, func() {
				defer func() { _ = conn.Close() }()
				answer(conn)
			})
		}
	})
}

// patience is how long a test waits for what should have happened before it
// says it did not: never reached when it did.
const patience = 5 * time.Second

// soon is what a channel delivers, or the test failing for want of it.
func soon[T any](t *testing.T, from <-chan T, what string) T {
	t.Helper()
	select {
	case got := <-from:
		return got
	case <-time.After(patience):
		t.Fatalf("%s: not after %s", what, patience)
		panic("unreachable")
	}
}

// noise is n bytes that are not the same from one megabyte to the next, so a
// frame delivered twice, or out of order, is a different sum.
func noise(seed byte, n int) []byte {
	out := make([]byte, n)
	block := sha256.Sum256([]byte{seed})
	for at := 0; at < n; at += len(block) {
		copy(out[at:], block[:])
		block = sha256.Sum256(block[:])
	}
	return out
}

// Serving is what gives the sandbox the broker: the listener says where it
// is, the daemon lists it for as long as the listener lasts, and the handle —
// what the sandbox was when it was made — does not have it.
func TestServingGivesTheSandboxABrokerAndSaysWhereItIs(t *testing.T) {
	d, sandbox := brokered(t)
	listener := serve(t, sandbox)
	listed := func() map[string]genv1.SandboxBroker {
		t.Helper()
		out := map[string]genv1.SandboxBroker{}
		for _, broker := range *d.Sandboxes()[0].Brokers {
			out[broker.Name] = broker
		}
		return out
	}
	llm, has := listed()["llm"]
	if !has || llm.Kind != genv1.BrokerKindClient || llm.Port != listener.Port() || llm.Port == sandbox.Brokers["claude"] || llm.Port == 0 {
		t.Errorf("the daemon lists %+v, %v; the listener says port %d", llm, has, listener.Port())
	}
	if listener.URL() != fmt.Sprintf("http://127.0.0.1:%d", llm.Port) {
		t.Errorf("the listener says the broker is at %s", listener.URL())
	}
	if _, has := sandbox.Broker("llm"); has || len(sandbox.Brokers) != 1 {
		t.Errorf("the handle made before it was served has %v", sandbox.Brokers)
	}
	if address, ok := sandbox.Broker("claude"); !ok || address != fmt.Sprintf("http://127.0.0.1:%d", sandbox.Brokers["claude"]) {
		t.Errorf("Broker(claude) = %q, %v", address, ok)
	}

	// Closing takes it away.
	if err := listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err := d.BrokerServed(t.Context(), sandbox.ID.String(), "llm", false); err != nil {
		t.Fatal(err)
	}
	if after := listed(); len(after) != 1 || after["claude"].Kind != genv1.BrokerKindShared {
		t.Errorf("once the listener is closed, the daemon lists %+v", after)
	}
	// Where it was is still what the listener says: it is what it was given.
	if listener.Port() != llm.Port {
		t.Errorf("a closed listener says port %d", listener.Port())
	}
}

// A daemon that gives a broker and does not say where has given nothing a
// workload can be pointed at: the serve is refused, and hung up.
func TestAnAnswerThatDoesNotSayThePortIsRefused(t *testing.T) {
	for name, port := range map[string]string{"nothing": "", "not a number": "the usual", "no port": "0", "more than a port": "65536"} {
		t.Run(name, func(t *testing.T) {
			d, sandbox := brokered(t)
			hungUp := make(chan struct{})
			d.Intercept("serveSandboxBroker", func(w http.ResponseWriter, r *http.Request, _ http.HandlerFunc) {
				defer close(hungUp)
				if port != "" {
					w.Header().Set(brokerPortHeader, port)
				}
				socket, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{brokerProtocol}})
				if err != nil {
					t.Errorf("accepting the handshake: %v", err)
					return
				}
				// Read until the caller hangs up, which it does at once.
				_, _, _ = socket.Read(r.Context())
				_ = socket.CloseNow()
			})
			listener, err := sandbox.ServeBroker(t.Context(), "llm")
			if err == nil || listener != nil || !strings.Contains(err.Error(), "did not say which port") {
				t.Fatalf("%v, %v", listener, err)
			}
			soon(t, hungUp, "the socket was left open")
		})
	}
}

func TestAServedBrokerCarriesTheWorkloadsBytesAndTheAnswer(t *testing.T) {
	d, sandbox := brokered(t)
	listener := serve(t, sandbox)
	defer func() { _ = listener.Close() }()
	if got := listener.Addr(); got.Network() != "runyard-broker" || got.String() != sandbox.ID.String()+"/llm" {
		t.Errorf("the listener is at %s %s", got.Network(), got)
	}
	conn := workload(t, d, sandbox)
	if _, err := io.WriteString(conn, "what is the answer\n"); err != nil {
		t.Fatal(err)
	}
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = accepted.Close() }()
	// Where it came from is the sandbox, and where it went the broker: there
	// is no host and port to give.
	if local, remote := accepted.LocalAddr(), accepted.RemoteAddr(); local.String() != listener.Addr().String() || remote.String() != sandbox.ID.String() || remote.Network() != "runyard-broker" {
		t.Errorf("the connection is from %s to %s", remote, local)
	}
	asked, err := bufio.NewReader(accepted).ReadString('\n')
	if err != nil || asked != "what is the answer\n" {
		t.Fatalf("%q %v", asked, err)
	}
	if _, err := io.WriteString(accepted, "42\n"); err != nil {
		t.Fatal(err)
	}
	answer, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil || answer != "42\n" {
		t.Fatalf("%q %v", answer, err)
	}
}

// A body far larger than a stream's window, and than a WebSocket's default
// read limit, each way: first one after the other, then both at once.
func TestMegabytesGoBothWays(t *testing.T) {
	const size = 6<<20 + 12345
	t.Run("a question and then an answer", func(t *testing.T) {
		d, sandbox := brokered(t)
		listener := serve(t, sandbox)
		defer func() { _ = listener.Close() }()
		question, answer := noise(1, size), noise(2, size)
		asked := make(chan []byte, 1)
		answering(t, listener, func(conn net.Conn) {
			got, err := io.ReadAll(conn)
			if err != nil {
				t.Errorf("reading the question: %v", err)
			}
			asked <- got
			if _, err := conn.Write(answer); err != nil {
				t.Errorf("writing the answer: %v", err)
			}
		})
		conn := workload(t, d, sandbox)
		if n, err := conn.Write(question); err != nil || n != size {
			t.Fatalf("wrote %d: %v", n, err)
		}
		finish(t, conn)
		got, err := io.ReadAll(conn)
		if err != nil || !bytes.Equal(got, answer) {
			t.Fatalf("the answer read is %d bytes, %v: not what was written", len(got), err)
		}
		if got := <-asked; !bytes.Equal(got, question) {
			t.Fatalf("the question read is %d bytes: not what was written", len(got))
		}
	})
	t.Run("both at once", func(t *testing.T) {
		d, sandbox := brokered(t)
		listener := serve(t, sandbox)
		defer func() { _ = listener.Close() }()
		answering(t, listener, func(conn net.Conn) {
			if _, err := io.Copy(conn, conn); err != nil {
				t.Errorf("echoing: %v", err)
			}
		})
		conn := workload(t, d, sandbox)
		sent := noise(3, size)
		spawn.Go(t, func() {
			if _, err := conn.Write(sent); err != nil {
				t.Errorf("writing: %v", err)
			}
			finish(t, conn)
		})
		got, err := io.ReadAll(conn)
		if err != nil || !bytes.Equal(got, sent) {
			t.Fatalf("%d bytes came back, %v: not what was sent", len(got), err)
		}
	})
}

func TestManyConnectionsAtOnceAreEachTheirOwn(t *testing.T) {
	d, sandbox := brokered(t)
	listener := serve(t, sandbox)
	defer func() { _ = listener.Close() }()
	answering(t, listener, func(conn net.Conn) {
		asked, err := io.ReadAll(conn)
		if err != nil {
			t.Errorf("reading: %v", err)
		}
		_, _ = conn.Write(append([]byte("heard "), asked...))
	})
	// Fewer than yamux keeps waiting to be accepted, so that how fast they
	// are accepted decides nothing.
	const connections = 120
	answers := make(chan error, connections)
	for i := range connections {
		spawn.Go(t, func() {
			conn, err := d.DialBroker(sandbox.ID.String(), "llm")
			if err != nil {
				answers <- err
				return
			}
			defer func() { _ = conn.Close() }()
			said := append(fmt.Appendf(nil, "connection %d: ", i), noise(byte(i), 64<<10)...)
			if _, err := conn.Write(said); err != nil {
				answers <- err
				return
			}
			finish(t, conn)
			got, err := io.ReadAll(conn)
			if err == nil && !bytes.Equal(got, append([]byte("heard "), said...)) {
				err = fmt.Errorf("connection %d was answered with another's bytes, %d of them", i, len(got))
			}
			answers <- err
		})
	}
	for range connections {
		if err := <-answers; err != nil {
			t.Error(err)
		}
	}
}

func TestEitherEndFinishesWritingAndStillReads(t *testing.T) {
	t.Run("the workload", func(t *testing.T) {
		d, sandbox := brokered(t)
		listener := serve(t, sandbox)
		defer func() { _ = listener.Close() }()
		conn := workload(t, d, sandbox)
		_, _ = io.WriteString(conn, "a request, and nothing after it")
		finish(t, conn)
		accepted, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = accepted.Close() }()
		asked, err := io.ReadAll(accepted)
		if err != nil || string(asked) != "a request, and nothing after it" {
			t.Fatalf("%q %v", asked, err)
		}
		// Its end is read, and it is still answered.
		if _, err := io.WriteString(accepted, "an answer"); err != nil {
			t.Fatal(err)
		}
		_ = accepted.Close()
		if answer, err := io.ReadAll(conn); err != nil || string(answer) != "an answer" {
			t.Fatalf("%q %v", answer, err)
		}
	})
	t.Run("the server", func(t *testing.T) {
		d, sandbox := brokered(t)
		listener := serve(t, sandbox)
		defer func() { _ = listener.Close() }()
		conn := workload(t, d, sandbox)
		accepted, err := listener.Accept()
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = accepted.Close() }()
		_, _ = io.WriteString(accepted, "all there is to say")
		finish(t, accepted)
		if said, err := io.ReadAll(conn); err != nil || string(said) != "all there is to say" {
			t.Fatalf("%q %v", said, err)
		}
		// Having finished, it writes no more, and still reads.
		if _, err := accepted.Write([]byte("more")); err == nil {
			t.Error("a write after CloseWrite")
		}
		_, _ = io.WriteString(conn, "and the workload goes on")
		_ = conn.Close()
		if heard, err := io.ReadAll(accepted); err != nil || string(heard) != "and the workload goes on" {
			t.Fatalf("%q %v", heard, err)
		}
	})
}

// What the listener is for: an http.Server on it answers what the workload
// asks of the broker's port.
func TestAnHTTPServerOnTheListenerAnswersTheWorkload(t *testing.T) {
	d, sandbox := brokered(t)
	listener := serve(t, sandbox)
	defer func() { _ = listener.Close() }()

	next := make(chan struct{})
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("X-From", r.RemoteAddr)
		w.Header().Set("X-Key", r.Header.Get("X-Api-Key"))
		_, _ = w.Write(body)
	})
	mux.HandleFunc("GET /v1/stream", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, event := range []string{"data: one\n", "data: two\n"} {
			_, _ = io.WriteString(w, event)
			if err := http.NewResponseController(w).Flush(); err != nil {
				t.Errorf("flushing: %v", err)
			}
			// Held here until the workload has read it: an event that only
			// arrived once the handler returned would never be asked for.
			<-next
		}
	})
	server := &http.Server{Handler: mux, ReadHeaderTimeout: time.Minute}
	served := make(chan error, 1)
	spawn.Go(t, func() { served <- server.Serve(listener) })

	transport := &http.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
		return d.DialBroker(sandbox.ID.String(), "llm")
	}}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	ask := func(method, path string, body io.Reader) *http.Response {
		t.Helper()
		request, err := http.NewRequestWithContext(t.Context(), method, "http://llm.broker"+path, body)
		if err != nil {
			t.Fatal(err)
		}
		res, err := client.Do(request)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}

	// Several on one connection, as a workload's client keeps it, with a
	// body that is more than one frame.
	question := noise(7, 1<<20)
	for range 3 {
		res := ask(http.MethodPost, "/v1/messages", bytes.NewReader(question))
		answer, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil || res.StatusCode != http.StatusOK || !bytes.Equal(answer, question) {
			t.Fatalf("answered %d with %d bytes, %v", res.StatusCode, len(answer), err)
		}
		// The handler sees the sandbox it answers, and none of its own
		// credentials came from the workload.
		if from := res.Header.Get("X-From"); from != sandbox.ID.String() || res.Header.Get("X-Key") != "" {
			t.Fatalf("the handler saw %q, with key %q", from, res.Header.Get("X-Key"))
		}
	}
	if res := ask(http.MethodGet, "/nowhere", http.NoBody); res.StatusCode != http.StatusNotFound {
		t.Errorf("a path nothing handles = %d", res.StatusCode)
	} else {
		_ = res.Body.Close()
	}

	// A streamed answer arrives as it is flushed, not when it is over.
	res := ask(http.MethodGet, "/v1/stream", http.NoBody)
	defer func() { _ = res.Body.Close() }()
	events := bufio.NewReader(res.Body)
	for _, want := range []string{"data: one\n", "data: two\n"} {
		event, err := events.ReadString('\n')
		if err != nil || event != want {
			t.Fatalf("%q %v, want %q", event, err, want)
		}
		next <- struct{}{}
	}
	if rest, err := io.ReadAll(events); err != nil || len(rest) != 0 {
		t.Fatalf("after the last event: %q %v", rest, err)
	}

	// Closing the server closes the listener, which is how it stops serving.
	if err := server.Close(); err != nil {
		t.Errorf("closing the server: %v", err)
	}
	if err := <-served; !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve returned %v", err)
	}
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("Accept on the listener the server closed: %v", err)
	}
}

func TestClosingTheListenerCutsWhatIsOpenAndIsDone(t *testing.T) {
	d, sandbox := brokered(t)
	listener := serve(t, sandbox)
	conn := workload(t, d, sandbox)
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	// A Read and an Accept in flight are ended by it.
	reads, accepts := make(chan error, 1), make(chan error, 1)
	spawn.Go(t, func() {
		_, err := accepted.Read(make([]byte, 1))
		reads <- err
	})
	spawn.Go(t, func() {
		_, err := listener.Accept()
		accepts <- err
	})
	if err := listener.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := soon(t, reads, "closing the listener did not end a read in flight"); !errors.Is(err, net.ErrClosed) {
		t.Errorf("a read in flight ended with %v", err)
	}
	if err := soon(t, accepts, "closing the listener did not end an accept in flight"); !errors.Is(err, net.ErrClosed) {
		t.Errorf("an accept in flight ended with %v", err)
	}
	// And everything after it is refused the same way.
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("accept after close: %v", err)
	}
	if _, err := accepted.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("a write after close: %v", err)
	}
	if _, err := accepted.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("a read after close: %v", err)
	}
	if err := accepted.Close(); err != nil {
		t.Errorf("closing a connection of a closed listener: %v", err)
	}
	if err := listener.Close(); err != nil {
		t.Errorf("close, again: %v", err)
	}
	// The workload's connection is cut, not ended: an answer it was reading
	// is not a whole one.
	if _, err := io.ReadAll(conn); err == nil {
		t.Error("the workload read a clean end from a server that stopped")
	}
	// And the broker is gone with it: there is nothing to connect to.
	if err := d.BrokerServed(t.Context(), sandbox.ID.String(), "llm", false); err != nil {
		t.Fatal(err)
	}
	if _, err := d.DialBroker(sandbox.ID.String(), "llm"); err == nil {
		t.Error("a connection to a broker whose listener was closed")
	}
}

// Connections nobody had accepted when the session ended are cut with the
// rest, and none is handed over afterwards as if it were good: yamux gives a
// stream that was waiting as readily as it says its session is over.
func TestAConnectionNobodyAcceptedIsNotHandedOverOnceTheSessionIsOver(t *testing.T) {
	// Enough of them that handing over even one in two would show.
	const waiting = 64
	for name, end := range map[string]func(*testing.T, *BrokerListener, *websocket.Conn) (closed bool){
		"closed here": func(t *testing.T, listener *BrokerListener, _ *websocket.Conn) bool {
			t.Helper()
			if err := listener.Close(); err != nil {
				t.Fatal(err)
			}
			return true
		},
		"the daemon vanished": func(t *testing.T, listener *BrokerListener, socket *websocket.Conn) bool {
			t.Helper()
			_ = socket.CloseNow()
			session, ok := listener.session.(*yamux.Session)
			if !ok {
				t.Fatalf("its session is a %T", listener.session)
			}
			soon(t, session.CloseChan(), "the session did not end with its daemon")
			return false
		},
	} {
		t.Run(name, func(t *testing.T) {
			d, sandbox := brokered(t)
			sockets, sessions := daemonEnd(t, d)
			listener := serve(t, sandbox)
			defer func() { _ = listener.Close() }()
			socket, session := <-sockets, <-sessions
			for range waiting {
				if _, err := session.OpenStream(); err != nil {
					t.Fatal(err)
				}
			}
			// Answered once every one of them has arrived: a session reads
			// what it is sent in order.
			if _, err := session.Ping(); err != nil {
				t.Fatal(err)
			}
			closed := end(t, listener, socket)
			for range waiting + 1 {
				conn, err := listener.Accept()
				if err == nil {
					t.Fatalf("a connection of a session that is over was handed over: %v", conn.RemoteAddr())
				}
				if errors.Is(err, net.ErrClosed) != closed {
					t.Fatalf("accept: %v", err)
				}
			}
		})
	}
}

func TestAConnectionClosedHereIsClosedForGood(t *testing.T) {
	d, sandbox := brokered(t)
	listener := serve(t, sandbox)
	defer func() { _ = listener.Close() }()
	conn := workload(t, d, sandbox)
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}

	// A deadline that passes is a timeout a caller can tell is one, and the
	// connection is still good once it is lifted: an http.Server ends its
	// own reads this way.
	if err := accepted.SetReadDeadline(time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	_, err = accepted.Read(make([]byte, 1))
	if timeout, ok := errors.AsType[net.Error](err); !ok || !timeout.Timeout() {
		t.Fatalf("a read past its deadline: %v", err)
	}
	if err := accepted.SetReadDeadline(time.Time{}); err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(conn, "x")
	if n, err := accepted.Read(make([]byte, 1)); n != 1 || err != nil {
		t.Fatalf("a read once the deadline is lifted: %d %v", n, err)
	}

	reads := make(chan error, 1)
	spawn.Go(t, func() {
		_, err := accepted.Read(make([]byte, 1))
		reads <- err
	})
	if err := accepted.Close(); err != nil {
		t.Fatal(err)
	}
	// A stream's own Close would leave it waiting for the workload to hang
	// up too, which this one does not.
	if err := soon(t, reads, "closing the connection did not end a read in flight"); !errors.Is(err, net.ErrClosed) {
		t.Errorf("a read in flight ended with %v", err)
	}
	if _, err := accepted.Read(make([]byte, 1)); !errors.Is(err, net.ErrClosed) {
		t.Errorf("a read after close: %v", err)
	}
	if _, err := accepted.Write([]byte("x")); !errors.Is(err, net.ErrClosed) {
		t.Errorf("a write after close: %v", err)
	}
	if err := accepted.Close(); err != nil {
		t.Errorf("close, again: %v", err)
	}
	// The workload reads the end, and the listener goes on: the next
	// connection is answered.
	if rest, err := io.ReadAll(conn); err != nil || len(rest) != 0 {
		t.Errorf("the workload read %q, %v", rest, err)
	}
	second := workload(t, d, sandbox)
	_, _ = io.WriteString(second, "next")
	finish(t, second)
	again, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = again.Close() }()
	if heard, err := io.ReadAll(again); err != nil || string(heard) != "next" {
		t.Errorf("%q %v", heard, err)
	}
}

// quiet is a session's configuration for a test's own end of one.
func quiet() *yamux.Config {
	config := yamux.DefaultConfig()
	config.EnableKeepAlive = false
	config.StreamOpenTimeout = 0
	config.LogOutput = io.Discard
	return config
}

// daemonEnd answers the next serve in the daemon's place, and hands the test
// the daemon's end of it: the socket, and the session on it. What the test
// does to them is what a daemon that goes wrong does. The handler holds the
// connection until the test has ended.
func daemonEnd(t *testing.T, d *fakedaemon.Daemon) (<-chan *websocket.Conn, <-chan *yamux.Session) {
	t.Helper()
	sockets, sessions := make(chan *websocket.Conn, 1), make(chan *yamux.Session, 1)
	over, ended := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		close(over)
		<-ended
	})
	answered := false
	d.Intercept("serveSandboxBroker", func(w http.ResponseWriter, r *http.Request, _ http.HandlerFunc) {
		answered = true
		defer close(ended)
		w.Header().Set(brokerPortHeader, "4100")
		socket, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{brokerProtocol}})
		if err != nil {
			t.Errorf("accepting the handshake: %v", err)
			return
		}
		session, err := yamux.Client(websocket.NetConn(context.WithoutCancel(r.Context()), socket, websocket.MessageBinary), quiet())
		if err != nil {
			t.Errorf("a session: %v", err)
			return
		}
		sockets <- socket
		sessions <- session
		<-over
		_ = session.Close()
	})
	t.Cleanup(func() {
		if !answered {
			close(ended)
		}
	})
	return sockets, sessions
}

func TestADaemonThatVanishesEndsTheListenerAndWhatIsOpen(t *testing.T) {
	d, sandbox := brokered(t)
	sockets, sessions := daemonEnd(t, d)
	listener := serve(t, sandbox)
	defer func() { _ = listener.Close() }()
	socket, session := <-sockets, <-sessions

	stream, err := session.OpenStream()
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.WriteString(stream, "half of an ans")
	accepted, err := listener.Accept()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = accepted.Close() }()
	// No goodbye: the connection is simply gone, as a daemon killed is.
	if err := socket.CloseNow(); err != nil {
		t.Fatal(err)
	}

	// What was sent is read, and then an error: not the end of an answer.
	read, err := io.ReadAll(accepted)
	if err == nil || errors.Is(err, net.ErrClosed) || string(read) != "half of an ans" {
		t.Errorf("read %q, %v", read, err)
	}
	if _, err := accepted.Write([]byte("x")); err == nil {
		t.Error("a write to a daemon that is gone")
	}
	_, err = listener.Accept()
	if err == nil || errors.Is(err, net.ErrClosed) || !strings.Contains(err.Error(), "the line to the daemon was lost") {
		t.Errorf("accept: %v", err)
	}
	// The listener is done, and stays done.
	if _, again := listener.Accept(); again == nil || again.Error() != err.Error() {
		t.Errorf("accept, again: %v", again)
	}
	// Closing one that ended is nothing, and from then on it is closed.
	if err := listener.Close(); err != nil {
		t.Errorf("close: %v", err)
	}
	if _, err := listener.Accept(); !errors.Is(err, net.ErrClosed) {
		t.Errorf("accept after close: %v", err)
	}
}

func TestADaemonThatSpeaksNonsenseEndsTheListener(t *testing.T) {
	d, sandbox := brokered(t)
	sockets, _ := daemonEnd(t, d)
	listener := serve(t, sandbox)
	defer func() { _ = listener.Close() }()
	// Text, where the frames of a session are binary.
	if err := (<-sockets).Write(t.Context(), websocket.MessageText, []byte(`{"type":"news"}`)); err != nil {
		t.Fatal(err)
	}
	if _, err := listener.Accept(); err == nil || errors.Is(err, net.ErrClosed) {
		t.Errorf("accept: %v", err)
	}
}

func TestAServeTheDaemonRefusesIsItsError(t *testing.T) {
	for _, c := range []struct {
		name   string
		serve  func(*fakedaemon.Daemon, *Sandbox) error
		status int
		code   string
	}{
		{"a name that is not a broker's", func(_ *fakedaemon.Daemon, s *Sandbox) error {
			_, err := s.ServeBroker(t.Context(), "Not_A_Name")
			return err
		}, http.StatusBadRequest, "bad_request"},
		{"a sandbox that is not there", func(_ *fakedaemon.Daemon, s *Sandbox) error {
			_, err := (&Sandbox{ID: uuid.New(), client: s.client}).ServeBroker(t.Context(), "llm")
			return err
		}, http.StatusNotFound, "not_found"},
		{"a sandbox that is not running", func(d *fakedaemon.Daemon, s *Sandbox) error {
			d.Settle(s.ID, genv1.SandboxStateStopped, "")
			_, err := s.ServeBroker(t.Context(), "llm")
			return err
		}, http.StatusConflict, "conflict"},
		{"the name of a broker it has", func(_ *fakedaemon.Daemon, s *Sandbox) error {
			_, err := s.ServeBroker(t.Context(), "claude")
			return err
		}, http.StatusConflict, "conflict"},
		{"a name another caller serves", func(_ *fakedaemon.Daemon, s *Sandbox) error {
			first, err := s.ServeBroker(t.Context(), "llm")
			if err != nil {
				return fmt.Errorf("the first to serve it: %w", err)
			}
			defer func() { _ = first.Close() }()
			_, err = s.ServeBroker(t.Context(), "llm")
			return err
		}, http.StatusConflict, "conflict"},
		{"a machine that cannot be told", func(d *fakedaemon.Daemon, s *Sandbox) error {
			d.Intercept("serveSandboxBroker", fakedaemon.Refuse(http.StatusServiceUnavailable, genv1.ErrorCodeSandboxUnreachable, "the sandbox's agent does not answer"))
			_, err := s.ServeBroker(t.Context(), "llm")
			return err
		}, http.StatusServiceUnavailable, "sandbox_unreachable"},
		{"another key", func(_ *fakedaemon.Daemon, s *Sandbox) error {
			s.client.key = "other"
			_, err := s.ServeBroker(t.Context(), "llm")
			return err
		}, http.StatusUnauthorized, "unauthorized"},
		{"a key without the scope", func(d *fakedaemon.Daemon, s *Sandbox) error {
			d.Intercept("serveSandboxBroker", fakedaemon.Refuse(http.StatusForbidden, genv1.ErrorCodeForbidden, "this key lacks network.write"))
			_, err := s.ServeBroker(t.Context(), "llm")
			return err
		}, http.StatusForbidden, "forbidden"},
		{"a sandbox that is gone", func(_ *fakedaemon.Daemon, s *Sandbox) error {
			if err := s.Close(t.Context()); err != nil {
				return err
			}
			_, err := s.ServeBroker(t.Context(), "llm")
			return err
		}, http.StatusNotFound, "not_found"},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, sandbox := brokered(t)
			err := c.serve(d, sandbox)
			refused, ok := errors.AsType[*Error](err)
			if !ok || refused.Status != c.status || refused.Code != c.code || refused.Message == "" {
				t.Fatalf("%#v", err)
			}
			if CodeOf(err) != c.code {
				t.Errorf("CodeOf = %q", CodeOf(err))
			}
		})
	}

	t.Run("in the daemon's words", func(t *testing.T) {
		d, sandbox := brokered(t)
		d.Intercept("serveSandboxBroker", fakedaemon.Refuse(http.StatusConflict, genv1.ErrorCodeConflict, "this sandbox already has a broker llm"))
		_, err := sandbox.ServeBroker(t.Context(), "llm")
		if refused, ok := errors.AsType[*Error](err); !ok || *refused != (Error{Status: http.StatusConflict, Code: "conflict", Message: "this sandbox already has a broker llm"}) {
			t.Fatalf("%#v", err)
		}
	})
	t.Run("by something that is not the daemon", func(t *testing.T) {
		d, sandbox := brokered(t)
		d.Intercept("serveSandboxBroker", fakedaemon.Answer(http.StatusBadGateway, "text/html", "<h1>Bad Gateway</h1>"))
		_, err := sandbox.ServeBroker(t.Context(), "llm")
		if refused, ok := errors.AsType[*Error](err); !ok || *refused != (Error{Status: http.StatusBadGateway, Message: "<h1>Bad Gateway</h1>"}) {
			t.Fatalf("%#v", err)
		}
	})
}

func TestAServeNobodyAnswersIsAnError(t *testing.T) {
	_, sandbox := brokered(t)
	t.Run("nothing listening", func(t *testing.T) {
		unreachable := *sandbox.client
		unreachable.baseURL = "http://127.0.0.1:1"
		_, err := (&Sandbox{ID: sandbox.ID, client: &unreachable}).ServeBroker(t.Context(), "llm")
		if _, refused := errors.AsType[*Error](err); err == nil || refused || !strings.Contains(err.Error(), "serving broker llm of "+sandbox.ID.String()) {
			t.Fatalf("%v", err)
		}
	})
	t.Run("a daemon that hangs up", func(t *testing.T) {
		d, sandbox := brokered(t)
		d.Intercept("serveSandboxBroker", fakedaemon.HangUp())
		if _, err := sandbox.ServeBroker(t.Context(), "llm"); err == nil || !strings.Contains(err.Error(), "serving broker llm of") {
			t.Fatalf("%v", err)
		}
	})
}

// Something that upgrades and does not agree to the protocol is not serving a
// broker, whatever it is: its frames are not read as a session's.
func TestAnUpgradeThatDoesNotAgreeToTheProtocolIsRefused(t *testing.T) {
	d, sandbox := brokered(t)
	hungUp := make(chan struct{})
	d.Intercept("serveSandboxBroker", func(w http.ResponseWriter, r *http.Request, _ http.HandlerFunc) {
		defer close(hungUp)
		socket, err := websocket.Accept(w, r, nil)
		if err != nil {
			t.Errorf("accepting the handshake: %v", err)
			return
		}
		// Read until the caller hangs up, which it does at once.
		_, _, _ = socket.Read(r.Context())
		_ = socket.CloseNow()
	})
	_, err := sandbox.ServeBroker(t.Context(), "llm")
	if err == nil || !strings.Contains(err.Error(), "did not agree to runyard.broker.v1") {
		t.Fatalf("%v", err)
	}
	<-hungUp
}

// A name is one broker's: a second caller is refused it while the first is
// there, and has it as soon as the first has closed.
func TestANameIsRefusedASecondCallerUntilTheFirstHasLeft(t *testing.T) {
	d, sandbox := brokered(t)
	first := serve(t, sandbox)
	if _, err := sandbox.ServeBroker(t.Context(), "llm"); CodeOf(err) != "conflict" {
		t.Fatalf("a second while the first serves: %v", err)
	}
	// The refusal took nothing from the first.
	answering(t, first, func(conn net.Conn) { _, _ = io.WriteString(conn, "the first") })
	if said, err := io.ReadAll(workload(t, d, sandbox)); err != nil || string(said) != "the first" {
		t.Fatalf("%q %v", said, err)
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := sandbox.ServeBroker(t.Context(), "llm")
	if err != nil {
		t.Fatalf("serving it once the first has closed: %v", err)
	}
	defer func() { _ = second.Close() }()
	answering(t, second, func(conn net.Conn) { _, _ = io.WriteString(conn, "the second") })
	if said, err := io.ReadAll(workload(t, d, sandbox)); err != nil || string(said) != "the second" {
		t.Fatalf("%q %v", said, err)
	}
}

// What takes the broker away from the daemon's side: each is the daemon
// hanging up, which Accept says, and what is open is cut.
func TestTheDaemonHangsUpWhenItTakesTheBrokerAway(t *testing.T) {
	for _, c := range []struct {
		name string
		end  func(*testing.T, *fakedaemon.Daemon, *Sandbox)
		// after is what serving it again is refused with, or nothing.
		after string
	}{
		{"the sandbox is deleted", func(t *testing.T, _ *fakedaemon.Daemon, s *Sandbox) {
			t.Helper()
			if err := s.Close(t.Context()); err != nil {
				t.Fatal(err)
			}
		}, "not_found"},
		{"the sandbox is stopped", func(_ *testing.T, d *fakedaemon.Daemon, s *Sandbox) {
			d.Settle(s.ID, genv1.SandboxStateStopped, "")
		}, "conflict"},
		{"the sandbox is paused", func(_ *testing.T, d *fakedaemon.Daemon, s *Sandbox) {
			d.Settle(s.ID, genv1.SandboxStatePaused, "")
		}, "conflict"},
		{"the broker is detached", func(t *testing.T, _ *fakedaemon.Daemon, s *Sandbox) {
			t.Helper()
			res, err := s.client.API().DetachSandboxBrokerWithResponse(t.Context(), s.ID, "llm")
			if err != nil || res.StatusCode() != http.StatusNoContent {
				t.Fatalf("%v %s", err, res.Body)
			}
		}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			d, sandbox := brokered(t)
			listener := serve(t, sandbox)
			defer func() { _ = listener.Close() }()
			conn := workload(t, d, sandbox)
			accepted, err := listener.Accept()
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = accepted.Close() }()

			c.end(t, d, sandbox)
			accepts := make(chan error, 1)
			spawn.Go(t, func() {
				_, err := listener.Accept()
				accepts <- err
			})
			err = soon(t, accepts, "the listener was not ended")
			if err == nil || errors.Is(err, net.ErrClosed) || !strings.Contains(err.Error(), "the daemon hung up") {
				t.Errorf("accept: %v", err)
			}
			if _, err := io.ReadAll(accepted); err == nil || errors.Is(err, net.ErrClosed) {
				t.Errorf("a read of what was open: %v", err)
			}
			if _, err := io.ReadAll(conn); err == nil {
				t.Error("the workload read a clean end")
			}
			// Served again, it is a broker given again — or refused, when
			// there is no longer a running sandbox to give it.
			again, err := sandbox.ServeBroker(t.Context(), "llm")
			if again != nil {
				_ = again.Close()
			}
			if CodeOf(err) != c.after || (c.after == "" && err != nil) {
				t.Errorf("serving it again: %v", err)
			}
		})
	}
}

func TestTheContextBoundsTheHandshakeAndNothingAfterIt(t *testing.T) {
	t.Run("ended during it", func(t *testing.T) {
		d, sandbox := brokered(t)
		arrived, left := make(chan struct{}), make(chan struct{})
		d.Intercept("serveSandboxBroker", func(_ http.ResponseWriter, r *http.Request, _ http.HandlerFunc) {
			defer close(left)
			close(arrived)
			<-r.Context().Done()
		})
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		refused := make(chan error, 1)
		spawn.Go(t, func() {
			_, err := sandbox.ServeBroker(ctx, "llm")
			refused <- err
		})
		<-arrived
		cancel()
		if err := <-refused; !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
		<-left
	})
	t.Run("ended before it", func(t *testing.T) {
		d, sandbox := brokered(t)
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		if _, err := sandbox.ServeBroker(ctx, "llm"); !errors.Is(err, context.Canceled) {
			t.Fatalf("%v", err)
		}
		if d.Calls("serveSandboxBroker") != 0 {
			t.Error("the daemon was asked")
		}
	})
	t.Run("ended after it", func(t *testing.T) {
		d, sandbox := brokered(t)
		ctx, cancel := context.WithCancel(t.Context())
		listener, err := sandbox.ServeBroker(ctx, "llm")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = listener.Close() }()
		cancel()
		// Still serving: megabytes each way, which is every frame a session
		// has, read and written under a context that is over.
		answering(t, listener, func(conn net.Conn) { _, _ = io.Copy(conn, conn) })
		conn := workload(t, d, sandbox)
		sent := noise(9, 1<<20)
		spawn.Go(t, func() {
			_, _ = conn.Write(sent)
			finish(t, conn)
		})
		if got, err := io.ReadAll(conn); err != nil || !bytes.Equal(got, sent) {
			t.Fatalf("%d bytes came back, %v", len(got), err)
		}
	})
}

// noSession is a session that fails to accept and has not ended: one whose
// answer to a new stream could not be written in time.
type noSession struct {
	closed bool
}

func (s *noSession) AcceptStream() (*yamux.Stream, error) {
	return nil, yamux.ErrConnectionWriteTimeout
}
func (s *noSession) IsClosed() bool { return s.closed }
func (s *noSession) Close() error {
	s.closed = true
	return nil
}

// An Accept that failed is a listener that is done, whatever failed: the
// session is ended with it, rather than left for the next Accept to wait on.
func TestAnAcceptThatFailsEndsTheSession(t *testing.T) {
	session := &noSession{}
	listener := &BrokerListener{session: session, addr: "sandbox/llm"}
	_, err := listener.Accept()
	if !errors.Is(err, yamux.ErrConnectionWriteTimeout) || !strings.Contains(err.Error(), "the line to the daemon was lost") {
		t.Fatalf("%v", err)
	}
	if !session.closed {
		t.Fatal("the session was left open by an Accept that failed")
	}
}

// The session's keepalives are on, at the client's interval, with twice that
// to answer in: they are the only way a daemon that went without closing
// anything is ever noticed.
func TestTheSessionAsksWhetherTheDaemonIsStillThere(t *testing.T) {
	client, err := New("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if client.brokerKeepAlive != 5*time.Second {
		t.Errorf("a client asks every %s", client.brokerKeepAlive)
	}
	config := sessionConfig(7 * time.Second)
	if !config.EnableKeepAlive || config.KeepAliveInterval != 7*time.Second || config.ConnectionWriteTimeout != 14*time.Second {
		t.Errorf("keepalives %v, every %s, answered within %s", config.EnableKeepAlive, config.KeepAliveInterval, config.ConnectionWriteTimeout)
	}
	if err := yamux.VerifyConfig(config); err != nil {
		t.Errorf("not a configuration yamux takes: %v", err)
	}
}

// A daemon that goes silent without closing anything — a network that
// dropped — is noticed by the keepalives it does not answer, and the listener
// is done.
func TestADaemonThatGoesSilentIsNoticed(t *testing.T) {
	d, sandbox := brokered(t)
	over, ended := make(chan struct{}), make(chan struct{})
	d.Intercept("serveSandboxBroker", func(w http.ResponseWriter, r *http.Request, _ http.HandlerFunc) {
		defer close(ended)
		w.Header().Set(brokerPortHeader, "4100")
		socket, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{brokerProtocol}})
		if err != nil {
			t.Errorf("accepting the handshake: %v", err)
			return
		}
		// Held open, and nothing of it read or answered.
		<-over
		_ = socket.CloseNow()
	})
	sandbox.client.brokerKeepAlive = 10 * time.Millisecond
	listener := serve(t, sandbox)
	// The daemon's end first, so that closing the listener is not left
	// waiting for a goodbye from it.
	defer func() { _ = listener.Close() }()
	defer func() {
		close(over)
		<-ended
	}()
	accepted := make(chan error, 1)
	spawn.Go(t, func() {
		_, err := listener.Accept()
		accepted <- err
	})
	err := soon(t, accepted, "a daemon that went silent was not noticed")
	if !errors.Is(err, yamux.ErrKeepAliveTimeout) || errors.Is(err, net.ErrClosed) || !strings.Contains(err.Error(), "the line to the daemon was lost") {
		t.Fatalf("accept: %v", err)
	}
}

// Closing says goodbye: the daemon reads that this caller stopped serving —
// a normal closure, 1000 — rather than finding the line gone.
func TestClosingTheListenerTellsTheDaemon(t *testing.T) {
	// More than once: a close that only sometimes says so is one that does
	// not.
	for range 20 {
		d, sandbox := brokered(t)
		closed := make(chan websocket.StatusCode, 1)
		d.Intercept("serveSandboxBroker", func(w http.ResponseWriter, r *http.Request, _ http.HandlerFunc) {
			w.Header().Set(brokerPortHeader, "4100")
			socket, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{brokerProtocol}})
			if err != nil {
				t.Errorf("accepting the handshake: %v", err)
				closed <- -1
				return
			}
			defer func() { _ = socket.CloseNow() }()
			for {
				// The session's own frames are read past.
				if _, _, err := socket.Read(context.WithoutCancel(r.Context())); err != nil {
					closed <- websocket.CloseStatus(err)
					return
				}
			}
		})
		listener := serve(t, sandbox)
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
		if status := soon(t, closed, "the daemon did not see the caller leave"); status != websocket.StatusNormalClosure {
			t.Fatalf("the daemon read a close with %d, want 1000", status)
		}
	}
}

// What an http.Server's Shutdown promises — nothing in flight is cut — it
// cannot keep on this listener, and the doc comment says so: closing the
// listener ends the session every connection is a stream of.
func TestShutdownOfAnHTTPServerOnTheListenerCutsWhatIsInFlight(t *testing.T) {
	d, sandbox := brokered(t)
	listener := serve(t, sandbox)
	defer func() { _ = listener.Close() }()

	release := make(chan struct{})
	server := &http.Server{ReadHeaderTimeout: time.Minute, Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "half of an answer, ")
		_ = http.NewResponseController(w).Flush()
		<-release
		_, _ = io.WriteString(w, "and the rest")
	})}
	served := make(chan error, 1)
	spawn.Go(t, func() { served <- server.Serve(listener) })

	conn := workload(t, d, sandbox)
	if _, err := io.WriteString(conn, "GET / HTTP/1.1\r\nHost: llm\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	answer := bufio.NewReader(conn)
	res, err := http.ReadResponse(answer, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	begun := make([]byte, len("half of an answer, "))
	if _, err := io.ReadFull(res.Body, begun); err != nil {
		t.Fatal(err)
	}

	shut := make(chan error, 1)
	spawn.Go(t, func() { shut <- server.Shutdown(t.Context()) })
	// The handler has not returned, and the workload's answer is over: cut,
	// not ended.
	if rest, err := io.ReadAll(res.Body); err == nil {
		t.Fatalf("the workload read the rest of an answer that was in flight, %q, as if it were whole", rest)
	}
	close(release)
	if err := soon(t, shut, "Shutdown did not return"); err != nil {
		t.Errorf("Shutdown: %v", err)
	}
	if err := soon(t, served, "Serve did not return"); !errors.Is(err, http.ErrServerClosed) {
		t.Errorf("Serve: %v", err)
	}
}
