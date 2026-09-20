// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"testing"

	daemonTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/metadata"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

const (
	testMac    = "aa:bb:cc:dd:ee:01"
	metaV4Path = "network/interfaces/macs/%s/primary-ip-address"
	metaV6Path = "network/interfaces/macs/%s/primary-ipv6-address"
	metaV4s    = "network/interfaces/macs/%s/private-ipv4s"
	metaV6Pfx  = "network/interfaces/macs/%s/ipv6-prefix"
)

// pathMeta answers metadata reads from a table. An absent path answers 404,
// which is what the real service returns for a key that does not apply here --
// so a test only has to name the keys it cares about.
type pathMeta struct {
	vals map[string]string
	errs map[string]error
}

func (m pathMeta) GetMetadata(_ context.Context, path string) (string, error) {
	if err, ok := m.errs[path]; ok {
		return "", err
	}
	if v, ok := m.vals[path]; ok {
		return v, nil
	}
	return "", &metadata.StatusError{StatusCode: http.StatusNotFound, Body: "not found"}
}

func p(tmpl string) string { return fmt.Sprintf(tmpl, testMac) }

// dualClient builds a dual-stack client whose only live dependency is metadata.
func dualClient(meta pathMeta) *ZecAPIImpl {
	return &ZecAPIImpl{
		IPStack:     daemonTypes.IPStackDual,
		metadataSvc: metadata.NewEC2MetadataWrapper(meta),
	}
}

func testENI(t *testing.T, describeV4, describeV6 string) *zecTypes.ENI {
	t.Helper()
	mac, err := net.ParseMAC(testMac)
	if err != nil {
		t.Fatalf("parse mac: %v", err)
	}
	eni := &zecTypes.ENI{ID: "eni-under-test", Mac: mac}
	if describeV4 != "" {
		eni.PrimaryIP.IPv4 = net.ParseIP(describeV4)
	}
	if describeV6 != "" {
		eni.PrimaryIP.IPv6 = net.ParseIP(describeV6)
	}
	return eni
}

// TestGetENIAllocatableIPsWithholdsBothPrimariesOnDisagreement checks that
// neither candidate primary reaches the pool when the two sources differ.
//
// Two sources name an interface's primary address: the metadata service, whose
// answer leads the list GetENIIPList returns, and the platform's describe,
// which fills PrimaryIP. When they differ there is no way to tell which address
// the interface actually holds, so both are withheld rather than one being
// picked -- a pod given an address the interface does not hold shows up as
// dropped traffic, not as a misconfiguration.
func TestGetENIAllocatableIPsWithholdsBothPrimariesOnDisagreement(t *testing.T) {
	t.Run("IPv4 sources disagree: neither is allocatable", func(t *testing.T) {
		e := dualClient(pathMeta{vals: map[string]string{
			p(metaV4Path): "10.0.0.4", // metadata's answer
			p(metaV4s):    `["10.0.0.7","10.0.0.8"]`,
			p(metaV6Pfx):  "2001:db8:0:1::/96",
			p(metaV6Path): "2001:db8:0:1::2",
		}})
		eni := testENI(t, "10.0.0.5", "2001:db8:0:1::2") // describe disagrees on v4

		got, _, err := e.GetENIAllocatableIPs(eni)
		if err != nil {
			t.Fatalf("GetENIAllocatableIPs: %v", err)
		}
		for _, withheld := range []string{"10.0.0.4", "10.0.0.5"} {
			for _, ip := range got {
				if ip.Equal(net.ParseIP(withheld)) {
					t.Errorf("%s is allocatable, but the two sources disagree on the primary "+
						"so neither candidate may be handed to a pod; got %v", withheld, got)
				}
			}
		}
		if len(got) != 2 {
			t.Errorf("allocatable = %v, want the two addresses no source called primary", got)
		}
	})

	t.Run("IPv6 sources disagree: the metadata answer is recorded as disputed", func(t *testing.T) {
		e := dualClient(pathMeta{vals: map[string]string{
			p(metaV4Path): "10.0.0.5",
			p(metaV4s):    `["10.0.0.7"]`,
			p(metaV6Pfx):  "2001:db8:0:1::/96",
			p(metaV6Path): "2001:db8:0:1::9", // metadata says ::9
		}})
		eni := testENI(t, "10.0.0.5", "2001:db8:0:1::2") // describe says ::2

		if _, _, err := e.GetENIAllocatableIPs(eni); err != nil {
			t.Fatalf("GetENIAllocatableIPs: %v", err)
		}
		// Recording it is the whole mechanism: the carve excludes PrimaryIP.IPv6
		// and PrimaryIPv6Disputed both, and nothing else can notice the clash --
		// the platform publishes no per-address IPv6 view to compare against.
		if eni.PrimaryIPv6Disputed == nil || !eni.PrimaryIPv6Disputed.Equal(net.ParseIP("2001:db8:0:1::9")) {
			t.Errorf("PrimaryIPv6Disputed = %v, want 2001:db8:0:1::9 so the carve skips it too",
				eni.PrimaryIPv6Disputed)
		}
	})

	t.Run("IPv6 sources agree: nothing is disputed", func(t *testing.T) {
		e := dualClient(pathMeta{vals: map[string]string{
			p(metaV4Path): "10.0.0.5",
			p(metaV4s):    `["10.0.0.7"]`,
			p(metaV6Pfx):  "2001:db8:0:1::/96",
			p(metaV6Path): "2001:db8:0:1::2",
		}})
		eni := testENI(t, "10.0.0.5", "2001:db8:0:1::2")
		eni.PrimaryIPv6Disputed = net.ParseIP("2001:db8:0:1::ff") // stale from a previous round

		if _, _, err := e.GetENIAllocatableIPs(eni); err != nil {
			t.Fatalf("GetENIAllocatableIPs: %v", err)
		}
		if eni.PrimaryIPv6Disputed != nil {
			t.Errorf("PrimaryIPv6Disputed = %v after the sources agreed, want nil: a dispute that "+
				"is never cleared keeps an address out of the prefix for the life of the interface",
				eni.PrimaryIPv6Disputed)
		}
	})

	// Metadata reported 2001:2:0:7775:0:6::2 while the daemon logged
	// 2001:2:0:7775:0:6:0:2 -- one address, two legal renderings.
	//
	// Both sides are parsed before they meet, so net.IP.Equal and a .String()
	// comparison behave the same here and this pins neither. What it guards is
	// a regression to comparing the metadata service's raw text against a
	// rendered address.
	t.Run("the same address written two ways is not a disagreement", func(t *testing.T) {
		e := dualClient(pathMeta{vals: map[string]string{
			p(metaV4Path): "10.0.0.5",
			p(metaV4s):    `["10.0.0.7"]`,
			p(metaV6Pfx):  "2001:2:0:7775:0:6::/96",
			p(metaV6Path): "2001:2:0:7775:0:6::2",
		}})
		eni := testENI(t, "10.0.0.5", "2001:2:0:7775:0:6:0:2")

		if _, _, err := e.GetENIAllocatableIPs(eni); err != nil {
			t.Fatalf("GetENIAllocatableIPs: %v", err)
		}
		if eni.PrimaryIPv6Disputed != nil {
			t.Errorf("PrimaryIPv6Disputed = %v: %q and %q are one address written two ways, so "+
				"something on the way here is comparing the metadata service's raw text instead "+
				"of the address it parses to",
				eni.PrimaryIPv6Disputed, "2001:2:0:7775:0:6::2", "2001:2:0:7775:0:6:0:2")
		}
	})
}

