// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package providerid

import "testing"

func TestFormat(t *testing.T) {
	// The cloud controller manager writes the same string; see the package doc.
	if got := Format("asia-east-1", "1755516676029420392"); got != "zenlayer:///asia-east-1/1755516676029420392" {
		t.Fatalf("Format = %q", got)
	}
}

func TestRoundTrip(t *testing.T) {
	region, id, err := Parse(Format("asia-east-1", "1755516676029420392"))
	if err != nil || region != "asia-east-1" || id != "1755516676029420392" {
		t.Fatalf("got %q %q, %v", region, id, err)
	}
}

// Nodes 2.0.0 already wrote keep being read; the field cannot be changed.
func TestParseLegacy(t *testing.T) {
	region, id, err := Parse("zenlayer://1755516676029420392")
	if err != nil || region != "" || id != "1755516676029420392" {
		t.Fatalf("got %q %q, %v", region, id, err)
	}
}

func TestParseRejects(t *testing.T) {
	for _, in := range []string{
		"", "zenlayer://", "zenlayer:///", "zenlayer:///region", "zenlayer:///region/", "zenlayer:///region/1/2", "zenlayer:////1",
		"aws:///us-east-1a/i-1", "zenlayer://a b", "zenlayer:///re gion/1", "zenlayer://region/1", "1755516676029420392",
	} {
		if r, id, err := Parse(in); err == nil {
			t.Errorf("Parse(%q) = %q %q, accepted", in, r, id)
		}
	}
}
