package fakeserver

import (
	"net/http"
	"slices"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// The per-turn grant: a model credential lent to a broker a sandbox's runner
// provides, for one turn. The fake keeps it on the sandbox, as its runner
// would, in memory and until it is taken back, the sandbox stops or goes,
// or ExpiresAt; Grant is the broker reading it. A shared broker declared
// with WithRunnerBrokers is one every runner here provides taking grants.

// grantAtMost is how far ahead a grant may expire: one turn's, lent again
// while the turn lasts.
const grantAtMost = 24 * time.Hour

// WithRunnerBrokers declares shared brokers, by name, as `url: runner`:
// each the one the sandbox's runner provides for itself, taking grants.
func WithRunnerBrokers(names ...string) Option {
	return func(d *Server) {
		for _, name := range names {
			d.brokers = append(d.brokers, genv1.Broker{Name: name, Url: "runner"})
		}
	}
}

// Grant is what the broker of that name would be served for sandbox id now:
// the grant lent for it, and false when there is none or it has expired.
func (d *Server) Grant(id genv1.SandboxID, broker string) (genv1.GrantWrite, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.sandboxes[id]
	if !ok {
		return genv1.GrantWrite{}, false
	}
	grant, ok := s.grants[broker]
	if !ok || !d.now().Before(grant.ExpiresAt) {
		return genv1.GrantWrite{}, false
	}
	return grant, true
}

// providesGrants says whether broker is declared `url: runner`.
func (d *Server) providesGrants(broker string) bool {
	return slices.ContainsFunc(d.brokers, func(b genv1.Broker) bool { return b.Name == broker && b.Url == "runner" })
}

func (d *Server) putGrant(w http.ResponseWriter, r *http.Request) {
	broker := r.PathValue("broker")
	if !brokerName.MatchString(broker) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, notABrokerName)
		return
	}
	var grant genv1.GrantWrite
	if err := strictly(r, &grant); err != nil {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, "the body is not a grant: "+err.Error())
		return
	}
	if wrong := grantWrong(grant); wrong != "" {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, wrong)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.lookup(w, r)
	if !ok {
		return
	}
	now := d.now()
	switch {
	case !d.providesGrants(broker):
		refuse(w, http.StatusConflict, genv1.ErrorCodeNotProvided, "no shared broker "+broker+" is declared url: runner, so no runner takes a grant for it")
		return
	case s.record.State != genv1.SandboxStateReady && s.record.State != genv1.SandboxStatePaused:
		refuse(w, http.StatusConflict, genv1.ErrorCodeConflict, "the sandbox is "+string(s.record.State)+": a grant is lent to a running one")
		return
	case !grant.ExpiresAt.After(now) || grant.ExpiresAt.After(now.Add(grantAtMost)):
		refuse(w, http.StatusUnprocessableEntity, genv1.ErrorCodeBadRequest, "expiresAt is in the future, and at most 24 hours ahead")
		return
	case chatGPT(grant) != (grant.Account != nil):
		refuse(w, http.StatusUnprocessableEntity, genv1.ErrorCodeBadRequest, "account is a ChatGPT sign-in's, a subscription to openai, and it needs one")
		return
	}
	if s.grants == nil {
		s.grants = map[string]genv1.GrantWrite{}
	}
	s.grants[broker] = grant
	w.WriteHeader(http.StatusNoContent)
}

func (d *Server) deleteGrant(w http.ResponseWriter, r *http.Request) {
	broker := r.PathValue("broker")
	if !brokerName.MatchString(broker) {
		refuse(w, http.StatusBadRequest, genv1.ErrorCodeBadRequest, notABrokerName)
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	s, ok := d.lookup(w, r)
	if !ok {
		return
	}
	delete(s.grants, broker)
	w.WriteHeader(http.StatusNoContent)
}

// chatGPT is a ChatGPT sign-in, the one grant that names an account.
func chatGPT(g genv1.GrantWrite) bool {
	return g.Kind == genv1.GrantSubscription && g.Api == genv1.GrantOpenAI
}

// grantWrong is what the contract refuses of a grant before anything is
// looked up, or nothing.
func grantWrong(g genv1.GrantWrite) string {
	switch {
	case g.Kind != genv1.GrantAPIKey && g.Kind != genv1.GrantSubscription:
		return "kind is apiKey or subscription"
	case g.Api != genv1.GrantAnthropic && g.Api != genv1.GrantOpenAI:
		return "api is anthropic or openai"
	case len(g.Credential) < 1 || len(g.Credential) > 16384:
		return "credential is 1 to 16384 characters"
	case len(g.Turn) < 1 || len(g.Turn) > 200:
		return "turn is 1 to 200 characters"
	case g.Account != nil && (len(*g.Account) < 1 || len(*g.Account) > 200):
		return "account is 1 to 200 characters"
	}
	return ""
}
