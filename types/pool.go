// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"net"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/rpc"
)

// GATEWAY_MAC is reported to the plugin in every ENIInfo, but nothing reads it
// back: the datapath does not resolve the gateway's hardware address at all. It
// installs a permanent neighbor entry pointing at the host end of the pod's
// veth pair, so no ARP or NDP exchange ever happens and this value never has to
// match anything real. It is kept because the field is part of the wire format.
const (
	GATEWAY_MAC = "52:54:00:00:00:00"
)

// Resource types as they are persisted in the record store. Changing either
// string orphans every record already on disk, so they are effectively part of
// the on-disk format rather than internal names.
const (
	NetResourceTypeEni   = "eni"
	NetResourceTypeEniIp = "eniIp"
)

// ResourceItem is the persisted form of a VPC network resource.
type ResourceItem struct {
	Type string `json:"type"`
	ID   string `json:"id"`

	ENIId  string `json:"eni_id"`
	ENIMac string `json:"eni_mac"`
	IPv4   string `json:"ipv4,omitempty"`
	IPv6   string `json:"ipv6,omitempty"`
}

// NetResource is anything the pool can hand out and the record store can
// persist. GetID must be stable across a daemon restart and unique within the
// node -- it is the key the persisted record is looked up by, so an ID that is
// derived differently after an upgrade orphans every record on disk.
type NetResource interface {
	GetID() string
	GetType() string
	GetResourceItem() ResourceItem
}

func (r *ResourceItem) ToNetResource() (res NetResource) {
	mac, err := net.ParseMAC(r.ENIMac)
	if err != nil {
		return
	}
	switch r.Type {
	case NetResourceTypeEniIp:
		res = &ENIIP{
			ENI: &ENI{
				ID:        r.ENIId,
				Mac:       mac,
				PrimaryIP: IPSet{},
				Subnet:    Subnet{},
			},
			IPSet: IPSet{
				IPv4: net.ParseIP(r.IPv4),
				IPv6: net.ParseIP(r.IPv6),
			},
		}
	}
	return res
}

type Subnet struct {
	ID         string
	Gateway    *IPSet
	GatewayMac net.HardwareAddr
	CIDR       *IPNetSet
}

func (s *Subnet) String() string {
	if s == nil {
		return ""
	}
	return fmt.Sprintf("ID: %s, Gateway: %s, GatewayMac: %s, CIDR: %s",
		s.ID, s.Gateway.String(), s.GatewayMac.String(), s.CIDR.String())
}

// ENI describes one network interface attached to this instance.
type ENI struct {
	ID        string
	Mac       net.HardwareAddr
	PrimaryIP IPSet
	// PrimaryIPv6Disputed is metadata's answer for this interface's primary
	// IPv6 when it does not match the describe answer already in
	// PrimaryIP.IPv6. Nil in the normal case, where the two agree.
	//
	// AllocateIPv6AddressLocked excludes both from the carve: there is no way
	// to tell which one DHCPv6 actually put on the interface, and handing out
	// the one that is really there black-holes the pod that receives it. The
	// host's local table matches the address before the to-pod rule runs, and
	// no reconcile can find it afterwards because the platform publishes no
	// per-address IPv6 view. Withholding one extra address out of a /96 costs
	// nothing.
	//
	// Written unlocked and read under two unrelated locks. Benign only because
	// a primary IPv6 does not change: every write after the first stores the
	// same value. A mutable primary would make this a real race.
	PrimaryIPv6Disputed net.IP
	IPv6Net             *net.IPNet
	Subnet              Subnet
	SecurityGroupID     string
	// IsPrimary marks the instance's own network interface, the one holding the
	// node address. It is never detached, and the addresses the interface
	// itself holds as primary are never handed to a pod. Its remaining
	// addresses are pooled like any other interface's, including ones this
	// daemon has no record of.
	IsPrimary bool
}

func (e *ENI) GetID() string {
	if e == nil {
		return ""
	}
	return e.ID
}

func (e *ENI) GetType() string {
	return NetResourceTypeEni
}

func (e *ENI) GetResourceItem() ResourceItem {
	return ResourceItem{
		Type:   NetResourceTypeEni,
		ID:     e.GetID(),
		ENIId:  e.ID,
		ENIMac: e.Mac.String(),
		IPv4:   e.PrimaryIP.GetIPv4(),
		IPv6:   e.PrimaryIP.GetIPv6(),
	}
}

func (e *ENI) String() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("ID: %s, Mac: %s, PrimaryIP: %s, Subnet: %s",
		e.ID, e.Mac.String(), e.PrimaryIP.String(), e.Subnet.String())
}

