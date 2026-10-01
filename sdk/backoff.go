package sdk

import (
	"context"
	"time"
)

// backoff is the pause between attempts at something across a network, which
// cannot say when it is worth trying again: first, then twice as long each
// time, up to most.
//
// Its own, rather than the daemon's: the SDK is a module that requires nothing
// of the daemon's code, and is imported by programs that are not the daemon.
type backoff struct {
	first, most time.Duration

	next time.Duration
}

// wait waits out the next pause, and says false — at once — when ctx ended
// first.
func (b *backoff) wait(ctx context.Context) bool {
	pause := max(b.next, b.first)
	b.next = min(pause*2, max(b.most, b.first))
	timer := time.NewTimer(pause)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return ctx.Err() == nil
	}
}
