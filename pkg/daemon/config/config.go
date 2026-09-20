// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package config loads and validates the daemon's configuration.
//
// Validation is deliberately eager and fatal: a node running with a
// configuration the operator did not intend is harder to diagnose than a node
// that refuses to start and says why. Note that the struct tags on Config are
// inert -- no validation or defaulting library consumes them -- and
// PopulateAndValidate is what actually enforces the rules.
package config

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"

	jsonpatch "github.com/evanphx/json-patch"
	"github.com/gdexlab/go-render/render"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/k8s"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

const (
	DefaultDebugPort                   = 11414
	MaxSubnetNum                       = 5
	DefaultPoolCapRatio                = 1
	DefaultPoolMonitorIntervalSec      = 120
	DefaultReconcileIntervalSec        = 1200
	DefaultGcProtectPeriodSec          = 120
	DefaultSubnetStatAgingSec          = 40
	DefaultSubnetStatUpdateIntervalSec = 60
	// Public IPv6 charge types the platform accepts.
	IPv6ChargeByTrafficPackage = "ByTrafficPackage"
	IPv6ChargeByBandwidth      = "ByBandwidth"
	IPv6ChargeBandwidthCluster = "BandwidthCluster"

	// DefaultIPv6InternetChargeType and DefaultIPv6BandwidthMbps are the
	// defaults for public IPv6 on an interface: bill by traffic, cap at 10Gbps.
	// Both are sent per interface (see buildAssignIPv6Request), so the cap
	// bounds one interface's public IPv6 while the bill follows actual use,
	// which suits addresses that come and go with pods.
	//
	// ByTrafficPackage needs a committed package size, which has no safe default
	// -- it is a billing commitment, so the operator states it.
	//
	// Whether an account may use this charge type at all is an account property,
	// not a platform one: an account on an hourly billing period is refused with
	//
	//	OPERATION_DENIED_FLOW_PACKAGE_NOT_SUPPORTED_HOUR_PERIOD
	//	The operation is denied as setting a committed traffic package size is
	//	not supported under hourly billing period
	//
	// Such an account picks ByBandwidth instead; that is a deployment choice, not
	// a reason to change what the chart defaults to.
	DefaultIPv6InternetChargeType = IPv6ChargeByTrafficPackage
	DefaultIPv6BandwidthMbps      = 10000

	// Static credentials are read from files mounted from a Secret rather than
	// carried in the ConfigMap. The env vars below override these paths; see
	// loadCredentialFromSecret.
	defaultSecretPathAK = "/etc/zec_secret/AccessKeyID"
	defaultSecretPathSK = "/etc/zec_secret/AccessKeyPassword"

	envSecretPathAK = "SECRET_PATH_AK"
	envSecretPathSK = "SECRET_PATH_SK"
)

var (
	log = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "ipam"})
)