func (e *ENI) ToPb() *rpc.ENIInfo {
	if e == nil {
		return nil
	}
	var mac string
	if e.Mac != nil {
		mac = e.Mac.String()
	}

	return &rpc.ENIInfo{
		ID:         e.ID,
		MAC:        mac,
		GatewayMac: GATEWAY_MAC,
		GatewayIP:  e.Subnet.Gateway.ToPb(),
		Subnet:     e.Subnet.CIDR.ToPb(),
		PrimaryIP:  e.PrimaryIP.ToPb(),
		IsPrimary:  e.IsPrimary,
	}
}

// ENIIP is one allocation: a set of addresses on a particular network
// interface, covering every family the node serves. It is the unit the pool
// deals in, and it is indivisible -- an IPv4 and its IPv6 counterpart are
// allocated, handed out and released together, never separately.
type ENIIP struct {
	ENI   *ENI
	IPSet IPSet
}

// GetID returns the interface id and the address set joined by a slash, so
// "<eniID>/<ipv4>-<ipv6>" for a dual-stack allocation and "<eniID>/<ipv4>" for a
// single-stack one -- IPSet.String only emits the separator when both families
// are present, so the shape of the key depends on the node's stack.
//
// Both halves are in the key because neither alone identifies the allocation:
// the same address may be re-assigned to a different interface, and one
// interface holds many. That the address set is part of the key is also why an
// allocation cannot be indexed by a single address -- any scheme that tries to
// will disagree with the record store.
func (e *ENIIP) GetID() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s/%s", e.ENI.GetID(), e.IPSet.String())
}

func (e *ENIIP) GetType() string {
	return NetResourceTypeEniIp
}

func (e *ENIIP) GetResourceItem() ResourceItem {
	return ResourceItem{
		Type:   e.GetType(),
		ID:     e.GetID(),
		ENIId:  e.ENI.ID,
		ENIMac: e.ENI.Mac.String(),
		IPv4:   e.IPSet.GetIPv4(),
		IPv6:   e.IPSet.GetIPv6(),
	}
}

type NetResourceAllocated struct {
	Owner    string
	Resource NetResource
}

func GetNetResourceAllocatedFromPods(pods []*Pod) map[string]map[string]NetResourceAllocated {
	result := map[string]map[string]NetResourceAllocated{}
	for _, pod := range pods {
		for _, res := range pod.Resources {
			if result[res.Type] == nil {
				result[res.Type] = make(map[string]NetResourceAllocated)
			}
			netRes := res.ToNetResource()
			if netRes == nil {
				continue
			}
			result[res.Type][res.ID] = NetResourceAllocated{
				Owner:    PodKey(pod.Namespace, pod.Name),
				Resource: netRes,
			}
		}
	}
	return result
}

// ResStatus labels a resource in two different reports, and only some values
// are meaningful in each. Reading it as one flat state machine is a mistake.
//
// A factory's List groups what the node actually holds:
//
//	Normal   usable, fully formed
//	Invalid  should be released
//	Legacy   incomplete -- missing an address family the node serves, so it is
//	         never handed to a pod, but it is left alone while a pod still has it
//
// A pool's snapshot reports where a resource sits inside the pool:
//
//	InUse     held by a pod
//	Available idle, ready to hand out
//	Invalid   queued for release
//	NotAdded  the factory reported it, the pool has no record of it
//
// Invalid and Legacy appear in both and mean the same thing in each. Normal
// never appears in a pool snapshot, and the other three never appear in a
// factory listing.
type ResStatus string

const (
	ResStatusInUse     ResStatus = "InUse"
	ResStatusAvailable ResStatus = "Available"
	ResStatusInvalid   ResStatus = "Invalid"
	ResStatusNotAdded  ResStatus = "NotAdded"
	ResStatusNormal    ResStatus = "Normal"
	ResStatusLegacy    ResStatus = "Legacy"
)

type NetResourceStatus interface {
	NetResource
	GetStatus() ResStatus
	GetOwner() string
}

type NetResourceSnapshot struct {
	ResourceItem
	Status ResStatus `json:"status"`
	Owner  string    `json:"owner,omitempty"`
}

func (m *NetResourceSnapshot) GetID() string {
	return m.ID
}

func (m *NetResourceSnapshot) GetType() string {
	return m.Type
}

func (m *NetResourceSnapshot) GetResourceItem() ResourceItem {
	return m.ResourceItem
}

func (m *NetResourceSnapshot) GetStatus() ResStatus {
	return m.Status
}

func (m *NetResourceSnapshot) GetOwner() string {
	return m.Owner
}
