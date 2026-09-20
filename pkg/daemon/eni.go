// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package daemon

import (
	"fmt"
	"math"
	"time"

	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/config"
	helper "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/factory"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/k8s"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/metrics"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/pool"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"
)

const (
	subnetAging = 10 * time.Second

	// instanceLimitPeriod is how often the cached instance limit is refreshed.
	//
	// Deliberately not tied to reconcileIntervalSec: reconciling with the
	// platform and probing how many interfaces this instance may hold are
	// different jobs on different timescales, and sharing the 20 minute
	// reconcile period made the limit stale for up to that long in both
	// directions -- after an operator attached an interface the daemon kept
	// trying to create one it could not attach, and after one was detached it
	// under-reported capacity and refused pods it could have served. The
	// detach direction has no error code to react to, so a short poll is the
	// only signal available. Not a knob: nothing an operator tunes here would
	// improve on it.
	instanceLimitPeriod = 2 * time.Minute
)

// eniResourceManager serves whole ENIs from a warm pool. It backs the
// exclusive-ENI mode; see newEniResourceManager below.
type eniResourceManager struct {
	pool pool.ResourcePool
}

func (e *eniResourceManager) Name() string {
	return "eni resource manager"
}

func (e *eniResourceManager) Allocate(ctx *netContext, prefer string) (zecTypes.NetResource, error) {
	res, err := e.pool.Allocate(ctx, prefer, zecTypes.PodKey(ctx.pod.Namespace, ctx.pod.Name))
	if err != nil {
		ctx.Log().Errorf("Allocate failed, %v", err)
		return nil, err
	}
	ctx.Log().Infof("Allocate succeed: %v", res)
	return res, nil
}

func (e *eniResourceManager) Release(ctx *netContext, resource *zecTypes.ResourceItem) error {
	err := e.pool.Release(resource.ID)
	if err != nil {
		ctx.Log().Errorf("Release %v failed, %v", resource, err)
		return err
	}
	ctx.Log().Infof("Release %v succeed", resource)
	return nil
}

// InuseByOwner returns what the pool still attributes to owner. It is the only
// way to find a pod's resources once its stored record is gone.
func (e *eniResourceManager) InuseByOwner(owner string) []zecTypes.ResourceItem {
	var items []zecTypes.ResourceItem
	for _, res := range e.pool.InuseByOwner(owner) {
		items = append(items, res.GetResourceItem())
	}
	return items
}

func (e *eniResourceManager) GetSnapshot() (pool.ResourcePoolSnapshot, error) {
	return e.pool.GetSnapshot()
}

func (e *eniResourceManager) GetPoolStatus() pool.Status {
	return e.pool.Status()
}

func (e *eniResourceManager) GetResourceLimit() int {
	return e.pool.GetResourceLimit()
}

func generateENIPoolCfg(cfg *config.Config, limits helper.InstanceLimits) pool.Config {
	// The instance quota is the only real ceiling on how many interfaces the
	// CNI can attach. There used to be a maxENI knob here, but nothing enforced
	// it -- the factory reports ENIAvailable() as its limit and always did --
	// so it promised a limit it could not keep.
	totalMax := limits.ENIAvailable()

	target := min(int(cfg.PoolTarget), int(math.Floor(cfg.PoolCapRatio*float64(totalMax))))
	targetMin := min(int(cfg.PoolTargetMin), int(math.Floor(cfg.PoolCapRatio*float64(totalMax))))

	return pool.Config{
		Name:            "eni",
		Type:            zecTypes.NetResourceTypeEni,
		TargetMin:       targetMin,
		Target:          target,
		MaxCap:          totalMax,
		MaxCapProbe:     cfg.PoolTargetCapProbe,
		MonitorInterval: time.Duration(cfg.PoolMonitorIntervalSec) * time.Second,
		GCProtectPeriod: time.Duration(cfg.PoolGCPeriodSec) * time.Second,
	}
}

// Compile-time guarantee that the reserved exclusive-ENI implementation still
// satisfies ResourceManager, and a reference that keeps the unused-code
// checkers quiet while nothing calls it yet. See newEniResourceManager below.
var (
	_ ResourceManager = (*eniResourceManager)(nil)
	_                 = newEniResourceManager
	_                 = generateENIPoolCfg
)

