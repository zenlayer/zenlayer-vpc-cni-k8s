// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestJSONStr(t *testing.T) {
	type nested struct {
		Enabled bool `json:"enabled"`
	}
	type payload struct {
		Name    string   `json:"name"`
		Count   int      `json:"count"`
		Tags    []string `json:"tags"`
		Skipped string   `json:"-"`
		Inner   nested   `json:"inner"`
	}

	tests := []struct {
		name  string
		input interface{}
		want  string
	}{
		{name: "nil", input: nil, want: "null"},
		{name: "string", input: "hello", want: `"hello"`},
		{name: "empty string", input: "", want: `""`},
		{name: "int", input: 42, want: "42"},
		{name: "bool", input: true, want: "true"},
		{name: "nil slice", input: []string(nil), want: "null"},
		{name: "empty slice", input: []string{}, want: "[]"},
		{name: "slice", input: []int{1, 2, 3}, want: "[1,2,3]"},
		{name: "nil map", input: map[string]string(nil), want: "null"},
		{name: "map keys are sorted", input: map[string]int{"b": 2, "a": 1}, want: `{"a":1,"b":2}`},
		{
			name:  "struct honors json tags",
			input: payload{Name: "eni", Count: 2, Tags: []string{"x"}, Skipped: "secret", Inner: nested{Enabled: true}},
			want:  `{"name":"eni","count":2,"tags":["x"],"inner":{"enabled":true}}`,
		},
		{
			name:  "pointer to struct",
			input: &nested{Enabled: false},
			want:  `{"enabled":false}`,
		},
		{name: "typed nil pointer", input: (*nested)(nil), want: "null"},
		{name: "unmarshalable channel yields empty string", input: make(chan int), want: ""},
		{name: "unmarshalable func yields empty string", input: func() {}, want: ""},
		{name: "NaN yields empty string", input: math.NaN(), want: ""},
		{name: "value containing an unmarshalable field", input: struct {
			C chan int `json:"c"`
		}{}, want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := JSONStr(tt.input); got != tt.want {
				t.Errorf("JSONStr(%#v) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestGrabFileLock(t *testing.T) {
	t.Run("creates the lock file and acquires the lock", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cni.lock")

		locker, err := GrabFileLock(path)
		if err != nil {
			t.Fatalf("GrabFileLock(%q) failed: %v", path, err)
		}
		if locker == nil {
			t.Fatal("GrabFileLock returned a nil locker without an error")
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("lock file was not created: %v", err)
		}
		if err := locker.Close(); err != nil {
			t.Errorf("Close() failed: %v", err)
		}
	})

	t.Run("lock can be re-acquired after release", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cni.lock")

		first, err := GrabFileLock(path)
		if err != nil {
			t.Fatalf("first GrabFileLock failed: %v", err)
		}
		if err := first.Close(); err != nil {
			t.Fatalf("first Close failed: %v", err)
		}

		second, err := GrabFileLock(path)
		if err != nil {
			t.Fatalf("second GrabFileLock failed: %v", err)
		}
		if err := second.Close(); err != nil {
			t.Errorf("second Close failed: %v", err)
		}
	})

	t.Run("reuses an existing lock file", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cni.lock")
		if err := os.WriteFile(path, []byte("stale"), 0600); err != nil {
			t.Fatalf("failed to pre-create the lock file: %v", err)
		}

		locker, err := GrabFileLock(path)
		if err != nil {
			t.Fatalf("GrabFileLock on an existing file failed: %v", err)
		}
		defer func() {
			if err := locker.Close(); err != nil {
				t.Errorf("Close() failed: %v", err)
			}
		}()

		// The lock file must not be truncated: it is only used as a flock target.
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("failed to read the lock file: %v", err)
		}
		if string(data) != "stale" {
			t.Errorf("lock file content = %q, want %q", string(data), "stale")
		}
	})

	// The two subtests below are the only ones that fail against an
	// implementation that takes no lock at all. flock is held per open file
	// description, not per process, so a second GrabFileLock in this same
	// process contends exactly as a second zec-cni process would.
	t.Run("a second acquisition blocks until the first is released", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "cni.lock")

		first, err := GrabFileLock(path)
		if err != nil {
			t.Fatalf("first GrabFileLock failed: %v", err)
		}

		var released atomic.Bool
		started := make(chan struct{})
		second := make(chan error, 1)
		go func() {
			close(started)
			l, err := GrabFileLock(path)
			if err == nil && !released.Load() {
				err = errors.New("acquired the lock while the first holder still had it")
			}
			if l != nil {
				_ = l.Close()
			}
			second <- err
		}()

		// Without this the next select would pass on a contender that never got
		// scheduled, which is the same observation as one that is correctly
		// waiting.
		<-started

		// Long enough for several of the 200ms retries to run and fail.
		select {
		case err := <-second:
			t.Fatalf("second GrabFileLock returned early (%v), want it to keep waiting", err)
		case <-time.After(time.Second):
		}

		released.Store(true)
		if err := first.Close(); err != nil {
			t.Fatalf("releasing the first lock failed: %v", err)
		}

		select {
		case err := <-second:
			if err != nil {
				t.Fatalf("second GrabFileLock after release: %v", err)
			}
		case <-time.After(fileLockTimeOut):
			t.Fatal("second GrabFileLock never acquired the lock after it was released")
		}
	})

	t.Run("gives up once the timeout expires", func(t *testing.T) {
		defer withFileLockTimeout(t, 600*time.Millisecond)()
		path := filepath.Join(t.TempDir(), "cni.lock")

		first, err := GrabFileLock(path)
		if err != nil {
			t.Fatalf("first GrabFileLock failed: %v", err)
		}
		defer func() { _ = first.Close() }()

		start := time.Now()
		second, err := GrabFileLock(path)
		elapsed := time.Since(start)
		if err == nil {
			_ = second.Close()
			t.Fatal("GrabFileLock succeeded while the lock was held elsewhere, want a timeout")
		}
		if second != nil {
			t.Errorf("GrabFileLock returned a locker %v alongside an error, want nil", second)
		}
		// Distinguishes "waited and gave up" from "failed for some other reason":
		// only the former is the bound this test is here to pin.
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("error = %v, want it to wrap context.DeadlineExceeded", err)
		}
		if elapsed < fileLockTimeOut {
			t.Errorf("gave up after %v, want it to wait at least %v", elapsed, fileLockTimeOut)
		}
	})

	t.Run("missing parent directory is an error", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "no-such-dir", "cni.lock")

		locker, err := GrabFileLock(path)
		if err == nil {
			_ = locker.Close()
			t.Fatalf("GrabFileLock(%q) succeeded, want an error", path)
		}
		if locker != nil {
			t.Errorf("GrabFileLock returned a locker %v alongside an error, want nil", locker)
		}
	})
}

func TestLockerClose(t *testing.T) {
	t.Run("zero value locker is safe to close", func(t *testing.T) {
		var l Locker
		if err := l.Close(); err != nil {
			t.Errorf("Close() on a zero-value Locker = %v, want nil", err)
		}
	})
}

// withFileLockTimeout shortens the contended-lock timeout for one subtest and
// returns the restore func. Not safe under t.Parallel -- the timeout is package
// scoped, which is the price of being able to reach the give-up path at all.
func withFileLockTimeout(t *testing.T, d time.Duration) func() {
	t.Helper()
	prev := fileLockTimeOut
	fileLockTimeOut = d
	return func() { fileLockTimeOut = prev }
}
