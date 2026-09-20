// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package utils

import (
	"net"
	"reflect"
	"strings"
	"testing"
)

func mustCIDR(t *testing.T, cidr string) *net.IPNet {
	t.Helper()
	_, ipNet, err := net.ParseCIDR(cidr)
	if err != nil {
		t.Fatalf("net.ParseCIDR(%q) failed: %v", cidr, err)
	}
	return ipNet
}

func TestParseIPEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string // expected ip.String(), only checked when wantErr is false
		wantErr bool
	}{
		{name: "ipv4", input: "192.168.1.1", want: "192.168.1.1"},
		{name: "ipv4 unspecified", input: "0.0.0.0", want: "0.0.0.0"},
		{name: "ipv4 broadcast", input: "255.255.255.255", want: "255.255.255.255"},
		{name: "ipv6 loopback", input: "::1", want: "::1"},
		{name: "ipv6 full form is normalised", input: "2001:0db8:0000:0000:0000:0000:0000:0001", want: "2001:db8::1"},
		{name: "ipv4 mapped ipv6 renders as ipv4", input: "::ffff:1.2.3.4", want: "1.2.3.4"},
		{name: "empty string", input: "", wantErr: true},
		{name: "leading space", input: " 1.1.1.1", wantErr: true},
		{name: "trailing newline", input: "1.1.1.1\n", wantErr: true},
		{name: "cidr is not an ip", input: "1.1.1.1/32", wantErr: true},
		{name: "octet out of range", input: "999.1.1.1", wantErr: true},
		{name: "too few octets", input: "1.1.1", wantErr: true},
		{name: "hostname", input: "localhost", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseIP(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseIP(%q) = %v, want error", tt.input, got)
				}
				if got != nil {
					t.Errorf("ParseIP(%q) returned ip %v alongside error, want nil", tt.input, got)
				}
				if !strings.Contains(err.Error(), tt.input) {
					t.Errorf("error %q does not mention the offending input %q", err.Error(), tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseIP(%q) unexpected error: %v", tt.input, err)
			}
			if got.String() != tt.want {
				t.Errorf("ParseIP(%q).String() = %q, want %q", tt.input, got.String(), tt.want)
			}
		})
	}
}

func TestParseIPsEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		input   []string
		want    []string
		wantErr bool
	}{
		{name: "nil slice yields nil result", input: nil, want: nil},
		{name: "empty slice yields nil result", input: []string{}, want: nil},
		{name: "mixed families", input: []string{"10.0.0.1", "::1"}, want: []string{"10.0.0.1", "::1"}},
		{name: "duplicates are preserved", input: []string{"10.0.0.1", "10.0.0.1"}, want: []string{"10.0.0.1", "10.0.0.1"}},
		{name: "invalid entry first", input: []string{"nope", "10.0.0.1"}, wantErr: true},
		{name: "invalid entry last aborts whole slice", input: []string{"10.0.0.1", "nope"}, wantErr: true},
		{name: "empty entry", input: []string{""}, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseIPs(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseIPs(%v) = %v, want error", tt.input, got)
				}
				if got != nil {
					t.Errorf("ParseIPs(%v) returned %v alongside error, want nil", tt.input, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseIPs(%v) unexpected error: %v", tt.input, err)
			}
			if !reflect.DeepEqual(ToStringSlice(got), tt.want) {
				t.Errorf("ParseIPs(%v) = %v, want %v", tt.input, ToStringSlice(got), tt.want)
			}
		})
	}
}

