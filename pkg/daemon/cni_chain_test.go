// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// vethOnlyConflist mirrors what the chart renders with cilium.enabled false,
// chainedConflist what it renders with cilium.enabled true. They are kept as
// whole conflists so a template change that breaks this detection fails here,
// rather than on a node that waits for an agent nobody deployed -- or worse,
// one that does not wait for an agent it does chain.
const vethOnlyConflist = `{
  "cniVersion": "0.4.0",
  "name": "zenlayer-cni",
  "plugins": [
    {
      "type": "zec-cni",
      "eniip_virtual_type": "veth",
      "debug": false
    }
  ]
}`

const chainedConflist = `{
  "cniVersion": "0.4.0",
  "name": "zenlayer-cni",
  "plugins": [
    {
      "type": "zec-cni",
      "eniip_virtual_type": "veth",
      "debug": false
    },
    {
      "type": "cilium-cni",
      "chaining-mode": "generic-veth",
      "enable-debug": false
    }
  ]
}`

func TestChainsCilium(t *testing.T) {
	cases := []struct {
		name     string
		conflist string
		want     bool
	}{
		{"chart output without policy", vethOnlyConflist, false},
		{"chart output with policy", chainedConflist, true},
		// A chainer placed before us would be a misconfiguration, but the wait
		// is still the right thing to do: the agent has to be up either way.
		{"cilium first", `{"plugins":[{"type":"cilium-cni"},{"type":"zec-cni"}]}`, true},
		{"no plugins key", `{"cniVersion":"0.4.0","name":"x"}`, false},
		{"empty plugin list", `{"plugins":[]}`, false},
		// The type is matched exactly: a plugin whose name merely contains
		// cilium-cni is not cilium-cni.
		{"lookalike type", `{"plugins":[{"type":"cilium-cni-shim"}]}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := chainsCilium([]byte(tc.conflist))
			if err != nil {
				t.Fatalf("chainsCilium: %v", err)
			}
			if got != tc.want {
				t.Errorf("chainsCilium = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestChainsCiliumRejectsGarbage pins that an unparsable conflist is an error.
// Returning false would install a file kubelet cannot use, and the wait would
// be skipped for a chain we simply failed to see.
func TestChainsCiliumRejectsGarbage(t *testing.T) {
	for _, in := range []string{"", "not json", `{"plugins": "notalist"}`} {
		if _, err := chainsCilium([]byte(in)); err == nil {
			t.Errorf("chainsCilium(%q) = nil error, want an error", in)
		}
	}
}

func TestCiliumWaitTimeout(t *testing.T) {
	cases := []struct {
		name string
		env  string
		want time.Duration
	}{
		{"unset", "", defaultCiliumWaitTimeout},
		{"override", "90s", 90 * time.Second},
		{"minutes", "2m", 2 * time.Minute},
		// Anything unusable falls back rather than failing: a typo in an
		// operational knob should not stop the node from coming up.
		{"garbage", "soon", defaultCiliumWaitTimeout},
		{"zero", "0s", defaultCiliumWaitTimeout},
		{"negative", "-5s", defaultCiliumWaitTimeout},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(envCiliumWaitTimeout, tc.env)
			if got := ciliumWaitTimeout(); got != tc.want {
				t.Errorf("ciliumWaitTimeout = %s, want %s", got, tc.want)
			}
		})
	}
}

// socketPath keeps the path short. Unix socket paths are capped around 100
// bytes by the kernel, and t.TempDir() under the default macOS/Linux temp
// roots plus a long test name can exceed it.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "cs")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "c.sock")
}

func TestAgentReachable(t *testing.T) {
	sock := socketPath(t)

	if agentReachable(sock) {
		t.Fatal("agentReachable = true for a path that does not exist")
	}

	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	// Keep the socket file after Close so the stale case below is reachable.
	// Go unlinks it by default, which is tidier than what a killed agent
	// leaves behind -- and it is that untidy leftover this test is about.
	ln.(*net.UnixListener).SetUnlinkOnClose(false)

	if !agentReachable(sock) {
		t.Error("agentReachable = false while a listener is accepting")
	}

	// The case that motivates dialing instead of stat'ing: an agent that died
	// leaves the socket file on disk. os.Stat still succeeds on it, so a
	// stat-based readiness check would call a dead agent ready and let the
	// conflist go live against an agent that answers nothing.
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	if _, err := os.Stat(sock); err != nil {
		t.Fatalf("socket file went away despite SetUnlinkOnClose(false): %v", err)
	}
	if agentReachable(sock) {
		t.Error("agentReachable = true for a stale socket with no listener")
	}
}

func TestWaitForCiliumAgentReturnsWhenReady(t *testing.T) {
	sock := socketPath(t)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	if err := waitForCiliumAgent(context.Background(), sock, time.Second); err != nil {
		t.Errorf("waitForCiliumAgent: %v", err)
	}
}

// TestWaitForCiliumAgentWaitsThenSucceeds covers the real sequence: the daemon
// gets there before cilium does.
func TestWaitForCiliumAgentWaitsThenSucceeds(t *testing.T) {
	sock := socketPath(t)

	started := make(chan struct{})
	go func() {
		time.Sleep(2 * ciliumWaitPoll)
		ln, err := net.Listen("unix", sock)
		if err != nil {
			close(started)
			return
		}
		close(started)
		<-t.Context().Done()
		_ = ln.Close()
	}()

	if err := waitForCiliumAgent(context.Background(), sock, 30*time.Second); err != nil {
		t.Errorf("waitForCiliumAgent: %v", err)
	}
	<-started
}

func TestWaitForCiliumAgentTimesOut(t *testing.T) {
	err := waitForCiliumAgent(context.Background(), socketPath(t), 2*ciliumWaitPoll)
	if err == nil {
		t.Fatal("waitForCiliumAgent = nil, want a timeout error")
	}
	// The message is the only thing an operator sees, so it has to name the
	// plugin that forced the wait and the variable that extends it.
	for _, want := range []string{"cilium-cni", envCiliumWaitTimeout} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}

func TestWaitForCiliumAgentHonoursContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// A generous timeout, so passing can only mean the context was honored.
	if err := waitForCiliumAgent(ctx, socketPath(t), time.Hour); err == nil {
		t.Fatal("waitForCiliumAgent = nil, want a context error")
	}
}

// TestWaitForCiliumAgentFailsFastWhenDirectoryMissing pins the difference
// between "the agent has not created its socket yet" and "this container has no
// path to the agent at all". Only the first is worth waiting out; the second
// means the DaemonSet and the CNI ConfigMap disagree about chaining, which no
// amount of waiting repairs. Reporting it as a timeout would name the agent,
// which is typically running with its socket present on the host, and so point
// the reader at the wrong component.
func TestWaitForCiliumAgentFailsFastWhenDirectoryMissing(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "not-mounted", "cilium.sock")

	start := time.Now()
	err := waitForCiliumAgent(context.Background(), missing, time.Minute)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("waitForCiliumAgent = nil, want an error when the agent directory is absent")
	}
	// Fast, not "faster than the timeout": it must never enter the poll loop.
	if elapsed > 5*time.Second {
		t.Errorf("waitForCiliumAgent took %s; it must not wait out the timeout when the directory is missing", elapsed)
	}
	// The message has to name the real cause. Checking for the directory alone
	// would also pass on the timeout message, which contains the socket path.
	for _, want := range []string{"does not exist inside this container", "DaemonSet", "ConfigMap", "helm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if strings.Contains(err.Error(), "not reachable after") {
		t.Errorf("error %q is the timeout message; the missing-mount case must not reach the poll loop", err)
	}
}
