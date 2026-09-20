// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"fmt"
	"net"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/rpc"
)

// ToIP parses a bare address. A CIDR is rejected rather than truncated, because
// the fields this is used on carry addresses and a mask arriving in one is a
// sign the caller mixed up an address slot with a subnet slot.
func ToIP(addr string) (net.IP, error) {
	ip := net.ParseIP(addr)
	if ip == nil {
		return nil, fmt.Errorf("failed to parse ip %s", addr)
	}
	return ip, nil
}

// BuildIPNet pairs a bare address with the mask of the subnet it sits in. The
// two arguments have the same type but not the same shape: ip carries addresses
// and subnet carries networks. A family is filled in only when both sides carry
// it, and a nil argument yields an empty set rather than an error.
func BuildIPNet(ip, subnet *rpc.IPSet) (*IPNetSet, error) {
	ipnet := &IPNetSet{}
	if ip == nil || subnet == nil {
		return ipnet, nil
	}
	exec := func(ip, subnet string) (*net.IPNet, error) {
		i, err := ToIP(ip)
		if err != nil {
			return nil, err
		}
		_, sub, err := net.ParseCIDR(subnet)
		if err != nil {
			return nil, err
		}
		sub.IP = i
		return sub, nil
	}
	var err error
	if ip.IPv4 != "" && subnet.IPv4 != "" {
		ipnet.IPv4, err = exec(ip.IPv4, subnet.IPv4)
		if err != nil {
			return nil, err
		}
	}
	if ip.IPv6 != "" && subnet.IPv6 != "" {
		ipnet.IPv6, err = exec(ip.IPv6, subnet.IPv6)
		if err != nil {
			return nil, err
		}
	}
	return ipnet, nil
}

// ToIPSet parses every non-empty field of ip as a bare address. Unlike
// BuildIPNet a nil argument is an error; an empty field is skipped, so a result
// with neither family set is possible.
func ToIPSet(ip *rpc.IPSet) (*IPSet, error) {
	if ip == nil {
		return nil, fmt.Errorf("ip is nil")
	}
	ipSet := &IPSet{}
	var err error
	if ip.IPv4 != "" {
		ipSet.IPv4, err = ToIP(ip.IPv4)
		if err != nil {
			return nil, err
		}
	}
	if ip.IPv6 != "" {
		ipSet.IPv6, err = ToIP(ip.IPv6)
		if err != nil {
			return nil, err
		}
	}
	return ipSet, nil
}

// ToIPNetSet parses every non-empty field of ip as a CIDR and keeps the
// network, so host bits in the input are dropped. BuildIPNet is the variant
// that keeps the address and takes only the mask from the subnet.
func ToIPNetSet(ip *rpc.IPSet) (*IPNetSet, error) {
	if ip == nil {
		return nil, fmt.Errorf("ip is nil")
	}
	ipNetSet := &IPNetSet{}
	var err error
	if ip.IPv4 != "" {
		_, ipNetSet.IPv4, err = net.ParseCIDR(ip.IPv4)
		if err != nil {
			return nil, err
		}
	}
	if ip.IPv6 != "" {
		_, ipNetSet.IPv6, err = net.ParseCIDR(ip.IPv6)
		if err != nil {
			return nil, err
		}
	}
	return ipNetSet, nil
}
