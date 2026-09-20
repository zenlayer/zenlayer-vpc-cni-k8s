// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"fmt"
	"net"
	"sort"
	"sync"
	"time"

	"golang.org/x/time/rate"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/wait"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/backoff"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/tracing"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/ec2"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

const (
	defaultSubnetInsufficientThreshold = 0.2
	percentage                         = 100
)

var (
	log = zlog.GetLogger().WithFields(zlog.Fields{"subsys": "ipam"})
)

// PodSubnet is the cached description of one subnet pods are placed in: its
// CIDRs, its gateways, and how many addresses it has left.
//
// The free-address count is a cached figure refreshed on a timer, not a
// reservation, so it can only ever say a subnet looked full or looked usable a
// moment ago. Every method that reads it must release the lock before calling
// anything that takes the lock again -- sync.RWMutex is not reentrant, and a
// waiting writer turns a nested read into a deadlock.
type PodSubnet struct {
	RegionId       string `json:"regionId"`
	VpcId          string `json:"vpcId"`
	SubnetId       string `json:"subnetId"`
	IPv4Gateway    string `json:"IPv4Gateway,omitempty"`
	IPv6Gateway    string `json:"IPv6Gateway,omitempty"`
	IPv4Cidr       string `json:"ipv4Cidr,omitempty"`
	IPv6Cidr       string `json:"ipv6Cidr,omitempty"`
	TotalIPv4Count int    `json:"totalIPv4Count"`

	// status
	sync.RWMutex
	Disable                   bool      `json:"disable,omitempty"`
	AvailableIPv4AddressCount int       `json:"availableIPv4AddressCount"`
	LastUpdate                time.Time `json:"lastUpdate"`
}

func (s *PodSubnet) Enable() bool {
	s.RLock()
	defer s.RUnlock()
	return !s.Disable
}

// IPStack reports which families the subnet carries. A subnet with no CIDR at
// all reports IPv4, so callers that need "does it really have this family" must
// use Cidr instead.
func (s *PodSubnet) IPStack() types.IPStack {
	s.RLock()
	defer s.RUnlock()

	if s.IPv4Cidr != "" && s.IPv6Cidr != "" {
		return types.IPStackDual
	} else if s.IPv6Cidr != "" {
		return types.IPStackIPv6
	} else {
		return types.IPStackIPv4
	}
}

// Cidr is the subnet's CIDR for one family, empty when it carries none.
//
// UpdateTopology rewrites these fields while callers are running, so read them
// through here rather than off the struct. Going per family also lets a caller
// report which family is missing instead of only that the subnet is not dual.
func (s *PodSubnet) Cidr(f zecTypes.IPFamily) string {
	s.RLock()
	defer s.RUnlock()

	if f == zecTypes.IPFamilyV6 {
		return s.IPv6Cidr
	}
	return s.IPv4Cidr
}

func (s *PodSubnet) DisableSubnet() {
	s.Lock()
	defer s.Unlock()
	s.Disable = true
}

func (s *PodSubnet) EnableSubnet() {
	s.Lock()
	defer s.Unlock()
	s.Disable = false
}

func (s *PodSubnet) GetAvailableIPv4AddressCount() int {
	s.RLock()
	defer s.RUnlock()
	return s.AvailableIPv4AddressCount
}

func (s *PodSubnet) UpdateAvailableIpAddressCount(q int) {
	s.Lock()
	defer s.Unlock()
	s.AvailableIPv4AddressCount = q
}

// UpdateTopology refreshes the fields that describe the subnet itself, as
// opposed to how full it is.
//
// UpdateSubnetsStatus calls it on every poll, so an operator who resizes a
// subnet or gives an existing one an IPv6 CIDR is picked up without restarting
// the daemon. Only writing these when a subnet first enters the manager would
// leave the CIDR the daemon reports to the plugin stale for the life of the
// process.
//
// An empty argument is ignored: a reply that omits a field must not erase what
// is already known.
func (s *PodSubnet) UpdateTopology(ipv4Cidr, ipv4Gateway, ipv6Cidr, ipv6Gateway string, totalIPv4Count int) {
	s.Lock()
	defer s.Unlock()
	if ipv4Cidr != "" {
		s.IPv4Cidr = ipv4Cidr
	}
	if ipv4Gateway != "" {
		s.IPv4Gateway = ipv4Gateway
	}
	if ipv6Cidr != "" {
		s.IPv6Cidr = ipv6Cidr
	}
	if ipv6Gateway != "" {
		s.IPv6Gateway = ipv6Gateway
	}
	if totalIPv4Count > 0 {
		s.TotalIPv4Count = totalIPv4Count
	}
}

