package fakeserver

import (
	"context"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// A host joins with a token as the runner it names, numbered for a fleet
// past every name taken, and is drained, undrained, revoked and rejoined as
// the server does it.
func TestTheFakeAddsListsAndRevokesRunners(t *testing.T) {
	d := New(t, WithKey("k"))
	client := typed(t, d, "k")
	ctx := context.Background()
	mint := func(ask genv1.JoinTokenRequest) genv1.JoinTokenCreated {
		t.Helper()
		minted, err := client.CreateJoinTokenWithResponse(ctx, ask)
		if err != nil || minted.JSON201 == nil {
			t.Fatalf("mint %+v = %v %s", ask, err, minted.Body)
		}
		return *minted.JSON201
	}
	one := mint(genv1.JoinTokenRequest{RunnerName: new("factory-03"), Labels: &map[string]string{"site": "factory"},
		AllowedSources: &[]string{"203.0.113.24/32"}, ExpiresIn: new("30m")})
	if !strings.HasPrefix(one.Secret, "rjt_"+one.Id+"_") || one.MaxUses != 1 || !one.ExpiresAt.Equal(one.CreatedAt.Add(30*time.Minute)) {
		t.Errorf("minted %+v", one)
	}
	held := mint(genv1.JoinTokenRequest{RunnerName: new("edge-02")})
	fleet := mint(genv1.JoinTokenRequest{RunnerName: new("edge"), MaxUses: new(2)})
	plain := mint(genv1.JoinTokenRequest{})

	factory := d.Join(t, one.Secret)
	if factory.Id != "r-01" || factory.Name != "factory-03" || factory.State != genv1.RunnerStateRunnerPending || factory.Labels["site"] != "factory" ||
		factory.AllowedSources[0] != "203.0.113.24/32" {
		t.Errorf("joined %+v", factory)
	}
	if first, second := d.Join(t, fleet.Secret), d.Join(t, fleet.Secret); first.Name != "edge-01" || second.Name != "edge-03" {
		t.Errorf("the fleet is %s and %s, past edge-02, which a token holds", first.Name, second.Name)
	}
	if unnamed := d.Join(t, plain.Secret); unnamed.Name != unnamed.Id {
		t.Errorf("a runner given no name is called %q", unnamed.Name)
	}
	if named := d.Join(t, held.Secret); named.Name != "edge-02" {
		t.Errorf("the token holding edge-02 enrolled %q", named.Name)
	}
	listed, err := client.ListRunnersWithResponse(ctx, nil)
	if err != nil || listed.JSON200 == nil || len(listed.JSON200.Items) != 5 || listed.JSON200.Items[0].Name != "factory-03" {
		t.Fatalf("listed = %v %s", err, listed.Body)
	}
	tokens, err := client.ListJoinTokensWithResponse(ctx)
	if err != nil || tokens.JSON200 == nil || tokens.JSON200.Items[0].Id != plain.Id || tokens.JSON200.Items[3].Uses != 1 {
		t.Fatalf("tokens = %v %s", err, tokens.Body)
	}

	got, err := client.GetRunnerWithResponse(ctx, "r-01")
	if err != nil || got.JSON200 == nil || got.JSON200.Name != "factory-03" {
		t.Fatalf("r-01 = %v %s", err, got.Body)
	}
	moved, err := client.UpdateRunnerWithResponse(ctx, "r-01", genv1.RunnerUpdate{AllowedSources: &[]string{}})
	if err != nil || moved.JSON200 == nil || len(moved.JSON200.AllowedSources) != 0 {
		t.Fatalf("anywhere = %v %s", err, moved.Body)
	}
	if taken, _ := client.UpdateRunnerWithResponse(ctx, "r-01", genv1.RunnerUpdate{Name: new("edge-02")}); taken.StatusCode() != http.StatusConflict {
		t.Errorf("renamed to another runner's name = %d %s", taken.StatusCode(), taken.Body)
	}
	renamed, err := client.UpdateRunnerWithResponse(ctx, "r-01", genv1.RunnerUpdate{Name: new("factory-04")})
	if err != nil || renamed.JSON200 == nil || renamed.JSON200.Name != "factory-04" || len(renamed.JSON200.AllowedSources) != 0 {
		t.Fatalf("renamed = %v %s", err, renamed.Body)
	}
	if same, _ := client.UpdateRunnerWithResponse(ctx, "r-01", genv1.RunnerUpdate{Name: new("factory-04")}); same.JSON200 == nil {
		t.Errorf("renamed to its own name = %d %s", same.StatusCode(), same.Body)
	}
	if nothing, _ := client.UpdateRunnerWithResponse(ctx, "r-01", genv1.RunnerUpdate{}); nothing.StatusCode() != http.StatusBadRequest {
		t.Errorf("a change of nothing = %d %s", nothing.StatusCode(), nothing.Body)
	}
	drained, err := client.DrainRunnerWithResponse(ctx, "r-01", genv1.RunnerDrainRequest{RevokeWhenEmpty: new(true)})
	if err != nil || drained.JSON202 == nil || drained.JSON202.Kind != genv1.OperationDrain || !drained.JSON202.RevokeWhenEmpty {
		t.Fatalf("drain = %v %s", err, drained.Body)
	}
	if again, _ := client.DrainRunnerWithResponse(ctx, "r-01", genv1.RunnerDrainRequest{}); again.JSON202 == nil || again.JSON202.Id != drained.JSON202.Id {
		t.Errorf("drained again = %s", again.Body)
	}
	undrained, err := client.UndrainRunnerWithResponse(ctx, "r-01")
	if err != nil || undrained.JSON202 == nil || undrained.JSON202.Step != genv1.OperationCancelled {
		t.Fatalf("undrain = %v %s", err, undrained.Body)
	}
	if state := runnerState(t, client, "r-01"); state != genv1.RunnerStateRunnerPending {
		t.Errorf("undrained, it is %s", state)
	}
	if again, _ := client.UndrainRunnerWithResponse(ctx, "r-01"); again.StatusCode() != http.StatusConflict {
		t.Errorf("undrained again = %d", again.StatusCode())
	}
	if wrong, _ := client.DrainRunnerWithBodyWithResponse(ctx, "r-01", "application/json", strings.NewReader(`{"nope":1}`)); wrong.StatusCode() != http.StatusBadRequest {
		t.Errorf("a drain asked wrong = %d", wrong.StatusCode())
	}
	if held, _ := client.AllowRunnerAddressWithResponse(ctx, "r-01", genv1.AllowAddress{Address: "198.51.100.7"}); held.StatusCode() != http.StatusConflict {
		t.Errorf("allowing a runner not held = %d", held.StatusCode())
	}

	// Rejoined, it is itself, pending again.
	rejoin := mint(genv1.JoinTokenRequest{HostID: new("r-01")})
	if back := d.Join(t, rejoin.Secret); back.Id != "r-01" || back.Name != "factory-04" || back.State != genv1.RunnerStateRunnerPending {
		t.Errorf("rejoined as %+v", back)
	}
	revoked, err := client.RevokeRunnerWithResponse(ctx, "r-01")
	if err != nil || revoked.JSON202 == nil || revoked.JSON202.Kind != genv1.OperationRevoke || revoked.JSON202.Step != genv1.OperationDone {
		t.Fatalf("revoke = %v %s", err, revoked.Body)
	}
	if again, _ := client.RevokeRunnerWithResponse(ctx, "r-01"); again.JSON202 == nil || again.JSON202.Id != revoked.JSON202.Id {
		t.Errorf("revoked again = %s", again.Body)
	}
	if state := runnerState(t, client, "r-01"); state != genv1.RunnerStateRunnerRevoked {
		t.Errorf("revoked, it is %s", state)
	}
	for name, status := range map[string]int{
		"drain":  statusOf(client.DrainRunnerWithResponse(ctx, "r-01", genv1.RunnerDrainRequest{})),
		"update": statusOf(client.UpdateRunnerWithResponse(ctx, "r-01", genv1.RunnerUpdate{AllowedSources: &[]string{}})),
		"rejoin": statusOf(client.CreateJoinTokenWithResponse(ctx, genv1.JoinTokenRequest{HostID: new("r-01")})),
		"rotate": statusOf(client.RotateRunnerCAWithResponse(ctx, "r-01")),
	} {
		if status != http.StatusConflict {
			t.Errorf("%s a revoked runner = %d", name, status)
		}
	}
	// Its name is free again.
	mint(genv1.JoinTokenRequest{RunnerName: new("factory-03")})

	// A rotation is asked of a runner not revoked, once.
	asked, err := client.RotateRunnerCAWithResponse(ctx, "r-02")
	if err != nil || asked.JSON202 == nil || asked.JSON202.CaRotationAskedAt == nil {
		t.Fatalf("rotate = %v %s", err, asked.Body)
	}
	if again, _ := client.RotateRunnerCAWithResponse(ctx, "r-02"); again.JSON202 == nil || !again.JSON202.CaRotationAskedAt.Equal(*asked.JSON202.CaRotationAskedAt) {
		t.Errorf("rotated again = %s", again.Body)
	}
	if got := statusOf(client.RotateRunnerCAWithResponse(ctx, "r-99")); got != http.StatusNotFound {
		t.Errorf("rotating nobody = %d", got)
	}

	// Removed from the list, once, it is listed only when asked for.
	if got := statusOf(client.UnlistRunnerWithResponse(ctx, "r-02")); got != http.StatusConflict {
		t.Errorf("unlisting a runner not revoked = %d", got)
	}
	unlisted, err := client.UnlistRunnerWithResponse(ctx, "r-01")
	if err != nil || unlisted.JSON200 == nil || unlisted.JSON200.UnlistedAt == nil || *unlisted.JSON200.UnlistedBy != "admin@example.com" {
		t.Fatalf("unlist = %v %s", err, unlisted.Body)
	}
	if again, _ := client.UnlistRunnerWithResponse(ctx, "r-01"); again.JSON200 == nil || !again.JSON200.UnlistedAt.Equal(*unlisted.JSON200.UnlistedAt) {
		t.Errorf("unlisted again = %s", again.Body)
	}
	if listed, _ := client.ListRunnersWithResponse(ctx, nil); listed.JSON200 == nil || len(listed.JSON200.Items) != 4 || listed.JSON200.Items[0].Id == "r-01" {
		t.Errorf("listed = %s, without r-01", listed.Body)
	}
	if all, _ := client.ListRunnersWithResponse(ctx, &genv1.ListRunnersParams{Unlisted: new(true)}); all.JSON200 == nil || len(all.JSON200.Items) != 5 || all.JSON200.Items[0].UnlistedAt == nil {
		t.Errorf("listed with the unlisted = %s", all.Body)
	}
	if got := statusOf(client.GetRunnerWithResponse(ctx, "r-01")); got != http.StatusOK {
		t.Errorf("getting a runner unlisted = %d", got)
	}
	if got := statusOf(client.ListRunnersWithResponse(ctx, nil, func(_ context.Context, r *http.Request) error {
		r.URL.RawQuery = "unlisted=sometimes"
		return nil
	})); got != http.StatusBadRequest {
		t.Errorf("unlisted=sometimes = %d", got)
	}
}

// A fleet installed as code mints the token its code declared, which never
// expires and approves its runners: a host that joins with it is approved
// at once, by the token, and the token keeps where it may be used from.
func TestTheFakeMintsAFleetsTokenItsCodeDeclared(t *testing.T) {
	d := New(t)
	client := typed(t, d, "")
	ctx := context.Background()
	given := "rjt_f1ee70000001_" + strings.Repeat("ab", 24)
	minted, err := client.CreateJoinTokenWithResponse(ctx, genv1.JoinTokenRequest{
		Token: &given, ExpiresIn: new("never"), MaxUses: new(1_000_000), Approval: new(genv1.JoinTokenApprovalAuto), AllowedSources: &[]string{"203.0.113.0/24"},
	})
	if err != nil || minted.JSON201 == nil {
		t.Fatalf("= %v %s", err, minted.Body)
	}
	token := minted.JSON201
	if token.Id != "f1ee70000001" || token.Secret != given || token.ExpiresAt != nil || token.Approval != genv1.JoinTokenApprovalAuto || token.HardwareKeys != genv1.HardwareKeysPreferred {
		t.Errorf("minted = %+v", token)
	}
	runner := d.Join(t, given)
	if runner.State != genv1.RunnerStateRunnerOnline || runner.ApprovedAt == nil || runner.ApprovedBy == nil || *runner.ApprovedBy != "join token f1ee70000001" {
		t.Errorf("joined = %+v", runner)
	}
	if plain := d.Join(t, mintedSecret(t, client, genv1.JoinTokenRequest{})); plain.State != genv1.RunnerStateRunnerPending || plain.ApprovedAt != nil {
		t.Errorf("joined with a token that approves nothing = %+v", plain)
	}
	if got := statusOf(client.UpdateJoinTokenWithResponse(ctx, token.Id, genv1.JoinTokenUpdate{AllowedSources: []string{}})); got != http.StatusBadRequest {
		t.Errorf("its sources emptied = %d", got)
	}
	if got := statusOf(client.CreateJoinTokenWithResponse(ctx, genv1.JoinTokenRequest{Token: &given, MaxUses: new(2)})); got != http.StatusConflict {
		t.Errorf("minted again = %d", got)
	}
	required, err := client.CreateJoinTokenWithResponse(ctx, genv1.JoinTokenRequest{ExpiresIn: new("2160h"), MaxUses: new(2), Approval: new(genv1.JoinTokenApprovalAuto), HardwareKeys: new(genv1.HardwareKeysRequired)})
	if err != nil || required.JSON201 == nil || required.JSON201.ExpiresAt == nil {
		t.Fatalf("a fleet's for 90 days, held to keys in a TPM = %v %s", err, required.Body)
	}
	if got := statusOf(client.UpdateJoinTokenWithResponse(ctx, required.JSON201.Id, genv1.JoinTokenUpdate{AllowedSources: []string{}})); got != http.StatusOK {
		t.Errorf("anywhere, for one held to keys in a TPM = %d", got)
	}
}

// mintedSecret is the secret of a token minted as ask says.
func mintedSecret(t *testing.T, client *genv1.ClientWithResponses, ask genv1.JoinTokenRequest) string {
	t.Helper()
	minted, err := client.CreateJoinTokenWithResponse(context.Background(), ask)
	if err != nil || minted.JSON201 == nil {
		t.Fatalf("= %v %s", err, minted.Body)
	}
	return minted.JSON201.Secret
}

func TestTheFakeRefusesWhatTheServerRefusesOfRunners(t *testing.T) {
	d := New(t)
	client := typed(t, d, "")
	ctx := context.Background()
	taken, err := client.CreateJoinTokenWithResponse(ctx, genv1.JoinTokenRequest{RunnerName: new("factory-03")})
	if err != nil || taken.JSON201 == nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		ask  genv1.JoinTokenRequest
		want int
	}{
		"a life that is not a duration":   {genv1.JoinTokenRequest{ExpiresIn: new("a week")}, 400},
		"a life longer than a day":        {genv1.JoinTokenRequest{ExpiresIn: new("25h")}, 400},
		"too many uses":                   {genv1.JoinTokenRequest{MaxUses: new(1_000_001)}, 400},
		"a host's own that never expires": {genv1.JoinTokenRequest{ExpiresIn: new("never")}, 400},
		"a fleet's for 14 minutes":        {genv1.JoinTokenRequest{ExpiresIn: new("14m"), MaxUses: new(2)}, 400},
		"auto, held to nothing":           {genv1.JoinTokenRequest{Approval: new(genv1.JoinTokenApprovalAuto), MaxUses: new(2)}, 400},
		"auto, a Rejoin":                  {genv1.JoinTokenRequest{Approval: new(genv1.JoinTokenApprovalAuto), HostID: new("r-01"), AllowedSources: &[]string{"203.0.113.0/24"}}, 400},
		"an approval that is not one":     {genv1.JoinTokenRequest{Approval: new(genv1.JoinTokenApproval("sometimes"))}, 400},
		"a setting that is not one":       {genv1.JoinTokenRequest{HardwareKeys: new(genv1.HardwareKeys("always"))}, 400},
		"a token not made as one is":      {genv1.JoinTokenRequest{Token: new("rjt_short_secret")}, 400},
		"a token of an id taken":          {genv1.JoinTokenRequest{Token: new("rjt_000000000001_" + strings.Repeat("ab", 24))}, 409},
		"a Rejoin for two":                {genv1.JoinTokenRequest{HostID: new("r-01"), MaxUses: new(2)}, 400},
		"a name as a hostID is written":   {genv1.JoinTokenRequest{RunnerName: new("r-07")}, 400},
		"a name that is not one":          {genv1.JoinTokenRequest{RunnerName: new("Factory")}, 400},
		"a fleet's name too long":         {genv1.JoinTokenRequest{RunnerName: new(strings.Repeat("a", 56)), MaxUses: new(2)}, 400},
		"a Rejoin naming it anew":         {genv1.JoinTokenRequest{HostID: new("r-01"), RunnerName: new("again")}, 400},
		"a Rejoin for nobody":             {genv1.JoinTokenRequest{HostID: new("r-99")}, 404},
		"a name a token will give":        {genv1.JoinTokenRequest{RunnerName: new("factory-03")}, 409},
	} {
		if got := statusOf(client.CreateJoinTokenWithResponse(ctx, c.ask)); got != c.want {
			t.Errorf("%s = %d, want %d", name, got, c.want)
		}
	}
	if got := statusOf(client.CreateJoinTokenWithBodyWithResponse(ctx, "application/json", strings.NewReader(`{"uses":1}`))); got != 400 {
		t.Errorf("a field the request does not have = %d", got)
	}
	for name, c := range map[string]struct {
		id   string
		body string
		want int
	}{
		"a token never minted": {"missing", `{"allowedSources":[]}`, 404},
		"no sources said":      {taken.JSON201.Id, `{}`, 400},
		"a field it has not":   {taken.JSON201.Id, `{"allowedSources":[],"labels":{}}`, 400},
		"anywhere":             {taken.JSON201.Id, `{"allowedSources":[]}`, 200},
	} {
		if got := statusOf(client.UpdateJoinTokenWithBodyWithResponse(ctx, c.id, "application/json", strings.NewReader(c.body))); got != c.want {
			t.Errorf("updating, %s = %d, want %d", name, got, c.want)
		}
	}
	if got := statusOf(client.RevokeJoinTokenWithResponse(ctx, "missing")); got != 404 {
		t.Errorf("revoking a token never minted = %d", got)
	}
	if got := statusOf(client.RevokeJoinTokenWithResponse(ctx, taken.JSON201.Id)); got != 204 {
		t.Errorf("revoking = %d", got)
	}
	if got := statusOf(client.UpdateJoinTokenWithResponse(ctx, taken.JSON201.Id, genv1.JoinTokenUpdate{AllowedSources: []string{}})); got != 409 {
		t.Errorf("updating a revoked token = %d", got)
	}
	// Revoked, its name is nobody's.
	if got := statusOf(client.CreateJoinTokenWithResponse(ctx, genv1.JoinTokenRequest{RunnerName: new("factory-03")})); got != 201 {
		t.Errorf("the name of a token revoked = %d", got)
	}
	for name, status := range map[string]int{
		"get":     statusOf(client.GetRunnerWithResponse(ctx, "r-99")),
		"update":  statusOf(client.UpdateRunnerWithResponse(ctx, "r-99", genv1.RunnerUpdate{AllowedSources: &[]string{}})),
		"drain":   statusOf(client.DrainRunnerWithResponse(ctx, "r-99", genv1.RunnerDrainRequest{})),
		"undrain": statusOf(client.UndrainRunnerWithResponse(ctx, "r-99")),
		"revoke":  statusOf(client.RevokeRunnerWithResponse(ctx, "r-99")),
		"unlist":  statusOf(client.UnlistRunnerWithResponse(ctx, "r-99")),
		"allow":   statusOf(client.AllowRunnerAddressWithResponse(ctx, "r-99", genv1.AllowAddress{Address: "198.51.100.7"})),
		"approve": statusOf(client.ApproveRunnerWithResponse(ctx, "r-99", genv1.RunnerApprove{})),
	} {
		if status != 404 {
			t.Errorf("%s of no runner = %d", name, status)
		}
	}
	if got := statusOf(client.UpdateRunnerWithBodyWithResponse(ctx, "r-01", "application/json", strings.NewReader(`{}`))); got != 400 {
		t.Errorf("an update saying nothing = %d", got)
	}
}

