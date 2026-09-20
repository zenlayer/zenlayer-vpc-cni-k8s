// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package signal delivers named wake-ups between goroutines in the daemon.
//
// It exists so that an event with no natural caller -- an address release that
// should prompt a reconcile sooner than the next tick -- can nudge the component
// that cares without either side importing the other. Delivery is best-effort:
// a receiver that is not keeping up misses a wake-up rather than blocking the
// sender.
package signal

import (
	"fmt"
	"sync"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
)

type SigData uint32

var log = zlog.GetLogger().WithFields(zlog.Fields{"subSys": "signal"})

const (
	// WakeGC names the signal that asks the daemon's GC to run before its next
	// scheduled tick. It is the only signal in use.
	WakeGC = "wakeGC"
)

const (
	SigWakeGC = iota
)

var (
	// mu guards both maps. A racing read and write on a Go map is a fatal error,
	// not merely a data race: NotifySignal runs on the allocation path while GC
	// mutes and unmutes the same signal from another goroutine.
	mu             sync.RWMutex
	signalChannels map[string]chan<- SigData
	muteSignal     map[string]struct{}
)

// RegisterChannel registers the channel that receives a signal. Registering the
// same signal twice is an error; the first channel keeps it.
func RegisterChannel(signal string, ch chan<- SigData) error {
	mu.Lock()
	defer mu.Unlock()

	if _, exist := signalChannels[signal]; exist {
		return fmt.Errorf("register a exist signal")
	}
	signalChannels[signal] = ch
	return nil
}

// NotifySignal is a non-blocking notification. It reports whether the receiver
// will act on it.
//
// False means the signal is muted: the notification was discarded and nothing
// runs because of it. A caller notifying on an edge -- a state change it cannot
// observe again later -- has to retry, or the change is silently lost.
//
// A full channel reports true: a notification is already queued and the receiver
// has not got to it yet. So does having no registered channel, since retrying
// would never reach anyone.
func NotifySignal(signal string, data SigData) bool {
	// The send happens under the read lock on purpose: it keeps MuteChannel from
	// completing while a send is in flight, so a caller that mutes a signal and
	// then closes its channel cannot race with a send on it. The select is
	// non-blocking, so the lock is never held for long.
	mu.RLock()
	defer mu.RUnlock()

	if _, exist := muteSignal[signal]; exist {
		log.Infof("Signal %s muted", signal)
		return false
	}
	if ch, exist := signalChannels[signal]; exist {
		select {
		case ch <- data:
		default:
			log.Infof("Signal [%v %v] processing", signal, data)
		}
	}
	return true
}

// MuteChannel stops delivery of a signal: until it is unmuted, NotifySignal
// discards notifications for it and reports false.
func MuteChannel(signal string) {
	mu.Lock()
	defer mu.Unlock()

	muteSignal[signal] = struct{}{}
}

// UnmuteChannel resumes delivery of a signal muted by MuteChannel. Anything
// notified while it was muted is gone; it is not replayed.
func UnmuteChannel(signal string) {
	mu.Lock()
	defer mu.Unlock()

	delete(muteSignal, signal)
}

func init() {
	signalChannels = map[string]chan<- SigData{}
	muteSignal = map[string]struct{}{}
}
