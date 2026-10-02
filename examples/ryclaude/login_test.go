package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakedaemon"
)

// signingIn is a daemon whose console signs people in, and so one a key can
// be approved at.
func signingIn(t *testing.T) *fakedaemon.Daemon {
	t.Helper()
	return fakedaemon.New(t, fakedaemon.WithKey("admin"), fakedaemon.WithGoogle())
}

// person is somebody signed in to a daemon's console, and their browser:
// opened on the page ryclaude names, they read what is asked, approve it,
// and the browser goes where the page sends it with the code.
type person struct {
	t     *testing.T
	d     *fakedaemon.Daemon
	owner string
	// expires is when the key they approve does: a week from now, unless
	// they chose otherwise on the page.
	expires time.Time

	// opened is how often a browser was, and asked what the page was asked
	// the last time.
	opened                int
	asked                 url.Values
	redirect, code, state string
	// instead is what happens in place of the browser following the
	// redirect with the code.
	instead func(p *person)
	// fails is what opening the browser fails with, the person having
	// opened the address themselves.
	fails error
}

func approving(t *testing.T, d *fakedaemon.Daemon) *person {
	t.Helper()
	return &person{t: t, d: d, owner: "ada@example.com", expires: time.Now().Add(7 * 24 * time.Hour).UTC().Truncate(time.Second)}
}

// at is a machine this person's browser is on.
func (p *person) at(con *fakeConsole) machine {
	m := at(con)
	m.browse = p.browse
	return m
}

func (p *person) browse(_ context.Context, address string) error {
	p.opened++
	p.approve(address)
	if p.instead != nil {
		p.instead(p)
	} else {
		p.follow(http.StatusOK, "code="+url.QueryEscape(p.code)+"&state="+url.QueryEscape(p.state))
	}
	return p.fails
}

// approve reads the page's address as the console's page does, and approves
// what it asks as the person signed in: the daemon's half of the page.
func (p *person) approve(address string) {
	p.t.Helper()
	page, err := url.Parse(address)
	if err != nil || page.Scheme+"://"+page.Host != p.d.URL || page.Path != "/console/authorize" || page.Fragment != "" {
		p.t.Fatalf("the browser was sent to %s (%v), which is not the page of %s that approves a key", address, err, p.d.URL)
	}
	p.asked = page.Query()
	var scopes []genv1.Scope
	for scope := range strings.SplitSeq(p.asked.Get("scopes"), ",") {
		scopes = append(scopes, genv1.Scope(scope))
	}
	reach := genv1.KeyReach(p.asked.Get("sandboxes"))
	request := genv1.KeyAuthorizationRequest{
		Name: p.asked.Get("client"), Scopes: scopes, Sandboxes: &reach, ExpiresAt: p.expires, CodeChallenge: p.asked.Get("code_challenge"),
	}
	p.redirect, p.state = p.asked.Get("redirect_uri"), p.asked.Get("state")
	if p.redirect != "" {
		request.RedirectUri = &p.redirect
	}
	p.code = p.d.Approve(p.owner, request).Code
}

// follow is the browser at ryclaude's callback, and the page it was shown.
func (p *person) follow(status int, query string) string {
	p.t.Helper()
	got := visit(p.t, http.MethodGet, p.redirect+"?"+query)
	if got.status != status {
		p.t.Fatalf("the callback answered %s with %d %s, want %d", query, got.status, got.page, status)
	}
	return got.page
}

// keyboard is what a person types, some time after they are asked: typing
// goes in one end, and the terminal reads the other.
func keyboard(t *testing.T) (typed io.Reader, typing *os.File) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = w.Close()
		_ = r.Close()
	})
	return r, w
}

// hearing is what ryclaude says, and somebody acting on it as it is said.
type hearing struct {
	bytes.Buffer
	said func(string)
}

func (h *hearing) Write(p []byte) (int, error) {
	if h.said != nil {
		h.said(string(p))
	}
	return h.Buffer.Write(p)
}

var authorizePage = regexp.MustCompile(`http\S+/console/authorize\S+`)

// redeemed records what each redeem presented, and lets it through.
type redeemed struct {
	presented []genv1.KeyAuthorizationRedemption
	// during is what the daemon does while it is asked, before it answers.
	during func()
}

func (r *redeemed) watch(d *fakedaemon.Daemon) {
	d.InterceptAll("redeemKeyAuthorization", func(w http.ResponseWriter, req *http.Request, serve http.HandlerFunc) {
		body, _ := io.ReadAll(req.Body)
		var presented genv1.KeyAuthorizationRedemption
		_ = json.Unmarshal(body, &presented)
		r.presented = append(r.presented, presented)
		if r.during != nil {
			r.during()
		}
		req.Body = io.NopCloser(bytes.NewReader(body))
		serve(w, req)
	})
}

// kept is the key ryclaude holds, which there must be.
func kept(t *testing.T) grant {
	t.Helper()
	path, err := grantPath()
	if err != nil {
		t.Fatal(err)
	}
	g, found, err := loadGrant(path)
	if err != nil || !found {
		t.Fatalf("no key was kept: %v %v", found, err)
	}
	return g
}

// nothingKept fails when a key, or anything else, is where one is kept.
func nothingKept(t *testing.T) {
	t.Helper()
	path, err := grantPath()
	if err != nil {
		t.Fatal(err)
	}
	if entries, err := os.ReadDir(filepath.Dir(path)); err == nil && len(entries) > 0 {
		t.Errorf("%d files were kept, and nothing was to be", len(entries))
	}
}

// logIn is `ryclaude auth login` at a daemon, approved, and the key it kept.
func logIn(t *testing.T, d *fakedaemon.Daemon) grant {
	t.Helper()
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, approving(t, d).at(typing("")), &stderr); code != 0 {
		t.Fatalf("logging in: exit %d: %s", code, stderr.String())
	}
	return kept(t)
}

var base64url43 = regexp.MustCompile(`^[A-Za-z0-9_-]{43}$`)

