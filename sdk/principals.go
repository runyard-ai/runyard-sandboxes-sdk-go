package sdk

import (
	"context"
	"fmt"
	"io"
	"net/http"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// A principal is an identity that outlives a sandbox, in the client's
// namespace: `{id, subject}`, where the id is stable and the subject is who
// it acts for now. A sandbox made with Spec.Principal acts for one, and every
// broker it reaches is told so by its certificate; a volume such a sandbox
// first attaches is bound to the principal, and attached by sandboxes acting
// for it alone (MoveVolume moves it). See the contract's Principals section.

// Principals is every principal in the client's namespace, by id.
func (c *Client) Principals(ctx context.Context) ([]genv1.Principal, error) {
	namespace, err := c.ResolveNamespace(ctx)
	if err != nil {
		return nil, err
	}
	res, err := c.api.ListPrincipalsWithResponse(ctx, namespace)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200.Items, nil
}

// PutPrincipal makes the principal id, or replaces its subject and labels:
// idempotent, so a caller that makes one each time it is needed gets the
// same one. Another subject is the same principal acting for somebody else:
// the daemon cuts the broker connections of the sandboxes acting for it, and
// what they connect with next presents the new subject.
func (c *Client) PutPrincipal(ctx context.Context, id string, p genv1.PrincipalSpec) (genv1.Principal, error) {
	namespace, err := c.ResolveNamespace(ctx)
	if err != nil {
		return genv1.Principal{}, err
	}
	res, err := c.api.PutPrincipalWithResponse(ctx, namespace, id, p)
	if err != nil {
		return genv1.Principal{}, err
	}
	switch {
	case res.JSON201 != nil:
		return *res.JSON201, nil
	case res.JSON200 != nil:
		return *res.JSON200, nil
	}
	return genv1.Principal{}, refused(res.HTTPResponse, res.Body)
}

// Principal is one principal of the client's namespace.
func (c *Client) Principal(ctx context.Context, id string) (genv1.Principal, error) {
	namespace, err := c.ResolveNamespace(ctx)
	if err != nil {
		return genv1.Principal{}, err
	}
	res, err := c.api.GetPrincipalWithResponse(ctx, namespace, id)
	if err != nil {
		return genv1.Principal{}, err
	}
	if res.JSON200 == nil {
		return genv1.Principal{}, refused(res.HTTPResponse, res.Body)
	}
	return *res.JSON200, nil
}

// DeletePrincipal deletes a principal. Deleting one that is not there
// succeeds; one a sandbox acts for, or a volume is bound to, is refused
// with `principal_in_use`.
func (c *Client) DeletePrincipal(ctx context.Context, id string) error {
	namespace, err := c.ResolveNamespace(ctx)
	if err != nil {
		return err
	}
	res, err := c.api.DeletePrincipalWithResponse(ctx, namespace, id)
	if err != nil {
		return err
	}
	return want(http.StatusNoContent, res.HTTPResponse, res.Body)
}

// BrokerTrusted reads a shared broker's document about a principal through
// the broker's trusted plane: path is under `.well-known/`, as
// `.well-known/runyard-broker`, and is sent escaped as the contract says.
//
// What the BROKER answered — whatever its status, a 404 saying there is none
// among them — is returned as its status, its Content-Type and its body, with
// a nil error. A refusal of the daemon's own is the error: no such principal
// or broker (`not_found`), a broker with no trusted plane
// (`no_trusted_plane`), a path that is not one (`bad_request`), and a broker
// that could not be reached or answered with more than 1 MiB
// (`upstream_error`).
func (c *Client) BrokerTrusted(ctx context.Context, principal, broker, path string) (status int, contentType string, body []byte, err error) {
	namespace, err := c.ResolveNamespace(ctx)
	if err != nil {
		return 0, "", nil, err
	}
	res, err := c.api.GetBrokerTrusted(ctx, namespace, principal, broker, path)
	if err != nil {
		return 0, "", nil, err
	}
	defer closeBody(res.Body)
	// The daemon passes on at most a mebibyte; anything past that here is
	// not a broker's answer, and is not read into memory.
	body, err = io.ReadAll(io.LimitReader(res.Body, maxTrustedBody+1))
	if err != nil {
		return 0, "", nil, fmt.Errorf("runyard-sandboxes: reading a trusted document: %w", err)
	}
	if res.Header.Get(brokerAnswered) != "true" {
		return 0, "", nil, refused(res, body)
	}
	if len(body) > maxTrustedBody {
		return 0, "", nil, fmt.Errorf("runyard-sandboxes: the daemon passed on more than %d bytes of a trusted document", maxTrustedBody)
	}
	return res.StatusCode, res.Header.Get("Content-Type"), body, nil
}

// brokerAnswered marks an answer that is the broker's own, rather than a
// refusal of the daemon's.
const brokerAnswered = "Runyard-Broker-Answered"

// maxTrustedBody is the most of a trusted document the daemon passes on.
const maxTrustedBody = 1 << 20

// MoveVolume binds a volume to a principal, from the one it was bound to or
// from none: from then on only sandboxes acting for that principal attach
// it. Refused with `volume_in_use` while a sandbox holds it.
func (c *Client) MoveVolume(ctx context.Context, volume, principal string) error {
	namespace, err := c.ResolveNamespace(ctx)
	if err != nil {
		return err
	}
	res, err := c.api.MoveVolumeWithResponse(ctx, namespace, volume, genv1.VolumePrincipal{Principal: principal})
	if err != nil {
		return err
	}
	if res.JSON200 == nil {
		return refused(res.HTTPResponse, res.Body)
	}
	return nil
}
