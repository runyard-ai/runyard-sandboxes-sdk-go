package fakedaemon

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// The tunnels plane: tunnels kept on the sandbox, as the daemon keeps them,
// served — there is no relay or gateway here — by a relay that is always
// docked at a gateway on tunnels.invalid, in the namespace "fake". What a
// visitor did is what a test recorded with RecordVisit, and who an admin let
// in from a tunnel's share page is who a test added with AddTunnelGuest.

// tunnelName is what a tunnel may be called: the start of a hostname label.
var tunnelName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,28}[a-z0-9])?$`)

var tokenID = regexp.MustCompile(`^[a-z0-9]{8}$`)

type tunnel struct {
	record genv1.Tunnel
	visits []genv1.TunnelRequest
	// guests are who its admins let in from its share page, oldest first.
	guests []genv1.TunnelGuest
}

// corners are where a tunnel's Share button may sit.
var corners = []genv1.TunnelOverlayCorner{genv1.BottomRight, genv1.BottomLeft, genv1.TopRight, genv1.TopLeft}

// TunnelDomain and TunnelNamespace are where the fake serves every tunnel.
const (
	TunnelDomain    = "tunnels.invalid"
	TunnelNamespace = "fake"
)

// Tunnels is a sandbox's tunnels now, as the daemon would report them.
func (d *Daemon) Tunnels(id genv1.SandboxID) []genv1.Tunnel {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sandboxes[id]
	if !ok {
		return nil
	}
	out := make([]genv1.Tunnel, 0, len(s.tunnels))
	for _, t := range s.tunnels {
		out = append(out, d.viewOf(s, t))
	}
	return out
}

// RecordVisit is a request a visitor made to a tunnel, as the gateway would
// have recorded it: the newest is answered first.
func (d *Daemon) RecordVisit(id genv1.SandboxID, name string, request genv1.TunnelRequest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.sandboxes[id]; ok {
		if t := findTunnel(s, name); t != nil {
			t.visits = append(t.visits, request)
		}
	}
}

// AddTunnelGuest is a person one of a tunnel's admins let in from its share
// page, as the gateway would keep them: listed after those let in before.
func (d *Daemon) AddTunnelGuest(id genv1.SandboxID, name, email, by string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, ok := d.sandboxes[id]; ok {
		if t := findTunnel(s, name); t != nil {
			t.guests = append(t.guests, genv1.TunnelGuest{Email: email, By: by, At: d.now().UTC()})
		}
	}
}

func findTunnel(s *sandbox, name string) *tunnel {
	for _, t := range s.tunnels {
		if t.record.Name == name {
			return t
		}
	}
	return nil
}

// viewOf is a tunnel as the daemon answers it, with its status as it is now.
func (d *Daemon) viewOf(s *sandbox, t *tunnel) genv1.Tunnel {
	view := t.record
	view.Tokens = append([]genv1.TunnelToken{}, t.record.Tokens...)
	// The daemon spells the Share button out whole, defaults and all.
	hidden, corner := t.record.Overlay.Hidden != nil && *t.record.Overlay.Hidden, genv1.BottomRight
	if t.record.Overlay.Corner != nil {
		corner = *t.record.Overlay.Corner
	}
	view.Overlay = genv1.TunnelOverlay{Hidden: &hidden, Corner: &corner}
	url := "https://" + t.record.Slug + "--" + TunnelNamespace + "." + TunnelDomain + "/"
	view.Url = &url
	switch {
	case t.record.Paused:
		view.Status = genv1.TunnelStatus{State: genv1.Paused}
	case t.record.ExpiresAt != nil && !d.now().Before(*t.record.ExpiresAt):
		view.Status = genv1.TunnelStatus{State: genv1.Expired}
	case s.record.State != genv1.SandboxStateReady:
		message := "the sandbox is " + string(s.record.State)
		view.Status = genv1.TunnelStatus{State: genv1.SandboxNotRunning, Message: &message}
	default:
		none := 0
		view.Status = genv1.TunnelStatus{State: genv1.Served, Connections: &none}
	}
	return view
}

func randomOf(n int, alphabet string) string {
	raw := make([]byte, n)
	_, _ = rand.Read(raw) // never fails: crypto/rand crashes the program instead
	for i, b := range raw {
		raw[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(raw)
}

func (d *Daemon) validTunnel(w http.ResponseWriter, r *http.Request) (string, bool) {
	name := r.PathValue("tunnel")
	if !tunnelName.MatchString(name) || regexp.MustCompile(`--`).MatchString(name) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, fmt.Sprintf("%q is not a tunnel's name", name))
		return "", false
	}
	return name, true
}

// tunnelOf is the tunnel a request names, answered 404 when there is none.
func (d *Daemon) tunnelOf(w http.ResponseWriter, r *http.Request) (*sandbox, *tunnel, bool) {
	name, ok := d.validTunnel(w, r)
	if !ok {
		return nil, nil, false
	}
	s, ok := d.lookup(w, r)
	if !ok {
		return nil, nil, false
	}
	t := findTunnel(s, name)
	if t == nil {
		refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, fmt.Sprintf("this sandbox has no tunnel %q", name))
		return nil, nil, false
	}
	return s, t, true
}

// listHostTunnels pages as the daemon does: oldest first by createdAt, then
// by slug, from the position a cursor names.
func (d *Daemon) listHostTunnels(w http.ResponseWriter, r *http.Request) {
	limit, after, ok := hostTunnelsPage(w, r)
	if !ok {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	gateway, domain, namespace := "https://gateway.invalid", TunnelDomain, TunnelNamespace
	out := genv1.HostTunnels{Items: []genv1.Tunnel{},
		Relay: genv1.TunnelRelay{Reachable: true, Docked: true, Gateway: &gateway, Domain: &domain, Namespace: &namespace}}
	var all []genv1.Tunnel
	for _, id := range d.order {
		s := d.sandboxes[id]
		for _, t := range s.tunnels {
			view := d.viewOf(s, t)
			sandboxID := id
			view.SandboxId = &sandboxID
			all = append(all, view)
		}
	}
	slices.SortStableFunc(all, func(a, b genv1.Tunnel) int { return strings.Compare(tunnelKey(a), tunnelKey(b)) })
	for _, t := range all {
		if after != "" && tunnelKey(t) <= after {
			continue
		}
		if limit > 0 && len(out.Items) == limit {
			next := base64.RawURLEncoding.EncodeToString([]byte(tunnelKey(out.Items[len(out.Items)-1])))
			out.NextCursor = &next
			break
		}
		out.Items = append(out.Items, t)
	}
	reply(w, http.StatusOK, out)
}

// tunnelKey orders the host's tunnels: a fixed-width time sorts as text.
func tunnelKey(t genv1.Tunnel) string {
	return t.CreatedAt.UTC().Format("2006-01-02T15:04:05.000000000Z") + "/" + t.Slug
}

// hostTunnelsPage reads `limit` and `cursor`, refusing what the daemon would.
func hostTunnelsPage(w http.ResponseWriter, r *http.Request) (limit int, after string, ok bool) {
	query := r.URL.Query()
	if raw := query.Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > 500 {
			refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "limit is between 1 and 500")
			return 0, "", false
		}
		limit = n
	}
	if raw := query.Get("cursor"); raw != "" {
		decoded, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil || !strings.Contains(string(decoded), "/") {
			refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "that cursor is not one this daemon gave out")
			return 0, "", false
		}
		after = string(decoded)
	}
	return limit, after, true
}

func (d *Daemon) listTunnels(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.lookup(w, r)
	if !ok {
		return
	}
	items := make([]genv1.Tunnel, 0, len(s.tunnels))
	for _, t := range s.tunnels {
		items = append(items, d.viewOf(s, t))
	}
	reply(w, http.StatusOK, map[string][]genv1.Tunnel{"items": items})
}

func (d *Daemon) getTunnel(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if s, t, ok := d.tunnelOf(w, r); ok {
		reply(w, http.StatusOK, d.viewOf(s, t))
	}
}

func (d *Daemon) putTunnel(w http.ResponseWriter, r *http.Request) {
	name, ok := d.validTunnel(w, r)
	if !ok {
		return
	}
	var spec genv1.TunnelSpec
	if err := strictly(r, &spec); err != nil {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "the body is not a tunnel: "+err.Error())
		return
	}
	if spec.Port < 1 || spec.Port > 65535 {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, fmt.Sprintf("port %d is not a TCP port", spec.Port))
		return
	}
	if spec.Overlay != nil && spec.Overlay.Corner != nil && !slices.Contains(corners, *spec.Overlay.Corner) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, fmt.Sprintf("overlay.corner %q is not a corner", *spec.Overlay.Corner))
		return
	}
	if spec.ExpiresAt != nil && !spec.ExpiresAt.After(d.now()) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "expiresAt has passed")
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.lookup(w, r)
	if !ok {
		return
	}
	if s.record.State == genv1.SandboxStateGone {
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "the sandbox is gone")
		return
	}
	t := findTunnel(s, name)
	status := http.StatusOK
	if t == nil {
		if len(s.tunnels) >= 16 {
			refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "this sandbox already has 16 tunnels")
			return
		}
		t = &tunnel{record: genv1.Tunnel{Name: name, Slug: name + "-" + randomOf(6, "abcdefghijklmnopqrstuvwxyz234567"),
			CreatedAt: d.now().UTC(), Tokens: []genv1.TunnelToken{}}}
		s.tunnels = append(s.tunnels, t)
		status = http.StatusCreated
	}
	access := genv1.TunnelAccess{}
	if spec.Access != nil {
		access = *spec.Access
	}
	t.record.Port, t.record.Description, t.record.ExpiresAt, t.record.Access = spec.Port, spec.Description, spec.ExpiresAt, access
	t.record.Paused = spec.Paused != nil && *spec.Paused
	t.record.Overlay = genv1.TunnelOverlay{}
	if spec.Overlay != nil {
		t.record.Overlay = *spec.Overlay
	}
	reply(w, status, d.viewOf(s, t))
}

func (d *Daemon) deleteTunnel(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, t, ok := d.tunnelOf(w, r)
	if !ok {
		return
	}
	for i := range s.tunnels {
		if s.tunnels[i] == t {
			s.tunnels = append(s.tunnels[:i], s.tunnels[i+1:]...)
			break
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (d *Daemon) pausingTunnel(paused bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		d.mu.Lock()
		defer d.mu.Unlock()
		if s, t, ok := d.tunnelOf(w, r); ok {
			t.record.Paused = paused
			reply(w, http.StatusOK, d.viewOf(s, t))
		}
	}
}

func (d *Daemon) createTunnelToken(w http.ResponseWriter, r *http.Request) {
	var body genv1.TunnelTokenRequest
	if err := strictly(r, &body); err != nil || body.Comment == "" {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "a token is asked for with a comment")
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, t, ok := d.tunnelOf(w, r)
	if !ok {
		return
	}
	live := 0
	for _, token := range t.record.Tokens {
		if !token.Revoked {
			live++
		}
	}
	if live >= 16 {
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "this tunnel already has 16 live tokens")
		return
	}
	id := randomOf(8, "abcdefghijklmnopqrstuvwxyz0123456789")
	record := genv1.TunnelToken{Id: id, Comment: body.Comment, CreatedAt: d.now().UTC()}
	t.record.Tokens = append(t.record.Tokens, record)
	reply(w, http.StatusCreated, genv1.TunnelTokenCreated{Id: id, Comment: body.Comment, CreatedAt: record.CreatedAt,
		Token: "ryt_" + id + "_" + randomOf(32, "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789")})
}

func (d *Daemon) revokeTunnelToken(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("token")
	if !tokenID.MatchString(id) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, fmt.Sprintf("%q is not a token's id", id))
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, t, ok := d.tunnelOf(w, r)
	if !ok {
		return
	}
	for i := range t.record.Tokens {
		token := &t.record.Tokens[i]
		if token.Id != id {
			continue
		}
		if !token.Revoked {
			now := d.now().UTC()
			token.Revoked, token.RevokedAt = true, &now
			if reason := r.URL.Query().Get("reason"); reason != "" {
				token.RevokedReason = &reason
			}
		}
		reply(w, http.StatusOK, *token)
		return
	}
	refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, fmt.Sprintf("the tunnel has no token %q", id))
}

func (d *Daemon) tunnelActivity(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, t, ok := d.tunnelOf(w, r)
	if !ok {
		return
	}
	items := make([]genv1.TunnelRequest, 0, len(t.visits))
	for _, visit := range slices.Backward(t.visits) {
		items = append(items, visit)
	}
	reply(w, http.StatusOK, genv1.TunnelActivityPage{Items: items})
}

func (d *Daemon) listTunnelGuests(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, t, ok := d.tunnelOf(w, r); ok {
		reply(w, http.StatusOK, genv1.TunnelGuestList{Items: append([]genv1.TunnelGuest{}, t.guests...)})
	}
}

func (d *Daemon) revokeTunnelGuest(w http.ResponseWriter, r *http.Request) {
	email := r.PathValue("email")
	if email == "" || len(email) > 254 || !strings.Contains(email, "@") {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, fmt.Sprintf("%q is not an address", email))
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	_, t, ok := d.tunnelOf(w, r)
	if !ok {
		return
	}
	for i, guest := range t.guests {
		if strings.EqualFold(guest.Email, email) {
			t.guests = slices.Delete(t.guests, i, i+1)
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, "the tunnel has no guest "+email)
}
