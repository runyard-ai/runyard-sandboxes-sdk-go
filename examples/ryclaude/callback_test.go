package main

import (
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

// shown is what a browser was answered: the status, the headers, and the
// page.
type shown struct {
	status int
	header http.Header
	// length is how long the answer said the page is.
	length int64
	page   string
}

// visit is a browser sent to an address.
func visit(t *testing.T, method, address string) shown {
	t.Helper()
	// No connection is kept: one left idle is a goroutine left reading it.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	req, err := http.NewRequestWithContext(t.Context(), method, address, http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, address, err)
	}
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("reading the page: %v", err)
	}
	return shown{status: res.StatusCode, header: res.Header, length: res.ContentLength, page: string(body)}
}

// answering reports whether the callback that was at an address, waiting on
// a state, is still there. Another program may have taken the port since,
// and it does not know the state: only the one that was closed would say it
// has its answer, or take one.
func answering(t *testing.T, redirect, state string) bool {
	t.Helper()
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, redirect+"?code=again&state="+url.QueryEscape(state), http.NoBody)
	if err != nil {
		t.Fatal(err)
	}
	res, err := client.Do(req)
	if err != nil {
		return false
	}
	_ = res.Body.Close()
	return res.StatusCode == http.StatusOK || res.StatusCode == http.StatusConflict
}

func listening(t *testing.T) *callback {
	t.Helper()
	c, err := listen(t.Context(), "the-state")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.close)
	return c
}

// taken is the answer the callback took. It tells it once the browser has
// its page, which is after the browser was answered: so it is waited for.
func taken(c *callback) answer { return <-c.answers }

// takes fails unless the next answer the callback takes is this code: which
// says, of everything it was sent before, that none of it was taken.
func takes(t *testing.T, c *callback, code string) {
	t.Helper()
	if got := visit(t, http.MethodGet, c.uri+"?code="+code+"&state="+url.QueryEscape(c.state)); got.status != http.StatusOK {
		t.Fatalf("the answer was answered %d %s", got.status, got.page)
	}
	if got := taken(c); got != (answer{code: code}) {
		t.Fatalf("%+v was taken, want the code %s", got, code)
	}
}

func TestTheCallbackIsOnePathOnThisMachinesLoopback(t *testing.T) {
	c := listening(t)
	back, err := url.Parse(c.uri)
	if err != nil || back.Scheme != "http" || back.Hostname() != "127.0.0.1" || back.Port() == "" || back.Port() == "0" || back.Path != "/callback" {
		t.Fatalf("it listens at %s %v", c.uri, err)
	}
	base := "http://" + back.Host
	for _, path := range []string{"/", "/callback/", "/callback/x", "/favicon.ico", "/console/authorize", "/Callback"} {
		if got := visit(t, http.MethodGet, base+path+"?code=c&state=the-state"); got.status != http.StatusNotFound {
			t.Errorf("%s was answered %d, want 404", path, got.status)
		}
	}
	for _, method := range []string{http.MethodPost, http.MethodPut, http.MethodDelete, http.MethodHead} {
		if got := visit(t, method, c.uri+"?code=c&state=the-state"); got.status != http.StatusMethodNotAllowed || got.header.Get("Allow") != http.MethodGet {
			t.Errorf("%s was answered %d, want 405", method, got.status)
		}
	}
	takes(t, c, "the-code")
}

// Anything on the machine can call the port: what does not carry the state
// is refused, and is not the answer.
func TestAnAnswerWithoutTheStateChangesNothing(t *testing.T) {
	c := listening(t)
	for name, query := range map[string]string{
		"no state":                  "code=stolen",
		"an empty state":            "code=stolen&state=",
		"another state":             "code=stolen&state=another",
		"the start of the state":    "code=stolen&state=the-",
		"the state and more":        "code=stolen&state=the-state-",
		"a denial without it":       "error=access_denied",
		"a denial with another":     "error=access_denied&state=another",
		"the state and nothing":     "state=the-state",
		"the state and empty code":  "state=the-state&code=",
		"the state and empty error": "state=the-state&error=",
	} {
		got := visit(t, http.MethodGet, c.uri+"?"+query)
		if got.status != http.StatusBadRequest || !strings.Contains(got.page, "not the answer ryclaude is waiting for") {
			t.Errorf("%s was answered %d %s, want 400", name, got.status, got.page)
		}
	}
	// And the wait is still on: the answer, when it comes, is taken.
	takes(t, c, "the-code")
}