func (s *PodSubnet) Available(ipStack types.IPStack) bool {
	// Read the fields, then release, before calling anything that locks again:
	// IPStack() takes RLock itself, Go's RWMutex is not reentrant, and a
	// recursive read lock deadlocks once a writer is queued behind it.
	// UpdateSubnetsStatus is that writer -- it calls
	// UpdateAvailableIpAddressCount and UpdateTopology, which take s.Lock.
	//
	// IPv4 is the only counted family, in dual as well: AvailableIPv4AddressCount
	// is the only count this type keeps. Support() is what rules out a subnet
	// missing IPv6 entirely.
	s.RLock()
	enabled := !s.Disable
	v4Count := s.AvailableIPv4AddressCount
	s.RUnlock()

	if !enabled || !s.IPStack().Support(ipStack) {
		return false
	}
	if ipStack.EnableIPv4() {
		return v4Count > 0
	}
	return true
}

type SortablePodSubnets []*PodSubnet

func (s SortablePodSubnets) Len() int {
	return len(s)
}

func (s SortablePodSubnets) Less(i, j int) bool {
	return s[i].GetAvailableIPv4AddressCount() < s[j].GetAvailableIPv4AddressCount()
}

func (s SortablePodSubnets) Swap(i, j int) {
	s[i], s[j] = s[j], s[i]
}

type PodSubnetManagerConfig struct {
	eventRecord                 *tracing.Tracer
	EventLimiter                *rate.Limiter
	SubnetInsufficientThreshold float64
	// IPStack is the node's configured stack. The manager reads it only to
	// decide whether a subnet without an IPv4 CIDR is worth reporting: in a
	// mode that serves IPv4 it means the subnet cannot take pods, and in one
	// that does not it is unremarkable. Which families a subnet must carry is
	// still decided at startup, by validateSubnetsForStack.
	IPStack types.IPStack
}

func DefaultPodSubnetManagerConfig() *PodSubnetManagerConfig {
	return &PodSubnetManagerConfig{
		SubnetInsufficientThreshold: defaultSubnetInsufficientThreshold,
		IPStack:                     types.IPStackIPv4,
	}
}

type PodSubnetManagerOption func(config *PodSubnetManagerConfig)

func WithEventRecord(eventRecord *tracing.Tracer) PodSubnetManagerOption {
	return func(config *PodSubnetManagerConfig) {
		config.eventRecord = eventRecord
	}
}

func WithDefaultEventLimiter() PodSubnetManagerOption {
	return func(config *PodSubnetManagerConfig) {
		config.EventLimiter = rate.NewLimiter(2, 5)
	}
}

func WithEventLimiter(limiter *rate.Limiter) PodSubnetManagerOption {
	return func(config *PodSubnetManagerConfig) {
		config.EventLimiter = limiter
	}
}

func WithSubnetInsufficientThreshold(threshold float64) PodSubnetManagerOption {
	return func(config *PodSubnetManagerConfig) {
		config.SubnetInsufficientThreshold = threshold
	}
}

// WithIPStack sets the stack the manager assumes. An empty value means the
// configuration left ipStack unset, which the daemon reads as IPv4; normalising
// it here keeps callers free to pass the raw config string, since
// IPStack.EnableIPv4 answers false for "" and would quietly turn the IPv4
// reporting off.
func WithIPStack(ipStack types.IPStack) PodSubnetManagerOption {
	return func(config *PodSubnetManagerConfig) {
		if ipStack == "" {
			ipStack = types.IPStackIPv4
		}
		config.IPStack = ipStack
	}
}

