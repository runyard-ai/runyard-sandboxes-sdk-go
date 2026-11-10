package fakeserver

import (
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// The runners of a server, as much as a program that adds, lists and drains
// them needs: join tokens minted and listed, a host that joins with one
// (Join), and the runner it becomes, drained and revoked. Nothing here runs
// on a runner, and a runner is never held, so none of them is ever allowed an
// address. Approving one is an admin's word, as on the server: pending to
// online, saying when and by whom. Moving a volume between runners is not
// in the fake, which refuses it as `not_implemented`.
//
// A runner's brokers are what a test says it reports (Provide). No runner is
// held to being approved, since none could be: so a rotation of its sandbox
// CA is asked of any runner not revoked, and is asked, since no runner here
// makes one.

// Provide is the runner hostID reporting that it provides brokers, as a
// runner whose brokers.provides names them does.
func (d *Server) Provide(tb testing.TB, hostID string, brokers ...string) {
	tb.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	runner, ok := d.runners[hostID]
	if !ok {
		tb.Fatalf("fakeserver: no runner %s has joined", hostID)
	}
	provides := slices.Clone(brokers)
	runner.Provides = &provides
}

// joinToken is a token minted, and its secret.
type joinToken struct {
	record genv1.JoinToken
	secret string
}

// runnerName is the contract's RunnerName; hostID how a hostID is written,
// which a name given is not.
var (
	runnerName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	hostID     = regexp.MustCompile(`^r-\d+$`)
)

// Consumed is what a test says was used, for GET /v1/usage: in the hour it
// is recorded in (Use), on Runner, by Sandbox for Principal. The fake meters
// nothing.
type Consumed struct {
	Runner, Sandbox, Principal string
	Usage                      genv1.Usage
}

type consumed struct {
	Consumed
	hour time.Time
}

// Use records what was consumed now, as the server would have measured it.
func (d *Server) Use(c Consumed) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.usage = append(d.usage, consumed{Consumed: c, hour: d.now().UTC().Truncate(time.Hour)})
}

// Join is a host joining with secret, as `runyard-runner join` does: the
// runner it is, pending, called what its token names it. A secret that
// cannot enrol fails the test.
func (d *Server) Join(tb testing.TB, secret string) genv1.Runner {
	tb.Helper()
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now().UTC()
	i := slices.IndexFunc(d.joinTokens, func(t *joinToken) bool { return t.secret == secret })
	if i < 0 {
		tb.Fatalf("fakeserver: no join token is %q", secret)
	}
	token := d.joinTokens[i]
	if t := token.record; t.Revoked || !now.Before(t.ExpiresAt) || t.Uses >= t.MaxUses {
		tb.Fatalf("fakeserver: join token %s cannot enrol: revoked, expired or used up", t.Id)
	}
	if token.record.HostID != nil {
		runner := d.runners[*token.record.HostID]
		token.record.Uses++
		runner.State, runner.JoinedAt, runner.ApprovedAt, runner.ApprovedBy = genv1.RunnerStateRunnerPending, now, nil, nil
		return *runner
	}
	id := fmt.Sprintf("r-%02d", len(d.runnerOrder)+1)
	name := id
	if given := token.record.RunnerName; given != nil {
		for n := token.record.Uses + 1; ; n++ {
			name = *given
			if token.record.MaxUses > 1 {
				name = fmt.Sprintf("%s-%02d", *given, n)
			}
			if !d.nameTaken(name, token.record.Id) {
				break
			}
		}
	}
	token.record.Uses++
	runner := &genv1.Runner{
		Id: id, Name: name, State: genv1.RunnerStateRunnerPending, Labels: maps.Clone(token.record.Labels),
		Version: "0.27.0", KeyKind: genv1.KeyKindFile, AllowedSources: slices.Clone(token.record.AllowedSources),
		EnrolledFrom: "203.0.113.24", JoinedAt: now, JoinedBy: token.record.CreatedBy, CertificateNotAfter: now.Add(30 * 24 * time.Hour),
		RecordKey: "-----BEGIN PUBLIC KEY-----\n-----END PUBLIC KEY-----\n", BrokerKey: "-----BEGIN PUBLIC KEY-----\n-----END PUBLIC KEY-----\n",
	}
	if d.runners == nil {
		d.runners = map[string]*genv1.Runner{}
	}
	d.runners[id] = runner
	d.runnerOrder = append(d.runnerOrder, id)
	return *runner
}

