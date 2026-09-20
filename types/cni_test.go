// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

//

package types

import (
	"net"
	"testing"
)

// mustIP parses addr or fails the test.
func mustIP(t *testing.T, addr string) net.IP {
	t.Helper()
	ip := net.ParseIP(addr)
	if ip == nil {
		t.Fatalf("mustIP: cannot parse %q", addr)
	}
	return ip
}

// mustIPNet parses cidr into a *net.IPNet keeping the host bits of the address.
// Both shapes reach IPNetSet in production: BuildIPNet keeps the address and
// takes only the mask from the subnet, ToIPNetSet and SetIPNet keep the masked
// network.
func mustIPNet(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	ip, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("mustIPNet: cannot parse %q: %v", cidr, err)
	}
	return &net.IPNet{IP: ip, Mask: ipNet.Mask}
}

// mustMAC parses a MAC or fails the test.
func mustMAC(t *testing.T, s string) net.HardwareAddr {
	t.Helper()
	mac, err := net.ParseMAC(s)
	if err != nil {
		t.Fatalf("mustMAC: cannot parse %q: %v", s, err)
	}
	return mac
}

func TestIPNetSet_SetIPNet(t *testing.T) {
	tests := []struct {
		name     string
		initial  *IPNetSet
		input    string
		wantIPv4 string // "" means the field must stay nil
		wantIPv6 string
	}{
		{
			name:     "ipv4 host address is masked down to the network",
			initial:  &IPNetSet{},
			input:    "192.168.1.5/24",
			wantIPv4: "192.168.1.0/24",
		},
		{
			name:     "ipv4 network address",
			initial:  &IPNetSet{},
			input:    "10.0.0.0/8",
			wantIPv4: "10.0.0.0/8",
		},
		{
			name:     "ipv4 single host /32",
			initial:  &IPNetSet{},
			input:    "172.16.0.1/32",
			wantIPv4: "172.16.0.1/32",
		},
		{
			name:     "ipv6 goes to the v6 field",
			initial:  &IPNetSet{},
			input:    "fd00::1/64",
			wantIPv6: "fd00::/64",
		},
		{
			name:     "ipv6 /128",
			initial:  &IPNetSet{},
			input:    "2001:db8::1/128",
			wantIPv6: "2001:db8::1/128",
		},
		{
			name:    "invalid cidr leaves the set untouched",
			initial: &IPNetSet{},
			input:   "not-a-cidr",
		},
		{
			name:    "bare ip without prefix is rejected",
			initial: &IPNetSet{},
			input:   "192.168.1.1",
		},
		{
			name:    "empty string is rejected",
			initial: &IPNetSet{},
			input:   "",
		},
		{
			name:    "out of range prefix length is rejected",
			initial: &IPNetSet{},
			input:   "192.168.1.0/33",
		},
		{
			name:     "existing values are preserved on parse failure",
			initial:  &IPNetSet{IPv4: mustIPNet(t, "10.0.0.0/8")},
			input:    "garbage",
			wantIPv4: "10.0.0.0/8",
		},
		{
			name:     "setting v6 keeps a previously set v4",
			initial:  &IPNetSet{IPv4: mustIPNet(t, "10.0.0.0/8")},
			input:    "fd00::/64",
			wantIPv4: "10.0.0.0/8",
			wantIPv6: "fd00::/64",
		},
		{
			name:     "an existing value of the same family is overwritten",
			initial:  &IPNetSet{IPv4: mustIPNet(t, "10.0.0.0/8")},
			input:    "192.168.0.0/16",
			wantIPv4: "192.168.0.0/16",
		},
		{
			// An IPv4-mapped IPv6 address reports To4() != nil, so it is
			// filed under IPv4. The mask ParseCIDR returns stays 128 bits
			// wide (Size() reports 120, 128); it is String() that renders the
			// pair as 192.168.1.0/24.
			name:     "ipv4-mapped ipv6 is classified as ipv4",
			initial:  &IPNetSet{},
			input:    "::ffff:192.168.1.1/120",
			wantIPv4: "192.168.1.0/24",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.initial.SetIPNet(tt.input)
			if got != tt.initial {
				t.Errorf("SetIPNet() must return the receiver for chaining")
			}
			checkIPNetField(t, "IPv4", tt.initial.IPv4, tt.wantIPv4)
			checkIPNetField(t, "IPv6", tt.initial.IPv6, tt.wantIPv6)
		})
	}
}

