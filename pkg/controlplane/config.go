// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package controlplane is the credential-holding half of ipamType crd. It
// keeps each node's interfaces and addresses at what the node's ZecNode asks
// for, and is the only component that calls the Zenlayer OpenAPI.
package controlplane

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	"k8s.io/apimachinery/pkg/labels"

	dconfig "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/config"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/ec2"
)

// Identity modes.
const (
	IdentityAuto   = "auto"
	IdentityManual = "manual"
)

// Config is the controlplane's own configuration file.
//
// Every platform-facing value -- the OpenAPI endpoint, the VPC, the subnets,
// the security group -- comes from here and never from an object in the
// cluster being served. On the platform the cluster belongs to a tenant, and a
// tenant who could edit the endpoint would decide where the platform's
// credentials are sent, and one who could edit the subnets would steer this
// component at resources outside the cluster.
type Config struct {
	OpenAPIDomain string `json:"openAPIDomain,omitempty"`
	// RegionID is the region the cluster's VPC is in; interfaces and subnets
	// are looked up there.
	RegionID      string   `json:"regionID"`
	VpcID         string   `json:"vpcID"`
	Subnets       []string `json:"subnets"`
	SecurityGroup string   `json:"securityGroup"`
	IPStack       string   `json:"ipStack"`

	IPv6InternetChargeType string  `json:"ipv6InternetChargeType,omitempty"`
	IPv6Bandwidth          int     `json:"ipv6Bandwidth,omitempty"`
	IPv6PackageSize        float64 `json:"ipv6PackageSize,omitempty"`
	IPv6ClusterId          string  `json:"ipv6ClusterId,omitempty"`

	// ManageUntaggedENI has no default, as in the daemon: see config.Config.
	ManageUntaggedENI *bool `json:"manageUntaggedENI"`

	// CredentialDir holds either a long-term access key (AccessKeyID and
	// AccessKeyPassword files) or a temporary credential issued by AssumeRole
	// (AccessKeyID, AccessKeySecret, SecurityToken and Expiration). The files
	// are re-read periodically, so a rotated Secret or a renewed temporary
	// credential takes effect without a restart.
	CredentialDir string `json:"credentialDir,omitempty"`

	Identity IdentityConfig `json:"identity,omitempty"`

	// NodeSelector picks the Nodes this controlplane serves; empty serves all.
	// Virtual-kubelet nodes are never served.
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	Scaler     ScalerConfig      `json:"scaler"`
	NodeGroups []NodeGroupConfig `json:"nodeGroups,omitempty"`

	Workers   int             `json:"workers,omitempty"`
	RateLimit RateLimitConfig `json:"rateLimit,omitempty"`
}

// IdentityConfig picks how Node.spec.providerID is obtained.
type IdentityConfig struct {
	// ProviderID is auto (default): fill an empty providerID after checking the
	// node's identity, or manual: never write it, and serve only nodes whose
	// providerID is already set.
	ProviderID string `json:"providerID,omitempty"`
}

// ScalerConfig is the warm-pool sizing handed to nodes through status.scaler.
// There is no upper bound here: the pool's cap is the node's capacity.
type ScalerConfig struct {
	TargetMin int `json:"targetMin"`
	Target    int `json:"target"`
}

// NodeGroupConfig overrides the scaler for the Nodes its selector matches.
// The first matching group wins.
type NodeGroupConfig struct {
	Selector map[string]string `json:"selector"`
	Scaler   ScalerConfig      `json:"scaler"`
}

// RateLimitConfig bounds the controlplane's OpenAPI calls, for the whole
// cluster: reads and writes separately.
type RateLimitConfig struct {
	ReadQPS    float64 `json:"readQPS,omitempty"`
	ReadBurst  int     `json:"readBurst,omitempty"`
	WriteQPS   float64 `json:"writeQPS,omitempty"`
	WriteBurst int     `json:"writeBurst,omitempty"`
}

// Defaults for the values that are not behavior switches.
const (
	defaultCredentialDir = "/etc/zec_secret"
	defaultWorkers       = 4
	defaultReadQPS       = 10
	defaultReadBurst     = 20
	defaultWriteQPS      = 5
	defaultWriteBurst    = 10
	// maxSubnets matches the daemon's limit (config.MaxSubnetNum) and the
	// chart schema, so a configuration valid for one is valid for the other.
	maxSubnets = dconfig.MaxSubnetNum
)

// LoadConfig reads and validates the file at path.
func LoadConfig(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read controlplane config %s: %w", path, err)
	}
	cfg := &Config{}
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("parse controlplane config %s: %w", path, err)
	}
	if err := cfg.Validate(); err != nil {
		return nil, fmt.Errorf("controlplane config %s: %w", path, err)
	}
	return cfg, nil
}

