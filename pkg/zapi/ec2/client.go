// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package ec2

import (
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"
)

// The two charge types that need a parameter beyond the bandwidth cap. Mirrored
// in the daemon's config package, which is what validates the operator's choice;
// ByBandwidth needs nothing extra and so has no constant here.
const (
	chargeByTrafficPackage = "ByTrafficPackage"
	chargeBandwidthCluster = "BandwidthCluster"
)

// IPv6Billing carries the billing parameters sent with an IPv6 assign.
//
// Required when the subnet's ipv6Type is Public: the assign is otherwise
// refused with MISSING_PARAMETER: internetChargeType. The daemon builds one
// from its configuration and passes it on every assign; buildAssignIPv6Request
// sends nothing when no charge type is set.
type IPv6Billing struct {
	// InternetChargeType is ByTrafficPackage, ByBandwidth or BandwidthCluster.
	InternetChargeType string
	// BandwidthMbps caps the interface's public IPv6 throughput.
	BandwidthMbps int
	// PackageSizeTB is required when InternetChargeType is ByTrafficPackage.
	PackageSizeTB float64
	// ClusterId is required when InternetChargeType is BandwidthCluster.
	ClusterId string
}

// EC2 is the set of platform API calls the CNI makes. ZecClient implements it
// over the cloud SDK; metrics.MetricEC2Wrapper decorates it, and tests fake it.
type EC2 interface {
	DescribeInstances(instanceIds []string) (*zecApi.DescribeInstancesResponse, error)
	DescribeSubnets(subnetIds []string) (*zecApi.DescribeSubnetsResponse, error)
	DescribeVpcs(vpcIds []string) (*zecApi.DescribeVpcsResponse, error)
	AssignPrivateIpAddress(nicId string, addresses []string, count int) (*zecApi.BatchAssignNetworkInterfaceIpv4Response, error)
	UnAssignPrivateIpAddress(nicId string, addresses []string) (*zecApi.UnassignNetworkInterfaceIpv4Response, error)
	// AssignNetworkInterfaceIpv6 enables IPv6 on a NIC: the platform gives it a
	// primary IPv6 address and an IPv6 prefix for the CNI to carve pod addresses
	// from. UnassignNetworkInterfaceIpv6 is the inverse.
	//
	// This pair is the whole IPv6 address API. There is no per-address assign or
	// unassign the way there is for IPv4 -- pod IPv6 addresses never reach the
	// platform at all.
	AssignNetworkInterfaceIpv6(nicId string, billing IPv6Billing) (*zecApi.AssignNetworkInterfaceIpv6Response, error)
	UnassignNetworkInterfaceIpv6(nicId string) (*zecApi.UnassignNetworkInterfaceIpv6Response, error)
	DescribeNetworkInterfaceAttributes(req *zecApi.DescribeNetworkInterfacesRequest) (*zecApi.DescribeNetworkInterfacesResponse, error)
	CreateNetworkInterface(name, subnetId, securityGroupId string, tags map[string]string) (*zecApi.CreateNetworkInterfaceResponse, error)
	AttachNetworkInterface(nicId, instanceId string) (*zecApi.AttachNetworkInterfaceResponse, error)
	DetachNetworkInterface(nicId string) (*zecApi.DetachNetworkInterfaceResponse, error)
	DeleteNetworkInterface(nicId string) (*zecApi.DeleteNetworkInterfaceResponse, error)
}
