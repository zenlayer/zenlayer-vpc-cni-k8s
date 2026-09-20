// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package plugin

import (
	"testing"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/plugin/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/rpc"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// TestGetDatePath: veth is the only datapath. An unknown value must be refused,
// not silently treated as veth -- a node running a datapath other than the
// configured one is harder to diagnose than a pod that will not start.
func TestGetDatePath(t *testing.T) {
	tests := []struct {
		name    string
		typ     string
		wantErr bool
	}{
		{name: "absent field defaults to veth", typ: ""},
		{name: "veth", typ: "veth"},
		{name: "veth is case-insensitive", typ: "VETH"},
		{name: "unknown value is refused", typ: "macvlan", wantErr: true},
		{name: "untrimmed veth is not veth", typ: " veth ", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getDatePath(rpc.IPType_TypeENIMultiIP, &zecTypes.CNIConf{ENIIPVirtualType: tt.typ})
			if tt.wantErr {
				if err == nil {
					t.Fatalf("getDatePath(%q) = %v, want an error", tt.typ, got)
				}
				// A refused value must never come back as a usable datapath.
				if got != types.DataPathUnknown {
					t.Errorf("getDatePath(%q) = %v alongside an error, want DataPathUnknown", tt.typ, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("getDatePath(%q) unexpected error: %v", tt.typ, err)
			}
			if got != types.PolicyRoute {
				t.Errorf("getDatePath(%q) = %v, want PolicyRoute", tt.typ, got)
			}
		})
	}
}

// TestGetDatePathRejectsOtherIPTypes uses an out-of-range value because
// TypeENIMultiIP is the enum's only member: it stands in for a future IP type,
// and the check has to refuse by default rather than fall through.
func TestGetDatePathRejectsOtherIPTypes(t *testing.T) {
	got, err := getDatePath(rpc.IPType(99), &zecTypes.CNIConf{})
	if err == nil {
		t.Fatalf("getDatePath accepted ipType 99, want an error (got %v)", got)
	}
	if got != types.DataPathUnknown {
		t.Errorf("getDatePath returned %v alongside an error, want DataPathUnknown", got)
	}
}
