// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

//

// Package types defines the resource model shared by the daemon, the plugin and
// the persistence layer.
//
// The central idea is that an allocation is one indivisible object covering
// every address family the node serves, not one object per address. IPSet and
// its Complete check exist to enforce that: a resource missing a family is never
// handed to a pod.
package types

import (
	"fmt"
	"net"
	"strings"

	cniTypes "github.com/containernetworking/cni/pkg/types"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/rpc"
)

type RuntimeDNS struct {
	Nameservers []string `json:"servers,omitempty"`
	Search      []string `json:"searches,omitempty"`
	Options     []string `json:"options,omitempty"`
}

type RuntimePortMapEntry struct {
	HostPort      int    `json:"hostPort"`
	ContainerPort int    `json:"containerPort"`
	Protocol      string `json:"protocol"`
	HostIP        string `json:"hostIP,omitempty"`
}

// RuntimeBandwidthEntry mirrors the CNI runtimeConfig bandwidth block.
//
// Nothing here reaches the datapath; the struct exists only so that a conflist
// carrying the block still unmarshals. The conflist deliberately does not
// advertise capabilities.bandwidth for that reason -- advertising it would have
// kubelet translate a pod's bandwidth annotations into these fields and report
// success while the pod ran unshaped.
type RuntimeBandwidthEntry struct {
	// IngressRate is in bits per second.
	IngressRate int `json:"ingressRate,omitempty"`
	// IngressBurst is in bits.
	IngressBurst int `json:"ingressBurst,omitempty"`
	// EgressRate is in bits per second.
	EgressRate int `json:"egressRate,omitempty"`
	// EgressBurst is in bits.
	EgressBurst int `json:"egressBurst,omitempty"`
}

type RuntimeConfig struct {
	DNS       RuntimeDNS            `json:"dns"`
	PortMaps  []RuntimePortMapEntry `json:"portMappings,omitempty"`
	Bandwidth RuntimeBandwidthEntry `json:"bandwidth,omitempty"`
}

// CNIConf is this plugin's entry in the conflist, unmarshalled from the
// network configuration kubelet passes on stdin.
type CNIConf struct {
	cniTypes.NetConf

	// HostVethPrefix is the name prefix of the host side of the pod's veth pair.
	HostVethPrefix string `json:"veth_prefix"`

	// ENIIPVirtualType selects the datapath. "veth" (or absent) is the only
	// accepted value; anything else is refused rather than ignored. See
	// getDatePath in pkg/plugin.
	ENIIPVirtualType string `json:"eniip_virtual_type"`

	// MTU is applied to the pod's veth pair and to CNI-created ENIs. The
	// instance's own interface is left alone. 0 means the plugin default.
	MTU int `json:"mtu"`

	RuntimeConfig RuntimeConfig `json:"runtimeConfig"`

	Debug bool `json:"debug"`

	// IPAM is the delegated-IPAM block. When another plugin owns the device --
	// it calls this binary through ipam.type rather than kubelet executing it
	// directly -- the conflist entry this binary is named in is that block, so
	// its own settings have to live there too.
	IPAM IPAMConf `json:"ipam"`
}

// IPAMConf is the conflist ipam block, read only when this binary runs as
// somebody else's delegated IPAM.
type IPAMConf struct {
	// Type is the delegate's name, which is this binary when the mode marker
	// below applies.
	Type string `json:"type"`

	// ZecMode selects what this binary does. Empty means the full plugin.
	ZecMode string `json:"zec_mode"`

	// InstallHostRoutes asks the IPAM-only path to place this pod's per-ENI ip
	// rules and that ENI's route table on the host. A pointer so an absent key
	// is distinguishable from an explicit false; absent means true, because a
	// multi-ENI node with no rules sends every pod out of the instance's own
	// interface with a source address that belongs to another port's MAC, and
	// the fabric drops it.
	InstallHostRoutes *bool `json:"install_host_routes"`
}

// ZecModeIPAMOnly is the only non-empty zec_mode. In this mode the binary
// allocates and releases addresses and places host routing, and creates no
// device: the calling plugin owns the pod's interface.
const ZecModeIPAMOnly = "ipam-only"

// IsIPAMOnly reports whether this invocation must take the IPAM-only path.
//
// The explicit marker is the primary signal. The fallback -- somebody else is
// the top-level plugin and named us as their ipam -- exists because the failure
// it prevents is silent and expensive: taking the full path there would build a
// veth and write pod-side routes underneath a device the caller already
// created, leaving two datapaths for one pod.
func (c *CNIConf) IsIPAMOnly() bool {
	if c == nil {
		return false
	}
	if c.IPAM.ZecMode == ZecModeIPAMOnly {
		return true
	}
	return c.IPAM.Type == pluginName && c.Type != "" && c.Type != pluginName
}

// ValidateIPAM rejects an unrecognised zec_mode rather than falling back to the
// full plugin path.
//
// Same reasoning as an unknown eniip_virtual_type: a typo that silently selects
// the other mode leaves the node running a datapath that does not match its
// configuration, and the symptom -- pods that come up but have two devices, or
// none of the host routing they need -- is far harder to trace than a pod that
// refuses to start.
func (c *CNIConf) ValidateIPAM() error {
	if c == nil {
		return nil
	}
	switch c.IPAM.ZecMode {
	case "", ZecModeIPAMOnly:
		return nil
	default:
		return fmt.Errorf("unsupported zec_mode %q in the conflist ipam block; the only "+
			"accepted value is %q, and an absent value means the full plugin",
			c.IPAM.ZecMode, ZecModeIPAMOnly)
	}
}

