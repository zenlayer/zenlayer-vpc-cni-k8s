// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package pool

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// slowReleaseFactory reports every resource invalid, so Release always reaches
// ReleaseInValid, and blocks inside it until the test lets go -- standing in for
// the platform write with its exponential backoff and metadata poll.
type slowReleaseFactory struct {
	entered  chan struct{}
	proceed  chan struct{}
	ret      types.NetResource
	retErr   error
	calls    int
	callsMu  sync.Mutex
	validErr error
}

func (f *slowReleaseFactory) Name() string                            { return "slow release factory" }
func (f *slowReleaseFactory) Create(int) ([]types.NetResource, error) { return nil, nil }
func (f *slowReleaseFactory) Release(types.NetResource) error         { return nil }
func (f *slowReleaseFactory) GC() error                               { return nil }
func (f *slowReleaseFactory) GetResourceLimit() int                   { return 10 }
func (f *slowReleaseFactory) Valid(types.NetResource) error           { return f.validErr }
func (f *slowReleaseFactory) List() (map[types.ResStatus]map[string]types.NetResource, error) {
	return nil, nil
}

func (f *slowReleaseFactory) ReleaseInValid(types.NetResource) (types.NetResource, error) {
	f.callsMu.Lock()
	f.calls++
	f.callsMu.Unlock()
	close(f.entered)
	<-f.proceed
	return f.ret, f.retErr
}

func (f *slowReleaseFactory) callCount() int {
	f.callsMu.Lock()
	defer f.callsMu.Unlock()
	return f.calls
}

func releasePool(t *testing.T, id string, f *slowReleaseFactory) *poolImpl {
	t.Helper()
	p := newTestPool()
	p.factory = f
	p.inUse[id] = poolItem{res: &fakeRes{id: id}, owner: "ns/pod", lastUse: time.Now()}
	return p
}

// TestReleaseDoesNotHoldTheLockAcrossTheFactoryCall is the regression this file
// exists for.
//
// ReleaseInValid is a platform write with exponential backoff followed by a
// metadata poll; a single hiccup makes it run for tens of seconds. Release used
// to hold p.mutex for all of it, and p.mutex is what every Allocate takes, so
// one slow dealloc stalled every pod starting on the node until the CNI ADD
// budget ran out -- with nothing in the log but an ordinary dealloc retry.
func TestReleaseDoesNotHoldTheLockAcrossTheFactoryCall(t *testing.T) {
	f := &slowReleaseFactory{
		entered:  make(chan struct{}),
		proceed:  make(chan struct{}),
		validErr: errors.New("invalid"),
	}
	p := releasePool(t, "eni-1/10.0.0.42", f)

	done := make(chan error, 1)
	go func() { done <- p.Release("eni-1/10.0.0.42") }()

	// Wait for the factory call rather than for a duration: a sleep here would
	// make the test pass on a slow machine for the wrong reason.
	select {
	case <-f.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("ReleaseInValid was never reached")
	}

	// The lock has to be free while the factory call is in flight. Taking it
	// from this goroutine is the whole assertion; before the fix this blocked
	// until ReleaseInValid returned.
	locked := make(chan struct{})
	go func() {
		p.lock()
		p.unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("p.mutex is held across the ReleaseInValid call; every Allocate on the node would be blocked behind it")
	}

	close(f.proceed)
	if err := <-done; err != nil {
		t.Fatalf("Release() = %v, want nil", err)
	}
}

// TestReleaseAlwaysPlacesTheResource pins the invariant the lock split
// introduces: while the lock is down the resource is in none of the three
// holders, so every outcome of the factory call has to put it back into exactly
// one of them. Losing it means the pool keeps counting a resource it can never
// hand out again.
func TestReleaseAlwaysPlacesTheResource(t *testing.T) {
	const id = "eni-1/10.0.0.42"

	tests := []struct {
		name          string
		ret           types.NetResource
		retErr        error
		wantAvailable int
		wantInvalid   bool
	}{
		{
			name: "released for good, nothing to hold",
		},
		{
			name:          "factory returned a replacement",
			ret:           &fakeRes{id: "eni-1/10.0.0.43"},
			wantAvailable: 1,
		},
		{
			name:        "release failed, held as invalid for checkInvalid to retry",
			retErr:      errors.New("platform said no"),
			wantInvalid: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &slowReleaseFactory{
				entered:  make(chan struct{}),
				proceed:  make(chan struct{}),
				ret:      tt.ret,
				retErr:   tt.retErr,
				validErr: errors.New("invalid"),
			}
			close(f.proceed) // no need to block for this one
			p := releasePool(t, id, f)
			p.metricTotal.Set(float64(p.capWithLocked()))

			if err := p.Release(id); err != nil {
				t.Fatalf("Release() = %v, want nil", err)
			}

			if _, stillInUse := p.inUse[id]; stillInUse {
				t.Error("resource is still in inUse after Release")
			}
			if _, held := p.invalid[id]; held != tt.wantInvalid {
				t.Errorf("held as invalid = %v, want %v", held, tt.wantInvalid)
			}
			if got := p.available.Size(); got != tt.wantAvailable {
				t.Errorf("available holds %d, want %d", got, tt.wantAvailable)
			}

			// GC sets metricTotal from capWithLocked, so that equality is the
			// invariant the incremental Inc/Dec calls have to preserve. A drift
			// here shows up much later, as a node advertising a capacity it does
			// not have.
			if got, want := testutil.ToFloat64(p.metricTotal), float64(p.capWithLocked()); got != want {
				t.Errorf("metricTotal = %v, want capWithLocked() = %v", got, want)
			}
			if got, want := testutil.ToFloat64(p.metricAvailable), float64(p.available.Size()); got != want {
				t.Errorf("metricAvailable = %v, want available.Size() = %v", got, want)
			}
		})
	}
}

