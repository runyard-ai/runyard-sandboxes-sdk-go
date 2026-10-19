package fakedaemon

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// The principal plane: identities that outlive a sandbox, the volumes that
// follow them, and what brokers' trusted planes say about them — answered as
// the daemon answers, with every principal the daemon's one key's.

// TrustedPlane is a shared broker's trusted plane: what it answers when the
// daemon reads path (`.well-known/…`) for principal.
type TrustedPlane func(principal genv1.Principal, path string) (status int, contentType string, body []byte)

// WithTrustedPlane gives the shared broker called broker a trusted plane,
// declaring the broker if WithBrokers did not.
func WithTrustedPlane(broker string, plane TrustedPlane) Option {
	return func(d *Daemon) {
		if d.trusted == nil {
			d.trusted = map[string]TrustedPlane{}
		}
		d.trusted[broker] = plane
		address := "https://" + broker + ".invalid:8454/"
		for i := range d.brokers {
			if d.brokers[i].Name == broker {
				d.brokers[i].TrustedUrl = &address
				return
			}
		}
		d.brokers = append(d.brokers, genv1.Broker{Name: broker, Url: "https://" + broker + ".invalid:8443/", TrustedUrl: &address})
	}
}

// WithTrustedAt gives the address trustedURL a trusted plane, for a caller
// that names it (POST .../principals/{id}/trusted): a broker this daemon
// does not declare. An address no option gave is one that cannot be
// reached.
func WithTrustedAt(trustedURL string, plane TrustedPlane) Option {
	return func(d *Daemon) {
		if d.trustedAt == nil {
			d.trustedAt = map[string]TrustedPlane{}
		}
		d.trustedAt[trustedURL] = plane
	}
}

// The contract's pattern for a principal's id.
var principalID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

// Principal is a principal of the first namespace, as the daemon holds it:
// what a test reads to see what the code under test made.
func (d *Daemon) Principal(id string) (genv1.Principal, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	p, ok := d.principals[volumeKey{d.namespaces[0], id}]
	if !ok {
		return genv1.Principal{}, false
	}
	return clonePrincipal(*p), true
}

// VolumePrincipal is the principal a volume of the first namespace is bound
// to, empty for none, and whether there is such a volume.
func (d *Daemon) VolumePrincipal(name string) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	v, ok := d.volumes[volumeKey{d.namespaces[0], name}]
	if !ok {
		return "", false
	}
	return v.principal, true
}

func clonePrincipal(p genv1.Principal) genv1.Principal {
	p.Labels = maps.Clone(p.Labels)
	return p
}

func (d *Daemon) listPrincipals(w http.ResponseWriter, r *http.Request) {
	namespace := r.PathValue("namespace")
	d.mu.Lock()
	defer d.mu.Unlock()
	page := genv1.PrincipalPage{Items: []genv1.Principal{}}
	for key, p := range d.principals {
		if key.namespace == namespace {
			page.Items = append(page.Items, clonePrincipal(*p))
		}
	}
	sort.Slice(page.Items, func(i, j int) bool { return page.Items[i].Id < page.Items[j].Id })
	reply(w, http.StatusOK, page)
}

// principalOf is the principal a path names, or an answer saying why not.
// Caller holds d.mu.
func (d *Daemon) principalOf(w http.ResponseWriter, r *http.Request) (*genv1.Principal, bool) {
	id := r.PathValue("principal")
	if !principalID.MatchString(id) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "a principal id is letters, digits, '_', '.' and '-', starting with a letter or a digit")
		return nil, false
	}
	p, ok := d.principals[volumeKey{r.PathValue("namespace"), id}]
	if !ok {
		refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, "no such principal: "+id)
		return nil, false
	}
	return p, true
}

func (d *Daemon) getPrincipal(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if p, ok := d.principalOf(w, r); ok {
		reply(w, http.StatusOK, clonePrincipal(*p))
	}
}

