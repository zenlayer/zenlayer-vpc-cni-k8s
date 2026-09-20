// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"sync"
	"testing"
	"time"

	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// slowLimitAPI blocks inside the first platform call a refresh makes, standing
// in for the retry-with-backoff each of them does when the API is unwell.
type slowLimitAPI struct {
	ZecAPI
	entered chan struct{}
	proceed chan struct{}
	once    sync.Once
	enis    []*zecTypes.ENI
	total   int
}

func (s *slowLimitAPI) GetInstanceLimit() (*InstanceLimits, error) {
	s.once.Do(func() { close(s.entered) })
	<-s.proceed
	return &InstanceLimits{InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 4, IPv4MaxPerENI: 30}}, nil
}

func (s *slowLimitAPI) GetAttachedENIs() ([]*zecTypes.ENI, error) { return s.enis, nil }

func (s *slowLimitAPI) GetTotalAttachedEniCnt() (int, error) { return s.total, nil }

// newSlowLimit starts with nothing but the instance's own interface. A test that
// needs the refresh to observe secondaries appends to api.enis and raises
// api.total before releasing the gate.
func newSlowLimit() (*defaultInstanceLimit, *slowLimitAPI) {
	api := &slowLimitAPI{
		entered: make(chan struct{}),
		proceed: make(chan struct{}),
		enis:    []*zecTypes.ENI{{ID: "primary", IsPrimary: true}},
		total:   1,
	}
	return &defaultInstanceLimit{api: api, eventWatchers: []chan<- struct{}{}}, api
}

// TestGetLimitDoesNotWaitForARefresh is the regression this file exists for.
//
// The update used to hold the write lock across three platform calls, each with
// its own backoff. GetLimit takes the read side of that lock, and every
// call site of it sits inside f.Lock or an interface's own lock -- so a slow
// platform did not merely delay the refresh, it froze allocation on the node
// while holding the locks the rest of the factory needs.
func TestGetLimitDoesNotWaitForARefresh(t *testing.T) {
	m, api := newSlowLimit()

	done := make(chan error, 1)
	go func() { done <- m.update() }()

	select {
	case <-api.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh never reached the platform call")
	}

	read := make(chan InstanceLimits, 1)
	go func() { read <- m.GetLimit() }()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("GetLimit blocked while a refresh was in flight; its callers hold f.Lock, so every " +
			"allocation on the node would queue behind a slow platform call")
	}

	close(api.proceed)
	if err := <-done; err != nil {
		t.Fatalf("update() = %v, want nil", err)
	}
}

// TestCordonCreateDoesNotWaitForARefresh covers the other entry point. It runs
// on the pool worker inside CreateWithIPCount, so it is on the allocation path
// too -- and it used to take the write lock before refreshing, not after.
func TestCordonCreateDoesNotWaitForARefresh(t *testing.T) {
	m, api := newSlowLimit()

	done := make(chan struct{})
	go func() { m.CordonCreate("test"); close(done) }()

	select {
	case <-api.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("CordonCreate never reached the platform call")
	}

	read := make(chan InstanceLimits, 1)
	go func() { read <- m.GetLimit() }()
	select {
	case <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("GetLimit blocked while CordonCreate was refreshing")
	}

	close(api.proceed)
	<-done
	if !m.GetLimit().Cordon {
		t.Error("CordonCreate returned without setting Cordon")
	}
}

// TestStaleRefreshDoesNotReviveAReleasedInterface pins the cost of the two tests
// above: two refreshes overlap, and whichever finishes last writes its count --
// which is not the same as the newest count. Reviving a released interface is
// not merely stale bookkeeping: under cordon ENIAvailable is ENIManaged, so
// AllocatableIPs advertises a whole interface of addresses that are not there
// and the scheduler fills the node with pods that cannot get one. The extra
// attach the factory then believes it may make is refused by the platform.
func TestStaleRefreshDoesNotReviveAReleasedInterface(t *testing.T) {
	m, api := newSlowLimit()
	api.enis = append(api.enis, &zecTypes.ENI{ID: "eni-1"}, &zecTypes.ENI{ID: "eni-2"},
		&zecTypes.ENI{ID: "eni-3"})
	api.total = 4

	done := make(chan error, 1)
	go func() { done <- m.update() }()

	select {
	case <-api.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh never reached the platform call")
	}

	// The pool releases one while that refresh is in flight, and the refresh
	// that observes it gets there first.
	m.lock.Lock()
	m.updateLocked(InstanceLimitsAttr{ENIQuota: 4, IPv4MaxPerENI: 30}, 2, 3)
	m.lock.Unlock()

	close(api.proceed)
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("update: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the refresh never finished")
	}

	if got := m.GetLimit().ENIManaged; got != 2 {
		t.Errorf("managed interfaces = %d, want 2: the refresh that started before the release "+
			"finished after it and wrote its own older count back", got)
	}
}
