// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"testing"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/rpc"
)

func TestToIP(t *testing.T) {
	tests := []struct {
		name    string
		addr    string
		want    string // canonical form, empty when an error is expected
		wantErr bool
	}{
		{name: "ipv4", addr: "192.168.1.1", want: "192.168.1.1"},
		{name: "ipv4 all zeros", addr: "0.0.0.0", want: "0.0.0.0"},
		{name: "ipv6", addr: "fd00::1", want: "fd00::1"},
		{name: "ipv6 is canonicalised", addr: "2001:0DB8:0000::0001", want: "2001:db8::1"},
		{name: "ipv4-mapped ipv6 prints as ipv4", addr: "::ffff:192.168.1.1", want: "192.168.1.1"},
		{name: "empty string", addr: "", wantErr: true},
		{name: "cidr is not an ip", addr: "192.168.1.1/24", wantErr: true},
		{name: "hostname", addr: "localhost", wantErr: true},
		{name: "octet out of range", addr: "256.1.1.1", wantErr: true},
		{name: "surrounding spaces", addr: " 192.168.1.1 ", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ToIP(tt.addr)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ToIP(%q) error = nil, want an error", tt.addr)
				}
				if got != nil {
					t.Errorf("ToIP(%q) = %v, want nil on error", tt.addr, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ToIP(%q) unexpected error: %v", tt.addr, err)
			}
			if got.String() != tt.want {
				t.Errorf("ToIP(%q) = %s, want %s", tt.addr, got, tt.want)
			}
		})
	}
}

func TestBuildIPNet(t *testing.T) {
	tests := []struct {
		name     string
		ip       *rpc.IPSet
		subnet   *rpc.IPSet
		wantIPv4 string // "" means the field must be nil
		wantIPv6 string
		wantErr  bool
	}{
		{
			name:   "nil ip yields an empty set without error",
			ip:     nil,
			subnet: &rpc.IPSet{IPv4: "192.168.1.0/24"},
		},
		{
			name:   "nil subnet yields an empty set without error",
			ip:     &rpc.IPSet{IPv4: "192.168.1.1"},
			subnet: nil,
		},
		{
			name:   "both nil",
			ip:     nil,
			subnet: nil,
		},
		{
			name:   "both empty",
			ip:     &rpc.IPSet{},
			subnet: &rpc.IPSet{},
		},
		{
			name:     "ipv4 keeps the host address and takes the subnet mask",
			ip:       &rpc.IPSet{IPv4: "192.168.1.5"},
			subnet:   &rpc.IPSet{IPv4: "192.168.1.0/24"},
			wantIPv4: "192.168.1.5/24",
		},
		{
			name:     "ipv6 only",
			ip:       &rpc.IPSet{IPv6: "fd00::5"},
			subnet:   &rpc.IPSet{IPv6: "fd00::/64"},
			wantIPv6: "fd00::5/64",
		},
		{
			name:     "dual stack",
			ip:       &rpc.IPSet{IPv4: "10.0.0.5", IPv6: "2001:db8::5"},
			subnet:   &rpc.IPSet{IPv4: "10.0.0.0/8", IPv6: "2001:db8::/32"},
			wantIPv4: "10.0.0.5/8",
			wantIPv6: "2001:db8::5/32",
		},
		{
			name:     "a family is skipped when its ip is missing",
			ip:       &rpc.IPSet{IPv4: "10.0.0.5"},
			subnet:   &rpc.IPSet{IPv4: "10.0.0.0/8", IPv6: "2001:db8::/32"},
			wantIPv4: "10.0.0.5/8",
		},
		{
			name:     "a family is skipped when its subnet is missing",
			ip:       &rpc.IPSet{IPv4: "10.0.0.5", IPv6: "2001:db8::5"},
			subnet:   &rpc.IPSet{IPv4: "10.0.0.0/8"},
			wantIPv4: "10.0.0.5/8",
		},
		{
			name:     "a subnet given as a host address still supplies the mask",
			ip:       &rpc.IPSet{IPv4: "192.168.1.5"},
			subnet:   &rpc.IPSet{IPv4: "192.168.1.200/24"},
			wantIPv4: "192.168.1.5/24",
		},
		{
			name:    "malformed ipv4 address",
			ip:      &rpc.IPSet{IPv4: "not-an-ip"},
			subnet:  &rpc.IPSet{IPv4: "192.168.1.0/24"},
			wantErr: true,
		},
		{
			name:    "malformed ipv4 subnet",
			ip:      &rpc.IPSet{IPv4: "192.168.1.5"},
			subnet:  &rpc.IPSet{IPv4: "192.168.1.0"},
			wantErr: true,
		},
		{
			name:    "malformed ipv6 address",
			ip:      &rpc.IPSet{IPv6: "fd00::zz"},
			subnet:  &rpc.IPSet{IPv6: "fd00::/64"},
			wantErr: true,
		},
		{
			name:    "malformed ipv6 subnet",
			ip:      &rpc.IPSet{IPv6: "fd00::5"},
			subnet:  &rpc.IPSet{IPv6: "fd00::/999"},
			wantErr: true,
		},
		{
			name:    "a bad ipv6 aborts even when ipv4 is fine",
			ip:      &rpc.IPSet{IPv4: "10.0.0.5", IPv6: "bad"},
			subnet:  &rpc.IPSet{IPv4: "10.0.0.0/8", IPv6: "2001:db8::/32"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := BuildIPNet(tt.ip, tt.subnet)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("BuildIPNet() error = nil, want an error (got %v)", got)
				}
				if got != nil {
					t.Errorf("BuildIPNet() = %v, want nil on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("BuildIPNet() unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("BuildIPNet() = nil, want a non-nil set")
			}
			checkIPNetField(t, "IPv4", got.IPv4, tt.wantIPv4)
			checkIPNetField(t, "IPv6", got.IPv6, tt.wantIPv6)
		})
	}
}

