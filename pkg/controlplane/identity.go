// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"fmt"
	"net"
	"strings"

	corev1 "k8s.io/api/core/v1"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/apis/network/v1alpha1"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/providerid"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
)

const nicTypePrimary = "primary"

// instanceLookup is the slice of the platform identity verification needs.
type instanceLookup interface {
	DescribeInstances(ids []string) (*zecApi.DescribeInstancesResponse, error)
	DescribeInstancesByIPv4(ip string, pageNum int) (*zecApi.DescribeInstancesResponse, error)
}

// identity is a node's verified instance.
type identity struct {
	instanceID string
	regionID   string
	zoneID     string
	// fromLookup: providerID was empty and the instance was found by the
	// Node's address, so the caller may write providerID.
	fromLookup bool
}

// verifyError carries the InstanceVerified reason of a failed verification.
type verifyError struct {
	reason string
	msg    string
	// warn: worth a Warning event, as opposed to waiting on something that
	// will arrive on its own (the daemon writing its claim).
	warn bool
	// inconclusive: the check could not be made (the platform or the API
	// server did not answer). It says nothing about the identity, so an
	// existing binding and its InstanceVerified condition stand.
	inconclusive bool
}

func (e *verifyError) Error() string { return e.reason + ": " + e.msg }

func failVerify(reason string, warn bool, format string, args ...any) *verifyError {
	return &verifyError{reason: reason, warn: warn, msg: fmt.Sprintf(format, args...)}
}

// internalIP returns the Node's first IPv4 InternalIP; a dual-stack node may
// list its IPv6 first.
func internalIP(node *corev1.Node) string {
	for _, a := range node.Status.Addresses {
		if a.Type != corev1.NodeInternalIP {
			continue
		}
		if ip := net.ParseIP(a.Address); ip != nil && ip.To4() != nil {
			return a.Address
		}
	}
	return ""
}

// verifyIdentity decides which instance node is, from two independent
// accounts that must agree: the platform's (found by providerID, or by the
// Node's InternalIP when providerID is empty) and the daemon's own claim,
// which an admission policy lets only that node's daemon write.
//
// Neither account is trusted alone. The address lookup is a prefix match that
// also hits secondary and public addresses and other VPCs; only an exact
// primary-address match inside the cluster VPC counts. The claim is the
// node's word. A node taken over before its first verification controls both
// its InternalIP and its claim, which no check here can see through.
func verifyIdentity(api instanceLookup, cfg *Config, node *corev1.Node, claim v1alpha1.IdentityClaim) (*identity, *verifyError) {
	if claim.InstanceID == "" {
		return nil, failVerify(v1alpha1.ReasonIdentityClaimMissing, false,
			"the node's daemon has not written spec.identity yet")
	}
	ip := internalIP(node)
	if ip == "" {
		return nil, failVerify(v1alpha1.ReasonInstanceNotFound, false, "node %s has no InternalIP yet", node.Name)
	}

	id := &identity{}
	// claimedRegion is the region providerID names; empty when it names none
	// (found by address, or written in the 2.0.0 form without a region).
	var claimedRegion string
	switch {
	case node.Spec.ProviderID != "":
		region, instanceID, err := providerid.Parse(node.Spec.ProviderID)
		if err != nil {
			return nil, failVerify(v1alpha1.ReasonProviderIDUnparseable, true, "%v", err)
		}
		id.instanceID, claimedRegion = instanceID, region
	case cfg.Identity.ProviderID == IdentityManual:
		return nil, failVerify(v1alpha1.ReasonProviderIDMissing, true,
			"node %s has no spec.providerID and identity.providerID is manual", node.Name)
	default:
		found, verr := lookupByPrimaryIP(api, cfg.VpcID, ip)
		if verr != nil {
			return nil, verr
		}
		id.instanceID = found
		id.fromLookup = true
	}

	resp, err := api.DescribeInstances([]string{id.instanceID})
	if err != nil {
		return nil, platformFailure(fmt.Sprintf("describe instance %s", id.instanceID), err)
	}
	if resp == nil || resp.Response == nil || len(resp.Response.DataSet) != 1 ||
		zecCom.ToString(resp.Response.DataSet[0].InstanceId) != id.instanceID {
		return nil, failVerify(v1alpha1.ReasonInstanceNotFound, true, "instance %s does not exist", id.instanceID)
	}
	inst := resp.Response.DataSet[0]
	primary := primaryNIC(inst)
	if primary == nil {
		return nil, failVerify(v1alpha1.ReasonInstanceNotFound, true, "instance %s reports no primary interface", id.instanceID)
	}
	id.zoneID = zecCom.ToString(inst.ZoneId)
	id.regionID = zecCom.ToString(primary.RegionId)

	mac := zecCom.ToString(primary.MacAddress)
	primaryIP := zecCom.ToString(primary.PrimaryIpv4)
	switch {
	case claimedRegion != "" && claimedRegion != id.regionID:
		return nil, failVerify(v1alpha1.ReasonIdentityMismatch, true,
			"node %s's providerID names region %s but instance %s is in %s", node.Name, claimedRegion, id.instanceID, id.regionID)
	case cfg.RegionID != "" && id.regionID != "" && id.regionID != cfg.RegionID:
		// One VPC can span regions, but the pod subnets and every interface
		// this controlplane creates are in its own: such a node could be
		// verified and then never get an address.
		return nil, failVerify(v1alpha1.ReasonIdentityMismatch, true,
			"instance %s is in region %s but the controlplane serves region %s (controlplane.regionID)",
			id.instanceID, id.regionID, cfg.RegionID)
	case zecCom.ToString(primary.VpcId) != cfg.VpcID:
		return nil, failVerify(v1alpha1.ReasonIdentityMismatch, true,
			"instance %s is in vpc %s, not the cluster's %s", id.instanceID, zecCom.ToString(primary.VpcId), cfg.VpcID)
	case primaryIP != ip:
		return nil, failVerify(v1alpha1.ReasonIdentityMismatch, true,
			"instance %s's primary address is %s but node %s reports InternalIP %s", id.instanceID, primaryIP, node.Name, ip)
	case claim.InstanceID != id.instanceID:
		return nil, failVerify(v1alpha1.ReasonIdentityMismatch, true,
			"the platform says node %s is instance %s but its daemon reads %s from DMI", node.Name, id.instanceID, claim.InstanceID)
	case !strings.EqualFold(claim.PrimaryMAC, mac):
		return nil, failVerify(v1alpha1.ReasonIdentityMismatch, true,
			"instance %s's primary interface is %s but the daemon reports %s", id.instanceID, mac, claim.PrimaryMAC)
	case claim.PrimaryIP != primaryIP:
		return nil, failVerify(v1alpha1.ReasonIdentityMismatch, true,
			"instance %s's primary address is %s but the daemon reports %s", id.instanceID, primaryIP, claim.PrimaryIP)
	}
	return id, nil
}

