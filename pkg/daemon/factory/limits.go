// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"fmt"
	"time"

	"golang.org/x/sync/singleflight"
	"k8s.io/apimachinery/pkg/util/cache"
)

// Instance networking limits reference:
//
//	https://docs.console.zenlayer.com/welcome/elastic-compute/instance-specification
const (
	// limitCacheTTL bounds how long a resolved limit is reused. It exists for
	// the API-backed implementation this is meant to become -- quotas can be
	// raised for an account without the node restarting. The table lookup below
	// is deterministic, so today the TTL only costs a periodic re-resolve.
	limitCacheTTL = 30 * time.Minute
	// limitCacheSize is generous: a node sees exactly one instance type, and
	// the extra room is for a control-plane style caller resolving many.
	limitCacheSize = 64
)

// LimitProvider resolves the networking limits of an instance type.
//
// The interface has the shape of a remote lookup -- it takes a context, may
// block, and may fail -- even though the current implementation answers from a
// table. The OpenAPI does not expose these two quotas yet:
// DescribeZoneInstanceConfigInfos returns CPU, memory, bandwidth and PPS, but
// neither the vNIC count nor the per-vNIC IP count. When an endpoint does
// appear, only resolveFn has to change; callers, caching and the singleflight
// collapsing around it stay as they are.
type LimitProvider interface {
	// GetLimit resolves the limits of an instance type. The table keys on vcpu;
	// instanceType only reaches the cache key and the error message today, and
	// is what an API lookup would key on instead.
	GetLimit(ctx context.Context, instanceType string, vcpu int) (InstanceLimitsAttr, error)
}

// resolveFn is the swap point between the table and a future API call.
type resolveFn func(ctx context.Context, instanceType string, vcpu int) (InstanceLimitsAttr, error)

type limitProvider struct {
	cache   *cache.LRUExpireCache
	sf      singleflight.Group
	ttl     time.Duration
	resolve resolveFn
}

// NewLimitProvider returns the provider backed by the published specification.
func NewLimitProvider() LimitProvider {
	return newLimitProvider(resolveFromSpecTable, limitCacheTTL)
}

func newLimitProvider(resolve resolveFn, ttl time.Duration) *limitProvider {
	return &limitProvider{
		cache:   cache.NewLRUExpireCache(limitCacheSize),
		ttl:     ttl,
		resolve: resolve,
	}
}

func (p *limitProvider) GetLimit(ctx context.Context, instanceType string, vcpu int) (InstanceLimitsAttr, error) {
	key := fmt.Sprintf("%s/%d", instanceType, vcpu)

	// A cache entry of the wrong type is treated as a miss rather than a panic:
	// this sits on the allocation path, and falling through to resolve gives the
	// right answer at the cost of one lookup.
	if v, ok := p.cache.Get(key); ok {
		if limits, typed := v.(InstanceLimitsAttr); typed {
			return limits, nil
		}
	}

	// singleflight so a burst of concurrent allocations produces one lookup
	// rather than one per caller. Matters once resolve talks to the network.
	v, err, _ := p.sf.Do(key, func() (any, error) {
		limits, resolveErr := p.resolve(ctx, instanceType, vcpu)
		if resolveErr != nil {
			return nil, resolveErr
		}
		p.cache.Add(key, limits, p.ttl)
		return limits, nil
	})
	if err != nil {
		return InstanceLimitsAttr{}, err
	}
	limits, typed := v.(InstanceLimitsAttr)
	if !typed {
		return InstanceLimitsAttr{}, fmt.Errorf("instance limit lookup for %s returned %T, want InstanceLimitsAttr", key, v)
	}
	return limits, nil
}

// The document's IP-per-vNIC figure counts the interface's own primary address,
// not just the secondaries: on a 15-IP tier an interface takes 14 secondaries
// and refuses the 15th with OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT.
// That is why poolCapacityLocked gives every interface perENI-1 addresses:
// each interface keeps a primary that occupies one of the slots counted here.
// On the instance's own interface that address is the node's; on a CNI-created
// one it is what the guest configures, so the interface has a source the fabric
// accepts.

// specLimit is one tier of the published networking table.
type specLimit struct {
	// minVCPU is the lower bound of the tier, inclusive. The document lists
	// tiers as ranges ("4-7"), so the lower bound is what identifies one.
	minVCPU     int
	maxVNICs    int
	ipv4PerVNIC int
}

// specTable is transcribed from the networking tables in the instance
// specification. Both published tables -- z4a/z4ah and z2a/z2i/z3a/z4i --
// carry identical numbers, so a single table keyed on vCPU covers every family
// that exists today. Should a family ever diverge, this becomes a
// map[family][]specLimit and resolveFromSpecTable picks by instanceType.
//
// Rows are ordered by minVCPU ascending; the document's "4-7" and "8" rows
// carry the same numbers and collapse into the single tier starting at 4.
// Where the document gives a range ("6-10" IP on the two smallest tiers) the
// lower bound is taken, and a vCPU count falling between two tiers resolves to
// the lower tier. Overstating a limit makes the daemon attach an ENI or claim
// an IP that the platform then refuses, which surfaces as a failed pod setup;
// understating it only leaves capacity unused.
var specTable = []specLimit{
	{minVCPU: 1, maxVNICs: 2, ipv4PerVNIC: 6},    // doc: 1 vCPU,    2 vNIC, 6-10 IP
	{minVCPU: 2, maxVNICs: 2, ipv4PerVNIC: 6},    // doc: 2 vCPU,    2 vNIC, 6-10 IP
	{minVCPU: 4, maxVNICs: 2, ipv4PerVNIC: 15},   // doc: 4-7 and 8, 2 vNIC, 15 IP
	{minVCPU: 16, maxVNICs: 4, ipv4PerVNIC: 30},  // doc: 16 vCPU,   4 vNIC, 30 IP
	{minVCPU: 32, maxVNICs: 8, ipv4PerVNIC: 30},  // doc: 32 vCPU,   8 vNIC, 30 IP
	{minVCPU: 64, maxVNICs: 16, ipv4PerVNIC: 30}, // doc: 64 vCPU,  16 vNIC, 30 IP
}

// ipv6CidrPerVNIC is not in the published table; one /96 block per interface is
// what the datapath assumes.
const ipv6CidrPerVNIC = 1

func resolveFromSpecTable(_ context.Context, instanceType string, vcpu int) (InstanceLimitsAttr, error) {
	if vcpu <= 0 {
		return InstanceLimitsAttr{}, fmt.Errorf("instance %q reported %d vCPU, cannot resolve networking limits",
			instanceType, vcpu)
	}

	// The highest tier this vCPU count reaches; anything beyond the last tier
	// stays on it.
	row := specTable[0]
	for _, r := range specTable {
		if vcpu < r.minVCPU {
			break
		}
		row = r
	}

	return InstanceLimitsAttr{
		ENIQuota:          row.maxVNICs,
		IPv4MaxPerENI:     row.ipv4PerVNIC,
		IPv6CidrMaxPerENI: ipv6CidrPerVNIC,
	}, nil
}
