// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

//

package metadata

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/metrics"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/utils"
)

const (
	// Paths under metadataURL. Keep these in step with the endpoint reference:
	//   https://docs.console.zenlayer.com/welcome/elastic-compute/get-started/manage-instances/get-instance-metadata
	// A field that quietly changes shape upstream shows up here as a parse
	// error at pod setup, so check the doc first when that happens.
	enisPath              = "network/interfaces/macs/"
	eniIDPath             = "network/interfaces/macs/%s/network-interface-id"
	eniGatewayPath        = "network/interfaces/macs/%s/gateway"
	eniV6GatewayPath      = "network/interfaces/macs/%s/ipv6-gateway"
	eniAddrPath           = "network/interfaces/macs/%s/primary-ip-address"
	eniV6AddrPath         = "network/interfaces/macs/%s/primary-ipv6-address"
	eniPrivateIPs         = "network/interfaces/macs/%s/private-ipv4s"
	eniPrivateIPv6Prefix  = "network/interfaces/macs/%s/ipv6-prefix"
	eniSubnetIDPath       = "network/interfaces/macs/%s/subnet-id"
	eniSubnetCIDRPath     = "network/interfaces/macs/%s/subnet-cidr-block"
	eniSubnetIPv6CIDRPath = "network/interfaces/macs/%s/subnet-ipv6-cidr-block"
)

type EC2MetadataInterface interface {
	GetMetadata(ctx context.Context, path string) (string, error)
}

type EC2MetadataWrapper struct {
	EC2MetadataInterface
}

func NewEC2MetadataWrapper(m EC2MetadataInterface) EC2MetadataWrapper {
	return EC2MetadataWrapper{m}
}

func getArray(jsonStr string) ([]string, error) {
	ret := []string{}
	err := json.Unmarshal([]byte(strings.TrimSpace(jsonStr)), &ret)
	if err != nil {
		return nil, err
	}
	return ret, nil
}

// GetENIsMACs lists the MAC addresses of every interface attached to this
// instance. Every other call in this file is keyed on one of them. The
// service answers a directory listing, one "mac/" per line, not JSON.
func (meta EC2MetadataWrapper) GetENIsMACs(ctx context.Context) ([]string, error) {
	start := time.Now()
	macsStr, err := meta.GetMetadata(ctx, enisPath)
	duration := metrics.MsSince(start)
	metrics.MetadataLatency.WithLabelValues("GetENIsMACs", fmt.Sprint(err != nil), StatusLabel(err)).Observe(duration)
	if err != nil {
		metrics.MetadataErrInc("GetENIsMACs", err)
		return []string{}, err
	}
	return parseMACListing(macsStr), nil
}

// parseMACListing reads a directory listing of interface MACs.
func parseMACListing(listing string) []string {
	var macs []string
	for _, f := range strings.Fields(listing) {
		if mac := strings.TrimSuffix(f, "/"); mac != "" {
			macs = append(macs, mac)
		}
	}
	return macs
}

func (meta EC2MetadataWrapper) GetENIID(ctx context.Context, mac string) (string, error) {
	start := time.Now()
	eniID, err := meta.GetMetadata(ctx, fmt.Sprintf(eniIDPath, mac))
	duration := metrics.MsSince(start)
	metrics.MetadataLatency.WithLabelValues("GetENIID", fmt.Sprint(err != nil), StatusLabel(err)).Observe(duration)
	if err != nil {
		metrics.MetadataErrInc("GetENIID", err)
		return "", err
	}
	return eniID, err
}

func (meta EC2MetadataWrapper) GetENIIPv4Gateway(ctx context.Context, mac string) (net.IP, error) {
	start := time.Now()
	gw, err := meta.GetMetadata(ctx, fmt.Sprintf(eniGatewayPath, mac))
	duration := metrics.MsSince(start)
	metrics.MetadataLatency.WithLabelValues("GetENIIPv4Gateway", fmt.Sprint(err != nil), StatusLabel(err)).Observe(duration)
	if err != nil {
		metrics.MetadataErrInc("GetENIIPv4Gateway", err)
		return nil, err
	}
	return utils.ParseIP(gw)
}

func (meta EC2MetadataWrapper) GetENIIPv6Gateway(ctx context.Context, mac string) (net.IP, error) {
	start := time.Now()
	gw, err := meta.GetMetadata(ctx, fmt.Sprintf(eniV6GatewayPath, mac))
	duration := metrics.MsSince(start)
	metrics.MetadataLatency.WithLabelValues("GetENIIPv6Gateway", fmt.Sprint(err != nil), StatusLabel(err)).Observe(duration)
	if err != nil {
		metrics.MetadataErrInc("GetENIIPv6Gateway", err)
		return nil, err
	}
	return utils.ParseIP(gw)
}

func (meta EC2MetadataWrapper) GetENIPrimaryIP(ctx context.Context, mac string) (net.IP, error) {
	start := time.Now()
	addr, err := meta.GetMetadata(ctx, fmt.Sprintf(eniAddrPath, mac))
	duration := metrics.MsSince(start)
	metrics.MetadataLatency.WithLabelValues("GetENIPrimaryIP", fmt.Sprint(err != nil), StatusLabel(err)).Observe(duration)
	if err != nil {
		metrics.MetadataErrInc("GetENIPrimaryIP", err)
		return nil, err
	}
	return utils.ParseIP(addr)
}

