package fakedaemon

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

func withTunnel(t *testing.T, opts ...Option) (*Daemon, *genv1.ClientWithResponses, genv1.SandboxID) {
	t.Helper()
	d := New(t, append([]Option{WithKey("k")}, opts...)...)
	client := typed(t, d, "k")
	created, err := client.CreateSandboxWithResponse(context.Background(), &genv1.CreateSandboxParams{IdempotencyKey: "t"},
		genv1.SandboxSpec{Image: "alpine:3.20"})
	if err != nil || created.JSON202 == nil {
		t.Fatalf("create = %v %s", err, created.Body)
	}
	return d, client, created.JSON202.Id
}

func TestTheFakeServesATunnelAsTheDaemonReportsOne(t *testing.T) {
	ctx := context.Background()
	var clock atomic.Pointer[time.Time]
	start := time.Now()
	clock.Store(&start)
	d, client, id := withTunnel(t, WithClock(func() time.Time { return *clock.Load() }))
	put, _ := client.PutTunnelWithResponse(ctx, id, "web", genv1.TunnelSpec{Port: 3000})
	if put.JSON201 == nil || !strings.HasPrefix(put.JSON201.Slug, "web-") || put.JSON201.Status.State != genv1.Served {
		t.Fatalf("put = %d %s", put.StatusCode(), put.Body)
	}
	if want := "https://" + put.JSON201.Slug + "--fake.tunnels.invalid/"; *put.JSON201.Url != want {
		t.Errorf("url %s, want %s", *put.JSON201.Url, want)
	}

	d.Settle(id, genv1.SandboxStateStopped, "")
	if got := d.Tunnels(id); got[0].Status.State != genv1.SandboxNotRunning {
		t.Errorf("a stopped sandbox's tunnel is %s", got[0].Status.State)
	}
	d.Settle(id, genv1.SandboxStateReady, "")
	soon := start.Add(time.Minute)
	if again, _ := client.PutTunnelWithResponse(ctx, id, "web", genv1.TunnelSpec{Port: 3000, ExpiresAt: &soon}); again.JSON200 == nil {
		t.Fatalf("replace = %d %s", again.StatusCode(), again.Body)
	}
	if got := d.Tunnels(id)[0].Status.State; got != genv1.Served {
		t.Errorf("a tunnel before its time is %s", got)
	}
	clock.Store(&soon)
	if got := d.Tunnels(id)[0].Status.State; got != genv1.Expired {
		t.Errorf("a tunnel at its time is %s, not expired", got)
	}
	if d.Tunnels(genv1.SandboxID{}) != nil {
		t.Error("a sandbox that is not here has tunnels")
	}
}

