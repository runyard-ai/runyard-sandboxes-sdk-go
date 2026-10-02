package fakedaemon

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// RFC 7636's own example of a verifier.
const verifier = "dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"

func challengeOf(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

// asked is what a program asks a person to approve.
func asked(expires time.Time) genv1.KeyAuthorizationRequest {
	own := genv1.KeyReachOwn
	return genv1.KeyAuthorizationRequest{
		Name: "ryclaude", Scopes: []genv1.Scope{genv1.SandboxesRead, genv1.Exec}, Sandboxes: &own,
		ExpiresAt: expires, CodeChallenge: challengeOf(verifier),
	}
}

func redeem(t *testing.T, client *genv1.ClientWithResponses, code, verifier string) *genv1.RedeemKeyAuthorizationResponse {
	t.Helper()
	res, err := client.RedeemKeyAuthorizationWithResponse(context.Background(),
		genv1.KeyAuthorizationRedemption{Code: code, CodeVerifier: verifier})
	if err != nil {
		t.Fatal(err)
	}
	return res
}

func TestTheFakeSaysHowAPersonSignsInWithoutBeingShownAKey(t *testing.T) {
	ctx := context.Background()
	for name, c := range map[string]struct {
		opts   []Option
		google bool
	}{
		"a daemon with no sign-in": {[]Option{WithKey("k")}, false},
		"a daemon with Google":     {[]Option{WithKey("k"), WithGoogle()}, true},
	} {
		t.Run(name, func(t *testing.T) {
			// With no key: it is what is read before there is one.
			auth, err := typed(t, New(t, c.opts...), "").GetAuthWithResponse(ctx)
			if err != nil || auth.JSON200 == nil || auth.JSON200.Google != c.google {
				t.Fatalf("auth = %v %d %s, want google=%v", err, auth.StatusCode(), auth.Body, c.google)
			}
		})
	}
}

// An approval's whole life: redeemed once for a key the daemon then accepts,
// until the key hands itself back.
func TestTheFakeTradesAnApprovalForAKeyOnce(t *testing.T) {
	ctx := context.Background()
	d := New(t, WithKey("admin"))
	anonymous := typed(t, d, "")
	expires := time.Now().Add(time.Hour).UTC().Truncate(time.Second)

	approved := d.Approve("ada@example.com", asked(expires))
	if approved.Code == "" || !approved.ExpiresAt.After(time.Now()) || approved.ExpiresAt.After(time.Now().Add(approvalTTL)) {
		t.Fatalf("approved = %+v, want a code good for five minutes", approved)
	}
	if got := d.Keys(); len(got) != 0 {
		t.Fatalf("approving minted %v: nothing is minted until the code is redeemed", got)
	}

	redeemed := redeem(t, anonymous, approved.Code, verifier)
	key := redeemed.JSON200
	if key == nil {
		t.Fatalf("redeem = %d %s", redeemed.StatusCode(), redeemed.Body)
	}
	if key.Name != "ryclaude" || key.Owner != "ada@example.com" || key.Sandboxes != genv1.KeyReachOwn ||
		len(key.Scopes) != 2 || !key.ExpiresAt.Equal(expires) || !strings.HasPrefix(key.Secret, "rysk_"+key.Id+"_") {
		t.Errorf("the key is %+v, which is not what was approved", key)
	}
	if got := redeemed.HTTPResponse.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("a key was answered with Cache-Control %q, want no-store", got)
	}
	if got := d.Keys(); len(got) != 1 || got[0].Id != key.Id {
		t.Errorf("the daemon's keys are %v, want the one minted", got)
	}

	if again := redeem(t, anonymous, approved.Code, verifier); again.JSON400 == nil || again.JSON400.Error.Message != noKey {
		t.Errorf("a second redeem = %d %s, want the 400 every spent code gets", again.StatusCode(), again.Body)
	}

	holder := typed(t, d, key.Secret)
	if info, _ := holder.GetInfoWithResponse(ctx); info.StatusCode() != 200 {
		t.Fatalf("the minted key was answered %d", info.StatusCode())
	}
	if revoked, _ := holder.RevokeKeyWithResponse(ctx, key.Id); revoked.StatusCode() != 204 {
		t.Fatalf("a key revoking itself = %d %s", revoked.StatusCode(), revoked.Body)
	}
	if info, _ := holder.GetInfoWithResponse(ctx); info.StatusCode() != 401 {
		t.Errorf("a revoked key was answered %d, want 401", info.StatusCode())
	}
	if got := d.Keys(); len(got) != 0 {
		t.Errorf("the daemon still has %v", got)
	}
	// Idempotent, and the same for a key that never was.
	for _, id := range []string{key.Id, "never"} {
		if revoked, _ := typed(t, d, "admin").RevokeKeyWithResponse(ctx, id); revoked.StatusCode() != 204 {
			t.Errorf("revoking %s again = %d, want 204", id, revoked.StatusCode())
		}
	}
	if revoked, _ := anonymous.RevokeKeyWithResponse(ctx, key.Id); revoked.StatusCode() != 401 {
		t.Errorf("revoking with no key = %d, want 401", revoked.StatusCode())
	}
}