func TestLoggingInIsApprovingInTheBrowserAndTheKeyIsKept(t *testing.T) {
	path := configHome(t)
	d := signingIn(t)
	p := approving(t, d)
	// The callback has its answer by the time the key is asked for, and is
	// closed by then: not left open for as long as the daemon takes.
	var listeningStill atomic.Bool
	seen := redeemed{during: func() { listeningStill.Store(answering(t, p.redirect, p.state)) }}
	seen.watch(d)
	var page string
	p.instead = func(p *person) {
		page = p.follow(http.StatusOK, "code="+url.QueryEscape(p.code)+"&state="+url.QueryEscape(p.state))
	}
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"auth", "login", "-url", d.URL + "/"}, p.at(typing("")), &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}

	// What the page was asked, and nothing else.
	if p.opened != 1 {
		t.Fatalf("a browser was opened %d times", p.opened)
	}
	for name, values := range p.asked {
		if len(values) != 1 {
			t.Errorf("%s was said %d times", name, len(values))
		}
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for name := range p.asked {
			if !yield(name) {
				return
			}
		}
	})
	if want := []string{"client", "code_challenge", "redirect_uri", "sandboxes", "scopes", "state"}; !slices.Equal(names, want) {
		t.Errorf("the page was asked %v, want %v", names, want)
	}
	if got := p.asked.Get("client"); got != "ryclaude on laptop" {
		t.Errorf("client %q", got)
	}
	// The four ryclaude uses, and never one more.
	if got := p.asked.Get("scopes"); got != "sandboxes.read,sandboxes.write,files.write,exec" {
		t.Errorf("scopes %q", got)
	}
	if got := p.asked.Get("sandboxes"); got != "own" {
		t.Errorf("sandboxes %q", got)
	}
	if !regexp.MustCompile(`^http://127\.0\.0\.1:[1-9]\d*/callback$`).MatchString(p.redirect) {
		t.Errorf("redirect_uri %q", p.redirect)
	}
	challenge := p.asked.Get("code_challenge")
	if !base64url43.MatchString(challenge) || !base64url43.MatchString(p.state) || p.state == challenge {
		t.Errorf("code_challenge %q, state %q", challenge, p.state)
	}
	if !strings.Contains(page, "You can close this tab and go back to your terminal.") {
		t.Errorf("the browser was shown %q", page)
	}

	// The verifier the redeem presented is the one the challenge was made
	// of, 32 random bytes of it, and was shown to nobody before.
	if len(seen.presented) != 1 || seen.presented[0].Code != p.code {
		t.Fatalf("redeemed: %d times", len(seen.presented))
	}
	verifier := seen.presented[0].CodeVerifier
	sum := sha256.Sum256([]byte(verifier))
	if raw, err := base64.RawURLEncoding.DecodeString(verifier); err != nil || len(raw) != 32 || base64.RawURLEncoding.EncodeToString(sum[:]) != challenge {
		t.Errorf("the verifier is not 32 bytes whose SHA-256 is the challenge: %v", err)
	}

	// The key is the approver's, for this machine, on its own sandboxes.
	keys := d.Keys()
	if len(keys) != 1 || keys[0].Owner != "ada@example.com" || keys[0].Name != "ryclaude on laptop" || keys[0].Sandboxes != genv1.KeyReachOwn ||
		!slices.Equal(keys[0].Scopes, []genv1.Scope{genv1.SandboxesRead, genv1.SandboxesWrite, genv1.FilesWrite, genv1.Exec}) {
		t.Fatalf("the daemon's keys: %+v", keys)
	}
	g := kept(t)
	if g.URL != d.URL || g.KeyID != keys[0].Id || !strings.HasPrefix(g.Key, "rysk_"+g.KeyID+"_") || g.Name != "ryclaude on laptop" ||
		g.Owner != "ada@example.com" || !g.ExpiresAt.Equal(p.expires) {
		t.Errorf("kept %+v", g)
	}
	if mode := modeOf(t, path); mode != 0o600 {
		t.Errorf("the file is %04o", mode)
	}
	if mode := modeOf(t, filepath.Dir(path)); mode != 0o700 {
		t.Errorf("the directory is %04o", mode)
	}

	// Who the key belongs to, what it is called and when it ends are said;
	// the key, the code and the verifier never are.
	said := stderr.String()
	for _, want := range []string{authorizePage.FindString(said), "ada@example.com", `"ryclaude on laptop"`, g.KeyID, when(p.expires), "ryclaude: logged in as ada@example.com, at " + d.URL + ", with the key"} {
		if want == "" || !strings.Contains(said, want) {
			t.Errorf("it did not say %q: %s", want, said)
		}
	}
	for name, secret := range map[string]string{"key": g.Key, "code": p.code, "verifier": verifier} {
		if strings.Contains(said, secret) {
			t.Errorf("the %s is in what was said: %s", name, said)
		}
	}
	if listeningStill.Load() || answering(t, p.redirect, p.state) {
		t.Error("the callback still answers once it has its answer")
	}
	if d.Calls("revokeKey") != 0 {
		t.Error("a key was revoked, and there was none before this one")
	}
}

// Two logins share nothing a third party could have kept from the first.
func TestEveryLoginAsksWithItsOwnStateAndChallenge(t *testing.T) {
	configHome(t)
	d := signingIn(t)
	first, second := approving(t, d), approving(t, d)
	for _, p := range []*person{first, second} {
		if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, p.at(typing("")), io.Discard); code != 0 {
			t.Fatalf("exit %d", code)
		}
	}
	if first.state == second.state || first.asked.Get("code_challenge") == second.asked.Get("code_challenge") {
		t.Errorf("the second login asked as the first did")
	}
}

