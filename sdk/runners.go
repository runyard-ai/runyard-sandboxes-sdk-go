package sdk

import (
	"context"
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// Runners are the hosts a server's machines run on, and adding, approving,
// draining and revoking them is an administrator's: what reads them needs
// `runners.read`, and what changes them `runners.admin`, which a key holds
// alone. A runner has an id, its hostID, which everything acts on it by,
// and a name a person reads, which its join token gives it. See the
// contract's Runners section.

// Runners is every runner, in the order they first joined: revoked ones
// too, which keep their ids.
func (c *Client) Runners(ctx context.Context) ([]genv1.Runner, error) {
	res, err := c.api.ListRunnersWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200.Items, nil
}

// Runner is one runner, by its id.
func (c *Client) Runner(ctx context.Context, id string) (genv1.Runner, error) {
	res, err := c.api.GetRunnerWithResponse(ctx, id)
	if err != nil {
		return genv1.Runner{}, err
	}
	return answered(res.JSON200, res.HTTPResponse, res.Body)
}

// SetRunnerSources replaces the addresses and ranges a runner may connect
// from, at once: a line held from one no longer allowed is dropped. None is
// anywhere.
func (c *Client) SetRunnerSources(ctx context.Context, id string, sources []string) (genv1.Runner, error) {
	if sources == nil {
		sources = []string{}
	}
	res, err := c.api.UpdateRunnerWithResponse(ctx, id, genv1.RunnerUpdate{AllowedSources: sources})
	if err != nil {
		return genv1.Runner{}, err
	}
	return answered(res.JSON200, res.HTTPResponse, res.Body)
}

// AllowRunnerAddress resolves a held runner for the address that held it,
// its HeldFrom: it is served from there, and the line from the other is
// dropped.
func (c *Client) AllowRunnerAddress(ctx context.Context, id, address string) (genv1.Runner, error) {
	res, err := c.api.AllowRunnerAddressWithResponse(ctx, id, genv1.AllowAddress{Address: address})
	if err != nil {
		return genv1.Runner{}, err
	}
	return answered(res.JSON200, res.HTTPResponse, res.Body)
}

// DrainRunner places nothing new on a runner, what runs there running on,
// and revokes it once it is empty when asked to. Its progress is the
// operation returned, and the runner's Operation from then on.
func (c *Client) DrainRunner(ctx context.Context, id string, ask genv1.RunnerDrainRequest) (genv1.RunnerOperation, error) {
	res, err := c.api.DrainRunnerWithResponse(ctx, id, ask)
	if err != nil {
		return genv1.RunnerOperation{}, err
	}
	return answered(res.JSON202, res.HTTPResponse, res.Body)
}

// UndrainRunner ends a drain: work is placed on the runner again.
func (c *Client) UndrainRunner(ctx context.Context, id string) (genv1.RunnerOperation, error) {
	res, err := c.api.UndrainRunnerWithResponse(ctx, id)
	if err != nil {
		return genv1.RunnerOperation{}, err
	}
	return answered(res.JSON202, res.HTTPResponse, res.Body)
}

// RevokeRunner revokes a runner, for good: nothing trusts it from the
// answer on, and what it held is let go of in the steps of the operation
// returned.
func (c *Client) RevokeRunner(ctx context.Context, id string) (genv1.RunnerOperation, error) {
	res, err := c.api.RevokeRunnerWithResponse(ctx, id)
	if err != nil {
		return genv1.RunnerOperation{}, err
	}
	return answered(res.JSON202, res.HTTPResponse, res.Body)
}

// ApproveRunner puts a pending runner in the bundle every broker trusts,
// with an approver's passkey assertion over the statement the contract
// spells out.
func (c *Client) ApproveRunner(ctx context.Context, id string, approval genv1.RunnerApproval) (genv1.Runner, error) {
	res, err := c.api.ApproveRunnerWithResponse(ctx, id, approval)
	if err != nil {
		return genv1.Runner{}, err
	}
	return answered(res.JSON200, res.HTTPResponse, res.Body)
}

// MintJoinToken makes a join token, and returns it with the only copy of
// its secret, Secret: a host joins with it as a runner, named
// ask.RunnerName. A name a runner has, or a usable token will give, is
// refused with `conflict`.
func (c *Client) MintJoinToken(ctx context.Context, ask genv1.JoinTokenRequest) (genv1.JoinTokenCreated, error) {
	res, err := c.api.CreateJoinTokenWithResponse(ctx, ask)
	if err != nil {
		return genv1.JoinTokenCreated{}, err
	}
	return answered(res.JSON201, res.HTTPResponse, res.Body)
}

// JoinTokens is every join token, newest first, used up, expired and
// revoked ones too. Never their secrets.
func (c *Client) JoinTokens(ctx context.Context) ([]genv1.JoinToken, error) {
	res, err := c.api.ListJoinTokensWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200.Items, nil
}

// RevokeJoinToken makes a join token enrol nothing more. What it enrolled
// stays enrolled. Revoking one twice succeeds.
func (c *Client) RevokeJoinToken(ctx context.Context, id string) error {
	res, err := c.api.RevokeJoinTokenWithResponse(ctx, id)
	if err != nil {
		return err
	}
	return want(http.StatusNoContent, res.HTTPResponse, res.Body)
}

// RunnerSettings is what the workspace asks of its runners: whether their
// keys must be in a TPM, and where its sandboxes may run.
func (c *Client) RunnerSettings(ctx context.Context) (genv1.RunnerSettings, error) {
	res, err := c.api.GetRunnerSettingsWithResponse(ctx)
	if err != nil {
		return genv1.RunnerSettings{}, err
	}
	return answered(res.JSON200, res.HTTPResponse, res.Body)
}

// UpdateRunnerSettings changes what change says, and leaves the rest.
func (c *Client) UpdateRunnerSettings(ctx context.Context, change genv1.RunnerSettingsUpdate) (genv1.RunnerSettings, error) {
	res, err := c.api.UpdateRunnerSettingsWithResponse(ctx, change)
	if err != nil {
		return genv1.RunnerSettings{}, err
	}
	return answered(res.JSON200, res.HTTPResponse, res.Body)
}

// Approvers are the passkeys whose assertions approve runners.
func (c *Client) Approvers(ctx context.Context) ([]genv1.Approver, error) {
	res, err := c.api.ListApproversWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200.Items, nil
}

// AddApprover adds a passkey that approves runners.
func (c *Client) AddApprover(ctx context.Context, ask genv1.ApproverRequest) (genv1.Approver, error) {
	res, err := c.api.AddApproverWithResponse(ctx, ask)
	if err != nil {
		return genv1.Approver{}, err
	}
	return answered(res.JSON201, res.HTTPResponse, res.Body)
}

// SealedCredential is the credential a person's brokers on runners use for
// them, sealed to each approved runner's broker key: what the server keeps
// of it, which it cannot open.
func (c *Client) SealedCredential(ctx context.Context, subject string) (genv1.SealedCredential, error) {
	res, err := c.api.GetSealedCredentialWithResponse(ctx, openapi_types.Email(subject))
	if err != nil {
		return genv1.SealedCredential{}, err
	}
	return answered(res.JSON200, res.HTTPResponse, res.Body)
}

// PutSealedCredential replaces subject's sealed credential.
func (c *Client) PutSealedCredential(ctx context.Context, subject string, sealed genv1.SealedCredentialWrite) (genv1.SealedCredential, error) {
	res, err := c.api.PutSealedCredentialWithResponse(ctx, openapi_types.Email(subject), sealed)
	if err != nil {
		return genv1.SealedCredential{}, err
	}
	return answered(res.JSON200, res.HTTPResponse, res.Body)
}

// DeleteSealedCredential removes subject's sealed credential, everywhere it
// was sealed to. Removing one that is not there succeeds.
func (c *Client) DeleteSealedCredential(ctx context.Context, subject string) error {
	res, err := c.api.DeleteSealedCredentialWithResponse(ctx, openapi_types.Email(subject))
	if err != nil {
		return err
	}
	return want(http.StatusNoContent, res.HTTPResponse, res.Body)
}

// SetRunnerBrokerCredential sets the credential of a broker the runner
// provides, sealed to its approved brokerKey (as `sealbox` seals, which a
// browser does with WebCrypto): the server keeps the box alone and the
// runner writes what it opens where that broker reads its credential. The
// answer says its kind and when it was set; the runner's brokerCredentials
// say once it has written it.
func (c *Client) SetRunnerBrokerCredential(ctx context.Context, runnerID, broker string, set genv1.RunnerBrokerCredentialWrite) (genv1.RunnerBrokerCredential, error) {
	res, err := c.api.SetRunnerBrokerCredentialWithResponse(ctx, runnerID, broker, set)
	if err != nil {
		return genv1.RunnerBrokerCredential{}, err
	}
	return answered(res.JSON200, res.HTTPResponse, res.Body)
}

// DeleteRunnerBrokerCredential takes away the credential set for a runner's
// broker: the runner removes the file it wrote it to.
func (c *Client) DeleteRunnerBrokerCredential(ctx context.Context, runnerID, broker string) error {
	res, err := c.api.DeleteRunnerBrokerCredentialWithResponse(ctx, runnerID, broker)
	if err != nil {
		return err
	}
	return want(http.StatusNoContent, res.HTTPResponse, res.Body)
}

// TransferVolume moves a volume, and the sandbox whose disk it is, to
// another runner: the answer is the volume as the move starts.
func (c *Client) TransferVolume(ctx context.Context, name string, ask genv1.VolumeTransfer) (genv1.Volume, error) {
	res, err := c.api.TransferVolumeWithResponse(ctx, name, ask)
	if err != nil {
		return genv1.Volume{}, err
	}
	return answered(res.JSON202, res.HTTPResponse, res.Body)
}

// UsageOf narrows Usage: Window is how far back, 7d when empty; GroupBy what
// each group is, runner when empty; Runner one runner's alone, by its id.
type UsageOf struct {
	Window  genv1.CostWindow
	GroupBy genv1.UsageGroupBy
	Runner  string
}

// Usage is what the whole workspace's sandboxes consumed over a window, on
// every runner — never what it cost, which whoever bills it prices. It
// needs `usage.read`.
func (c *Client) Usage(ctx context.Context, of UsageOf) (genv1.UsageReport, error) {
	params := &genv1.GetUsageParams{}
	if of.Window != "" {
		params.Window = &of.Window
	}
	if of.GroupBy != "" {
		params.GroupBy = &of.GroupBy
	}
	if of.Runner != "" {
		params.Runner = &of.Runner
	}
	res, err := c.api.GetUsageWithResponse(ctx, params)
	if err != nil {
		return genv1.UsageReport{}, err
	}
	return answered(res.JSON200, res.HTTPResponse, res.Body)
}

// answered is what the contract said the answer is, or the refusal it was
// instead.
func answered[T any](value *T, res *http.Response, body []byte) (T, error) {
	if value == nil {
		var zero T
		return zero, refused(res, body)
	}
	return *value, nil
}
