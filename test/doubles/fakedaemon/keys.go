package fakedaemon

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// The keys plane, as much of it as a program that is given a key reaches: how
// a person signs in here, an approval traded for its key, and a key handed
// back. There is no console here and nobody signed in to one: a test is the
// person, and Approve is them pressing the button.

// approvalTTL is how long an approval's code works, as the contract says.
const approvalTTL = 5 * time.Minute

// noKey is the one answer for every code that yields no key. Which of them it
// was is what the daemon does not say, so a program cannot have been written
// to tell them apart.
const noKey = "that code and verifier redeem no key: the code is not one this daemon gave, was already used, or is older than five minutes, or the verifier is not the one it was approved for"

// approval is a key somebody approved, waiting for its code.
type approval struct {
	owner     string
	request   genv1.KeyAuthorizationRequest
	expiresAt time.Time
}

// WithGoogle is a daemon whose console signs people in with Google, which is
// what `GET /v1/auth` says of it. Without it nobody can sign in, and so
// nobody can approve a key.
func WithGoogle() Option { return func(d *Daemon) { d.google = true } }

// Approve is a person signed in to the console as owner approving a key for
// the program that asked: what `POST /v1/keys/authorizations` does when the
// page calls it. The code it answers with is what the page would send the
// browser to the program's redirect URI with.
//
// Nothing is checked of what is asked, which a daemon would: the test is the
// person and the page, and what it approves is what it means to.
func (d *Daemon) Approve(owner string, request genv1.KeyAuthorizationRequest) genv1.KeyAuthorization {
	d.mu.Lock()
	defer d.mu.Unlock()
	code := base64.RawURLEncoding.EncodeToString(random(32))
	expires := d.now().UTC().Add(approvalTTL)
	d.approvals[code] = approval{owner: owner, request: request, expiresAt: expires}
	return genv1.KeyAuthorization{Code: code, ExpiresAt: expires}
}

// Keys is every key minted from an approval and not revoked since, oldest
// first.
func (d *Daemon) Keys() []genv1.Key {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]genv1.Key, 0, len(d.minted))
	for _, key := range d.minted {
		out = append(out, genv1.Key{
			Id: key.Id, Name: key.Name, Owner: key.Owner, Scopes: key.Scopes, Sandboxes: key.Sandboxes,
			CreatedAt: key.CreatedAt, ExpiresAt: key.ExpiresAt,
		})
	}
	return out
}

