// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"errors"
	"net"
	"sync"
	"testing"

	helper "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/factory"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

// prefixAPI answers the prefix assign and counts how often it was asked.
type prefixAPI struct {
	helper.ZecAPI
	v6Net     *net.IPNet
	primaryV6 net.IP
	err       error
	calls     int
}

func (p *prefixAPI) GetOrAssignIPv6Block(string, string) (*net.IPNet, net.IP, error) {
	p.calls++
	if p.err != nil {
		return nil, nil, p.err
	}
	return p.v6Net, p.primaryV6, nil
}

func prefixENI(t *testing.T, api *prefixAPI, existing *net.IPNet) *ENI {
	t.Helper()
	return &ENI{
		Mutex:   sync.Mutex{},
		ENI:     &zecTypes.ENI{ID: "eni-1", Mac: net.HardwareAddr{0xaa, 0, 0, 0, 0, 1}, IPv6Net: existing},
		ips:     []*ENIIPRes{},
		zecApi:  api,
		ipStack: "dual",
	}
}

func mustNet(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("parse %s: %v", s, err)
	}
	return n
}

// TestEnsureIPv6PrefixRepairsAnAdoptedInterface is the regression this covers.
//
// GC adopts an interface into the factory and starts a worker for it, but never
// asks for an IPv6 prefix -- factory.GC runs with the pool paused and must not
// add a platform round trip. Without a repair at the point of use, every order
// routed to that interface failed on a nil prefix for the life of the process,
// and nothing stopped the routing: the forbidAssign switch in worker() reacts to
// platform error codes only, and this one is local.
func TestEnsureIPv6PrefixRepairsAnAdoptedInterface(t *testing.T) {
	api := &prefixAPI{
		v6Net:     mustNet(t, "2001:db8:0:1::/96"),
		primaryV6: net.ParseIP("2001:db8:0:1::1"),
	}
	e := prefixENI(t, api, nil)

	if err := e.ensureIPv6Prefix(); err != nil {
		t.Fatalf("ensureIPv6Prefix() = %v, want nil", err)
	}

	if e.IPv6Net == nil || e.IPv6Net.String() != "2001:db8:0:1::/96" {
		t.Errorf("prefix = %v, want 2001:db8:0:1::/96", e.IPv6Net)
	}
	// The primary is not optional: it falls inside the prefix, and the carve
	// excludes it. Handing out the address the interface already answers on
	// black-holes the pod undetectably.
	if e.PrimaryIP.IPv6 == nil || !e.PrimaryIP.IPv6.Equal(net.ParseIP("2001:db8:0:1::1")) {
		t.Errorf("primary IPv6 = %v, want it recorded with the prefix", e.PrimaryIP.IPv6)
	}
}

// TestEnsureIPv6PrefixIsANoOpWhenOneIsPresent keeps the repair off the hot
// path: worker() calls this before every carve, so an interface that already
// has a prefix must not pay for a platform round trip each time.
func TestEnsureIPv6PrefixIsANoOpWhenOneIsPresent(t *testing.T) {
	api := &prefixAPI{v6Net: mustNet(t, "2001:db8:0:2::/96")}
	have := mustNet(t, "2001:db8:0:1::/96")
	e := prefixENI(t, api, have)

	if err := e.ensureIPv6Prefix(); err != nil {
		t.Fatalf("ensureIPv6Prefix() = %v, want nil", err)
	}

	if api.calls != 0 {
		t.Errorf("asked the platform %d times for a prefix the interface already has, want 0", api.calls)
	}
	if e.IPv6Net.String() != "2001:db8:0:1::/96" {
		t.Errorf("prefix = %v, want the existing one left alone", e.IPv6Net)
	}
}

// TestEnsureIPv6PrefixReturnsAnUnwrappableError pins the error chain. receiveRes
// reads the platform code off this to decide whether to refresh the limit and
// wake GC -- a subnet that cannot carry IPv6 answers the assign underneath with
// INVALID_SUBNET_IPSTACK_NOT_SUPPORT, and a %v here would leave that code
// visible to a human and unreachable to errors.As.
func TestEnsureIPv6PrefixReturnsAnUnwrappableError(t *testing.T) {
	sentinel := errors.New("subnet does not support ipv6")
	api := &prefixAPI{err: sentinel}
	e := prefixENI(t, api, nil)

	err := e.ensureIPv6Prefix()
	if err == nil {
		t.Fatal("ensureIPv6Prefix() = nil although the platform refused")
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("errors.Is could not find the platform error in %q", err)
	}
	if e.IPv6Net != nil {
		t.Error("a failed assign left a prefix behind")
	}
}
