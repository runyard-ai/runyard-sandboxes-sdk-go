package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"github.com/coder/websocket"
)

// terminalProtocol is the WebSocket subprotocol of the terminal route.
const terminalProtocol = "runyard.terminal.v1"

// TerminalOptions is what a terminal runs, and how big it starts.
type TerminalOptions struct {
	// Argv is what to run instead of the image's login shell. It is not a
	// shell line, as for Run.
	Argv []string
	// Cols and Rows are its size to start with; zero is the host's default,
	// 80 by 24. Resize changes them.
	Cols, Rows int
	// User is who it runs as, `name` or `uid:gid`; empty is the image's own.
	User string
}

// Terminal is a process inside a sandbox with a PTY on its standard streams:
// what it prints is read, what is typed is written, as a terminal is.
//
// Read and Write may be called at once — from the two goroutines that copy a
// person's keyboard in and the screen out — but Read only from one at a time.
// The process is sent SIGHUP when the terminal is closed, as closing a terminal
// has always meant.
type Terminal struct {
	conn *websocket.Conn
	ctx  context.Context

	// The binary message being read; nil between messages.
	message io.Reader

	mu       sync.Mutex
	exitCode *int
}

// Terminal opens a terminal in the sandbox. ctx bounds all of it: ending it
// closes the terminal.
//
// What can refuse — no such sandbox, one that is not running, no such user —
// refuses here, as an *Error, before anything runs.
func (s *Sandbox) Terminal(ctx context.Context, opts TerminalOptions) (*Terminal, error) {
	query := url.Values{}
	for _, arg := range opts.Argv {
		query.Add("argv", arg)
	}
	if opts.Cols > 0 {
		query.Set("cols", strconv.Itoa(opts.Cols))
	}
	if opts.Rows > 0 {
		query.Set("rows", strconv.Itoa(opts.Rows))
	}
	if opts.User != "" {
		query.Set("user", opts.User)
	}
	address := s.client.baseURL + "/v1/sandboxes/" + url.PathEscape(s.ID.String()) + "/terminal"
	if len(query) > 0 {
		address += "?" + query.Encode()
	}
	conn, _, err := s.client.upgrade(ctx, address, terminalProtocol)
	if err != nil {
		if _, refusal := errors.AsType[*Error](err); refusal {
			return nil, err
		}
		return nil, fmt.Errorf("opening a terminal in %s: %w", s.ID, err)
	}
	// The terminal's bytes are a person's screen: a message of a megabyte is
	// a lot of screen, and one bigger is not a terminal's.
	conn.SetReadLimit(1 << 20)
	return &Terminal{conn: conn, ctx: ctx}, nil
}

// upgrade makes the WebSocket handshake of a route, offering its subprotocol,
// and is the socket and the headers it was answered with. A daemon that
// refuses does so before the upgrade, as it refuses any request: that comes
// back as the *Error it is.
func (c *Client) upgrade(ctx context.Context, address, protocol string) (*websocket.Conn, http.Header, error) {
	header := http.Header{}
	if c.key != "" {
		// A header, not the subprotocol a browser offers its key in: this is
		// not a browser, and a header is not echoed into anyone's logs.
		header.Set("Authorization", "Bearer "+c.key)
	}
	//nolint:bodyclose // a dial that succeeded leaves no body, the connection being the socket's; one that failed is closed below
	conn, res, err := websocket.Dial(ctx, address, &websocket.DialOptions{
		HTTPClient:   c.httpClient,
		HTTPHeader:   header,
		Subprotocols: []string{protocol},
	})
	if err != nil {
		if res != nil && res.StatusCode != http.StatusSwitchingProtocols {
			// What the library kept of the body: enough for the contract's
			// error, which is short.
			body, _ := io.ReadAll(res.Body)
			closeBody(res.Body)
			return nil, nil, refused(res, body)
		}
		return nil, nil, err
	}
	return conn, res.Header, nil
}

// control is a text message: a resize going in, an exit coming out.
type control struct {
	Type     string `json:"type"`
	Cols     int    `json:"cols,omitempty"`
	Rows     int    `json:"rows,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

// Read is what the terminal printed. It returns io.EOF once the process has
// exited — ExitCode then says how — or the sandbox has hung up.
func (t *Terminal) Read(p []byte) (int, error) {
	for {
		if t.message != nil {
			n, err := t.message.Read(p)
			if errors.Is(err, io.EOF) {
				t.message = nil
				err = nil
			}
			if n > 0 || err != nil {
				return n, err
			}
			continue
		}
		kind, message, err := t.conn.Reader(t.ctx)
		if err != nil {
			// Exited, or hung up: either is the end of the screen. Any other
			// close is a terminal that went wrong.
			if status := websocket.CloseStatus(err); status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway {
				return 0, io.EOF
			}
			return 0, fmt.Errorf("reading the terminal: %w", err)
		}
		if kind == websocket.MessageBinary {
			t.message = message
			continue
		}
		var c control
		if err := json.NewDecoder(message).Decode(&c); err != nil {
			return 0, fmt.Errorf("reading the terminal: a control message that is not JSON: %w", err)
		}
		if c.Type == "exit" && c.ExitCode != nil {
			t.mu.Lock()
			t.exitCode = c.ExitCode
			t.mu.Unlock()
		}
		// Any other kind is from a newer daemon, and nothing a reader of the
		// screen acts on.
	}
}

// Write is typed into the terminal.
func (t *Terminal) Write(p []byte) (int, error) {
	if err := t.conn.Write(t.ctx, websocket.MessageBinary, p); err != nil {
		return 0, fmt.Errorf("writing to the terminal: %w", err)
	}
	return len(p), nil
}

// Resize tells the process its terminal's new size, as a window's resize does.
func (t *Terminal) Resize(cols, rows int) error {
	message, err := json.Marshal(control{Type: "resize", Cols: cols, Rows: rows})
	if err != nil {
		return err
	}
	if err := t.conn.Write(t.ctx, websocket.MessageText, message); err != nil {
		return fmt.Errorf("resizing the terminal: %w", err)
	}
	return nil
}

// ExitCode is how the process ended, once Read has returned io.EOF. false is a
// terminal that ended without saying: closed, or a sandbox that hung up.
func (t *Terminal) ExitCode() (int, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.exitCode == nil {
		return 0, false
	}
	return *t.exitCode, true
}

// Close hangs the terminal up, which sends the process SIGHUP.
func (t *Terminal) Close() error {
	err := t.conn.Close(websocket.StatusNormalClosure, "closed")
	if websocket.CloseStatus(err) != -1 || errors.Is(err, net.ErrClosed) {
		return nil // closed already, by the other end
	}
	return err
}
