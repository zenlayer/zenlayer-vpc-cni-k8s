// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
)

// MergeConfigAndUnmarshal

func TestMergeConfigAndUnmarshal(t *testing.T) {
	tests := []struct {
		name    string
		topCfg  string
		baseCfg string
		wantErr bool
		check   func(t *testing.T, c *Config)
	}{
		{
			name:    "both empty is an error",
			topCfg:  "",
			baseCfg: "",
			wantErr: true,
		},
		{
			name:    "only top",
			topCfg:  `{"securityGroup":"sg-top","poolTarget":3}`,
			baseCfg: "",
			check: func(t *testing.T, c *Config) {
				if c.SecurityGroup != "sg-top" {
					t.Errorf("SecurityGroup = %q, want %q", c.SecurityGroup, "sg-top")
				}
				if c.PoolTarget != 3 {
					t.Errorf("PoolTarget = %d, want 3", c.PoolTarget)
				}
			},
		},
		{
			name:    "only base",
			topCfg:  "",
			baseCfg: `{"securityGroup":"sg-base","poolTargetMax":4}`,
			check: func(t *testing.T, c *Config) {
				if c.SecurityGroup != "sg-base" {
					t.Errorf("SecurityGroup = %q, want %q", c.SecurityGroup, "sg-base")
				}
				if c.PoolTargetMax != 4 {
					t.Errorf("PoolTargetMax = %d, want 4", c.PoolTargetMax)
				}
			},
		},
		{
			name:    "top overrides base and base-only keys survive",
			topCfg:  `{"securityGroup":"sg-top"}`,
			baseCfg: `{"securityGroup":"sg-base","vpcID":"vpc-base","poolTargetMax":4}`,
			check: func(t *testing.T, c *Config) {
				if c.SecurityGroup != "sg-top" {
					t.Errorf("SecurityGroup = %q, want the top value %q", c.SecurityGroup, "sg-top")
				}
				if c.VpcID != "vpc-base" {
					t.Errorf("VpcID = %q, want the base value %q", c.VpcID, "vpc-base")
				}
				if c.PoolTargetMax != 4 {
					t.Errorf("PoolTargetMax = %d, want the base value 4", c.PoolTargetMax)
				}
			},
		},
		{
			// RFC7396: arrays are replaced wholesale, never merged element-wise.
			name:    "arrays are replaced not merged",
			topCfg:  `{"subnets":["subnet-top"]}`,
			baseCfg: `{"subnets":["subnet-a","subnet-b"]}`,
			check: func(t *testing.T, c *Config) {
				want := []string{"subnet-top"}
				if !reflect.DeepEqual(c.Subnets, want) {
					t.Errorf("Subnets = %v, want %v", c.Subnets, want)
				}
			},
		},
		{
			// RFC7396: an explicit null in the patch deletes the key.
			name:    "null in top deletes a base key",
			topCfg:  `{"securityGroup":null}`,
			baseCfg: `{"securityGroup":"sg-base","vpcID":"vpc-base"}`,
			check: func(t *testing.T, c *Config) {
				if c.SecurityGroup != "" {
					t.Errorf("SecurityGroup = %q, want it deleted by the null patch", c.SecurityGroup)
				}
				if c.VpcID != "vpc-base" {
					t.Errorf("VpcID = %q, want the base value to survive", c.VpcID)
				}
			},
		},
		{
			// RFC7396: objects merge key by key.
			name:    "objects merge key by key",
			topCfg:  `{"rateLimit":{"a":2,"c":3}}`,
			baseCfg: `{"rateLimit":{"a":1,"b":1}}`,
			check: func(t *testing.T, c *Config) {
				want := map[string]int{"a": 2, "b": 1, "c": 3}
				if !reflect.DeepEqual(c.RateLimit, want) {
					t.Errorf("RateLimit = %v, want %v", c.RateLimit, want)
				}
			},
		},
		{
			name:    "empty json object on both sides yields a zero config",
			topCfg:  `{}`,
			baseCfg: `{}`,
			check: func(t *testing.T, c *Config) {
				if !reflect.DeepEqual(*c, Config{}) {
					t.Errorf("config = %+v, want the zero value", *c)
				}
			},
		},
		{
			name:    "malformed top only",
			topCfg:  `{"securityGroup":`,
			baseCfg: "",
			wantErr: true,
		},
		{
			name:    "malformed base only",
			topCfg:  "",
			baseCfg: `not json`,
			wantErr: true,
		},
		{
			name:    "malformed top during merge",
			topCfg:  `{"securityGroup":`,
			baseCfg: `{"vpcID":"vpc-base"}`,
			wantErr: true,
		},
		{
			name:    "malformed base during merge",
			topCfg:  `{"vpcID":"vpc-top"}`,
			baseCfg: `{{{`,
			wantErr: true,
		},
		{
			name:    "wrong type for a field",
			topCfg:  `{"poolTargetMax":"not-a-number"}`,
			baseCfg: "",
			wantErr: true,
		},
		{
			name:    "unknown fields are ignored",
			topCfg:  `{"securityGroup":"sg-1","somethingNobodyKnows":true}`,
			baseCfg: "",
			check: func(t *testing.T, c *Config) {
				if c.SecurityGroup != "sg-1" {
					t.Errorf("SecurityGroup = %q, want %q", c.SecurityGroup, "sg-1")
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var top, base []byte
			if tt.topCfg != "" {
				top = []byte(tt.topCfg)
			}
			if tt.baseCfg != "" {
				base = []byte(tt.baseCfg)
			}

			got, err := MergeConfigAndUnmarshal(top, base)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("MergeConfigAndUnmarshal() = %+v, want an error", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("MergeConfigAndUnmarshal() error = %v", err)
			}
			if got == nil {
				t.Fatal("MergeConfigAndUnmarshal() returned a nil config with no error")
			}
			if tt.check != nil {
				tt.check(t, got)
			}
		})
	}
}

func TestGetConfigFromFileWithMerge(t *testing.T) {
	t.Run("empty file path skips the file read", func(t *testing.T) {
		got, err := getConfigFromFileWithMerge("", []byte(`{"securityGroup":"sg-1"}`))
		if err != nil {
			t.Fatalf("getConfigFromFileWithMerge() error = %v", err)
		}
		if got.SecurityGroup != "sg-1" {
			t.Errorf("SecurityGroup = %q, want %q", got.SecurityGroup, "sg-1")
		}
	})

	t.Run("empty file path and empty cfg is an error", func(t *testing.T) {
		if _, err := getConfigFromFileWithMerge("", nil); err == nil {
			t.Error("getConfigFromFileWithMerge() error = nil, want an error")
		}
	})

	t.Run("missing file is an error", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "nope.json")
		if _, err := getConfigFromFileWithMerge(missing, []byte(`{}`)); err == nil {
			t.Error("getConfigFromFileWithMerge() error = nil, want an error")
		}
	})

	t.Run("file is the base and cfg is the override", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "base.json")
		if err := os.WriteFile(path, []byte(`{"securityGroup":"sg-file","vpcID":"vpc-file"}`), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := getConfigFromFileWithMerge(path, []byte(`{"securityGroup":"sg-cm"}`))
		if err != nil {
			t.Fatalf("getConfigFromFileWithMerge() error = %v", err)
		}
		if got.SecurityGroup != "sg-cm" {
			t.Errorf("SecurityGroup = %q, want the configmap value %q", got.SecurityGroup, "sg-cm")
		}
		if got.VpcID != "vpc-file" {
			t.Errorf("VpcID = %q, want the file value %q", got.VpcID, "vpc-file")
		}
	})
}