func TestTheFakeRefusesATunnelAsTheDaemonDoes(t *testing.T) {
	ctx := context.Background()
	d, client, id := withTunnel(t)
	past := time.Now().Add(-time.Minute)
	for name, c := range map[string]struct {
		tunnel string
		body   string
		status int
	}{
		"a name that is not one":   {"Web", `{"port":1}`, 400},
		"a double hyphen":          {"a--b", `{"port":1}`, 400},
		"a field it does not have": {"web", `{"port":1,"secret":1}`, 400},
		"port zero":                {"web", `{"port":0}`, 400},
		"a time that has been":     {"web", `{"port":1,"expiresAt":"` + past.Format(time.RFC3339) + `"}`, 400},
	} {
		t.Run(name, func(t *testing.T) {
			res, err := client.PutTunnelWithBodyWithResponse(ctx, id, c.tunnel, "application/json", strings.NewReader(c.body))
			if err != nil || res.StatusCode() != c.status {
				t.Errorf("= %d %s, %v", res.StatusCode(), res.Body, err)
			}
		})
	}
	for i := range 16 {
		if put, _ := client.PutTunnelWithResponse(ctx, id, fmt.Sprintf("t%d", i), genv1.TunnelSpec{Port: 1}); put.JSON201 == nil {
			t.Fatalf("tunnel %d = %d", i, put.StatusCode())
		}
	}
	if put, _ := client.PutTunnelWithResponse(ctx, id, "one-more", genv1.TunnelSpec{Port: 1}); put.StatusCode() != http.StatusConflict {
		t.Errorf("a seventeenth = %d", put.StatusCode())
	}
	for i := range 16 {
		if made, _ := client.CreateTunnelTokenWithResponse(ctx, id, "t0", genv1.TunnelTokenRequest{Comment: "x"}); made.JSON201 == nil {
			t.Fatalf("token %d = %d", i, made.StatusCode())
		}
	}
	if made, _ := client.CreateTunnelTokenWithResponse(ctx, id, "t0", genv1.TunnelTokenRequest{Comment: "x"}); made.StatusCode() != http.StatusConflict {
		t.Errorf("a seventeenth token = %d", made.StatusCode())
	}
	if made, _ := client.CreateTunnelTokenWithResponse(ctx, id, "t0", genv1.TunnelTokenRequest{}); made.StatusCode() != http.StatusBadRequest {
		t.Errorf("a token with no comment = %d", made.StatusCode())
	}
	if revoked, _ := client.RevokeTunnelTokenWithResponse(ctx, id, "t0", "BAD", nil); revoked.StatusCode() != http.StatusBadRequest {
		t.Errorf("a token id that is not one = %d", revoked.StatusCode())
	}
	if revoked, _ := client.RevokeTunnelTokenWithResponse(ctx, id, "t0", "zzzzzzzz", nil); revoked.StatusCode() != http.StatusNotFound {
		t.Errorf("a token that is not there = %d", revoked.StatusCode())
	}
	token := d.Tunnels(id)[0].Tokens[0].Id
	first, _ := client.RevokeTunnelTokenWithResponse(ctx, id, "t0", token, nil)
	second, _ := client.RevokeTunnelTokenWithResponse(ctx, id, "t0", token, &genv1.RevokeTunnelTokenParams{Reason: new("late")})
	if first.JSON200 == nil || second.JSON200 == nil || !second.JSON200.RevokedAt.Equal(*first.JSON200.RevokedAt) || second.JSON200.RevokedReason != nil {
		t.Errorf("revoking twice changed the record: %s then %s", first.Body, second.Body)
	}
	if missing, _ := client.GetTunnelWithResponse(ctx, id, "nope"); missing.StatusCode() != http.StatusNotFound {
		t.Errorf("a tunnel that is not there = %d", missing.StatusCode())
	}
	other := genv1.SandboxID{}
	if missing, _ := client.ListTunnelsWithResponse(ctx, other); missing.StatusCode() != http.StatusNotFound {
		t.Errorf("a sandbox that is not there = %d", missing.StatusCode())
	}
	d.Settle(id, genv1.SandboxStateGone, "")
	if put, _ := client.PutTunnelWithResponse(ctx, id, "late", genv1.TunnelSpec{Port: 1}); put.StatusCode() != http.StatusConflict {
		t.Errorf("a tunnel on a gone sandbox = %d", put.StatusCode())
	}
}

