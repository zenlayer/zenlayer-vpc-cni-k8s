// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"fmt"
	"sync"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"

	zapiCom "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/common"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/ec2"
)

var defaultInstanceMetadata *InstanceMetadata
var once sync.Once

// InstanceMetadataGetter exposes the instance identity that InitInstanceMetadata
// resolved.
type InstanceMetadataGetter interface {
	GetInstanceId() string
	GetRegionId() string
	GetZoneId() string
}

type InstanceMetadata struct {
	InstanceId string
	RegionId   string
	ZoneId     string
}

// GetInstanceId returns the instance ID read from DMI (product_serial).
func (m *InstanceMetadata) GetInstanceId() string {
	return m.InstanceId
}

// GetRegionId returns the region resolved by resolveRegion, e.g.
// "asia-east-1". This, not the zone, is what the OpenAPI expects.
func (m *InstanceMetadata) GetRegionId() string {
	return m.RegionId
}

// GetZoneId returns the availability zone read from DMI, e.g. "SH1A". Nothing
// in this repository calls it: the OpenAPI models used here key on region --
// a subnet reports RegionId and no zone, and ENI creation takes a subnet ID.
func (m *InstanceMetadata) GetZoneId() string {
	return m.ZoneId
}

// InitInstanceMetadata resolves the identity of the instance this daemon runs
// on, once. It must be called before GetInstanceMetadata. A failed attempt is
// not retried: the sync.Once has already fired, so every later call reports
// "instance metadata was not initialized" instead of the original error.
//
// The OpenAPI client is required because the region is not discoverable
// locally. DMI exposes only the availability zone, as product_family ("SH1A"),
// and the metadata service answers 501 for region and placement (see
// zapi/common.GetZone) -- but the OpenAPI keys subnets and ENIs on the region
// ("asia-east-1"). The two are not interchangeable, and neither form is
// rejected as malformed: DescribeNetworkInterfaces requests carry RegionId and
// would match no ENI, and every subnet would be dropped by the region check in
// subnetManager.updateSubnets, ending as "no available subnet" from the pool.
func InitInstanceMetadata(client ec2.EC2) (InstanceMetadataGetter, error) {
	var initErr error
	once.Do(func() {
		instanceId, err := zapiCom.GetInstanceID()
		if err != nil {
			initErr = fmt.Errorf("get instanceId for instance failed, %w", err)
			return
		}
		zoneId, err := zapiCom.GetZone()
		if err != nil {
			initErr = fmt.Errorf("get zone for instance failed, %w", err)
			return
		}
		regionId, err := resolveRegion(client, instanceId)
		if err != nil {
			initErr = err
			return
		}
		defaultInstanceMetadata = &InstanceMetadata{
			InstanceId: instanceId,
			RegionId:   regionId,
			ZoneId:     zoneId,
		}
	})
	if initErr != nil {
		return nil, initErr
	}
	if defaultInstanceMetadata == nil {
		return nil, fmt.Errorf("instance metadata was not initialized")
	}
	return defaultInstanceMetadata, nil
}

// resolveRegion asks the OpenAPI which region this instance is in.
//
// DescribeInstances reports ZoneId on the instance record but no region; the
// region appears per NIC, so this takes it from the first NIC that reports one
// -- on the assumption that all NICs of an instance share a region.
func resolveRegion(client ec2.EC2, instanceId string) (string, error) {
	resp, err := client.DescribeInstances([]string{instanceId})
	if err != nil {
		return "", fmt.Errorf("describe instance %s to resolve its region: %w", instanceId, err)
	}
	if resp == nil || resp.Response == nil || len(resp.Response.DataSet) == 0 {
		return "", fmt.Errorf("describe instance %s returned no data, cannot resolve region", instanceId)
	}
	for _, nic := range resp.Response.DataSet[0].Nics {
		if region := zecCom.ToString(nic.RegionId); region != "" {
			return region, nil
		}
	}
	return "", fmt.Errorf("instance %s reports no region on any of its NICs", instanceId)
}

// GetInstanceMetadata returns the information resolved by InitInstanceMetadata.
// It returns nil if that has not run, or did not succeed.
func GetInstanceMetadata() InstanceMetadataGetter {
	if defaultInstanceMetadata == nil {
		return nil
	}
	return defaultInstanceMetadata
}