// PopulateAndValidate

// validConfig is a configuration PopulateAndValidate accepts. The tests below
// start from it and change only what they are about.
func validConfig() *Config {
	manage := false
	return &Config{
		IPAMType:                  types.IPAMTypeDirect,
		IPStack:                   string(types.IPStackIPv4),
		ManageUntaggedENI:         &manage,
		CredentialAccessKeyId:     "ak",
		CredentialAccessKeySecret: "sk",
		SecurityGroup:             "sg-1",
		Subnets:                   []string{"subnet-1"},
		PoolTargetMin:             1,
		PoolTarget:                2,
		PoolTargetMax:             3,
	}
}

// TestPopulateAndValidateIPv6Billing pins the public IPv6 billing contract: with
// ipStack dual the billing settings have to be complete and valid before the
// daemon starts, because the platform's own complaint is a MISSING_PARAMETER
// raised from inside pool warm-up, where it reaches an operator as a node that
// cannot create interfaces for no stated reason. See Config.IPv6InternetChargeType
// for what the platform requires and when.
func TestPopulateAndValidateIPv6Billing(t *testing.T) {
	t.Run("charge type and cap default", func(t *testing.T) {
		c := validConfig()
		c.IPStack = string(types.IPStackDual)
		c.IPv6PackageSize = 1
		if err := c.PopulateAndValidate(); err != nil {
			t.Fatalf("PopulateAndValidate() = %v, want nil", err)
		}
		if c.IPv6InternetChargeType != DefaultIPv6InternetChargeType {
			t.Errorf("IPv6InternetChargeType = %q, want %q", c.IPv6InternetChargeType, DefaultIPv6InternetChargeType)
		}
		if c.IPv6Bandwidth != DefaultIPv6BandwidthMbps {
			t.Errorf("IPv6Bandwidth = %d, want %d", c.IPv6Bandwidth, DefaultIPv6BandwidthMbps)
		}
	})

	t.Run("ByTrafficPackage without a package size is refused", func(t *testing.T) {
		c := validConfig()
		c.IPStack = string(types.IPStackDual)
		c.IPv6InternetChargeType = IPv6ChargeByTrafficPackage
		err := c.PopulateAndValidate()
		if err == nil {
			t.Fatal("PopulateAndValidate() = nil, want an error naming ipv6PackageSize")
		}
		if !strings.Contains(err.Error(), "ipv6PackageSize") {
			t.Errorf("error = %q, want it to name ipv6PackageSize", err)
		}
	})

	t.Run("BandwidthCluster without a cluster id is refused", func(t *testing.T) {
		c := validConfig()
		c.IPStack = string(types.IPStackDual)
		c.IPv6InternetChargeType = IPv6ChargeBandwidthCluster
		err := c.PopulateAndValidate()
		if err == nil {
			t.Fatal("PopulateAndValidate() = nil, want an error naming ipv6ClusterId")
		}
		if !strings.Contains(err.Error(), "ipv6ClusterId") {
			t.Errorf("error = %q, want it to name ipv6ClusterId", err)
		}
	})

	t.Run("ByBandwidth needs nothing further", func(t *testing.T) {
		c := validConfig()
		c.IPStack = string(types.IPStackDual)
		c.IPv6InternetChargeType = IPv6ChargeByBandwidth
		if err := c.PopulateAndValidate(); err != nil {
			t.Fatalf("PopulateAndValidate() = %v, want nil", err)
		}
	})

	t.Run("an unknown charge type is refused", func(t *testing.T) {
		c := validConfig()
		c.IPStack = string(types.IPStackDual)
		c.IPv6InternetChargeType = "Free"
		if err := c.PopulateAndValidate(); err == nil {
			t.Fatal("PopulateAndValidate() = nil, want an error")
		}
	})

	t.Run("ipv4 ignores the billing settings entirely", func(t *testing.T) {
		c := validConfig()
		c.IPStack = string(types.IPStackIPv4)
		c.IPv6InternetChargeType = "Free"
		if err := c.PopulateAndValidate(); err != nil {
			t.Fatalf("PopulateAndValidate() = %v, want nil: ipv4 must not read ipv6 billing", err)
		}
	})
}