// The fake fails the test that joins with what is not a token that enrols.
func TestTheFakeJoinsNothingWithATokenThatCannotEnrol(t *testing.T) {
	d := New(t, WithClock(func() time.Time { return time.Date(2026, 11, 7, 9, 0, 0, 0, time.UTC) }))
	client := typed(t, d, "")
	minted, err := client.CreateJoinTokenWithResponse(context.Background(), genv1.JoinTokenRequest{})
	if err != nil || minted.JSON201 == nil {
		t.Fatal(err)
	}
	d.Join(t, minted.JSON201.Secret)
	for name, secret := range map[string]string{"no token": "rjt_x_y", "used up": minted.JSON201.Secret} {
		failing := &fatal{}
		func() {
			defer func() { _ = recover() }()
			d.Join(failing, secret)
		}()
		if !failing.failed {
			t.Errorf("%s: joined", name)
		}
	}
}

// fatal is a testing.TB whose Fatalf is recorded, and ends the call.
type fatal struct {
	testing.TB
	failed bool
}

func (f *fatal) Helper() {}

func (f *fatal) Fatalf(string, ...any) {
	f.failed = true
	panic("fatal")
}

func TestTheFakeKeepsRunnerSettings(t *testing.T) {
	d := New(t)
	client := typed(t, d, "")
	ctx := context.Background()
	got, err := client.GetRunnerSettingsWithResponse(ctx)
	if err != nil || got.JSON200 == nil || got.JSON200.HardwareKeys != genv1.HardwareKeysPreferred || len(got.JSON200.TrustedMakers) != 0 {
		t.Fatalf("settings = %v %s", err, got.Body)
	}
	changed, err := client.UpdateRunnerSettingsWithResponse(ctx, genv1.RunnerSettingsUpdate{HardwareKeys: new(genv1.HardwareKeysRequired)})
	if err != nil || changed.JSON200 == nil || changed.JSON200.HardwareKeys != genv1.HardwareKeysRequired {
		t.Fatalf("changed = %v %s", err, changed.Body)
	}
	if got := statusOf(client.UpdateRunnerSettingsWithBodyWithResponse(ctx, "application/json", strings.NewReader(`{"residency":{"label":"site","crossing":"allowed"}}`))); got != 400 {
		t.Errorf("a residency, which is no setting = %d", got)
	}
	if got := statusOf(client.UpdateRunnerSettingsWithResponse(ctx, genv1.RunnerSettingsUpdate{})); got != 400 {
		t.Errorf("saying nothing = %d", got)
	}
}

