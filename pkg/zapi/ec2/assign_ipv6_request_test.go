// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package ec2

import (
	"strconv"
	"testing"
)

func strOrNil(p *string) string {
	if p == nil {
		return "<unset>"
	}
	return *p
}

func intOrNil(p *int) string {
	if p == nil {
		return "<unset>"
	}
	return strconv.Itoa(*p)
}

// TestBuildAssignIPv6RequestSendsOnlyWhatTheChargeTypeUses asserts a request
// carries the billing fields of its charge type and no others. A Public-IPv6
// subnet -- every dual-stack subnet seen so far -- refuses the assign with
// MISSING_PARAMETER: internetChargeType, so these fields decide whether a node
// can carve IPv6 at all.
//
// The regression: after an hourly-billing rejection an operator switches the
// charge type but leaves the package size behind. Sending both keeps the
// platform refusing for the same reason, with nothing to show why.
func TestBuildAssignIPv6RequestSendsOnlyWhatTheChargeTypeUses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		billing IPv6Billing
		// "<unset>" means the field must not be sent at all
		wantChargeType string
		wantPackage    bool
		wantCluster    string
		wantBandwidth  bool
	}{
		{
			// A Private-IPv6 subnet needs none of this and the platform ignores
			// the fields there. Sending nothing keeps a misconfigured value
			// from mattering on a subnet that never needed one.
			name:           "no charge type configured sends no billing at all",
			billing:        IPv6Billing{},
			wantChargeType: "<unset>",
			wantCluster:    "<unset>",
		},
		{
			name: "ByTrafficPackage sends the package size",
			billing: IPv6Billing{
				InternetChargeType: chargeByTrafficPackage,
				PackageSizeTB:      5,
				BandwidthMbps:      100,
			},
			wantChargeType: chargeByTrafficPackage,
			wantPackage:    true,
			wantBandwidth:  true,
			wantCluster:    "<unset>",
		},
		{
			name: "BandwidthCluster sends the cluster id",
			billing: IPv6Billing{
				InternetChargeType: chargeBandwidthCluster,
				ClusterId:          "cluster-1",
				BandwidthMbps:      200,
			},
			wantChargeType: chargeBandwidthCluster,
			wantBandwidth:  true,
			wantCluster:    "cluster-1",
		},
		{
			// A package size left over from the previous charge type must not
			// ride along, or the switch the operator was told to make changes
			// nothing.
			name: "ByBandwidth drops a package size left over from a previous charge type",
			billing: IPv6Billing{
				InternetChargeType: "ByBandwidth",
				PackageSizeTB:      5,
				BandwidthMbps:      100,
			},
			wantChargeType: "ByBandwidth",
			wantPackage:    false,
			wantBandwidth:  true,
			wantCluster:    "<unset>",
		},
		{
			// The same in the other direction: a cluster id has no meaning
			// under ByTrafficPackage.
			name: "ByTrafficPackage drops a stale cluster id",
			billing: IPv6Billing{
				InternetChargeType: chargeByTrafficPackage,
				PackageSizeTB:      2,
				ClusterId:          "cluster-stale",
			},
			wantChargeType: chargeByTrafficPackage,
			wantPackage:    true,
			wantCluster:    "<unset>",
		},
		{
			// And a package size has no meaning under BandwidthCluster.
			name: "BandwidthCluster drops a stale package size",
			billing: IPv6Billing{
				InternetChargeType: chargeBandwidthCluster,
				ClusterId:          "cluster-2",
				PackageSizeTB:      9,
			},
			wantChargeType: chargeBandwidthCluster,
			wantPackage:    false,
			wantCluster:    "cluster-2",
		},
		{
			// Zero is "not configured", not "cap it at zero" -- a zero cap sent
			// as a value would be a throughput limit the operator never asked
			// for.
			name: "a zero bandwidth is not sent as a cap",
			billing: IPv6Billing{
				InternetChargeType: chargeByTrafficPackage,
				PackageSizeTB:      1,
				BandwidthMbps:      0,
			},
			wantChargeType: chargeByTrafficPackage,
			wantPackage:    true,
			wantBandwidth:  false,
			wantCluster:    "<unset>",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := buildAssignIPv6Request("nic-1", tc.billing)

			if strOrNil(req.NicId) != "nic-1" {
				t.Errorf("NicId = %s, want nic-1", strOrNil(req.NicId))
			}
			if got := strOrNil(req.InternetChargeType); got != tc.wantChargeType {
				t.Errorf("InternetChargeType = %s, want %s", got, tc.wantChargeType)
			}
			if got := req.PackageSize != nil; got != tc.wantPackage {
				t.Errorf("PackageSize sent = %v, want %v: a package size that does not belong to "+
					"this charge type keeps the platform refusing for the reason the operator was "+
					"told to fix", got, tc.wantPackage)
			}
			if got := strOrNil(req.ClusterId); got != tc.wantCluster {
				t.Errorf("ClusterId = %s, want %s", got, tc.wantCluster)
			}
			if got := req.Bandwidth != nil; got != tc.wantBandwidth {
				t.Errorf("Bandwidth sent = %v (%s), want %v", got, intOrNil(req.Bandwidth), tc.wantBandwidth)
			}
		})
	}
}
