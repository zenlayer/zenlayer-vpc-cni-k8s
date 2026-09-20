// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package backoff

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

func TestBackOff(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want wait.Backoff
	}{
		{
			name: "default key",
			key:  DefaultKey,
			want: wait.Backoff{Duration: 2 * time.Second, Factor: 1.5, Jitter: 0.3, Steps: 6},
		},
		{
			name: "api write ops",
			key:  APIWriteOps,
			want: wait.Backoff{Duration: 4 * time.Second, Factor: 1.5, Jitter: 0.5, Steps: 6},
		},
		{
			name: "api status wait",
			key:  APIStatusWait,
			want: wait.Backoff{Duration: 5 * time.Second, Factor: 1.5, Jitter: 0.5, Steps: 8},
		},
		{
			name: "api fast retry",
			key:  APIFastRetry,
			want: wait.Backoff{Duration: 500 * time.Millisecond, Factor: 1.7, Jitter: 0.3, Steps: 6},
		},
		{
			name: "wait crd status",
			key:  WaitCRDStatus,
			want: wait.Backoff{Duration: 5 * time.Second, Factor: 2, Jitter: 0.3, Steps: 3},
		},
		{
			name: "meta status wait",
			key:  MetaStatusWait,
			want: wait.Backoff{Duration: 1100 * time.Millisecond, Factor: 1, Jitter: 0.2, Steps: 10},
		},
		{
			// An unknown key must not hand back a zero Backoff: Steps==0 would
			// make wait.ExponentialBackoff give up without ever calling the
			// condition.
			name: "unknown key falls back to default",
			key:  "no_such_key",
			want: wait.Backoff{Duration: 2 * time.Second, Factor: 1.5, Jitter: 0.3, Steps: 6},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BackOff(tt.key)
			if got != tt.want {
				t.Errorf("BackOff(%q) = %+v, want %+v", tt.key, got, tt.want)
			}
		})
	}
}

// TestBackOffReturnsIndependentCopy checks that each caller gets its own copy.
// wait.Backoff.Step() mutates the receiver, so a shared one would let a single
// retry loop exhaust the Steps budget of every later caller.
func TestBackOffReturnsIndependentCopy(t *testing.T) {
	for _, key := range []string{DefaultKey, APIWriteOps, APIStatusWait, APIFastRetry, WaitCRDStatus, MetaStatusWait} {
		t.Run(key, func(t *testing.T) {
			first := BackOff(key)
			b := BackOff(key)
			for b.Steps > 0 {
				b.Step()
			}
			if again := BackOff(key); again != first {
				t.Errorf("BackOff(%q) mutated by a previous caller: got %+v, want %+v", key, again, first)
			}
		})
	}
}

// TestBackoffMapEntriesAreSane checks the invariants every entry must satisfy,
// so a new backoffMap entry cannot silently break the retry helpers.
func TestBackoffMapEntriesAreSane(t *testing.T) {
	for key, b := range backoffMap {
		name := key
		if name == DefaultKey {
			name = "<default>"
		}
		t.Run(name, func(t *testing.T) {
			if b.Steps <= 0 {
				t.Errorf("Steps = %d, want > 0", b.Steps)
			}
			if b.Duration <= 0 {
				t.Errorf("Duration = %v, want > 0", b.Duration)
			}
			if b.Factor < 1 {
				t.Errorf("Factor = %v, want >= 1 (a shrinking backoff retries faster over time)", b.Factor)
			}
			if b.Jitter < 0 || b.Jitter >= 1 {
				t.Errorf("Jitter = %v, want in [0, 1)", b.Jitter)
			}
		})
	}
}

func TestDefaultKeyEntryExists(t *testing.T) {
	if _, ok := backoffMap[DefaultKey]; !ok {
		t.Fatal("backoffMap has no DefaultKey entry; BackOff would return a zero Backoff for unknown keys")
	}
}