// GetENIPrimaryIPv6 gets an interface's primary IPv6 from metadata. It is the
// second source for that address; the first is the describe reply the daemon
// normally reads (NicInfo.PrimaryIpv6). GetENIAllocatableIPs compares the two
// and records a disagreement as ENI.PrimaryIPv6Disputed, which keeps both
// candidates out of the pool.
//
// The comparison earns its cost on the instance's own interface, where nothing
// in this daemon writes the address: the guest gets it by DHCPv6 from the
// platform's record. If describe disagreed there, the carve would skip the
// wrong address and hand out the real one, which is already configured on the
// host. The host's local table then matches it ahead of the priority-512
// to-pod rule and the pod is black-holed, with nothing to detect it afterwards
// because the platform publishes no per-address IPv6 view.
//
// Callers should tolerate a 404: interfaces that have never had IPv6 enabled
// answer 404 here, as they do for ipv6-prefix.
func (meta EC2MetadataWrapper) GetENIPrimaryIPv6(ctx context.Context, mac string) (net.IP, error) {
	start := time.Now()
	addr, err := meta.GetMetadata(ctx, fmt.Sprintf(eniV6AddrPath, mac))
	duration := metrics.MsSince(start)
	metrics.MetadataLatency.WithLabelValues("GetENIPrimaryIPv6", fmt.Sprint(err != nil), StatusLabel(err)).Observe(duration)
	if err != nil {
		metrics.MetadataErrInc("GetENIPrimaryIPv6", err)
		return nil, err
	}
	return utils.ParseIP(addr)
}

func (meta EC2MetadataWrapper) GetENIPrivateIPv4s(ctx context.Context, mac string) ([]net.IP, error) {
	start := time.Now()
	ipsStr, err := meta.GetMetadata(ctx, fmt.Sprintf(eniPrivateIPs, mac))
	duration := metrics.MsSince(start)
	metrics.MetadataLatency.WithLabelValues("GetENIPrivateIPv4s", fmt.Sprint(err != nil), StatusLabel(err)).Observe(duration)
	if err != nil {
		metrics.MetadataErrInc("GetENIPrivateIPv4s", err)
		return nil, err
	}
	if len(ipsStr) == 0 {
		return nil, nil
	}
	// The metadata service returns this as a JSON array (["10.0.0.16"]),
	// unlike the MAC listing, which is one entry per line. Splitting on
	// newlines instead yields the whole literal as one element that fails to
	// parse as an address. That failure only shows up once an interface has a
	// secondary IP: an empty "[]" splits into something harmless.
	addressStrList, err := getArray(ipsStr)
	if err != nil {
		return nil, fmt.Errorf("parse private ipv4s %q of eni %s: %w", ipsStr, mac, err)
	}
	ips, err := utils.ParseIPs(addressStrList)
	if err != nil {
		return nil, err
	}
	return ips, nil
}

// GetENIIPv6Prefix returns the IPv6 prefix assigned to an interface, or nil
// when metadata answers with an empty body. Callers should tolerate a 404: an
// interface in an IPv4-only subnet, and one that has not been assigned a prefix
// yet, both answer 404 here.
func (meta EC2MetadataWrapper) GetENIIPv6Prefix(ctx context.Context, mac string) (*net.IPNet, error) {
	start := time.Now()
	ip6CidrStr, err := meta.GetMetadata(ctx, fmt.Sprintf(eniPrivateIPv6Prefix, mac))
	duration := metrics.MsSince(start)
	metrics.MetadataLatency.WithLabelValues("GetENIIPv6Prefix", fmt.Sprint(err != nil), StatusLabel(err)).Observe(duration)
	if err != nil {
		metrics.MetadataErrInc("GetENIIPv6Prefix", err)
		return nil, err
	}
	if len(ip6CidrStr) == 0 {
		return nil, nil
	}
	ip6Cidr, err := utils.ParseCidr(ip6CidrStr)
	if err != nil {
		return nil, err
	}

	return ip6Cidr, nil
}

func (meta EC2MetadataWrapper) GetENISubnetID(ctx context.Context, mac string) (string, error) {
	start := time.Now()
	subnetID, err := meta.GetMetadata(ctx, fmt.Sprintf(eniSubnetIDPath, mac))
	duration := metrics.MsSince(start)
	metrics.MetadataLatency.WithLabelValues("GetENISubnetID", fmt.Sprint(err != nil), StatusLabel(err)).Observe(duration)
	if err != nil {
		metrics.MetadataErrInc("GetENISubnetID", err)
		return "", err
	}
	return subnetID, err
}

func (meta EC2MetadataWrapper) GetENISubnetCIDR(ctx context.Context, mac string) (*net.IPNet, error) {
	start := time.Now()
	cidr, err := meta.GetMetadata(ctx, fmt.Sprintf(eniSubnetCIDRPath, mac))
	duration := metrics.MsSince(start)
	metrics.MetadataLatency.WithLabelValues("GetENISubnetCIDR", fmt.Sprint(err != nil), StatusLabel(err)).Observe(duration)
	if err != nil {
		metrics.MetadataErrInc("GetENISubnetCIDR", err)
		return nil, err
	}
	_, subnetCIDR, err := net.ParseCIDR(cidr)
	return subnetCIDR, err
}

func (meta EC2MetadataWrapper) GetENISubnetIPv6CIDR(ctx context.Context, mac string) (*net.IPNet, error) {
	start := time.Now()
	cidr, err := meta.GetMetadata(ctx, fmt.Sprintf(eniSubnetIPv6CIDRPath, mac))
	duration := metrics.MsSince(start)
	metrics.MetadataLatency.WithLabelValues("GetENISubnetIPv6CIDR", fmt.Sprint(err != nil), StatusLabel(err)).Observe(duration)
	if err != nil {
		metrics.MetadataErrInc("GetENISubnetIPv6CIDR", err)
		return nil, err
	}
	_, subnetCIDR, err := net.ParseCIDR(cidr)
	return subnetCIDR, err
}
