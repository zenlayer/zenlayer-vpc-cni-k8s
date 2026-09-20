// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package signal

import (
	"testing"
)

// The registry these tests exercise is package scoped, so they reset it rather
// than build their own and must not run in parallel.
func resetRegistry(t *testing.T) {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	signalChannels = map[string]chan<- SigData{}
	muteSignal = map[string]struct{}{}
}

// recvOrFail reads one value without blocking. Every delivery in this package is
// either already queued by the time NotifySignal returns or was dropped, so a
// test never has to wait for one -- and waiting would hide a dropped signal
// behind a timeout.
func recvOrFail(t *testing.T, ch <-chan SigData) SigData {
	t.Helper()
	select {
	case got := <-ch:
		return got
	default:
		t.Fatal("expected a signal to be queued on the channel, found none")
		return 0
	}
}

func assertEmpty(t *testing.T, ch <-chan SigData) {
	t.Helper()
	select {
	case got := <-ch:
		t.Fatalf("expected nothing on the channel, got %v", got)
	default:
	}
}

func TestNotifyDeliversToTheRegisteredChannel(t *testing.T) {
	resetRegistry(t)
	ch := make(chan SigData, 1)
	if err := RegisterChannel(WakeGC, ch); err != nil {
		t.Fatalf("RegisterChannel(%q) failed: %v", WakeGC, err)
	}

	if !NotifySignal(WakeGC, SigWakeGC) {
		t.Error("NotifySignal on a live signal returned false, want true")
	}
	if got := recvOrFail(t, ch); got != SigWakeGC {
		t.Errorf("received %v, want %v", got, SigData(SigWakeGC))
	}
}

func TestRegisterChannelKeepsTheFirstChannel(t *testing.T) {
	resetRegistry(t)
	first := make(chan SigData, 1)
	second := make(chan SigData, 1)
	if err := RegisterChannel(WakeGC, first); err != nil {
		t.Fatalf("first RegisterChannel failed: %v", err)
	}
	if err := RegisterChannel(WakeGC, second); err == nil {
		t.Error("second RegisterChannel succeeded, want an error")
	}

	// Which channel wins matters: the loser would wait for a wake-up that goes
	// somewhere else, and nothing would report it.
	NotifySignal(WakeGC, SigWakeGC)
	recvOrFail(t, first)
	assertEmpty(t, second)
}

func TestNotifyWithoutAReceiverDoesNotBlock(t *testing.T) {
	resetRegistry(t)

	// Unbuffered and nobody receiving: the send has to fall through to the
	// default case. NotifySignal sits on the allocation path, so blocking here
	// would stall every pod waiting for an address.
	ch := make(chan SigData)
	if err := RegisterChannel(WakeGC, ch); err != nil {
		t.Fatalf("RegisterChannel failed: %v", err)
	}
	if !NotifySignal(WakeGC, SigWakeGC) {
		t.Error("NotifySignal returned false, want true -- a dropped send is not a mute")
	}
}

func TestNotifyOnAFullChannelReportsTrue(t *testing.T) {
	resetRegistry(t)
	ch := make(chan SigData, 1)
	if err := RegisterChannel(WakeGC, ch); err != nil {
		t.Fatalf("RegisterChannel failed: %v", err)
	}

	NotifySignal(WakeGC, SigWakeGC)
	// A wake-up is already queued, so dropping this one loses nothing: true is
	// the honest answer and the caller must not retry.
	if !NotifySignal(WakeGC, SigWakeGC) {
		t.Error("NotifySignal on a full channel returned false, want true")
	}
	recvOrFail(t, ch)
	assertEmpty(t, ch)
}

func TestNotifyOnAnUnregisteredSignalReportsTrue(t *testing.T) {
	resetRegistry(t)
	// Nobody to reach, so retrying would never help; false is reserved for
	// "muted", which is the one case a caller can do something about.
	if !NotifySignal(WakeGC, SigWakeGC) {
		t.Error("NotifySignal with no registered channel returned false, want true")
	}
}

func TestMuteDiscardsAndReportsFalse(t *testing.T) {
	resetRegistry(t)
	ch := make(chan SigData, 1)
	if err := RegisterChannel(WakeGC, ch); err != nil {
		t.Fatalf("RegisterChannel failed: %v", err)
	}

	MuteChannel(WakeGC)
	if NotifySignal(WakeGC, SigWakeGC) {
		t.Error("NotifySignal on a muted signal returned true, want false")
	}
	assertEmpty(t, ch)
}

func TestUnmuteResumesDeliveryWithoutReplaying(t *testing.T) {
	resetRegistry(t)
	ch := make(chan SigData, 1)
	if err := RegisterChannel(WakeGC, ch); err != nil {
		t.Fatalf("RegisterChannel failed: %v", err)
	}

	MuteChannel(WakeGC)
	NotifySignal(WakeGC, SigWakeGC)
	UnmuteChannel(WakeGC)

	// What was notified while muted is gone. A caller that signaled an edge it
	// cannot observe again has to notify afresh.
	assertEmpty(t, ch)

	if !NotifySignal(WakeGC, SigWakeGC) {
		t.Error("NotifySignal after unmute returned false, want true")
	}
	recvOrFail(t, ch)
}

func TestMuteBeforeClosingKeepsNotifyOffAClosedChannel(t *testing.T) {
	resetRegistry(t)
	ch := make(chan SigData, 1)
	if err := RegisterChannel(WakeGC, ch); err != nil {
		t.Fatalf("RegisterChannel failed: %v", err)
	}

	// This is the sequence a shutting-down receiver follows, and it is why
	// NotifySignal checks the mute set while holding the lock: a send on a
	// closed channel panics, and the panic would take the daemon down rather
	// than the receiver.
	MuteChannel(WakeGC)
	close(ch)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("NotifySignal panicked on a muted, closed channel: %v", r)
		}
	}()
	if NotifySignal(WakeGC, SigWakeGC) {
		t.Error("NotifySignal on a muted signal returned true, want false")
	}
}
