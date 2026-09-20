// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"strings"
	"testing"
)

func TestGetCniNicName(t *testing.T) {
	tests := []struct {
		name     string
		nodeName string
		want     string
	}{
		{name: "simple node name", nodeName: "node1", want: "zenlayer-cni-create-nic-node1"},
		{name: "empty node name still gets the separator", nodeName: "", want: "zenlayer-cni-create-nic-"},
		{name: "node name with dots", nodeName: "ip-10.0.0.1", want: "zenlayer-cni-create-nic-ip-10.0.0.1"},
		{name: "node name with dashes", nodeName: "worker-a-b", want: "zenlayer-cni-create-nic-worker-a-b"},
		{name: "node name with uppercase is not normalised", nodeName: "Node1", want: "zenlayer-cni-create-nic-Node1"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := GetCniNicName(tt.nodeName)
			if got != tt.want {
				t.Errorf("GetCniNicName(%q) = %q, want %q", tt.nodeName, got, tt.want)
			}
		})
	}
}

func TestGetCniNicNameIsDeterministic(t *testing.T) {
	first := GetCniNicName("node1")
	second := GetCniNicName("node1")
	if first != second {
		t.Errorf("GetCniNicName is not deterministic: %q then %q", first, second)
	}
	if GetCniNicName("node1") == GetCniNicName("node2") {
		t.Error("GetCniNicName produced the same name for two different nodes")
	}
}

// TestCniENITagsCarryBothClaims pins the two tags ownership rests on. The name
// is deliberately not one of them: an operator can name an interface anything,
// including this daemon's exact prefix, and isOwnENI must not adopt it on that
// basis -- adoption ends in detach and delete. Both keys are required, since
// created-by alone would let one node manage another node's interfaces.
func TestCniENITagsCarryBothClaims(t *testing.T) {
	const instanceID = "1755670178034880550"
	tags := CniENITags(instanceID)

	if len(tags) != 2 {
		t.Fatalf("CniENITags returned %d tags, want 2: %v", len(tags), tags)
	}
	if got := tags[TagKeyCreatedBy]; got != TagValueCreatedBy {
		t.Errorf("tag %q = %q, want %q", TagKeyCreatedBy, got, TagValueCreatedBy)
	}
	if got := tags[TagKeyInstanceID]; got != instanceID {
		t.Errorf("tag %q = %q, want %q", TagKeyInstanceID, got, instanceID)
	}

	// Two nodes must never produce the same instance-id claim.
	if CniENITags("node-a")[TagKeyInstanceID] == CniENITags("node-b")[TagKeyInstanceID] {
		t.Error("CniENITags produced the same instance-id for two different instances")
	}
}

func TestCniNicNameLengthAwareness(t *testing.T) {
	// Linux limits interface names to 15 characters (IFNAMSIZ-1); the generated
	// name is far longer, which documents that it is not used as a kernel
	// interface name but as an API-side resource name.
	name := GetCniNicName("node1")
	if len(name) <= 15 {
		t.Errorf("GetCniNicName(%q) = %q (%d chars); the test's assumption about the naming scheme changed",
			"node1", name, len(name))
	}
	if !strings.HasPrefix(name, "zenlayer-") {
		t.Errorf("GetCniNicName(%q) = %q, want it to start with the zenlayer vendor prefix", "node1", name)
	}
}
