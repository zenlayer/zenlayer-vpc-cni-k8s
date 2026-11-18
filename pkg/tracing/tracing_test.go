// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package tracing

import (
	"strings"
	"testing"
)

// TestUnregisteredTracerReportsFailure pins that a tracer with no recorder
// registered reports it cannot record, rather than accepting the event.
// Every call site outside this package discards the error (`_ =
// tracing.RecordNodeEvent(...)`), so with no recorder registered the whole
// node-event surface goes missing and nothing says so.
func TestUnregisteredTracerReportsFailure(t *testing.T) {
	tr := NewTracer()

	if err := tr.RecordNodeEvent("Warning", "Reason", "message"); err == nil {
		t.Error("RecordNodeEvent() on an unregistered tracer returned nil, want an error")
	}
	if err := tr.RecordPodEvent("pod", "ns", "Warning", "Reason", "message"); err == nil {
		t.Error("RecordPodEvent() on an unregistered tracer returned nil, want an error")
	}
}

func TestRegisteredRecordersReceiveEvents(t *testing.T) {
	tr := NewTracer()

	var (
		nodeCalls int
		podCalls  int
		gotReason string
	)
	tr.RegisterEventRecorder(
		func(eventType, reason, message string) {
			nodeCalls++
			gotReason = reason
		},
		func(podName, podNamespace, eventType, reason, message string) error {
			podCalls++
			return nil
		},
	)

	if err := tr.RecordNodeEvent("Warning", "InsufficientIpInSubnet", "subnet is full"); err != nil {
		t.Fatalf("RecordNodeEvent() = %v, want nil", err)
	}
	if nodeCalls != 1 {
		t.Errorf("node recorder called %d times, want 1", nodeCalls)
	}
	if gotReason != "InsufficientIpInSubnet" {
		t.Errorf("node recorder got reason %q, want %q", gotReason, "InsufficientIpInSubnet")
	}

	if err := tr.RecordPodEvent("pod", "ns", "Normal", "AllocIPSucceed", "ok"); err != nil {
		t.Fatalf("RecordPodEvent() = %v, want nil", err)
	}
	if podCalls != 1 {
		t.Errorf("pod recorder called %d times, want 1", podCalls)
	}
}

// A platform error can carry a whole response body; the event keeps the head.
func TestTruncateMessage(t *testing.T) {
	long := strings.Repeat("<html>", 200)
	got := TruncateMessage(long)
	if n := len([]rune(got)); n >= len(long) || !strings.HasSuffix(got, "(truncated; see the log)") {
		t.Fatalf("got %d runes ending %q; want it cut and marked", n, got[max(0, len(got)-30):])
	}
	if short := "subnet is full"; TruncateMessage(short) != short {
		t.Fatalf("a short message changed: %q", TruncateMessage(short))
	}
}
