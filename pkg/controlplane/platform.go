// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"
	"golang.org/x/time/rate"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/metrics"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/tracing"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/ec2"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
)

var log = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "controlplane"})

const (
	credentialReloadPeriod = time.Minute
	// flowLimitPause is how long every call waits after the platform says the
	// account is over its rate. Global, not per node: N nodes backing off on
	// their own is what drove the account over the limit in the first place.
	flowLimitPause = 30 * time.Second
)

// Health states reported on /healthz.
const (
	healthOK                 = ""
	healthCredentialInvalid  = "credential invalid"
	healthOpenAPIUnreachable = "openapi unreachable"
	healthClockSkew          = "clock skew: request signatures rejected"
)

// Platform is the controlplane's single OpenAPI client: the credentials are
// swapped in place when the files change, every call waits on a cluster-wide
// token bucket (reads and writes separately), and a flow-limit answer pauses
// all calls. It implements ec2.EC2, so the platform operations written for the
// daemon run on it unchanged.
type Platform struct {
	domain string
	dir    string

	client atomic.Pointer[ec2.EC2]
	// loaded identifies the credentials behind client, so an unchanged file is
	// not turned into a new client every minute.
	loaded atomic.Pointer[string]

	read, write *rate.Limiter

	mu         sync.Mutex
	pauseUntil time.Time
	health     string
}

var _ ec2.EC2 = (*Platform)(nil)

// NewPlatform reads the credentials once and fails if they are unusable.
func NewPlatform(cfg *Config) (*Platform, error) {
	p := &Platform{
		domain: cfg.OpenAPIDomain,
		dir:    cfg.CredentialDir,
		read:   rate.NewLimiter(rate.Limit(cfg.RateLimit.ReadQPS), cfg.RateLimit.ReadBurst),
		write:  rate.NewLimiter(rate.Limit(cfg.RateLimit.WriteQPS), cfg.RateLimit.WriteBurst),
	}
	if err := p.reload(); err != nil {
		return nil, err
	}
	return p, nil
}

// RunCredentialReload re-reads the credential files until ctx ends.
func (p *Platform) RunCredentialReload(ctx context.Context) {
	t := time.NewTicker(credentialReloadPeriod)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := p.reload(); err != nil {
				log.Errorf("Reload OpenAPI credentials: %v; keeping the previous ones", err)
			}
		}
	}
}

func (p *Platform) reload() error {
	cred, err := readCredentials(p.dir)
	if err != nil {
		return err
	}
	if cred.temporary() && !time.Now().Before(cred.expiration) {
		// Keep using it: the files may be renewed on the next reload, and an
		// expired token fails no worse than none. Say so, since every call
		// will now be refused as unauthenticated.
		log.Errorf("OpenAPI credential key %s expired at %s and has not been renewed",
			maskKey(cred.id), cred.expiration.Format(time.RFC3339))
	}
	// The secret half is compared by building a new client every time it might
	// have changed: a key ID alone can stay the same across a rotation.
	fingerprint := cred.id + "\x00" + cred.secret + "\x00" + cred.token
	if cur := p.loaded.Load(); cur != nil && *cur == fingerprint {
		return nil
	}
	var c *ec2.ZecClient
	if cred.temporary() {
		c, err = ec2.NewZecClientWithStsCredential(zecCom.NewStsCredential(cred.id, cred.secret, cred.token), p.domain)
	} else {
		c, err = ec2.NewZecClient(cred.id, cred.secret, p.domain)
	}
	if err != nil {
		return fmt.Errorf("create OpenAPI client: %w", err)
	}
	var wrapped ec2.EC2 = metrics.NewMetricEC2Wrapper(c)
	p.client.Store(&wrapped)
	p.loaded.Store(&fingerprint)
	if cred.temporary() {
		log.Infof("OpenAPI temporary credentials loaded, key %s, expiring %s",
			maskKey(cred.id), cred.expiration.Format(time.RFC3339))
	} else {
		log.Infof("OpenAPI credentials loaded, key %s", maskKey(cred.id))
	}
	return nil
}

// credentials is what CredentialDir holds: either a long-term access key
// (AccessKeyID, AccessKeyPassword -- the Secret the chart mounts), or a
// temporary credential issued by AssumeRole (AccessKeyID, AccessKeySecret,
// SecurityToken, and Expiration in RFC 3339 -- what the platform writes for a
// controlplane it runs, renewing the files well before expiry). The presence
// of SecurityToken tells them apart. Never log the secret or the token.
type credentials struct {
	id, secret, token string
	expiration        time.Time
}