// A key approved with no reach acts on every sandbox, as one minted with none
// does.
func TestAKeyApprovedWithNoReachActsOnAll(t *testing.T) {
	d := New(t)
	request := asked(time.Now().Add(time.Hour))
	request.Sandboxes = nil
	redeemed := redeem(t, typed(t, d, ""), d.Approve("ada@example.com", request).Code, verifier)
	if redeemed.JSON200 == nil || redeemed.JSON200.Sandboxes != genv1.KeyReachAll {
		t.Fatalf("redeem = %d %s, want a key on all sandboxes", redeemed.StatusCode(), redeemed.Body)
	}
}

// Every code that yields no key is answered alike, and one presented with the
// wrong verifier is spent by it.
func TestEveryCodeThatYieldsNoKeyIsAnsweredAlike(t *testing.T) {
	var clock atomic.Pointer[time.Time]
	start := time.Now()
	clock.Store(&start)
	d := New(t, WithClock(func() time.Time { return *clock.Load() }))
	client := typed(t, d, "")
	request := asked(start.Add(time.Hour))

	refused := func(what string, res *genv1.RedeemKeyAuthorizationResponse) {
		t.Helper()
		if res.JSON400 == nil || res.JSON400.Error.Code != genv1.ErrorCodeBadRequest || res.JSON400.Error.Message != noKey {
			t.Errorf("%s = %d %s, want the one 400", what, res.StatusCode(), res.Body)
		}
	}
	refused("a code never given", redeem(t, client, "never-given", verifier))
	refused("no code at all", redeem(t, client, "", verifier))

	guessed := d.Approve("ada@example.com", request)
	refused("the wrong verifier", redeem(t, client, guessed.Code, strings.Repeat("x", 43)))
	refused("the right verifier, after a wrong one spent the code", redeem(t, client, guessed.Code, verifier))

	late := d.Approve("ada@example.com", request)
	clock.Store(&late.ExpiresAt)
	refused("a code at its expiry", redeem(t, client, late.Code, verifier))

	clock.Store(&start)
	inTime := d.Approve("ada@example.com", request)
	almost := inTime.ExpiresAt.Add(-time.Nanosecond)
	clock.Store(&almost)
	if res := redeem(t, client, inTime.Code, verifier); res.JSON200 == nil {
		t.Errorf("a code just before its expiry = %d %s, want its key", res.StatusCode(), res.Body)
	}
	if got := len(d.Keys()); got != 1 {
		t.Errorf("%d keys were minted, want only the one redeemed in time", got)
	}

	// A body that is not the route's is a 400 too, and says what it is.
	malformed, err := client.RedeemKeyAuthorizationWithBodyWithResponse(context.Background(), "application/json", strings.NewReader(`{"code":`))
	if err != nil || malformed.JSON400 == nil || malformed.JSON400.Error.Message == noKey {
		t.Errorf("a malformed body = %v %d %s, want a 400 that says so", err, malformed.StatusCode(), malformed.Body)
	}
}

