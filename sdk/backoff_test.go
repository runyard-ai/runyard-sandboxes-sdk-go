package sdk

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

// A backoff pauses first, then twice as long each time, up to most.
func TestABackoffDoublesUpToItsMost(t *testing.T) {
	for name, tc := range map[string]struct {
		b    backoff
		want []time.Duration
	}{
		"doubling":   {backoff{first: time.Second, most: 5 * time.Second}, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 5 * time.Second, 5 * time.Second}},
		"fixed":      {backoff{first: 2 * time.Second, most: 2 * time.Second}, []time.Duration{2 * time.Second, 2 * time.Second, 2 * time.Second}},
		"most below": {backoff{first: 3 * time.Second, most: time.Second}, []time.Duration{3 * time.Second, 3 * time.Second}},
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := tc.b
				for i, want := range tc.want {
					start := time.Now()
					if !b.wait(t.Context()) {
						t.Fatalf("pause %d ended early", i)
					}
					if got := time.Since(start); got != want {
						t.Errorf("pause %d = %s, want %s", i, got, want)
					}
				}
			})
		})
	}
}

// A pause is not waited out once its context has ended, and says so.
func TestABackoffEndsWithItsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		b := backoff{first: time.Minute, most: time.Minute}
		start := time.Now()
		if b.wait(ctx) {
			t.Error("a pause outlasted its context")
		}
		if got := time.Since(start); got != time.Second {
			t.Errorf("waited %s, want the second its context had", got)
		}
		if b.wait(ctx) {
			t.Error("a pause on an ended context")
		}
	})
}