func TestWhatTheBrowserBringsBack(t *testing.T) {
	answer := func(p *person) string {
		return "code=" + url.QueryEscape(p.code) + "&state=" + url.QueryEscape(p.state)
	}
	for _, c := range []struct {
		name    string
		browser func(p *person)
		exit    int
		said    string
	}{
		{"a denial", func(p *person) {
			if page := p.follow(http.StatusOK, "error=access_denied&state="+url.QueryEscape(p.state)); !strings.Contains(page, "Denied") {
				p.t.Errorf("the browser was shown %q", page)
			}
		}, 1, "the key was denied in the browser"},
		// Anything on the machine can call the port. What does not know the
		// state is refused, and the wait goes on to the real answer.
		{"a stranger's answers and then the browser's", func(p *person) {
			p.follow(http.StatusBadRequest, "code=stolen&state=guessed")
			p.follow(http.StatusBadRequest, "code=stolen")
			p.follow(http.StatusBadRequest, "error=access_denied&state=guessed")
			p.follow(http.StatusBadRequest, "state="+url.QueryEscape(p.state))
			p.follow(http.StatusOK, answer(p))
		}, 0, "logged in as ada@example.com"},
		{"the answer twice", func(p *person) {
			p.follow(http.StatusOK, answer(p))
			p.follow(http.StatusConflict, answer(p))
		}, 0, "logged in as ada@example.com"},
		// A denial after the approval is not the answer: the first was.
		{"the answer and then a denial", func(p *person) {
			p.follow(http.StatusOK, answer(p))
			p.follow(http.StatusConflict, "error=access_denied&state="+url.QueryEscape(p.state))
		}, 0, "logged in as ada@example.com"},
		{"a page that is not the callback, first", func(p *person) {
			for _, path := range []string{"/", "/favicon.ico", "/callback/more"} {
				if got := visit(p.t, http.MethodGet, strings.TrimSuffix(p.redirect, "/callback")+path+"?"+answer(p)); got.status != http.StatusNotFound {
					p.t.Errorf("%s was answered %d", path, got.status)
				}
			}
			p.follow(http.StatusOK, answer(p))
		}, 0, "logged in as ada@example.com"},
		// The code of an approval made for another verifier: somebody who
		// knows the state still cannot have their own key kept here.
		{"a code approved for somebody else's verifier", func(p *person) {
			request := genv1.KeyAuthorizationRequest{Name: "theirs", ExpiresAt: p.expires, CodeChallenge: "E9Melhoa2OwvFrEMTJguCHaoeK1t8URWbuGJSstw-cM"}
			p.follow(http.StatusOK, "code="+url.QueryEscape(p.d.Approve("mallory@example.com", request).Code)+"&state="+url.QueryEscape(p.state))
		}, 1, "the approval was refused or has expired; run `ryclaude auth login` again"},
		{"a code the daemon never gave", func(p *person) {
			p.follow(http.StatusOK, "code=made-up&state="+url.QueryEscape(p.state))
		}, 1, "the approval was refused or has expired; run `ryclaude auth login` again"},
	} {
		t.Run(c.name, func(t *testing.T) {
			configHome(t)
			d := signingIn(t)
			p := approving(t, d)
			p.instead = c.browser
			var stderr bytes.Buffer
			if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, p.at(typing("")), &stderr); code != c.exit || !strings.Contains(stderr.String(), c.said) {
				t.Fatalf("exit %d, want %d and %q: %s", code, c.exit, c.said, stderr.String())
			}
			if answering(t, p.redirect, p.state) {
				t.Error("the callback still answers once the login is over")
			}
			if c.exit != 0 {
				nothingKept(t)
				if keys := d.Keys(); len(keys) != 0 {
					t.Errorf("the daemon minted %+v", keys)
				}
				return
			}
			if g, keys := kept(t), d.Keys(); len(keys) != 1 || keys[0].Id != g.KeyID || keys[0].Owner != "ada@example.com" || d.Calls("redeemKeyAuthorization") != 1 {
				t.Errorf("kept %s, of the daemon's %+v", g.KeyID, keys)
			}
		})
	}
}