// SubnetManager owns the cache of subnets this node may place pods in.
//
// It exists so that the allocation path never has to ask the platform where a
// subnet's gateway is or whether it still has room: those answers are refreshed
// in the background, because fetching them while a pod waits would put a remote
// round trip inside container startup.
type SubnetManager interface {
	// FlushSubnets loads the named subnets, unconditionally, and reports any
	// that did not survive the filter -- one belonging to another VPC or region
	// is dropped silently everywhere else. It runs during daemon construction so
	// that a mistyped subnet id is attributable at startup, rather than
	// surfacing later as an unexplained "no available subnet".
	FlushSubnets(subnetIds ...string) error

	// GetPodSubnet returns the cached subnet, fetching it from the platform if
	// the cache has no entry. It answers "does this subnet exist", which is not
	// the same question as IsPodSubnet's.
	GetPodSubnet(subnetId string) *PodSubnet

	// IsPodSubnet reports whether subnetId is one of the subnets configured for
	// pod addresses. Unlike GetPodSubnet it never falls back to the platform,
	// so it answers "is this configured", not "does this exist".
	IsPodSubnet(subnetId string) bool

	// GetUpdatedPodSubnet fetches one subnet from the platform and returns the
	// refreshed entry, bypassing the cache. It is a remote round trip with
	// retries, so it does not belong on the allocation path.
	GetUpdatedPodSubnet(subnetId string) (subnet *PodSubnet, err error)

	// SelectSubnet picks where the next interface should go: among the subnets
	// that can serve ipFamily and are not disabled, the one reporting the most
	// free IPv4 addresses.
	//
	// The choice is made on cached counts, so it is a preference and not a
	// reservation -- two concurrent selections can pick the same subnet and one
	// of them can still be refused by the platform.
	SelectSubnet(ipFamily types.IPStack, options ...UpdateSubnetsStatusOption) *PodSubnet

	// UpdateSubnetsStatus refreshes every cached subnet from the platform. With
	// the aging option it skips entries younger than the given limit, which is
	// what keeps a periodic refresh from turning into one API call per tick per
	// subnet.
	//
	// It takes the manager's write lock, so it is the writer that any nested
	// read lock elsewhere would deadlock against.
	UpdateSubnetsStatus(options ...UpdateSubnetsStatusOption) error

	// DisableSubnet takes a subnet out of consideration for new interfaces. It
	// is how the daemon stops retrying a subnet the platform keeps refusing;
	// existing addresses in it are unaffected.
	DisableSubnet(subnetId string)

	// Status reports the cached view of every subnet, for the daemon's debug
	// endpoint. It is a snapshot, not a live query.
	Status() *Status
}

type subnetManager struct {
	vpcId    string
	regionId string
	lock     sync.RWMutex
	// podSubnets record available subnets configured by user
	podSubnets map[string]*PodSubnet
	// legacyPodSubnet record subnets used by attached eni but not exist in podSubnets
	legacyPodSubnet map[string]*PodSubnet

	apiClient ec2.EC2
	config    *PodSubnetManagerConfig
}

// Status of SubnetManager.
type Status struct {
	RegionId         string                `json:"regionId"`
	VpcId            string                `json:"vpcId"`
	PodSubnets       map[string]*PodSubnet `json:"podSubnets,omitempty"`
	LegacyPodSubnets map[string]*PodSubnet `json:"legacyPodSubnets,omitempty"`
}

func (m *subnetManager) Status() *Status {
	m.lock.RLock()
	defer m.lock.RUnlock()

	podSubnets := make(map[string]*PodSubnet)
	legacyPodSubnet := make(map[string]*PodSubnet)
	for _, subnet := range m.podSubnets {
		podSubnets[subnet.SubnetId] = subnet
	}
	for _, subnet := range m.legacyPodSubnet {
		legacyPodSubnet[subnet.SubnetId] = subnet
	}
	return &Status{
		RegionId:         m.regionId,
		VpcId:            m.vpcId,
		PodSubnets:       podSubnets,
		LegacyPodSubnets: legacyPodSubnet,
	}
}

// RecordSubnetEvent record events of SubnetManager.
func (m *subnetManager) RecordSubnetEvent(eventType, reason, message string) {
	if m.config.eventRecord == nil {
		return
	}

	if m.config.EventLimiter == nil || m.config.EventLimiter.Allow() {
		_ = m.config.eventRecord.RecordNodeEvent(eventType, reason, message)
	}
}