// Config is the ipam daemon's configuration.
type Config struct {
	Version     string `yaml:"version" json:"version"`
	NetworkMode string `yaml:"networkMode" json:"networkMode"`
	// CredentialAccessKeyId used in static authentication
	CredentialAccessKeyId string `yaml:"credentialAccessKeyId" json:"credentialAccessKeyId,omitempty"`
	// CredentialAccessKeySecret used in static authentication
	CredentialAccessKeySecret string `yaml:"credentialAccessKeySecret" json:"credentialAccessKeySecret,omitempty"`
	// OpenAPIDomain overrides the host the Zenlayer OpenAPI is called on.
	// Empty means the SDK default (the public production endpoint); set it to
	// reach an isolated environment, where production credentials are not
	// valid and the signature is rejected.
	OpenAPIDomain string `yaml:"openAPIDomain" json:"openAPIDomain,omitempty"`
	ServiceCIDR   string `yaml:"serviceCidr" json:"serviceCidr"`
	// PoolTargetMax is the pool's max cap: the most resources it will hold and
	// hand out. Ignored when PoolTargetCapProbe is set, which caps the pool at
	// the instance's own capacity instead.
	PoolTargetMax uint32 `yaml:"poolTargetMax" json:"poolTargetMax"`
	// PoolTarget the target number of cached resources
	PoolTarget uint32 `yaml:"poolTarget" json:"poolTarget,omitempty"`
	// PoolTargetMin the min target number of cached resources
	PoolTargetMin      uint32   `yaml:"poolTargetMin" json:"poolTargetMin"`
	PoolTargetCapProbe bool     `yaml:"poolTargetCapProbe" json:"poolTargetCapProbe,omitempty"`
	PoolCapRatio       float64  `yaml:"poolCapRatio" json:"poolCapRatio" mod:"default=1"`
	Prefix             string   `yaml:"prefix" json:"prefix"`
	VpcID              string   `yaml:"vpcID" json:"vpcID"`
	Subnets            []string `yaml:"subnets" json:"subnets"` // Subnets used by pods (actually used by ENI), All subnets must belong to the same vpcID
	SecurityGroup      string   `yaml:"securityGroup" json:"securityGroup"`
	// IPStack selects the address families served. Defaults to ipv4; dual is the
	// other mode that runs. The validate and mod tags are inert -- no validator
	// or defaulting library is wired up -- so the switch in PopulateAndValidate
	// is the only gate, and the default is applied there by hand.
	IPStack string `yaml:"ipStack" json:"ipStack" validate:"oneof=ipv4 ipv6 dual" mod:"default=ipv4"`
	// IPv6InternetChargeType and IPv6Bandwidth are what the platform charges for
	// public IPv6 on an interface. They are required, not optional: enabling
	// IPv6 on a NIC in a subnet whose ipv6Type is Public is refused outright
	// without them (MISSING_PARAMETER: internetChargeType), and every dual-stack
	// subnet observed so far has been Public.
	//
	// The CNI has to supply them because it is the CNI that enables IPv6 on each
	// interface it creates. They are configuration rather than a constant so the
	// billing decision stays the operator's.
	IPv6InternetChargeType string `yaml:"ipv6InternetChargeType" json:"ipv6InternetChargeType,omitempty"`
	// IPv6Bandwidth is the cap in Mbps.
	IPv6Bandwidth int `yaml:"ipv6Bandwidth" json:"ipv6Bandwidth,omitempty"`
	// IPv6PackageSize is the traffic package size in TB, required by the
	// platform when the charge type is ByTrafficPackage. PopulateAndValidate
	// then demands a positive multiple of 0.1; it is ignored under the other
	// charge types.
	IPv6PackageSize float64 `yaml:"ipv6PackageSize" json:"ipv6PackageSize,omitempty"`
	// IPv6ClusterId is the shared bandwidth package to join, required by the
	// platform when the charge type is BandwidthCluster.
	IPv6ClusterId string `yaml:"ipv6ClusterId" json:"ipv6ClusterId,omitempty"`
	// ManageUntaggedENI decides what the daemon does with a secondary network
	// interface that is attached to this instance but was not created by it and
	// is not exempted by the zenlayer:cni:no-manage tag.
	//
	// true (the default) adopts it if it sits in a configured pod subnet: its
	// secondary addresses join the pool and it may be detached when the pool
	// shrinks. false leaves it untouched, in which case it still consumes the
	// instance's interface quota and so reduces the number of addresses this
	// node can serve. See isManagedENI in pkg/daemon/factory.
	//
	// A pointer so an unset field is distinguishable from an explicit false.
	ManageUntaggedENI           *bool            `yaml:"manageUntaggedENI" json:"manageUntaggedENI,omitempty"`
	IPAMType                    types.IPAMType   `yaml:"ipamType" json:"ipamType"` // accepted but never read; no values are defined for it
	ExtraRoutes                 []zecTypes.Route `yaml:"extraRoutes" json:"extraRoutes,omitempty"`
	DisableDevicePlugin         bool             `yaml:"disableDevicePlugin" json:"disableDevicePlugin"`
	RateLimit                   map[string]int   `yaml:"rateLimit" json:"rateLimit"`
	ReconcileIntervalSec        uint32           `yaml:"reconcileIntervalSec" json:"reconcileIntervalSec"`
	PoolMonitorIntervalSec      uint32           `yaml:"poolMonitorIntervalSec" json:"poolMonitorIntervalSec,omitempty"`
	PoolGCPeriodSec             uint32           `yaml:"poolGCPeriodSec" json:"poolGCPeriodSec"`
	SubnetStatAgingSec          uint32           `yaml:"subnetStatAgingSec" json:"subnetStatAgingSec,omitempty"`
	SubnetStatUpdateIntervalSec uint32           `yaml:"subnetStatUpdateIntervalSec" json:"subnetStatUpdateIntervalSec,omitempty"`
	// HealthAndDebugPort port for health check and debug
	HealthAndDebugPort uint32 `yaml:"healthAndDebugPort" json:"healthAndDebugPort,omitempty"`
	// ENITags                     map[string]string   `yaml:"eniTags" json:"eniTags"`
	// ENITagFilter                map[string]string   `yaml:"eniTagFilter" json:"eniTagFilter"` // if set , only enis match filter, will be managed
}