func checkIPNetField(t *testing.T, name string, got *net.IPNet, want string) {
	t.Helper()
	if want == "" {
		if got != nil {
			t.Errorf("%s = %s, want nil", name, got)
		}
		return
	}
	if got == nil {
		t.Fatalf("%s = nil, want %s", name, want)
	}
	if got.String() != want {
		t.Errorf("%s = %s, want %s", name, got, want)
	}
}

func TestIPNetSet_String(t *testing.T) {
	tests := []struct {
		name string
		set  *IPNetSet
		want string
	}{
		{name: "nil receiver", set: nil, want: ""},
		{name: "both fields nil", set: &IPNetSet{}, want: ""},
		{
			name: "ipv4 only",
			set:  &IPNetSet{IPv4: mustIPNet(t, "192.168.1.1/24")},
			want: "192.168.1.1/24",
		},
		{
			name: "ipv6 only",
			set:  &IPNetSet{IPv6: mustIPNet(t, "fd00::1/64")},
			want: "fd00::1/64",
		},
		{
			name: "dual stack is dash separated with v4 first",
			set: &IPNetSet{
				IPv4: mustIPNet(t, "192.168.1.1/24"),
				IPv6: mustIPNet(t, "fd00::1/64"),
			},
			want: "192.168.1.1/24-fd00::1/64",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIPNetSet_ToPb(t *testing.T) {
	tests := []struct {
		name     string
		set      *IPNetSet
		wantIPv4 string
		wantIPv6 string
	}{
		// Unlike IPSet.ToPb, IPNetSet.ToPb never returns nil.
		{name: "nil receiver yields an empty message", set: nil},
		{name: "empty set", set: &IPNetSet{}},
		{
			name:     "ipv4 only",
			set:      &IPNetSet{IPv4: mustIPNet(t, "192.168.1.1/24")},
			wantIPv4: "192.168.1.1/24",
		},
		{
			name:     "ipv6 only",
			set:      &IPNetSet{IPv6: mustIPNet(t, "fd00::1/64")},
			wantIPv6: "fd00::1/64",
		},
		{
			name: "dual stack",
			set: &IPNetSet{
				IPv4: mustIPNet(t, "10.0.0.5/8"),
				IPv6: mustIPNet(t, "2001:db8::5/32"),
			},
			wantIPv4: "10.0.0.5/8",
			wantIPv6: "2001:db8::5/32",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.set.ToPb()
			if got == nil {
				t.Fatal("ToPb() = nil, want a non-nil message")
			}
			if got.IPv4 != tt.wantIPv4 {
				t.Errorf("IPv4 = %q, want %q", got.IPv4, tt.wantIPv4)
			}
			if got.IPv6 != tt.wantIPv6 {
				t.Errorf("IPv6 = %q, want %q", got.IPv6, tt.wantIPv6)
			}
		})
	}
}

func TestIPSet_String(t *testing.T) {
	tests := []struct {
		name string
		set  *IPSet
		want string
	}{
		{name: "nil receiver", set: nil, want: ""},
		{name: "empty set", set: &IPSet{}, want: ""},
		{name: "ipv4 only", set: &IPSet{IPv4: mustIP(t, "192.168.1.1")}, want: "192.168.1.1"},
		{name: "ipv6 only", set: &IPSet{IPv6: mustIP(t, "fd00::1")}, want: "fd00::1"},
		{
			name: "dual stack is dash separated with v4 first",
			set:  &IPSet{IPv4: mustIP(t, "192.168.1.1"), IPv6: mustIP(t, "fd00::1")},
			want: "192.168.1.1-fd00::1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.String(); got != tt.want {
				t.Errorf("String() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestIPSet_Getters(t *testing.T) {
	tests := []struct {
		name     string
		set      *IPSet
		wantIPv4 string
		wantIPv6 string
	}{
		{name: "nil receiver", set: nil},
		{name: "empty set", set: &IPSet{}},
		{
			name:     "ipv4 only",
			set:      &IPSet{IPv4: mustIP(t, "10.1.2.3")},
			wantIPv4: "10.1.2.3",
		},
		{
			name:     "ipv6 only",
			set:      &IPSet{IPv6: mustIP(t, "2001:db8::1")},
			wantIPv6: "2001:db8::1",
		},
		{
			name:     "dual stack",
			set:      &IPSet{IPv4: mustIP(t, "10.1.2.3"), IPv6: mustIP(t, "2001:db8::1")},
			wantIPv4: "10.1.2.3",
			wantIPv6: "2001:db8::1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.set.GetIPv4(); got != tt.wantIPv4 {
				t.Errorf("GetIPv4() = %q, want %q", got, tt.wantIPv4)
			}
			if got := tt.set.GetIPv6(); got != tt.wantIPv6 {
				t.Errorf("GetIPv6() = %q, want %q", got, tt.wantIPv6)
			}
		})
	}
}

func TestIPSet_ToPb(t *testing.T) {
	t.Run("nil receiver yields nil", func(t *testing.T) {
		var s *IPSet
		if got := s.ToPb(); got != nil {
			t.Errorf("ToPb() = %v, want nil", got)
		}
	})

	tests := []struct {
		name     string
		set      *IPSet
		wantIPv4 string
		wantIPv6 string
	}{
		{name: "empty set yields empty strings", set: &IPSet{}},
		{name: "ipv4 only", set: &IPSet{IPv4: mustIP(t, "192.168.1.1")}, wantIPv4: "192.168.1.1"},
		{name: "ipv6 only", set: &IPSet{IPv6: mustIP(t, "fd00::1")}, wantIPv6: "fd00::1"},
		{
			name:     "dual stack",
			set:      &IPSet{IPv4: mustIP(t, "192.168.1.1"), IPv6: mustIP(t, "fd00::1")},
			wantIPv4: "192.168.1.1",
			wantIPv6: "fd00::1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.set.ToPb()
			if got == nil {
				t.Fatal("ToPb() = nil, want a non-nil message")
			}
			if got.IPv4 != tt.wantIPv4 {
				t.Errorf("IPv4 = %q, want %q", got.IPv4, tt.wantIPv4)
			}
			if got.IPv6 != tt.wantIPv6 {
				t.Errorf("IPv6 = %q, want %q", got.IPv6, tt.wantIPv6)
			}
		})
	}
}

func TestIPSet_ToPbWithMask(t *testing.T) {
	t.Run("nil receiver yields nil", func(t *testing.T) {
		var s *IPSet
		if got := s.ToPbWithMask(&IPNetSet{IPv4: mustIPNet(t, "192.168.1.0/24")}); got != nil {
			t.Errorf("ToPbWithMask() = %v, want nil", got)
		}
	})

	tests := []struct {
		name     string
		set      *IPSet
		cidr     *IPNetSet
		wantIPv4 string
		wantIPv6 string
	}{
		{
			name:     "nil cidr falls back to host masks",
			set:      &IPSet{IPv4: mustIP(t, "192.168.1.5"), IPv6: mustIP(t, "fd00::5")},
			cidr:     nil,
			wantIPv4: "192.168.1.5/32",
			wantIPv6: "fd00::5/128",
		},
		{
			name:     "empty cidr set falls back to host masks",
			set:      &IPSet{IPv4: mustIP(t, "192.168.1.5"), IPv6: mustIP(t, "fd00::5")},
			cidr:     &IPNetSet{},
			wantIPv4: "192.168.1.5/32",
			wantIPv6: "fd00::5/128",
		},
		{
			name:     "masks are taken from the cidr and host bits kept",
			set:      &IPSet{IPv4: mustIP(t, "192.168.1.5"), IPv6: mustIP(t, "fd00::5")},
			cidr:     &IPNetSet{IPv4: mustIPNet(t, "192.168.1.0/24"), IPv6: mustIPNet(t, "fd00::/64")},
			wantIPv4: "192.168.1.5/24",
			wantIPv6: "fd00::5/64",
		},
		{
			name:     "per family fallback when only the v4 cidr is known",
			set:      &IPSet{IPv4: mustIP(t, "10.0.0.7"), IPv6: mustIP(t, "2001:db8::7")},
			cidr:     &IPNetSet{IPv4: mustIPNet(t, "10.0.0.0/8")},
			wantIPv4: "10.0.0.7/8",
			wantIPv6: "2001:db8::7/128",
		},
		{
			name:     "single stack v4 leaves v6 empty",
			set:      &IPSet{IPv4: mustIP(t, "10.0.0.7")},
			cidr:     &IPNetSet{IPv4: mustIPNet(t, "10.0.0.0/16")},
			wantIPv4: "10.0.0.7/16",
		},
		{
			name:     "single stack v6 leaves v4 empty",
			set:      &IPSet{IPv6: mustIP(t, "2001:db8::7")},
			cidr:     &IPNetSet{IPv6: mustIPNet(t, "2001:db8::/32")},
			wantIPv6: "2001:db8::7/32",
		},
		{
			name: "empty ip set yields empty strings",
			set:  &IPSet{},
			cidr: &IPNetSet{IPv4: mustIPNet(t, "10.0.0.0/8")},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.set.ToPbWithMask(tt.cidr)
			if got == nil {
				t.Fatal("ToPbWithMask() = nil, want a non-nil message")
			}
			if got.IPv4 != tt.wantIPv4 {
				t.Errorf("IPv4 = %q, want %q", got.IPv4, tt.wantIPv4)
			}
			if got.IPv6 != tt.wantIPv6 {
				t.Errorf("IPv6 = %q, want %q", got.IPv6, tt.wantIPv6)
			}
		})
	}
}

func TestPairIPs(t *testing.T) {
	ips := func(t *testing.T, addrs ...string) []net.IP {
		t.Helper()
		if addrs == nil {
			return nil
		}
		out := make([]net.IP, 0, len(addrs))
		for _, a := range addrs {
			out = append(out, mustIP(t, a))
		}
		return out
	}

	tests := []struct {
		name  string
		ipv4s []net.IP
		ipv6s []net.IP
		// Expected String() of every produced IPSet, in order.
		want []string
	}{
		{
			name: "both nil yields an empty slice",
			want: []string{},
		},
		{
			name:  "both empty yields an empty slice",
			ipv4s: []net.IP{},
			ipv6s: []net.IP{},
			want:  []string{},
		},
		{
			name:  "equal lengths pair up by index",
			ipv4s: ips(t, "10.0.0.1", "10.0.0.2"),
			ipv6s: ips(t, "fd00::1", "fd00::2"),
			want:  []string{"10.0.0.1-fd00::1", "10.0.0.2-fd00::2"},
		},
		{
			name:  "only ipv4s",
			ipv4s: ips(t, "10.0.0.1", "10.0.0.2", "10.0.0.3"),
			want:  []string{"10.0.0.1", "10.0.0.2", "10.0.0.3"},
		},
		{
			name:  "only ipv6s",
			ipv6s: ips(t, "fd00::1", "fd00::2"),
			want:  []string{"fd00::1", "fd00::2"},
		},
		{
			name:  "more ipv4s than ipv6s leaves the tail v6-less",
			ipv4s: ips(t, "10.0.0.1", "10.0.0.2", "10.0.0.3"),
			ipv6s: ips(t, "fd00::1"),
			want:  []string{"10.0.0.1-fd00::1", "10.0.0.2", "10.0.0.3"},
		},
		{
			name:  "more ipv6s than ipv4s leaves the tail v4-less",
			ipv4s: ips(t, "10.0.0.1"),
			ipv6s: ips(t, "fd00::1", "fd00::2", "fd00::3"),
			want:  []string{"10.0.0.1-fd00::1", "fd00::2", "fd00::3"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := PairIPs(tt.ipv4s, tt.ipv6s)
			if got == nil {
				t.Fatal("PairIPs() = nil, want a non-nil slice")
			}
			if len(got) != len(tt.want) {
				t.Fatalf("len(PairIPs()) = %d, want %d (%v)", len(got), len(tt.want), got)
			}
			for i := range got {
				if s := got[i].String(); s != tt.want[i] {
					t.Errorf("result[%d] = %q, want %q", i, s, tt.want[i])
				}
			}
			// Recomputed from the inputs, so a want list of the wrong length
			// cannot pass.
			if want := max(len(tt.ipv4s), len(tt.ipv6s)); len(got) != want {
				t.Errorf("len(PairIPs()) = %d, want max(len(v4), len(v6)) = %d", len(got), want)
			}
		})
	}

	t.Run("input slices are shared, not copied", func(t *testing.T) {
		v4 := ips(t, "10.0.0.1")
		got := PairIPs(v4, nil)
		if !got[0].IPv4.Equal(v4[0]) {
			t.Fatalf("result[0].IPv4 = %v, want %v", got[0].IPv4, v4[0])
		}
	})
}
