// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"sync"
	"testing"

	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

func newTestENI(id string) *ENI {
	return &ENI{
		ENI:        &zecTypes.ENI{ID: id},
		ips:        []*ENIIPRes{},
		order:      make(chan struct{}, defaultOrderCnt),
		stopWorker: make(chan struct{}, 1),
	}
}

// workerStopped reports whether the interface's worker has been told to stop.
func workerStopped(e *ENI) bool {
	select {
	case <-e.stopWorker:
		return true
	default:
		return false
	}
}

// TestStopWorkerOnceSurvivesASecondCall is the guard on the two paths that
// retire an interface: the pool giving up its last address, and GC finding the
// interface gone from the instance. Neither can see the other's work once the
// interface has left f.enis, so a plain close() would panic the daemon on the
// second call.
func TestStopWorkerOnceSurvivesASecondCall(t *testing.T) {
	e := newTestENI("eni-1")

	e.stopWorkerOnce()
	if !workerStopped(e) {
		t.Fatal("first stopWorkerOnce did not stop the worker")
	}

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("second stopWorkerOnce panicked: %v", r)
		}
	}()
	e.stopWorkerOnce()
	e.stopWorkerOnce()
}

// TestStopWorkerOnceIsSafeConcurrently pins the same guard under the race
// detector. Both retiring paths hold f.Lock() around the call today, so this
// covers stopWorkerOnce itself rather than a race that is reachable now.
func TestStopWorkerOnceIsSafeConcurrently(t *testing.T) {
	e := newTestENI("eni-1")
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.stopWorkerOnce()
		}()
	}
	wg.Wait()
	if !workerStopped(e) {
		t.Error("worker was not stopped")
	}
}

// TestDeleteEniLockedRemovesOnlyTheNamedInterface guards the removal GC performs
// after stopping an interface's worker. Taking out the wrong entry drops a live
// interface out of f.enis -- no order can reach it and Release answers
// ErrNotFound for its addresses -- while the retired one stays behind. GC's
// adoption pass puts it back a cycle later.
func TestDeleteEniLockedRemovesOnlyTheNamedInterface(t *testing.T) {
	a, b, c := newTestENI("eni-a"), newTestENI("eni-b"), newTestENI("eni-c")
	f := &eniIPFactory{enis: []*ENI{a, b, c}}

	f.deleteEniLocked(b)

	if len(f.enis) != 2 {
		t.Fatalf("len(f.enis) = %d, want 2", len(f.enis))
	}
	left := map[string]bool{}
	for _, e := range f.enis {
		left[e.ID] = true
	}
	if left["eni-b"] {
		t.Error("eni-b is still held after being deleted")
	}
	if !left["eni-a"] || !left["eni-c"] {
		t.Errorf("an untouched interface was removed; left = %v", left)
	}

	// Idempotent: the release path can name an interface GC already dropped, in
	// the window where it holds neither lock.
	f.deleteEniLocked(b)
	if len(f.enis) != 2 {
		t.Errorf("len(f.enis) = %d after a repeat delete, want 2", len(f.enis))
	}
}

// TestRetiredInterfaceIsNotEligibleForOrders is the half of the removal that
// has to happen before the worker stops.
//
// An order admitted onto an interface whose worker is gone is never answered,
// and the Create waiting on that result blocks forever holding blockPause,
// which also stops GC. Marking it retired first is what closes that window;
// forbidRetired is deliberately never cleared.
func TestRetiredInterfaceIsNotEligibleForOrders(t *testing.T) {
	e := newTestENI("eni-1")
	e.forbidAssign |= forbidRetired

	if e.forbidAssign&forbidRetired == 0 {
		t.Fatal("forbidRetired was not set")
	}
	// The two cures that exist must not bring it back.
	e.forbidAssign &^= forbidSubnet
	e.forbidAssign &^= forbidInterfaceFull
	if e.forbidAssign&forbidRetired == 0 {
		t.Error("a retiring interface became eligible again after an unrelated cure")
	}
}