// nameTaken says whether a runner not revoked is called name, or a token
// still usable but except will call one that.
func (d *Server) nameTaken(name, except string) bool {
	for _, runner := range d.runners {
		if runner.Name == name && runner.State != genv1.RunnerStateRunnerRevoked {
			return true
		}
	}
	now := d.now()
	for _, t := range d.joinTokens {
		r := t.record
		if r.Id != except && r.RunnerName != nil && *r.RunnerName == name && !r.Revoked && now.Before(r.ExpiresAt) && r.Uses < r.MaxUses {
			return true
		}
	}
	return false
}

func (d *Server) createJoinToken(w http.ResponseWriter, r *http.Request) {
	var ask genv1.JoinTokenRequest
	if err := strictly(r, &ask); err != nil {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, err.Error())
		return
	}
	life := time.Hour
	if ask.ExpiresIn != nil {
		var err error
		if life, err = time.ParseDuration(*ask.ExpiresIn); err != nil || life < 15*time.Minute || life > 24*time.Hour {
			refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "expiresIn is a duration, 15 minutes to a day")
			return
		}
	}
	uses := 1
	if ask.MaxUses != nil {
		uses = *ask.MaxUses
	}
	if uses < 1 || uses > 1000 || (ask.HostID != nil && uses != 1) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "a join token enrols 1 to 1000 hosts, and one bound to a runner enrols it once")
		return
	}
	if name := ask.RunnerName; name != nil {
		if !runnerName.MatchString(*name) || hostID.MatchString(*name) || ask.HostID != nil || (uses > 1 && len(*name) > 58) {
			refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "a runner's name is lowercase letters, digits and hyphens, not written as a hostID is, and a Rejoin keeps the runner's own")
			return
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if ask.HostID != nil {
		runner, ok := d.runners[*ask.HostID]
		switch {
		case !ok:
			refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, "no runner "+*ask.HostID+" has enrolled here, so there is none to rejoin")
			return
		case runner.State == genv1.RunnerStateRunnerRevoked:
			refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "runner "+*ask.HostID+" was revoked, and nothing brings a revoked runner back")
			return
		}
	}
	if ask.RunnerName != nil && d.nameTaken(*ask.RunnerName, "") {
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "a runner is called "+*ask.RunnerName+" already, or a join token not yet used will call one that")
		return
	}
	now := d.now().UTC()
	id := fmt.Sprintf("%012x", len(d.joinTokens)+1)
	token := &joinToken{
		record: genv1.JoinToken{
			Id: id, Name: ask.Name, RunnerName: ask.RunnerName, Labels: map[string]string{}, AllowedSources: []string{},
			CreatedAt: now, CreatedBy: "admin@example.com", ExpiresAt: now.Add(life), MaxUses: uses, HostID: ask.HostID,
		},
		secret: "rjt_" + id + "_" + strings.Repeat("s", 32),
	}
	if ask.Labels != nil {
		token.record.Labels = maps.Clone(*ask.Labels)
	}
	if ask.AllowedSources != nil {
		token.record.AllowedSources = slices.Clone(*ask.AllowedSources)
	}
	d.joinTokens = append(d.joinTokens, token)
	t := token.record
	reply(w, http.StatusCreated, genv1.JoinTokenCreated{
		Id: t.Id, Name: t.Name, RunnerName: t.RunnerName, Labels: t.Labels, AllowedSources: t.AllowedSources, CreatedAt: t.CreatedAt,
		CreatedBy: t.CreatedBy, ExpiresAt: t.ExpiresAt, MaxUses: t.MaxUses, Uses: t.Uses, HostID: t.HostID, Revoked: t.Revoked,
		Secret: token.secret,
	})
}

func (d *Server) listJoinTokens(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	page := genv1.JoinTokenPage{Items: []genv1.JoinToken{}}
	for _, t := range slices.Backward(d.joinTokens) {
		page.Items = append(page.Items, t.record)
	}
	reply(w, http.StatusOK, page)
}

func (d *Server) updateJoinToken(w http.ResponseWriter, r *http.Request) {
	var change genv1.JoinTokenUpdate
	if err := strictly(r, &change); err != nil || change.AllowedSources == nil {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "say where it may be used from: allowedSources ([] for anywhere)")
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	i := slices.IndexFunc(d.joinTokens, func(t *joinToken) bool { return t.record.Id == r.PathValue("tokenId") })
	if i < 0 {
		refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, "no join token "+r.PathValue("tokenId")+" was minted here")
		return
	}
	t := &d.joinTokens[i].record
	if t.Revoked || !d.now().Before(t.ExpiresAt) || t.Uses >= t.MaxUses {
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "join token "+t.Id+" is revoked, expired or used up, and enrols nothing more")
		return
	}
	t.AllowedSources = slices.Clone(change.AllowedSources)
	reply(w, http.StatusOK, *t)
}