// The ipStack switch in PopulateAndValidate and the enum in the chart's
// values.schema.json both gate this value, so the set accepted here has to stay
// a superset of the enum: a mode the schema lets through and the daemon rejects
// fails at daemon start on every node instead of at install time.
func TestPopulateAndValidateAcceptsStackModes(t *testing.T) {
	for _, mode := range []string{string(types.IPStackIPv4), string(types.IPStackDual)} {
		t.Run("ipStack="+mode, func(t *testing.T) {
			c := validConfig()
			c.IPStack = mode
			// dual additionally needs the public IPv6 billing settings: the
			// charge type and the cap have defaults, the traffic commitment
			// does not. See TestPopulateAndValidateIPv6Billing.
			if types.IPStack(mode).EnableIPv6() {
				c.IPv6PackageSize = 1
			}
			if err := c.PopulateAndValidate(); err != nil {
				t.Fatalf("PopulateAndValidate() with ipStack %q = %v, want nil", mode, err)
			}
			want := mode
			if c.IPStack != want {
				t.Errorf("IPStack = %q, want %q", c.IPStack, want)
			}
		})
	}
}

func TestPopulateAndValidateRejects(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(c *Config)
		wantMsg string
	}{
		{
			name:    "missing access key id",
			mutate:  func(c *Config) { c.CredentialAccessKeyId = "" },
			wantMsg: "no credentials for the zenlayer api",
		},
		{
			name:    "missing access key secret",
			mutate:  func(c *Config) { c.CredentialAccessKeySecret = "" },
			wantMsg: "no credentials for the zenlayer api",
		},
		{
			name:    "missing both credentials",
			mutate:  func(c *Config) { c.CredentialAccessKeyId, c.CredentialAccessKeySecret = "", "" },
			wantMsg: "no credentials for the zenlayer api",
		},
		{
			name:    "missing security group",
			mutate:  func(c *Config) { c.SecurityGroup = "" },
			wantMsg: "securityGroup is not set",
		},
		{
			name:    "nil subnets",
			mutate:  func(c *Config) { c.Subnets = nil },
			wantMsg: "subnets is empty",
		},
		{
			name:    "empty subnets",
			mutate:  func(c *Config) { c.Subnets = []string{} },
			wantMsg: "subnets is empty",
		},
		{
			name: "too many subnets",
			mutate: func(c *Config) {
				c.Subnets = make([]string, MaxSubnetNum+1)
				for i := range c.Subnets {
					c.Subnets[i] = "subnet"
				}
			},
			wantMsg: "at most 5 are supported",
		},
		{
			name:    "pool target min above pool target",
			mutate:  func(c *Config) { c.PoolTargetMin = 3; c.PoolTarget = 2 },
			wantMsg: "poolTargetMin (3) must be less than or equal to poolTarget (2)",
		},
		{
			name:    "pool target above pool target max",
			mutate:  func(c *Config) { c.PoolTarget = 5; c.PoolTargetMax = 4 },
			wantMsg: "poolTarget (5) must be less than or equal to poolTargetMax (4)",
		},
		{
			name:    "zero pool target without cap probe",
			mutate:  func(c *Config) { c.PoolTargetMin, c.PoolTarget = 0, 0 },
			wantMsg: "must both be > 0 unless poolTargetCapProbe is true",
		},
		{
			name:    "zero pool target max without cap probe",
			mutate:  func(c *Config) { c.PoolTargetMin, c.PoolTarget, c.PoolTargetMax = 0, 0, 0 },
			wantMsg: "must both be > 0 unless poolTargetCapProbe is true",
		},
		{
			// IPv6-only is rejected with its own message rather than the
			// generic one, so that "not built yet" cannot be mistaken for
			// "you misspelled it".
			name:    "ipv6-only is rejected as unimplemented, not as a typo",
			mutate:  func(c *Config) { c.IPStack = string(types.IPStackIPv6) },
			wantMsg: "(IPv6-only) is not implemented yet",
		},
		{
			// No default: a default that changed in a later release would
			// silently change the families an existing node serves.
			name:    "unset ip stack",
			mutate:  func(c *Config) { c.IPStack = "" },
			wantMsg: "ipStack is not set",
		},
		{
			name:    "unset manageUntaggedENI",
			mutate:  func(c *Config) { c.ManageUntaggedENI = nil },
			wantMsg: "manageUntaggedENI is not set",
		},
		{
			name:    "unknown ip stack",
			mutate:  func(c *Config) { c.IPStack = "ipv9" },
			wantMsg: `unsupported ipStack "ipv9"`,
		},
		{
			name:    "unsupported network mode",
			mutate:  func(c *Config) { c.NetworkMode = "veth" },
			wantMsg: `unsupported daemon mode "veth"`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(c)
			err := c.PopulateAndValidate()
			if err == nil {
				t.Fatal("PopulateAndValidate() error = nil, want an error")
			}
			if !strings.Contains(err.Error(), tt.wantMsg) {
				t.Errorf("PopulateAndValidate() error = %q, want it to contain %q", err.Error(), tt.wantMsg)
			}
		})
	}
}