// getConfigFromFileWithMerge parses Config from cfg, optionally merged on top
// of a base config read from filePath (RFC7396 merge patch).
//
// filePath is intentionally kept even though LoadConfig, the only non-test
// caller, passes "" today: it is the hook for a future on-disk base config (a
// file baked into the image or mounted from the chart) that the ConfigMap would
// then override. Do not drop the parameter as dead code.
func getConfigFromFileWithMerge(filePath string, cfg []byte) (*Config, error) {
	// An empty filePath means there is no base config; skip the read rather than
	// failing on os.ReadFile("").
	var data []byte
	if filePath != "" {
		var err error
		data, err = os.ReadFile(filePath)
		if err != nil {
			return nil, err
		}
	}

	config, err := MergeConfigAndUnmarshal(cfg, data)
	if err != nil {
		return nil, err
	}

	return config, nil
}

// MergeConfigAndUnmarshal parses topCfg merged onto baseCfg. Either side may be
// empty, in which case the other is parsed alone; both empty is an error, which
// is what rejects an eni_conf key that exists but is blank.
func MergeConfigAndUnmarshal(topCfg, baseCfg []byte) (*Config, error) {
	if len(topCfg) == 0 && len(baseCfg) == 0 {
		return nil, fmt.Errorf("no config to parse: both the configmap and the config file are empty")
	}

	if len(topCfg) == 0 {
		config := &Config{}
		err := json.Unmarshal(baseCfg, config)
		return config, err
	}

	if len(baseCfg) == 0 {
		config := &Config{}
		err := json.Unmarshal(topCfg, config)
		return config, err
	}

	// MergePatch in RFC7396
	jsonBytes, err := jsonpatch.MergePatch(baseCfg, topCfg)
	if err != nil {
		return nil, err
	}

	config := &Config{}
	err = json.Unmarshal(jsonBytes, config)

	return config, err
}

// secretPaths returns the two files the credentials are read from, which the
// environment can move. The error raised when neither yields a credential names
// them, and naming the defaults instead would point at files never opened.
func secretPaths() (akPath, skPath string) {
	akPath, skPath = defaultSecretPathAK, defaultSecretPathSK
	if p := os.Getenv(envSecretPathAK); p != "" {
		akPath = p
	}
	if p := os.Getenv(envSecretPathSK); p != "" {
		skPath = p
	}
	return akPath, skPath
}