func (d *Daemon) putPrincipal(w http.ResponseWriter, r *http.Request) {
	namespace, id := r.PathValue("namespace"), r.PathValue("principal")
	var spec genv1.PrincipalSpec
	if err := strictly(r, &spec); err != nil {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, err.Error())
		return
	}
	if !principalID.MatchString(id) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "a principal id is letters, digits, '_', '.' and '-', starting with a letter or a digit")
		return
	}
	labels := map[string]string{}
	if spec.Labels != nil {
		labels = maps.Clone(*spec.Labels)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	// As the daemon: an id a deleted principal had is never made again,
	// because a broker may hold grants under it.
	if d.retired[volumeKey{namespace, id}] {
		refuse(w, http.StatusConflict, genv1.ErrorCodePrincipalRetired, "that id was a principal's, which was deleted, and an id is never reused: "+id)
		return
	}
	if existing, ok := d.principals[volumeKey{namespace, id}]; ok {
		existing.Labels = labels
		reply(w, http.StatusOK, clonePrincipal(*existing))
		return
	}
	p := &genv1.Principal{Id: id, Namespace: &namespace, Labels: labels, CreatedBy: d.creatorOf(r), CreatedAt: d.now().UTC()}
	if d.principals == nil {
		d.principals = map[volumeKey]*genv1.Principal{}
	}
	d.principals[volumeKey{namespace, id}] = p
	reply(w, http.StatusCreated, clonePrincipal(*p))
}

func (d *Daemon) deletePrincipal(w http.ResponseWriter, r *http.Request) {
	namespace, id := r.PathValue("namespace"), r.PathValue("principal")
	if !principalID.MatchString(id) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "a principal id is letters, digits, '_', '.' and '-'")
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	var using []string
	for _, s := range d.sandboxes {
		if s.in(namespace) && valueOf(s.record.Principal) == id {
			using = append(using, "sandbox "+s.record.Id.String())
		}
	}
	for key, v := range d.volumes {
		if key.namespace == namespace && v.principal == id {
			using = append(using, "volume "+key.name)
		}
	}
	if len(using) > 0 {
		sort.Strings(using)
		refuse(w, http.StatusConflict, genv1.ErrorCodePrincipalInUse, strings.Join(using, ", ")+" uses "+id)
		return
	}
	if _, ok := d.principals[volumeKey{namespace, id}]; ok {
		delete(d.principals, volumeKey{namespace, id})
		if d.retired == nil {
			d.retired = map[volumeKey]bool{}
		}
		d.retired[volumeKey{namespace, id}] = true
	}
	w.WriteHeader(http.StatusNoContent)
}

// getBrokerTrusted reads a broker's trusted plane, as the daemon does: what
// the plane answered, marked as the broker's, or the daemon's own refusal.
func (d *Daemon) getBrokerTrusted(w http.ResponseWriter, r *http.Request) {
	broker, path := r.PathValue("broker"), r.PathValue("path")
	if !trustedPath(path) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "a trusted plane is asked only for a path under .well-known/, with no '..' in it")
		return
	}
	d.mu.Lock()
	p, ok := d.principalOf(w, r)
	if !ok {
		d.mu.Unlock()
		return
	}
	principal := clonePrincipal(*p)
	declared := false
	for _, b := range d.brokers {
		declared = declared || b.Name == broker
	}
	plane := d.trusted[broker]
	d.mu.Unlock()
	switch {
	case !declared:
		refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, "no shared broker "+broker+" is declared")
		return
	case plane == nil:
		refuse(w, http.StatusNotFound, genv1.ErrorCodeNoTrustedPlane, "the broker has no trusted plane: "+broker)
		return
	}
	answerTrusted(w, plane, principal, path)
}

