// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package ec2 is the client for the platform's VPC API.
//
// It covers the calls IPAM needs: creating, attaching, detaching and deleting
// network interfaces, assigning and unassigning their addresses in both
// families, and describing the instance, its interfaces and its subnets. What
// bounds any of that -- how many interfaces and addresses this instance type
// allows -- is not asked of the API here; it is derived from the instance
// description in pkg/daemon/factory.
//
// The interface it exposes is what the daemon's factory depends on, so tests can
// substitute a fake without reaching the network.
package ec2

import (
	"os"
	"strconv"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
)

// envSDKHTTPDump turns on the SDK's raw HTTP dump. Off by default, and
// deliberately NOT tied to logLevel.
//
// The dump prints the full header block of every request, including
// `Authorization: ZC2-HMAC-SHA256 Credential=<AccessKeyID>, ... Signature=...`,
// so it writes a long-lived static credential into the pod log once per API
// call. Tying it to debug would put the leak on for the whole testing round,
// and the dump bypasses zlog (stdlib logger), so no filter can catch it.
const envSDKHTTPDump = "ZENLAYER_CNI_SDK_HTTP_DUMP"

// sdkHTTPDumpEnabled reports whether the caller explicitly asked for the dump.
// An unparsable value counts as off -- the safe direction for a switch whose
// on-state leaks credentials.
func sdkHTTPDumpEnabled() bool {
	v := os.Getenv(envSDKHTTPDump)
	if v == "" {
		return false
	}
	enabled, err := strconv.ParseBool(v)
	if err != nil {
		log.Errorf("Failed to parse %s=%q, keeping the SDK HTTP dump off: %v", envSDKHTTPDump, v, err)
		return false
	}
	if enabled {
		log.Warnf("%s is on: the SDK will write every request's Authorization header, "+
			"including the AccessKeyID, to this log in plaintext. Non-production use only.", envSDKHTTPDump)
	}
	return enabled
}

// apiAttemptTimeoutSec bounds one HTTP attempt. Retries are layered above it,
// so this is not the budget for an operation -- it is how long to wait before
// concluding a single request will not be answered.
const apiAttemptTimeoutSec = 20

var log = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "ZecClient"})

type ZecClient struct {
	zecClient *zecApi.Client
	isLocal   bool
}

// NewZecClient builds the OpenAPI client. domain overrides the host requests
// are sent to; empty keeps the SDK default (common.DOMAIN, the public
// production endpoint). The SDK exposes Domain as a plain field rather than a
// With* option, so it is assigned directly.
func NewZecClient(secretKeyId, secretKeyPassword, domain string) (*ZecClient, error) {
	var (
		err       error
		zecClient *zecApi.Client
	)
	// AutoRetry off: retries live in this repo's ExponentialBackoff wrappers,
	// which read the error code and stop once a rejection is final. The SDK
	// retries blindly and, with RetryDuration at zero, without pausing. Its
	// rate-limit layer stays on -- that one reads HTTP 429 semantics this side
	// cannot see.
	//
	// The timeout is well under a CNI ADD's budget, since an attempt outliving
	// daemonRPCTimeout cannot help the pod waiting on it -- but not tighter,
	// because abandoning a write is worse than waiting for one: the platform has
	// applied an assign whose response never arrived, leaving an address nothing
	// here knows it owns.
	config := zecCom.NewConfig().WithTimeout(apiAttemptTimeoutSec).WithAutoRetry(false)
	config.Debug = zecCom.Bool(sdkHTTPDumpEnabled())
	if domain != "" {
		config.Domain = domain
		log.Infof("Zenlayer OpenAPI domain overridden to %s", domain)
	}
	zecClient, err = zecApi.NewClient(config, secretKeyId, secretKeyPassword)
	if err != nil || zecClient == nil {
		log.Errorf("Create zenlayer api client failed, %v", err)
		return nil, err
	}
	cm := &ZecClient{
		zecClient: zecClient,
		isLocal:   true,
	}
	return cm, nil
}

