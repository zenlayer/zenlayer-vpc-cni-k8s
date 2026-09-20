// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Every tier the instance specification actually publishes. Both documented
// tables (z4a/z4ah and z2a/z2i/z3a/z4i) carry these same numbers.
func TestResolveFromSpecTableMatchesPublishedTiers(t *testing.T) {
	tests := []struct {
		vcpu         int
		wantVNICs    int
		wantIPPerENI int
	}{
		{1, 2, 6},
		{2, 2, 6},
		{4, 2, 15},
		{7, 2, 15},
		{8, 2, 15},
		{16, 4, 30},
		{32, 8, 30},
		{64, 16, 30},
	}
	for _, tt := range tests {
		got, err := resolveFromSpecTable(context.Background(), "z4a.large", tt.vcpu)
		if err != nil {
			t.Fatalf("%d vCPU: %v", tt.vcpu, err)
		}
		if got.ENIQuota != tt.wantVNICs {
			t.Errorf("%d vCPU: ENIQuota = %d, want %d", tt.vcpu, got.ENIQuota, tt.wantVNICs)
		}
		if got.IPv4MaxPerENI != tt.wantIPPerENI {
			t.Errorf("%d vCPU: IPv4MaxPerENI = %d, want %d", tt.vcpu, got.IPv4MaxPerENI, tt.wantIPPerENI)
		}
	}
}

// A vCPU count between two published tiers must round DOWN. Overstating a
// limit makes the daemon claim capacity the platform refuses.
func TestResolveFromSpecTableRoundsDown(t *testing.T) {
	tests := []struct {
		vcpu         int
		wantVNICs    int
		wantIPPerENI int
	}{
		{3, 2, 6},     // between the 2 and 7 tiers
		{12, 2, 15},   // between 8 and 16
		{24, 4, 30},   // between 16 and 32
		{48, 8, 30},   // between 32 and 64
		{128, 16, 30}, // above the largest published tier
	}
	for _, tt := range tests {
		got, err := resolveFromSpecTable(context.Background(), "z2a.xlarge", tt.vcpu)
		if err != nil {
			t.Fatalf("%d vCPU: %v", tt.vcpu, err)
		}
		if got.ENIQuota != tt.wantVNICs || got.IPv4MaxPerENI != tt.wantIPPerENI {
			t.Errorf("%d vCPU: got {%d vNIC, %d IP}, want {%d, %d}",
				tt.vcpu, got.ENIQuota, got.IPv4MaxPerENI, tt.wantVNICs, tt.wantIPPerENI)
		}
	}
}

func TestResolveFromSpecTableRejectsBadVCPU(t *testing.T) {
	for _, vcpu := range []int{0, -1} {
		if _, err := resolveFromSpecTable(context.Background(), "z2a.small", vcpu); err == nil {
			t.Errorf("vcpu=%d: want an error, got nil", vcpu)
		}
	}
}

func TestGetLimitCaches(t *testing.T) {
	var calls int32
	p := newLimitProvider(func(context.Context, string, int) (InstanceLimitsAttr, error) {
		atomic.AddInt32(&calls, 1)
		return InstanceLimitsAttr{ENIQuota: 4, IPv4MaxPerENI: 30}, nil
	}, time.Hour)

	for i := 0; i < 5; i++ {
		got, err := p.GetLimit(context.Background(), "z4a.large", 16)
		if err != nil {
			t.Fatal(err)
		}
		if got.ENIQuota != 4 {
			t.Fatalf("ENIQuota = %d, want 4", got.ENIQuota)
		}
	}
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("resolve called %d times, want 1 (the rest should hit the cache)", n)
	}

	// A different key must not be served from the first entry.
	if _, err := p.GetLimit(context.Background(), "z4a.large", 32); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("resolve called %d times after a second key, want 2", n)
	}
}

func TestGetLimitExpiresAfterTTL(t *testing.T) {
	var calls int32
	p := newLimitProvider(func(context.Context, string, int) (InstanceLimitsAttr, error) {
		atomic.AddInt32(&calls, 1)
		return InstanceLimitsAttr{ENIQuota: 2}, nil
	}, 50*time.Millisecond)

	if _, err := p.GetLimit(context.Background(), "z2a.small", 2); err != nil {
		t.Fatal(err)
	}
	time.Sleep(120 * time.Millisecond)
	if _, err := p.GetLimit(context.Background(), "z2a.small", 2); err != nil {
		t.Fatal(err)
	}
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("resolve called %d times, want 2 (the entry should have expired)", n)
	}
}

// Concurrent callers for the same key collapse into one resolve.
func TestGetLimitSingleflight(t *testing.T) {
	var calls int32
	release := make(chan struct{})
	p := newLimitProvider(func(context.Context, string, int) (InstanceLimitsAttr, error) {
		atomic.AddInt32(&calls, 1)
		<-release // hold the flight open so the others pile up behind it
		return InstanceLimitsAttr{ENIQuota: 8}, nil
	}, time.Hour)

	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := p.GetLimit(context.Background(), "z4a.8xlarge", 32); err != nil {
				t.Error(err)
			}
		}()
	}
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("resolve called %d times for 10 concurrent callers, want 1", n)
	}
}

// A failed resolve must not be cached, and must not be reported as success.
func TestGetLimitDoesNotCacheFailures(t *testing.T) {
	var calls int32
	wantErr := errors.New("api unavailable")
	p := newLimitProvider(func(context.Context, string, int) (InstanceLimitsAttr, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return InstanceLimitsAttr{}, wantErr
		}
		return InstanceLimitsAttr{ENIQuota: 2}, nil
	}, time.Hour)

	if _, err := p.GetLimit(context.Background(), "z2a.small", 1); !errors.Is(err, wantErr) {
		t.Fatalf("first call err = %v, want %v", err, wantErr)
	}
	got, err := p.GetLimit(context.Background(), "z2a.small", 1)
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if got.ENIQuota != 2 {
		t.Errorf("ENIQuota = %d, want 2 (the retry should have been attempted)", got.ENIQuota)
	}
}
