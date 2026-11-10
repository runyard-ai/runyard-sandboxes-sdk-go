package fakeserver

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// A grant is kept on its sandbox as its runner keeps it: replaced by the
// next, taken back, forgotten at its expiry and when the sandbox stops or is
// deleted; what the server refuses, the fake refuses.
func TestTheFakeKeepsAGrantAsItsRunnerWould(t *testing.T) {
	now := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	d := New(t, WithKey("k"), WithRunnerBrokers("llm"), WithBrokers("files"), WithClock(func() time.Time { return now }))
	client := typed(t, d, "k")
	ctx := t.Context()
	create := func(key string) genv1.SandboxID {
		created, err := client.CreateSandboxWithResponse(ctx, &genv1.CreateSandboxParams{IdempotencyKey: key}, genv1.SandboxSpec{Image: "alpine"})
		if err != nil || created.JSON202 == nil {
			t.Fatalf("create = %v", err)
		}
		return created.JSON202.Id
	}
	id, other := create("one"), create("two")
	grant := genv1.GrantWrite{Kind: genv1.GrantSubscription, Credential: "token", Api: genv1.GrantAnthropic, Turn: "t1", ExpiresAt: now.Add(time.Hour)}
	if got := statusOf(client.PutSandboxBrokerGrantWithResponse(ctx, id, "llm", grant)); got != http.StatusNoContent {
		t.Fatalf("put = %d", got)
	}
	if held, ok := d.Grant(id, "llm"); !ok || held != grant {
		t.Errorf("held = %+v, %v", held, ok)
	}
	if _, ok := d.Grant(other, "llm"); ok {
		t.Error("one sandbox's grant is another's")
	}
	if _, ok := d.Grant(genv1.SandboxID{}, "llm"); ok {
		t.Error("nobody's grant")
	}
	now = now.Add(time.Hour)
	if _, ok := d.Grant(id, "llm"); ok {
		t.Error("an expired grant is served")
	}

	bad := func(change func(g *genv1.GrantWrite)) genv1.GrantWrite {
		g := grant
		g.ExpiresAt = now.Add(time.Hour)
		change(&g)
		return g
	}
	for name, c := range map[string]struct {
		id     genv1.SandboxID
		broker string
		grant  genv1.GrantWrite
		want   int
	}{
		"nobody's":              {genv1.SandboxID{}, "llm", bad(func(*genv1.GrantWrite) {}), 404},
		"not url: runner":       {id, "files", bad(func(*genv1.GrantWrite) {}), 409},
		"undeclared":            {id, "github", bad(func(*genv1.GrantWrite) {}), 409},
		"not a broker's name":   {id, "LLM", bad(func(*genv1.GrantWrite) {}), 400},
		"another kind":          {id, "llm", bad(func(g *genv1.GrantWrite) { g.Kind = "password" }), 400},
		"another api":           {id, "llm", bad(func(g *genv1.GrantWrite) { g.Api = "gemini" }), 400},
		"no credential":         {id, "llm", bad(func(g *genv1.GrantWrite) { g.Credential = "" }), 400},
		"too long a credential": {id, "llm", bad(func(g *genv1.GrantWrite) { g.Credential = strings.Repeat("k", 16385) }), 400},
		"no turn":               {id, "llm", bad(func(g *genv1.GrantWrite) { g.Turn = "" }), 400},
		"expired already":       {id, "llm", bad(func(g *genv1.GrantWrite) { g.ExpiresAt = now }), 422},
		"past a day":            {id, "llm", bad(func(g *genv1.GrantWrite) { g.ExpiresAt = now.Add(25 * time.Hour) }), 422},
		"chatgpt, no account":   {id, "llm", bad(func(g *genv1.GrantWrite) { g.Api = genv1.GrantOpenAI }), 422},
		"an account elsewhere":  {id, "llm", bad(func(g *genv1.GrantWrite) { g.Account = new("acct-1") }), 422},
		"an empty account":      {id, "llm", bad(func(g *genv1.GrantWrite) { g.Api, g.Account = genv1.GrantOpenAI, new("") }), 400},
	} {
		if got := statusOf(client.PutSandboxBrokerGrantWithResponse(ctx, c.id, c.broker, c.grant)); got != c.want {
			t.Errorf("%s = %d, want %d", name, got, c.want)
		}
	}
	if got := statusOf(client.PutSandboxBrokerGrantWithBodyWithResponse(ctx, id, "llm", "application/json", strings.NewReader(`{"kind":"apiKey","extra":1}`))); got != 400 {
		t.Errorf("a field a grant does not have = %d", got)
	}

	chatgpt := bad(func(g *genv1.GrantWrite) { g.Api, g.Account = genv1.GrantOpenAI, new("acct-1") })
	if got := statusOf(client.PutSandboxBrokerGrantWithResponse(ctx, id, "llm", chatgpt)); got != 204 {
		t.Fatalf("a chatgpt sign-in = %d", got)
	}
	if held, ok := d.Grant(id, "llm"); !ok || held.Account == nil || *held.Account != "acct-1" {
		t.Errorf("its account = %+v", held)
	}
	fresh := bad(func(*genv1.GrantWrite) {})
	if got := statusOf(client.PutSandboxBrokerGrantWithResponse(ctx, id, "llm", fresh)); got != 204 {
		t.Fatalf("put again = %d", got)
	}
	if got := statusOf(client.DeleteSandboxBrokerGrantWithResponse(ctx, id, "llm")); got != 204 {
		t.Errorf("deleted = %d", got)
	}
	if _, ok := d.Grant(id, "llm"); ok {
		t.Error("a grant taken back is served")
	}
	for name, c := range map[string]struct {
		id     genv1.SandboxID
		broker string
		want   int
	}{"twice": {id, "llm", 204}, "nobody's": {genv1.SandboxID{}, "llm", 404}, "not a name": {id, "LLM", 400}} {
		if got := statusOf(client.DeleteSandboxBrokerGrantWithResponse(ctx, c.id, c.broker)); got != c.want {
			t.Errorf("deleted %s = %d, want %d", name, got, c.want)
		}
	}

	// Stopped, it is forgotten, and a stopped sandbox is lent nothing.
	if got := statusOf(client.PutSandboxBrokerGrantWithResponse(ctx, id, "llm", fresh)); got != 204 {
		t.Fatalf("put = %d", got)
	}
	if got := statusOf(client.StopSandboxWithResponse(ctx, id)); got != http.StatusAccepted {
		t.Fatalf("stop = %d", got)
	}
	if _, ok := d.Grant(id, "llm"); ok {
		t.Error("a stopped sandbox's grant is served")
	}
	if got := statusOf(client.PutSandboxBrokerGrantWithResponse(ctx, id, "llm", fresh)); got != 409 {
		t.Errorf("lent to a stopped sandbox = %d", got)
	}
	// Deleted, it goes with the sandbox.
	if got := statusOf(client.PutSandboxBrokerGrantWithResponse(ctx, other, "llm", fresh)); got != 204 {
		t.Fatalf("put = %d", got)
	}
	if got := statusOf(client.DeleteSandboxWithResponse(ctx, other, nil)); got != http.StatusNoContent {
		t.Fatalf("delete = %d", got)
	}
	if _, ok := d.Grant(other, "llm"); ok {
		t.Error("a deleted sandbox's grant is served")
	}
}