// With no browser here the page is given nowhere to come back to: it shows
// the code, the person pastes it, and nothing listens for anything.
func TestWithoutABrowserTheCodeIsPasted(t *testing.T) {
	for name, typed := range map[string]func(code string) string{
		"the code":                     func(code string) string { return code + "\n" },
		"the code among what was not":  func(code string) string { return "\n   \n\t" + code + "  \r\nand more\n" },
		"the code and no end of line":  func(code string) string { return code },
		"another code, after the code": func(code string) string { return code + "\nmade-up\n" },
	} {
		t.Run(name, func(t *testing.T) {
			configHome(t)
			d := signingIn(t)
			p := approving(t, d)
			in, out := keyboard(t)
			con := typing("")
			con.in = in
			stderr := &hearing{}
			stderr.said = func(said string) {
				if address := authorizePage.FindString(said); address != "" {
					p.approve(address)
					if _, err := out.WriteString(typed(p.code)); err != nil {
						t.Fatal(err)
					}
					// The end of what is typed, for the code with no end
					// of line.
					_ = out.Close()
				}
			}
			if code := run(t.Context(), []string{"auth", "login", "-no-browser", "-url", d.URL}, p.at(con), stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			if p.opened != 0 {
				t.Error("a browser was opened")
			}
			if _, given := p.asked["redirect_uri"]; given || len(p.asked) != 5 {
				t.Errorf("the page was asked %v, want no redirect_uri", p.asked)
			}
			if p.asked.Get("scopes") != "sandboxes.read,sandboxes.write,files.write,exec" || p.asked.Get("sandboxes") != "own" ||
				p.asked.Get("client") != "ryclaude on laptop" || !base64url43.MatchString(p.state) {
				t.Errorf("the page was asked %v", p.asked)
			}
			if g, keys := kept(t), d.Keys(); len(keys) != 1 || keys[0].Id != g.KeyID {
				t.Errorf("kept %s, of the daemon's %+v", g.KeyID, keys)
			}
			if said := stderr.String(); !strings.Contains(said, "paste here the code it shows") || strings.Contains(said, p.code) {
				t.Errorf("said: %s", said)
			}
		})
	}
}

func TestWithoutABrowserWhatIsPastedMustBeTheCode(t *testing.T) {
	for _, c := range []struct{ name, typed, said string }{
		{"nothing, and the end of input", "", "no code was pasted: standard input ended"},
		{"empty lines, and the end of input", "\n\n  \n", "no code was pasted: standard input ended"},
		{"something that is not the code", "made-up\n", "the approval was refused or has expired; run `ryclaude auth login` again"},
	} {
		t.Run(c.name, func(t *testing.T) {
			configHome(t)
			d := signingIn(t)
			var stderr bytes.Buffer
			if code := run(t.Context(), []string{"auth", "login", "-url", d.URL, "-no-browser"}, approving(t, d).at(typing(c.typed)), &stderr); code != 1 || !strings.Contains(stderr.String(), c.said) {
				t.Fatalf("exit %d, want 1 and %q: %s", code, c.said, stderr.String())
			}
			nothingKept(t)
		})
	}
}

// A browser on another machine cannot follow a redirect to this one: at a
// terminal the code is pasted, and the callback is closed all the same.
func TestAtATerminalTheCodeMayBePastedInstead(t *testing.T) {
	configHome(t)
	d := signingIn(t)
	p := approving(t, d)
	in, out := keyboard(t)
	con := typing("")
	con.in, con.typedAt = in, true
	p.instead = func(p *person) {
		if _, err := out.WriteString(p.code + "\n"); err != nil {
			t.Fatal(err)
		}
	}
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, p.at(con), &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if p.redirect == "" || answering(t, p.redirect, p.state) {
		t.Errorf("the callback at %q still answers", p.redirect)
	}
	if g, keys := kept(t), d.Keys(); len(keys) != 1 || keys[0].Id != g.KeyID {
		t.Errorf("kept %s, of the daemon's %+v", g.KeyID, keys)
	}
	if !strings.Contains(stderr.String(), "paste the code") {
		t.Errorf("it did not say a code may be pasted: %s", stderr.String())
	}
}

// What is not a terminal is not read: a login in a script waits for the
// browser, and a line on its input is not a code.
func TestWhatIsNotATerminalIsNotReadForACode(t *testing.T) {
	configHome(t)
	d := signingIn(t)
	p := approving(t, d)
	con := typing("made-up\n")
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, p.at(con), &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if left, _ := io.ReadAll(con.in); string(left) != "made-up\n" {
		t.Errorf("its input was read: %q is left", left)
	}
	if strings.Contains(stderr.String(), "paste") {
		t.Errorf("it asked for a code to be pasted: %s", stderr.String())
	}
}

// A browser that cannot be opened is not the end: the address was said, and
// the person opens it.
func TestABrowserThatDoesNotOpenIsSaidAndTheLoginGoesOn(t *testing.T) {
	configHome(t)
	d := signingIn(t)
	p := approving(t, d)
	p.fails = errors.New("exec: \"xdg-open\": executable file not found in $PATH")
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, p.at(typing("")), &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	said := stderr.String()
	if !strings.Contains(said, "no browser was opened") || !strings.Contains(said, "executable file not found") || !strings.Contains(said, "open that address yourself") {
		t.Errorf("said: %s", said)
	}
	if address := authorizePage.FindString(said); !strings.HasPrefix(address, d.URL+"/console/authorize?") {
		t.Errorf("the address was not said: %s", said)
	}
	kept(t)
}

// The daemon takes a name of text, of at most 128 characters: what a
// machine is called is made to fit, rather than a login refused for it.
func TestWhatIsAskingIsNamedInTextThatFits(t *testing.T) {
	long := strings.Repeat("a", 200)
	for name, c := range map[string]struct{ hostname, asker string }{
		"a name":                        {"laptop", "ryclaude on laptop"},
		"a name of another script":      {"zoë’s-ラップトップ", "ryclaude on zoë’s-ラップトップ"},
		"a name with spaces within":     {"ada's laptop", "ryclaude on ada's laptop"},
		"no name":                       {"", "ryclaude"},
		"spaces":                        {" \t \n", "ryclaude"},
		"control characters alone":      {"\x1b\x00\x07\r\n", "ryclaude"},
		"format characters alone":       {"\u202e\u200b\ufeff", "ryclaude"},
		"bytes that are no rune":        {"\xff\xfe", "ryclaude"},
		"an escape sequence in it":      {"lap\x1b[2Jtop", "ryclaude on lap[2Jtop"},
		"a line break after it":         {"laptop\n", "ryclaude on laptop"},
		"a bidi override in it":         {"lap\u202etop", "ryclaude on laptop"},
		"a zero-width joiner in it":     {"lap\u200dtop", "ryclaude on laptop"},
		"controls around spaces":        {"\x00 laptop \x00", "ryclaude on laptop"},
		"as long as fits":               {long[:116], "ryclaude on " + long[:116]},
		"one more than fits":            {long[:117], "ryclaude on " + long[:116]},
		"far more than fits":            {long, "ryclaude on " + long[:116]},
		"more than fits, of wide runes": {strings.Repeat("鍵", 200), "ryclaude on " + strings.Repeat("鍵", 116)},
		"cut at a space":                {long[:115] + " b", "ryclaude on " + long[:115]},
	} {
		got := asker(func() (string, error) { return c.hostname, nil })
		if got != c.asker {
			t.Errorf("%s: asking as %q, want %q", name, got, c.asker)
		}
		if n := len([]rune(got)); n > 128 || !allText(got) || strings.TrimSpace(got) != got || got == "" {
			t.Errorf("%s: %q is %d characters, and the daemon takes text of at most 128", name, got, n)
		}
	}
	// A name that could not be read is no name, whatever came with it.
	if got := asker(func() (string, error) { return "half", errors.New("interrupted") }); got != "ryclaude" {
		t.Errorf("asking as %q", got)
	}
}

// What the page is asked, and what the key is then called, is the name made
// to fit: end to end, with a machine named to break both.
func TestAMachineNamedToBreakThePageIsAskedForInText(t *testing.T) {
	configHome(t)
	d := signingIn(t)
	p := approving(t, d)
	m := p.at(typing(""))
	m.hostname = func() (string, error) { return "lap\x1b[2Jtop\u202e\r\n" + strings.Repeat("x", 200), nil }
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, m, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	client := p.asked.Get("client")
	if len([]rune(client)) != 128 || !allText(client) || !strings.HasPrefix(client, "ryclaude on lap[2Jtopxxx") {
		t.Errorf("client %q", client)
	}
	if g := kept(t); g.Name != client {
		t.Errorf("the key is named %q", g.Name)
	}
	onlyText(t, stderr.String())
}

// A daemon names the key and says whose it is, and may be lying about what
// it is: nothing it says reaches the terminal as anything but text, and a
// key named in what is not text is not kept, and is handed back.
func TestWhatADaemonCallsTheKeyIsNotTrusted(t *testing.T) {
	for name, c := range map[string]struct{ field, value string }{
		"an escape sequence in the name":  {"name", "ryclaude\x1b[2J\x1b[H"},
		"a carriage return in the name":   {"name", "theirs\rryclaude on laptop"},
		"a bidi override in the name":     {"name", "ryclaude\u202epotpal no"},
		"a line break in the name":        {"name", "ryclaude\nryclaude: logged in as ada@example.com"},
		"an escape sequence in the owner": {"owner", "ada\x1b[2J@example.com"},
		"a carriage return in the owner":  {"owner", "mallory@example.com\rada@example.com"},
	} {
		t.Run(name, func(t *testing.T) {
			configHome(t)
			d := signingIn(t)
			// Minted as the daemon mints, and answered with one field of
			// the daemon's own choosing.
			d.Intercept("redeemKeyAuthorization", func(w http.ResponseWriter, r *http.Request, serve http.HandlerFunc) {
				minted := httptest.NewRecorder()
				serve(minted, r)
				var key map[string]any
				if err := json.Unmarshal(minted.Body.Bytes(), &key); err != nil {
					t.Errorf("the daemon minted %s: %v", minted.Body, err)
				}
				key[c.field] = c.value
				body, _ := json.Marshal(key)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write(body)
			})
			var stderr bytes.Buffer
			if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, approving(t, d).at(typing("")), &stderr); code != 1 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			said := stderr.String()
			onlyText(t, said)
			if strings.Contains(said, "logged in as") || strings.Contains(said, "rysk_") {
				t.Errorf("said: %s", said)
			}
			nothingKept(t)
			// An owner that is no address is refused where the answer is
			// read, before there is a key to hand back.
			if c.field == "owner" {
				return
			}
			if !strings.Contains(said, "a name, an owner or an id that is not text; the key that was made has been revoked") {
				t.Errorf("it did not say the key was handed back: %s", said)
			}
			if keys := d.Keys(); len(keys) != 0 {
				t.Errorf("the daemon still has %+v", keys)
			}
		})
	}
}