func TestToIPSet(t *testing.T) {
	tests := []struct {
		name     string
		ip       *rpc.IPSet
		wantIPv4 string // "" means the field must be nil
		wantIPv6 string
		wantErr  bool
	}{
		{name: "nil input is an error", ip: nil, wantErr: true},
		{name: "empty message yields an empty set", ip: &rpc.IPSet{}},
		{name: "ipv4 only", ip: &rpc.IPSet{IPv4: "192.168.1.1"}, wantIPv4: "192.168.1.1"},
		{name: "ipv6 only", ip: &rpc.IPSet{IPv6: "fd00::1"}, wantIPv6: "fd00::1"},
		{
			name:     "dual stack",
			ip:       &rpc.IPSet{IPv4: "192.168.1.1", IPv6: "fd00::1"},
			wantIPv4: "192.168.1.1",
			wantIPv6: "fd00::1",
		},
		{
			// Not a typo: each field is parsed, never checked against the
			// family its name implies.
			name:     "families are not cross-checked",
			ip:       &rpc.IPSet{IPv4: "fd00::1"},
			wantIPv4: "fd00::1",
		},
		{name: "malformed ipv4", ip: &rpc.IPSet{IPv4: "1.2.3"}, wantErr: true},
		{name: "malformed ipv6", ip: &rpc.IPSet{IPv6: "fd00:::1"}, wantErr: true},
		{name: "cidr instead of ip", ip: &rpc.IPSet{IPv4: "192.168.1.1/24"}, wantErr: true},
		{
			name:    "a bad ipv6 aborts even when ipv4 is fine",
			ip:      &rpc.IPSet{IPv4: "192.168.1.1", IPv6: "nope"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ToIPSet(tt.ip)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ToIPSet() error = nil, want an error (got %v)", got)
				}
				if got != nil {
					t.Errorf("ToIPSet() = %v, want nil on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ToIPSet() unexpected error: %v", err)
			}
			if got.GetIPv4() != tt.wantIPv4 {
				t.Errorf("IPv4 = %q, want %q", got.GetIPv4(), tt.wantIPv4)
			}
			if got.GetIPv6() != tt.wantIPv6 {
				t.Errorf("IPv6 = %q, want %q", got.GetIPv6(), tt.wantIPv6)
			}
		})
	}
}

