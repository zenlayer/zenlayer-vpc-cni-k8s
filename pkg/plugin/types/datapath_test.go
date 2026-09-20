// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"strings"
	"testing"
)

// The table in dataPathNames is the contract. These tests are written against
// it rather than against a hardcoded list, so adding a datapath extends the
// coverage instead of needing new cases -- and a datapath added without a name
// fails here immediately.
func TestEveryDataPathHasAName(t *testing.T) {
	for _, dp := range AllDataPaths() {
		name := dp.String()
		if name == "" || strings.HasPrefix(name, "unknown") {
			t.Errorf("DataPathType(%d) has no name in dataPathNames, String() = %q", int(dp), name)
		}
	}
}

func TestParseDataPathRoundTrip(t *testing.T) {
	for _, dp := range AllDataPaths() {
		name := dp.String()
		for _, spelling := range []string{name, strings.ToUpper(name), capitalize(name)} {
			got, err := ParseDataPath(spelling)
			if err != nil {
				t.Errorf("ParseDataPath(%q) unexpected error: %v", spelling, err)
				continue
			}
			if got != dp {
				t.Errorf("ParseDataPath(%q) = %v, want %v", spelling, got, dp)
			}
		}
	}
}

// An absent eniip_virtual_type has to resolve to a real datapath, and never to
// the zero value.
func TestParseDataPathEmptyIsTheDefault(t *testing.T) {
	got, err := ParseDataPath("")
	if err != nil {
		t.Fatalf("ParseDataPath(\"\") unexpected error: %v", err)
	}
	if got == DataPathUnknown {
		t.Fatal("ParseDataPath(\"\") = DataPathUnknown, want a usable datapath")
	}
	if got != defaultDataPath {
		t.Errorf("ParseDataPath(\"\") = %v, want %v", got, defaultDataPath)
	}
}

func TestParseDataPathRejectsUnknown(t *testing.T) {
	// Unsupported names, plus " veth " and "veth0" as near misses on the one
	// real name. Each must be refused rather than defaulted: a node running a
	// datapath other than the configured one is far harder to diagnose than a
	// pod that will not start.
	for _, name := range []string{"bridge", "macvlan", " veth ", "veth0"} {
		got, err := ParseDataPath(name)
		if err == nil {
			t.Errorf("ParseDataPath(%q) = %v, want an error", name, got)
			continue
		}
		if got != DataPathUnknown {
			t.Errorf("ParseDataPath(%q) = %v alongside an error, want DataPathUnknown", name, got)
		}
		// The error has to tell the operator what is accepted.
		for _, accepted := range AcceptedDataPathNames() {
			if !strings.Contains(err.Error(), accepted) {
				t.Errorf("ParseDataPath(%q) error %q does not mention accepted name %q", name, err, accepted)
			}
		}
	}
}

// DataPathUnknown must stay unusable: it is the zero value, and code that skips
// an error must not end up with a working datapath by accident.
func TestDataPathUnknownIsNotUsable(t *testing.T) {
	if _, ok := dataPathNames[DataPathUnknown]; ok {
		t.Fatal("DataPathUnknown has a name in dataPathNames, so it can be selected from a conflist")
	}
	for _, name := range AcceptedDataPathNames() {
		if got, _ := ParseDataPath(name); got == DataPathUnknown {
			t.Errorf("ParseDataPath(%q) resolved to DataPathUnknown", name)
		}
	}
}

func TestAcceptedDataPathNamesIsStable(t *testing.T) {
	first := AcceptedDataPathNames()
	second := AcceptedDataPathNames()
	if len(first) != len(dataPathNames) {
		t.Errorf("AcceptedDataPathNames() returned %d names, want %d", len(first), len(dataPathNames))
	}
	for i := range first {
		if first[i] != second[i] {
			t.Fatalf("AcceptedDataPathNames() is not stable: %v then %v", first, second)
		}
	}
}

// capitalize upper-cases the first byte and leaves the rest alone. The datapath
// names are lowercase ASCII, so this covers the mixed-case spelling the round
// trip needs without strings.Title, which is deprecated for mishandling Unicode
// word boundaries.
func capitalize(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}