func (c *ZecClient) DescribeInstances(instanceIds []string) (*zecApi.DescribeInstancesResponse, error) {
	req := zecApi.NewDescribeInstancesRequest()
	req.InstanceIds = instanceIds
	resp, err := c.zecClient.DescribeInstances(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *ZecClient) DescribeSubnets(subnetIds []string) (*zecApi.DescribeSubnetsResponse, error) {
	req := zecApi.NewDescribeSubnetsRequest()
	req.SubnetIds = subnetIds
	resp, err := c.zecClient.DescribeSubnets(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *ZecClient) DescribeVpcs(vpcIds []string) (*zecApi.DescribeVpcsResponse, error) {
	req := zecApi.NewDescribeVpcsRequest()
	req.VpcIds = vpcIds
	resp, err := c.zecClient.DescribeVpcs(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *ZecClient) AssignPrivateIpAddress(nicId string, addresses []string, count int) (*zecApi.BatchAssignNetworkInterfaceIpv4Response, error) {
	req := zecApi.NewBatchAssignNetworkInterfaceIpv4Request()
	req.NicId = zecCom.String(nicId)
	req.IpAddresses = addresses
	if len(addresses) == 0 {
		req.IpAddressCount = &count
	}
	resp, err := c.zecClient.BatchAssignNetworkInterfaceIpv4(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *ZecClient) UnAssignPrivateIpAddress(nicId string, addresses []string) (*zecApi.UnassignNetworkInterfaceIpv4Response, error) {
	req := zecApi.NewUnassignNetworkInterfaceIpv4Request()
	req.NicId = zecCom.String(nicId)
	req.IpAddresses = addresses
	resp, err := c.zecClient.UnassignNetworkInterfaceIpv4(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// buildAssignIPv6Request assembles the assign request from the operator's
// billing configuration. Split from the call below so it can be tested alone.
//
// Fields are sent per charge type, not per field-is-set: leaving a committed
// package size alongside ByBandwidth is how switching charge type fails to
// clear a billing rejection. Nothing is sent when no charge type is configured,
// so a Private-IPv6 subnet is not handed values it cannot use.
func buildAssignIPv6Request(nicId string, billing IPv6Billing) *zecApi.AssignNetworkInterfaceIpv6Request {
	req := zecApi.NewAssignNetworkInterfaceIpv6Request()
	req.NicId = zecCom.String(nicId)
	if billing.InternetChargeType != "" {
		req.InternetChargeType = zecCom.String(billing.InternetChargeType)
		if billing.BandwidthMbps > 0 {
			bw := billing.BandwidthMbps
			req.Bandwidth = &bw
		}
		switch billing.InternetChargeType {
		case chargeByTrafficPackage:
			if billing.PackageSizeTB > 0 {
				sz := billing.PackageSizeTB
				req.PackageSize = &sz
			}
		case chargeBandwidthCluster:
			if billing.ClusterId != "" {
				req.ClusterId = zecCom.String(billing.ClusterId)
			}
		}
	}
	return req
}

func (c *ZecClient) AssignNetworkInterfaceIpv6(nicId string, billing IPv6Billing) (*zecApi.AssignNetworkInterfaceIpv6Response, error) {
	resp, err := c.zecClient.AssignNetworkInterfaceIpv6(buildAssignIPv6Request(nicId, billing))
	if err != nil {
		return nil, err
	}
	return resp, nil
}

// UnassignNetworkInterfaceIpv6 removes a NIC's IPv6, undoing
// AssignNetworkInterfaceIpv6.
//
// Wired because enabling IPv6 on a NIC is otherwise a one-way door, and the CNI
// opens it on the instance's own interface: the primary interface serves pods,
// so PreStart asks for a prefix on it like any other. Without the inverse there
// is no way to put a node back, which also makes the feature untestable without
// permanently changing the machine.
//
// Nothing calls this automatically. Removing IPv6 from the instance's own
// interface is not a side effect any reconcile should have -- it is the node's
// own addressing.
func (c *ZecClient) UnassignNetworkInterfaceIpv6(nicId string) (*zecApi.UnassignNetworkInterfaceIpv6Response, error) {
	req := zecApi.NewUnassignNetworkInterfaceIpv6Request()
	req.NicId = zecCom.String(nicId)
	resp, err := c.zecClient.UnassignNetworkInterfaceIpv6(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *ZecClient) DescribeNetworkInterfaceAttributes(req *zecApi.DescribeNetworkInterfacesRequest) (*zecApi.DescribeNetworkInterfacesResponse, error) {
	resp, err := c.zecClient.DescribeNetworkInterfaces(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *ZecClient) CreateNetworkInterface(name, subnetId, securityGroupId string, tags map[string]string) (*zecApi.CreateNetworkInterfaceResponse, error) {
	req := zecApi.NewCreateNetworkInterfaceRequest()
	req.Name = zecCom.String(name)
	req.SubnetId = zecCom.String(subnetId)
	if securityGroupId != "" {
		req.SecurityGroupId = zecCom.String(securityGroupId)
	}
	if len(tags) > 0 {
		assoc := &zecApi.TagAssociation{}
		for k, v := range tags {
			assoc.Tags = append(assoc.Tags, &zecApi.Tag{
				Key:   zecCom.String(k),
				Value: zecCom.String(v),
			})
		}
		req.Tags = assoc
	}
	resp, err := c.zecClient.CreateNetworkInterface(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *ZecClient) AttachNetworkInterface(nicId, instanceId string) (*zecApi.AttachNetworkInterfaceResponse, error) {
	req := zecApi.NewAttachNetworkInterfaceRequest()
	req.NicId = zecCom.String(nicId)
	req.InstanceId = zecCom.String(instanceId)
	resp, err := c.zecClient.AttachNetworkInterface(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *ZecClient) DetachNetworkInterface(nicId string) (*zecApi.DetachNetworkInterfaceResponse, error) {
	req := zecApi.NewDetachNetworkInterfaceRequest()
	req.NicId = zecCom.String(nicId)
	resp, err := c.zecClient.DetachNetworkInterface(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}

func (c *ZecClient) DeleteNetworkInterface(nicId string) (*zecApi.DeleteNetworkInterfaceResponse, error) {
	req := zecApi.NewDeleteNetworkInterfaceRequest()
	req.NicId = zecCom.String(nicId)
	resp, err := c.zecClient.DeleteNetworkInterface(req)
	if err != nil {
		return nil, err
	}
	return resp, nil
}