func TestParseCidrEdgeCases(t *testing.T) {
	tests := []struct {
		name    string
		input   string
		want    string
		wantErr bool
	}{
		{name: "ipv4 network", input: "192.168.1.0/24", want: "192.168.1.0/24"},
		{name: "host bits are masked off", input: "192.168.1.5/24", want: "192.168.1.0/24"},
		{name: "single host", input: "10.0.0.7/32", want: "10.0.0.7/32"},
		{name: "default route", input: "0.0.0.0/0", want: "0.0.0.0/0"},
		{name: "ipv6 network", input: "2001:db8::/32", want: "2001:db8::/32"},
		{name: "ipv6 host bits are masked off", input: "2001:db8::1/64", want: "2001:db8::/64"},
		{name: "missing prefix length", input: "192.168.1.0", wantErr: true},
		{name: "prefix length out of range", input: "192.168.1.0/33", wantErr: true},
		{name: "garbage", input: "bad-cidr", wantErr: true},
		{name: "empty string", input: "", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseCidr(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseCidr(%q) = %v, want error", tt.input, got)
				}
				if got != nil {
					t.Errorf("ParseCidr(%q) returned %v alongside error, want nil", tt.input, got)
				}
				if !strings.Contains(err.Error(), tt.input) {
					t.Errorf("error %q does not mention the offending input %q", err.Error(), tt.input)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseCidr(%q) unexpected error: %v", tt.input, err)
			}
			if got.String() != tt.want {
				t.Errorf("ParseCidr(%q) = %q, want %q", tt.input, got.String(), tt.want)
			}
		})
	}
}

func TestToStringIPNetEdgeCases(t *testing.T) {
	t.Run("nil returns empty string", func(t *testing.T) {
		if got := ToStringIPNet(nil); got != "" {
			t.Errorf("ToStringIPNet(nil) = %q, want empty string", got)
		}
	})

	t.Run("zero value renders as invalid cidr marker", func(t *testing.T) {
		// A non-nil but empty *net.IPNet is still formatted by net.IPNet.String,
		// which yields "<nil>" rather than the empty string.
		if got := ToStringIPNet(&net.IPNet{}); got != "<nil>" {
			t.Errorf("ToStringIPNet(&net.IPNet{}) = %q, want %q", got, "<nil>")
		}
	})

	t.Run("ipv6", func(t *testing.T) {
		if got := ToStringIPNet(mustCIDR(t, "fd00::/8")); got != "fd00::/8" {
			t.Errorf("ToStringIPNet = %q, want %q", got, "fd00::/8")
		}
	})
}

func TestToStringSliceEdgeCases(t *testing.T) {
	t.Run("nil input yields nil output", func(t *testing.T) {
		if got := ToStringSlice(nil); got != nil {
			t.Errorf("ToStringSlice(nil) = %v, want nil", got)
		}
	})

	t.Run("empty input yields nil output", func(t *testing.T) {
		if got := ToStringSlice([]net.IP{}); got != nil {
			t.Errorf("ToStringSlice([]) = %v, want nil", got)
		}
	})

	t.Run("nil element renders as invalid ip marker", func(t *testing.T) {
		got := ToStringSlice([]net.IP{nil})
		want := []string{"<nil>"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ToStringSlice([nil]) = %v, want %v", got, want)
		}
	})

	t.Run("4 byte and 16 byte forms render identically", func(t *testing.T) {
		got := ToStringSlice([]net.IP{net.IPv4(10, 0, 0, 1).To4(), net.IPv4(10, 0, 0, 1).To16()})
		want := []string{"10.0.0.1", "10.0.0.1"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ToStringSlice = %v, want %v", got, want)
		}
	})

	t.Run("order is preserved", func(t *testing.T) {
		got := ToStringSlice([]net.IP{net.ParseIP("3.3.3.3"), net.ParseIP("1.1.1.1"), net.ParseIP("::1")})
		want := []string{"3.3.3.3", "1.1.1.1", "::1"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ToStringSlice = %v, want %v", got, want)
		}
	})
}