// readTrusted reads the trusted plane at the address the caller named, as
// the daemon does.
func (d *Daemon) readTrusted(w http.ResponseWriter, r *http.Request) {
	var body genv1.TrustedRead
	if err := strictly(r, &body); err != nil {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "the body is a TrustedRead: "+err.Error())
		return
	}
	if !trustedPath(body.Path) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "a trusted plane is asked only for a path under .well-known/, with no '..' in it")
		return
	}
	at := body.Upstream.TrustedUrl
	if at == nil || *at == "" {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "the upstream names no trustedUrl to read")
		return
	}
	trusted, err := url.Parse(*at)
	data, dataErr := url.Parse(body.Upstream.Url)
	if err != nil || dataErr != nil || trusted.Host == "" || data.Host == "" {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "the upstream's url and trustedUrl are addresses")
		return
	}
	if body.Upstream.Auth != nil && *body.Upstream.Auth == genv1.BrokerAuthJWT && trusted.Hostname() != data.Hostname() {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "with auth jwt, the trustedUrl is on the url's host, which its token is for")
		return
	}
	d.mu.Lock()
	p, ok := d.principalOf(w, r)
	if !ok {
		d.mu.Unlock()
		return
	}
	principal := clonePrincipal(*p)
	plane := d.trustedAt[*at]
	d.mu.Unlock()
	if plane == nil {
		refuse(w, http.StatusBadGateway, genv1.ErrorCodeUpstreamError, "the broker's trusted plane could not be reached: "+*at)
		return
	}
	answerTrusted(w, plane, principal, body.Path)
}

// answerTrusted is what plane answers, marked as the broker's.
func answerTrusted(w http.ResponseWriter, plane TrustedPlane, principal genv1.Principal, path string) {
	status, contentType, body := plane(principal, path)
	if len(body) > 1<<20 {
		refuse(w, http.StatusBadGateway, genv1.ErrorCodeUpstreamError, "the broker's trusted plane answered with more than 1 MiB")
		return
	}
	w.Header().Set("Runyard-Broker-Answered", "true")
	if contentType != "" {
		w.Header().Set("Content-Type", contentType)
	}
	w.WriteHeader(status)
	_, _ = w.Write(body)
}

// trustedPath is the daemon's rule for what a trusted plane is asked for.
func trustedPath(path string) bool {
	if !strings.HasPrefix(path, ".well-known/") || len(path) > 1024 || strings.ContainsAny(path, "?#\\") {
		return false
	}
	for segment := range strings.SplitSeq(path, "/") {
		if segment == "" || segment == "." || segment == ".." {
			return false
		}
	}
	return true
}

func (d *Daemon) moveVolume(w http.ResponseWriter, r *http.Request) {
	namespace, name := r.PathValue("namespace"), r.PathValue("name")
	var body genv1.VolumePrincipal
	if err := strictly(r, &body); err != nil || !principalID.MatchString(body.Principal) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "the body is {\"principal\": \"<id>\"}")
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.principals[volumeKey{namespace, body.Principal}]; !ok {
		refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, "no such principal: "+body.Principal)
		return
	}
	v, ok := d.volumes[volumeKey{namespace, name}]
	if !ok {
		refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, "no such volume: "+name)
		return
	}
	if v.holder != nil {
		refuse(w, http.StatusConflict, genv1.ErrorCodeVolumeInUse, fmt.Sprintf("volume %s is held by sandbox %s", name, *v.holder))
		return
	}
	v.principal = body.Principal
	reply(w, http.StatusOK, volumeOf(namespace, name, v))
}

// actsFor is why a sandbox in namespace may not be bound to principal and
// have v, or empty. Caller holds d.mu.
func (d *Daemon) actsFor(namespace, principal string, v *volume) (genv1.ErrorCode, int, string) {
	if principal != "" {
		if _, ok := d.principals[volumeKey{namespace, principal}]; !ok {
			return genv1.ErrorCodeNotFound, http.StatusNotFound, "no such principal: " + principal
		}
	}
	if v != nil && v.principal != "" && v.principal != principal {
		return genv1.ErrorCodeVolumePrincipal, http.StatusConflict, "that volume is bound to another principal than the one this sandbox is bound to"
	}
	return "", 0, ""
}