// loadCredentialFromSecret reads the static credentials from the mounted
// Secret. What it finds there wins over the ConfigMap, so the credentials
// never have to be written into the ConfigMap at all. A missing mount is not
// an error: the ConfigMap values (if any) are kept, and PopulateAndValidate
// rejects the config if both sources come up empty.
func (c *Config) loadCredentialFromSecret() error {
	akPath, skPath := secretPaths()

	for _, item := range []struct {
		path  string
		field *string
	}{
		{akPath, &c.CredentialAccessKeyId},
		{skPath, &c.CredentialAccessKeySecret},
	} {
		data, err := os.ReadFile(item.path)
		if os.IsNotExist(err) {
			log.Warnf("Credential file %s not mounted, falling back to configmap", item.path)
			continue
		}
		if err != nil {
			return fmt.Errorf("read credential from %s failed: %w", item.path, err)
		}
		// Trim the trailing newline a --from-file secret carries.
		if v := strings.TrimSpace(string(data)); v != "" {
			*item.field = v
		}
	}
	return nil
}

// ManageUntagged reports the effective value of ManageUntaggedENI. Unset means
// true: an untagged interface is adopted unless the operator says otherwise.
func (c *Config) ManageUntagged() bool {
	if c.ManageUntaggedENI == nil {
		return true
	}
	return *c.ManageUntaggedENI
}