// What a daemon says of why it refused is said as text too.
func TestWhatADaemonSaysWentWrongIsNotTrusted(t *testing.T) {
	const hostile = "the database\x1b[2J\x1b]0;owned\x07 is\rnot\u202e answering"
	const shown = "the database\ufffd[2J\ufffd]0;owned\ufffd is\ufffdnot\ufffd answering"
	refusing := fakedaemon.Refuse(http.StatusServiceUnavailable, genv1.ErrorCodeUpstreamError, hostile)

	t.Run("logging in", func(t *testing.T) {
		for _, operation := range []string{"getAuth", "redeemKeyAuthorization"} {
			configHome(t)
			d := signingIn(t)
			d.InterceptAll(operation, refusing)
			var stderr bytes.Buffer
			if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, approving(t, d).at(typing("")), &stderr); code != 1 || !strings.Contains(stderr.String(), shown) {
				t.Fatalf("%s: exit %d: %q", operation, code, stderr.String())
			}
			onlyText(t, stderr.String())
		}
	})
	t.Run("logging in again, and out", func(t *testing.T) {
		configHome(t)
		d := signingIn(t)
		logIn(t, d)
		d.InterceptAll("revokeKey", refusing)
		var stderr bytes.Buffer
		if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, approving(t, d).at(typing("")), &stderr); code != 0 || !strings.Contains(stderr.String(), shown) {
			t.Fatalf("exit %d: %q", code, stderr.String())
		}
		onlyText(t, stderr.String())
		for _, args := range [][]string{{"logout"}, {"logout", "-forget"}} {
			_, stdout, said := auth(t, args...)
			if !strings.Contains(said, shown) {
				t.Errorf("%v: %q", args, said)
			}
			onlyText(t, stdout+said)
		}
	})
	t.Run("asking for the status", func(t *testing.T) {
		configHome(t)
		d := signingIn(t)
		logIn(t, d)
		d.InterceptAll("getMe", refusing)
		code, stdout, said := auth(t, "status")
		if code != 1 || !strings.Contains(stdout, "status:  unknown: runyard-sandboxes: upstream_error: "+shown+"\n") {
			t.Fatalf("exit %d: %q%q", code, stdout, said)
		}
		onlyText(t, stdout+said)
	})
	t.Run("making a sandbox", func(t *testing.T) {
		d, _ := daemon(t, 0)
		d.Intercept("createSandbox", fakedaemon.Refuse(http.StatusBadRequest, genv1.ErrorCodeImageUnknown, hostile))
		code, said := ryclaude(t, "-addr", d.URL, "-key", "k")
		if code != 1 || !strings.Contains(said, shown) {
			t.Fatalf("exit %d: %q", code, said)
		}
		onlyText(t, said)
	})
}

func TestTheKeyIsNamedForTheMachineWhenItHasAName(t *testing.T) {
	for name, hostname := range map[string]func() (string, error){
		"no name can be read": func() (string, error) { return "", errors.New("sethostname: not permitted") },
		"the name is empty":   func() (string, error) { return "", nil },
	} {
		t.Run(name, func(t *testing.T) {
			configHome(t)
			d := signingIn(t)
			p := approving(t, d)
			m := p.at(typing(""))
			m.hostname = hostname
			if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, m, io.Discard); code != 0 {
				t.Fatalf("exit %d", code)
			}
			if got := p.asked.Get("client"); got != "ryclaude" {
				t.Errorf("client %q, want ryclaude alone", got)
			}
			if g := kept(t); g.Name != "ryclaude" {
				t.Errorf("the key is named %q", g.Name)
			}
		})
	}
}

// A login that is interrupted leaves nothing: no key, and nothing listening.
func TestALoginInterruptedLeavesNothing(t *testing.T) {
	configHome(t)
	d := signingIn(t)
	p := approving(t, d)
	ctx, interrupt := context.WithCancel(t.Context())
	defer interrupt()
	// The person approved, and Ctrl-C came before their browser did.
	p.instead = func(*person) { interrupt() }
	in, _ := keyboard(t)
	con := typing("")
	con.in, con.typedAt = in, true
	var stderr bytes.Buffer
	if code := run(ctx, []string{"auth", "login", "-url", d.URL}, p.at(con), &stderr); code != 1 || !strings.Contains(stderr.String(), "interrupted before anything was approved") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if answering(t, p.redirect, p.state) {
		t.Error("the callback still answers once the login is over")
	}
	nothingKept(t)
	if d.Calls("redeemKeyAuthorization") != 0 || len(d.Keys()) != 0 {
		t.Error("a key was collected")
	}
}

func TestALoginAlreadyInterruptedAsksNobody(t *testing.T) {
	configHome(t)
	d := signingIn(t)
	p := approving(t, d)
	ctx, interrupt := context.WithCancel(t.Context())
	interrupt()
	if code := run(ctx, []string{"auth", "login", "-url", d.URL}, p.at(typing("")), io.Discard); code != 1 {
		t.Fatalf("exit %d", code)
	}
	if p.opened != 0 {
		t.Error("a browser was opened")
	}
	nothingKept(t)
}