func (c credentials) temporary() bool { return c.token != "" }

// readCredentials reads the directory twice and accepts it only if both reads
// agree: each file is replaced on its own, so a read in the middle of an
// update can pair a new key with an old secret or token.
func readCredentials(dir string) (credentials, error) {
	first, err := readCredentialsOnce(dir)
	if err != nil {
		return credentials{}, err
	}
	second, err := readCredentialsOnce(dir)
	if err != nil {
		return credentials{}, err
	}
	if first != second {
		return credentials{}, fmt.Errorf("credentials changed while being read; trying again on the next reload")
	}
	return first, nil
}

func readCredentialsOnce(dir string) (credentials, error) {
	id, err := readCredential(dir, "AccessKeyID")
	if err != nil {
		return credentials{}, err
	}
	if _, err := os.Stat(filepath.Join(dir, "SecurityToken")); errors.Is(err, os.ErrNotExist) {
		secret, err := readCredential(dir, "AccessKeyPassword")
		if err != nil {
			return credentials{}, err
		}
		return credentials{id: id, secret: secret}, nil
	}
	c := credentials{id: id}
	if c.secret, err = readCredential(dir, "AccessKeySecret"); err != nil {
		return credentials{}, err
	}
	if c.token, err = readCredential(dir, "SecurityToken"); err != nil {
		return credentials{}, err
	}
	exp, err := readCredential(dir, "Expiration")
	if err != nil {
		return credentials{}, err
	}
	if c.expiration, err = time.Parse(time.RFC3339, exp); err != nil {
		return credentials{}, fmt.Errorf("credential Expiration in %s is not RFC 3339: %w", dir, err)
	}
	return c, nil
}

func readCredential(dir, name string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", fmt.Errorf("read credential %s: %w", name, err)
	}
	v := strings.TrimSpace(string(b))
	if v == "" {
		return "", fmt.Errorf("credential %s in %s is empty", name, dir)
	}
	return v, nil
}

func maskKey(k string) string {
	if len(k) <= 4 {
		return "****"
	}
	return k[:4] + "****"
}

// Health returns "" when the last call reached the platform with a usable
// credential, or the reason it did not.
func (p *Platform) Health() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.health
}

// wait blocks for a token, and for any flow-limit pause -- checked again after
// the token, since a pause may have started while this call was queued.
func (p *Platform) wait(l *rate.Limiter) {
	p.sleepPause()
	_ = l.Wait(context.Background())
	p.sleepPause()
}

func (p *Platform) sleepPause() {
	for {
		p.mu.Lock()
		until := p.pauseUntil
		p.mu.Unlock()
		d := time.Until(until)
		if d <= 0 {
			return
		}
		time.Sleep(d)
	}
}

// observe records what a call's outcome says about the platform as a whole.
func (p *Platform) observe(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		p.health = healthOK
		return
	}
	switch apiErr.OpenAPIErrEventReason(err) {
	case tracing.EventOpenApiFlowLimit:
		p.pauseUntil = time.Now().Add(flowLimitPause)
		log.Warnf("OpenAPI flow limit reached; pausing every call for %s", flowLimitPause)
	case tracing.EventCredentialServiceAbnormal:
		p.health = healthCredentialInvalid
	case tracing.EventOpenApiUnreachable:
		p.health = healthOpenAPIUnreachable
	case tracing.EventNodeClockSkew:
		p.health = healthClockSkew
	default:
		// A refusal with a code is the platform answering: reachable and
		// authenticated.
		if apiErr.ErrCode(err) != "" {
			p.health = healthOK
		}
	}
}

func (p *Platform) c() ec2.EC2 { return *p.client.Load() }

func call[T any](p *Platform, l *rate.Limiter, f func(ec2.EC2) (T, error)) (T, error) {
	p.wait(l)
	r, err := f(p.c())
	p.observe(err)
	return r, err
}

func (p *Platform) DescribeInstances(ids []string) (*zecApi.DescribeInstancesResponse, error) {
	return call(p, p.read, func(c ec2.EC2) (*zecApi.DescribeInstancesResponse, error) { return c.DescribeInstances(ids) })
}

