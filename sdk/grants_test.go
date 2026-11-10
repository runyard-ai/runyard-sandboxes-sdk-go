package sdk

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakeserver"
)

// A grant is lent to a running sandbox's runner-provided broker for a turn,
// replaced by the next, and taken back; what the server refuses is the
// caller's error, and an id that is not a sandbox's is refused before
// anything is sent.
func TestAGrantIsLentForATurnAndTakenBack(t *testing.T) {
	d, client := fake(t, fakeserver.WithRunnerBrokers("llm"), fakeserver.WithBrokers("files"))
	ctx := context.Background()
	sandbox, err := client.Create(ctx, "", Spec{Image: "alpine"})
	if err != nil {
		t.Fatal(err)
	}
	id := sandbox.ID.String()
	grant := genv1.GrantWrite{Kind: genv1.GrantAPIKey, Credential: "sk-ant-1", Api: genv1.GrantAnthropic, Turn: "t1", ExpiresAt: time.Now().Add(time.Hour)}
	if err := client.PutGrant(ctx, id, "llm", grant); err != nil {
		t.Fatalf("PutGrant = %v", err)
	}
	grant.Credential, grant.Turn = "sk-ant-2", "t2"
	if err := client.PutGrant(ctx, id, "llm", grant); err != nil {
		t.Fatalf("PutGrant again = %v", err)
	}
	if held, ok := d.Grant(sandbox.ID, "llm"); !ok || held.Credential != "sk-ant-2" || held.Turn != "t2" {
		t.Errorf("held = %+v, %v", held, ok)
	}
	if err := client.DeleteGrant(ctx, id, "llm"); err != nil {
		t.Errorf("DeleteGrant = %v", err)
	}
	if _, ok := d.Grant(sandbox.ID, "llm"); ok {
		t.Error("a grant taken back is held")
	}
	if err := client.DeleteGrant(ctx, id, "llm"); err != nil {
		t.Errorf("DeleteGrant twice = %v", err)
	}

	chatgpt := genv1.GrantWrite{Kind: genv1.GrantSubscription, Credential: "token", Api: genv1.GrantOpenAI, Account: new("acct-1"), Turn: "t3", ExpiresAt: time.Now().Add(time.Hour)}
	if err := client.PutGrant(ctx, id, "llm", chatgpt); err != nil {
		t.Errorf("a chatgpt sign-in = %v", err)
	}
	if held, ok := d.Grant(sandbox.ID, "llm"); !ok || held.Account == nil || *held.Account != "acct-1" {
		t.Errorf("its account = %+v", held)
	}
	chatgpt.Account = nil
	var refused *Error
	if err := client.PutGrant(ctx, id, "llm", chatgpt); !errors.As(err, &refused) || refused.Status != http.StatusUnprocessableEntity {
		t.Errorf("a chatgpt sign-in naming no account = %v", err)
	}
	if err := client.PutGrant(ctx, id, "files", grant); !errors.As(err, &refused) || refused.Status != http.StatusConflict || CodeOf(err) != "not_provided" {
		t.Errorf("a broker no runner provides = %v", err)
	}
	past := grant
	past.ExpiresAt = time.Now().Add(-time.Minute)
	if err := client.PutGrant(ctx, id, "llm", past); !errors.As(err, &refused) || refused.Status != http.StatusUnprocessableEntity {
		t.Errorf("expired already = %v", err)
	}
	nobody := "0192f7a4-5b1e-7c3d-9a2f-4e6b8c1d0a53"
	if err := client.PutGrant(ctx, nobody, "llm", grant); CodeOf(err) != "not_found" {
		t.Errorf("nobody's = %v", err)
	}
	if err := client.DeleteGrant(ctx, nobody, "llm"); CodeOf(err) != "not_found" {
		t.Errorf("nobody's, taken back = %v", err)
	}
	for name, err := range map[string]error{
		"PutGrant":    client.PutGrant(ctx, "not-a-uuid", "llm", grant),
		"DeleteGrant": client.DeleteGrant(ctx, "not-a-uuid", "llm"),
	} {
		if err == nil || !strings.Contains(err.Error(), "not a sandbox's id") {
			t.Errorf("%s of no sandbox's id = %v", name, err)
		}
	}
	gone, cancel := context.WithCancel(ctx)
	cancel()
	if err := client.PutGrant(gone, id, "llm", grant); !errors.Is(err, context.Canceled) {
		t.Errorf("PutGrant, gone = %v", err)
	}
	if err := client.DeleteGrant(gone, id, "llm"); !errors.Is(err, context.Canceled) {
		t.Errorf("DeleteGrant, gone = %v", err)
	}
}
