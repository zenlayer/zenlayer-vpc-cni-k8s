// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package linux

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
)

func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	hw, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("parse mac %q: %v", s, err)
	}
	return hw
}

func device(name string, index int, mac net.HardwareAddr, masterIndex int) *netlink.Device {
	return &netlink.Device{LinkAttrs: netlink.LinkAttrs{
		Name:         name,
		Index:        index,
		HardwareAddr: mac,
		MasterIndex:  masterIndex,
	}}
}

func TestPickDeviceByMac(t *testing.T) {
	const eniMAC = "52:54:00:36:2a:02"
	mac := mustMAC(t, eniMAC)
	other := mustMAC(t, "52:54:00:10:95:d2")

	// The topology this function exists for: virtio-net failover presents one
	// logical NIC as three links sharing a MAC -- eth0 is the addressed master,
	// eth1 the virtio standby leg, eth2 the SR-IOV VF leg. Only eth0 is the ENI.
	failover := []netlink.Link{
		device("eth0", 2, mac, 0),
		device("eth1", 3, mac, 2),
		device("eth2", 4, mac, 2),
	}

	tests := []struct {
		name    string
		links   []netlink.Link
		mac     string
		want    int32
		wantErr bool
	}{
		{
			name:  "failover group resolves to the master",
			links: failover,
			mac:   eniMAC,
			want:  2,
		},
		{
			name: "master enumerated after its slaves",
			// LinkList order is not part of netlink's contract, so the slaves
			// must be rejected on their own merits rather than out-raced.
			links: []netlink.Link{
				device("eth1", 3, mac, 2),
				device("eth2", 4, mac, 2),
				device("eth0", 2, mac, 0),
			},
			mac:  eniMAC,
			want: 2,
		},
		{
			name:  "plain virtio instance, one link per mac",
			links: []netlink.Link{device("eth0", 2, mac, 0)},
			mac:   eniMAC,
			want:  2,
		},
		{
			name: "plain virtio, second eni",
			// Two ENIs attached, no failover: distinct MACs, neither enslaved.
			links: []netlink.Link{device("eth0", 2, other, 0), device("eth1", 3, mac, 0)},
			mac:   eniMAC,
			want:  3,
		},
		{
			name: "failover, second eni is its own group",
			// Each ENI brings its own master+standby+VF triple; groups are told
			// apart by MAC, and within a group by MasterIndex.
			links: []netlink.Link{
				device("eth0", 2, other, 0), device("eth1", 3, other, 2), device("eth2", 4, other, 2),
				device("eth3", 5, mac, 0), device("eth4", 6, mac, 5), device("eth5", 7, mac, 5),
			},
			mac:  eniMAC,
			want: 5,
		},
		{
			name: "virtual links are skipped even on a mac match",
			// An ipvlan L2 slave inherits its parent's MAC, and a bridge
			// inherits a member's, so a MAC match alone does not identify an
			// ENI -- the concrete type has to reject them.
			links: []netlink.Link{
				&netlink.Veth{LinkAttrs: netlink.LinkAttrs{Name: "eni123", Index: 9, HardwareAddr: mac}},
				&netlink.IPVlan{LinkAttrs: netlink.LinkAttrs{Name: "ipvl_2", Index: 10, HardwareAddr: mac}},
				&netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: "br0", Index: 11, HardwareAddr: mac}},
				device("eth0", 2, mac, 0),
			},
			mac:  eniMAC,
			want: 2,
		},
		{
			name: "master reported with an unrecognized kind is still accepted",
			// A link the kernel labels with a kind netlink does not know
			// decodes as GenericLink rather than Device. Rejecting it would
			// leave no candidate at all once the slaves are excluded.
			links: []netlink.Link{
				device("eth1", 3, mac, 2),
				&netlink.GenericLink{
					LinkType:  "failover",
					LinkAttrs: netlink.LinkAttrs{Name: "eth0", Index: 2, HardwareAddr: mac},
				},
			},
			mac:  eniMAC,
			want: 2,
		},
		{
			name:    "only slaves carry the mac",
			links:   []netlink.Link{device("eth1", 3, mac, 2), device("eth2", 4, mac, 2)},
			mac:     eniMAC,
			wantErr: true,
		},
		{
			name:    "mac not present",
			links:   []netlink.Link{device("eth0", 2, other, 0)},
			mac:     eniMAC,
			wantErr: true,
		},
		{
			name:    "no links at all",
			links:   nil,
			mac:     eniMAC,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := pickDeviceByMac(tt.links, tt.mac)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got ifindex %d", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("ifindex = %d, want %d", got, tt.want)
			}
		})
	}
}

func TestVethNameForPod(t *testing.T) {
	// The host end has to be re-derivable at teardown from pod identity alone,
	// so the mapping must be stable and collision-resistant across the fields
	// that identify a pod.
	name, err := VethNameForPod("nginx", "default", "eth0", "eni")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(name) > 15 {
		t.Errorf("veth name %q is %d chars, kernel limit is 15", name, len(name))
	}

	again, err := VethNameForPod("nginx", "default", "eth0", "eni")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if again != name {
		t.Errorf("not deterministic: %q then %q", name, again)
	}

	// eth0 is normalised away, so the default interface and an empty ifName
	// name the same device rather than two.
	empty, err := VethNameForPod("nginx", "default", "", "eni")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if empty != name {
		t.Errorf("eth0 should normalise to empty ifName: %q vs %q", name, empty)
	}

	for _, other := range []struct{ pod, ns, ifName string }{
		{"nginx", "kube-system", "eth0"},
		{"nginx2", "default", "eth0"},
		{"nginx", "default", "eth1"},
	} {
		got, err := VethNameForPod(other.pod, other.ns, other.ifName, "eni")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == name {
			t.Errorf("collision: %v maps to %q, same as default/nginx/eth0", other, got)
		}
	}
}