// The wait is as long as a code lasts, and is over then: in a bubble, where
// five minutes pass when nothing else can happen.
func TestAnApprovalIsWaitedForAsLongAsItsCodeLasts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		code, err := await(t.Context(), make(chan answer), make(chan string))
		if code != "" || err == nil || !strings.Contains(err.Error(), "nothing was approved within 5m0s") || !strings.Contains(err.Error(), "run `ryclaude auth login` again") {
			t.Errorf("%q %v", code, err)
		}
		if waited := time.Since(start); waited != 5*time.Minute {
			t.Errorf("waited %s, want the five minutes a code lasts", waited)
		}
	})
	// Input that ends is not the browser giving up: it may still come back,
	// and is waited for as long.
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		ended := make(chan string)
		close(ended)
		if _, err := await(t.Context(), make(chan answer), ended); err == nil || !strings.Contains(err.Error(), "nothing was approved within") || time.Since(start) != approveWithin {
			t.Errorf("%v, after %s", err, time.Since(start))
		}
	})
	// With no way for a code to come at all, the wait is no shorter.
	synctest.Test(t, func(t *testing.T) {
		start := time.Now()
		if _, err := await(t.Context(), nil, nil); err == nil || time.Since(start) != approveWithin {
			t.Errorf("%v, after %s", err, time.Since(start))
		}
	})
}

func TestTheWaitEndsWithWhateverComesFirst(t *testing.T) {
	ready := func(got answer) <-chan answer {
		answers := make(chan answer, 1)
		answers <- got
		return answers
	}
	typed := func(code string) <-chan string {
		pasted := make(chan string, 1)
		pasted <- code
		return pasted
	}
	ended := make(chan string)
	close(ended)
	interrupted, interrupt := context.WithCancel(t.Context())
	interrupt()
	for _, c := range []struct {
		name    string
		ctx     context.Context
		answers <-chan answer
		pasted  <-chan string
		code    string
		said    string
	}{
		{"the browser's code", t.Context(), ready(answer{code: "from-the-browser"}), nil, "from-the-browser", ""},
		{"the browser's code, the input having ended", t.Context(), ready(answer{code: "from-the-browser"}), ended, "from-the-browser", ""},
		{"a denial", t.Context(), ready(answer{denied: true}), nil, "", "the key was denied in the browser"},
		{"a pasted code", t.Context(), nil, typed("pasted"), "pasted", ""},
		{"a pasted code, the browser being waited for", t.Context(), make(chan answer), typed("pasted"), "pasted", ""},
		{"the input ending with no browser to wait for", t.Context(), nil, ended, "", "no code was pasted"},
		{"an interruption", interrupted, make(chan answer), make(chan string), "", "interrupted before anything was approved: no key was made"},
	} {
		t.Run(c.name, func(t *testing.T) {
			code, err := await(c.ctx, c.answers, c.pasted)
			if code != c.code || (c.said == "") != (err == nil) || (err != nil && !strings.Contains(err.Error(), c.said)) {
				t.Fatalf("%q %v, want %q %q", code, err, c.code, c.said)
			}
		})
	}
}

func TestADaemonNobodySignsInToGivesNoKeyThisWay(t *testing.T) {
	configHome(t)
	d := fakedaemon.New(t, fakedaemon.WithKey("admin"))
	p := approving(t, d)
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, p.at(typing("")), &stderr); code != 1 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	for _, want := range []string{"signs nobody in", "runyard-sandboxes keys mint", "-key or RUNYARD_SANDBOXES_KEY"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("it did not say %q: %s", want, stderr.String())
		}
	}
	if p.opened != 0 {
		t.Error("a browser was opened")
	}
	nothingKept(t)
}

func TestADaemonThatDoesNotAnswerTheLogin(t *testing.T) {
	// Elsewhere: where a daemon that redirects would send what it was sent.
	var elsewhere int
	other := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { elsewhere++ }))
	t.Cleanup(other.Close)
	redirect := func(w http.ResponseWriter, r *http.Request, _ http.HandlerFunc) {
		http.Redirect(w, r, other.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}
	for _, c := range []struct {
		name      string
		operation string
		answer    fakedaemon.Interceptor
		said      string
		opened    int
	}{
		{"hangs up when asked how people sign in", "getAuth", fakedaemon.HangUp(), "asking DAEMON how people sign in: ", 0},
		{"is behind a proxy with nothing behind it", "getAuth", fakedaemon.Answer(http.StatusBadGateway, "text/html", "<h1>Bad gateway</h1>"), "it answered HTTP 502, which is not what a runyard-sandboxes daemon answers", 0},
		{"is not a daemon", "getAuth", fakedaemon.Answer(http.StatusOK, "text/html", "<h1>Welcome</h1>"), "it answered HTTP 200, which is not what a runyard-sandboxes daemon answers", 0},
		{"answers what is not JSON as JSON", "getAuth", fakedaemon.Answer(http.StatusOK, "application/json", "<h1>Welcome</h1>"), "asking DAEMON how people sign in: ", 0},
		{"refuses to say how people sign in", "getAuth", fakedaemon.Refuse(http.StatusServiceUnavailable, genv1.ErrorCodeUpstreamError, "the database is not answering"), "upstream_error: the database is not answering", 0},
		{"sends the question elsewhere", "getAuth", redirect, "it answered HTTP 307", 0},
		{"hangs up on the redeem", "redeemKeyAuthorization", fakedaemon.HangUp(), "collecting the key from DAEMON: ", 1},
		{"refuses the redeem", "redeemKeyAuthorization", fakedaemon.Refuse(http.StatusBadRequest, genv1.ErrorCodeBadRequest, "that code and verifier redeem no key"), "the approval was refused or has expired; run `ryclaude auth login` again", 1},
		{"cannot mint", "redeemKeyAuthorization", fakedaemon.Refuse(http.StatusServiceUnavailable, genv1.ErrorCodeUpstreamError, "the database is not answering"), "collecting the key from DAEMON: runyard-sandboxes: upstream_error: the database is not answering", 1},
		{"answers the redeem with no key", "redeemKeyAuthorization", fakedaemon.Answer(http.StatusOK, "application/json", `{"id":"k1234567"}`), "it answered with no key", 1},
		{"answers the redeem with a key that has no id", "redeemKeyAuthorization", fakedaemon.Answer(http.StatusOK, "application/json", `{"secret":"rysk_leaked"}`), "it answered with no key", 1},
		// What it said is not repeated: it is where a key would be.
		{"answers the redeem in a shape of its own", "redeemKeyAuthorization", fakedaemon.Answer(http.StatusOK, "text/plain", "rysk_leaked"), "it answered HTTP 200", 1},
		{"answers the redeem with what is not JSON", "redeemKeyAuthorization", fakedaemon.Answer(http.StatusOK, "application/json", "rysk_leaked"), "collecting the key from DAEMON: ", 1},
		// The code and the verifier are for this daemon, and go nowhere
		// its answer says.
		{"sends the redeem elsewhere", "redeemKeyAuthorization", redirect, "it answered HTTP 307", 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			configHome(t)
			d := signingIn(t)
			// Every time it is asked: a question that changes nothing is
			// asked again by the transport when its connection is hung up.
			d.InterceptAll(c.operation, c.answer)
			p := approving(t, d)
			var stderr bytes.Buffer
			code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, p.at(typing("")), &stderr)
			if said := strings.ReplaceAll(c.said, "DAEMON", d.URL); code != 1 || !strings.Contains(stderr.String(), said) {
				t.Fatalf("exit %d, want 1 and %q: %s", code, said, stderr.String())
			}
			if p.opened != c.opened {
				t.Errorf("a browser was opened %d times, want %d", p.opened, c.opened)
			}
			for name, secret := range map[string]string{"code": p.code, "key": "rysk_leaked"} {
				if secret != "" && strings.Contains(stderr.String(), secret) {
					t.Errorf("the %s is in what was said: %s", name, stderr.String())
				}
			}
			if elsewhere != 0 {
				t.Errorf("%d requests followed a redirect to another server", elsewhere)
			}
			nothingKept(t)
		})
	}
}

