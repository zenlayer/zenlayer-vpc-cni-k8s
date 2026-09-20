// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"net"
	"testing"
)

func TestParseIP(t *testing.T) {
	validIP := "192.168.1.1"
	ip, err := ParseIP(validIP)
	if err != nil || ip.String() != validIP {
		t.Errorf("ParseIP(%q) failed, got %v, err: %v", validIP, ip, err)
	}

	invalidIP := "invalid-ip"
	_, err = ParseIP(invalidIP)
	if err == nil {
		t.Errorf("Expected error for invalid IP %q", invalidIP)
	}
}

func TestParseIPs(t *testing.T) {
	ips := []string{"192.168.1.1", "10.0.0.1"}
	result, err := ParseIPs(ips)
	if err != nil {
		t.Fatalf("ParseIPs failed: %v", err)
	}
	if len(result) != len(ips) {
		t.Errorf("Expected %d IPs, got %d", len(ips), len(result))
	}
}

func TestParseCidr(t *testing.T) {
	valid := "192.168.1.0/24"
	_, err := ParseCidr(valid)
	if err != nil {
		t.Errorf("ParseCidr(%q) failed: %v", valid, err)
	}

	invalid := "bad-cidr"
	_, err = ParseCidr(invalid)
	if err == nil {
		t.Errorf("Expected error for invalid CIDR %q", invalid)
	}
}

func TestToStringSlice(t *testing.T) {
	ips := []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2.2.2.2")}
	strs := ToStringSlice(ips)
	if len(strs) != len(ips) {
		t.Errorf("Expected %d strings, got %d", len(ips), len(strs))
	}
	if strs[0] != "1.1.1.1" {
		t.Errorf("Expected first IP string to be '1.1.1.1', got %q", strs[0])
	}
}

func TestNetIPContainAll(t *testing.T) {
	a := []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2.2.2.2")}
	b := []net.IP{net.ParseIP("2.2.2.2")}

	if !NetIPContainAll(a, b) {
		t.Error("Expected NetIPContainAll to return true")
	}

	b = []net.IP{net.ParseIP("3.3.3.3")}
	if NetIPContainAll(a, b) {
		t.Error("Expected NetIPContainAll to return false")
	}
}

func TestNetIPContainAny(t *testing.T) {
	a := []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2.2.2.2")}
	b := []net.IP{net.ParseIP("3.3.3.3"), net.ParseIP("2.2.2.2")}

	if !NetIPContainAny(a, b) {
		t.Error("Expected NetIPContainAny to return true")
	}

	b = []net.IP{net.ParseIP("4.4.4.4")}
	if NetIPContainAny(a, b) {
		t.Error("Expected NetIPContainAny to return false")
	}
}

func TestNetIPToMap(t *testing.T) {
	ips := []net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("2.2.2.2")}
	m := NetIPToMap(ips)
	if len(m) != len(ips) {
		t.Errorf("Expected map size %d, got %d", len(ips), len(m))
	}
	if _, ok := m["1.1.1.1"]; !ok {
		t.Error("Expected map to contain key '1.1.1.1'")
	}
}

func TestToStringIPNet(t *testing.T) {
	_, ipNet, _ := net.ParseCIDR("192.168.0.0/16")
	str := ToStringIPNet(ipNet)
	if str != "192.168.0.0/16" {
		t.Errorf("Expected CIDR string '192.168.0.0/16', got %q", str)
	}

	var nilNet *net.IPNet
	if ToStringIPNet(nilNet) != "" {
		t.Errorf("Expected empty string for nil net.IPNet")
	}
}