func (m *subnetManager) updateSubnets(flush bool, subnetIds ...string) error {
	var addSubnets []string
	currentPodSubnetMap := make(map[string]struct{})
	for _, sb := range subnetIds {
		currentPodSubnetMap[sb] = struct{}{}
		if _, exist := m.podSubnets[sb]; !exist {
			addSubnets = append(addSubnets, sb)
		}
	}

	if flush {
		for id := range m.podSubnets {
			if _, exist := currentPodSubnetMap[id]; !exist {
				delete(m.podSubnets, id)
			}
		}
	}

	if len(addSubnets) == 0 {
		return nil
	}

	var (
		resp *zecApi.DescribeSubnetsResponse
		err  error
	)
	werr := wait.ExponentialBackoff(backoff.BackOff(backoff.APIFastRetry), func() (bool, error) {
		resp, err = m.apiClient.DescribeSubnets(addSubnets)
		return err == nil, nil
	})
	if err = apiErr.BackoffErrWrapper(werr, err); err != nil {
		log.Errorf("DescribeSubnets %s failed: %s", addSubnets, err.Error())
		return err
	}

	if resp.Response == nil || len(resp.Response.DataSet) == 0 {
		err := fmt.Errorf("get no result while describe subnets %v in vpc %s, check if they are matched", addSubnets, m.vpcId)
		log.Error(err)
		return err
	}

	for _, subnet := range resp.Response.DataSet {
		totalIPv4Cnt, availableIPv4AddressCount, ok := m.inspectSubnet(subnet)
		if !ok {
			continue
		}
		m.podSubnets[zecCom.ToString(subnet.SubnetId)] = &PodSubnet{
			VpcId:                     zecCom.ToString(subnet.VpcId),
			RegionId:                  zecCom.ToString(subnet.RegionId),
			SubnetId:                  zecCom.ToString(subnet.SubnetId),
			IPv4Gateway:               zecCom.ToString(subnet.GatewayIpAddress),
			IPv6Gateway:               zecCom.ToString(subnet.Ipv6GatewayIpAddress),
			IPv4Cidr:                  zecCom.ToString(subnet.CidrBlock),
			IPv6Cidr:                  zecCom.ToString(subnet.Ipv6CidrBlock),
			TotalIPv4Count:            totalIPv4Cnt,
			AvailableIPv4AddressCount: availableIPv4AddressCount,
			LastUpdate:                time.Now(),
		}
	}
	return nil
}

