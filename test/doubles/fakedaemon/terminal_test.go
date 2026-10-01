package fakedaemon

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/coder/websocket"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// ready is a daemon with one ready sandbox, and that sandbox's terminal route.
func ready(t *testing.T, opts ...Option) (*Daemon, string) {
	t.Helper()
	d := New(t, append([]Option{WithKey("k")}, opts...)...)
	request, err := http.NewRequestWithContext(t.Context(), http.MethodPost, d.URL+"/v1/sandboxes", strings.NewReader(`{"image":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer k")
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", "one")
	res, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = res.Body.Close() }()
	var created genv1.Sandbox
	if err := json.NewDecoder(res.Body).Decode(&created); err != nil {
		t.Fatal(err)
	}
	return d, d.URL + "/v1/sandboxes/" + created.Id.String() + "/terminal"
}

// dial opens a terminal, and is the status it was answered with.
func dial(t *testing.T, url string) (*websocket.Conn, int, error) {
	t.Helper()
	conn, res, err := websocket.Dial(t.Context(), url, &websocket.DialOptions{
		HTTPHeader:   http.Header{"Authorization": {"Bearer k"}},
		Subprotocols: []string{terminalProtocol},
	})
	status := 0
	if res != nil {
		status = res.StatusCode
		if res.Body != nil {
			_ = res.Body.Close()
		}
	}
	return conn, status, err
}

func TestATerminalIsRefusedWhatTheRouteRefuses(t *testing.T) {
	d, url := ready(t, WithTerminal(func(context.Context, genv1.SandboxID, *Terminal) int { return 0 }))
	for _, c := range []struct {
		name, url string
		status    int
	}{
		{"too wide", url + "?cols=1001", http.StatusBadRequest},
		{"not a number", url + "?rows=many", http.StatusBadRequest},
		{"no such sandbox", d.URL + "/v1/sandboxes/00000000-0000-0000-0000-000000000000/terminal", http.StatusNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			conn, status, err := dial(t, c.url)
			if err == nil {
				_ = conn.CloseNow()
				t.Fatal("it opened")
			}
			if status != c.status {
				t.Fatalf("%d %v", status, err)
			}
		})
	}
}

func TestATerminalIsAWebSocket(t *testing.T) {
	_, url := ready(t, WithTerminal(func(context.Context, genv1.SandboxID, *Terminal) int { return 0 }))
	request, err := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Authorization", "Bearer k")
	res, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	_ = res.Body.Close()
	if res.StatusCode != http.StatusBadRequest {
		t.Fatalf("%d", res.StatusCode)
	}
}

// A terminal still open when the test ends is hung up by its daemon, and the
// function running in it is told.
func TestATerminalLeftOpenIsHungUpAtTheEnd(t *testing.T) {
	hungUp := make(chan struct{})
	t.Run("leaves it open", func(t *testing.T) {
		_, url := ready(t, WithTerminal(func(ctx context.Context, _ genv1.SandboxID, _ *Terminal) int {
			<-ctx.Done()
			close(hungUp)
			return 0
		}))
		conn, _, err := dial(t, url)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = conn.CloseNow() })
	})
	<-hungUp
}

// Resizes nobody takes are kept up to a point, and then dropped rather than
// holding the terminal.
func TestResizesNobodyTakesAreDropped(t *testing.T) {
	sizes := make(chan int, 1)
	_, url := ready(t, WithTerminal(func(_ context.Context, _ genv1.SandboxID, tty *Terminal) int {
		buf := make([]byte, 1)
		_, _ = tty.In.Read(buf) // the keystroke typed after every resize
		sizes <- len(tty.Resizes)
		return 0
	}))
	conn, _, err := dial(t, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.CloseNow() }()
	for range 100 {
		if err := conn.Write(t.Context(), websocket.MessageText, []byte(`{"type":"resize","cols":10,"rows":10}`)); err != nil {
			t.Fatal(err)
		}
	}
	_ = conn.Write(t.Context(), websocket.MessageText, []byte(`not a resize`))
	if err := conn.Write(t.Context(), websocket.MessageBinary, []byte("x")); err != nil {
		t.Fatal(err)
	}
	if kept := <-sizes; kept != 64 {
		t.Errorf("%d kept", kept)
	}
}
