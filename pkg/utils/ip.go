// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"fmt"
	"math/big"
	"net"

	"k8s.io/apimachinery/pkg/util/sets"
)

// ParseIP parses ip, returning an error where net.ParseIP returns a nil IP.
func ParseIP(ip string) (net.IP, error) {
	netIP := net.ParseIP(ip)
	if netIP == nil {
		return nil, fmt.Errorf("invalid ip [%s]", ip)
	}
	return netIP, nil
}

// ParseIPs parses every entry, stopping at the first unparseable one and
// returning no partial result.
func ParseIPs(ips []string) ([]net.IP, error) {
	var result []net.IP
	for _, ipStr := range ips {
		ip, err := ParseIP(ipStr)
		if err != nil {
			return nil, err
		}
		result = append(result, ip)
	}
	return result, nil
}

// ParseCidr returns the masked network, discarding any host bits in cidr.
func ParseCidr(cidr string) (*net.IPNet, error) {
	_, IPNet, err := net.ParseCIDR(cidr)
	if err != nil {
		return nil, fmt.Errorf("invalid cidr [%s]", cidr)
	}
	return IPNet, nil
}

// ToStringIPNet renders ipNet with net.IPNet.String, except that a nil pointer
// yields the empty string instead of "<nil>", so an optional CIDR can be logged
// without a nil check.
func ToStringIPNet(ipNet *net.IPNet) string {
	if ipNet != nil {
		return ipNet.String()
	}
	return ""
}

// ToStringSlice converts a net.IP slice to its string forms.
func ToStringSlice(ips []net.IP) []string {
	var result []string
	for _, ip := range ips {
		result = append(result, ip.String())
	}
	return result
}

// NetIPContainAll return true if all items in b can be found in a.
func NetIPContainAll(a, b []net.IP) bool {
	return sets.NewString(ToStringSlice(a)...).HasAll(ToStringSlice(b)...)
}

// NetIPContainAny return true if any items in b can be found in a.
func NetIPContainAny(a, b []net.IP) bool {
	return sets.NewString(ToStringSlice(a)...).HasAny(ToStringSlice(b)...)
}

// NetIPToMap keys ips by their string form, the same key NetIPContainAll and
// NetIPContainAny compare on.
func NetIPToMap(ips []net.IP) map[string]net.IP {
	result := make(map[string]net.IP)
	for _, ip := range ips {
		result[ip.String()] = ip
	}
	return result
}

// NetIPv6ToMap enumerates the addresses at offsets 2 through 102 of an IPv6
// network, keyed by their string form. A bounded window, because a /96 holds
// 2^32 addresses. The prefix length is never inspected: every candidate is
// filtered through ipv6.Contains instead, so a network smaller than the window
// yields fewer than 101 entries and a /128 yields none.
//
// Nothing outside the tests calls this today.
func NetIPv6ToMap(ipv6 *net.IPNet) map[string]net.IP {
	result := make(map[string]net.IP)

	baseIP := ipv6.IP.To16()
	if baseIP == nil {
		return result
	}

	ipInt := new(big.Int).SetBytes(baseIP)

	for i := 2; i <= 102; i++ {
		newIPInt := new(big.Int).Add(ipInt, big.NewInt(int64(i)))
		ipBytes := newIPInt.Bytes()

		// big.Int drops leading zero bytes; pad them back to form an address.
		if len(ipBytes) < 16 {
			padding := make([]byte, 16-len(ipBytes))
			ipBytes = append(padding, ipBytes...)
		}

		ip := net.IP(ipBytes)
		if ipv6.Contains(ip) {
			result[ip.String()] = ip
		}
	}

	return result
}