// PopulateAndValidate fills in the defaults and refuses a config the daemon
// cannot run with. It is the whole of the validation: the struct tags above are
// inert, so a field this function does not check is not checked at all.
func (c *Config) PopulateAndValidate() error {
	if c.CredentialAccessKeyId == "" || c.CredentialAccessKeySecret == "" {
		akPath, skPath := secretPaths()
		return fmt.Errorf("no credentials for the zenlayer api: neither %s and %s were readable, "+
			"nor were credentialAccessKeyId and credentialAccessKeySecret set in the cni configmap. "+
			"Create the secret the daemonset mounts (chart value zec.secretName) with the keys "+
			"AccessKeyID and AccessKeyPassword", akPath, skPath)
	}

	if c.SecurityGroup == "" {
		return fmt.Errorf("securityGroup is not set in the cni configmap (chart value " +
			"zec.securityGroupID); every network interface this node creates needs one")
	}
	log.WithField("SecurityGroup", c.SecurityGroup).Info("config loaded")

	if len(c.Subnets) == 0 {
		// vpcID is not validated anywhere, so it may still be empty here; the
		// constraint is named rather than interpolated.
		return fmt.Errorf("subnets is empty in the cni configmap (chart value zec.subnetIDs): " +
			"list the pod subnets this node may take addresses from, all of which must be in " +
			"the vpc named by vpcID")
	}
	if len(c.Subnets) > MaxSubnetNum {
		return fmt.Errorf("subnets lists %d entries but at most %d are supported "+
			"(chart value zec.subnetIDs)", len(c.Subnets), MaxSubnetNum)
	}
	log.WithField("Subnets", c.Subnets).Info("config loaded")

	if c.ReconcileIntervalSec == 0 {
		c.ReconcileIntervalSec = DefaultReconcileIntervalSec
	}
	log.WithField("ReconcileIntervalSec", c.ReconcileIntervalSec).Info("config loaded")

	if l := c.PoolCapRatio; l <= 0 || l > 1 {
		c.PoolCapRatio = DefaultPoolCapRatio
	}
	log.WithField("PoolCapRatio", c.PoolCapRatio).Info("config loaded")

	// The keys are spelled as they appear in the cni configmap and as chart
	// values, not as the Go fields they land in: whoever reads this message is
	// about to grep for the name in values.yaml or in the configmap, and the
	// Go spelling occurs in neither.
	if c.PoolTargetMin > c.PoolTarget {
		return fmt.Errorf("poolTargetMin (%d) must be less than or equal to poolTarget (%d) "+
			"in the cni configmap (chart values zec.poolTargetMin, zec.poolTarget)", c.PoolTargetMin, c.PoolTarget)
	}
	if c.PoolTarget > c.PoolTargetMax {
		return fmt.Errorf("poolTarget (%d) must be less than or equal to poolTargetMax (%d) "+
			"in the cni configmap (chart values zec.poolTarget, zec.poolTargetMax)", c.PoolTarget, c.PoolTargetMax)
	}
	if c.PoolTargetMax == 0 || c.PoolTarget == 0 {
		if !c.PoolTargetCapProbe {
			return fmt.Errorf("poolTarget (%d) and poolTargetMax (%d) must both be > 0 unless "+
				"poolTargetCapProbe is true, in which case the node's own capacity supplies the cap: "+
				"set them in the cni configmap (chart values zec.poolTarget, zec.poolTargetMax, "+
				"zec.poolTargetCapProbe)", c.PoolTarget, c.PoolTargetMax)
		}
	}
	log.WithField("PoolTarget", c.PoolTarget).Info("config loaded")
	log.WithField("PoolTargetMin", c.PoolTargetMin).Info("config loaded")
	log.WithField("PoolTargetMax", c.PoolTargetMax).Info("config loaded")

	if c.PoolMonitorIntervalSec == 0 {
		c.PoolMonitorIntervalSec = DefaultPoolMonitorIntervalSec
	}
	log.WithField("PoolMonitorIntervalSec", c.PoolMonitorIntervalSec).Info("config loaded")

	if c.SubnetStatAgingSec == 0 {
		c.SubnetStatAgingSec = DefaultSubnetStatAgingSec
	}
	log.WithField("SubnetStatAgingSec", c.SubnetStatAgingSec).Info("config loaded")

	if c.SubnetStatUpdateIntervalSec == 0 {
		c.SubnetStatUpdateIntervalSec = DefaultSubnetStatUpdateIntervalSec
	}
	log.WithField("SubnetStatUpdateIntervalSec", c.SubnetStatUpdateIntervalSec).Info("config loaded")

	if c.PoolGCPeriodSec == 0 {
		c.PoolGCPeriodSec = DefaultGcProtectPeriodSec
	}
	log.WithField("PoolGCPeriodSec", c.PoolGCPeriodSec).Info("config loaded")

	if c.NetworkMode == "" {
		c.NetworkMode = types.ModeENIMultiIP
	}
	log.WithField("NetworkMode", c.NetworkMode).Info("config loaded")

	if c.IPStack == "" {
		c.IPStack = string(types.IPStackIPv4)
	}
	log.WithField("IPStack", c.IPStack).Info("config loaded")

	if c.HealthAndDebugPort == 0 {
		c.HealthAndDebugPort = DefaultDebugPort
	}
	log.WithField("HealthAndDebugPort", c.HealthAndDebugPort).Info("config loaded")

	switch c.IPStack {
	case "", string(types.IPStackIPv4), string(types.IPStackDual):
	case string(types.IPStackIPv6):
		// Refused as unimplemented, not as a typo -- the message has to say
		// which it is. Three things are missing and each fails silently:
		// capacity has no number to report (the platform hands out an IPv6
		// prefix per interface, not countable addresses, so nothing bounds the
		// pool once IPv4 is gone), the service CIDR lookup still insists on an
		// IPv4 answer, and a new ENI is not stripped of the IPv4 address it
		// comes with.
		return fmt.Errorf("ipStack %q (IPv6-only) is not implemented yet: use %q for dual-stack, or %q",
			c.IPStack, types.IPStackDual, types.IPStackIPv4)
	default:
		return fmt.Errorf("unsupported ipStack %q in configMap: use %q or %q",
			c.IPStack, types.IPStackIPv4, types.IPStackDual)
	}

	// After the ipStack switch, so an unimplemented or misspelled stack mode is
	// reported as such rather than as a missing billing setting.
	if types.IPStack(c.IPStack).EnableIPv6() {
		if c.IPv6InternetChargeType == "" {
			c.IPv6InternetChargeType = DefaultIPv6InternetChargeType
		}
		if c.IPv6Bandwidth == 0 {
			c.IPv6Bandwidth = DefaultIPv6BandwidthMbps
		}
		// Caught here rather than at the first ENI creation. The platform's own
		// complaint is a MISSING_PARAMETER on an API call buried inside pool
		// warm-up, which surfaces as a node that cannot create interfaces for
		// no stated reason; these say which setting is missing.
		switch c.IPv6InternetChargeType {
		case IPv6ChargeByTrafficPackage:
			// Computed on the value times ten, not with math.Mod: 0.1 has no
			// exact float64 representation, so math.Mod(0.3, 0.1) is 0.0999...
			// and every legal value but a few would be rejected.
			if tenths := c.IPv6PackageSize * 10; tenths != math.Trunc(tenths) {
				return fmt.Errorf("ipv6PackageSize %v is not a multiple of 0.1, which the platform requires", c.IPv6PackageSize)
			}
			if c.IPv6PackageSize <= 0 {
				return fmt.Errorf("ipv6InternetChargeType %q requires ipv6PackageSize (TB, 0 or a multiple of 0.1) -- "+
					"a traffic commitment has no safe default, so set one; if this account is on an hourly billing "+
					"period the platform refuses committed package sizes outright and %q applies instead",
					c.IPv6InternetChargeType, IPv6ChargeByBandwidth)
			}
		case IPv6ChargeBandwidthCluster:
			if c.IPv6ClusterId == "" {
				return fmt.Errorf("ipv6InternetChargeType %q requires ipv6ClusterId", c.IPv6InternetChargeType)
			}
		case IPv6ChargeByBandwidth:
		default:
			return fmt.Errorf("unsupported ipv6InternetChargeType %q: use %q, %q or %q",
				c.IPv6InternetChargeType, IPv6ChargeByBandwidth, IPv6ChargeByTrafficPackage, IPv6ChargeBandwidthCluster)
		}
		if c.IPv6Bandwidth <= 0 {
			return fmt.Errorf("ipStack %q needs a positive ipv6Bandwidth (Mbps): the platform requires a cap for public IPv6", c.IPStack)
		}
		log.WithFields(map[string]interface{}{
			"internetChargeType": c.IPv6InternetChargeType,
			"bandwidthMbps":      c.IPv6Bandwidth,
			"packageSizeTB":      c.IPv6PackageSize,
			"clusterId":          c.IPv6ClusterId,
		}).Info("ipv6 public billing config loaded")
	}

	switch c.NetworkMode {
	case types.ModeENIMultiIP:
	default:
		return fmt.Errorf("unsupported daemon mode %q: the only mode this daemon implements is %q",
			c.NetworkMode, types.ModeENIMultiIP)
	}

	return nil
}

func (c *Config) String() string {
	if c == nil {
		return ""
	}
	// Redact static credentials: String() is written to the log on every start.
	redacted := *c
	if redacted.CredentialAccessKeyId != "" {
		redacted.CredentialAccessKeyId = "<redacted>"
	}
	if redacted.CredentialAccessKeySecret != "" {
		redacted.CredentialAccessKeySecret = "<redacted>"
	}
	return render.AsCode(&redacted)
}

// LoadConfig reads the daemon's ConfigMap, overlays the credentials from the
// mounted Secret and validates the result.
func LoadConfig(ctx context.Context, k8s k8s.K8sService, namespace, nodeName string) (*Config, error) {
	dynamicCfg, err := getDynamicConfig(ctx, k8s, namespace, nodeName)
	if err != nil {
		return nil, err
	}
	config, err := getConfigFromFileWithMerge("", []byte(dynamicCfg))
	if err != nil {
		return nil, fmt.Errorf("failed parse config: %v", err)
	}
	err = config.loadCredentialFromSecret()
	if err != nil {
		return nil, err
	}
	err = config.PopulateAndValidate()
	if err != nil {
		return nil, err
	}
	log.WithField("config", config.String()).Info("config loaded")
	return config, nil
}