func TestTheFakeListsEveryTunnelAndWhatVisitorsDidNewestFirst(t *testing.T) {
	ctx := context.Background()
	d, client, id := withTunnel(t)
	for _, name := range []string{"web", "api"} {
		if put, _ := client.PutTunnelWithResponse(ctx, id, name, genv1.TunnelSpec{Port: 1}); put.JSON201 == nil {
			t.Fatal(put.StatusCode())
		}
	}
	d.RecordVisit(id, "web", genv1.TunnelRequest{Path: "/first"})
	d.RecordVisit(id, "web", genv1.TunnelRequest{Path: "/second"})
	d.RecordVisit(id, "nope", genv1.TunnelRequest{Path: "/nowhere"})
	d.RecordVisit(genv1.SandboxID{}, "web", genv1.TunnelRequest{Path: "/nobody"})
	page, _ := client.GetTunnelActivityWithResponse(ctx, id, "web", nil)
	if page.JSON200 == nil || len(page.JSON200.Items) != 2 || page.JSON200.Items[0].Path != "/second" {
		t.Fatalf("activity = %s", page.Body)
	}
	host, _ := client.ListHostTunnelsWithResponse(ctx, nil)
	if host.JSON200 == nil || len(host.JSON200.Items) != 2 || *host.JSON200.Items[0].SandboxId != id || !host.JSON200.Relay.Docked ||
		host.JSON200.NextCursor != nil {
		t.Fatalf("host = %s", host.Body)
	}
	one := 1
	first, _ := client.ListHostTunnelsWithResponse(ctx, &genv1.ListHostTunnelsParams{Limit: &one})
	if first.JSON200 == nil || len(first.JSON200.Items) != 1 || first.JSON200.NextCursor == nil {
		t.Fatalf("first page = %s", first.Body)
	}
	rest, _ := client.ListHostTunnelsWithResponse(ctx, &genv1.ListHostTunnelsParams{Limit: &one, Cursor: first.JSON200.NextCursor})
	// The last page reaches the end, and says so by carrying no cursor.
	if rest.JSON200 == nil || len(rest.JSON200.Items) != 1 || rest.JSON200.Items[0].Slug == first.JSON200.Items[0].Slug ||
		rest.JSON200.NextCursor != nil {
		t.Fatalf("second page = %s", rest.Body)
	}
	zero, forged := 0, "forged"
	for _, params := range []*genv1.ListHostTunnelsParams{{Limit: &zero}, {Cursor: &forged}} {
		if refused, _ := client.ListHostTunnelsWithResponse(ctx, params); refused.StatusCode() != http.StatusBadRequest {
			t.Errorf("%+v = %d", params, refused.StatusCode())
		}
	}
	if paused, _ := client.PauseTunnelWithResponse(ctx, id, "api"); paused.JSON200 == nil || paused.JSON200.Status.State != genv1.Paused {
		t.Errorf("paused = %s", paused.Body)
	}
	if deleted, _ := client.DeleteTunnelWithResponse(ctx, id, "web"); deleted.StatusCode() != http.StatusNoContent {
		t.Errorf("delete = %d", deleted.StatusCode())
	}
	if list, _ := client.ListTunnelsWithResponse(ctx, id); list.JSON200 == nil || len(list.JSON200.Items) != 1 || list.JSON200.Items[0].Name != "api" {
		t.Errorf("list = %s", list.Body)
	}
}