func (d *Server) revokeJoinToken(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, t := range d.joinTokens {
		if t.record.Id == r.PathValue("tokenId") {
			t.record.Revoked = true
			w.WriteHeader(http.StatusNoContent)
			return
		}
	}
	refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, "no join token "+r.PathValue("tokenId")+" was minted here")
}

func (d *Server) listRunners(w http.ResponseWriter, r *http.Request) {
	unlisted := false
	if asked := r.URL.Query().Get("unlisted"); asked != "" {
		var err error
		if unlisted, err = strconv.ParseBool(asked); err != nil {
			refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "unlisted is true or false, not "+asked)
			return
		}
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	page := genv1.RunnerPage{Items: []genv1.Runner{}}
	for _, id := range d.runnerOrder {
		if runner := d.runners[id]; runner.UnlistedAt == nil || unlisted {
			page.Items = append(page.Items, *runner)
		}
	}
	reply(w, http.StatusOK, page)
}

// unlistRunner removes a revoked runner from the list, once: as the
// server, one removed already is answered as it is.
func (d *Server) unlistRunner(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	runner := d.runnerOf(w, r)
	if runner == nil {
		return
	}
	if runner.State != genv1.RunnerStateRunnerRevoked {
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "runner "+runner.Id+" is not revoked: only a revoked runner is removed from the list")
		return
	}
	if runner.UnlistedAt == nil {
		now, by := d.now().UTC(), openapi_types.Email("admin@example.com")
		runner.UnlistedAt, runner.UnlistedBy = &now, &by
	}
	reply(w, http.StatusOK, *runner)
}

// rotateRunnerCA asks a runner to rotate its sandbox CA: since when, kept
// as it was asked first while it is asked.
func (d *Server) rotateRunnerCA(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	runner := d.runnerOf(w, r)
	if runner == nil {
		return
	}
	if runner.State == genv1.RunnerStateRunnerRevoked {
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "runner "+runner.Id+" is revoked, and has no CA in the bundle to rotate from")
		return
	}
	if runner.CaRotationAskedAt == nil {
		now := d.now().UTC()
		runner.CaRotationAskedAt = &now
	}
	reply(w, http.StatusAccepted, *runner)
}

// approveRunner puts a pending runner in the bundle: online, approved now
// by whoever the body says, or by the fake's administrator.
func (d *Server) approveRunner(w http.ResponseWriter, r *http.Request) {
	var ask genv1.RunnerApprove
	if r.ContentLength != 0 {
		if err := strictly(r, &ask); err != nil && !errors.Is(err, io.EOF) {
			refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "the body is not an approval: "+err.Error())
			return
		}
	}
	by := "admin@example.com"
	if ask.ApprovedBy != nil {
		if len(*ask.ApprovedBy) < 1 || len(*ask.ApprovedBy) > 200 {
			refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "approvedBy is 1 to 200 characters")
			return
		}
		by = *ask.ApprovedBy
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	runner := d.runnerOf(w, r)
	if runner == nil {
		return
	}
	if runner.State != genv1.RunnerStateRunnerPending || runner.ApprovedAt != nil {
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "runner "+runner.Id+" is not pending approval: it was approved already, or revoked")
		return
	}
	now := d.now().UTC()
	runner.State, runner.ApprovedAt, runner.ApprovedBy = genv1.RunnerStateRunnerOnline, &now, &by
	reply(w, http.StatusOK, *runner)
}

// runnerOf answers the runner the path names, under d.mu, or says there is
// none and returns nil.
func (d *Server) runnerOf(w http.ResponseWriter, r *http.Request) *genv1.Runner {
	runner, ok := d.runners[r.PathValue("runnerId")]
	if !ok {
		refuse(w, http.StatusNotFound, genv1.ErrorCodeNotFound, "no runner "+r.PathValue("runnerId")+" has enrolled here")
		return nil
	}
	return runner
}

func (d *Server) getRunner(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if runner := d.runnerOf(w, r); runner != nil {
		reply(w, http.StatusOK, *runner)
	}
}