func TestPopulateAndValidateAccepts(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(c *Config)
	}{
		{"minimal valid config", func(c *Config) {}},
		{"min equals target equals max", func(c *Config) { c.PoolTargetMin, c.PoolTarget, c.PoolTargetMax = 2, 2, 2 }},
		{"explicit ipv4", func(c *Config) { c.IPStack = string(types.IPStackIPv4) }},
		{"explicit network mode", func(c *Config) { c.NetworkMode = types.ModeENIMultiIP }},
		{
			name: "exactly the maximum number of subnets",
			mutate: func(c *Config) {
				c.Subnets = make([]string, MaxSubnetNum)
				for i := range c.Subnets {
					c.Subnets[i] = "subnet"
				}
			},
		},
		{
			// The cap-probe escape hatch: the pool cap comes from the
			// instance's own limits instead of poolTargetMax, so the pool
			// targets may be left at zero.
			name: "cap probe allows zero pool targets",
			mutate: func(c *Config) {
				c.PoolTargetCapProbe = true
				c.PoolTargetMin, c.PoolTarget, c.PoolTargetMax = 0, 0, 0
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			tt.mutate(c)
			if err := c.PopulateAndValidate(); err != nil {
				t.Fatalf("PopulateAndValidate() error = %v, want nil", err)
			}
		})
	}
}

// The cap-probe escape hatch only covers the "is it zero" checks; the ordering
// rules still apply, because a min above a target is a typo either way.
func TestPopulateAndValidateCapProbeDoesNotBypassOrdering(t *testing.T) {
	c := validConfig()
	c.PoolTargetCapProbe = true
	c.PoolTargetMin, c.PoolTarget = 3, 1
	if err := c.PopulateAndValidate(); err == nil {
		t.Error("PopulateAndValidate() error = nil, want the ordering rule to still apply under cap probe")
	}
}

