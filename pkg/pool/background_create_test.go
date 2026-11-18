// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package pool

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// createCountingFactory records Create calls; with CreateInBackground the
// allocation path must make none.
type createCountingFactory struct {
	slowReleaseFactory
	creates atomic.Int32
}

func (f *createCountingFactory) Create(int) ([]types.NetResource, error) {
	f.creates.Add(1)
	return nil, nil
}

func backgroundPool(f ObjectFactory) *poolImpl {
	p := newTestPool()
	p.factory = f
	p.createInBackground = true
	p.maxCap = 10
	p.target = 2
	p.ticket = make(chan struct{}, 10)
	p.scale = make(chan struct{}, 1)
	p.metricsMaxCap = prometheus.NewGauge(prometheus.GaugeOpts{Name: "test_max_cap"})
	return p
}

// TestBackgroundAllocateWaitsWithoutPauseLock: an Allocate on an empty pool
// must neither call Create nor hold pauseLock while it waits, so a GC can pause
// the pool meanwhile and a Release is not stuck behind it. Holding it is what
// froze every pod deletion on a crd node for minutes while Creates waited on
// the controlplane.
func TestBackgroundAllocateWaitsWithoutPauseLock(t *testing.T) {
	f := &createCountingFactory{}
	p := backgroundPool(f)

	got := make(chan types.NetResource, 1)
	go func() {
		res, err := p.Allocate(context.Background(), "", "ns/pod")
		if err != nil {
			t.Errorf("Allocate: %v", err)
		}
		got <- res
	}()

	// The waiter registers as demand and asks the worker to scale.
	deadline := time.Now().Add(2 * time.Second)
	for {
		p.lock()
		w := p.waiting
		p.unlock()
		if w == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Allocate never registered as waiting")
		}
		time.Sleep(5 * time.Millisecond)
	}
	select {
	case <-p.scale:
	default:
		t.Error("waiting Allocate did not notify the worker")
	}
	if n := p.shouldIncrease(); n != 3 {
		t.Errorf("shouldIncrease = %d, want target 2 + 1 waiting", n)
	}

	// A GC's pause must not block on the waiter.
	paused := make(chan struct{})
	go func() {
		p.pause()
		p.unpause()
		close(paused)
	}()
	select {
	case <-paused:
	case <-time.After(time.Second):
		t.Fatal("pause blocked behind a waiting Allocate")
	}

	p.AddAvailable(&fakeRes{id: "eni-1/10.0.0.9"})
	select {
	case res := <-got:
		if res == nil || res.GetID() != "eni-1/10.0.0.9" {
			t.Fatalf("Allocate got %v", res)
		}
	case <-time.After(time.Second):
		t.Fatal("Allocate was not woken by AddAvailable")
	}
	if n := f.creates.Load(); n != 0 {
		t.Errorf("Create called %d times on the allocation path", n)
	}
	p.lock()
	defer p.unlock()
	if p.waiting != 0 {
		t.Errorf("waiting = %d after Allocate returned", p.waiting)
	}
}

// TestBackgroundAllocateHonoursContext: a waiter whose deadline passes returns
// ErrContextDone and stops counting as demand.
func TestBackgroundAllocateHonoursContext(t *testing.T) {
	p := backgroundPool(&createCountingFactory{})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if _, err := p.Allocate(ctx, "", "ns/pod"); err != ErrContextDone {
		t.Fatalf("Allocate err = %v, want ErrContextDone", err)
	}
	p.lock()
	defer p.unlock()
	if p.waiting != 0 {
		t.Errorf("waiting = %d after a canceled Allocate", p.waiting)
	}
}

// blockingCreateFactory blocks in Create until released, standing in for a
// crd Create waiting on the controlplane.
type blockingCreateFactory struct {
	slowReleaseFactory
	entered chan struct{}
	proceed chan struct{}
	once    sync.Once
}

// The worker runs a cycle at start as well as on notifyScale, so Create can be
// entered more than once.
func (f *blockingCreateFactory) Create(int) ([]types.NetResource, error) {
	f.once.Do(func() { close(f.entered) })
	<-f.proceed
	return nil, nil
}

// TestBackgroundWorkerCreatesOutsidePauseLock: the worker's own Create must
// not hold pauseLock either, or one minute-long wait still queues a GC and
// every Release behind it.
func TestBackgroundWorkerCreatesOutsidePauseLock(t *testing.T) {
	f := &blockingCreateFactory{entered: make(chan struct{}), proceed: make(chan struct{})}
	p := backgroundPool(f)
	p.monitorInterval = time.Hour
	for range 10 {
		p.productTicket()
	}
	go p.worker()
	p.notifyScale()
	select {
	case <-f.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("worker never called Create")
	}
	paused := make(chan struct{})
	go func() {
		p.pause()
		p.unpause()
		close(paused)
	}()
	select {
	case <-paused:
	case <-time.After(time.Second):
		t.Fatal("pause blocked behind the worker's Create")
	}
	close(f.proceed)
}

// limitFactory has a resource limit the test moves, as status.capacity does,
// and records what Create was asked for.
type limitFactory struct {
	slowReleaseFactory
	limit  int
	asked  []int
	nextID int
}

func (f *limitFactory) GetResourceLimit() int { return f.limit }

func (f *limitFactory) Create(n int) ([]types.NetResource, error) {
	f.asked = append(f.asked, n)
	out := make([]types.NetResource, 0, n)
	for range n {
		f.nextID++
		out = append(out, &fakeRes{id: fmt.Sprintf("eni-1/10.0.1.%d", f.nextID)})
	}
	return out, nil
}

// Measured on a crd node: an interface attached by hand took the cap from 28
// to 14 and a GC sized the tickets to that; when it was removed the cap went
// back to 28, but no ticket came with it, and the pool logged "Try create 0
// resource" with pods in ContainerCreating until the next GC.
func TestTicketsFollowGrowingMaxCap(t *testing.T) {
	f := &limitFactory{limit: 14}
	p := backgroundPool(f)
	p.maxCapProbe = true
	p.ticket = make(chan struct{}, maxTickets)
	for i := range 14 {
		inUse(p, &fakeRes{id: fmt.Sprintf("eni-1/10.0.0.%d", i)})
	}
	p.lock()
	p.resetTicketWithLocked() // at the cap: no tickets
	p.waiting = 3
	p.unlock()

	f.limit = 28
	p.tryIncreasePool()
	if len(f.asked) != 1 || f.asked[0] != 5 {
		t.Fatalf("Create asked for %v, want [5] (target 2 + 3 waiting)", f.asked)
	}
}
