package fakedaemon

import (
	"net/http"
	"slices"
	"strings"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// The namespaces plane, as much of it as a program given a key reaches: every
// sandbox, volume and tunnel is in a namespace, and so is every route about
// one, under /v1/namespaces/{namespace}. A namespace the caller does not reach
// and one that is not there are one 404, with one body, as the daemon's are.

// DefaultNamespace is the namespace a fake has when it is not given others,
// and the one its keys are bound to.
const DefaultNamespace = "default"

// WithNamespaces is the namespaces the daemon has. The first is the one the
// key it is made with (WithKey) is bound to, and the one Volume and
// PutVolume are about.
func WithNamespaces(first string, more ...string) Option {
	return func(d *Daemon) { d.namespaces = append([]string{first}, more...) }
}

// WithGlobalKeys makes the key the daemon is made with global: bound to no
// namespace, and reaching every namespace the daemon has.
func WithGlobalKeys() Option { return func(d *Daemon) { d.global = true } }

// home is the namespace the daemon's key is bound to: the first, or none for
// a daemon whose keys are global.
func (d *Daemon) home() *genv1.NamespaceName {
	if d.global {
		return nil
	}
	return new(d.namespaces[0])
}

// boundTo is the namespace the key a request carries is bound to, or empty
// for a global key and for a daemon made with no key, which lets every call
// in.
func (d *Daemon) boundTo(r *http.Request) string {
	if d.ownKey(r) {
		return valueOf(d.home())
	}
	return ""
}

// inNamespace lets a request about a namespace through to serve only when
// the namespace is there and the caller's key reaches it.
func (d *Daemon) inNamespace(serve http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		namespace := r.PathValue("namespace")
		d.mu.Lock()
		bound := d.boundTo(r)
		reached := slices.Contains(d.namespaces, namespace) && (bound == "" || bound == namespace)
		d.mu.Unlock()
		if !reached {
			refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, "there is no namespace "+namespace+" that this call reaches")
			return
		}
		serve(w, r)
	}
}

// movedPaths are the paths that were before namespaces were: each answers
// 404, naming where it went.
var movedPaths = []string{"/v1/sandboxes", "/v1/sandboxes/", "/v1/volumes", "/v1/volumes/", "/v1/builds", "/v1/builds/", "/v1/tunnels", "/v1/counts", "/v1/images", "/v1/images/", "/v1/brokers", "/v1/brokers/", "/v1/ca"}

// moved is a path from before namespaces: an old client is told where it
// went, rather than left to guess at a bare 404.
func moved(w http.ResponseWriter, r *http.Request) {
	refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound,
		r.URL.Path+" is gone: it is /v1/namespaces/{namespace}"+strings.TrimPrefix(r.URL.Path, "/v1")+" now, in the namespace it is about")
}

// in says whether a sandbox is in a namespace.
func (s *sandbox) in(namespace string) bool {
	return valueOf(s.record.Namespace) == namespace
}

// volumeKey is a volume's name in its namespace: the same name in two is two
// volumes.
type volumeKey struct {
	namespace, name string
}