func TestTheFakeReportsWhatATestSaysWasUsed(t *testing.T) {
	now := time.Date(2026, 11, 7, 9, 30, 0, 0, time.UTC)
	d := New(t, WithClock(func() time.Time { return now }))
	client := typed(t, d, "")
	ctx := context.Background()
	d.Use(Consumed{Runner: "r-01", Sandbox: "s1", Principal: "p", Usage: genv1.Usage{SandboxSeconds: 60, Sandboxes: 1,
		Models: []genv1.ModelUsage{{Model: "m", Calls: 1, Input: 10}}}})
	d.Use(Consumed{Runner: "r-01", Sandbox: "s2", Usage: genv1.Usage{SandboxSeconds: 30, Sandboxes: 1, BrokerCalls: 2,
		Models: []genv1.ModelUsage{{Model: "m", Calls: 1, Input: 5}, {Model: "n", Calls: 1}}}})
	d.Use(Consumed{Runner: "r-02", Usage: genv1.Usage{VolumeGiBHours: 1}})

	report := func(params *genv1.GetUsageParams) genv1.UsageReport {
		t.Helper()
		got, err := client.GetUsageWithResponse(ctx, params)
		if err != nil || got.JSON200 == nil {
			t.Fatalf("usage %+v = %v %s", params, err, got.Body)
		}
		return *got.JSON200
	}
	week := report(nil)
	if week.Window != genv1.CostWindow7d || week.GroupBy != genv1.UsageByRunner || !week.To.Equal(now) || len(week.Groups) != 2 ||
		*week.Groups[0].Runner != "r-01" || week.Groups[0].Usage.SandboxSeconds != 90 || week.Total.VolumeGiBHours != 1 ||
		len(week.Total.Models) != 2 || week.Total.Models[0].Input != 15 {
		t.Errorf("a week by runner = %+v", week)
	}
	bySandbox := report(&genv1.GetUsageParams{GroupBy: new(genv1.UsageBySandbox), Runner: new("r-01")})
	if len(bySandbox.Groups) != 2 || *bySandbox.Groups[0].Sandbox != "s1" || bySandbox.Total.VolumeGiBHours != 0 {
		t.Errorf("r-01's by sandbox = %+v", bySandbox)
	}
	byPrincipal := report(&genv1.GetUsageParams{GroupBy: new(genv1.UsageByPrincipal)})
	if len(byPrincipal.Groups) != 2 || byPrincipal.Groups[0].Principal != nil || *byPrincipal.Groups[1].Principal != "p" {
		t.Errorf("by principal = %+v", byPrincipal)
	}
	byHour := report(&genv1.GetUsageParams{Window: new(genv1.CostWindow24h), GroupBy: new(genv1.UsageByHour)})
	if len(byHour.Groups) != 24 || byHour.Groups[23].Usage.Sandboxes != 2 || byHour.Groups[0].Usage.Sandboxes != 0 || !byHour.From.Equal(now.Truncate(time.Hour).Add(-23*time.Hour)) {
		t.Errorf("by hour = %d groups, from %s", len(byHour.Groups), byHour.From)
	}
	// What is older than the window is not in it.
	now = now.Add(25 * time.Hour)
	if day := report(&genv1.GetUsageParams{Window: new(genv1.CostWindow24h)}); len(day.Groups) != 0 || day.Total.Sandboxes != 0 {
		t.Errorf("a day later = %+v", day)
	}
	for name, params := range map[string]*genv1.GetUsageParams{
		"a window that is not one":   {Window: new(genv1.CostWindow("1y"))},
		"a grouping that is not one": {GroupBy: new(genv1.UsageGroupBy("tenant"))},
	} {
		if got := statusOf(client.GetUsageWithResponse(ctx, params)); got != 400 {
			t.Errorf("%s = %d", name, got)
		}
	}
}

