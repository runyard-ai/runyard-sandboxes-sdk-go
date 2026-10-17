package sdk

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakedaemon"
)

// A principal is made idempotently, read, listed and deleted; its subject
// is replaced by a put with another.
func TestAPrincipalIsMadeReadListedAndDeleted(t *testing.T) {
	d, client := fake(t)
	ctx := t.Context()
	made, err := client.PutPrincipal(ctx, "p1", genv1.PrincipalSpec{Subject: "u-1", Labels: &map[string]string{"agent": "a1"}})
	if err != nil || made.Id != "p1" || made.Subject != "u-1" || made.Labels["agent"] != "a1" || made.CreatedBy == nil {
		t.Fatalf("PutPrincipal = %+v, %v", made, err)
	}
	again, err := client.PutPrincipal(ctx, "p1", genv1.PrincipalSpec{Subject: "u-1"})
	if err != nil || len(again.Labels) != 0 || !again.CreatedAt.Equal(made.CreatedAt) {
		t.Errorf("again = %+v, %v", again, err)
	}
	if changed, err := client.PutPrincipal(ctx, "p1", genv1.PrincipalSpec{Subject: "u-2"}); err != nil || changed.Subject != "u-2" || !changed.CreatedAt.Equal(made.CreatedAt) {
		t.Errorf("another subject = %+v, %v", changed, err)
	}
	if _, err := client.PutPrincipal(ctx, "p1", genv1.PrincipalSpec{Subject: "a/b"}); CodeOf(err) != "bad_request" {
		t.Errorf("a subject that is not one = %v", err)
	}
	if got, err := client.Principal(ctx, "p1"); err != nil || got.Subject != "u-2" {
		t.Errorf("Principal = %+v, %v", got, err)
	}
	if _, err := client.Principal(ctx, "nobody"); CodeOf(err) != "not_found" {
		t.Errorf("one that is not there = %v", err)
	}
	if _, err := client.PutPrincipal(ctx, "p0", genv1.PrincipalSpec{Subject: "u-0"}); err != nil {
		t.Fatal(err)
	}
	all, err := client.Principals(ctx)
	if err != nil || len(all) != 2 || all[0].Id != "p0" || all[1].Id != "p1" {
		t.Errorf("Principals = %+v, %v", all, err)
	}
	if err := client.DeletePrincipal(ctx, "p1"); err != nil {
		t.Fatal(err)
	}
	if err := client.DeletePrincipal(ctx, "p1"); err != nil {
		t.Errorf("deleting one that is not there = %v", err)
	}
	if _, ok := d.Principal("p1"); ok {
		t.Error("still there")
	}
	for _, operation := range []string{"listPrincipals", "getPrincipal", "putPrincipal", "deletePrincipal"} {
		d.Intercept(operation, fakedaemon.Refuse(403, genv1.ErrorCodeForbidden, "no"))
	}
	if _, err := client.Principals(ctx); CodeOf(err) != "forbidden" {
		t.Errorf("a refused Principals = %v", err)
	}
	if _, err := client.Principal(ctx, "p0"); CodeOf(err) != "forbidden" {
		t.Errorf("a refused Principal = %v", err)
	}
	if _, err := client.PutPrincipal(ctx, "p0", genv1.PrincipalSpec{Subject: "u-0"}); CodeOf(err) != "forbidden" {
		t.Errorf("a refused PutPrincipal = %v", err)
	}
	if err := client.DeletePrincipal(ctx, "p0"); CodeOf(err) != "forbidden" {
		t.Errorf("a refused DeletePrincipal = %v", err)
	}
}