// mintedKey reports whether an `Authorization` header carries a key this daemon
// minted, has not revoked, and has not outlived.
func (d *Daemon) mintedKey(authorization string) bool {
	secret, ok := strings.CutPrefix(authorization, "Bearer ")
	if !ok {
		return false
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, key := range d.minted {
		if key.Secret == secret {
			return d.now().Before(key.ExpiresAt)
		}
	}
	return false
}

func random(n int) []byte {
	bytes := make([]byte, n)
	// crypto/rand does not fail: it aborts the program when the kernel has
	// no randomness to give.
	_, _ = rand.Read(bytes)
	return bytes
}

func (d *Daemon) getAuth(w http.ResponseWriter, _ *http.Request) {
	reply(w, http.StatusOK, genv1.AuthMethods{Google: d.google})
}

// operator is whose the key a daemon is made with (WithKey) is: nobody
// approved it, so it is the administrator's who started the daemon.
const operator = "operator@example.com"

// getMe says whose key a call carried: its approver's for one minted here,
// and the operator's for the daemon's own, which may do everything. It is
// what a program asks to learn whether its key is still accepted.
func (d *Daemon) getMe(w http.ResponseWriter, r *http.Request) {
	secret, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	me := genv1.Me{Via: genv1.ViaKey}
	d.mu.Lock()
	for _, key := range d.minted {
		if key.Secret == secret {
			me.Email, me.Scopes = key.Owner, key.Scopes
			me.Key = &struct {
				Id        string         `json:"id"`
				Name      string         `json:"name"`
				Sandboxes genv1.KeyReach `json:"sandboxes"`
			}{key.Id, key.Name, key.Sandboxes}
		}
	}
	d.mu.Unlock()
	switch {
	case me.Key != nil:
	case d.key != "" && secret == d.key:
		me.Admin, me.Email = true, operator
		me.Scopes = []genv1.Scope{
			genv1.SandboxesRead, genv1.SandboxesWrite, genv1.Exec, genv1.FilesRead, genv1.FilesWrite,
			genv1.NetworkWrite, genv1.TunnelsWrite, genv1.ImagesRead, genv1.ImagesWrite, genv1.BrokersAdmin, genv1.KeysAdmin,
		}
		me.Key = &struct {
			Id        string         `json:"id"`
			Name      string         `json:"name"`
			Sandboxes genv1.KeyReach `json:"sandboxes"`
		}{"koperator", "the daemon's own", genv1.KeyReachAll}
	default:
		// A daemon made with no key lets every call in, and this one still
		// has nobody to name.
		refuse(w, http.StatusUnauthorized, genv1.ErrorCodeUnauthorized, "no key, or one this daemon will not accept")
		return
	}
	reply(w, http.StatusOK, me)
}

func (d *Daemon) redeemKeyAuthorization(w http.ResponseWriter, r *http.Request) {
	var body genv1.KeyAuthorizationRedemption
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "the body is not the JSON this route takes: "+err.Error())
		return
	}
	d.mu.Lock()
	// Spent by being presented, whatever the verifier: a code good for a
	// second guess is one somebody else can race the program for.
	pending, known := d.approvals[body.Code]
	delete(d.approvals, body.Code)
	challenge := sha256.Sum256([]byte(body.CodeVerifier))
	if !known || !d.now().Before(pending.expiresAt) ||
		base64.RawURLEncoding.EncodeToString(challenge[:]) != pending.request.CodeChallenge {
		d.mu.Unlock()
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, noKey)
		return
	}
	reach := genv1.KeyReachAll
	if pending.request.Sandboxes != nil {
		reach = *pending.request.Sandboxes
	}
	id := "k" + hex.EncodeToString(random(4))[:7]
	key := genv1.KeyCreated{
		Id: id, Name: pending.request.Name, Owner: openapi_types.Email(pending.owner),
		Scopes: pending.request.Scopes, Sandboxes: reach,
		CreatedAt: d.now().UTC(), ExpiresAt: pending.request.ExpiresAt,
		Secret: "rysk_" + id + "_" + base64.RawURLEncoding.EncodeToString(random(24)),
	}
	d.minted = append(d.minted, key)
	d.mu.Unlock()
	w.Header().Set("Cache-Control", "no-store")
	reply(w, http.StatusOK, key)
}

// revokeKey takes back a key this daemon minted, and answers the same for one
// it never did: revoking is idempotent, and to the daemon a key that is gone
// and one that never was are the same key.
//
// Who may is the contract's. A key names itself, whatever its scopes; with
// `keys.admin` it names any key of its owner's; and the daemon's own key
// (WithKey), the operator's, names anybody's. Any other is refused, as the
// daemon refuses it: a program that handed back one key with another's
// credential would pass here and fail there.
func (d *Daemon) revokeKey(w http.ResponseWriter, r *http.Request) {
	secret, _ := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	id := r.PathValue("key")
	d.mu.Lock()
	defer d.mu.Unlock()
	var caller, named *genv1.KeyCreated
	for i := range d.minted {
		if d.minted[i].Secret == secret {
			caller = &d.minted[i]
		}
		if d.minted[i].Id == id {
			named = &d.minted[i]
		}
	}
	switch {
	case caller == nil, caller.Id == id:
	case !slices.Contains(caller.Scopes, genv1.KeysAdmin):
		refuse(w, http.StatusForbidden, genv1.ErrorCodeForbidden, "a key without keys.admin revokes itself and no other, and "+id+" is not the key this call carried")
		return
	case named != nil && named.Owner != caller.Owner:
		refuse(w, http.StatusForbidden, genv1.ErrorCodeForbidden, id+" is somebody else's key, and only an administrator revokes those")
		return
	}
	for i, key := range d.minted {
		if key.Id == id {
			d.minted = append(d.minted[:i:i], d.minted[i+1:]...)
			break
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