func runnerState(t *testing.T, client *genv1.ClientWithResponses, id string) genv1.RunnerState {
	t.Helper()
	got, err := client.GetRunnerWithResponse(context.Background(), id)
	if err != nil || got.JSON200 == nil {
		t.Fatalf("%s = %v %s", id, err, got.Body)
	}
	return got.JSON200.State
}

// A runner provides what a test says it reports, and a runner that never
// joined provides nothing: the test that says it does fails.
func TestTheFakeSaysWhatARunnerProvides(t *testing.T) {
	d := New(t)
	client := typed(t, d, "")
	ctx := context.Background()
	minted, err := client.CreateJoinTokenWithResponse(ctx, genv1.JoinTokenRequest{})
	if err != nil || minted.JSON201 == nil {
		t.Fatal(err)
	}
	runner := d.Join(t, minted.JSON201.Secret)
	d.Provide(t, runner.Id, "llm", "codex")
	got, err := client.GetRunnerWithResponse(ctx, runner.Id)
	if err != nil || got.JSON200 == nil || got.JSON200.Provides == nil || !slices.Equal(*got.JSON200.Provides, []string{"llm", "codex"}) {
		t.Errorf("provides = %v %s", err, got.Body)
	}
	failing := &fatal{}
	func() {
		defer func() { _ = recover() }()
		d.Provide(failing, "r-99", "llm")
	}()
	if !failing.failed {
		t.Error("a runner that never joined provided a broker")
	}
}

