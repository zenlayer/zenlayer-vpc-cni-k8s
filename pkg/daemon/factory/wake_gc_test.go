// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"sync"
	"testing"
	"time"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/signal"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// forceRefresh runs an update regardless of how recently the last one ran.
// update skips anything younger than a minute, which is right in production and
// would make every call after the first a no-op here.
func forceRefresh(t *testing.T, m *defaultInstanceLimit) {
	t.Helper()
	m.lock.Lock()
	m.lastUpdate = time.Time{}
	m.lock.Unlock()
	if err := m.update(); err != nil {
		t.Fatalf("update: %v", err)
	}
}

// wakeAPI drives a refresh with a controllable attached-interface count.
type wakeAPI struct {
	ZecAPI
	attached int // non-primary managed interfaces
	total    int // everything attached, the instance's own interface included
}

func (w *wakeAPI) GetInstanceLimit() (*InstanceLimits, error) {
	return &InstanceLimits{InstanceLimitsAttr: InstanceLimitsAttr{ENIQuota: 4, IPv4MaxPerENI: 30}}, nil
}

func (w *wakeAPI) GetAttachedENIs() ([]*zecTypes.ENI, error) {
	out := []*zecTypes.ENI{{ID: "primary", IsPrimary: true}}
	for i := 0; i < w.attached; i++ {
		out = append(out, &zecTypes.ENI{ID: "eni"})
	}
	return out, nil
}

func (w *wakeAPI) GetTotalAttachedEniCnt() (int, error) { return w.total, nil }

// The signal package has no unregister, and RegisterChannel refuses a second
// channel for the same signal, so the channel is registered once for the whole
// package and every test drains it rather than making its own.
var (
	wakeCh   = make(chan signal.SigData, 8)
	wakeOnce sync.Once
)

// drainWakeGC returns a func reporting whether a WakeGC signal arrived since
// the last call, and leaves the channel empty and unmuted for the next test.
func drainWakeGC(t *testing.T) func() bool {
	t.Helper()
	wakeOnce.Do(func() {
		if err := signal.RegisterChannel(signal.WakeGC, wakeCh); err != nil {
			t.Fatalf("register WakeGC: %v", err)
		}
	})
	drain := func() bool {
		got := false
		for {
			select {
			case <-wakeCh:
				got = true
			default:
				return got
			}
		}
	}
	signal.UnmuteChannel(signal.WakeGC)
	drain()
	t.Cleanup(func() {
		signal.UnmuteChannel(signal.WakeGC)
		drain()
	})
	return drain
}

// TestUpdateWakesGCOnlyWhenManagedCountDrops pins the condition.
//
// A drop is the signal that an interface left the instance while this daemon
// still holds it: GC is what removes it and stops its worker, and until that
// happens submitOrder keeps choosing an interface with no NIC behind it.
//
// It must be a drop and nothing else. GC pauses the whole pool for its
// duration, so waking it on an unchanged or growing count would trade a real
// pause for no information -- and growing is the ordinary case of this daemon
// attaching an interface it just created.
func TestUpdateWakesGCOnlyWhenManagedCountDrops(t *testing.T) {
	woke := drainWakeGC(t)

	api := &wakeAPI{attached: 2, total: 3}
	m := &defaultInstanceLimit{lock: sync.RWMutex{}, api: api, eventWatchers: []chan<- struct{}{}}

	// First pass establishes the baseline; nothing to compare against yet.
	forceRefresh(t, m)
	woke()

	for _, tc := range []struct {
		name     string
		attached int
		total    int
		want     bool
	}{
		{"unchanged", 2, 3, false},
		{"one interface gone", 1, 2, true},
		{"unchanged again", 1, 2, false},
		{"an interface attached", 2, 3, false},
		{"all gone at once", 0, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			api.attached, api.total = tc.attached, tc.total
			forceRefresh(t, m)
			if got := woke(); got != tc.want {
				t.Errorf("WakeGC signaled = %v, want %v (ENIManaged now %d)",
					got, tc.want, m.limit.ENIManaged)
			}
		})
	}
}

// TestWakeGCSurvivesBeingMuted covers the case the retry exists for.
//
// GC mutes this signal while it runs. A notification sent in that window is
// discarded, and by the next poll the managed count matches again -- the drop
// that justified the wake is no longer visible anywhere. Without holding the
// intent, the interface would stay in the factory until the next reconcile
// tick, a whole reconcileIntervalSec away.
func TestWakeGCSurvivesBeingMuted(t *testing.T) {
	woke := drainWakeGC(t)

	api := &wakeAPI{attached: 2, total: 3}
	m := &defaultInstanceLimit{lock: sync.RWMutex{}, api: api, eventWatchers: []chan<- struct{}{}}
	forceRefresh(t, m)
	woke()

	// A GC is running: the drop is seen but the notification goes nowhere.
	signal.MuteChannel(signal.WakeGC)
	api.attached, api.total = 1, 2
	forceRefresh(t, m)
	if woke() {
		t.Fatal("a muted signal reached the channel")
	}
	if !m.wakeGCPending {
		t.Fatal("the wake was dropped instead of being held for retry")
	}

	// GC is done. The counts now match, so nothing rediscovers the drop --
	// only the held intent can still deliver it.
	signal.UnmuteChannel(signal.WakeGC)
	forceRefresh(t, m)
	if !woke() {
		t.Error("GC was never woken after the mute lifted")
	}
	if m.wakeGCPending {
		t.Error("wakeGCPending stayed set after a delivered notification")
	}

	// Delivered once, not on every subsequent poll.
	forceRefresh(t, m)
	if woke() {
		t.Error("GC was woken again with nothing to report")
	}
}
