// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package zlog

import "testing"

func TestTrimPath(t *testing.T) {
	cases := map[string]string{
		"/go/src/app/pkg/daemon/eni_multi_ip.go": "daemon/eni_multi_ip.go",
		"/a/b.go":                                "a/b.go",
		"b.go":                                   "b.go",
	}
	for in, want := range cases {
		if got := trimPath(in); got != want {
			t.Errorf("trimPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestPkgPrefixResolved guards the frame walk: pkgPrefix is what keeps it from
// stopping inside this package, and if it fails to resolve every log line is
// attributed to zlog.go.
func TestPkgPrefixResolved(t *testing.T) {
	if pkgPrefix == "" {
		t.Fatal("pkgPrefix did not resolve")
	}
	if !isWrapperFrame(pkgPrefix + "someFunc") {
		t.Errorf("frames in this package should be treated as wrappers")
	}
	if isWrapperFrame("github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon.AllocIP") {
		t.Errorf("caller frames must not be treated as wrappers")
	}
	// The external test package must not be mistaken for this one.
	if isWrapperFrame(pkgPrefix[:len(pkgPrefix)-1] + "_test.TestX") {
		t.Errorf("the _test package must not be treated as a wrapper")
	}
}
