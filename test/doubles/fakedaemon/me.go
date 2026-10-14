package fakedaemon

import (
	"net/http"
	"slices"
	"strings"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// operator is whose the key a daemon is made with (WithKey) is: it is the
// administrator's who started the daemon.
const operator = "operator@example.com"

// ownKey reports whether a request carries the key the daemon is made with.
func (d *Daemon) ownKey(r *http.Request) bool {
	secret, _ := strings.CutPrefix(authorizationOf(r), "Bearer ")
	return d.key != "" && secret == d.key
}

// creatorOf is who a request's key is, as a volume it lays down records them:
// nil for a daemon made with no key, which has nobody to name.
func (d *Daemon) creatorOf(r *http.Request) *genv1.Creator {
	if d.ownKey(r) {
		return &genv1.Creator{User: operator, KeyId: new("koperator"), KeyName: new("the daemon's own")}
	}
	return nil
}

// getMe says whose key a call carried — the operator's, which may do
// everything — the namespace it is bound to, and those it reaches. It is
// what a program asks to learn where it acts.
func (d *Daemon) getMe(w http.ResponseWriter, r *http.Request) {
	if !d.ownKey(r) {
		// A daemon made with no key lets every call in, and has nobody to
		// name.
		refuse(w, http.StatusUnauthorized, genv1.ErrorCodeUnauthorized, "no key, or one this daemon will not accept")
		return
	}
	me := genv1.Me{Via: genv1.ViaKey, Admin: true, Email: operator}
	me.Scopes = []genv1.Scope{
		genv1.SandboxesRead, genv1.SandboxesWrite, genv1.Exec, genv1.FilesRead, genv1.FilesWrite,
		genv1.NetworkWrite, genv1.TunnelsWrite, genv1.ImagesRead, genv1.ImagesWrite, genv1.BrokersAdmin, genv1.KeysAdmin,
	}
	me.Key = &struct {
		Id        string               `json:"id"`
		Name      string               `json:"name"`
		Namespace *genv1.NamespaceName `json:"namespace,omitempty"`
		Sandboxes genv1.KeyReach       `json:"sandboxes"`
	}{Id: "koperator", Name: "the daemon's own", Namespace: d.home(), Sandboxes: genv1.KeyReachAll}
	reached := slices.Clone(d.namespaces)
	if me.Key.Namespace != nil {
		reached = []string{*me.Key.Namespace}
	}
	me.Namespaces = &reached
	reply(w, http.StatusOK, me)
}
