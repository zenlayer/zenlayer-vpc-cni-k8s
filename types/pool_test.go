// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"net"
	"reflect"
	"testing"
)

func TestResourceItem_ToNetResource(t *testing.T) {
	tests := []struct {
		name       string
		item       ResourceItem
		wantNil    bool
		wantType   string
		wantID     string
		wantENIID  string
		wantENIMac string
		wantIPv4   string
		wantIPv6   string
	}{
		{
			name: "eniIp with a dual stack address",
			item: ResourceItem{
				Type:   NetResourceTypeEniIp,
				ID:     "stored-id",
				ENIId:  "eni-1",
				ENIMac: "00:11:22:33:44:55",
				IPv4:   "192.168.1.10",
				IPv6:   "fd00::10",
			},
			wantType:   NetResourceTypeEniIp,
			wantID:     "eni-1/192.168.1.10-fd00::10",
			wantENIID:  "eni-1",
			wantENIMac: "00:11:22:33:44:55",
			wantIPv4:   "192.168.1.10",
			wantIPv6:   "fd00::10",
		},
		{
			name: "eniIp with ipv4 only",
			item: ResourceItem{
				Type:   NetResourceTypeEniIp,
				ENIId:  "eni-2",
				ENIMac: "00:11:22:33:44:66",
				IPv4:   "10.0.0.1",
			},
			wantType:   NetResourceTypeEniIp,
			wantID:     "eni-2/10.0.0.1",
			wantENIID:  "eni-2",
			wantENIMac: "00:11:22:33:44:66",
			wantIPv4:   "10.0.0.1",
		},
		{
			name: "eniIp with ipv6 only",
			item: ResourceItem{
				Type:   NetResourceTypeEniIp,
				ENIId:  "eni-3",
				ENIMac: "00:11:22:33:44:77",
				IPv6:   "2001:db8::1",
			},
			wantType:   NetResourceTypeEniIp,
			wantID:     "eni-3/2001:db8::1",
			wantENIID:  "eni-3",
			wantENIMac: "00:11:22:33:44:77",
			wantIPv6:   "2001:db8::1",
		},
		{
			// Unparsable addresses become nil rather than an error.
			name: "eniIp with malformed addresses keeps the eni but drops the ips",
			item: ResourceItem{
				Type:   NetResourceTypeEniIp,
				ENIId:  "eni-4",
				ENIMac: "00:11:22:33:44:88",
				IPv4:   "not-an-ip",
				IPv6:   "",
			},
			wantType:   NetResourceTypeEniIp,
			wantID:     "eni-4/",
			wantENIID:  "eni-4",
			wantENIMac: "00:11:22:33:44:88",
		},
		{
			name: "mac is normalised to lower case",
			item: ResourceItem{
				Type:   NetResourceTypeEniIp,
				ENIId:  "eni-5",
				ENIMac: "AA:BB:CC:DD:EE:FF",
				IPv4:   "10.0.0.2",
			},
			wantType:   NetResourceTypeEniIp,
			wantID:     "eni-5/10.0.0.2",
			wantENIID:  "eni-5",
			wantENIMac: "aa:bb:cc:dd:ee:ff",
			wantIPv4:   "10.0.0.2",
		},
		{
			name: "dash separated mac is accepted and normalised",
			item: ResourceItem{
				Type:   NetResourceTypeEniIp,
				ENIId:  "eni-6",
				ENIMac: "00-11-22-33-44-99",
				IPv4:   "10.0.0.3",
			},
			wantType:   NetResourceTypeEniIp,
			wantID:     "eni-6/10.0.0.3",
			wantENIID:  "eni-6",
			wantENIMac: "00:11:22:33:44:99",
			wantIPv4:   "10.0.0.3",
		},
		{
			name: "invalid mac yields nil",
			item: ResourceItem{
				Type:   NetResourceTypeEniIp,
				ENIId:  "eni-7",
				ENIMac: "zz:zz:zz:zz:zz:zz",
				IPv4:   "10.0.0.4",
			},
			wantNil: true,
		},
		{
			name: "empty mac yields nil",
			item: ResourceItem{
				Type:  NetResourceTypeEniIp,
				ENIId: "eni-8",
				IPv4:  "10.0.0.5",
			},
			wantNil: true,
		},
		{
			name: "truncated mac yields nil",
			item: ResourceItem{
				Type:   NetResourceTypeEniIp,
				ENIId:  "eni-9",
				ENIMac: "00:11:22:33:44",
				IPv4:   "10.0.0.6",
			},
			wantNil: true,
		},
		{
			// Only eniIp is handled by the switch; every other type falls
			// through and returns nil even though the mac parsed fine.
			name: "eni type is not converted",
			item: ResourceItem{
				Type:   NetResourceTypeEni,
				ENIId:  "eni-10",
				ENIMac: "00:11:22:33:44:aa",
				IPv4:   "10.0.0.7",
			},
			wantNil: true,
		},
		{
			name: "unknown type is not converted",
			item: ResourceItem{
				Type:   "something-else",
				ENIId:  "eni-11",
				ENIMac: "00:11:22:33:44:bb",
			},
			wantNil: true,
		},
		{
			name:    "zero value item yields nil",
			item:    ResourceItem{},
			wantNil: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.item.ToNetResource()
			if tt.wantNil {
				if got != nil {
					t.Fatalf("ToNetResource() = %#v, want nil", got)
				}
				return
			}
			if got == nil {
				t.Fatal("ToNetResource() = nil, want a resource")
			}
			if got.GetType() != tt.wantType {
				t.Errorf("GetType() = %q, want %q", got.GetType(), tt.wantType)
			}
			if got.GetID() != tt.wantID {
				t.Errorf("GetID() = %q, want %q", got.GetID(), tt.wantID)
			}
			item := got.GetResourceItem()
			if item.ENIId != tt.wantENIID {
				t.Errorf("ENIId = %q, want %q", item.ENIId, tt.wantENIID)
			}
			if item.ENIMac != tt.wantENIMac {
				t.Errorf("ENIMac = %q, want %q", item.ENIMac, tt.wantENIMac)
			}
			if item.IPv4 != tt.wantIPv4 {
				t.Errorf("IPv4 = %q, want %q", item.IPv4, tt.wantIPv4)
			}
			if item.IPv6 != tt.wantIPv6 {
				t.Errorf("IPv6 = %q, want %q", item.IPv6, tt.wantIPv6)
			}
		})
	}
}

