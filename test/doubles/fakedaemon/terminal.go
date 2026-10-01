package fakedaemon

import (
	"context"
	"encoding/json"
	"io"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/coder/websocket"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// Terminal is a terminal a caller opened, as WithTerminal's function sees it:
// what was asked for, what is typed, and where the screen goes.
type Terminal struct {
	Argv       []string
	User       string
	Cols, Rows int
	// In is what the caller types. It ends when the caller hangs up, which is
	// also when the function's context ends.
	In io.Reader
	// Out is the screen.
	Out io.Writer
	// Resizes are the sizes the caller sends, as [cols, rows], in order. The
	// first 64 a function has not taken are kept, and later ones dropped.
	Resizes <-chan [2]int
}

// WithTerminal decides what a terminal does: it runs for as long as what the
// terminal runs would, and what it returns is the exit code the caller is
// told. Its context ends when the caller hangs up; one that ignores that keeps
// the terminal's handler running. A daemon without it refuses terminals, as
// `not_implemented`.
func WithTerminal(run func(ctx context.Context, id genv1.SandboxID, terminal *Terminal) int) Option {
	return func(d *Daemon) { d.tty = run }
}

const terminalProtocol = "runyard.terminal.v1"

func (d *Daemon) openTerminal(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	terminal := &Terminal{Argv: query["argv"], User: query.Get("user"), Cols: 80, Rows: 24}
	for name, size := range map[string]*int{"cols": &terminal.Cols, "rows": &terminal.Rows} {
		if value := query.Get(name); value != "" {
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 1000 {
				refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "cols and rows are each between 1 and 1000")
				return
			}
			*size = n
		}
	}
	d.mu.Lock()
	s, ok := d.lookup(w, r)
	if !ok {
		d.mu.Unlock()
		return
	}
	id, state := s.record.Id, s.record.State
	d.mu.Unlock()
	switch {
	case state != genv1.SandboxStateReady:
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "the sandbox is "+string(state))
		return
	case d.tty == nil:
		refuse(w, http.StatusNotImplemented, genv1.ErrorCodeNotImplemented, "this fake daemon was given no WithTerminal")
		return
	case !strings.EqualFold(r.Header.Get("Upgrade"), "websocket"):
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "this route is a WebSocket: send the handshake, offering "+terminalProtocol)
		return
	}

	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{terminalProtocol}})
	if err != nil {
		return // Accept has answered
	}
	d.mu.Lock()
	d.terminals[conn] = struct{}{}
	d.serving.Add(1)
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		delete(d.terminals, conn)
		d.mu.Unlock()
		d.serving.Done()
	}()

	// Not the request's context: the connection stopped being the server's
	// when it became a WebSocket, and its end is the caller hanging up.
	ctx, hungUp := context.WithCancel(context.WithoutCancel(r.Context()))
	defer hungUp()
	in, typed := io.Pipe()
	resizes := make(chan [2]int, 64)
	terminal.In, terminal.Resizes = in, resizes
	terminal.Out = screen{ctx: ctx, conn: conn}

	reading := make(chan struct{})
	go func() { //task:unowned joined below, by <-reading
		defer close(reading)
		defer hungUp()
		defer func() { _ = typed.Close() }()
		for {
			kind, message, err := conn.Read(ctx)
			if err != nil {
				return
			}
			if kind == websocket.MessageBinary {
				if _, err := typed.Write(message); err != nil {
					return
				}
				continue
			}
			var resize struct {
				Type       string `json:"type"`
				Cols, Rows int
			}
			if json.Unmarshal(message, &resize) == nil && resize.Type == "resize" {
				select {
				case resizes <- [2]int{resize.Cols, resize.Rows}:
				default:
				}
			}
		}
	}()

	code := d.tty(ctx, id, terminal)
	_ = in.Close()
	if exit, err := json.Marshal(map[string]any{"type": "exit", "exitCode": code}); err == nil {
		_ = conn.Write(ctx, websocket.MessageText, exit)
	}
	_ = conn.Close(websocket.StatusNormalClosure, "exited")
	hungUp()
	<-reading
}

// screen is a terminal's output, a binary message per write.
type screen struct {
	ctx  context.Context
	conn *websocket.Conn
}

func (s screen) Write(p []byte) (int, error) {
	if err := s.conn.Write(s.ctx, websocket.MessageBinary, p); err != nil {
		return 0, err
	}
	return len(p), nil
}

// hangUpTerminals closes every terminal still open, as a daemon going down
// does, and returns once their handlers have.
func (d *Daemon) hangUpTerminals() {
	d.mu.Lock()
	open := slices.Collect(maps.Keys(d.terminals))
	d.mu.Unlock()
	// Outside the lock: a close waits for the other end to answer it, and a
	// handler ending takes the lock to say it has.
	for _, conn := range open {
		_ = conn.Close(websocket.StatusGoingAway, "the daemon is going down")
	}
	d.serving.Wait()
}
