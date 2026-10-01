// Package spawn runs a function beside a test, and does not let the test end
// before it has.
//
// It is how this module's tests start a goroutine: one that would outlive its
// test runs into the next one, and goleak, which holds every package here,
// blames whichever test happens to be last. Import it from tests only.
package spawn

import "testing"

// Go runs f on a goroutine of its own, and returns what waits for it to
// return. The test's cleanup waits too, so a test that never calls wait still
// does not end before f has.
func Go(tb testing.TB, f func()) (wait func()) {
	tb.Helper()
	done := make(chan struct{})
	//task:unowned joined by the wait it returns, and by the test's cleanup
	go func() {
		defer close(done)
		f()
	}()
	wait = func() { <-done }
	tb.Cleanup(wait)
	return wait
}