func TestTheFirstAnswerIsTheOneTaken(t *testing.T) {
	c := listening(t)
	first := visit(t, http.MethodGet, c.uri+"?code=the-code&state=the-state")
	if first.status != http.StatusOK || !strings.Contains(first.page, "You can close this tab and go back to your terminal.") {
		t.Fatalf("answered %d %s", first.status, first.page)
	}
	// The same again, another code, and a denial: none of them replaces it,
	// and none blocks for want of somebody to take it.
	for _, query := range []string{"code=the-code&state=the-state", "code=another&state=the-state", "error=access_denied&state=the-state"} {
		if got := visit(t, http.MethodGet, c.uri+"?"+query); got.status != http.StatusConflict || !strings.Contains(got.page, "Another answer reached ryclaude before this one, and this one was not used.") ||
			!strings.Contains(got.page, "check the account it says it is logged in as") || strings.Contains(got.page, "close this tab") {
			t.Errorf("%s was answered %d %s, want 409", query, got.status, got.page)
		}
	}
	if got := taken(c); got != (answer{code: "the-code"}) {
		t.Fatalf("taken: %+v", got)
	}
}

func TestADenialIsAnAnswer(t *testing.T) {
	c := listening(t)
	denial := visit(t, http.MethodGet, c.uri+"?error=access_denied&state=the-state")
	if denial.status != http.StatusOK || !strings.Contains(denial.page, "Denied") {
		t.Fatalf("answered %d %s", denial.status, denial.page)
	}
	if got := taken(c); got != (answer{denied: true}) {
		t.Fatalf("taken: %+v", got)
	}
}

// The page is this program's own sentence: nothing the address said is in
// it, and it loads and runs nothing.
func TestThePageHoldsNothingOfWhatWasAsked(t *testing.T) {
	const script = "<script>alert(1)</script>"
	for name, address := range map[string]string{
		"taken":      "/callback?code=" + url.QueryEscape(script) + "&state=the-state&error_description=" + url.QueryEscape(script),
		"refused":    "/callback?code=x&state=" + url.QueryEscape(script),
		"not a path": "/" + url.PathEscape(script),
	} {
		c := listening(t)
		back, _ := url.Parse(c.uri)
		got := visit(t, http.MethodGet, "http://"+back.Host+address)
		body := got.page
		if strings.Contains(body, "<script") || strings.Contains(body, "alert") {
			t.Errorf("%s: the page repeats what it was asked: %s", name, body)
		}
		for _, loaded := range []string{"http://", "https://", "//", "src=", "href=", "<link", "<img", "<style", "<iframe"} {
			if strings.Contains(body, loaded) {
				t.Errorf("%s: the page has %q in it: %s", name, loaded, body)
			}
		}
		for header, want := range map[string]string{
			"Content-Type":            "text/html; charset=utf-8",
			"Cache-Control":           "no-store",
			"Referrer-Policy":         "no-referrer",
			"Content-Security-Policy": "default-src 'none'",
			"X-Content-Type-Options":  "nosniff",
		} {
			if value := got.header.Get(header); value != want {
				t.Errorf("%s: %s is %q, want %q", name, header, value, want)
			}
		}
		// Whole when it is flushed, since the listener closes behind it.
		if got.length != int64(len(body)) {
			t.Errorf("%s: the page says it is %d bytes and is %d", name, got.length, len(body))
		}
	}
}

func TestAClosedCallbackAnswersNothing(t *testing.T) {
	c, err := listen(t.Context(), "the-state")
	if err != nil {
		t.Fatal(err)
	}
	if !answering(t, c.uri, "the-state") {
		t.Fatal("it does not answer while it is open")
	}
	c.close()
	if answering(t, c.uri, "the-state") {
		t.Error("it still answers once closed")
	}
	// Closing twice is closing once.
	c.close()
	if c.server.ReadHeaderTimeout <= 0 || c.server.ReadTimeout <= 0 || c.server.WriteTimeout <= 0 {
		t.Errorf("a browser that says nothing is waited for without end: %+v", c.server)
	}
}