func TestTheFakeSpellsATunnelsShareButtonOutAndKeepsWhatItWasGiven(t *testing.T) {
	ctx := context.Background()
	_, client, id := withTunnel(t)
	put, _ := client.PutTunnelWithResponse(ctx, id, "web", genv1.TunnelSpec{Port: 1})
	if put.JSON201 == nil || put.JSON201.Overlay.Hidden == nil || *put.JSON201.Overlay.Hidden ||
		put.JSON201.Overlay.Corner == nil || *put.JSON201.Overlay.Corner != genv1.BottomRight {
		t.Fatalf("a tunnel put without one = %s", put.Body)
	}
	corner := genv1.TopLeft
	hidden := true
	again, _ := client.PutTunnelWithResponse(ctx, id, "web", genv1.TunnelSpec{Port: 1, Overlay: &genv1.TunnelOverlay{Hidden: &hidden, Corner: &corner}})
	if again.JSON200 == nil || !*again.JSON200.Overlay.Hidden || *again.JSON200.Overlay.Corner != genv1.TopLeft {
		t.Fatalf("a tunnel put with one = %s", again.Body)
	}
	if got, _ := client.GetTunnelWithResponse(ctx, id, "web"); got.JSON200 == nil || *got.JSON200.Overlay.Corner != genv1.TopLeft {
		t.Fatalf("read back = %s", got.Body)
	}
	// A tunnel is put whole: one put without it goes back to the default.
	reset, _ := client.PutTunnelWithResponse(ctx, id, "web", genv1.TunnelSpec{Port: 1})
	if reset.JSON200 == nil || *reset.JSON200.Overlay.Hidden || *reset.JSON200.Overlay.Corner != genv1.BottomRight {
		t.Fatalf("put again without one = %s", reset.Body)
	}
	for name, body := range map[string]string{
		"a corner that is not one":     `{"port":1,"overlay":{"corner":"middle"}}`,
		"a field the button lacks":     `{"port":1,"overlay":{"colour":"red"}}`,
		"hidden that is not a boolean": `{"port":1,"overlay":{"hidden":"yes"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			res, err := client.PutTunnelWithBodyWithResponse(ctx, id, "web", "application/json", strings.NewReader(body))
			if err != nil || res.StatusCode() != http.StatusBadRequest {
				t.Errorf("= %d %s, %v", res.StatusCode(), res.Body, err)
			}
		})
	}
}

func TestTheFakeListsATunnelsGuestsOldestFirstAndTakesThemBackOut(t *testing.T) {
	ctx := context.Background()
	d, client, id := withTunnel(t)
	if put, _ := client.PutTunnelWithResponse(ctx, id, "web", genv1.TunnelSpec{Port: 1}); put.JSON201 == nil {
		t.Fatal(put.StatusCode())
	}
	empty, _ := client.ListTunnelGuestsWithResponse(ctx, id, "web")
	if empty.JSON200 == nil || empty.JSON200.Items == nil || len(empty.JSON200.Items) != 0 {
		t.Fatalf("no guests = %s", empty.Body)
	}
	d.AddTunnelGuest(id, "web", "grace@example.com", "ada@example.com")
	d.AddTunnelGuest(id, "web", "linus@example.com", "ada@example.com")
	d.AddTunnelGuest(id, "nope", "nobody@example.com", "ada@example.com")
	d.AddTunnelGuest(genv1.SandboxID{}, "web", "nobody@example.com", "ada@example.com")
	list, _ := client.ListTunnelGuestsWithResponse(ctx, id, "web")
	if list.JSON200 == nil || len(list.JSON200.Items) != 2 || list.JSON200.Items[0].Email != "grace@example.com" ||
		list.JSON200.Items[0].By != "ada@example.com" || list.JSON200.Items[0].At.IsZero() {
		t.Fatalf("guests = %s", list.Body)
	}
	if gone, _ := client.RevokeTunnelGuestWithResponse(ctx, id, "web", "GRACE@example.com"); gone.StatusCode() != http.StatusNoContent {
		t.Fatalf("revoke = %d %s", gone.StatusCode(), gone.Body)
	}
	if again, _ := client.RevokeTunnelGuestWithResponse(ctx, id, "web", "grace@example.com"); again.StatusCode() != http.StatusNotFound {
		t.Errorf("revoked twice = %d", again.StatusCode())
	}
	if left, _ := client.ListTunnelGuestsWithResponse(ctx, id, "web"); left.JSON200 == nil || len(left.JSON200.Items) != 1 ||
		left.JSON200.Items[0].Email != "linus@example.com" {
		t.Fatalf("left = %s", left.Body)
	}
	for name, c := range map[string]struct {
		tunnel, email string
		status        int
	}{
		"no tunnel by that name":  {"nope", "linus@example.com", http.StatusNotFound},
		"a name that is not one":  {"Web", "linus@example.com", http.StatusBadRequest},
		"not an address":          {"web", "linus", http.StatusBadRequest},
		"an address past its max": {"web", strings.Repeat("a", 250) + "@b.cd", http.StatusBadRequest},
	} {
		t.Run(name, func(t *testing.T) {
			if res, _ := client.RevokeTunnelGuestWithResponse(ctx, id, c.tunnel, c.email); res.StatusCode() != c.status {
				t.Errorf("= %d %s", res.StatusCode(), res.Body)
			}
		})
	}
	if missing, _ := client.ListTunnelGuestsWithResponse(ctx, id, "nope"); missing.StatusCode() != http.StatusNotFound {
		t.Errorf("guests of a tunnel that is not there = %d", missing.StatusCode())
	}
	if missing, _ := client.ListTunnelGuestsWithResponse(ctx, genv1.SandboxID{}, "web"); missing.StatusCode() != http.StatusNotFound {
		t.Errorf("guests on a sandbox that is not there = %d", missing.StatusCode())
	}
}