func TestNetIPContainAllEdgeCases(t *testing.T) {
	ips := func(s ...string) []net.IP {
		out := make([]net.IP, 0, len(s))
		for _, v := range s {
			out = append(out, net.ParseIP(v))
		}
		return out
	}

	tests := []struct {
		name string
		a    []net.IP
		b    []net.IP
		want bool
	}{
		{name: "empty needle in empty haystack", a: nil, b: nil, want: true},
		{name: "empty needle in non empty haystack", a: ips("1.1.1.1"), b: nil, want: true},
		{name: "non empty needle in empty haystack", a: nil, b: ips("1.1.1.1"), want: false},
		{name: "all present", a: ips("1.1.1.1", "2.2.2.2", "::1"), b: ips("::1", "1.1.1.1"), want: true},
		{name: "one missing", a: ips("1.1.1.1", "2.2.2.2"), b: ips("1.1.1.1", "3.3.3.3"), want: false},
		{name: "duplicate needles", a: ips("1.1.1.1"), b: ips("1.1.1.1", "1.1.1.1"), want: true},
		{name: "identical sets", a: ips("1.1.1.1"), b: ips("1.1.1.1"), want: true},
		{
			name: "4 byte and 16 byte representations compare equal",
			a:    []net.IP{net.IPv4(10, 0, 0, 1).To16()},
			b:    []net.IP{net.IPv4(10, 0, 0, 1).To4()},
			want: true,
		},
		{
			name: "ipv6 mapped ipv4 matches plain ipv4",
			a:    ips("1.2.3.4"),
			b:    ips("::ffff:1.2.3.4"),
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NetIPContainAll(tt.a, tt.b); got != tt.want {
				t.Errorf("NetIPContainAll(%v, %v) = %v, want %v",
					ToStringSlice(tt.a), ToStringSlice(tt.b), got, tt.want)
			}
		})
	}
}

func TestNetIPContainAnyEdgeCases(t *testing.T) {
	ips := func(s ...string) []net.IP {
		out := make([]net.IP, 0, len(s))
		for _, v := range s {
			out = append(out, net.ParseIP(v))
		}
		return out
	}

	tests := []struct {
		name string
		a    []net.IP
		b    []net.IP
		want bool
	}{
		{name: "empty needle in empty haystack", a: nil, b: nil, want: false},
		{name: "empty needle in non empty haystack", a: ips("1.1.1.1"), b: nil, want: false},
		{name: "non empty needle in empty haystack", a: nil, b: ips("1.1.1.1"), want: false},
		{name: "one of many present", a: ips("1.1.1.1", "2.2.2.2"), b: ips("9.9.9.9", "2.2.2.2"), want: true},
		{name: "none present", a: ips("1.1.1.1"), b: ips("9.9.9.9"), want: false},
		{name: "ipv6 match", a: ips("fd00::1", "1.1.1.1"), b: ips("fd00::1"), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := NetIPContainAny(tt.a, tt.b); got != tt.want {
				t.Errorf("NetIPContainAny(%v, %v) = %v, want %v",
					ToStringSlice(tt.a), ToStringSlice(tt.b), got, tt.want)
			}
		})
	}
}

func TestNetIPToMapEdgeCases(t *testing.T) {
	t.Run("nil input yields empty non nil map", func(t *testing.T) {
		got := NetIPToMap(nil)
		if got == nil {
			t.Fatal("NetIPToMap(nil) = nil, want empty map")
		}
		if len(got) != 0 {
			t.Errorf("NetIPToMap(nil) has %d entries, want 0", len(got))
		}
	})

	t.Run("duplicates collapse to one entry", func(t *testing.T) {
		got := NetIPToMap([]net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("1.1.1.1")})
		if len(got) != 1 {
			t.Errorf("NetIPToMap has %d entries, want 1", len(got))
		}
	})

	t.Run("4 byte and 16 byte forms share a key", func(t *testing.T) {
		got := NetIPToMap([]net.IP{net.IPv4(10, 0, 0, 1).To4(), net.IPv4(10, 0, 0, 1).To16()})
		if len(got) != 1 {
			t.Fatalf("NetIPToMap has %d entries, want 1", len(got))
		}
		if _, ok := got["10.0.0.1"]; !ok {
			t.Errorf("NetIPToMap missing key %q, got %v", "10.0.0.1", got)
		}
	})

	t.Run("key always equals value string", func(t *testing.T) {
		got := NetIPToMap([]net.IP{net.ParseIP("1.1.1.1"), net.ParseIP("fd00::1"), net.ParseIP("::ffff:2.2.2.2")})
		if len(got) != 3 {
			t.Fatalf("NetIPToMap has %d entries, want 3", len(got))
		}
		for k, v := range got {
			if k != v.String() {
				t.Errorf("key %q does not match value %q", k, v.String())
			}
		}
	})
}

