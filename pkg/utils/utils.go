// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	filemutex "github.com/alexflint/go-filemutex"
	"k8s.io/apimachinery/pkg/util/wait"
)

// fileLockTimeOut bounds how long GrabFileLock waits for a contended lock. It
// sits just under the container runtime's own CNI timeout, so a caller that
// gives up here still has a chance to report a useful error instead of being
// killed mid-operation.
//
// A var rather than a const so tests can shrink it: the timeout path is only
// reachable by holding the lock until it expires, and eleven seconds per
// assertion is too slow to keep in the suite.
var fileLockTimeOut = 11 * time.Second

// JSONStr marshals v to JSON, returning "" if that fails.
func JSONStr(v interface{}) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}

type Locker struct {
	m *filemutex.FileMutex
}

// Close releases the flock and closes the descriptor that filemutex.New opened.
//
// It calls the mutex's Close, not its Unlock: Unlock is flock(LOCK_UN) alone
// and leaves that descriptor open, one per acquisition.
func (l *Locker) Close() error {
	if l.m != nil {
		return l.m.Close()
	}
	return nil
}

// GrabFileLock takes an exclusive flock on lockfilePath, creating the file if
// it does not exist. It retries every 200ms and gives up after fileLockTimeOut.
// The returned Locker must be closed.
func GrabFileLock(lockfilePath string) (*Locker, error) {
	var m, err = filemutex.New(lockfilePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open lock %s: %v", lockfilePath, err)
	}

	// TryLock, not Lock: filemutex.Lock is flock(LOCK_EX) with no LOCK_NB, so
	// it parks inside the syscall and the poll timeout below never gets a
	// chance to fire -- the fileLockTimeOut bound would be unenforceable.
	err = wait.PollUntilContextTimeout(context.Background(), 200*time.Millisecond, fileLockTimeOut, true, func(ctx context.Context) (bool, error) {
		switch lockErr := m.TryLock(); {
		case lockErr == nil:
			return true, nil
		case errors.Is(lockErr, filemutex.AlreadyLocked):
			return false, nil // held elsewhere, keep polling
		default:
			return false, lockErr // a real failure, stop now
		}
	})
	if err != nil {
		// The descriptor is ours even when the lock is not.
		_ = m.Close()
		return nil, fmt.Errorf("failed to acquire lock %s: %w", lockfilePath, err)
	}

	return &Locker{
		m: m,
	}, nil
}