// newEniResourceManager builds the resource manager for the exclusive-ENI mode,
// where a pod gets a whole ENI (zecTypes.NetResourceTypeEni) instead of a
// secondary IP on a shared one.
//
// It is deliberately kept although nothing calls it yet: config.Config only
// accepts types.ModeENIMultiIP today, so NewIpamDaemon has no branch that
// reaches this path. Wire it up from that switch when the exclusive-ENI mode
// is enabled; do not delete it as dead code.
//
//nolint:unused // reserved for the exclusive-ENI network mode
func newEniResourceManager(cfg *config.Config, subnet helper.SubnetManager, secManager helper.SecurityGroupManager, zecApi helper.ZecAPI, allocatedResource map[string]zecTypes.NetResourceAllocated, k8s k8s.K8sService) (*eniResourceManager, error) {
	log.Infof("Creating EniResourceManager")
	m := &eniResourceManager{}
	limit, err := helper.NewInstanceLimitManager(zecApi)
	if err != nil {
		return nil, err
	}
	poolConfig := generateENIPoolCfg(cfg, limit.GetLimit())
	created, err := zecApi.GetAttachedENIs()
	if err != nil {
		return nil, fmt.Errorf("get attached enis failed while init, %v", err)
	}

	factory, err := newEniFactory(secManager, subnet, zecApi, limit, types.IPStack(cfg.IPStack))
	if err != nil {
		return nil, fmt.Errorf("create eni factory failed, %v", err)
	}
	poolConfig.Factory = factory

	poolConfig.PreStart = func(pool pool.ResourcePoolOp) error {
		for _, e := range created {
			if item, exist := allocatedResource[e.GetID()]; exist {
				pool.AddInuse(e, item.Owner)
			} else {
				pool.AddAvailable(e)
			}
		}
		return nil
	}
	factory.monitor(time.Duration(cfg.SubnetStatUpdateIntervalSec)*time.Second, instanceLimitPeriod)

	p, err := pool.NewResourcePool(poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create resource pool %s failed, %v", poolConfig.Name, err)
	}
	m.pool = p

	return m, nil
}

// eniFactory creates and releases whole ENIs.
type eniFactory struct {
	ipStack    types.IPStack
	secManager helper.SecurityGroupManager
	subnets    helper.SubnetManager
	zecApi     helper.ZecAPI
	limit      helper.InstanceLimitManager
}

func (f *eniFactory) Name() string {
	return zecTypes.NetResourceTypeEni
}

// Create creates count interfaces, each with a single address.
//
// A partial result is a success: the caller asked for warm capacity, not for an
// exact number, so returning what was created beats failing the whole request
// and leaving the interfaces already attached to be reclaimed by reconcile. The
// last error is only reported when nothing at all was created.
func (f *eniFactory) Create(count int) (res []zecTypes.NetResource, err error) {
	for i := 0; i < count; i++ {
		var eni zecTypes.NetResource
		eni, err = f.CreateWithIPCount(1)
		if err == nil {
			res = append(res, eni)
		}
	}
	if len(res) > 0 {
		return res, nil
	}
	return nil, err
}

// CreateWithIPCount creates one interface carrying ipCnt addresses.
func (f *eniFactory) CreateWithIPCount(ipCnt int) (zecTypes.NetResource, error) {
	var err error
	defer func() {
		if err != nil {
			metrics.ResourceManagerErrInc("CreateWithIPCount", err)
		}
	}()
	subnet := f.subnets.SelectSubnet(f.ipStack, helper.WithAging(subnetAging))
	if subnet == nil {
		f.limit.CordonCreate("eniFactory create eni")
		return nil, fmt.Errorf("no available subnet, please check subnets and available ip of subnets")
	}
	f.limit.UnCordonCreate("eniFactory create eni")

	eni, err := f.zecApi.AllocENI(subnet.SubnetId, f.secManager.GetSecurityGroup(), ipCnt)
	if err != nil {
		log.Errorf("Failed to create eni, %v", err)
		// Every spelling of "this instance may not have another interface",
		// whichever call produced it: the create answers with
		// LimitExceeded.EnisPerInstance or QuotaExceeded.Eni, the attach with
		// OPERATION_DENIED_INSTANCE_ATTACH_NIC_EXCEED_LIMIT or
		// INVALID_INSTANCE_NIC_LIMIT. Missing one means the cached quota is
		// never refreshed on it, so createEniAsync keeps believing there is
		// room and loops create -> attach fail -> delete until the limit
		// manager's own 2-minute tick happens to catch up.
		//
		// By code and not by substring, which only works while the platform
		// error stays reachable through errors.As -- see the %w chain from
		// createENI/attachENI through AllocENI, and the test that pins it.
		errCodes := &apiErr.OpenApiErrCodeChain{}
		if errCodes.WithErrCodes(apiErr.LimitExceededEnisPerInstance,
			apiErr.QuotaExceededEni,
			apiErr.InvalidInstanceNicLimit,
			apiErr.OPERATION_DENIED_INSTANCE_ATTACH_NIC_EXCEED_LIMIT).ErrChainEqual(err) {
			f.limit.Update()
		}
		return nil, err
	}
	log.WithFields(zlog.Fields{"eniID": eni.GetID()}).Info("ENI created")
	return eni, nil
}

