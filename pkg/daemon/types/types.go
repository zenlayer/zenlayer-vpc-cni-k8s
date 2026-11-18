// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

const (
	ModeENIMultiIP = "eni_multi_ip"
)

// IPStack is the address family mode the daemon runs in.
type IPStack string

// The stack modes the daemon knows. IPStackIPv6 parses but the ipStack switch
// in pkg/daemon/config rejects it, so an IPv6-only configuration is refused
// with a reason rather than as a typo.
const (
	IPStackIPv4 IPStack = "ipv4"
	IPStackDual IPStack = "dual"
	IPStackIPv6 IPStack = "ipv6"
)

// EnableIPv4 reports whether this stack mode serves IPv4.
func (f IPStack) EnableIPv4() bool {
	return f == IPStackIPv4 || f == IPStackDual
}

// EnableIPv6 reports whether this stack mode serves IPv6.
func (f IPStack) EnableIPv6() bool {
	return f == IPStackIPv6 || f == IPStackDual
}

// Families lists the address families this stack mode enables, IPv4 first.
//
// It is for the callers that mean "one address of each" -- IPSet.Complete and
// validateSubnetsForStack -- where spelling both families out separately is
// what lets them drift apart. To branch on a single family use EnableIPv4 or
// EnableIPv6.
func (f IPStack) Families() []zecTypes.IPFamily {
	var out []zecTypes.IPFamily
	if f.EnableIPv4() {
		out = append(out, zecTypes.IPFamilyV4)
	}
	if f.EnableIPv6() {
		out = append(out, zecTypes.IPFamilyV6)
	}
	return out
}

// Support reports whether f, the stack a subnet offers, can serve a daemon
// configured for s. A dual receiver supports any s; otherwise the two must be
// equal, so an IPv4-only subnet does not support a dual daemon.
func (f IPStack) Support(s IPStack) bool {
	if f == IPStackDual {
		return true
	}
	return (s == IPStackIPv4 && f == IPStackIPv4) ||
		(s == IPStackIPv6 && f == IPStackIPv6)
}

// IPAMType picks who talks to the platform. It is a behavior switch with no
// default, like ipStack.
type IPAMType string

const (
	// IPAMTypeDirect: the daemon calls the OpenAPI itself, with the
	// credentials mounted on every node.
	IPAMTypeDirect IPAMType = "direct"
	// IPAMTypeCRD: the daemon holds no credentials. It states what it needs in
	// its node's ZecNode and the controlplane does the platform side.
	IPAMTypeCRD IPAMType = "crd"
)
