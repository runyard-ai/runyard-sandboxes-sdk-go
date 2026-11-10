package sdk

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"testing"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/test/doubles/fakeserver"
)

// A runner joined with a name is listed with it, and what an administrator
// does with it is a method each.
func TestARunnerJoinedWithANameIsListedWithIt(t *testing.T) {
	d, client := fake(t)
	ctx := context.Background()
	minted, err := client.MintJoinToken(ctx, genv1.JoinTokenRequest{RunnerName: new("factory-03"), Labels: &map[string]string{"site": "factory"}})
	if err != nil || minted.Secret == "" || *minted.RunnerName != "factory-03" {
		t.Fatalf("MintJoinToken = %+v, %v", minted, err)
	}
	if _, err := client.MintJoinToken(ctx, genv1.JoinTokenRequest{RunnerName: new("factory-03")}); CodeOf(err) != "conflict" {
		t.Errorf("a name a token will give = %v", err)
	}
	if changed, err := client.SetJoinTokenSources(ctx, minted.Id, []string{"203.0.113.24/32"}); err != nil || changed.Id != minted.Id || !slices.Equal(changed.AllowedSources, []string{"203.0.113.24/32"}) {
		t.Errorf("SetJoinTokenSources = %+v, %v", changed, err)
	}
	joined := d.Join(t, minted.Secret)
	if !slices.Equal(joined.AllowedSources, []string{"203.0.113.24/32"}) {
		t.Errorf("joined with sources %v, want the token's as changed", joined.AllowedSources)
	}
	if _, err := client.SetJoinTokenSources(ctx, minted.Id, nil); CodeOf(err) != "conflict" {
		t.Errorf("SetJoinTokenSources of a token used up = %v", err)
	}
	runners, err := client.Runners(ctx)
	if err != nil || len(runners) != 1 || runners[0].Id != joined.Id || runners[0].Name != "factory-03" {
		t.Fatalf("Runners = %+v, %v", runners, err)
	}
	one, err := client.Runner(ctx, joined.Id)
	if err != nil || one.Name != "factory-03" || one.Labels["site"] != "factory" {
		t.Fatalf("Runner = %+v, %v", one, err)
	}
	tokens, err := client.JoinTokens(ctx)
	if err != nil || len(tokens) != 1 || tokens[0].Uses != 1 {
		t.Fatalf("JoinTokens = %+v, %v", tokens, err)
	}
	if err := client.RevokeJoinToken(ctx, minted.Id); err != nil {
		t.Errorf("RevokeJoinToken = %v", err)
	}

	if anywhere, err := client.SetRunnerSources(ctx, joined.Id, nil); err != nil || anywhere.AllowedSources == nil || len(anywhere.AllowedSources) != 0 {
		t.Errorf("SetRunnerSources(nil) = %+v, %v", anywhere, err)
	}
	if renamed, err := client.RenameRunner(ctx, joined.Id, "factory-09"); err != nil || renamed.Name != "factory-09" || renamed.Id != joined.Id {
		t.Errorf("RenameRunner = %+v, %v", renamed, err)
	}
	asked, err := client.RotateRunnerCA(ctx, joined.Id)
	if err != nil || asked.CaRotationAskedAt == nil {
		t.Fatalf("RotateRunnerCA = %+v, %v", asked, err)
	}
	if again, err := client.RotateRunnerCA(ctx, joined.Id); err != nil || !again.CaRotationAskedAt.Equal(*asked.CaRotationAskedAt) {
		t.Errorf("RotateRunnerCA again = %+v, %v, want it asked as it was", again, err)
	}
	if _, err := client.AllowRunnerAddress(ctx, joined.Id, "198.51.100.7"); CodeOf(err) != "conflict" {
		t.Errorf("AllowRunnerAddress of a runner not held = %v", err)
	}
	drain, err := client.DrainRunner(ctx, joined.Id, genv1.RunnerDrainRequest{})
	if err != nil || drain.Kind != genv1.OperationDrain {
		t.Fatalf("DrainRunner = %+v, %v", drain, err)
	}
	if undrain, err := client.UndrainRunner(ctx, joined.Id); err != nil || undrain.Step != genv1.OperationCancelled {
		t.Errorf("UndrainRunner = %+v, %v", undrain, err)
	}
	revoke, err := client.RevokeRunner(ctx, joined.Id)
	if err != nil || revoke.Kind != genv1.OperationRevoke {
		t.Fatalf("RevokeRunner = %+v, %v", revoke, err)
	}
	if _, err := client.Runner(ctx, "r-99"); CodeOf(err) != "not_found" {
		t.Errorf("Runner of nobody = %v", err)
	}
	if _, err := client.UndrainRunner(ctx, joined.Id); CodeOf(err) != "conflict" {
		t.Errorf("UndrainRunner of a revoked runner = %v", err)
	}
	if _, err := client.RotateRunnerCA(ctx, joined.Id); CodeOf(err) != "conflict" {
		t.Errorf("RotateRunnerCA of a revoked runner = %v", err)
	}
	if unlisted, err := client.UnlistRunner(ctx, joined.Id); err != nil || unlisted.UnlistedAt == nil {
		t.Fatalf("UnlistRunner = %+v, %v", unlisted, err)
	}
	if listed, err := client.Runners(ctx); err != nil || slices.ContainsFunc(listed, func(r genv1.Runner) bool { return r.Id == joined.Id }) {
		t.Errorf("Runners = %+v, %v, without the one unlisted", listed, err)
	}
	if all, err := client.AllRunners(ctx); err != nil || !slices.ContainsFunc(all, func(r genv1.Runner) bool { return r.Id == joined.Id && r.UnlistedBy != nil }) {
		t.Errorf("AllRunners = %+v, %v, with the one unlisted", all, err)
	}

	settings, err := client.RunnerSettings(ctx)
	if err != nil || settings.HardwareKeys != genv1.HardwareKeysPreferred {
		t.Fatalf("RunnerSettings = %+v, %v", settings, err)
	}
	if changed, err := client.UpdateRunnerSettings(ctx, genv1.RunnerSettingsUpdate{HardwareKeys: new(genv1.HardwareKeysRequired)}); err != nil || changed.HardwareKeys != genv1.HardwareKeysRequired {
		t.Errorf("UpdateRunnerSettings = %+v, %v", changed, err)
	}
}

