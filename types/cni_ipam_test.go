// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package types

import (
	"encoding/json"
	"testing"
)

// TestIsIPAMOnlyFromConflist pins the mode decision against the two conflist
// shapes it has to tell apart, parsed the way the plugin parses them.
//
// The fallback case is the one worth pinning: a conflist that names this binary
// as another plugin's ipam but forgets the marker must not take the full path.
// That mistake produces a pod with two datapaths -- a veth this binary built
// and whatever the caller built -- and no error anywhere.
func TestIsIPAMOnlyFromConflist(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stdin string
		want  bool
	}{
		{
			name:  "top level plugin, no ipam block",
			stdin: `{"cniVersion":"0.4.0","name":"zec-pod-network","type":"zec-cni"}`,
			want:  false,
		},
		{
			name: "top level plugin that also has an ipam block naming something else",
			stdin: `{"cniVersion":"0.4.0","name":"zec-pod-network","type":"zec-cni",
			         "ipam":{"type":"host-local"}}`,
			want: false,
		},
		{
			name: "delegated with the marker",
			stdin: `{"cniVersion":"0.4.0","name":"zec-pod-network","type":"cilium-cni",
			         "ipam":{"type":"zec-cni","zec_mode":"ipam-only"}}`,
			want: true,
		},
		{
			name: "delegated without the marker: inferred, not ignored",
			stdin: `{"cniVersion":"0.4.0","name":"zec-pod-network","type":"cilium-cni",
			         "ipam":{"type":"zec-cni"}}`,
			want: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var conf CNIConf
			if err := json.Unmarshal([]byte(tc.stdin), &conf); err != nil {
				t.Fatalf("parse conflist: %v", err)
			}
			if got := conf.IsIPAMOnly(); got != tc.want {
				t.Errorf("IsIPAMOnly() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestValidateIPAMRejectsUnknownMode pins that a typo is refused rather than
// quietly meaning "full plugin". A node running the wrong datapath while its
// configuration says otherwise is the failure this prevents.
func TestValidateIPAMRejectsUnknownMode(t *testing.T) {
	for _, tc := range []struct {
		name    string
		stdin   string
		wantErr bool
	}{
		{"absent", `{"type":"zec-cni"}`, false},
		{"the one accepted value", `{"ipam":{"zec_mode":"ipam-only"}}`, false},
		{"a typo", `{"ipam":{"zec_mode":"ipam_only"}}`, true},
		{"something invented", `{"ipam":{"zec_mode":"delegated"}}`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var conf CNIConf
			if err := json.Unmarshal([]byte(tc.stdin), &conf); err != nil {
				t.Fatalf("parse conflist: %v", err)
			}
			err := conf.ValidateIPAM()
			if (err != nil) != tc.wantErr {
				t.Errorf("ValidateIPAM() error = %v, wantErr %v", err, tc.wantErr)
			}
		})
	}
}

// TestInstallHostRoutesDefaultsOn pins that an absent key means on.
//
// Off is the dangerous value: with no per-ENI ip rules, a pod whose address
// sits on a secondary interface leaves through the instance's own one, carrying
// a source address that belongs to a different port's MAC. A fabric that checks
// source addresses drops that, and the pod simply has no connectivity. Defaults
// have to fail towards the configuration that works.
func TestInstallHostRoutesDefaultsOn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stdin string
		want  bool
	}{
		{"absent", `{"ipam":{"type":"zec-cni","zec_mode":"ipam-only"}}`, true},
		{"explicit true", `{"ipam":{"zec_mode":"ipam-only","install_host_routes":true}}`, true},
		{"explicit false", `{"ipam":{"zec_mode":"ipam-only","install_host_routes":false}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var conf CNIConf
			if err := json.Unmarshal([]byte(tc.stdin), &conf); err != nil {
				t.Fatalf("parse conflist: %v", err)
			}
			if got := conf.InstallHostRoutes(); got != tc.want {
				t.Errorf("InstallHostRoutes() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestIsIPAMOnlyNilReceiver keeps the mode decision safe on a nil config: the
// full path is the safer answer, since it is the one that fails loudly rather
// than silently producing a pod nobody wired up.
func TestIsIPAMOnlyNilReceiver(t *testing.T) {
	var conf *CNIConf
	if conf.IsIPAMOnly() {
		t.Error("a nil config must not select the ipam-only path")
	}
	if !conf.InstallHostRoutes() {
		t.Error("a nil config must still default host routing on")
	}
}
