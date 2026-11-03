package fakeserver

import (
	"net/http"
	"strings"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// operator is whose the key a server is made with (WithKey) is: it is the
// administrator's who started the server.
const operator = "operator@example.com"

// ownKey reports whether a request carries the key the server is made with.
func (d *Server) ownKey(r *http.Request) bool {
	secret, _ := strings.CutPrefix(authorizationOf(r), "Bearer ")
	return d.key != "" && secret == d.key
}

// creatorOf is who a request's key is, as a volume it lays down records them:
// nil for a server made with no key, which has nobody to name.
func (d *Server) creatorOf(r *http.Request) *genv1.Creator {
	if d.ownKey(r) {
		return &genv1.Creator{User: operator, KeyId: new("koperator"), KeyName: new("the server's own")}
	}
	return nil
}

// getMe says whose key a call carried: the operator's, which may do
// everything.
func (d *Server) getMe(w http.ResponseWriter, r *http.Request) {
	if !d.ownKey(r) {
		// A server made with no key lets every call in, and has nobody to
		// name.
		refuse(w, http.StatusUnauthorized, genv1.ErrorCodeUnauthorized, "no key, or one this server will not accept")
		return
	}
	me := genv1.Me{Via: genv1.ViaKey, Admin: true, Email: operator}
	me.Scopes = []genv1.Scope{
		genv1.SandboxesRead, genv1.SandboxesWrite, genv1.Exec, genv1.FilesRead, genv1.FilesWrite,
		genv1.NetworkWrite, genv1.TunnelsWrite, genv1.ImagesRead, genv1.ImagesWrite, genv1.BrokersAdmin, genv1.KeysAdmin,
		genv1.PrincipalsRead, genv1.PrincipalsWrite,
	}
	me.Key = &struct {
		Id        string         `json:"id"`
		Name      string         `json:"name"`
		Sandboxes genv1.KeyReach `json:"sandboxes"`
	}{Id: "koperator", Name: "the server's own", Sandboxes: genv1.KeyReachAll}
	reply(w, http.StatusOK, me)
}
