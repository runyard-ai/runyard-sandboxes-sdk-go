package spawn

import (
	"testing"

	"go.uber.org/goleak"
)

func TestMain(m *testing.M) { goleak.VerifyTestMain(m) }

// What Go returns waits for f, and f runs beside the test rather than in it.
func TestWaitReturnsOnceFHas(t *testing.T) {
	release := make(chan struct{})
	ran := false
	wait := Go(t, func() {
		<-release // the test goes on while f waits here
		ran = true
	})
	close(release)
	wait()
	if !ran {
		t.Fatal("wait returned before f did")
	}
	wait() // again: f has returned, and waiting is nothing
}

// A test that never waits does not end before f has: its cleanup waits.
func TestTheTestsCleanupWaitsForF(t *testing.T) {
	release := make(chan struct{})
	ran := false
	t.Run("never waits", func(t *testing.T) {
		Go(t, func() {
			<-release
			ran = true
		})
		close(release)
	})
	if !ran {
		t.Fatal("the subtest ended before f did")
	}
}
