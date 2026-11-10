package sdk

import (
	"context"
	"fmt"
	"net/http"

	"github.com/google/uuid"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// A grant lends a model credential to the broker a sandbox's runner
// provides, for one turn (docs/design/runners, "Where the credential is
// kept: the per-turn grant"). Whoever keeps the credential puts it when a
// turn starts, again before ExpiresAt while the turn lasts, and deletes it
// on every way the turn ends: the server keeps nothing of it, and the
// runner holds it in memory alone.

// PutGrant lends broker, which the runner of sandbox sandboxID provides,
// the credential in grant for that sandbox alone, until grant.ExpiresAt or
// DeleteGrant. A new one replaces the one before.
func (c *Client) PutGrant(ctx context.Context, sandboxID, broker string, grant genv1.GrantWrite) error {
	id, err := sandboxUUID(sandboxID)
	if err != nil {
		return err
	}
	res, err := c.api.PutSandboxBrokerGrantWithResponse(ctx, id, broker, grant)
	if err != nil {
		return err
	}
	return want(http.StatusNoContent, res.HTTPResponse, res.Body)
}

// DeleteGrant takes back the credential lent to broker for sandbox
// sandboxID: the broker's next call for it finds none. Deleting one that is
// not there succeeds.
func (c *Client) DeleteGrant(ctx context.Context, sandboxID, broker string) error {
	id, err := sandboxUUID(sandboxID)
	if err != nil {
		return err
	}
	res, err := c.api.DeleteSandboxBrokerGrantWithResponse(ctx, id, broker)
	if err != nil {
		return err
	}
	return want(http.StatusNoContent, res.HTTPResponse, res.Body)
}

// sandboxUUID is a sandbox's id as the contract's paths take it: one that is
// not a uuid names no sandbox, and is refused before anything is sent.
func sandboxUUID(id string) (genv1.SandboxID, error) {
	parsed, err := uuid.Parse(id)
	if err != nil {
		return genv1.SandboxID{}, fmt.Errorf("runyard-sandboxes: %q is not a sandbox's id: %w", id, err)
	}
	return parsed, nil
}