// TestGetENIAllocatableIPsToleratesNoSecondOpinion covers the metadata read
// failing rather than disagreeing.
//
// Interfaces that never had IPv6 enabled answer 404 for primary-ipv6-address
// just as they do for ipv6-prefix. "No second opinion" must leave the describe
// answer in force -- treating it as a dispute would withhold a primary the
// interface really does hold, and treating it as an error would take the
// interface out of service over a key that was never going to answer.
func TestGetENIAllocatableIPsToleratesNoSecondOpinion(t *testing.T) {
	for _, tc := range []struct {
		name string
		errs map[string]error
	}{
		{"404 for the primary ipv6", nil}, // absent from the table -> 404
		{"the read fails outright", map[string]error{
			p(metaV6Path): errors.New("connection refused"),
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			vals := map[string]string{
				p(metaV4Path): "10.0.0.5",
				p(metaV4s):    `["10.0.0.7"]`,
				p(metaV6Pfx):  "2001:db8:0:1::/96",
			}
			if tc.errs != nil {
				vals[p(metaV6Path)] = "2001:db8:0:1::2"
			}
			e := dualClient(pathMeta{vals: vals, errs: tc.errs})
			eni := testENI(t, "10.0.0.5", "2001:db8:0:1::2")

			got, v6Net, err := e.GetENIAllocatableIPs(eni)
			if err != nil {
				t.Fatalf("GetENIAllocatableIPs: %v -- a missing second opinion must not take "+
					"the interface out of service", err)
			}
			if eni.PrimaryIPv6Disputed != nil {
				t.Errorf("PrimaryIPv6Disputed = %v, want nil: a failed read is not a disagreement",
					eni.PrimaryIPv6Disputed)
			}
			if v6Net == nil {
				t.Errorf("v6Net = nil, want the prefix to survive a missing primary-ipv6 read")
			}
			if len(got) != 1 || !got[0].Equal(net.ParseIP("10.0.0.7")) {
				t.Errorf("allocatable = %v, want [10.0.0.7]", got)
			}
		})
	}
}

// TestGetENIAllocatableIPsRefusesAPrefixWithNoPrimaryIPv6 pins the refusal that
// guards the IPv6 carve.
//
// This platform puts an interface's primary IPv6 inside the prefix pod
// addresses are carved from. Without knowing where it sits, the carve can hand
// a pod the address the interface itself holds, and nothing detects that
// afterwards. Refusing to build a pool is the recoverable outcome.
func TestGetENIAllocatableIPsRefusesAPrefixWithNoPrimaryIPv6(t *testing.T) {
	e := dualClient(pathMeta{vals: map[string]string{
		p(metaV4Path): "10.0.0.5",
		p(metaV4s):    `["10.0.0.7"]`,
		p(metaV6Pfx):  "2001:db8:0:1::/96",
	}})
	eni := testENI(t, "10.0.0.5", "") // a prefix, but describe reports no primary v6

	if _, _, err := e.GetENIAllocatableIPs(eni); err == nil {
		t.Errorf("GetENIAllocatableIPs succeeded on an interface with a prefix and no primary "+
			"ipv6; the carve would then be free to hand a pod the interface's own address. eni=%+v", eni)
	}
}
