// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"testing"
	"time"
)

// gc must debounce: a burst of WakeGC signals should not turn into a burst of
// GCs. eniIPManager is nil here, so gc() does no real work and only the
// debounce/bookkeeping path is exercised.
func TestGCDebounce(t *testing.T) {
	d := &ipamDaemon{}

	d.gc()
	first := d.lastGC
	if first.IsZero() {
		t.Fatal("lastGC not recorded after the first gc")
	}

	for i := 0; i < 5; i++ {
		d.gc()
	}
	if !d.lastGC.Equal(first) {
		t.Errorf("lastGC moved during the debounce window: %v -> %v", first, d.lastGC)
	}

	// Backdated past the window, so gc runs again.
	d.lastGC = time.Now().Add(-2 * gcDebounce)
	d.gc()
	if !d.lastGC.After(first) {
		t.Errorf("gc did not run after the debounce window elapsed")
	}
}

// The two GC timing constants are pinned here: a non-positive debounce disables
// debouncing, and a slow threshold outside (0, daemonRPCTimeout) either warns on
// every GC or warns only once the CNI ADD waiting on the paused pool has already
// timed out. Despite the name, the two are not compared against each other.
func TestGCDebounceShorterThanSlowThreshold(t *testing.T) {
	if gcDebounce <= 0 {
		t.Errorf("gcDebounce = %v, want a positive duration", gcDebounce)
	}
	if gcSlowThreshold <= 0 || gcSlowThreshold >= daemonRPCTimeout {
		t.Errorf("gcSlowThreshold = %v, want it between 0 and the CNI timeout %v",
			gcSlowThreshold, daemonRPCTimeout)
	}
}
