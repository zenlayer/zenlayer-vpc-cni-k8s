// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"sort"
	"strings"

	cniTypes "github.com/containernetworking/cni/pkg/types"
	"github.com/containernetworking/plugins/pkg/ns"
	"github.com/gdexlab/go-render/render"

	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// DataPathType identifies how a pod is wired to its ENI in the kernel.
//
// This is the device model only. Whether an eBPF layer (Cilium via CNI
// chaining) sits on top is a separate choice: it turns the conflist into a
// chain and adds an agent, but the device this plugin creates is the same.
// Folding "with Cilium" into this enum would double the values here for every
// device model added.
type DataPathType int

const (
	// DataPathUnknown is the zero value on purpose, so a DataPathType that was
	// never set can never be mistaken for a usable datapath. A caller that
	// ignores the error from ParseDataPath gets something that fails loudly
	// rather than a silent default.
	DataPathUnknown DataPathType = iota

	// PolicyRoute connects the pod with a veth pair and steers traffic with
	// per-ENI route tables.
	PolicyRoute

	// Add a new device model here, give it a name in dataPathNames below, and
	// register a driver for it in pkg/plugin/datapath. Forgetting the driver
	// does not break the build, so a test in that package walks AllDataPaths()
	// and fails if any of them has no driver.
)

// dataPathNames is the single source of truth for the values accepted in the
// conflist's eniip_virtual_type: String, ParseDataPath and
// AcceptedDataPathNames all derive from it, so a new datapath cannot be
// half-added here.
//
// The chart's zec.dataPath enum is a separate list maintained by hand and is
// not checked against this one -- adding a datapath means editing both.
var dataPathNames = map[DataPathType]string{
	PolicyRoute: "veth",
}

// defaultDataPath is what an absent eniip_virtual_type means. It is spelled out
// rather than left to the zero value so that adding a datapath cannot silently
// change the default.
const defaultDataPath = PolicyRoute

// String returns the conflist name of d, or an "unknown" marker for a value
// that has none.
func (d DataPathType) String() string {
	if name, ok := dataPathNames[d]; ok {
		return name
	}
	if d == DataPathUnknown {
		return "unknown"
	}
	return fmt.Sprintf("unknown(%d)", int(d))
}

// ParseDataPath maps a conflist eniip_virtual_type onto a datapath.
//
// An empty value selects defaultDataPath; anything unrecognized is an error
// rather than a fallback. A node quietly running a datapath other than the
// configured one is far harder to diagnose than a pod that will not start.
func ParseDataPath(name string) (DataPathType, error) {
	if name == "" {
		return defaultDataPath, nil
	}
	for dp, dpName := range dataPathNames {
		if strings.EqualFold(name, dpName) {
			return dp, nil
		}
	}
	return DataPathUnknown, fmt.Errorf("unsupported eniip_virtual_type %q, want one of %s",
		name, strings.Join(AcceptedDataPathNames(), ", "))
}

// AllDataPaths returns every usable datapath, in a stable order. DataPathUnknown
// is not one of them.
func AllDataPaths() []DataPathType {
	all := make([]DataPathType, 0, len(dataPathNames))
	for dp := range dataPathNames {
		all = append(all, dp)
	}
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	return all
}

// AcceptedDataPathNames returns the conflist values that ParseDataPath accepts,
// in a stable order. It is what ParseDataPath lists in its error.
func AcceptedDataPathNames() []string {
	names := make([]string, 0, len(dataPathNames))
	for _, dp := range AllDataPaths() {
		names = append(names, dataPathNames[dp])
	}
	return names
}

type SetupConfig struct {
	DP DataPathType

	Netns ns.NetNS

	HostVETHName string

	ContainerIfName string
	ContainerIPNet  *zecTypes.IPNetSet
	GatewayIP       *zecTypes.IPSet
	MTU             int
	ENIIndex        int
	ENIGatewayIP    *zecTypes.IPSet
	// ENIPrimaryIP is the address the platform bound to this interface's own
	// port. eniAddrs configures it on the interface; it is never a pod's.
	ENIPrimaryIP *zecTypes.IPSet
	// ENIIsPrimary marks the instance's own interface. Its addresses belong to
	// whatever configures the guest -- DHCP here -- and the datapath must not
	// touch them.
	ENIIsPrimary bool

	DefaultRoute bool
	MultiNetwork bool

	// add extra route in container
	ExtraRoutes []cniTypes.Route

	RuntimeConfig zecTypes.RuntimeConfig
}

func (c *SetupConfig) String() string {
	if c == nil {
		return ""
	}
	return render.AsCode(c)
}

type TeardownCfg struct {
	DP DataPathType

	Netns ns.NetNS

	HostVETHName string

	ENIIndex int

	ContainerIfName string
	ContainerIPNet  *zecTypes.IPNetSet
}

func (c *TeardownCfg) String() string {
	if c == nil {
		return ""
	}
	return render.AsCode(c)
}