func (d *Server) updateRunner(w http.ResponseWriter, r *http.Request) {
	var change genv1.RunnerUpdate
	if err := strictly(r, &change); err != nil || (change.AllowedSources == nil && change.Name == nil) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "say what to change: name, allowedSources ([] for anywhere), or both")
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	runner := d.runnerOf(w, r)
	if runner == nil {
		return
	}
	if runner.State == genv1.RunnerStateRunnerRevoked {
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "runner "+runner.Id+" was revoked")
		return
	}
	if name := change.Name; name != nil && *name != runner.Name {
		if d.nameTaken(*name, "") {
			refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "a runner is called "+*name+" already, or a join token not yet used will call one that")
			return
		}
		runner.Name = *name
	}
	if change.AllowedSources != nil {
		runner.AllowedSources = slices.Clone(*change.AllowedSources)
	}
	reply(w, http.StatusOK, *runner)
}

func (d *Server) allowRunnerAddress(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if runner := d.runnerOf(w, r); runner != nil {
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "runner "+runner.Id+" is not held: no fake runner ever is")
	}
}

// operate opens an operation of kind on the runner the path names, which
// becomes state; one already open of that kind is answered again.
func (d *Server) operate(kind genv1.RunnerOperationKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var ask genv1.RunnerDrainRequest
		if kind == genv1.OperationDrain && r.ContentLength != 0 {
			if err := strictly(r, &ask); err != nil {
				refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, err.Error())
				return
			}
		}
		d.mu.Lock()
		defer d.mu.Unlock()
		runner := d.runnerOf(w, r)
		if runner == nil {
			return
		}
		if open := runner.Operation; open != nil && open.Kind == kind && open.Step != genv1.OperationCancelled {
			reply(w, http.StatusAccepted, *open)
			return
		}
		if runner.State == genv1.RunnerStateRunnerRevoked {
			refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "runner "+runner.Id+" was revoked")
			return
		}
		now := d.now().UTC()
		d.operations++
		op := genv1.RunnerOperation{
			Id: d.operations, RunnerId: runner.Id, Kind: kind, RequestedBy: openapi_types.Email("admin@example.com"),
			RequestedAt: now, Step: genv1.OperationDraining, Pending: []genv1.RunnerOperationPending{}, UpdatedAt: &now,
		}
		if ask.RevokeWhenEmpty != nil {
			op.RevokeWhenEmpty = *ask.RevokeWhenEmpty
		}
		if kind == genv1.OperationRevoke {
			// Nothing of a fake runner's is anywhere to let go of.
			op.Step = genv1.OperationDone
			by := op.RequestedBy
			runner.State, runner.RevokedAt, runner.RevokedBy = genv1.RunnerStateRunnerRevoked, &now, &by
		} else {
			runner.State = genv1.RunnerStateRunnerDraining
		}
		runner.Operation = &op
		reply(w, http.StatusAccepted, op)
	}
}

func (d *Server) undrainRunner(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	runner := d.runnerOf(w, r)
	if runner == nil {
		return
	}
	if runner.State != genv1.RunnerStateRunnerDraining {
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "runner "+runner.Id+" is not draining")
		return
	}
	now := d.now().UTC()
	runner.Operation.Step, runner.Operation.UpdatedAt = genv1.OperationCancelled, &now
	runner.State = genv1.RunnerStateRunnerPending
	if runner.ApprovedAt != nil {
		runner.State = genv1.RunnerStateRunnerOnline
	}
	reply(w, http.StatusAccepted, *runner.Operation)
}

// settings is the workspace's runner settings, under d.mu: the server's
// defaults until changed.
func (d *Server) settings() *genv1.RunnerSettings {
	if d.runnerSettings == nil {
		d.runnerSettings = &genv1.RunnerSettings{
			HardwareKeys: genv1.HardwareKeysPreferred, TrustedMakers: []string{},
		}
	}
	return d.runnerSettings
}

func (d *Server) getRunnerSettings(w http.ResponseWriter, _ *http.Request) {
	d.mu.Lock()
	defer d.mu.Unlock()
	reply(w, http.StatusOK, *d.settings())
}

func (d *Server) updateRunnerSettings(w http.ResponseWriter, r *http.Request) {
	var change genv1.RunnerSettingsUpdate
	if err := strictly(r, &change); err != nil || change.HardwareKeys == nil {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "say hardwareKeys")
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	settings := d.settings()
	settings.HardwareKeys = *change.HardwareKeys
	reply(w, http.StatusOK, *settings)
}