// A pending runner is approved, by whoever the call names or the key's
// owner, once; what the fake does not do — a move between runners — is
// answered for it by the test, and the SDK reads the answer.
func TestARunnersAdministratorApprovesItAndMovesWhatItHolds(t *testing.T) {
	d, client := fake(t)
	ctx := context.Background()
	token, err := client.MintJoinToken(ctx, genv1.JoinTokenRequest{RunnerName: new("factory-03")})
	if err != nil {
		t.Fatal(err)
	}
	runner := d.Join(t, token.Secret)
	approved, err := client.ApproveRunner(ctx, runner.Id, genv1.RunnerApprove{ApprovedBy: new("ada@acme.example")})
	if err != nil || approved.State != genv1.RunnerStateRunnerOnline || approved.ApprovedBy == nil || *approved.ApprovedBy != "ada@acme.example" || approved.ApprovedAt == nil {
		t.Errorf("ApproveRunner = %+v, %v", approved, err)
	}
	var refused *Error
	if _, err := client.ApproveRunner(ctx, runner.Id, genv1.RunnerApprove{}); !errors.As(err, &refused) || refused.Status != http.StatusConflict {
		t.Errorf("approved twice = %v", err)
	}
	if _, err := client.ApproveRunner(ctx, "r-99", genv1.RunnerApprove{}); CodeOf(err) != "not_found" {
		t.Errorf("nobody = %v", err)
	}
	other := d.Join(t, mustMint(t, client).Secret)
	if got, err := client.ApproveRunner(ctx, other.Id, genv1.RunnerApprove{}); err != nil || got.ApprovedBy == nil || *got.ApprovedBy == "" {
		t.Errorf("approved by the key's owner = %+v, %v", got, err)
	}

	d.Intercept("transferVolume", fakeserver.Interceptor(func(w http.ResponseWriter, _ *http.Request, _ http.HandlerFunc) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		_ = json.NewEncoder(w).Encode(genv1.Volume{Name: "data"})
	}))
	if got, err := client.TransferVolume(ctx, "data", genv1.VolumeTransfer{}); err != nil || got.Name != "data" {
		t.Errorf("TransferVolume = %+v, %v", got, err)
	}
	if _, err := client.TransferVolume(ctx, "data", genv1.VolumeTransfer{}); CodeOf(err) != "not_implemented" {
		t.Errorf("TransferVolume, not intercepted = %v", err)
	}
}

// mustMint is a join token, minted.
func mustMint(t *testing.T, client *Client) genv1.JoinTokenCreated {
	t.Helper()
	token, err := client.MintJoinToken(context.Background(), genv1.JoinTokenRequest{})
	if err != nil {
		t.Fatal(err)
	}
	return token
}