// TestReleaseTotalAccountingWhenTheReplacementIsAlreadyHeld covers the one
// branch the table above cannot reach: addAvailableLocked refusing the
// replacement because another holder already has that id. The resource Release
// took out of inUse is gone and the replacement was not added, so the pool is
// counting one fewer than before -- which is what the metricTotal.Dec() there is
// for.
func TestReleaseTotalAccountingWhenTheReplacementIsAlreadyHeld(t *testing.T) {
	const (
		id     = "eni-1/10.0.0.42"
		dupeID = "eni-1/10.0.0.43"
	)
	f := &slowReleaseFactory{
		entered:  make(chan struct{}),
		proceed:  make(chan struct{}),
		ret:      &fakeRes{id: dupeID},
		validErr: errors.New("invalid"),
	}
	close(f.proceed)
	p := releasePool(t, id, f)
	// Someone else already holds the id ReleaseInValid is about to hand back.
	p.inUse[dupeID] = poolItem{res: &fakeRes{id: dupeID}, owner: "ns/other", lastUse: time.Now()}
	p.metricTotal.Set(float64(p.capWithLocked()))
	before := testutil.ToFloat64(p.metricTotal)

	if err := p.Release(id); err != nil {
		t.Fatalf("Release() = %v, want nil", err)
	}

	if got := p.available.Size(); got != 0 {
		t.Errorf("available holds %d, want 0 -- the replacement is already in use elsewhere", got)
	}
	if after := testutil.ToFloat64(p.metricTotal); after != before-1 {
		t.Errorf("metricTotal = %v, want %v (one fewer than before)", after, before-1)
	}
	if got, want := testutil.ToFloat64(p.metricTotal), float64(p.capWithLocked()); got != want {
		t.Errorf("metricTotal = %v, want capWithLocked() = %v", got, want)
	}
}

// TestReleaseHoldsOffGCAcrossTheFactoryCall pins the second half of the lock
// split, and it is the one that is easy to get wrong.
//
// Dropping the mutex is what keeps Allocate moving, but it also lets GC in --
// and GC replaces p.available and p.invalid wholesale from factory.List(). In
// that window the resource has left inUse while the factory still reports it, so
// a GC landing there would push it into available and hand it to the next pod
// while ReleaseInValid is unassigning that same address from the interface.
// worker() wraps checkInvalid in blockPause for exactly this reason; Release has
// to hold it itself.
func TestReleaseHoldsOffGCAcrossTheFactoryCall(t *testing.T) {
	const id = "eni-1/10.0.0.42"
	f := &slowReleaseFactory{
		entered:  make(chan struct{}),
		proceed:  make(chan struct{}),
		validErr: errors.New("invalid"),
	}
	p := releasePool(t, id, f)

	done := make(chan error, 1)
	go func() { done <- p.Release(id) }()
	<-f.entered

	// The mutex has to be free -- that is the whole point of the split.
	locked := make(chan struct{})
	go func() {
		p.lock()
		p.unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(5 * time.Second):
		t.Fatal("p.mutex is held across the ReleaseInValid call")
	}

	// pauseLock must not be, so a GC starting now waits instead of rebuilding
	// the pool around a resource that is mid-release.
	paused := make(chan struct{})
	go func() {
		p.pause()
		p.unpause()
		close(paused)
	}()
	select {
	case <-paused:
		t.Fatal("GC could pause the pool while ReleaseInValid was in flight; it would rebuild available from the factory listing and hand out an address that is being unassigned")
	case <-time.After(300 * time.Millisecond):
	}

	close(f.proceed)
	if err := <-done; err != nil {
		t.Fatalf("Release() = %v, want nil", err)
	}

	// Once Release is done the pause must go through, or the worker is wedged.
	select {
	case <-paused:
	case <-time.After(5 * time.Second):
		t.Fatal("GC still cannot pause the pool after Release returned")
	}

	if f.callCount() != 1 {
		t.Errorf("ReleaseInValid called %d times, want 1", f.callCount())
	}
}
