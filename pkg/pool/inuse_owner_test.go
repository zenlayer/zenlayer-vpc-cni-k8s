// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package pool

import (
	"sort"
	"testing"
)

// TestInuseByOwner covers the lookup the pool provides when the stored pod
// record is gone -- the case a lost database leaves behind. Without it the
// address stayed in use until reconcile noticed (ReconcileIntervalSec, 1200s by
// default) and the host rules naming it were never removed at all; the caller is
// releaseOrphanedResources.
func TestInuseByOwner(t *testing.T) {
	p := newTestPool()

	p.AddInuse(&fakeRes{id: "eni-1/10.0.0.5"}, "default/pod-a")
	p.AddInuse(&fakeRes{id: "eni-1/10.0.0.6"}, "default/pod-a")
	p.AddInuse(&fakeRes{id: "eni-2/10.0.0.7"}, "default/pod-b")
	p.AddAvailable(&fakeRes{id: "eni-2/10.0.0.8"})

	got := []string{}
	for _, res := range p.InuseByOwner("default/pod-a") {
		got = append(got, res.GetID())
	}
	sort.Strings(got)
	want := []string{"eni-1/10.0.0.5", "eni-1/10.0.0.6"}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Errorf("InuseByOwner(pod-a) = %v, want %v", got, want)
	}

	if n := len(p.InuseByOwner("default/pod-b")); n != 1 {
		t.Errorf("InuseByOwner(pod-b) returned %d resources, want 1", n)
	}
	// An unknown owner, and the empty one in particular, must match nothing --
	// not the available entry queued above, which carries no owner.
	// releaseOrphanedResources acts on whatever comes back: it unassigns the
	// address and tears down the host rules naming it.
	if n := len(p.InuseByOwner("default/pod-c")); n != 0 {
		t.Errorf("InuseByOwner(unknown owner) returned %d resources, want 0", n)
	}
	if n := len(p.InuseByOwner("")); n != 0 {
		t.Errorf("InuseByOwner(\"\") returned %d resources, want 0", n)
	}
}