func TestToIPNetSet(t *testing.T) {
	tests := []struct {
		name     string
		ip       *rpc.IPSet
		wantIPv4 string // "" means the field must be nil
		wantIPv6 string
		wantErr  bool
	}{
		{name: "nil input is an error", ip: nil, wantErr: true},
		{name: "empty message yields an empty set", ip: &rpc.IPSet{}},
		{
			// Unlike BuildIPNet, this keeps the network address only.
			name:     "host bits are dropped",
			ip:       &rpc.IPSet{IPv4: "192.168.1.5/24"},
			wantIPv4: "192.168.1.0/24",
		},
		{
			name:     "ipv6 host bits are dropped",
			ip:       &rpc.IPSet{IPv6: "fd00::5/64"},
			wantIPv6: "fd00::/64",
		},
		{
			name:     "dual stack",
			ip:       &rpc.IPSet{IPv4: "10.0.0.0/8", IPv6: "2001:db8::/32"},
			wantIPv4: "10.0.0.0/8",
			wantIPv6: "2001:db8::/32",
		},
		{name: "bare ip without a prefix is an error", ip: &rpc.IPSet{IPv4: "192.168.1.1"}, wantErr: true},
		{name: "prefix length out of range", ip: &rpc.IPSet{IPv4: "192.168.1.0/33"}, wantErr: true},
		{name: "malformed ipv6 cidr", ip: &rpc.IPSet{IPv6: "fd00::/200"}, wantErr: true},
		{
			name:    "a bad ipv6 aborts even when ipv4 is fine",
			ip:      &rpc.IPSet{IPv4: "10.0.0.0/8", IPv6: "junk"},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ToIPNetSet(tt.ip)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ToIPNetSet() error = nil, want an error (got %v)", got)
				}
				if got != nil {
					t.Errorf("ToIPNetSet() = %v, want nil on error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ToIPNetSet() unexpected error: %v", err)
			}
			if got == nil {
				t.Fatal("ToIPNetSet() = nil, want a non-nil set")
			}
			checkIPNetField(t, "IPv4", got.IPv4, tt.wantIPv4)
			checkIPNetField(t, "IPv6", got.IPv6, tt.wantIPv6)
		})
	}
}

func TestIPSetRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   *rpc.IPSet
	}{
		{name: "empty", in: &rpc.IPSet{}},
		{name: "ipv4 only", in: &rpc.IPSet{IPv4: "192.168.1.1"}},
		{name: "ipv6 only", in: &rpc.IPSet{IPv6: "fd00::1"}},
		{name: "dual stack", in: &rpc.IPSet{IPv4: "192.168.1.1", IPv6: "fd00::1"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, err := ToIPSet(tt.in)
			if err != nil {
				t.Fatalf("ToIPSet() unexpected error: %v", err)
			}
			out := set.ToPb()
			if out.IPv4 != tt.in.IPv4 || out.IPv6 != tt.in.IPv6 {
				t.Errorf("round trip = {%q %q}, want {%q %q}", out.IPv4, out.IPv6, tt.in.IPv4, tt.in.IPv6)
			}
		})
	}
}

// TestIPNetSetRoundTrip only feeds already normalised networks: ToIPNetSet drops
// host bits, so anything else does not come back unchanged.
func TestIPNetSetRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		in   *rpc.IPSet
	}{
		{name: "empty", in: &rpc.IPSet{}},
		{name: "ipv4 network", in: &rpc.IPSet{IPv4: "192.168.1.0/24"}},
		{name: "ipv6 network", in: &rpc.IPSet{IPv6: "fd00::/64"}},
		{name: "dual stack", in: &rpc.IPSet{IPv4: "10.0.0.0/8", IPv6: "2001:db8::/32"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			set, err := ToIPNetSet(tt.in)
			if err != nil {
				t.Fatalf("ToIPNetSet() unexpected error: %v", err)
			}
			out := set.ToPb()
			if out.IPv4 != tt.in.IPv4 || out.IPv6 != tt.in.IPv6 {
				t.Errorf("round trip = {%q %q}, want {%q %q}", out.IPv4, out.IPv6, tt.in.IPv4, tt.in.IPv6)
			}
		})
	}
}