// TestResourceItem_ToNetResourceRoundTrip documents that ToNetResource followed
// by GetResourceItem reproduces the fields it is given here, except ID, which is
// recomputed from the ENI id and the addresses rather than carried over. The mac
// survives only because it is already normalised; see the cases above.
func TestResourceItem_ToNetResourceRoundTrip(t *testing.T) {
	in := ResourceItem{
		Type:   NetResourceTypeEniIp,
		ID:     "an-id-that-is-not-preserved",
		ENIId:  "eni-abc",
		ENIMac: "00:11:22:33:44:55",
		IPv4:   "192.168.1.10",
		IPv6:   "fd00::10",
	}
	out := in.ToNetResource().GetResourceItem()

	want := in
	want.ID = "eni-abc/192.168.1.10-fd00::10"
	if !reflect.DeepEqual(out, want) {
		t.Errorf("round trip = %+v, want %+v", out, want)
	}
}

func TestENIIP_GetID(t *testing.T) {
	tests := []struct {
		name string
		in   *ENIIP
		want string
	}{
		{name: "nil receiver", in: nil, want: ""},
		{
			name: "nil eni still renders",
			in:   &ENIIP{IPSet: IPSet{IPv4: mustIP(t, "10.0.0.1")}},
			want: "/10.0.0.1",
		},
		{
			name: "eni without addresses",
			in:   &ENIIP{ENI: &ENI{ID: "eni-1"}},
			want: "eni-1/",
		},
		{
			name: "dual stack",
			in: &ENIIP{
				ENI:   &ENI{ID: "eni-1"},
				IPSet: IPSet{IPv4: mustIP(t, "10.0.0.1"), IPv6: mustIP(t, "fd00::1")},
			},
			want: "eni-1/10.0.0.1-fd00::1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.in.GetID(); got != tt.want {
				t.Errorf("GetID() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestENIIP_GetResourceItem(t *testing.T) {
	e := &ENIIP{
		ENI: &ENI{
			ID:  "eni-1",
			Mac: mustMAC(t, "00:11:22:33:44:55"),
		},
		IPSet: IPSet{IPv4: mustIP(t, "10.0.0.1"), IPv6: mustIP(t, "fd00::1")},
	}
	want := ResourceItem{
		Type:   NetResourceTypeEniIp,
		ID:     "eni-1/10.0.0.1-fd00::1",
		ENIId:  "eni-1",
		ENIMac: "00:11:22:33:44:55",
		IPv4:   "10.0.0.1",
		IPv6:   "fd00::1",
	}
	if got := e.GetResourceItem(); !reflect.DeepEqual(got, want) {
		t.Errorf("GetResourceItem() = %+v, want %+v", got, want)
	}

	t.Run("missing mac and addresses render as empty strings", func(t *testing.T) {
		bare := &ENIIP{ENI: &ENI{ID: "eni-2"}}
		want := ResourceItem{
			Type:  NetResourceTypeEniIp,
			ID:    "eni-2/",
			ENIId: "eni-2",
		}
		if got := bare.GetResourceItem(); !reflect.DeepEqual(got, want) {
			t.Errorf("GetResourceItem() = %+v, want %+v", got, want)
		}
	})
}

func TestENI_GetIDAndType(t *testing.T) {
	var nilENI *ENI
	if got := nilENI.GetID(); got != "" {
		t.Errorf("nil ENI GetID() = %q, want empty", got)
	}
	if got := (&ENI{ID: "eni-1"}).GetID(); got != "eni-1" {
		t.Errorf("GetID() = %q, want eni-1", got)
	}
	if got := (&ENI{}).GetType(); got != NetResourceTypeEni {
		t.Errorf("GetType() = %q, want %q", got, NetResourceTypeEni)
	}
}

func TestENI_GetResourceItem(t *testing.T) {
	tests := []struct {
		name string
		eni  *ENI
		want ResourceItem
	}{
		{
			name: "fully populated",
			eni: &ENI{
				ID:        "eni-1",
				Mac:       mustMAC(t, "00:11:22:33:44:55"),
				PrimaryIP: IPSet{IPv4: mustIP(t, "10.0.0.1"), IPv6: mustIP(t, "fd00::1")},
			},
			want: ResourceItem{
				Type:   NetResourceTypeEni,
				ID:     "eni-1",
				ENIId:  "eni-1",
				ENIMac: "00:11:22:33:44:55",
				IPv4:   "10.0.0.1",
				IPv6:   "fd00::1",
			},
		},
		{
			name: "nil mac and empty primary ip",
			eni:  &ENI{ID: "eni-2"},
			want: ResourceItem{
				Type:  NetResourceTypeEni,
				ID:    "eni-2",
				ENIId: "eni-2",
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.eni.GetResourceItem(); !reflect.DeepEqual(got, tt.want) {
				t.Errorf("GetResourceItem() = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestENI_ToPb(t *testing.T) {
	t.Run("nil receiver yields nil", func(t *testing.T) {
		var e *ENI
		if got := e.ToPb(); got != nil {
			t.Errorf("ToPb() = %v, want nil", got)
		}
	})

	t.Run("fully populated", func(t *testing.T) {
		e := &ENI{
			ID:  "eni-1",
			Mac: mustMAC(t, "00:11:22:33:44:55"),
			Subnet: Subnet{
				ID: "subnet-1",
				Gateway: &IPSet{
					IPv4: mustIP(t, "192.168.1.1"),
					IPv6: mustIP(t, "fd00::1"),
				},
				CIDR: &IPNetSet{
					IPv4: mustIPNet(t, "192.168.1.0/24"),
					IPv6: mustIPNet(t, "fd00::/64"),
				},
			},
		}
		got := e.ToPb()
		if got.ID != "eni-1" {
			t.Errorf("ID = %q, want eni-1", got.ID)
		}
		if got.MAC != "00:11:22:33:44:55" {
			t.Errorf("MAC = %q, want 00:11:22:33:44:55", got.MAC)
		}
		if got.GatewayMac != GATEWAY_MAC {
			t.Errorf("GatewayMac = %q, want the hard coded %q", got.GatewayMac, GATEWAY_MAC)
		}
		if got.GatewayIP.GetIPv4() != "192.168.1.1" || got.GatewayIP.GetIPv6() != "fd00::1" {
			t.Errorf("GatewayIP = %v, want 192.168.1.1 / fd00::1", got.GatewayIP)
		}
		if got.Subnet.GetIPv4() != "192.168.1.0/24" || got.Subnet.GetIPv6() != "fd00::/64" {
			t.Errorf("Subnet = %v, want 192.168.1.0/24 / fd00::/64", got.Subnet)
		}
	})

	t.Run("nil mac becomes an empty string", func(t *testing.T) {
		got := (&ENI{ID: "eni-1"}).ToPb()
		if got.MAC != "" {
			t.Errorf("MAC = %q, want empty", got.MAC)
		}
	})

	t.Run("a missing gateway yields a nil GatewayIP but a non-nil Subnet", func(t *testing.T) {
		// IPSet.ToPb returns nil for a nil receiver while IPNetSet.ToPb
		// returns an empty message; ToPb inherits both behaviors.
		got := (&ENI{ID: "eni-1"}).ToPb()
		if got.GatewayIP != nil {
			t.Errorf("GatewayIP = %v, want nil", got.GatewayIP)
		}
		if got.Subnet == nil {
			t.Fatal("Subnet = nil, want an empty message")
		}
		if got.Subnet.IPv4 != "" || got.Subnet.IPv6 != "" {
			t.Errorf("Subnet = %v, want empty strings", got.Subnet)
		}
	})
}

func TestENI_String(t *testing.T) {
	var nilENI *ENI
	if got := nilENI.String(); got != "" {
		t.Errorf("nil ENI String() = %q, want empty", got)
	}

	e := &ENI{
		ID:        "eni-1",
		Mac:       mustMAC(t, "00:11:22:33:44:55"),
		PrimaryIP: IPSet{IPv4: mustIP(t, "10.0.0.1")},
		Subnet: Subnet{
			ID:         "subnet-1",
			Gateway:    &IPSet{IPv4: mustIP(t, "10.0.0.254")},
			GatewayMac: mustMAC(t, GATEWAY_MAC),
			CIDR:       &IPNetSet{IPv4: mustIPNet(t, "10.0.0.0/24")},
		},
	}
	want := "ID: eni-1, Mac: 00:11:22:33:44:55, PrimaryIP: 10.0.0.1, " +
		"Subnet: ID: subnet-1, Gateway: 10.0.0.254, GatewayMac: 52:54:00:00:00:00, CIDR: 10.0.0.0/24"
	if got := e.String(); got != want {
		t.Errorf("String() =\n%q\nwant\n%q", got, want)
	}
}

func TestSubnet_String(t *testing.T) {
	tests := []struct {
		name   string
		subnet *Subnet
		want   string
	}{
		{name: "nil receiver", subnet: nil, want: ""},
		{
			name:   "zero value renders empty components",
			subnet: &Subnet{},
			want:   "ID: , Gateway: , GatewayMac: , CIDR: ",
		},
		{
			name: "fully populated",
			subnet: &Subnet{
				ID:         "subnet-1",
				Gateway:    &IPSet{IPv4: mustIP(t, "10.0.0.254"), IPv6: mustIP(t, "fd00::ffff")},
				GatewayMac: mustMAC(t, GATEWAY_MAC),
				CIDR: &IPNetSet{
					IPv4: mustIPNet(t, "10.0.0.0/24"),
					IPv6: mustIPNet(t, "fd00::/64"),
				},
			},
			want: "ID: subnet-1, Gateway: 10.0.0.254-fd00::ffff, GatewayMac: 52:54:00:00:00:00, CIDR: 10.0.0.0/24-fd00::/64",
		},
		{
			name:   "empty gateway mac renders empty",
			subnet: &Subnet{ID: "subnet-2", GatewayMac: net.HardwareAddr{}},
			want:   "ID: subnet-2, Gateway: , GatewayMac: , CIDR: ",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.subnet.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestNetResourceSnapshot(t *testing.T) {
	s := &NetResourceSnapshot{
		ResourceItem: ResourceItem{
			Type:   NetResourceTypeEniIp,
			ID:     "eni-1/10.0.0.1",
			ENIId:  "eni-1",
			ENIMac: "00:11:22:33:44:55",
			IPv4:   "10.0.0.1",
		},
		Status: ResStatusInUse,
		Owner:  "default/pod-1",
	}

	// It must satisfy the richer interface, not just NetResource.
	var _ NetResourceStatus = s

	if got := s.GetID(); got != "eni-1/10.0.0.1" {
		t.Errorf("GetID() = %q", got)
	}
	if got := s.GetType(); got != NetResourceTypeEniIp {
		t.Errorf("GetType() = %q", got)
	}
	if got := s.GetStatus(); got != ResStatusInUse {
		t.Errorf("GetStatus() = %q", got)
	}
	if got := s.GetOwner(); got != "default/pod-1" {
		t.Errorf("GetOwner() = %q", got)
	}
	if got := s.GetResourceItem(); !reflect.DeepEqual(got, s.ResourceItem) {
		t.Errorf("GetResourceItem() = %+v, want %+v", got, s.ResourceItem)
	}
}

func TestGetNetResourceAllocatedFromPods(t *testing.T) {
	eniIP := func(eniID, mac, ipv4 string) ResourceItem {
		return ResourceItem{
			Type:   NetResourceTypeEniIp,
			ID:     eniID + "/" + ipv4,
			ENIId:  eniID,
			ENIMac: mac,
			IPv4:   ipv4,
		}
	}

	t.Run("no pods yields an empty map", func(t *testing.T) {
		got := GetNetResourceAllocatedFromPods(nil)
		if got == nil {
			t.Fatal("got nil, want an empty map")
		}
		if len(got) != 0 {
			t.Errorf("got %v, want an empty map", got)
		}
	})

	t.Run("a pod with no resources contributes nothing", func(t *testing.T) {
		got := GetNetResourceAllocatedFromPods([]*Pod{{Namespace: "default", Name: "p"}})
		if len(got) != 0 {
			t.Errorf("got %v, want an empty map", got)
		}
	})

	t.Run("resources are indexed by type then id and owned by their pod", func(t *testing.T) {
		pods := []*Pod{
			{
				Namespace: "default",
				Name:      "pod-a",
				Resources: []ResourceItem{
					eniIP("eni-1", "00:11:22:33:44:55", "10.0.0.1"),
					eniIP("eni-1", "00:11:22:33:44:55", "10.0.0.2"),
				},
			},
			{
				Namespace: "kube-system",
				Name:      "pod-b",
				Resources: []ResourceItem{
					eniIP("eni-2", "00:11:22:33:44:66", "10.0.1.1"),
				},
			},
		}
		got := GetNetResourceAllocatedFromPods(pods)

		byType, ok := got[NetResourceTypeEniIp]
		if !ok {
			t.Fatalf("missing %q bucket in %v", NetResourceTypeEniIp, got)
		}
		if len(byType) != 3 {
			t.Fatalf("got %d entries, want 3: %v", len(byType), byType)
		}
		wantOwners := map[string]string{
			"eni-1/10.0.0.1": "default/pod-a",
			"eni-1/10.0.0.2": "default/pod-a",
			"eni-2/10.0.1.1": "kube-system/pod-b",
		}
		for id, wantOwner := range wantOwners {
			alloc, ok := byType[id]
			if !ok {
				t.Errorf("missing entry %q", id)
				continue
			}
			if alloc.Owner != wantOwner {
				t.Errorf("entry %q owner = %q, want %q", id, alloc.Owner, wantOwner)
			}
			if alloc.Resource == nil {
				t.Errorf("entry %q has a nil resource", id)
			}
		}
	})

	t.Run("the last pod wins when two pods claim the same resource id", func(t *testing.T) {
		res := eniIP("eni-1", "00:11:22:33:44:55", "10.0.0.1")
		pods := []*Pod{
			{Namespace: "default", Name: "first", Resources: []ResourceItem{res}},
			{Namespace: "default", Name: "second", Resources: []ResourceItem{res}},
		}
		got := GetNetResourceAllocatedFromPods(pods)
		if owner := got[NetResourceTypeEniIp]["eni-1/10.0.0.1"].Owner; owner != "default/second" {
			t.Errorf("owner = %q, want default/second", owner)
		}
	})

	t.Run("unconvertible resources are skipped but still create their bucket", func(t *testing.T) {
		// The type bucket is created before ToNetResource is consulted, so a
		// resource that cannot be converted leaves an empty map behind.
		pods := []*Pod{{
			Namespace: "default",
			Name:      "pod-a",
			Resources: []ResourceItem{
				{Type: NetResourceTypeEni, ID: "eni-1", ENIId: "eni-1", ENIMac: "00:11:22:33:44:55"},
				{Type: NetResourceTypeEniIp, ID: "bad", ENIId: "eni-2", ENIMac: "bogus"},
			},
		}}
		got := GetNetResourceAllocatedFromPods(pods)

		bucket, ok := got[NetResourceTypeEni]
		if !ok {
			t.Errorf("expected an (empty) %q bucket, got %v", NetResourceTypeEni, got)
		}
		if len(bucket) != 0 {
			t.Errorf("%q bucket = %v, want empty", NetResourceTypeEni, bucket)
		}
		if len(got[NetResourceTypeEniIp]) != 0 {
			t.Errorf("%q bucket = %v, want empty", NetResourceTypeEniIp, got[NetResourceTypeEniIp])
		}
	})
}