// Validate checks the configuration and fills the defaults of values that
// are not behavior switches.
func (c *Config) Validate() error {
	if c.RegionID == "" {
		return fmt.Errorf("regionID is empty: interfaces and subnets are looked up by region")
	}
	if c.VpcID == "" {
		return fmt.Errorf("vpcID is empty: every subnet is checked against it, " +
			"and an empty one would let any be used")
	}
	if len(c.Subnets) == 0 {
		return fmt.Errorf("subnets is empty: list the pod subnets nodes may take addresses from")
	}
	if len(c.Subnets) > maxSubnets {
		return fmt.Errorf("subnets lists %d entries but at most %d are supported", len(c.Subnets), maxSubnets)
	}
	// Only present, not checked against the VPC: security groups are global
	// on this platform. A wrong one shows when the first interface is created.
	if c.SecurityGroup == "" {
		return fmt.Errorf("securityGroup is empty: every interface created needs one")
	}
	switch types.IPStack(c.IPStack) {
	case types.IPStackIPv4, types.IPStackDual:
	case "":
		return fmt.Errorf("ipStack is not set; it has no default, set ipv4 or dual")
	default:
		return fmt.Errorf("ipStack %q is not supported; set ipv4 or dual", c.IPStack)
	}
	if types.IPStack(c.IPStack).EnableIPv6() {
		if err := c.validateIPv6Billing(); err != nil {
			return err
		}
	}
	if c.ManageUntaggedENI == nil {
		return fmt.Errorf("manageUntaggedENI is not set; it has no default, set true or false")
	}
	switch c.Identity.ProviderID {
	case "":
		c.Identity.ProviderID = IdentityAuto
	case IdentityAuto, IdentityManual:
	default:
		return fmt.Errorf("identity.providerID %q is neither %s nor %s", c.Identity.ProviderID, IdentityAuto, IdentityManual)
	}
	if err := c.Scaler.validate("scaler"); err != nil {
		return err
	}
	for i, g := range c.NodeGroups {
		if len(g.Selector) == 0 {
			return fmt.Errorf("nodeGroups[%d] has an empty selector, which would match every node", i)
		}
		if _, err := labels.ValidatedSelectorFromSet(g.Selector); err != nil {
			return fmt.Errorf("nodeGroups[%d].selector: %w", i, err)
		}
		if err := g.Scaler.validate(fmt.Sprintf("nodeGroups[%d].scaler", i)); err != nil {
			return err
		}
	}
	if _, err := labels.ValidatedSelectorFromSet(c.NodeSelector); err != nil {
		return fmt.Errorf("nodeSelector: %w", err)
	}
	if c.CredentialDir == "" {
		c.CredentialDir = defaultCredentialDir
	}
	if c.Workers <= 0 {
		c.Workers = defaultWorkers
	}
	r := &c.RateLimit
	if r.ReadQPS <= 0 {
		r.ReadQPS = defaultReadQPS
	}
	if r.ReadBurst <= 0 {
		r.ReadBurst = defaultReadBurst
	}
	if r.WriteQPS <= 0 {
		r.WriteQPS = defaultWriteQPS
	}
	if r.WriteBurst <= 0 {
		r.WriteBurst = defaultWriteBurst
	}
	return nil
}

func (s ScalerConfig) validate(field string) error {
	if s.TargetMin < 0 || s.Target <= 0 || s.TargetMin > s.Target {
		return fmt.Errorf("%s needs 0 <= targetMin <= target and target > 0, got %+v", field, s)
	}
	return nil
}

// ScalerFor returns the scaler of the first node group matching nodeLabels,
// or the default one.
func (c *Config) ScalerFor(nodeLabels map[string]string) ScalerConfig {
	for _, g := range c.NodeGroups {
		if labels.SelectorFromSet(g.Selector).Matches(labels.Set(nodeLabels)) {
			return g.Scaler
		}
	}
	return c.Scaler
}

// virtualNodeLabel marks virtual-kubelet nodes, which have no instance.
const virtualNodeLabel = "type"

// Serves reports whether the controlplane manages a Node with these labels.
// Virtual nodes are never served.
func (c *Config) Serves(nodeLabels map[string]string) bool {
	if nodeLabels[virtualNodeLabel] == "virtual-kubelet" {
		return false
	}
	return labels.SelectorFromSet(c.NodeSelector).Matches(labels.Set(nodeLabels))
}

// IPv6Billing is what the platform charges for public IPv6 on an interface.
func (c *Config) IPv6Billing() ec2.IPv6Billing {
	return ec2.IPv6Billing{
		InternetChargeType: c.IPv6InternetChargeType,
		BandwidthMbps:      c.IPv6Bandwidth,
		PackageSizeTB:      c.IPv6PackageSize,
		ClusterId:          c.IPv6ClusterId,
	}
}

// validateIPv6Billing applies the daemon's rules and defaults for public IPv6
// billing: the platform refuses to enable IPv6 on an interface without them,
// and the refusal would otherwise surface only as interfaces that never get a
// prefix.
func (c *Config) validateIPv6Billing() error {
	if c.IPv6InternetChargeType == "" {
		c.IPv6InternetChargeType = dconfig.DefaultIPv6InternetChargeType
	}
	if c.IPv6Bandwidth == 0 {
		c.IPv6Bandwidth = dconfig.DefaultIPv6BandwidthMbps
	}
	switch c.IPv6InternetChargeType {
	case "ByBandwidth":
	case "ByTrafficPackage":
		if c.IPv6PackageSize <= 0 {
			return fmt.Errorf("ipv6InternetChargeType ByTrafficPackage requires a positive ipv6PackageSize (TB)")
		}
		if t := c.IPv6PackageSize * 10; t != math.Trunc(t) {
			return fmt.Errorf("ipv6PackageSize %v is not a multiple of 0.1, which the platform requires", c.IPv6PackageSize)
		}
	case "BandwidthCluster":
		if c.IPv6ClusterId == "" {
			return fmt.Errorf("ipv6InternetChargeType BandwidthCluster requires ipv6ClusterId")
		}
	case "":
		return fmt.Errorf("ipStack dual requires ipv6InternetChargeType (ByBandwidth, ByTrafficPackage or BandwidthCluster)")
	default:
		return fmt.Errorf("unsupported ipv6InternetChargeType %q", c.IPv6InternetChargeType)
	}
	if c.IPv6Bandwidth <= 0 {
		return fmt.Errorf("ipStack dual requires a positive ipv6Bandwidth (Mbps)")
	}
	return nil
}