func TestPopulateAndValidateDefaults(t *testing.T) {
	c := validConfig()
	if err := c.PopulateAndValidate(); err != nil {
		t.Fatalf("PopulateAndValidate() error = %v", err)
	}

	checks := []struct {
		field string
		got   any
		want  any
	}{
		{"PoolCapRatio", c.PoolCapRatio, float64(DefaultPoolCapRatio)},
		{"PoolMonitorIntervalSec", c.PoolMonitorIntervalSec, uint32(DefaultPoolMonitorIntervalSec)},
		{"SubnetStatAgingSec", c.SubnetStatAgingSec, uint32(DefaultSubnetStatAgingSec)},
		{"SubnetStatUpdateIntervalSec", c.SubnetStatUpdateIntervalSec, uint32(DefaultSubnetStatUpdateIntervalSec)},
		{"PoolGCPeriodSec", c.PoolGCPeriodSec, uint32(DefaultGcProtectPeriodSec)},
		{"NetworkMode", c.NetworkMode, types.ModeENIMultiIP},
		{"HealthAndDebugPort", c.HealthAndDebugPort, uint32(DefaultDebugPort)},
	}
	for _, ck := range checks {
		t.Run(ck.field, func(t *testing.T) {
			if ck.got != ck.want {
				t.Errorf("%s = %v, want the default %v", ck.field, ck.got, ck.want)
			}
		})
	}
}

// Explicitly configured values must survive defaulting.
func TestPopulateAndValidateKeepsExplicitValues(t *testing.T) {
	c := validConfig()
	c.PoolCapRatio = 0.5
	c.PoolMonitorIntervalSec = 7
	c.SubnetStatAgingSec = 8
	c.SubnetStatUpdateIntervalSec = 9
	c.PoolGCPeriodSec = 10
	c.HealthAndDebugPort = 18080
	c.ReconcileIntervalSec = 11

	if err := c.PopulateAndValidate(); err != nil {
		t.Fatalf("PopulateAndValidate() error = %v", err)
	}

	if c.PoolCapRatio != 0.5 {
		t.Errorf("PoolCapRatio = %v, want 0.5", c.PoolCapRatio)
	}
	if c.PoolMonitorIntervalSec != 7 {
		t.Errorf("PoolMonitorIntervalSec = %d, want 7", c.PoolMonitorIntervalSec)
	}
	if c.SubnetStatAgingSec != 8 {
		t.Errorf("SubnetStatAgingSec = %d, want 8", c.SubnetStatAgingSec)
	}
	if c.SubnetStatUpdateIntervalSec != 9 {
		t.Errorf("SubnetStatUpdateIntervalSec = %d, want 9", c.SubnetStatUpdateIntervalSec)
	}
	if c.PoolGCPeriodSec != 10 {
		t.Errorf("PoolGCPeriodSec = %d, want 10", c.PoolGCPeriodSec)
	}
	if c.HealthAndDebugPort != 18080 {
		t.Errorf("HealthAndDebugPort = %d, want 18080", c.HealthAndDebugPort)
	}
	// ReconcileIntervalSec has no defaulting today; it is only logged.
	if c.ReconcileIntervalSec != 11 {
		t.Errorf("ReconcileIntervalSec = %d, want 11", c.ReconcileIntervalSec)
	}
}

// PoolCapRatio is a fraction: anything outside (0, 1] is coerced to
// DefaultPoolCapRatio rather than rejected.
func TestPopulateAndValidatePoolCapRatio(t *testing.T) {
	tests := []struct {
		name string
		in   float64
		want float64
	}{
		{"zero is coerced", 0, DefaultPoolCapRatio},
		{"negative is coerced", -0.5, DefaultPoolCapRatio},
		{"above one is coerced", 1.5, DefaultPoolCapRatio},
		{"one is kept", 1, 1},
		{"a fraction is kept", 0.25, 0.25},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := validConfig()
			c.PoolCapRatio = tt.in
			if err := c.PopulateAndValidate(); err != nil {
				t.Fatalf("PopulateAndValidate() error = %v", err)
			}
			if c.PoolCapRatio != tt.want {
				t.Errorf("PoolCapRatio = %v, want %v", c.PoolCapRatio, tt.want)
			}
		})
	}
}

// String

func TestStringRedactsCredentials(t *testing.T) {
	const (
		ak = "AKIDsupersecretkeyid"
		sk = "sUpErSeCrEtAccessKeySecretValue"
	)
	c := validConfig()
	c.CredentialAccessKeyId = ak
	c.CredentialAccessKeySecret = sk

	out := c.String()

	if strings.Contains(out, ak) {
		t.Errorf("String() leaked CredentialAccessKeyId; output = %s", out)
	}
	if strings.Contains(out, sk) {
		t.Errorf("String() leaked CredentialAccessKeySecret; output = %s", out)
	}
	if n := strings.Count(out, "<redacted>"); n != 2 {
		t.Errorf("String() contains %d <redacted> markers, want 2; output = %s", n, out)
	}

	// Redaction must not swallow the rest of the config -- the log line is
	// still supposed to be useful.
	for _, want := range []string{"sg-1", "subnet-1"} {
		if !strings.Contains(out, want) {
			t.Errorf("String() dropped non-credential value %q; output = %s", want, out)
		}
	}

	// The receiver must be untouched: String() is called for logging, not to
	// scrub the live config the daemon then authenticates with.
	if c.CredentialAccessKeyId != ak || c.CredentialAccessKeySecret != sk {
		t.Errorf("String() mutated the receiver: id=%q secret=%q", c.CredentialAccessKeyId, c.CredentialAccessKeySecret)
	}
}

