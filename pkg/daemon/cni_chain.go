// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
	// ciliumPluginType is the plugin name cilium installs into /opt/cni/bin.
	ciliumPluginType = "cilium-cni"

	// ciliumAgentSocket is the cilium-agent API socket. cilium-cni dials it on
	// every ADD, so the agent has to be up before the conflist that chains it
	// becomes visible to kubelet.
	//
	// The path is cilium's default (--socket-path) and is not configurable here
	// on purpose: a value that disagrees with the agent's would make this wait
	// pass or hang for reasons unrelated to the agent's state.
	ciliumAgentSocket = "/var/run/cilium/cilium.sock"

	// envCiliumWaitTimeout overrides how long to wait for the agent.
	envCiliumWaitTimeout = "ZENLAYER_CNI_CILIUM_WAIT_TIMEOUT"

	defaultCiliumWaitTimeout = 5 * time.Minute

	// ciliumWaitPoll is how often the socket is dialed, ciliumWaitLogEvery how
	// often the wait is reported. Waiting quietly for minutes looks from the
	// outside like a node whose conflist never lands: /healthz answers 200
	// throughout, while a fresh node admits no pod until the wait finishes.
	ciliumWaitPoll     = 500 * time.Millisecond
	ciliumWaitLogEvery = 10 * time.Second
)

// conflistPlugins is the sliver of a CNI conflist this file needs. Unknown keys
// are ignored; a conflist that is not valid JSON becomes an error rather than a
// silent "not chained".
type conflistPlugins struct {
	Plugins []struct {
		Type string `json:"type"`
	} `json:"plugins"`
}

// chainsCilium reports whether the conflist hands the pod to cilium-cni after
// this plugin.
//
// The conflist is the source of truth rather than a helm value of our own,
// because it is what kubelet executes. A separate flag could disagree with it,
// and either way round -- waiting for an agent that is not chained, or not
// waiting for one that is -- is the failure this check exists to prevent.
func chainsCilium(conflist []byte) (bool, error) {
	var c conflistPlugins
	if err := json.Unmarshal(conflist, &c); err != nil {
		return false, fmt.Errorf("parse conflist: %w", err)
	}
	for _, p := range c.Plugins {
		if p.Type == ciliumPluginType {
			return true, nil
		}
	}
	return false, nil
}

// ciliumWaitTimeout reads envCiliumWaitTimeout, falling back to
// defaultCiliumWaitTimeout when it is unset, unparseable or not positive.
func ciliumWaitTimeout() time.Duration {
	raw := os.Getenv(envCiliumWaitTimeout)
	if raw == "" {
		return defaultCiliumWaitTimeout
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		log.Errorf("Ignoring %s=%q, using %s: %v", envCiliumWaitTimeout, raw, defaultCiliumWaitTimeout, err)
		return defaultCiliumWaitTimeout
	}
	return d
}

// agentReachable reports whether something is currently accepting on socket.
//
// It dials rather than stats: a unix socket left behind by a dead agent still
// stats fine, and treating that as ready would publish the conflist against an
// agent that never answers -- the failure this wait exists to avoid.
func agentReachable(socket string) bool {
	conn, err := net.DialTimeout("unix", socket, time.Second)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// waitForCiliumAgent blocks until the cilium-agent socket answers, ctx is
// done, or timeout elapses.
//
// Only ADD needs this. cilium-cni's DEL degrades on its own when the agent is
// unreachable -- it queues the deletion under /run/cilium/deleteQueue for the
// agent to replay -- but ADD has no such path, so a conflist that goes live
// early turns into pods stuck in ContainerCreating with no self-healing.
//
// Timing out is an error rather than a warning: proceeding would install a
// conflist whose every ADD fails. The error terminates the daemon, so it shows
// up as a container restart; a conflist that never lands shows up nowhere.
func waitForCiliumAgent(ctx context.Context, socket string, timeout time.Duration) error {
	if agentReachable(socket) {
		log.Infof("Cilium agent socket %s is ready", socket)
		return nil
	}

	// Separate "the socket is not there yet" from "this container has no way to
	// reach it at all", because only the first one is worth waiting out.
	//
	// The chart mounts the agent's directory only when cilium.enabled is set,
	// while the decision made here comes from the conflist -- a different
	// Kubernetes object. An upgrade that fails partway can leave a node with a
	// DaemonSet that dropped the mount and a ConfigMap that still chains
	// cilium-cni. The conflist is still the right thing to trust (see
	// chainsCilium), but with the directory absent the wait can never succeed,
	// and the timeout message would blame the agent -- which is usually running,
	// with its socket present on the host -- sending whoever is on call to the
	// wrong component.
	//
	// Checked on the directory rather than the socket: the socket is legitimately
	// absent before the agent creates it, so only the directory distinguishes the
	// two cases.
	dir := filepath.Dir(socket)
	if _, err := os.Stat(dir); os.IsNotExist(err) {
		return fmt.Errorf("the conflist chains %s but %s does not exist inside this container: "+
			"the DaemonSet was rendered without the cilium agent mount while the CNI ConfigMap "+
			"still chains %s, so the two disagree and no amount of waiting will fix it. "+
			"Check whether the last helm upgrade failed partway (helm history), then re-apply it. "+
			"Not waiting for the agent",
			ciliumPluginType, dir, ciliumPluginType)
	}

	log.Infof("Conflist chains %s; waiting up to %s for the cilium agent on %s",
		ciliumPluginType, timeout, socket)

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(ciliumWaitPoll)
	defer ticker.Stop()
	lastLog := time.Now()

	for {
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for cilium agent on %s: %w", socket, ctx.Err())
		case <-ticker.C:
			if agentReachable(socket) {
				log.Infof("Cilium agent on %s is ready", socket)
				return nil
			}
			if time.Now().After(deadline) {
				return fmt.Errorf("cilium agent on %s not reachable after %s: "+
					"the conflist chains %s, so installing it now would fail every pod. "+
					"Check that the cilium DaemonSet is running on this node, or set %s to wait longer",
					socket, timeout, ciliumPluginType, envCiliumWaitTimeout)
			}
			if time.Since(lastLog) >= ciliumWaitLogEvery {
				log.Infof("Still waiting for the cilium agent on %s (%s left)",
					socket, time.Until(deadline).Truncate(time.Second))
				lastLog = time.Now()
			}
		}
	}
}