func (m *subnetManager) FlushSubnets(subnetIds ...string) error {
	subnetIds = removeDuplicateElement(subnetIds)
	m.lock.Lock()
	defer m.lock.Unlock()
	err := m.updateSubnets(true, subnetIds...)
	if err != nil {
		return err
	}

	// Every configured subnet must have survived the filter above, which drops
	// subnets belonging to another VPC or region and returns nil all the same.
	// This is the only caller that turns that into an error, and it runs during
	// daemon construction, so a mistyped subnet is attributable at startup
	// rather than showing up later as "No available subnet".
	var missing []string
	for _, id := range subnetIds {
		if _, ok := m.podSubnets[id]; !ok {
			missing = append(missing, id)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("subnet %v unusable in vpc %s region %s -- they must exist, belong to that vpc and be in that region; see the preceding log lines for what each one actually belongs to",
			missing, m.vpcId, m.regionId)
	}

	log.Infof("Flush subnets list: %v", subnetIds)
	return nil
}

func (m *subnetManager) UpdateSubnets(subnetIds ...string) error {
	subnetIds = removeDuplicateElement(subnetIds)
	m.lock.Lock()
	defer m.lock.Unlock()
	return m.updateSubnets(false, subnetIds...)
}

// UpdateSubnetsStatusConfig config used while update status of PodSubnets.
type UpdateSubnetsStatusConfig struct {
	Aging time.Duration
}

// UpdateSubnetsStatusOption option used while update status of PodSubnets.
type UpdateSubnetsStatusOption func(config *UpdateSubnetsStatusConfig)

// WithAging sets how stale a subnet has to be before UpdateSubnetsStatus
// refreshes it.
func WithAging(aging time.Duration) UpdateSubnetsStatusOption {
	return func(config *UpdateSubnetsStatusConfig) {
		config.Aging = aging
	}
}

// WithUnlimitedAging sets the aging window to 24 hours, so any subnet refreshed
// within the last day is left alone.
func WithUnlimitedAging() UpdateSubnetsStatusOption {
	return func(config *UpdateSubnetsStatusConfig) {
		config.Aging = time.Hour * 24
	}
}

func (m *subnetManager) UpdateSubnetsStatus(options ...UpdateSubnetsStatusOption) error {
	conf := &UpdateSubnetsStatusConfig{}
	for _, option := range options {
		option(conf)
	}

	m.lock.Lock()
	defer m.lock.Unlock()

	var subnetIds []string
	for _, subnet := range m.podSubnets {
		if time.Since(subnet.LastUpdate) > conf.Aging {
			subnetIds = append(subnetIds, subnet.SubnetId)
		}
	}
	configuredCnt := len(subnetIds)
	// The legacy entries need refreshing too, and they are the ones that needed
	// it most: legacyPodSubnet holds exactly the subnets an attached interface
	// uses that podSubnets does not, nothing validated them and nothing else
	// updates them, so one first seen full stayed full for the life of the
	// daemon -- and subnetMonitor only clears forbidSubnet while Available() is
	// true, which reads that count.
	for _, subnet := range m.legacyPodSubnet {
		if time.Since(subnet.LastUpdate) > conf.Aging {
			subnetIds = append(subnetIds, subnet.SubnetId)
		}
	}
	if len(subnetIds) == 0 {
		return nil
	}

	var (
		resp *zecApi.DescribeSubnetsResponse
		err  error
	)
	werr := wait.ExponentialBackoff(backoff.BackOff(backoff.APIFastRetry), func() (bool, error) {
		resp, err = m.apiClient.DescribeSubnets(subnetIds)
		return err == nil, nil
	})

	if err = apiErr.BackoffErrWrapper(werr, err); err != nil {
		log.Errorf("DescribeSubnets %s failed: %s", subnetIds, err.Error())
		return err
	}

	// An empty answer is only a problem when a configured subnet was asked
	// about: those have to exist, so none of them coming back is a
	// misconfiguration to report -- never a license to purge the cache, because
	// dropping a subnet makes every interface in it unmanaged and takes the
	// node's capacity to zero. A batch of nothing but legacy entries is the
	// opposite: nothing else ever removes one, and an empty answer is how the
	// platform says they are gone.
	if resp.Response == nil || (len(resp.Response.DataSet) == 0 && configuredCnt > 0) {
		err := fmt.Errorf("get no result while describe subnets %v in vpc %s, check if they are matched", subnetIds, m.vpcId)
		log.Error(err)
		return err
	}

	effectiveSubnets := make(map[string]struct{})
	for _, subnet := range resp.Response.DataSet {
		totalIPv4Cnt, availableIPv4AddressCount, ok := m.inspectSubnet(subnet)
		id := zecCom.ToString(subnet.SubnetId)
		effectiveSubnets[id] = struct{}{}
		cidr := zecCom.ToString(subnet.CidrBlock)
		gateway := zecCom.ToString(subnet.GatewayIpAddress)
		v6Cidr := zecCom.ToString(subnet.Ipv6CidrBlock)
		v6Gateway := zecCom.ToString(subnet.Ipv6GatewayIpAddress)
		for _, sbs := range []map[string]*PodSubnet{m.podSubnets, m.legacyPodSubnet} {
			if sb, exist := sbs[id]; exist {
				if !ok {
					// Unusable, but the platform did answer for it, so it has not
					// gone away. Zeroing the count is what stops it being selected,
					// since that is what Available() reads. The entry itself stays:
					// dropping a configured one makes every interface in it
					// unmanaged and takes the node's capacity to zero, and dropping
					// a legacy one leaves the interfaces already carrying pods with
					// nowhere to read their subnet from. Nothing is copied over --
					// the reply may describe another vpc, or carry a cidr that will
					// not parse.
					sb.UpdateAvailableIpAddressCount(0)
					sb.LastUpdate = time.Now()
					continue
				}
				sb.UpdateAvailableIpAddressCount(availableIPv4AddressCount)
				sb.UpdateTopology(cidr, gateway, v6Cidr, v6Gateway, totalIPv4Cnt)
				sb.LastUpdate = time.Now()
			}
		}
	}

	if len(subnetIds) == len(effectiveSubnets) {
		return nil
	}

	// Only the ids the platform did not answer for at all are gone. Measured on
	// this platform: DescribeSubnets never rejects a batch over an unknown id,
	// it leaves that id out of the answer.
	for _, id := range subnetIds {
		if _, exist := effectiveSubnets[id]; !exist {
			delete(m.podSubnets, id)
			delete(m.legacyPodSubnet, id)
		}
	}
	return nil
}

func (m *subnetManager) GetUpdatedPodSubnet(subnetId string) (subnet *PodSubnet, err error) {
	var (
		resp *zecApi.DescribeSubnetsResponse
	)
	// No special case for a subnet that does not exist: measured on this
	// platform, DescribeSubnets answers an unknown id with an empty list rather
	// than an error, so the empty-answer check below is what catches it.
	werr := wait.ExponentialBackoff(backoff.BackOff(backoff.APIFastRetry), func() (bool, error) {
		resp, err = m.apiClient.DescribeSubnets([]string{subnetId})
		return err == nil, nil
	})

	if err = apiErr.BackoffErrWrapper(werr, err); err != nil {
		log.Errorf("DescribeSubnets %s failed: %s", subnetId, err.Error())
		return
	}

	if resp.Response == nil || len(resp.Response.DataSet) == 0 {
		err = fmt.Errorf("get no result while describe subnet %v in vpc %s, check if they are matched", subnetId, m.vpcId)
		log.Error(err)
		return
	}
	subnetResp := resp.Response.DataSet[0]
	// inspectSubnet, not a third copy of the same arithmetic. This function used
	// to compute the counts inline and had drifted from the other two callers in
	// three ways, which is what the comment on inspectSubnet warns about: it did
	// not check the vpc and region at all, so a subnet belonging to someone else
	// was cached on demand; it accepted a subnet with no IPv4 CIDR that the
	// refresh had just refused, filing it under legacyPodSubnet where the next
	// refresh deleted it again; and it divided by a zero total on such a subnet,
	// so the NaN silenced the threshold event.
	//
	// Nothing is cached when the checks refuse. GetPodSubnet, the only caller,
	// answers nil for the subnet -- which is the honest answer for one this node
	// cannot place a pod in, and the same answer the refresh path gives.
	totalIPv4Cnt, availableIPv4AddressCount, ok := m.inspectSubnet(subnetResp)
	if !ok {
		err = fmt.Errorf("subnet %s cannot serve pods on this node; the preceding log line and the "+
			"node event raised for it say why", subnetId)
		return
	}

	// Both lookups under m.lock. Reading these maps unlocked is not a stale
	// read, it is `fatal error: concurrent map read and map write`: the monitor
	// tick deletes from both in UpdateSubnetsStatus, and a pod ADD reaches here
	// on a cache miss holding nothing.
	//
	// Taken once for the whole tail, rather than released and re-taken before
	// the insert below -- dropping it in between would let two ADDs for the same
	// unknown subnet both miss and both insert.
	m.lock.Lock()
	defer m.lock.Unlock()

	if sb, exist := m.podSubnets[subnetId]; exist {
		sb.UpdateAvailableIpAddressCount(availableIPv4AddressCount)
		subnet = sb
		return
	}

	if sb, exist := m.legacyPodSubnet[subnetId]; exist {
		sb.UpdateAvailableIpAddressCount(availableIPv4AddressCount)
		subnet = sb
		return
	}

	// add to legacy

	subnet = &PodSubnet{
		RegionId:                  zecCom.ToString(subnetResp.RegionId),
		VpcId:                     zecCom.ToString(subnetResp.VpcId),
		SubnetId:                  subnetId,
		IPv4Gateway:               zecCom.ToString(subnetResp.GatewayIpAddress),
		IPv6Gateway:               zecCom.ToString(subnetResp.Ipv6GatewayIpAddress),
		IPv4Cidr:                  zecCom.ToString(subnetResp.CidrBlock),
		IPv6Cidr:                  zecCom.ToString(subnetResp.Ipv6CidrBlock),
		TotalIPv4Count:            totalIPv4Cnt,
		AvailableIPv4AddressCount: availableIPv4AddressCount,
		LastUpdate:                time.Now(),
	}
	m.legacyPodSubnet[subnetId] = subnet
	return
}

func (m *subnetManager) IsPodSubnet(subnetId string) bool {
	if subnetId == "" {
		return false
	}
	m.lock.RLock()
	defer m.lock.RUnlock()
	_, exist := m.podSubnets[subnetId]
	return exist
}

func (m *subnetManager) GetPodSubnet(subnetId string) *PodSubnet {
	exec := func() *PodSubnet {
		m.lock.RLock()
		defer m.lock.RUnlock()
		if subnet, exist := m.podSubnets[subnetId]; exist {
			return subnet
		}
		if subnet, exist := m.legacyPodSubnet[subnetId]; exist {
			return subnet
		}
		return nil
	}
	if subnet := exec(); subnet != nil {
		return subnet
	}

	if subnet, err := m.GetUpdatedPodSubnet(subnetId); err == nil {
		return subnet
	}
	return nil
}

func (m *subnetManager) SelectSubnet(ipFamily types.IPStack, options ...UpdateSubnetsStatusOption) *PodSubnet {
	err := m.UpdateSubnetsStatus(options...)
	if err != nil {
		log.Errorf("UpdateSubnetsStatus failed, %s", err.Error())
		return nil
	}

	m.lock.RLock()
	defer m.lock.RUnlock()

	subnets := SortablePodSubnets{}
	for _, subnet := range m.podSubnets {
		if subnet.Available(ipFamily) {
			subnets = append(subnets, subnet)
		}
	}

	if subnets.Len() == 0 {
		m.RecordSubnetEvent(v1.EventTypeWarning, tracing.EventNoAvailableSubnet,
			fmt.Sprintf("No available subnet in %s", m.regionId))
		return nil
	}
	sort.Sort(sort.Reverse(subnets))
	return subnets[0]
}

// DisableSubnet holds m.lock for the map lookup only: UpdateSubnetsStatus
// deletes from m.podSubnets, and reading it unlocked is a runtime throw, not a
// stale read. The PodSubnet the map yields has its own lock. EnableSubnet
// below is the same shape.
func (m *subnetManager) DisableSubnet(subnetId string) {
	m.lock.RLock()
	subnet, exist := m.podSubnets[subnetId]
	m.lock.RUnlock()
	if exist {
		subnet.DisableSubnet()
	}
}

func (m *subnetManager) EnableSubnet(subnetId string) {
	m.lock.RLock()
	subnet, exist := m.podSubnets[subnetId]
	m.lock.RUnlock()
	if exist {
		subnet.EnableSubnet()
	}
}

func NewPodSubnetManager(regionId, vpcId string, client ec2.EC2, options ...PodSubnetManagerOption) (SubnetManager, error) {
	if regionId == "" {
		return nil, fmt.Errorf("invalid regionId %q", regionId)
	}

	if vpcId == "" {
		return nil, fmt.Errorf("invalid vpcId %q", vpcId)
	}
	if client == nil {
		return nil, fmt.Errorf("client of subnet open api is nil")
	}
	m := &subnetManager{
		regionId:        regionId,
		vpcId:           vpcId,
		podSubnets:      make(map[string]*PodSubnet),
		legacyPodSubnet: make(map[string]*PodSubnet),
		apiClient:       client,
		config:          DefaultPodSubnetManagerConfig(),
	}

	for _, option := range options {
		option(m.config)
	}
	return m, nil
}

func removeDuplicateElement(origin []string) []string {
	result := make([]string, 0, len(origin))
	temp := map[string]struct{}{}
	for _, item := range origin {
		if _, ok := temp[item]; !ok {
			temp[item] = struct{}{}
			result = append(result, item)
		}
	}
	return result
}

// inspectSubnet validates one subnet from a DescribeSubnets response and derives
// its IPv4 counts, recording a node event for whatever it finds wrong. ok is
// false when the subnet must be skipped entirely.
//
// It is a method rather than inline code because both updateSubnets and
// UpdateSubnetsStatus need exactly these checks before they diverge over what to
// do with the result -- one creates cache entries, the other refreshes them.
// Duplicating the checks meant duplicating the reasoning above each of them, and
// two copies of a rationale drift apart without anything failing.
func (m *subnetManager) inspectSubnet(subnet *zecApi.SubnetInfo) (totalIPv4Cnt, availableIPv4AddressCount int, ok bool) {
	// Log why instead of dropping it silently. A subnet that exists but
	// belongs to another VPC or region otherwise surfaces much later as the
	// "No available subnet" event SelectSubnet records, which names neither
	// the subnet that was dropped nor the reason.
	if zecCom.ToString(subnet.VpcId) != m.vpcId || zecCom.ToString(subnet.RegionId) != m.regionId {
		log.Errorf("Ignoring subnet %s: it belongs to vpc %s region %s, but this daemon is configured for vpc %s region %s",
			zecCom.ToString(subnet.SubnetId), zecCom.ToString(subnet.VpcId), zecCom.ToString(subnet.RegionId),
			m.vpcId, m.regionId)
		return 0, 0, false
	}
	// A subnet with no IPv4 CIDR is refused outright while IPv4 is on: it
	// cannot take a pod, so it never enters the cache and the refresh reports
	// it. Refusing here matches the two branches around it -- neither a subnet
	// belonging to another VPC nor one whose CIDR will not parse is kept
	// either -- and it keeps the pool from ever being offered one.
	//
	// Falling through would be worse than keeping a useless entry: the ratio
	// below divides by totalIPv4Cnt, 0 makes it NaN, NaN compares false against
	// any threshold, and the subnet would look healthy for ever while every
	// IPv4 allocation against it failed.
	//
	// Worth knowing at startup: FlushSubnets turns this into "subnet ...
	// unusable in vpc ... region ...", and it runs before
	// validateSubnetsForStack, whose message names the missing family and both
	// CIDRs. So the operator sees the blunter of the two first.
	//
	// A mode that does not serve IPv4 keeps the subnet: the entry is what
	// carries its IPv6 CIDR and gateway, and there is nothing wrong with it.
	if cidrStr := zecCom.ToString(subnet.CidrBlock); cidrStr == "" {
		if m.config.IPStack.EnableIPv4() {
			msg := fmt.Sprintf("subnet %s reports no IPv4 cidr, so it cannot take pods on an %s node",
				zecCom.ToString(subnet.SubnetId), m.config.IPStack)
			log.Error(msg)
			m.RecordSubnetEvent(v1.EventTypeWarning, tracing.EventSubnetCidrBlockInvalid, msg)
			return 0, 0, false
		}
	} else if _, cidrNet, cidrErr := net.ParseCIDR(cidrStr); cidrErr != nil {
		msg := fmt.Sprintf("the CidrBlock is invalid in subnet %s", zecCom.ToString(subnet.SubnetId))
		log.Error(msg)
		m.RecordSubnetEvent(v1.EventTypeWarning, tracing.EventSubnetCidrBlockInvalid, msg)
		return 0, 0, false
	} else {
		ones, size := cidrNet.Mask.Size()
		totalIPv4Cnt = int(1<<(size-ones)) - 3 // the platform reserves three addresses in every subnet
		availableIPv4AddressCount = totalIPv4Cnt - zecCom.ToInteger(subnet.UsageIpv4Count)
		if totalIPv4Cnt > 0 {
			curPr := float64(availableIPv4AddressCount) / float64(totalIPv4Cnt)
			if curPr < m.config.SubnetInsufficientThreshold {
				msg := fmt.Sprintf("Current remaining available ip in subnet %s is %.2f%%, less than %.2f%%",
					zecCom.ToString(subnet.SubnetId), curPr*percentage, m.config.SubnetInsufficientThreshold*percentage)
				log.Error(msg)
				m.RecordSubnetEvent(v1.EventTypeWarning, tracing.EventSubnetAvailableIPBelowThreshold, msg)
			}
		} else {
			// A /31 or /32 leaves nothing after the three reserved addresses,
			// which makes the ratio meaningless rather than merely small.
			msg := fmt.Sprintf("subnet %s cidr %s has %d usable addresses after the three the platform reserves",
				zecCom.ToString(subnet.SubnetId), cidrStr, totalIPv4Cnt)
			log.Error(msg)
			m.RecordSubnetEvent(v1.EventTypeWarning, tracing.EventSubnetCidrBlockInvalid, msg)
		}
	}
	return totalIPv4Cnt, availableIPv4AddressCount, true
}