// A key the daemon minted is refused once it has expired, as a real one is.
func TestAMintedKeyIsRefusedOnceItHasExpired(t *testing.T) {
	ctx := context.Background()
	var clock atomic.Pointer[time.Time]
	start := time.Now()
	clock.Store(&start)
	d := New(t, WithKey("admin"), WithClock(func() time.Time { return *clock.Load() }))
	expires := start.Add(time.Minute)
	redeemed := redeem(t, typed(t, d, ""), d.Approve("ada@example.com", asked(expires)).Code, verifier)
	if redeemed.JSON200 == nil {
		t.Fatalf("redeem = %d %s", redeemed.StatusCode(), redeemed.Body)
	}
	holder := typed(t, d, redeemed.JSON200.Secret)
	if info, _ := holder.GetInfoWithResponse(ctx); info.StatusCode() != 200 {
		t.Fatalf("a key before its expiry was answered %d", info.StatusCode())
	}
	clock.Store(&expires)
	if info, _ := holder.GetInfoWithResponse(ctx); info.StatusCode() != 401 {
		t.Errorf("a key at its expiry was answered %d, want 401", info.StatusCode())
	}
	// Something that is not a bearer token is nobody's key, minted or not.
	raw, err := genv1.NewClientWithResponses(d.URL, genv1.WithRequestEditorFn(func(_ context.Context, req *http.Request) error {
		req.Header.Set("Authorization", redeemed.JSON200.Secret)
		return nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	clock.Store(&start)
	if info, _ := raw.GetInfoWithResponse(ctx); info.StatusCode() != 401 {
		t.Errorf("a secret sent without `Bearer` was answered %d, want 401", info.StatusCode())
	}
}

// What a program that logs in is tested against when the daemon does not do
// its part: each of the three operations can be answered in its place.
func TestTheKeysPlaneCanBeAnsweredInTheDaemonsPlace(t *testing.T) {
	ctx := context.Background()
	d := New(t, WithGoogle())
	client := typed(t, d, "")
	d.Intercept("getAuth", Refuse(503, genv1.ErrorCodeUpstreamError, "the database is not answering"))
	d.Intercept("redeemKeyAuthorization", Refuse(503, genv1.ErrorCodeUpstreamError, "the database is not answering"))
	d.Intercept("revokeKey", Refuse(403, genv1.ErrorCodeForbidden, "that key is somebody else's"))

	if auth, _ := client.GetAuthWithResponse(ctx); auth.StatusCode() != 503 {
		t.Errorf("getAuth = %d, want the 503 it was given", auth.StatusCode())
	}
	approved := d.Approve("ada@example.com", asked(time.Now().Add(time.Hour)))
	if res := redeem(t, client, approved.Code, verifier); res.JSON503 == nil {
		t.Errorf("redeem = %d %s, want the 503 it was given", res.StatusCode(), res.Body)
	}
	// The daemon was not reached, so the code is not spent.
	res := redeem(t, client, approved.Code, verifier)
	if res.JSON200 == nil {
		t.Fatalf("the redeem after it = %d %s, want the key", res.StatusCode(), res.Body)
	}
	if revoked, _ := client.RevokeKeyWithResponse(ctx, res.JSON200.Id); revoked.JSON403 == nil {
		t.Errorf("revoke = %d, want the 403 it was given", revoked.StatusCode())
	}
	if got := len(d.Keys()); got != 1 {
		t.Errorf("%d keys, want the one a refused revoke left", got)
	}
	for operation, want := range map[string]int{"getAuth": 1, "redeemKeyAuthorization": 2, "revokeKey": 1} {
		if got := d.Calls(operation); got != want {
			t.Errorf("%s was counted %d times, want %d", operation, got, want)
		}
	}
}

// Who a key is: its approver for one minted here, the operator for the
// daemon's own, and nobody once it is revoked or was never shown.
func TestTheFakeSaysWhoseKeyACallCarried(t *testing.T) {
	ctx := context.Background()
	d := New(t, WithKey("admin"))
	redeemed := redeem(t, typed(t, d, ""), d.Approve("ada@example.com", asked(time.Now().Add(time.Hour))).Code, verifier)
	key := redeemed.JSON200
	if key == nil {
		t.Fatalf("redeem = %d %s", redeemed.StatusCode(), redeemed.Body)
	}
	holder := typed(t, d, key.Secret)

	me, err := holder.GetMeWithResponse(ctx)
	if err != nil || me.JSON200 == nil {
		t.Fatalf("me = %v %d %s", err, me.StatusCode(), me.Body)
	}
	if got := me.JSON200; got.Email != "ada@example.com" || got.Admin || got.Via != genv1.ViaKey ||
		got.Key == nil || got.Key.Id != key.Id || got.Key.Name != "ryclaude" || len(got.Scopes) != 2 {
		t.Errorf("me = %+v, want the approver and the key that was minted", got)
	}

	own, err := typed(t, d, "admin").GetMeWithResponse(ctx)
	if err != nil || own.JSON200 == nil || !own.JSON200.Admin || own.JSON200.Email != operator || own.JSON200.Key == nil {
		t.Errorf("the daemon's own key = %v %d %s, want its operator", err, own.StatusCode(), own.Body)
	}

	if revoked, _ := holder.RevokeKeyWithResponse(ctx, key.Id); revoked.StatusCode() != 204 {
		t.Fatalf("revoke = %d", revoked.StatusCode())
	}
	if gone, _ := holder.GetMeWithResponse(ctx); gone.JSON401 == nil {
		t.Errorf("a revoked key was answered %d %s, want 401", gone.StatusCode(), gone.Body)
	}
	// A daemon that asks for no key lets a call in, and has nobody to name.
	if nobody, _ := typed(t, New(t), "").GetMeWithResponse(ctx); nobody.JSON401 == nil {
		t.Errorf("a call with no key was answered %d %s, want 401", nobody.StatusCode(), nobody.Body)
	}
}

// A key hands itself back and no other, as the daemon has it: what revokes
// more is `keys.admin`, among its owner's keys, and the operator's key.
func TestTheFakeLetsAKeyRevokeOnlyWhatTheDaemonWould(t *testing.T) {
	ctx := context.Background()
	d := New(t, WithKey("admin"))
	mint := func(owner string, scopes ...genv1.Scope) *genv1.KeyCreated {
		t.Helper()
		request := asked(time.Now().Add(time.Hour))
		request.Scopes = scopes
		redeemed := redeem(t, typed(t, d, ""), d.Approve(owner, request).Code, verifier)
		if redeemed.JSON200 == nil {
			t.Fatalf("redeem = %d %s", redeemed.StatusCode(), redeemed.Body)
		}
		return redeemed.JSON200
	}
	live := func(id string) bool {
		for _, key := range d.Keys() {
			if key.Id == id {
				return true
			}
		}
		return false
	}
	old, renewed := mint("ada@example.com", genv1.Exec), mint("ada@example.com", genv1.Exec)
	admin := mint("ada@example.com", genv1.KeysAdmin)
	theirs := mint("grace@example.com", genv1.Exec)

	for _, c := range []struct {
		name   string
		caller string
		named  string
		status int
	}{
		// The same person's, and still not its own: the newer key of a
		// program that logged in twice cannot hand back the older.
		{"a key naming another of its owner's", renewed.Secret, old.Id, 403},
		{"a key naming somebody else's", renewed.Secret, theirs.Id, 403},
		{"a key naming one that never was", renewed.Secret, "never", 403},
		{"a key with keys.admin naming somebody else's", admin.Secret, theirs.Id, 403},
		{"a key with keys.admin naming another of its owner's", admin.Secret, renewed.Id, 204},
		{"a key with keys.admin naming one that never was", admin.Secret, "never", 204},
		{"a key naming itself", old.Secret, old.Id, 204},
		{"the operator naming anybody's", "admin", theirs.Id, 204},
	} {
		before := len(d.Keys())
		res, err := typed(t, d, c.caller).RevokeKeyWithResponse(ctx, c.named)
		if err != nil || res.StatusCode() != c.status || (c.status == 403) != (res.JSON403 != nil) {
			t.Errorf("%s = %v %d %s, want %d", c.name, err, res.StatusCode(), res.Body, c.status)
		}
		if c.status == 403 && len(d.Keys()) != before {
			t.Errorf("%s was refused, and a key is gone", c.name)
		}
		if c.status == 204 && live(c.named) {
			t.Errorf("%s was answered 204, and the key is still there", c.name)
		}
	}
	if !live(admin.Id) || live(old.Id) || live(renewed.Id) || live(theirs.Id) {
		t.Errorf("left: %+v", d.Keys())
	}
}