// instanceExists reports whether the platform still has the instance. One in
// any state counts, a stopped or recycled one included: only a gone instance
// frees the node name bound to it.
func instanceExists(api instanceLookup, id string) (bool, *verifyError) {
	resp, err := api.DescribeInstances([]string{id})
	if err != nil {
		return false, platformFailure(fmt.Sprintf("describe instance %s", id), err)
	}
	return resp != nil && resp.Response != nil && len(resp.Response.DataSet) == 1 &&
		zecCom.ToString(resp.Response.DataSet[0].InstanceId) == id, nil
}

// lookupByPrimaryIP finds the one instance in vpcID whose primary interface's
// primary address is ip.
func lookupByPrimaryIP(api instanceLookup, vpcID, ip string) (string, *verifyError) {
	var hits []string
	seen := 0
	for page := 1; ; page++ {
		resp, err := api.DescribeInstancesByIPv4(ip, page)
		if err != nil {
			return "", platformFailure("look up instance by "+ip, err)
		}
		if resp == nil || resp.Response == nil {
			break
		}
		for _, inst := range resp.Response.DataSet {
			p := primaryNIC(inst)
			if p != nil && zecCom.ToString(p.VpcId) == vpcID && zecCom.ToString(p.PrimaryIpv4) == ip {
				hits = append(hits, zecCom.ToString(inst.InstanceId))
			}
		}
		seen += len(resp.Response.DataSet)
		if len(resp.Response.DataSet) == 0 || seen >= zecCom.ToInteger(resp.Response.TotalCount) {
			break
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", failVerify(v1alpha1.ReasonInstanceNotFound, true, "no instance in vpc %s has primary address %s", vpcID, ip)
	default:
		return "", failVerify(v1alpha1.ReasonInstanceAmbiguous, true,
			"instances %s in vpc %s all report primary address %s", strings.Join(hits, ","), vpcID, ip)
	}
}

func primaryNIC(inst *zecApi.InstanceInfo) *zecApi.NicInfo {
	if inst == nil {
		return nil
	}
	for _, n := range inst.Nics {
		if strings.EqualFold(zecCom.ToString(n.NicType), nicTypePrimary) {
			return n
		}
	}
	return nil
}

// platformFailure reports a failed platform call during verification under the
// reason the error itself names (credential, flow limit, unreachable), so the
// node is not left looking like it has no instance.
func platformFailure(what string, err error) *verifyError {
	reason := apiErr.OpenAPIErrEventReason(err)
	if reason == "" {
		reason = reasonAllocError
	}
	e := failVerify(reason, true, "%s: %v", what, err)
	e.inconclusive = true
	return e
}