// InstallHostRoutes reports whether the IPAM-only path should place host
// routing. See IPAMConf.InstallHostRoutes for why absent means true.
func (c *CNIConf) InstallHostRoutes() bool {
	if c == nil || c.IPAM.InstallHostRoutes == nil {
		return true
	}
	return *c.IPAM.InstallHostRoutes
}

// pluginName is this binary's name as it appears in a conflist, both as a
// top-level type and as an ipam.type.
const pluginName = "zec-cni"

// K8SArgs is the CNI_ARGS block kubelet passes, naming the pod being set up.
type K8SArgs struct {
	cniTypes.CommonArgs
	IP                         net.IP
	K8S_POD_NAME               cniTypes.UnmarshallableString // nolint
	K8S_POD_NAMESPACE          cniTypes.UnmarshallableString // nolint
	K8S_POD_INFRA_CONTAINER_ID cniTypes.UnmarshallableString // nolint
}

// IPFamily is one of the two address families a pod can be given. It lets code
// that does the same thing for every configured family loop over them instead
// of branching on the stack mode. Most of the daemon still branches; prefer the
// loop in new code.
type IPFamily string

const (
	IPFamilyV4 IPFamily = "ipv4"
	IPFamilyV6 IPFamily = "ipv6"
)

type IPNetSet struct {
	IPv4 *net.IPNet
	IPv6 *net.IPNet
}

func (i *IPNetSet) String() string {
	if i == nil {
		return ""
	}
	var result []string
	if i.IPv4 != nil {
		result = append(result, i.IPv4.String())
	}
	if i.IPv6 != nil {
		result = append(result, i.IPv6.String())
	}
	return strings.Join(result, "-")
}

func (i *IPNetSet) ToPb() *rpc.IPSet {
	var ipv4, ipv6 string
	if i != nil && i.IPv4 != nil {
		ipv4 = i.IPv4.String()
	}
	if i != nil && i.IPv6 != nil {
		ipv6 = i.IPv6.String()
	}
	return &rpc.IPSet{
		IPv4: ipv4,
		IPv6: ipv6,
	}
}

func (i *IPNetSet) SetIPNet(str string) *IPNetSet {
	ip, ipNet, err := net.ParseCIDR(str)
	if err != nil {
		return i
	}
	if ip == nil {
		return i
	}
	// IPv6
	if ip.To4() == nil {
		i.IPv6 = ipNet
		return i
	}
	i.IPv4 = ipNet
	return i
}

// IPSet holds one bare address per family (no mask; see IPNetSet for that).
// Either slot may be nil, so use Complete to check a set against the families
// the node is configured for.
type IPSet struct {
	IPv4 net.IP
	IPv6 net.IP
}

func (s *IPSet) String() string {
	if s == nil {
		return ""
	}
	var result []string
	if s.IPv4 != nil {
		result = append(result, s.IPv4.String())
	}
	if s.IPv6 != nil {
		result = append(result, s.IPv6.String())
	}
	return strings.Join(result, "-")
}

// Get returns the bare address for one family, or nil if this set has none.
// Any family other than IPFamilyV6 is read as IPv4.
func (s *IPSet) Get(f IPFamily) net.IP {
	if s == nil {
		return nil
	}
	if f == IPFamilyV6 {
		return s.IPv6
	}
	return s.IPv4
}

// Complete reports whether this set carries an address for every family given.
//
// It is how a caller asks whether a resource covers the families the node is
// configured for. The daemon uses it to drop a half-paired address instead of
// handing it to a pod.
func (s *IPSet) Complete(families []IPFamily) bool {
	for _, f := range families {
		if s.Get(f) == nil {
			return false
		}
	}
	return true
}

func (s *IPSet) GetIPv4() string {
	if s != nil && s.IPv4 != nil {
		return s.IPv4.String()
	}
	return ""
}

func (s *IPSet) GetIPv6() string {
	if s != nil && s.IPv6 != nil {
		return s.IPv6.String()
	}
	return ""
}

func (s *IPSet) ToPb() *rpc.IPSet {
	if s == nil {
		return nil
	}
	var ipv4, ipv6 string
	if s.IPv4 != nil {
		ipv4 = s.IPv4.String()
	}
	if s.IPv6 != nil {
		ipv6 = s.IPv6.String()
	}
	return &rpc.IPSet{
		IPv4: ipv4,
		IPv6: ipv6,
	}
}

func (s *IPSet) ToPbWithMask(cidr *IPNetSet) *rpc.IPSet {
	if s == nil {
		return nil
	}
	var ipv4, ipv6 string
	if s.IPv4 != nil {
		t := &net.IPNet{
			IP: s.IPv4,
		}
		if cidr != nil && cidr.IPv4 != nil {
			t.Mask = cidr.IPv4.Mask
		} else {
			t.Mask = net.CIDRMask(32, 32)
		}
		ipv4 = t.String()
	}
	if s.IPv6 != nil {
		t := &net.IPNet{
			IP: s.IPv6,
		}
		if cidr != nil && cidr.IPv6 != nil {
			t.Mask = cidr.IPv6.Mask
		} else {
			t.Mask = net.CIDRMask(128, 128)
		}
		ipv6 = t.String()
	}
	return &rpc.IPSet{
		IPv4: ipv4,
		IPv6: ipv6,
	}
}

// PairIPs zips ipv4s and ipv6s into IPSets by index. The result is as long as
// the longer input, so unequal lengths leave a half-filled tail; IPSet.Complete
// is the check for those.
func PairIPs(ipv4s, ipv6s []net.IP) []IPSet {
	result := make([]IPSet, max(len(ipv4s), len(ipv6s)))
	for i, ip := range ipv4s {
		result[i].IPv4 = ip
	}
	for i, ip := range ipv6s {
		result[i].IPv6 = ip
	}
	return result
}