// A pending runner is approved once, online then, saying when and by
// whom: the body's word, or the fake's administrator's.
func TestTheFakeApprovesAPendingRunner(t *testing.T) {
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	d := New(t, WithClock(func() time.Time { return at }))
	client := typed(t, d, "")
	ctx := context.Background()
	join := func() genv1.Runner {
		minted, err := client.CreateJoinTokenWithResponse(ctx, genv1.JoinTokenRequest{})
		if err != nil || minted.JSON201 == nil {
			t.Fatal(err)
		}
		return d.Join(t, minted.JSON201.Secret)
	}
	one, two := join(), join()
	got, err := client.ApproveRunnerWithResponse(ctx, one.Id, genv1.RunnerApprove{ApprovedBy: new("ada@acme.example")})
	if err != nil || got.JSON200 == nil || got.JSON200.State != genv1.RunnerStateRunnerOnline || *got.JSON200.ApprovedBy != "ada@acme.example" || !got.JSON200.ApprovedAt.Equal(at) {
		t.Fatalf("approved = %v %s", err, got.Body)
	}
	if got := statusOf(client.ApproveRunnerWithResponse(ctx, one.Id, genv1.RunnerApprove{})); got != 409 {
		t.Errorf("approved twice = %d", got)
	}
	if got := statusOf(client.ApproveRunnerWithBodyWithResponse(ctx, two.Id, "application/json", strings.NewReader(`{"approvedBy":""}`))); got != 400 {
		t.Errorf("approved by nobody = %d", got)
	}
	if got := statusOf(client.ApproveRunnerWithBodyWithResponse(ctx, two.Id, "application/json", strings.NewReader(`{"passkey":"x"}`))); got != 400 {
		t.Errorf("a field an approval has not = %d", got)
	}
	unsaid, err := client.ApproveRunnerWithBodyWithResponse(ctx, two.Id, "application/json", http.NoBody)
	if err != nil || unsaid.JSON200 == nil || *unsaid.JSON200.ApprovedBy != "admin@example.com" {
		t.Errorf("approved with no body = %v %s", err, unsaid.Body)
	}
	// Enrolled again, it waits for approval as a new one would.
	if got := statusOf(client.RevokeRunnerWithResponse(ctx, two.Id)); got != 202 {
		t.Errorf("revoked = %d", got)
	}
	if got := statusOf(client.ApproveRunnerWithResponse(ctx, two.Id, genv1.RunnerApprove{})); got != 409 {
		t.Errorf("a revoked runner approved = %d", got)
	}
}
