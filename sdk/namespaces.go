package sdk

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"github.com/runyard-ai/runyard-sandboxes-sdk-go/sandboxes/genv1"
)

// ErrNamespaceRequired is what a call about something in a namespace returns,
// without sending anything, on a client whose key is bound to no namespace and
// which was not told one: a global key reaches several, and which one a call
// is about is the caller's to say, with Namespace or WithNamespace.
var ErrNamespaceRequired = errors.New("runyard-sandboxes: this key is bound to no namespace, so a call about sandboxes, volumes or tunnels names one: client.Namespace(name)")

// WithNamespace is the namespace every call about sandboxes, volumes and
// tunnels is about, for a caller that already knows it: the client does not
// ask the daemon which namespace its key is bound to. The daemon still holds
// the key to it, on every call.
func WithNamespace(name string) Option {
	return func(o *options) { o.namespace = name }
}

// Namespace is this client bound to the namespace name: the same daemon, key
// and options, with every call about sandboxes, volumes and tunnels going to
// name. It sends nothing. Its first such call asks the daemon once what the
// key is — the answer this client has already had, if it has — and a key
// bound to another namespace fails that call, and every one after, with an
// error naming both, rather than a 404 from the daemon that would name
// neither. Empty is the namespace the key is bound to, as New's is.
func (c *Client) Namespace(name string) *Client {
	bound := *c
	bound.namespace = name
	bound.trusted = false
	return &bound
}

// Namespaces is the namespaces this client's key reaches, by name: the one it
// is bound to, or for a global key those its owner is a member of — every one,
// for an administrator. It is the answer the client asks for once and keeps.
func (c *Client) Namespaces(ctx context.Context) ([]string, error) {
	me, err := c.me.get(ctx, c.askMe)
	if err != nil {
		return nil, err
	}
	if me.Namespaces == nil {
		return []string{}, nil
	}
	return append([]string{}, *me.Namespaces...), nil
}

// ResolveNamespace is the namespace this client's calls about sandboxes,
// volumes and tunnels go to — what a caller of API passes those operations —
// or the error each such call would return: ErrNamespaceRequired for a global
// key that was told none, and a key bound elsewhere than this client was told.
func (c *Client) ResolveNamespace(ctx context.Context) (string, error) {
	if c.trusted && c.namespace != "" {
		return c.namespace, nil
	}
	me, err := c.me.get(ctx, c.askMe)
	if err != nil {
		return "", err
	}
	var bound string
	if me.Key != nil && me.Key.Namespace != nil {
		bound = *me.Key.Namespace
	}
	switch {
	case c.namespace != "" && bound != "" && bound != c.namespace:
		return "", fmt.Errorf("runyard-sandboxes: this key is bound to namespace %s, and reaches nothing in namespace %s", bound, c.namespace)
	case c.namespace != "":
		return c.namespace, nil
	case bound != "":
		return bound, nil
	}
	return "", ErrNamespaceRequired
}

// askMe is who the daemon says this client's key is.
func (c *Client) askMe(ctx context.Context) (*genv1.Me, error) {
	res, err := c.api.GetMeWithResponse(ctx)
	if err != nil {
		return nil, err
	}
	if res.JSON200 == nil {
		return nil, refused(res.HTTPResponse, res.Body)
	}
	return res.JSON200, nil
}

// whoami is the daemon's answer to who a key is, asked for once and kept for
// as long as the clients that share it: a key's namespace never changes.
//
// Only an answer is kept. A call that could not be answered — the daemon
// away, the caller's context ended — leaves the next call to ask again,
// rather than leaving the client failing for the rest of its life on what
// went wrong once.
type whoami struct {
	mu sync.Mutex
	me *genv1.Me
	// asking is closed when the one call asking now has its answer, or has
	// failed; nil while nobody asks.
	asking chan struct{}
}

// get is the kept answer, or asks for it with ask. Callers that arrive while
// another asks wait for that answer rather than asking too, and stop waiting
// when their own context ends; if the asker fails, one of them asks in turn.
func (w *whoami) get(ctx context.Context, ask func(context.Context) (*genv1.Me, error)) (*genv1.Me, error) {
	for {
		w.mu.Lock()
		if w.me != nil {
			me := w.me
			w.mu.Unlock()
			return me, nil
		}
		if waiting := w.asking; waiting != nil {
			w.mu.Unlock()
			select {
			case <-waiting:
				continue
			case <-ctx.Done():
				return nil, ctx.Err()
			}
		}
		done := make(chan struct{})
		w.asking = done
		w.mu.Unlock()

		me, err := ask(ctx)

		w.mu.Lock()
		if err == nil {
			w.me = me
		}
		w.asking = nil
		close(done)
		w.mu.Unlock()
		return me, err
	}
}
