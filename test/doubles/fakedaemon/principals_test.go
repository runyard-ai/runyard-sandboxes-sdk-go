package fakedaemon

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

func TestPrincipalsAreMadeReadAndDeletedAsTheDaemonDoes(t *testing.T) {
	d := New(t, WithKey("k"))
	client := typed(t, d, "k")
	ctx := t.Context()
	labels := map[string]string{"agent": "a1"}
	made, err := client.PutPrincipalWithResponse(ctx, DefaultNamespace, "p1", genv1.PrincipalSpec{Subject: "u-1", Labels: &labels})
	if err != nil || made.JSON201 == nil || made.JSON201.CreatedBy == nil || made.JSON201.CreatedBy.User != operator {
		t.Fatalf("made = %v %s", err, made.Body)
	}
	labels["agent"] = "changed"
	if p, ok := d.Principal("p1"); !ok || p.Labels["agent"] != "a1" || p.Subject != "u-1" {
		t.Errorf("Principal = %+v, %v", p, ok)
	}
	if _, ok := d.Principal("nobody"); ok {
		t.Error("one that is not there is")
	}
	again, _ := client.PutPrincipalWithResponse(ctx, DefaultNamespace, "p1", genv1.PrincipalSpec{Subject: "u-1"})
	if again.JSON200 == nil || len(again.JSON200.Labels) != 0 {
		t.Errorf("again = %s", again.Body)
	}
	if other, _ := client.PutPrincipalWithResponse(ctx, DefaultNamespace, "p1", genv1.PrincipalSpec{Subject: "u-2"}); other.StatusCode() != 409 {
		t.Errorf("another subject = %d", other.StatusCode())
	}
	for _, tc := range []struct{ id, subject string }{{"a b", "u"}, {"p", "a/b"}, {"p", ""}} {
		if got, _ := client.PutPrincipalWithResponse(ctx, DefaultNamespace, tc.id, genv1.PrincipalSpec{Subject: tc.subject}); got.StatusCode() != 400 {
			t.Errorf("%+v = %d", tc, got.StatusCode())
		}
	}
	if got, _ := client.PutPrincipalWithBodyWithResponse(ctx, DefaultNamespace, "p1", "application/json", strings.NewReader(`{"subject":"u-1","x":1}`)); got.StatusCode() != 400 {
		t.Errorf("a field it does not take = %d", got.StatusCode())
	}
	if got, _ := client.GetPrincipalWithResponse(ctx, DefaultNamespace, "p1"); got.JSON200 == nil {
		t.Errorf("read = %s", got.Body)
	}
	if got, _ := client.GetPrincipalWithResponse(ctx, DefaultNamespace, "a b"); got.StatusCode() != 400 {
		t.Errorf("a malformed id = %d", got.StatusCode())
	}
	listed, _ := client.ListPrincipalsWithResponse(ctx, DefaultNamespace)
	if listed.JSON200 == nil || len(listed.JSON200.Items) != 1 {
		t.Errorf("listed %s", listed.Body)
	}
	if got, _ := client.DeletePrincipalWithResponse(ctx, DefaultNamespace, "p1"); got.StatusCode() != 204 {
		t.Errorf("delete = %d", got.StatusCode())
	}
	if got, _ := client.DeletePrincipalWithResponse(ctx, DefaultNamespace, "a b"); got.StatusCode() != 400 {
		t.Errorf("delete a malformed id = %d", got.StatusCode())
	}
	if got, _ := client.GetPrincipalWithResponse(ctx, DefaultNamespace, "p1"); got.StatusCode() != 404 {
		t.Errorf("after the delete = %d", got.StatusCode())
	}
}