func TestNetIPv6ToMap(t *testing.T) {
	t.Run("96 bit network yields offsets 2 through 102", func(t *testing.T) {
		got := NetIPv6ToMap(mustCIDR(t, "2001:db8::/96"))
		if len(got) != 101 {
			t.Fatalf("got %d addresses, want 101", len(got))
		}
		for _, want := range []string{"2001:db8::2", "2001:db8::66"} { // 0x66 == 102
			if _, ok := got[want]; !ok {
				t.Errorf("result is missing %q", want)
			}
		}
		for _, unwanted := range []string{"2001:db8::", "2001:db8::1", "2001:db8::67"} {
			if _, ok := got[unwanted]; ok {
				t.Errorf("result unexpectedly contains %q", unwanted)
			}
		}
	})

	t.Run("keys match value strings and stay 16 bytes wide", func(t *testing.T) {
		got := NetIPv6ToMap(mustCIDR(t, "fd00::/96"))
		if len(got) == 0 {
			t.Fatal("got no addresses, want 101")
		}
		for k, v := range got {
			if k != v.String() {
				t.Errorf("key %q does not match value %q", k, v.String())
			}
			if len(v) != net.IPv6len {
				t.Errorf("address %q has %d bytes, want %d", k, len(v), net.IPv6len)
			}
		}
	})

	t.Run("network smaller than the offset range is truncated", func(t *testing.T) {
		// A /126 only holds 4 addresses, so only offsets 2 and 3 stay inside it.
		got := NetIPv6ToMap(mustCIDR(t, "2001:db8::/126"))
		if len(got) != 2 {
			t.Fatalf("got %d addresses (%v), want 2", len(got), got)
		}
		for _, want := range []string{"2001:db8::2", "2001:db8::3"} {
			if _, ok := got[want]; !ok {
				t.Errorf("result is missing %q", want)
			}
		}
	})

	t.Run("single address network yields nothing", func(t *testing.T) {
		got := NetIPv6ToMap(mustCIDR(t, "2001:db8::/128"))
		if len(got) != 0 {
			t.Errorf("got %d addresses (%v), want 0", len(got), got)
		}
	})

	t.Run("offsets are added to the network base not to zero", func(t *testing.T) {
		got := NetIPv6ToMap(mustCIDR(t, "2001:db8:0:1::/120"))
		if len(got) != 101 {
			t.Fatalf("got %d addresses, want 101", len(got))
		}
		if _, ok := got["2001:db8:0:1::2"]; !ok {
			t.Errorf("result is missing %q, got %v", "2001:db8:0:1::2", got)
		}
	})

	t.Run("ipv4 network is handled through its v4 mapped form", func(t *testing.T) {
		got := NetIPv6ToMap(mustCIDR(t, "192.168.1.0/24"))
		if len(got) != 101 {
			t.Fatalf("got %d addresses, want 101", len(got))
		}
		if _, ok := got["192.168.1.2"]; !ok {
			t.Errorf("result is missing %q", "192.168.1.2")
		}
		if _, ok := got["192.168.1.102"]; !ok {
			t.Errorf("result is missing %q", "192.168.1.102")
		}
	})

	t.Run("malformed address yields empty map", func(t *testing.T) {
		got := NetIPv6ToMap(&net.IPNet{IP: net.IP{1, 2, 3}, Mask: net.CIDRMask(96, 128)})
		if len(got) != 0 {
			t.Errorf("got %d addresses (%v), want 0", len(got), got)
		}
	})
}