// notInTheFake answers what the fake does not do, saying so, as a server
// answers a plane it has not built: a test that needs it intercepts it.
func notInTheFake(what string) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		refuse(w, http.StatusNotImplemented, genv1.ErrorCodeNotImplemented, "the fake server does not "+what+": Intercept it")
	}
}

func (d *Server) getUsage(w http.ResponseWriter, r *http.Request) {
	window := genv1.CostWindow7d
	if asked := r.URL.Query().Get("window"); asked != "" {
		window = genv1.CostWindow(asked)
	}
	span, ok := costWindows[window]
	by := genv1.UsageByRunner
	if asked := r.URL.Query().Get("groupBy"); asked != "" {
		by = genv1.UsageGroupBy(asked)
	}
	if !ok || !slices.Contains([]genv1.UsageGroupBy{genv1.UsageByRunner, genv1.UsageBySandbox, genv1.UsageByPrincipal, genv1.UsageByHour}, by) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "window is one of 24h, 7d, 30d, 90d, and groupBy one of runner, sandbox, principal, hour")
		return
	}
	only := r.URL.Query().Get("runner")
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now().UTC()
	from := now.Truncate(time.Hour).Add(-span.back + time.Hour)
	report := genv1.UsageReport{Window: window, From: from, To: now, GroupBy: by, Total: noUsage(), Groups: []genv1.UsageGroup{}}
	groups := map[string]*genv1.UsageGroup{}
	var keys []string
	if by == genv1.UsageByHour {
		for hour := from; !hour.After(now); hour = hour.Add(time.Hour) {
			key := hour.Format(time.RFC3339)
			groups[key] = &genv1.UsageGroup{Hour: &hour, Usage: noUsage()}
			keys = append(keys, key)
		}
	}
	for _, c := range d.usage {
		if c.hour.Before(from) || (only != "" && c.Runner != only) {
			continue
		}
		add(&report.Total, c.Usage)
		key := map[genv1.UsageGroupBy]string{
			genv1.UsageByRunner: c.Runner, genv1.UsageBySandbox: c.Sandbox, genv1.UsageByPrincipal: c.Principal,
			genv1.UsageByHour: c.hour.Format(time.RFC3339),
		}[by]
		group, ok := groups[key]
		if !ok {
			group = &genv1.UsageGroup{Usage: noUsage()}
			if key != "" {
				switch by {
				case genv1.UsageByRunner:
					group.Runner = &c.Runner
				case genv1.UsageBySandbox:
					group.Sandbox = &c.Sandbox
				case genv1.UsageByPrincipal:
					group.Principal = &c.Principal
				case genv1.UsageByHour:
					// Every hour's group was made above.
				}
			}
			groups[key] = group
			keys = append(keys, key)
		}
		add(&group.Usage, c.Usage)
	}
	if by != genv1.UsageByHour {
		slices.Sort(keys)
	}
	for _, key := range keys {
		report.Groups = append(report.Groups, *groups[key])
	}
	reply(w, http.StatusOK, report)
}

func noUsage() genv1.Usage { return genv1.Usage{Models: []genv1.ModelUsage{}} }

// add adds u to total, model by model.
func add(total *genv1.Usage, u genv1.Usage) {
	total.SandboxSeconds += u.SandboxSeconds
	total.VcpuSeconds += u.VcpuSeconds
	total.MemoryGiBSeconds += u.MemoryGiBSeconds
	total.VolumeGiBHours += u.VolumeGiBHours
	total.ImageGiBHours += u.ImageGiBHours
	total.TunnelSeconds += u.TunnelSeconds
	total.Sandboxes += u.Sandboxes
	total.Tunnels += u.Tunnels
	total.BrokerCalls += u.BrokerCalls
	total.BrokerBytesIn += u.BrokerBytesIn
	total.BrokerBytesOut += u.BrokerBytesOut
	for _, m := range u.Models {
		i := slices.IndexFunc(total.Models, func(t genv1.ModelUsage) bool { return t.Model == m.Model })
		if i < 0 {
			total.Models = append(total.Models, genv1.ModelUsage{Model: m.Model})
			i = len(total.Models) - 1
		}
		t := &total.Models[i]
		t.Calls += m.Calls
		t.Input += m.Input
		t.Output += m.Output
		t.CacheRead += m.CacheRead
		t.CacheWrite += m.CacheWrite
		t.CacheWrite1h += m.CacheWrite1h
		t.Reasoning += m.Reasoning
	}
}
