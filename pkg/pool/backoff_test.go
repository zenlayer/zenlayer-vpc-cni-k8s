// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package pool

import (
	"testing"
	"time"
)

// TestBackoffNeverStaysAtZero is the regression this file exists for.
//
// The period was doubled on every failure and reset to the default on success,
// and nothing set it beforehand -- so until the factory succeeded once it was
// the zero value, doubling to zero. Both failure paths then slept for 0s and
// went straight back to the platform API. The pools that never see a success
// are exactly the ones in trouble: unreachable platform, rejected credentials,
// exhausted quota. None of them clear while the worker spins.
func TestBackoffNeverStaysAtZero(t *testing.T) {
	// The zero value on purpose: whatever built this pool, the first failure
	// must produce a real pause.
	p := &poolImpl{}

	p.backoffCallFactory()

	if p.backoff <= 0 {
		t.Fatalf("backoff = %v after the first failure, want a positive period -- a zero period "+
			"turns the retry into a loop against the platform API", p.backoff)
	}
	if p.backoff != defaultPoolBackoffPeriod {
		t.Errorf("backoff = %v, want the default %v", p.backoff, defaultPoolBackoffPeriod)
	}
}

// TestBackoffGrowsAndStopsAtTheMaximum covers the other half: the gate was on
// the value before doubling, so 40s passed it and became 80s -- above the
// maximum the constant names.
func TestBackoffGrowsAndStopsAtTheMaximum(t *testing.T) {
	p := &poolImpl{backoff: defaultPoolBackoffPeriod}

	prev := p.backoff
	// Enough rounds to pass the cap several times over from the default.
	for i := 0; i < 12; i++ {
		p.backoffCallFactory()
		if p.backoff > maxPoolBackoffPeriod {
			t.Fatalf("round %d: backoff = %v, above the maximum %v", i, p.backoff, maxPoolBackoffPeriod)
		}
		if p.backoff < prev {
			t.Fatalf("round %d: backoff shrank from %v to %v", i, prev, p.backoff)
		}
		prev = p.backoff
	}
	if p.backoff != maxPoolBackoffPeriod {
		t.Errorf("backoff settled at %v, want it to reach the maximum %v", p.backoff, maxPoolBackoffPeriod)
	}
}

// TestBackoffResetReturnsToTheDefault pins that a success undoes the growth --
// otherwise one bad spell leaves the pool slow to refill for the rest of the
// process.
func TestBackoffResetReturnsToTheDefault(t *testing.T) {
	p := &poolImpl{backoff: maxPoolBackoffPeriod}

	p.backoffReset()

	if p.backoff != defaultPoolBackoffPeriod {
		t.Errorf("backoff = %v after a reset, want %v", p.backoff, defaultPoolBackoffPeriod)
	}
}

// TestNewResourcePoolStartsWithABackoff pins the constructor rather than the
// guard above, because the failure paths log p.backoff *before* lengthening it:
// with the field unset the operator was told "backoff: 0s" on exactly the
// failure that matters most, while the pause actually taken was something else.
func TestNewResourcePoolStartsWithABackoff(t *testing.T) {
	f := &slowReleaseFactory{entered: make(chan struct{}), proceed: make(chan struct{})}
	close(f.proceed)

	rp, err := NewResourcePool(Config{
		Name:    "backoff-test",
		Type:    "test",
		Factory: f,
		// Long enough that the worker this starts just sits on its timer for
		// the life of the test.
		MonitorInterval: time.Hour,
	})
	if err != nil {
		t.Fatalf("NewResourcePool: %v", err)
	}

	impl, ok := rp.(*poolImpl)
	if !ok {
		t.Fatalf("NewResourcePool returned %T, want *poolImpl", rp)
	}
	if impl.backoff != defaultPoolBackoffPeriod {
		t.Errorf("a fresh pool has backoff = %v, want %v -- the failure paths log this value",
			impl.backoff, defaultPoolBackoffPeriod)
	}
}