// The SDK reads /v1/usage, narrowed as it is asked.
func TestUsageIsReadNarrowedAsAsked(t *testing.T) {
	d, client := fake(t)
	ctx := context.Background()
	d.Use(fakeserver.Consumed{Runner: "r-01", Sandbox: "s1", Usage: genv1.Usage{SandboxSeconds: 60, Sandboxes: 1}})
	d.Use(fakeserver.Consumed{Runner: "r-02", Sandbox: "s2", Usage: genv1.Usage{SandboxSeconds: 30, Sandboxes: 1}})
	all, err := client.Usage(ctx, UsageOf{})
	if err != nil || all.Window != genv1.CostWindow7d || all.GroupBy != genv1.UsageByRunner || len(all.Groups) != 2 || all.Total.SandboxSeconds != 90 {
		t.Fatalf("Usage = %+v, %v", all, err)
	}
	one, err := client.Usage(ctx, UsageOf{Window: genv1.CostWindow24h, GroupBy: genv1.UsageBySandbox, Runner: "r-02"})
	if err != nil || one.Window != genv1.CostWindow24h || len(one.Groups) != 1 || *one.Groups[0].Sandbox != "s2" {
		t.Fatalf("Usage of r-02 by sandbox = %+v, %v", one, err)
	}
	if _, err := client.Usage(ctx, UsageOf{Window: "1y"}); CodeOf(err) != "bad_request" {
		t.Errorf("Usage over a window that is not one = %v", err)
	}
}

func TestRunnerCallsThatNeverReachTheServerSaySo(t *testing.T) {
	_, client := fake(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for name, err := range map[string]error{
		"Runners":              func() error { _, err := client.Runners(ctx); return err }(),
		"Runner":               func() error { _, err := client.Runner(ctx, "r-01"); return err }(),
		"SetRunnerSources":     func() error { _, err := client.SetRunnerSources(ctx, "r-01", nil); return err }(),
		"RenameRunner":         func() error { _, err := client.RenameRunner(ctx, "r-01", "a"); return err }(),
		"AllowRunnerAddress":   func() error { _, err := client.AllowRunnerAddress(ctx, "r-01", "a"); return err }(),
		"DrainRunner":          func() error { _, err := client.DrainRunner(ctx, "r-01", genv1.RunnerDrainRequest{}); return err }(),
		"UndrainRunner":        func() error { _, err := client.UndrainRunner(ctx, "r-01"); return err }(),
		"RevokeRunner":         func() error { _, err := client.RevokeRunner(ctx, "r-01"); return err }(),
		"UnlistRunner":         func() error { _, err := client.UnlistRunner(ctx, "r-01"); return err }(),
		"RotateRunnerCA":       func() error { _, err := client.RotateRunnerCA(ctx, "r-01"); return err }(),
		"AllRunners":           func() error { _, err := client.AllRunners(ctx); return err }(),
		"ApproveRunner":        func() error { _, err := client.ApproveRunner(ctx, "r-01", genv1.RunnerApprove{}); return err }(),
		"MintJoinToken":        func() error { _, err := client.MintJoinToken(ctx, genv1.JoinTokenRequest{}); return err }(),
		"JoinTokens":           func() error { _, err := client.JoinTokens(ctx); return err }(),
		"RevokeJoinToken":      client.RevokeJoinToken(ctx, "x"),
		"SetJoinTokenSources":  func() error { _, err := client.SetJoinTokenSources(ctx, "x", nil); return err }(),
		"RunnerSettings":       func() error { _, err := client.RunnerSettings(ctx); return err }(),
		"UpdateRunnerSettings": func() error { _, err := client.UpdateRunnerSettings(ctx, genv1.RunnerSettingsUpdate{}); return err }(),
		"TransferVolume":       func() error { _, err := client.TransferVolume(ctx, "data", genv1.VolumeTransfer{}); return err }(),
		"Usage":                func() error { _, err := client.Usage(ctx, UsageOf{}); return err }(),
	} {
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s = %v", name, err)
		}
	}
}

// What a server refuses to list is the caller's error.
func TestARunnersListRefusedIsAnError(t *testing.T) {
	d, client := fake(t)
	ctx := context.Background()
	for _, op := range []string{"listRunners", "listJoinTokens"} {
		d.Intercept(op, fakeserver.Refuse(http.StatusForbidden, genv1.ErrorCodeForbidden, "runners.read"))
	}
	if _, err := client.Runners(ctx); CodeOf(err) != "forbidden" {
		t.Errorf("Runners = %v", err)
	}
	if _, err := client.JoinTokens(ctx); CodeOf(err) != "forbidden" {
		t.Errorf("JoinTokens = %v", err)
	}
}