func TestStringWithEmptyCredentials(t *testing.T) {
	c := &Config{SecurityGroup: "sg-1"}
	out := c.String()
	if strings.Contains(out, "<redacted>") {
		t.Errorf("String() redacted an empty credential; output = %s", out)
	}
	if !strings.Contains(out, "sg-1") {
		t.Errorf("String() dropped SecurityGroup; output = %s", out)
	}
}

func TestStringRedactsWhenOnlyOneCredentialIsSet(t *testing.T) {
	c := &Config{CredentialAccessKeyId: "only-the-id"}
	out := c.String()
	if strings.Contains(out, "only-the-id") {
		t.Errorf("String() leaked CredentialAccessKeyId; output = %s", out)
	}
	if n := strings.Count(out, "<redacted>"); n != 1 {
		t.Errorf("String() contains %d <redacted> markers, want 1; output = %s", n, out)
	}
}

func TestStringOnNilConfig(t *testing.T) {
	var c *Config
	if got := c.String(); got != "" {
		t.Errorf("(*Config)(nil).String() = %q, want the empty string", got)
	}
}

// loadCredentialFromSecret

// setSecretPaths points the loader at a temp dir. A non-nil ak or sk is written
// to the corresponding path; nil means the file is not created at all.
func setSecretPaths(t *testing.T, ak, sk *string) (akPath, skPath string) {
	t.Helper()
	dir := t.TempDir()
	akPath = filepath.Join(dir, "AccessKeyID")
	skPath = filepath.Join(dir, "AccessKeyPassword")
	t.Setenv(envSecretPathAK, akPath)
	t.Setenv(envSecretPathSK, skPath)

	if ak != nil {
		if err := os.WriteFile(akPath, []byte(*ak), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if sk != nil {
		if err := os.WriteFile(skPath, []byte(*sk), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return akPath, skPath
}

func strptr(s string) *string { return &s }

func TestLoadCredentialFromSecret(t *testing.T) {
	tests := []struct {
		name   string
		akFile *string
		skFile *string
		cfgAK  string
		cfgSK  string
		wantAK string
		wantSK string
	}{
		{
			name:   "secret files win over the configmap",
			akFile: strptr("file-ak"),
			skFile: strptr("file-sk"),
			cfgAK:  "cm-ak",
			cfgSK:  "cm-sk",
			wantAK: "file-ak",
			wantSK: "file-sk",
		},
		{
			name:   "secret files populate empty configmap values",
			akFile: strptr("file-ak"),
			skFile: strptr("file-sk"),
			wantAK: "file-ak",
			wantSK: "file-sk",
		},
		{
			name:   "missing files keep the configmap values",
			akFile: nil,
			skFile: nil,
			cfgAK:  "cm-ak",
			cfgSK:  "cm-sk",
			wantAK: "cm-ak",
			wantSK: "cm-sk",
		},
		{
			name:   "missing files and no configmap values leave both empty",
			akFile: nil,
			skFile: nil,
			wantAK: "",
			wantSK: "",
		},
		{
			name:   "only the ak file is mounted",
			akFile: strptr("file-ak"),
			skFile: nil,
			cfgAK:  "cm-ak",
			cfgSK:  "cm-sk",
			wantAK: "file-ak",
			wantSK: "cm-sk",
		},
		{
			name:   "only the sk file is mounted",
			akFile: nil,
			skFile: strptr("file-sk"),
			cfgAK:  "cm-ak",
			cfgSK:  "cm-sk",
			wantAK: "cm-ak",
			wantSK: "file-sk",
		},
		{
			// kubectl create secret --from-file keeps the trailing newline.
			name:   "trailing newline is trimmed",
			akFile: strptr("file-ak\n"),
			skFile: strptr("file-sk\r\n"),
			wantAK: "file-ak",
			wantSK: "file-sk",
		},
		{
			name:   "surrounding whitespace is trimmed",
			akFile: strptr("  file-ak  \n\n"),
			skFile: strptr("\tfile-sk\t"),
			wantAK: "file-ak",
			wantSK: "file-sk",
		},
		{
			// An empty mount must not blank out a working configmap value.
			name:   "empty file is ignored",
			akFile: strptr(""),
			skFile: strptr(""),
			cfgAK:  "cm-ak",
			cfgSK:  "cm-sk",
			wantAK: "cm-ak",
			wantSK: "cm-sk",
		},
		{
			name:   "whitespace only file is ignored",
			akFile: strptr("   \n\t "),
			skFile: strptr("\n"),
			cfgAK:  "cm-ak",
			cfgSK:  "cm-sk",
			wantAK: "cm-ak",
			wantSK: "cm-sk",
		},
		{
			name:   "inner whitespace is preserved",
			akFile: strptr("file ak\n"),
			skFile: strptr("file/sk+with=padding==\n"),
			wantAK: "file ak",
			wantSK: "file/sk+with=padding==",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setSecretPaths(t, tt.akFile, tt.skFile)

			c := &Config{CredentialAccessKeyId: tt.cfgAK, CredentialAccessKeySecret: tt.cfgSK}
			if err := c.loadCredentialFromSecret(); err != nil {
				t.Fatalf("loadCredentialFromSecret() error = %v", err)
			}
			if c.CredentialAccessKeyId != tt.wantAK {
				t.Errorf("CredentialAccessKeyId = %q, want %q", c.CredentialAccessKeyId, tt.wantAK)
			}
			if c.CredentialAccessKeySecret != tt.wantSK {
				t.Errorf("CredentialAccessKeySecret = %q, want %q", c.CredentialAccessKeySecret, tt.wantSK)
			}
		})
	}
}

// An unset override env var must leave the documented default mount path in
// place: loadCredentialFromSecret takes the env value only when it is non-empty,
// and a default path that is not mounted is tolerated.
func TestLoadCredentialFromSecretDefaultPaths(t *testing.T) {
	t.Setenv(envSecretPathAK, "")
	t.Setenv(envSecretPathSK, "")

	if defaultSecretPathAK != "/etc/zec_secret/AccessKeyID" {
		t.Errorf("defaultSecretPathAK = %q, want the documented mount path", defaultSecretPathAK)
	}
	if defaultSecretPathSK != "/etc/zec_secret/AccessKeyPassword" {
		t.Errorf("defaultSecretPathSK = %q, want the documented mount path", defaultSecretPathSK)
	}

	if _, err := os.Stat(defaultSecretPathAK); err == nil {
		t.Skip("the default secret path exists on this machine; skipping the fallback assertion")
	}

	c := &Config{CredentialAccessKeyId: "cm-ak", CredentialAccessKeySecret: "cm-sk"}
	if err := c.loadCredentialFromSecret(); err != nil {
		t.Fatalf("loadCredentialFromSecret() error = %v, want a missing mount to be tolerated", err)
	}
	if c.CredentialAccessKeyId != "cm-ak" || c.CredentialAccessKeySecret != "cm-sk" {
		t.Errorf("configmap credentials were not kept: id=%q secret=%q", c.CredentialAccessKeyId, c.CredentialAccessKeySecret)
	}
}

// A mount that exists but cannot be read is a real failure, not a fallback:
// silently starting with the configmap credentials would hide a broken mount.
func TestLoadCredentialFromSecretUnreadablePathIsAnError(t *testing.T) {
	dir := t.TempDir()
	akDir := filepath.Join(dir, "AccessKeyID")
	if err := os.Mkdir(akDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv(envSecretPathAK, akDir)
	t.Setenv(envSecretPathSK, filepath.Join(dir, "missing-sk"))

	c := &Config{}
	err := c.loadCredentialFromSecret()
	if err == nil {
		t.Fatal("loadCredentialFromSecret() error = nil, want an error for an unreadable credential path")
	}
	if !strings.Contains(err.Error(), "read credential from") {
		t.Errorf("loadCredentialFromSecret() error = %q, want it to name the failing path", err.Error())
	}
}

// The end-to-end contract LoadConfig relies on: credentials arriving only from
// the Secret are enough to pass validation.
func TestSecretOnlyCredentialsPassValidation(t *testing.T) {
	setSecretPaths(t, strptr("file-ak\n"), strptr("file-sk\n"))

	c := validConfig()
	c.CredentialAccessKeyId = ""
	c.CredentialAccessKeySecret = ""

	if err := c.loadCredentialFromSecret(); err != nil {
		t.Fatalf("loadCredentialFromSecret() error = %v", err)
	}
	if err := c.PopulateAndValidate(); err != nil {
		t.Fatalf("PopulateAndValidate() error = %v, want credentials from the secret to be accepted", err)
	}
}

// Conversely, no Secret and no ConfigMap credentials must be rejected rather
// than letting the daemon start with empty credentials.
func TestNoCredentialsAnywhereIsRejected(t *testing.T) {
	setSecretPaths(t, nil, nil)

	c := validConfig()
	c.CredentialAccessKeyId = ""
	c.CredentialAccessKeySecret = ""

	if err := c.loadCredentialFromSecret(); err != nil {
		t.Fatalf("loadCredentialFromSecret() error = %v", err)
	}
	// The daemon picks the node event reason by this sentinel.
	if err := c.PopulateAndValidate(); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("PopulateAndValidate() error = %v, want ErrNoCredentials", err)
	}
}

// An unvalidated Config must not panic, and must not read unset as adoption:
// adoption can detach and delete an interface the operator attached.
func TestManageUntaggedUnsetIsNotAdoption(t *testing.T) {
	var c Config
	if c.ManageUntagged() {
		t.Error("ManageUntagged() = true with the field unset, want false")
	}
	on := true
	c.ManageUntaggedENI = &on
	if !c.ManageUntagged() {
		t.Error("ManageUntagged() = false with the field set to true")
	}
}

// TestIPAMTypeIsRequired pins ipamType as a behavior switch with no default.
func TestIPAMTypeIsRequired(t *testing.T) {
	for _, v := range []types.IPAMType{"", "Direct", "controlplane"} {
		c := validConfig()
		c.IPAMType = v
		if err := c.PopulateAndValidate(); err == nil {
			t.Errorf("ipamType %q accepted", v)
		}
	}
}

func crdConfig() *Config {
	return &Config{IPAMType: types.IPAMTypeCRD, IPStack: string(types.IPStackDual)}
}

// TestCRDModeAcceptsMinimalConfig: a crd daemon needs only ipamType and
// ipStack; the rest is the controlplane's.
func TestCRDModeAcceptsMinimalConfig(t *testing.T) {
	c := crdConfig()
	if err := c.PopulateAndValidate(); err != nil {
		t.Fatalf("minimal crd config rejected: %v", err)
	}
	if c.NetworkMode != types.ModeENIMultiIP || c.PoolMonitorIntervalSec == 0 || c.HealthAndDebugPort == 0 {
		t.Fatalf("defaults not filled: %+v", c)
	}
}

// TestCRDModeRejectsPlatformSettings: a platform setting on a crd node would
// be silently unused, so it is refused -- credentials above all, which a crd
// node must not hold.
func TestCRDModeRejectsPlatformSettings(t *testing.T) {
	manage := true
	for name, set := range map[string]func(*Config){
		"credentials":   func(c *Config) { c.CredentialAccessKeyId, c.CredentialAccessKeySecret = "ak", "sk" },
		"openAPIDomain": func(c *Config) { c.OpenAPIDomain = "example.com" },
		"vpcID":         func(c *Config) { c.VpcID = "vpc-1" },
		"subnets":       func(c *Config) { c.Subnets = []string{"subnet-1"} },
		"securityGroup": func(c *Config) { c.SecurityGroup = "sg-1" },
		"manage":        func(c *Config) { c.ManageUntaggedENI = &manage },
		"ipv6 billing":  func(c *Config) { c.IPv6Bandwidth = 10 },
		"pool sizing":   func(c *Config) { c.PoolTarget = 5 },
		"subnet stats":  func(c *Config) { c.SubnetStatAgingSec = 40 },
		"rate limit":    func(c *Config) { c.RateLimit = map[string]int{"x": 1} },
		"ipStack unset": func(c *Config) { c.IPStack = "" },
		"ipStack ipv6":  func(c *Config) { c.IPStack = string(types.IPStackIPv6) },
	} {
		c := crdConfig()
		set(c)
		if err := c.PopulateAndValidate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// poolTargetMax is ignored while the cap is probed, so a target above it is not
// an error then.
func TestPopulateAndValidateCapProbeIgnoresTargetMax(t *testing.T) {
	c := validConfig()
	c.PoolTargetCapProbe = true
	c.PoolTarget, c.PoolTargetMax = 10, 5
	if err := c.PopulateAndValidate(); err != nil {
		t.Fatalf("PopulateAndValidate() error = %v, want nil under cap probe", err)
	}
	c = validConfig()
	c.PoolTarget, c.PoolTargetMax = 10, 5
	if err := c.PopulateAndValidate(); err == nil {
		t.Fatal("PopulateAndValidate() error = nil, want poolTarget > poolTargetMax rejected without cap probe")
	}
}

// ipv6 is refused as unimplemented in crd mode too, not as an unknown value.
func TestCRDModeIPv6NotImplemented(t *testing.T) {
	c := crdConfig()
	c.IPStack = string(types.IPStackIPv6)
	err := c.PopulateAndValidate()
	if err == nil || !strings.Contains(err.Error(), "not implemented yet") {
		t.Fatalf("err %v; want the not-implemented answer", err)
	}
}
