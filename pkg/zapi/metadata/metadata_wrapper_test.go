// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package metadata

import (
	"reflect"
	"testing"
)

// The listing format is what the platform's metadata service answers on
// network/interfaces/macs/ (seen on a test node, 2026-11-07).
func TestParseMACListing(t *testing.T) {
	got := parseMACListing("52:54:00:36:2a:02/\n52:54:00:9d:39:5a/\n")
	want := []string{"52:54:00:36:2a:02", "52:54:00:9d:39:5a"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	if got := parseMACListing(""); len(got) != 0 {
		t.Fatalf("empty listing gave %v", got)
	}
}