// DescribeInstancesByIPv4 lists instances holding ip; see
// ec2.ZecClient.DescribeInstancesByIPv4 for why the answer needs filtering.
func (p *Platform) DescribeInstancesByIPv4(ip string, pageNum int) (*zecApi.DescribeInstancesResponse, error) {
	return call(p, p.read, func(c ec2.EC2) (*zecApi.DescribeInstancesResponse, error) {
		d, ok := c.(ec2.InstanceByIPDescriber)
		if !ok {
			return nil, fmt.Errorf("the platform client cannot describe instances by address")
		}
		return d.DescribeInstancesByIPv4(ip, pageNum)
	})
}

func (p *Platform) DescribeSubnets(ids []string) (*zecApi.DescribeSubnetsResponse, error) {
	return call(p, p.read, func(c ec2.EC2) (*zecApi.DescribeSubnetsResponse, error) { return c.DescribeSubnets(ids) })
}

func (p *Platform) DescribeVpcs(ids []string) (*zecApi.DescribeVpcsResponse, error) {
	return call(p, p.read, func(c ec2.EC2) (*zecApi.DescribeVpcsResponse, error) { return c.DescribeVpcs(ids) })
}

func (p *Platform) DescribeNetworkInterfaceAttributes(req *zecApi.DescribeNetworkInterfacesRequest) (*zecApi.DescribeNetworkInterfacesResponse, error) {
	return call(p, p.read, func(c ec2.EC2) (*zecApi.DescribeNetworkInterfacesResponse, error) {
		return c.DescribeNetworkInterfaceAttributes(req)
	})
}

func (p *Platform) AssignPrivateIpAddress(nicId string, addresses []string, count int) (*zecApi.BatchAssignNetworkInterfaceIpv4Response, error) {
	return call(p, p.write, func(c ec2.EC2) (*zecApi.BatchAssignNetworkInterfaceIpv4Response, error) {
		return c.AssignPrivateIpAddress(nicId, addresses, count)
	})
}

func (p *Platform) UnAssignPrivateIpAddress(nicId string, addresses []string) (*zecApi.UnassignNetworkInterfaceIpv4Response, error) {
	return call(p, p.write, func(c ec2.EC2) (*zecApi.UnassignNetworkInterfaceIpv4Response, error) {
		return c.UnAssignPrivateIpAddress(nicId, addresses)
	})
}

func (p *Platform) AssignNetworkInterfaceIpv6(nicId string, billing ec2.IPv6Billing) (*zecApi.AssignNetworkInterfaceIpv6Response, error) {
	return call(p, p.write, func(c ec2.EC2) (*zecApi.AssignNetworkInterfaceIpv6Response, error) {
		return c.AssignNetworkInterfaceIpv6(nicId, billing)
	})
}

func (p *Platform) UnassignNetworkInterfaceIpv6(nicId string) (*zecApi.UnassignNetworkInterfaceIpv6Response, error) {
	return call(p, p.write, func(c ec2.EC2) (*zecApi.UnassignNetworkInterfaceIpv6Response, error) {
		return c.UnassignNetworkInterfaceIpv6(nicId)
	})
}

func (p *Platform) CreateNetworkInterface(name, subnetId, securityGroupId string, tags map[string]string) (*zecApi.CreateNetworkInterfaceResponse, error) {
	return call(p, p.write, func(c ec2.EC2) (*zecApi.CreateNetworkInterfaceResponse, error) {
		return c.CreateNetworkInterface(name, subnetId, securityGroupId, tags)
	})
}

func (p *Platform) AttachNetworkInterface(nicId, instanceId string) (*zecApi.AttachNetworkInterfaceResponse, error) {
	return call(p, p.write, func(c ec2.EC2) (*zecApi.AttachNetworkInterfaceResponse, error) {
		return c.AttachNetworkInterface(nicId, instanceId)
	})
}

func (p *Platform) DetachNetworkInterface(nicId string) (*zecApi.DetachNetworkInterfaceResponse, error) {
	return call(p, p.write, func(c ec2.EC2) (*zecApi.DetachNetworkInterfaceResponse, error) {
		return c.DetachNetworkInterface(nicId)
	})
}

func (p *Platform) DeleteNetworkInterface(nicId string) (*zecApi.DeleteNetworkInterfaceResponse, error) {
	return call(p, p.write, func(c ec2.EC2) (*zecApi.DeleteNetworkInterfaceResponse, error) {
		return c.DeleteNetworkInterface(nicId)
	})
}