func TestAVolumeFollowsItsPrincipalAsTheDaemonsDoes(t *testing.T) {
	d := New(t, WithKey("k"))
	client := typed(t, d, "k")
	ctx := t.Context()
	for _, id := range []string{"p1", "p2"} {
		if _, err := client.PutPrincipalWithResponse(ctx, DefaultNamespace, id, genv1.PrincipalSpec{Subject: "u"}); err != nil {
			t.Fatal(err)
		}
	}
	create := func(key string, principal *string, volume string) *genv1.CreateSandboxResponse {
		t.Helper()
		res, err := client.CreateSandboxWithResponse(ctx, DefaultNamespace, &genv1.CreateSandboxParams{IdempotencyKey: key}, genv1.SandboxSpec{Image: "alpine", Principal: principal, Disk: &genv1.DiskSpec{Volume: &volume}})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if _, ok := d.VolumePrincipal("chat"); ok {
		t.Error("a volume nothing made is there")
	}
	// Made by a sandbox acting for nobody, then bound by the first that acts
	// for somebody.
	first := create("k1", nil, "chat")
	if bound, ok := d.VolumePrincipal("chat"); !ok || bound != "" {
		t.Errorf("bound to %q", bound)
	}
	if got, _ := client.DeleteSandboxWithResponse(ctx, DefaultNamespace, first.JSON202.Id, &genv1.DeleteSandboxParams{}); got.StatusCode() != 204 {
		t.Fatal(got.StatusCode())
	}
	second := create("k2", new("p1"), "chat")
	if bound, _ := d.VolumePrincipal("chat"); bound != "p1" || second.JSON202 == nil || *second.JSON202.Principal != "p1" {
		t.Errorf("bound to %q: %s", bound, second.Body)
	}
	if got, _ := client.DeleteSandboxWithResponse(ctx, DefaultNamespace, second.JSON202.Id, &genv1.DeleteSandboxParams{}); got.StatusCode() != 204 {
		t.Fatal(got.StatusCode())
	}
	for name, principal := range map[string]*string{"another": new("p2"), "none": nil} {
		if got := create("k3-"+name, principal, "chat"); got.StatusCode() != 409 || !strings.Contains(string(got.Body), "volume_principal") {
			t.Errorf("%s = %d %s", name, got.StatusCode(), got.Body)
		}
	}
	if got := create("k4", new("nobody"), "fresh"); got.StatusCode() != 404 {
		t.Errorf("acting for one that is not there = %d", got.StatusCode())
	}
	if got, _ := client.MoveVolumeWithBodyWithResponse(ctx, DefaultNamespace, "chat", "application/json", strings.NewReader(`{}`)); got.StatusCode() != 400 {
		t.Errorf("a move with no principal = %d", got.StatusCode())
	}
	moved, _ := client.MoveVolumeWithResponse(ctx, DefaultNamespace, "chat", genv1.VolumePrincipal{Principal: "p2"})
	if moved.JSON200 == nil || *moved.JSON200.Principal != "p2" {
		t.Errorf("moved = %s", moved.Body)
	}
}

func TestATrustedPlaneIsReadAsTheDaemonReadsOne(t *testing.T) {
	d := New(t, WithKey("k"), WithBrokers("github", "plain"), WithTrustedPlane("github", func(p genv1.Principal, path string) (int, string, []byte) {
		if path == ".well-known/none" {
			return 404, "", nil
		}
		return 200, "text/markdown", []byte("# " + p.Subject + " " + path)
	}), WithTrustedPlane("other", func(genv1.Principal, string) (int, string, []byte) { return 204, "", nil }))
	client := typed(t, d, "k")
	ctx := t.Context()
	if _, err := client.PutPrincipalWithResponse(ctx, DefaultNamespace, "p1", genv1.PrincipalSpec{Subject: "u-1"}); err != nil {
		t.Fatal(err)
	}
	res, err := client.GetBrokerTrusted(ctx, DefaultNamespace, "p1", "github", ".well-known/agent-skills/a.md")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(res.Body)
	_ = res.Body.Close()
	if res.StatusCode != http.StatusOK || res.Header.Get("Content-Type") != "text/markdown" || res.Header.Get("Runyard-Broker-Answered") != "true" || string(body) != "# u-1 .well-known/agent-skills/a.md" {
		t.Errorf("= %d %v %s", res.StatusCode, res.Header, body)
	}
	for path, want := range map[string]int{".well-known/none": 404, ".well-known//x": 400, ".well-known/./x": 400, ".well-known/x?y": 400} {
		res, err := client.GetBrokerTrusted(ctx, DefaultNamespace, "p1", "github", path)
		if err != nil {
			t.Fatal(err)
		}
		_ = res.Body.Close()
		if res.StatusCode != want {
			t.Errorf("%q = %d, want %d", path, res.StatusCode, want)
		}
	}
	// A broker WithTrustedPlane declared itself, which says so.
	brokers, _ := client.ListBrokersWithResponse(ctx, DefaultNamespace)
	trusted := map[string]bool{}
	for _, broker := range brokers.JSON200.Items {
		trusted[broker.Name] = broker.TrustedUrl != nil
	}
	if !trusted["github"] || trusted["plain"] || !trusted["other"] {
		t.Errorf("brokers = %v", trusted)
	}
	other, err := client.GetBrokerTrusted(ctx, DefaultNamespace, "p1", "other", ".well-known/x")
	if err != nil {
		t.Fatal(err)
	}
	_ = other.Body.Close()
	if other.StatusCode != http.StatusNoContent {
		t.Errorf("other = %d", other.StatusCode)
	}
}