// ReleaseInValid releases the interface. A whole-ENI resource has no partial
// form to repair, so there is never a replacement to return.
func (f *eniFactory) ReleaseInValid(resource zecTypes.NetResource) (zecTypes.NetResource, error) {
	return nil, f.Release(resource)
}

func (f *eniFactory) Release(resource zecTypes.NetResource) error {
	var err error
	defer func() {
		if err != nil {
			metrics.ResourceManagerErrInc("Release", err)
			log.Errorf("Release eni %s failed, %v", resource.GetID(), err)
		} else {
			log.Infof("Release eni %s success", resource.GetID())
		}
	}()
	eni := resource.GetResourceItem()
	err = f.zecApi.FreeENI(eni.ENIId)
	return err
}

// Valid asks the platform for the interface. Any error counts as invalid to the
// caller, a transient lookup failure included.
func (f *eniFactory) Valid(resource zecTypes.NetResource) error {
	eni := resource.GetResourceItem()
	_, err := f.zecApi.GetENI(eni.ENIId, "", eni.ENIMac)
	return err
}

// List reports every attached interface as Normal. A whole ENI either exists on
// the platform or it does not, so there is no partial state to report.
func (f *eniFactory) List() (map[zecTypes.ResStatus]map[string]zecTypes.NetResource, error) {
	enis, err := f.zecApi.GetAttachedENIs()
	if err != nil {
		return nil, err
	}

	list := map[zecTypes.ResStatus]map[string]zecTypes.NetResource{}
	normal := map[string]zecTypes.NetResource{}
	for _, eni := range enis {
		normal[eni.GetID()] = eni
	}
	list[zecTypes.ResStatusNormal] = normal
	return list, nil
}

func (f *eniFactory) GC() error {
	return nil
}

func (f *eniFactory) GetResourceLimit() int {
	limit := f.limit.GetLimit()
	return limit.ENIAvailable()
}

func (f *eniFactory) monitor(subnetPeriod, limitPeriod time.Duration) {
	go wait.Forever(func() {
		log.Debugf("Monitor check subnet")
		defer runtime.HandleCrash(func(r interface{}) {
			log.Error(r)
		})
		if subnet := f.subnets.SelectSubnet(f.ipStack, helper.WithAging(subnetAging)); subnet != nil {
			f.limit.UnCordonCreate("eniFactory subnet monitor")
		}
	}, subnetPeriod)
	go wait.Forever(func() {
		log.Debugf("Monitor check limit")
		defer runtime.HandleCrash(func(r interface{}) {
			log.Error(r)
		})
		f.limit.Update()
	}, limitPeriod)
}

func newEniFactory(secManager helper.SecurityGroupManager, subnetManager helper.SubnetManager, api helper.ZecAPI, limit helper.InstanceLimitManager, ipStack types.IPStack) (*eniFactory, error) {
	if subnetManager == nil {
		return nil, fmt.Errorf("subnet manager is nil")
	}
	if api == nil {
		return nil, fmt.Errorf("zenlayer api is nil")
	}
	return &eniFactory{
		ipStack:    ipStack,
		secManager: secManager,
		subnets:    subnetManager,
		zecApi:     api,
		limit:      limit,
	}, nil
}