func TestNoDaemonAtTheAddress(t *testing.T) {
	configHome(t)
	// Closed, so that nothing is there, and nothing of anybody else's is.
	gone := httptest.NewServer(http.NotFoundHandler())
	gone.Close()
	p := &person{t: t}
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"auth", "login", "-url", gone.URL}, p.at(typing("")), &stderr); code != 1 || !strings.Contains(stderr.String(), "asking "+gone.URL+" how people sign in: ") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if p.opened != 0 {
		t.Error("a browser was opened")
	}
}

// revoking is each revoke a daemon was asked for: the key named, and the
// credential it was asked with. The daemon answers each as it would.
type revoking struct{ named, carried []string }

func (r *revoking) watch(d *fakedaemon.Daemon) {
	d.InterceptAll("revokeKey", func(w http.ResponseWriter, req *http.Request, serve http.HandlerFunc) {
		r.named = append(r.named, req.PathValue("key"))
		r.carried = append(r.carried, req.Header.Get("Authorization"))
		serve(w, req)
	})
}

// A key hands itself back, and the daemon refuses one that names another:
// the old key is revoked as the old key, and never with the new one's
// credential, even where both are the same person's at the same daemon.
func TestLoggingInAgainHandsBackTheKeyBeforeIt(t *testing.T) {
	t.Run("at the same daemon", func(t *testing.T) {
		configHome(t)
		d := signingIn(t)
		var revokes revoking
		revokes.watch(d)
		first := logIn(t, d)
		second := logIn(t, d)
		if keys := d.Keys(); first.KeyID == second.KeyID || len(keys) != 1 || keys[0].Id != second.KeyID {
			t.Errorf("after %s and then %s, the daemon's keys are %+v", first.KeyID, second.KeyID, keys)
		}
		if !slices.Equal(revokes.named, []string{first.KeyID}) || !slices.Equal(revokes.carried, []string{"Bearer " + first.Key}) {
			t.Errorf("revoked %v, asked as the key before in %d of %d", revokes.named, strings.Count(strings.Join(revokes.carried, " "), first.Key), len(revokes.carried))
		}
	})
	// One daemon at a time: the key for the first is revoked there, and is
	// never shown to the second.
	t.Run("at another daemon", func(t *testing.T) {
		configHome(t)
		one, other := signingIn(t), signingIn(t)
		var revokes revoking
		revokes.watch(one)
		first := logIn(t, one)
		g := logIn(t, other)
		if !slices.Equal(revokes.named, []string{first.KeyID}) || !slices.Equal(revokes.carried, []string{"Bearer " + first.Key}) {
			t.Errorf("revoked %v at the first daemon, asked as its own key in %d of %d", revokes.named, strings.Count(strings.Join(revokes.carried, " "), first.Key), len(revokes.carried))
		}
		if g.URL != other.URL || len(one.Keys()) != 0 || len(other.Keys()) != 1 {
			t.Errorf("kept the key for %s; the first daemon has %d keys and the second %d", g.URL, len(one.Keys()), len(other.Keys()))
		}
		if one.Calls("revokeKey") != 1 || other.Calls("revokeKey") != 0 {
			t.Errorf("revokes: %d at the first daemon, %d at the second", one.Calls("revokeKey"), other.Calls("revokeKey"))
		}
	})
	// An expired key is nobody's any more, and is not sent anywhere again.
	t.Run("when the key before has expired", func(t *testing.T) {
		path := configHome(t)
		d := signingIn(t)
		old := logIn(t, d)
		old.ExpiresAt = time.Now().Add(-time.Minute)
		if err := old.save(path); err != nil {
			t.Fatal(err)
		}
		var stderr bytes.Buffer
		if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, approving(t, d).at(typing("")), &stderr); code != 0 || strings.Contains(stderr.String(), "warning") {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		if d.Calls("revokeKey") != 0 {
			t.Error("the expired key was sent to be revoked")
		}
	})
}

