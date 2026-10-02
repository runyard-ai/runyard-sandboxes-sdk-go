package main

import (
	"context"
	"crypto/subtle"
	"fmt"
	"html"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

// answer is what the person decided, as their browser brought it back.
type answer struct {
	// code is what the approval gave, to be traded for the key.
	code   string
	denied bool
}

// callback is where the console sends the browser once the person has
// decided: one path, on a port of this machine's loopback, for one answer.
type callback struct {
	// uri is the redirect_uri the console is given.
	uri     string
	state   string
	answers chan answer
	// answered is set by the first answer, which is the only one taken.
	answered atomic.Bool
	server   *http.Server
	serving  sync.WaitGroup
}

// What a browser is given to say all it has to in. It is on this machine,
// and whatever takes longer is not a browser following a redirect.
const callbackPatience = 10 * time.Second

// listen starts the callback, which answers until it is closed.
func listen(ctx context.Context, state string) (*callback, error) {
	// 127.0.0.1 and not localhost: a name is whatever the resolver says it
	// is, and this must not be reachable from another machine.
	var config net.ListenConfig
	listener, err := config.Listen(ctx, "tcp4", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("listening on this machine for the browser to come back: %w", err)
	}
	c := &callback{uri: "http://" + listener.Addr().String() + "/callback", state: state, answers: make(chan answer, 1)}
	c.server = &http.Server{
		Handler:           c,
		ReadHeaderTimeout: callbackPatience,
		ReadTimeout:       callbackPatience,
		WriteTimeout:      callbackPatience,
		MaxHeaderBytes:    1 << 16,
		// What a stranger to this port does wrong is not the person's to
		// read in the middle of logging in.
		ErrorLog: log.New(io.Discard, "", 0),
	}
	// One answer is all it is for, so no connection is kept for a second.
	c.server.SetKeepAlivesEnabled(false)
	c.serving.Go(func() { _ = c.server.Serve(listener) }) //task:unowned joined by serving.Wait, in close
	return c, nil
}

// close stops listening and hangs up on whoever is still connected. The
// browser that brought the answer has its page by then: it is sent before
// the answer is told.
func (c *callback) close() {
	_ = c.server.Close()
	c.serving.Wait()
}

func (c *callback) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	switch {
	case r.URL.Path != "/callback":
		page(w, http.StatusNotFound, "There is nothing here.")
		return
	case r.Method != http.MethodGet:
		w.Header().Set("Allow", http.MethodGet)
		page(w, http.StatusMethodNotAllowed, "There is nothing here.")
		return
	// Anything on this machine can call a loopback port, and any page in
	// the browser can send it here: the state is all that says this is the
	// answer to what ryclaude asked, so one without it changes nothing and
	// the wait goes on. Compared in constant time, since how long a guess
	// took would otherwise say how much of it was right.
	case subtle.ConstantTimeCompare([]byte(query.Get("state")), []byte(c.state)) != 1:
		page(w, http.StatusBadRequest, "This is not the answer ryclaude is waiting for.")
		return
	}
	got := answer{code: query.Get("code"), denied: query.Get("error") != ""}
	if got.code == "" && !got.denied {
		page(w, http.StatusBadRequest, "This is not the answer ryclaude is waiting for.")
		return
	}
	if !c.answered.CompareAndSwap(false, true) {
		// Not "done": if this is the person's own browser, what was taken
		// before it was somebody else's answer, and the key in the terminal
		// may be theirs.
		page(w, http.StatusConflict, "Another answer reached ryclaude before this one, and this one was not used. Go back to your terminal and check the account it says it is logged in as: if it is not yours, run ryclaude auth logout.")
		return
	}
	if got.denied {
		page(w, http.StatusOK, "Denied: ryclaude was given no key. You can close this tab.")
	} else {
		page(w, http.StatusOK, "You can close this tab and go back to your terminal.")
	}
	// Sent before the answer is told: once told, the listener is closed
	// under whatever has not left yet.
	_ = http.NewResponseController(w).Flush()
	c.answers <- got
}

// page answers a browser with a sentence. It holds nothing from the request
// and loads nothing: what is said is one of this file's own sentences.
func page(w http.ResponseWriter, status int, sentence string) {
	header := w.Header()
	header.Set("Content-Type", "text/html; charset=utf-8")
	// The address this was asked at has a code in it.
	header.Set("Cache-Control", "no-store")
	header.Set("Referrer-Policy", "no-referrer")
	header.Set("Content-Security-Policy", "default-src 'none'")
	header.Set("X-Content-Type-Options", "nosniff")
	body := "<!doctype html>\n<html lang=\"en\"><head><meta charset=\"utf-8\"><title>ryclaude</title></head>\n<body><p>" +
		html.EscapeString(sentence) + "</p></body></html>\n"
	// Its length said, so that a page flushed is a page whole: the
	// connection may be closed as soon as it has been.
	header.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(status)
	_, _ = io.WriteString(w, body)
}