// A sandbox acts for a principal that is there, and the volume it first
// attaches follows the principal: another principal is refused it, without
// being told whose it is, until it is moved.
func TestASandboxActsForAPrincipalAndItsVolumeFollows(t *testing.T) {
	d, client := fake(t)
	ctx := t.Context()
	if _, err := client.Create(ctx, "", Spec{Image: "alpine", Principal: new("nobody")}); CodeOf(err) != "not_found" {
		t.Errorf("acting for one that is not there = %v", err)
	}
	for _, id := range []string{"p1", "p2"} {
		if _, err := client.PutPrincipal(ctx, id, genv1.PrincipalSpec{Subject: "u-" + id}); err != nil {
			t.Fatal(err)
		}
	}
	sandbox, err := client.Create(ctx, "", Spec{Image: "alpine", Principal: new("p1"), Disk: &genv1.DiskSpec{Volume: new("chat")}})
	if err != nil {
		t.Fatal(err)
	}
	if bound, _ := d.VolumePrincipal("chat"); bound != "p1" {
		t.Errorf("the volume is bound to %q", bound)
	}
	if err := client.DeletePrincipal(ctx, "p1"); CodeOf(err) != "principal_in_use" {
		t.Errorf("deleting one a sandbox acts for = %v", err)
	}
	if err := client.MoveVolume(ctx, "chat", "p2"); CodeOf(err) != "volume_in_use" {
		t.Errorf("moving a held volume = %v", err)
	}
	if err := sandbox.Release(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = client.Create(ctx, "", Spec{Image: "alpine", Principal: new("p2"), Disk: &genv1.DiskSpec{Volume: new("chat")}})
	if CodeOf(err) != "volume_principal" || strings.Contains(err.Error(), "p1") || strings.Contains(err.Error(), "p2") {
		t.Errorf("another principal = %v", err)
	}
	if err := client.MoveVolume(ctx, "chat", "nobody"); CodeOf(err) != "not_found" {
		t.Errorf("moving to one that is not there = %v", err)
	}
	if err := client.MoveVolume(ctx, "never-was", "p2"); CodeOf(err) != "not_found" {
		t.Errorf("moving one that is not there = %v", err)
	}
	if err := client.MoveVolume(ctx, "chat", "p2"); err != nil {
		t.Fatal(err)
	}
	volumes, err := client.Volumes(ctx)
	if err != nil || len(volumes) != 1 || volumes[0].Principal == nil || *volumes[0].Principal != "p2" {
		t.Errorf("Volumes = %+v, %v", volumes, err)
	}
	moved, err := client.Create(ctx, "", Spec{Image: "alpine", Principal: new("p2"), Disk: &genv1.DiskSpec{Volume: new("chat")}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := client.Open(ctx, moved.ID); err != nil || got.ID != moved.ID {
		t.Fatal(err)
	}
	listed, _ := client.List(ctx)
	if len(listed) != 1 || listed[0].Principal == nil || *listed[0].Principal != "p2" {
		t.Errorf("listed %+v", listed)
	}
	if err := moved.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := client.DeletePrincipal(ctx, "p1"); err != nil {
		t.Errorf("deleting the principal it was moved from = %v", err)
	}
}

// What the broker's trusted plane answers is returned as it was, a 404 among
// them; what the daemon refuses is the error.
func TestABrokersTrustedDocumentIsReadForAPrincipal(t *testing.T) {
	var asked struct {
		principal genv1.Principal
		path      string
	}
	d, client := fake(t, fakedaemon.WithBrokers("plain"), fakedaemon.WithTrustedPlane("github", func(p genv1.Principal, path string) (int, string, []byte) {
		asked.principal, asked.path = p, path
		if path == ".well-known/runyard-contexts" {
			return http.StatusNotFound, "text/plain", []byte("none")
		}
		return http.StatusOK, "application/json", []byte(`{"name":"github"}`)
	}))
	ctx := t.Context()
	if _, err := client.PutPrincipal(ctx, "p1", genv1.PrincipalSpec{Subject: "u-1"}); err != nil {
		t.Fatal(err)
	}
	status, contentType, body, err := client.BrokerTrusted(ctx, "p1", "github", ".well-known/agent-skills/index.json")
	if err != nil || status != 200 || contentType != "application/json" || string(body) != `{"name":"github"}` {
		t.Errorf("= %d %q %s, %v", status, contentType, body, err)
	}
	if asked.principal.Subject != "u-1" || asked.path != ".well-known/agent-skills/index.json" {
		t.Errorf("the plane was asked %+v", asked)
	}
	status, _, body, err = client.BrokerTrusted(ctx, "p1", "github", ".well-known/runyard-contexts")
	if err != nil || status != 404 || string(body) != "none" {
		t.Errorf("the broker's own 404 = %d %s, %v", status, body, err)
	}
	for _, tc := range []struct{ principal, broker, path, code string }{
		{"nobody", "github", ".well-known/x", "not_found"},
		{"p1", "nothing", ".well-known/x", "not_found"},
		{"p1", "plain", ".well-known/x", "no_trusted_plane"},
		{"p1", "github", "v1/self", "bad_request"},
		{"p1", "github", ".well-known/../v1/self", "bad_request"},
	} {
		if _, _, _, err := client.BrokerTrusted(ctx, tc.principal, tc.broker, tc.path); CodeOf(err) != tc.code {
			t.Errorf("%+v = %v, want %s", tc, err, tc.code)
		}
	}
	brokers, _ := client.Brokers(ctx)
	for _, broker := range brokers {
		if (broker.Name == "github") != (broker.TrustedUrl != nil) {
			t.Errorf("broker %s says trustedUrl %v", broker.Name, broker.TrustedUrl)
		}
	}

	// A body that is not the broker's, unmarked, is the daemon's error
	// however it is spelled — a proxy's page, say.
	d.Intercept("getBrokerTrusted", fakedaemon.Answer(502, "text/html", "<h1>bad gateway</h1>"))
	if _, _, _, err := client.BrokerTrusted(ctx, "p1", "github", ".well-known/x"); err == nil || !strings.Contains(err.Error(), "bad gateway") {
		t.Errorf("an unmarked answer = %v", err)
	}
	// More than the daemon ever passes on is not read into memory.
	d.Intercept("getBrokerTrusted", func(w http.ResponseWriter, _ *http.Request, _ http.HandlerFunc) {
		w.Header().Set("Runyard-Broker-Answered", "true")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(make([]byte, 1<<20+1))
	})
	if _, _, _, err := client.BrokerTrusted(ctx, "p1", "github", ".well-known/x"); err == nil {
		t.Error("more than a mebibyte was returned")
	}
	// A body cut short is an error, not a document.
	d.Intercept("getBrokerTrusted", func(w http.ResponseWriter, _ *http.Request, _ http.HandlerFunc) {
		w.Header().Set("Runyard-Broker-Answered", "true")
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "short")
	})
	if _, _, _, err := client.BrokerTrusted(ctx, "p1", "github", ".well-known/x"); err == nil {
		t.Error("a body cut short was returned")
	}
	d.Intercept("getBrokerTrusted", fakedaemon.HangUp())
	if _, _, _, err := client.BrokerTrusted(ctx, "p1", "github", ".well-known/x"); err == nil {
		t.Error("a daemon that hung up answered")
	}
}

func TestACallThatNeverArrivesIsAnError(t *testing.T) {
	d, client := fake(t)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := client.PutPrincipal(ctx, "p1", genv1.PrincipalSpec{Subject: "u"}); err == nil {
		t.Error("PutPrincipal")
	}
	if _, err := client.Principals(ctx); err == nil {
		t.Error("Principals")
	}
	if _, err := client.Principal(ctx, "p1"); err == nil {
		t.Error("Principal")
	}
	if err := client.DeletePrincipal(ctx, "p1"); err == nil {
		t.Error("DeletePrincipal")
	}
	if err := client.MoveVolume(ctx, "chat", "p1"); err == nil {
		t.Error("MoveVolume")
	}
	if _, _, _, err := client.BrokerTrusted(ctx, "p1", "github", ".well-known/x"); err == nil {
		t.Error("BrokerTrusted")
	}
	_ = d
}