// The login worked: a key before it that cannot be handed back is a warning,
// and the new one is kept.
func TestAKeyBeforeThatCannotBeRevokedIsAWarning(t *testing.T) {
	for _, c := range []struct {
		name   string
		answer fakedaemon.Interceptor
		said   string
	}{
		{"the daemon fails", fakedaemon.Refuse(http.StatusInternalServerError, genv1.ErrorCodeInternal, "the database is gone"), "could not be revoked: runyard-sandboxes: internal: the database is gone"},
		{"the daemon hangs up", fakedaemon.HangUp(), "could not be revoked: reaching "},
		{"the daemon refuses", fakedaemon.Refuse(http.StatusForbidden, genv1.ErrorCodeForbidden, "not yours"), "could not be revoked: runyard-sandboxes: forbidden: not yours"},
		// Revoked already, or expired there: there is nothing left to do,
		// and nothing to say.
		{"the daemon no longer takes it", fakedaemon.Refuse(http.StatusUnauthorized, genv1.ErrorCodeUnauthorized, "no key, or one this daemon will not accept"), ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			configHome(t)
			d := signingIn(t)
			old := logIn(t, d)
			d.Intercept("revokeKey", c.answer)
			var stderr bytes.Buffer
			if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, approving(t, d).at(typing("")), &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			said := stderr.String()
			if c.said == "" && strings.Contains(said, "warning") {
				t.Errorf("it warned: %s", said)
			}
			for _, want := range []string{c.said, "logged in as ada@example.com, at " + d.URL} {
				if !strings.Contains(said, want) {
					t.Errorf("it did not say %q: %s", want, said)
				}
			}
			if c.said != "" {
				for _, want := range []string{"ryclaude: warning: ", old.KeyID, when(old.ExpiresAt)} {
					if !strings.Contains(said, want) {
						t.Errorf("the warning does not say %q: %s", want, said)
					}
				}
			}
			if strings.Contains(said, old.Key) {
				t.Errorf("the key is in what was said: %s", said)
			}
			if g := kept(t); g.KeyID == old.KeyID || len(d.Keys()) != 2 {
				t.Errorf("kept %s, and the daemon has %d keys", g.KeyID, len(d.Keys()))
			}
		})
	}
}

// What was kept before and cannot be read is replaced, and said: the key it
// may have held is one nobody revoked.
func TestWhatWasKeptBeforeAndCannotBeReadIsReplaced(t *testing.T) {
	whole, err := json.Marshal(granted())
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		name, body string
		mode       os.FileMode
	}{
		{"not a key", "{", 0o600},
		{"a key others could read", string(whole), 0o644},
	} {
		t.Run(c.name, func(t *testing.T) {
			path := configHome(t)
			keptAs(t, path, c.body, c.mode)
			d := signingIn(t)
			var stderr bytes.Buffer
			if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, approving(t, d).at(typing("")), &stderr); code != 0 {
				t.Fatalf("exit %d: %s", code, stderr.String())
			}
			if said := stderr.String(); !strings.Contains(said, "warning: what was kept before could not be read") || strings.Contains(said, "rysk_") {
				t.Errorf("said: %s", said)
			}
			if g := kept(t); g.URL != d.URL || modeOf(t, path) != 0o600 {
				t.Errorf("kept %s at mode %04o", g.URL, modeOf(t, path))
			}
		})
	}
}

// A key that was minted and cannot be kept would work for a week with
// nobody holding it: it is handed straight back.
func TestAKeyThatCannotBeKeptIsRevoked(t *testing.T) {
	inTheWay := func(t *testing.T) {
		t.Helper()
		if err := os.MkdirAll(filepath.Join(configHome(t), "in the way"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("and is", func(t *testing.T) {
		inTheWay(t)
		d := signingIn(t)
		var stderr bytes.Buffer
		if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, approving(t, d).at(typing("")), &stderr); code != 1 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		if said := stderr.String(); !strings.Contains(said, "keeping the key: ") || !strings.Contains(said, "the key that was made has been revoked") || strings.Contains(said, "logged in") {
			t.Errorf("said: %s", said)
		}
		if keys := d.Keys(); len(keys) != 0 {
			t.Errorf("the daemon still has %+v", keys)
		}
	})
	t.Run("and cannot be", func(t *testing.T) {
		inTheWay(t)
		d := signingIn(t)
		d.Intercept("revokeKey", fakedaemon.HangUp())
		var stderr bytes.Buffer
		if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, approving(t, d).at(typing("")), &stderr); code != 1 {
			t.Fatalf("exit %d: %s", code, stderr.String())
		}
		keys := d.Keys()
		if len(keys) != 1 {
			t.Fatalf("the daemon has %+v", keys)
		}
		said := stderr.String()
		for _, want := range []string{"keeping the key: ", "could not be revoked", keys[0].Id, "revoke it in the console of " + d.URL} {
			if !strings.Contains(said, want) {
				t.Errorf("it did not say %q: %s", want, said)
			}
		}
		if strings.Contains(said, "rysk_") {
			t.Errorf("the key is in what was said: %s", said)
		}
	})
}

// With nowhere to keep a key, nobody is asked for one.
func TestWithNowhereToKeepAKeyNobodyIsAsked(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", "")
	d := signingIn(t)
	p := approving(t, d)
	var stderr bytes.Buffer
	if code := run(t.Context(), []string{"auth", "login", "-url", d.URL}, p.at(typing("")), &stderr); code != 1 || !strings.Contains(stderr.String(), "finding where ryclaude keeps its key") {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	if p.opened != 0 || d.Calls("getAuth") != 0 {
		t.Error("the daemon or a browser was asked")
	}
}

func TestLoginIsToldWhichDaemon(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		exit int
		said string
	}{
		{"no daemon", nil, 2, "-url says which daemon to log in to, and it is not a daemon's address"},
		{"no daemon, and no browser", []string{"-no-browser"}, 2, "-url says which daemon to log in to"},
		{"a daemon reached in the clear", []string{"-url", "http://sandboxes.example.com"}, 2, "the key would cross the network for anybody on the way to read: use https://sandboxes.example.com"},
		{"a daemon with a password", []string{"-url", "https://ada:hunter2@sandboxes.example.com"}, 2, "a user or a password"},
		{"a page of the daemon's", []string{"-url", "https://sandboxes.example.com/console"}, 2, "a path, a query or a fragment"},
		{"an address with no scheme", []string{"-url", "sandboxes.example.com"}, 2, "it is not a daemon's address"},
		{"something more", []string{"-url", "https://sandboxes.example.com", "now"}, 2, `it takes no "now"`},
		{"a flag it does not have", []string{"-key", "k"}, 2, "flag provided but not defined: -key"},
		{"help", []string{"-h"}, 0, "-no-browser"},
	} {
		t.Run(c.name, func(t *testing.T) {
			configHome(t)
			p := &person{t: t}
			var stderr bytes.Buffer
			if code := run(t.Context(), append([]string{"auth", "login"}, c.args...), p.at(typing("")), &stderr); code != c.exit || !strings.Contains(stderr.String(), c.said) {
				t.Fatalf("exit %d, want %d and %q: %s", code, c.exit, c.said, stderr.String())
			}
			if p.opened != 0 || strings.Contains(stderr.String(), "hunter2") {
				t.Errorf("a browser was opened %d times; said: %s", p.opened, stderr.String())
			}
			nothingKept(t)
		})
	}
}
