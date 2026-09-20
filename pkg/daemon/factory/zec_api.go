// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

// Package factory turns platform resources into the objects the pool hands out.
//
// It covers the three things the daemon must ask the platform about: network
// interfaces (create, attach, release), the subnets those interfaces live in
// (CIDRs, gateways, free-address counts), and the per-instance quotas that cap
// how many of each this node may hold.
package factory

import (
	"context"
	"errors"
	"fmt"

	"math/rand"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	v1 "k8s.io/api/core/v1"

	k8sErr "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/wait"

	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/metadata"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zlog"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/backoff"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/utils"
	pkgUtils "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/utils"
	zecTypes "github.com/zenlayer/zenlayer-vpc-cni-k8s/types"

	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/tracing"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/ec2"
)

const (
	// Lowercase because every comparison against these goes through
	// strings.EqualFold: the OpenAPI answers "Primary"/"Secondary".
	ENITypePrimary   = "primary"
	ENITypeSecondary = "secondary"

	ENIStatusAvailable = "AVAILABLE"
	ENIStatusInuse     = "USED"

	eniLeakedTime = 5 * time.Minute
	maxPageSize   = 100
)

// ZecAPI is every platform call IPAM makes, gathered behind one interface so
// that the daemon depends on the operations rather than on the client, and so
// that tests can substitute a fake instead of reaching the network.
//
// Two properties of the underlying API shape everything here. Assign and attach
// calls are not idempotent, so a retry after an ambiguous failure can create a
// second resource rather than return the first. And a call that times out may
// still have taken effect, which is why reconcile has to be able to find and
// adopt a resource the daemon never recorded.
type ZecAPI interface {

	// AllocENI creates an ENI and attaches it to the instance
	AllocENI(subnetId string, securityGroupId string, ipCnt int) (*zecTypes.ENI, error)

	// FreeENI detaches an ENI from the instance and deletes it
	FreeENI(eniID string) error

	// GetENI returns an ENI's subnet gateway and CIDR, read from metadata by mac
	GetENI(eniId, subnetId, mac string) (*zecTypes.ENI, error)

	// GetAttachedENIs returns the attached ENIs whose addresses this daemon
	// manages; see isManagedENI for what qualifies
	GetAttachedENIs() ([]*zecTypes.ENI, error)

	// GetSecondaryENIMACs returns the MACs of the in-use secondary ENIs this
	// daemon created
	GetSecondaryENIMACs() ([]string, error)

	// GetENIIPList returns every IP the metadata service reports for an ENI,
	// its primary address included. GetENIAllocatableIPs is the filtered form.
	GetENIIPList(eniMac string) ([]net.IP, *net.IPNet, error)
	// GetENIAllocatableIPs returns only the addresses on an ENI that may be
	// given to a pod: GetENIIPList less that interface's own primary address,
	// which the guest configures on the interface itself.
	GetENIAllocatableIPs(eni *zecTypes.ENI) ([]net.IP, *net.IPNet, error)

	// AllocIPv4Addresses allocates v4Cnt IPv4 addresses on an ENI
	AllocIPv4Addresses(eniID, eniMac string, v4Cnt int) ([]net.IP, error)

	// DeallocIPv4Addresses deallocates the IPv4 addresses from an ENI
	DeallocIPv4Addresses(eniID, eniMac string, ipv4s []net.IP) error

	// GetOrAssignIPv6Block assigns an IPv6 /96 block to an ENI, or returns the
	// one it already has, together with the interface's own primary IPv6. That
	// primary falls inside the prefix on this platform, so a caller that takes
	// the prefix without it has nothing to exclude the interface's own address
	// from the carve.
	GetOrAssignIPv6Block(eniID, eniMac string) (ipv6Net *net.IPNet, primaryV6 net.IP, err error)

	// GetInstanceLimit returns the instance's vNIC and per-ENI IP quotas
	GetInstanceLimit() (*InstanceLimits, error)

	// GetTotalAttachedEniCnt returns the count of all ENIs attached to the
	// instance -- primary and secondary, even across accounts
	GetTotalAttachedEniCnt() (int, error)
}

type ZecAPIImpl struct {
	IPStack types.IPStack
	// ipv6Billing is what the platform charges for public IPv6 on an interface.
	// Carried here because every path that enables IPv6 on a NIC needs it, and
	// the assign is refused without it on a Public-IPv6 subnet.
	ipv6Billing ec2.IPv6Billing
	// metadata info
	InstanceMetadataGetter
	privateIPMutex sync.RWMutex
	// ec2
	ec2Client   ec2.EC2
	metadataSvc metadata.EC2MetadataWrapper
	// subnetMgr answers whether a subnet is a configured pod subnet; isManagedENI
	// is the only reader left. The IPv6 subnet questions it once served are now
	// read from metadata directly by GetENI.
	subnetMgr SubnetManager

	// manageUntaggedENI decides what happens to an attached secondary interface
	// this daemon did not create and that carries no no-manage tag: true adopts
	// it into the pool, false leaves it alone. See isManagedENI.
	manageUntaggedENI bool

	// limits resolves the per-instance vNIC / IP quotas. Injected so a future
	// API-backed provider drops in without touching this type.
	limits LimitProvider

	// primaryENIID is the instance's own interface, used to refuse operations
	// that would tear down the node's own networking. Only a successful lookup
	// is cached: the id cannot change for the life of the instance, while a
	// failed describe is usually the platform being briefly unavailable, and
	// caching that would disable the check for the rest of the process.
	primaryENIMu sync.Mutex
	primaryENIID string

	tags map[string]string
}

// PrimaryENIID returns the id of the instance's own network interface.
//
// The platform also rejects detaching it (InvalidNicPrimaryCantDetach), but
// relying on that alone surfaces the mistake as a remote error after a round of
// retries, with the caller already treating the release as in flight. Resolved
// here so the daemon can refuse before calling out.
func (e *ZecAPIImpl) PrimaryENIID() (string, error) {
	e.primaryENIMu.Lock()
	defer e.primaryENIMu.Unlock()

	if e.primaryENIID != "" {
		return e.primaryENIID, nil
	}
	enis, err := e.getNetworkInterfacesByDescribe(ENIStatusInuse, ENITypePrimary, nil, false)
	if err != nil {
		return "", fmt.Errorf("resolve primary eni: %w", err)
	}
	if len(enis) == 0 {
		return "", fmt.Errorf("instance reports no primary eni")
	}
	e.primaryENIID = zecCom.ToString(enis[0].NicId)
	return e.primaryENIID, nil
}

// deleteENI deletes an ENI. The platform refuses while it is still attached
// (InvalidNicAttached), and a ResourceNotFound is treated as success.
func (e *ZecAPIImpl) deleteENI(eniID string) error {
	var err error
	defer func() {
		if err != nil {
			fmtErr := fmt.Sprintf("delete eni failed, %v", err)
			_ = tracing.RecordNodeEvent(v1.EventTypeWarning, tracing.EventReleaseResourceFailed, fmtErr)
		}
	}()

	werr := wait.ExponentialBackoff(backoff.BackOff(backoff.APIWriteOps), func() (bool, error) {
		_, err = e.ec2Client.DeleteNetworkInterface(eniID)
		if err == nil {
			return true, nil
		}
		errCodes := &apiErr.OpenApiErrCodeChain{}
		if errCodes.WithErrCodes(apiErr.ResourceNotFound).ErrChainEqual(err) {
			log.Warnf("DeleteNetworkInterface %s occur err: %s, ignore", eniID, apiErr.ErrCode(err))
			return true, nil
		}
		errCodes = &apiErr.OpenApiErrCodeChain{}
		if errCodes.WithPublicErrCodes().WithErrCodes(apiErr.InvalidNicPrimaryCantDelete, apiErr.InvalidNicAttached,
			apiErr.InvalidNicStatus, apiErr.OperationDeniedPostpaidInPromise).ErrChainEqual(err) {
			return false, err
		}
		return false, nil
	})

	if err = apiErr.BackoffErrWrapper(werr, err); err != nil {
		return err
	}
	log.Infof("Deleted eni %s", eniID)
	return nil
}

// freeENI detaches an ENI and then deletes it, waiting sleepDelayAfterDetach in
// between: the delete is refused with InvalidNicAttached while the interface is
// still attached.
func (e *ZecAPIImpl) freeENI(eniID string, sleepDelayAfterDetach time.Duration) error {
	log.Debugf("free ENI")
	var err error

	// Detaching the instance's own interface would take the node off the
	// network -- kubelet, ssh and this daemon with it. Refuse before calling
	// out, rather than letting the platform reject it after a retry loop.
	if primaryID, pErr := e.PrimaryENIID(); pErr != nil {
		log.Warnf("Cannot confirm the primary eni before freeing %s: %v", eniID, pErr)
	} else if eniID == primaryID {
		return fmt.Errorf("refusing to free eni %s: it is the instance's primary interface", eniID)
	}

	defer func() {
		if err != nil {
			log.Errorf("Free eni failed, %v", err)
		}
	}()

	werr := wait.ExponentialBackoff(backoff.BackOff(backoff.APIWriteOps), func() (bool, error) {
		_, err = e.ec2Client.DetachNetworkInterface(eniID)
		if err == nil {
			return true, nil
		}
		errCodes := &apiErr.OpenApiErrCodeChain{}
		// Both codes describe an interface that is already off the instance,
		// which is the state this detach is trying to reach -- so they count as
		// success and the delete below still runs. The attach rollback in
		// AllocENI depends on that: it cannot tell whether the attach took
		// effect, so it has to free either way, and failing here on "does not
		// exist on any instance" would return before the delete and strand the
		// interface it had just created.
		if errCodes.WithErrCodes(apiErr.ResourceNotFound, apiErr.NicNotExistInstance).ErrChainEqual(err) {
			log.Warnf("Detach networkInterface %s occur err: %s, ignore", eniID, apiErr.ErrCode(err))
			return true, nil
		}
		errCodes.WithPublicErrCodes().WithErrCodes(apiErr.InvalidNicPrimaryCantDetach,
			apiErr.InstanceStatusNotSupport, apiErr.InstancePrecheckAbnormal)

		if errCodes.ErrChainEqual(err) {
			return false, err
		}
		return false, nil
	})

	if err = apiErr.BackoffErrWrapper(werr, err); err != nil {
		log.Errorf("FreeENI: detach network interface failed: %s", err.Error())
		fmtErr := fmt.Sprintf("detach eni failed, %v", err)
		_ = tracing.RecordNodeEvent(v1.EventTypeWarning, tracing.EventReleaseResourceFailed, fmtErr)
		return fmt.Errorf("free ENI failed while detach: %s", err.Error())
	}

	time.Sleep(sleepDelayAfterDetach)

	// Returned, not discarded: a successful detach followed by a failed delete
	// leaves an unattached interface behind. The hourly leak sweep is scoped by
	// the created-by tags, so an adopted interface it cannot recognize would be
	// orphaned for good. Reporting failure is safe -- both calls above tolerate
	// ResourceNotFound, so a retry over an already-detached or already-deleted
	// interface succeeds rather than wedging.
	return e.deleteENI(eniID)
}

// createENI creates an unattached ENI and pre-assigns the addresses it should
// carry: ipv4Cnt-1 secondary IPv4s on top of the one the create returns, and an
// IPv6 prefix when enableIpv6. A failure after the create rolls it back.
func (e *ZecAPIImpl) createENI(subnetId string, securityGroupId string, ipv4Cnt int, enableIpv6 bool) (string, error) {
	var eniResponse *zecApi.CreateNetworkInterfaceResponse
	var err error

	nicName := utils.GetCniNicName(e.GetInstanceId())

	werr := wait.ExponentialBackoff(backoff.BackOff(backoff.APIWriteOps), func() (bool, error) {
		eniResponse, err = e.ec2Client.CreateNetworkInterface(nicName, subnetId, securityGroupId, e.tags)
		errCodes := &apiErr.OpenApiErrCodeChain{}

		// A subnet, security group or resource group that does not exist will not
		// start existing on a retry. Retrying spends all six APIWriteOps
		// attempts before returning a refusal that was already final on the
		// first one.
		//
		// The instance's vNIC quota is the same argument: nothing frees a slot
		// while the backoff runs, and the wait is spent holding blockPause,
		// which also stops GC. LimitExceeded.EnisPerInstance and
		// QuotaExceeded.Eni are two spellings of the same refusal.
		if err == nil || errCodes.WithPublicErrCodes().WithErrCodes(apiErr.OperationFailedResourceGroupNotFound, apiErr.InvalidSubnetNotFound,
			apiErr.InvalidSecurityGroupNotFound, apiErr.OperationDeniedInternetChargeTypeNotSupport,
			apiErr.LimitExceededEnisPerInstance, apiErr.QuotaExceededEni).
			ErrChainEqual(err) {
			return true, err
		}
		return false, nil
	})

	if err = apiErr.BackoffErrWrapper(werr, err); err != nil {
		log.Errorf("Failed to CreateNetworkInterface %s", err.Error())
		// %w, not %s: eniFactory.CreateWithIPCount decides whether the cached
		// instance quota is stale from this error's code, and ErrChainEqual is
		// errors.As underneath. Rendering the platform error into the message
		// leaves the code readable to a human but unreachable to errors.As, so
		// the refresh never fires and the daemon keeps creating interfaces the
		// instance cannot attach.
		return "", fmt.Errorf("failed to create network interface: %w", err)
	}

	if eniResponse.Response == nil {
		err := fmt.Errorf("get no result while CreateNetworkInterface in subnet %s", subnetId)
		log.Error(err)
		return "", err
	}

	eniID := *(eniResponse.Response.NicId)
	defer func() {
		if err != nil {
			err = e.deleteENI(eniID)
			if err != nil {
				log.Errorf("rollback create eni due to %v", err)
			}
		}
	}()
	// %w on both: receiveRes refreshes the instance limit and wakes GC on the
	// codes these two return -- LAN_IP_EXCEED_LIMIT from the address assign,
	// INVALID_SUBNET_IPSTACK_NOT_SUPPORT from the prefix assign. The error
	// travels through AllocENI, CreateWithIPCount and initENI's epilogue; a
	// flattened wrap anywhere on that route silently defeats the check.
	if ipv4Cnt > 1 {
		_, err = e.ec2Client.AssignPrivateIpAddress(eniID, []string{}, ipv4Cnt-1)
		if err != nil {
			return "", fmt.Errorf("assign ipv4 address for %s failed, %w", eniID, err)
		}
	}
	if enableIpv6 {
		_, err = e.ec2Client.AssignNetworkInterfaceIpv6(eniID, e.ipv6Billing)
		if err != nil {
			return "", fmt.Errorf("assign ipv6 address for %s failed, %w", eniID, err)
		}
	}
	return eniID, nil
}

// attachENI attaches the ENI to the instance and waits for its status to become
// inuse.
func (e *ZecAPIImpl) attachENI(eniID string) (*zecApi.NicInfo, error) {
	var err error
	werr := wait.ExponentialBackoff(backoff.BackOff(backoff.APIWriteOps), func() (bool, error) {
		_, err = e.ec2Client.AttachNetworkInterface(eniID, e.GetInstanceId())
		errCodes := &apiErr.OpenApiErrCodeChain{}
		// OPERATION_DENIED_INSTANCE_ATTACH_NIC_EXCEED_LIMIT is the code attach
		// actually returns once the instance carries as many interfaces as it
		// may; InvalidInstanceNicLimit is the other code for the same refusal.
		// Naming only one of them left the other burning the whole APIWriteOps
		// budget -- 68s per attempt, measured 2026-08-27 -- for a refusal no
		// retry can clear, since nothing frees a vNIC slot while the backoff
		// runs.
		if errCodes.WithPublicErrCodes().WithErrCodes(apiErr.InvalidNicStatus, apiErr.OperationDeniedInstanceStatusNotSupport,
			apiErr.InvalidNicAttached, apiErr.InvalidInstanceNicLimit, apiErr.InvalidInstancePrecheckAbnormal,
			apiErr.OperationFailedResourceNotFound,
			apiErr.OPERATION_DENIED_INSTANCE_ATTACH_NIC_EXCEED_LIMIT).ErrChainEqual(err) {
			return false, err
		}
		return err == nil, nil
	})

	if err = apiErr.BackoffErrWrapper(werr, err); err != nil {
		log.Errorf("Attach ENI %s failed: %s", eniID, err.Error())
		// %w for the same reason as createENI above: the quota refusal has to
		// stay reachable by errors.As all the way up to CreateWithIPCount.
		return nil, fmt.Errorf("attach ENI %s failed: %w", eniID, err)
	}

	req := zecApi.NewDescribeNetworkInterfacesRequest()
	req.NicIds = []string{eniID}
	regionId := e.GetRegionId()
	req.RegionId = zecCom.String(regionId)
	var eniAttr *zecApi.DescribeNetworkInterfacesResponse
	werr = wait.ExponentialBackoff(backoff.BackOff(backoff.APIStatusWait), func() (bool, error) {
		eniAttr, err = e.ec2Client.DescribeNetworkInterfaceAttributes(req)
		if err == nil && eniAttr.Response != nil && len(eniAttr.Response.DataSet) != 0 && *(eniAttr.Response.DataSet[0].Status) == ENIStatusInuse {
			return true, nil
		}
		return false, nil
	})

	if err = apiErr.BackoffErrWrapper(werr, err); err != nil {
		return nil, fmt.Errorf("eni %s status has not become %s: %w", eniID, ENIStatusInuse, err)
	}

	return eniAttr.Response.DataSet[0], nil
}

func (e *ZecAPIImpl) AllocENI(subnetId string, securityGroupId string, ipCnt int) (*zecTypes.ENI, error) {
	lg := log.WithFields(zlog.Fields{
		"SubnetId":      subnetId,
		"InstanceId":    e.GetInstanceId(),
		"SecurityGroup": securityGroupId,
		"IPCnt":         ipCnt,
	})
	lg.Infof("Creating eni")

	ipv4Cnt := 0
	if e.IPStack.EnableIPv4() {
		ipv4Cnt = ipCnt
	}

	eniId, err := e.createENI(subnetId, securityGroupId, ipv4Cnt, e.IPStack.EnableIPv6())
	if err != nil {
		fmtErr := fmt.Sprintf("AllocENI failed, %v", err)
		_ = tracing.RecordNodeEvent(v1.EventTypeWarning, tracing.EventAllocateResourceFailed, fmtErr)
		// %w: last hop before eniFactory.CreateWithIPCount, which reads the
		// platform's error code off it to decide whether the cached vNIC quota
		// is stale. A %v here undoes the %w in createENI and attachENI.
		return nil, fmt.Errorf("AllocENI: failed to create ENI: %w", err)
	}

	lg = lg.WithFields(zlog.Fields{"EniId": eniId})
	eniInfo, err := e.attachENI(eniId)
	if err != nil {
		// An attach error does not mean the interface stayed off the instance.
		// attachENI also reports failure when the write landed but the status
		// poll ran out of budget, and when a retry gets INVALID_NIC_ATTACHED
		// because an earlier attempt took effect and its response was lost. A
		// delete cannot remove an attached interface, so cleaning up that way
		// left the interface this call had just created on the instance for
		// good. FreeENI detaches first and treats an already-detached interface
		// as detached, so it covers the attached and the never-attached case
		// alike.
		lg.Errorf("Attach eni failed, freeing it, %v", err)
		if freeErr := e.FreeENI(eniId); freeErr != nil {
			lg.Errorf("Failed to free newly created ENI %s: %v", eniId, freeErr)
		}
		return nil, fmt.Errorf("alloc ENI: error attaching ENI: %w", err)
	}

	defer func() {
		if err != nil {
			rollErr := e.FreeENI(eniId)
			if rollErr != nil {
				fmtErr := fmt.Sprintf("Free eni failed while rollback AllocENI, %v", rollErr)
				_ = tracing.RecordNodeEvent(v1.EventTypeWarning, tracing.EventAllocateResourceFailed, fmtErr)
				lg.Error(fmtErr)
			}
		}
	}()

	var eni *zecTypes.ENI
	var inErr error
	err = wait.ExponentialBackoff(backoff.BackOff(backoff.APIStatusWait), func() (bool, error) {
		eni, inErr = e.GetENI(zecCom.ToString(eniInfo.NicId), zecCom.ToString(eniInfo.SubnetId), zecCom.ToString(eniInfo.MacAddress))
		if inErr != nil {
			return false, nil
		}
		return true, nil
	})
	if err != nil {
		return nil, fmt.Errorf("get eni from metadata failed, %v, %v", inErr, err)
	}
	if e.IPStack.EnableIPv4() && zecCom.ToString(eniInfo.PrimaryIpv4) != "" {
		eni.PrimaryIP.IPv4 = net.ParseIP(zecCom.ToString(eniInfo.PrimaryIpv4))
	}
	if e.IPStack.EnableIPv6() && zecCom.ToString(eniInfo.PrimaryIpv6) != "" {
		eni.PrimaryIP.IPv6 = net.ParseIP(zecCom.ToString(eniInfo.PrimaryIpv6))
	}
	// An interface with no IPv6 prefix yet is described, not rejected. Erroring
	// here made dual unreachable on any node that had ever run IPv4-only: the
	// instance's own interface has no prefix, and neither do the interfaces this
	// CNI created before the switch, since it passed enableIpv6=false. The caller
	// repairs a nil prefix by asking GetOrAssignIPv6Block for one.
	if e.IPStack.EnableIPv6() && zecCom.ToString(eniInfo.Ipv6Cidr) != "" {
		eni.IPv6Net, err = pkgUtils.ParseCidr(zecCom.ToString(eniInfo.Ipv6Cidr))
		if err != nil {
			return nil, fmt.Errorf("parse cidr %s of eni %s failed, %v", zecCom.ToString(eniInfo.Ipv6Cidr), eni.ID, err)
		}
	}
	return eni, nil
}

func (e *ZecAPIImpl) FreeENI(eniID string) error {
	return e.freeENI(eniID, 2*time.Second)
}

// isOwnENI reports whether this daemon created an ENI.
//
// Ownership rests on the tags alone. A name is something anyone can set,
// including to this daemon's exact prefix, so accepting it as proof would let
// the daemon adopt -- and eventually detach and delete -- an interface an
// operator attached by hand. Both tags are required: created-by alone would
// let one node manage another node's interfaces.
func (e *ZecAPIImpl) isOwnENI(nic *zecApi.NicInfo) bool {
	if nic == nil || nic.Tags == nil {
		return false
	}
	var createdBy, instanceID bool
	for _, t := range nic.Tags.Tags {
		switch zecCom.ToString(t.Key) {
		case utils.TagKeyCreatedBy:
			createdBy = zecCom.ToString(t.Value) == utils.TagValueCreatedBy
		case utils.TagKeyInstanceID:
			instanceID = zecCom.ToString(t.Value) == e.GetInstanceId()
		}
	}
	return createdBy && instanceID
}

// isManagedENI reports whether this daemon may serve pod addresses from an
// attached secondary interface, and detach it when the pool shrinks.
//
// Three rules, in order:
//
//	no-manage tag set to "true"  -> never managed, whatever else is true
//	created by this daemon       -> managed
//	otherwise                    -> managed iff manageUntaggedENI AND the
//	                                interface sits in a configured pod subnet
//
// The tag is checked first so it also exempts an interface this daemon created,
// and so it holds whichever way manageUntaggedENI is set: tagging a storage or
// management interface gives one answer without having to know the node's
// configuration.
//
// The subnet condition applies to adoption only, never to an interface this
// daemon created -- that one stays managed even after its subnet is dropped
// from the configuration, or it would be stranded with its addresses.
//
// Adopting outside the configured subnets would bring pods up on a subnet
// nobody chose for pod traffic, while its addresses counted towards the node's
// advertised capacity.
func (e *ZecAPIImpl) isManagedENI(nic *zecApi.NicInfo) bool {
	if nic == nil {
		return false
	}
	if nic.Tags != nil {
		for _, t := range nic.Tags.Tags {
			if zecCom.ToString(t.Key) == utils.TagKeyNoManage &&
				strings.EqualFold(zecCom.ToString(t.Value), utils.TagValueNoManage) {
				return false
			}
		}
	}
	if e.isOwnENI(nic) {
		return true
	}
	if !e.manageUntaggedENI {
		return false
	}
	// No subnet manager means nothing can be confirmed as configured, so
	// nothing is adopted. Refusing is the recoverable direction: an interface
	// left alone costs capacity, one adopted by mistake can be deleted.
	return e.subnetMgr != nil && e.subnetMgr.IsPodSubnet(zecCom.ToString(nic.SubnetId))
}

// GetAttachedENIs returns the ENIs whose addresses this daemon manages: the
// ones it created, plus the instance's primary interface, whose secondary
// addresses can serve pods -- excluding it would waste half the capacity of a
// two-interface node. The primary's own address belongs to the node and is
// never handed out; see GetENIAllocatableIPs.
//
// Secondary interfaces this daemon did not create are managed too, unless they
// carry the no-manage tag or manageUntaggedENI is off. See isManagedENI.
func (e *ZecAPIImpl) GetAttachedENIs() (result []*zecTypes.ENI, err error) {
	enis, err := e.getNetworkInterfacesByDescribe(ENIStatusInuse, "", nil, false)
	if err != nil {
		return nil, fmt.Errorf("filter eni failed, %v", err)
	}

	var cniMacs, adoptedMacs, primaryMacs, skippedMacs []string
	managed := map[string]*zecApi.NicInfo{}
	primary := map[string]bool{}
	for _, eni := range enis {
		id := zecCom.ToString(eni.NicId)
		isPrimary := strings.EqualFold(zecCom.ToString(eni.NicType), ENITypePrimary)
		switch {
		case isPrimary:
			primary[id] = true
			primaryMacs = append(primaryMacs, zecCom.ToString(eni.MacAddress))
		case e.isManagedENI(eni):
			if e.isOwnENI(eni) {
				cniMacs = append(cniMacs, zecCom.ToString(eni.MacAddress))
			} else {
				adoptedMacs = append(adoptedMacs, zecCom.ToString(eni.MacAddress))
			}
		default:
			// Left alone: no-manage tag, outside the configured pod subnets, or
			// manageUntaggedENI is off. It still holds one of the instance's
			// interface slots, so the node's capacity is lower than the spec
			// table predicts -- hence the log line rather than a silent skip.
			skippedMacs = append(skippedMacs, zecCom.ToString(eni.MacAddress))
			continue
		}
		managed[id] = eni
	}
	log.Infof("Attached eni created by cni: %v, adopted: %v, primary: %v", cniMacs, adoptedMacs, primaryMacs)
	if len(skippedMacs) > 0 {
		// The count is spelled out because it is what lowers allocatable: the
		// limit manager arrives at the same figure independently, as
		// ENIUnmanaged.
		log.Infof("Attached eni not managed by cni: %v; %d interface slot(s) held without serving pod addresses (ENIUnmanaged)",
			skippedMacs, len(skippedMacs))
	}

	for _, eni := range enis {
		eni, inErr := e.GetENI(zecCom.ToString(eni.NicId), zecCom.ToString(eni.SubnetId), zecCom.ToString(eni.MacAddress))
		if inErr != nil {
			return nil, inErr
		}
		if item, exist := managed[eni.ID]; exist {
			eni.IsPrimary = primary[eni.ID]
			if e.IPStack.EnableIPv4() && zecCom.ToString(item.PrimaryIpv4) != "" {
				eni.PrimaryIP.IPv4 = net.ParseIP(zecCom.ToString(item.PrimaryIpv4))
			}
			if e.IPStack.EnableIPv6() && zecCom.ToString(item.PrimaryIpv6) != "" {
				eni.PrimaryIP.IPv6 = net.ParseIP(zecCom.ToString(item.PrimaryIpv6))
			}
			// Same as the single-interface path above: a missing prefix is a
			// state the caller repairs, not a reason to refuse to describe the
			// interface. This loop covers the instance's own interface, so
			// erroring here stopped every dual node from starting.
			if e.IPStack.EnableIPv6() && zecCom.ToString(item.Ipv6Cidr) != "" {
				eni.IPv6Net, err = pkgUtils.ParseCidr(zecCom.ToString(item.Ipv6Cidr))
				if err != nil {
					return nil, fmt.Errorf("parse cidr %s of eni %s failed, %v", zecCom.ToString(item.Ipv6Cidr), eni.ID, err)
				}
			}
			result = append(result, eni)
		}
	}
	return
}

// GetTotalAttachedEniCnt returns the count of all ENIs attached to the instance
// -- primary and secondary, even across accounts.
func (e *ZecAPIImpl) GetTotalAttachedEniCnt() (int, error) {
	var inErr error
	var resp *zecApi.DescribeInstancesResponse
	err := wait.ExponentialBackoff(backoff.BackOff(backoff.APIFastRetry), func() (bool, error) {
		resp, inErr = e.ec2Client.DescribeInstances([]string{e.GetInstanceId()})
		if inErr != nil {
			return false, nil
		}
		return true, nil
	})
	if err = apiErr.BackoffErrWrapper(err, inErr); err != nil {
		return 0, fmt.Errorf("describe instance failed, %v, %v", err, inErr)
	}
	if resp == nil || resp.Response == nil || len(resp.Response.DataSet) == 0 {
		return 0, fmt.Errorf("describe instance failed, no result")
	}
	return len(resp.Response.DataSet[0].Nics), nil
}

func (e *ZecAPIImpl) GetSecondaryENIMACs() ([]string, error) {
	// The metadata service's MAC list also carries non-secondary interfaces in
	// scenarios such as rdma and cross-account attachment, so this goes through
	// the OpenAPI instead.
	var result []string
	enis, err := e.getNetworkInterfacesByDescribe(ENIStatusInuse, ENITypeSecondary, nil, true)
	if err != nil {
		return nil, fmt.Errorf("filter secondary eni failed, %v", err)
	}
	for _, eni := range enis {
		result = append(result, zecCom.ToString(eni.MacAddress))
	}
	return result, nil
}

// GetENIAllocatableIPs returns the addresses of an ENI that may be handed to
// pods, plus the ENI's IPv6 prefix. Every interface's own primary is filtered
// out, the instance's own interface included; its secondaries remain.
//
// The primary is withheld because the guest configures it on the interface, for
// a source address the fabric accepts when resolving the gateway (see
// eniAddrs). Taking it off the netdev first does not make it safe to hand out:
// a fabric checking source addresses drops an ARP whose sender belongs to
// another port's MAC.
func (e *ZecAPIImpl) GetENIAllocatableIPs(eni *zecTypes.ENI) ([]net.IP, *net.IPNet, error) {
	v4s, v6Net, err := e.GetENIIPList(eni.Mac.String())
	if err != nil {
		return nil, nil, err
	}

	// Withholding the primary matters more for IPv6: this platform puts an
	// interface's primary IPv6 inside the prefix pod addresses are carved from,
	// and nothing detects a collision afterwards. firstPodOffsetV6 skips where
	// the primary usually sits, but nothing pins it there.
	if v6Net != nil && e.IPStack.EnableIPv6() && eni.PrimaryIP.IPv6 == nil {
		return nil, nil, fmt.Errorf("eni %s has ipv6 prefix %s but reports no primary ipv6, refusing to carve pod addresses from it",
			eni.ID, v6Net)
	}

	// Same two-source rule as the IPv4 primary below, except the second answer
	// is recorded in PrimaryIPv6Disputed for the carve to skip rather than
	// filtered out here. The dangerous case is the instance's own interface,
	// where DHCPv6 writes the address from the platform's record -- if describe
	// disagrees, the carve skips the wrong one and hands out the address already
	// on the host.
	//
	// A 404 is not a disagreement: interfaces that never had IPv6 answer 404
	// here as they do for ipv6-prefix. Treat a read failure as "no second
	// opinion" rather than taking the interface out of service.
	if v6Net != nil && e.IPStack.EnableIPv6() && eni.Mac != nil {
		metaV6, metaErr := e.metadataSvc.GetENIPrimaryIPv6(context.Background(), eni.Mac.String())
		switch {
		case metaErr != nil:
			log.Debugf("Eni %s: no metadata answer for the primary ipv6 (%v); using the describe answer %s alone",
				eni.ID, metaErr, eni.PrimaryIP.IPv6)
		case metaV6 != nil && !metaV6.Equal(eni.PrimaryIP.IPv6):
			log.Warnf("Eni %s primary ipv6 disagrees between sources: metadata says %s, describe says %s; withholding both",
				eni.ID, metaV6, eni.PrimaryIP.IPv6)
			eni.PrimaryIPv6Disputed = metaV6
		default:
			eni.PrimaryIPv6Disputed = nil
		}
	}

	// Refuse rather than guess: without knowing which address is the
	// interface's own, filtering cannot be done safely, and handing it out is
	// unrecoverable.
	if eni.PrimaryIP.IPv4 == nil {
		return nil, nil, fmt.Errorf("eni %s reports no primary address, refusing to treat any of %v as allocatable",
			eni.ID, v4s)
	}

	// Two sources name the primary address: this list leads with the metadata
	// service's answer (see GetENIIPList), PrimaryIP.IPv4 comes from the
	// platform's describe. Exclude both, and say so when they differ -- the
	// datapath configures the describe's answer on the interface, so a metadata
	// answer that slipped through would be handed to a pod while the interface
	// holds a different address.
	metaPrimary := net.IP(nil)
	if len(v4s) > 0 {
		metaPrimary = v4s[0]
	}
	if metaPrimary != nil && !metaPrimary.Equal(eni.PrimaryIP.IPv4) {
		log.Warnf("Eni %s primary address disagrees between sources: metadata says %s, describe says %s; withholding both",
			eni.ID, metaPrimary, eni.PrimaryIP.IPv4)
	}

	allocatable := make([]net.IP, 0, len(v4s))
	for _, ip := range v4s {
		if ip.Equal(eni.PrimaryIP.IPv4) || ip.Equal(metaPrimary) {
			continue
		}
		allocatable = append(allocatable, ip)
	}
	return allocatable, v6Net, nil
}

// isMetadataNotFound reports whether the metadata service answered 404, which
// it uses for "this key does not apply here" rather than for a failure.
func isMetadataNotFound(err error) bool {
	var statusErr *metadata.StatusError
	return errors.As(err, &statusErr) && statusErr.StatusCode == http.StatusNotFound
}

// GetENIIPList returns an ENI's IPv4 addresses from the instance metadata
// service, primary first, along with its IPv6 prefix when IPv6 is enabled.
func (e *ZecAPIImpl) GetENIIPList(eniMac string) ([]net.IP, *net.IPNet, error) {
	e.privateIPMutex.RLock()
	defer e.privateIPMutex.RUnlock()
	ctx := context.Background()
	primaryIP, err := e.metadataSvc.GetENIPrimaryIP(ctx, eniMac)
	if err != nil {
		return nil, nil, err
	}
	privateIPv4s, err := e.metadataSvc.GetENIPrivateIPv4s(ctx, eniMac)
	if err != nil {
		return nil, nil, err
	}

	// Same as GetENI: ipv6-prefix is 404 on an IPv4-only subnet, so only ask for
	// it when the daemon runs a stack that needs it. A 404 is also the honest
	// answer for an interface that has no prefix yet -- every interface on a node
	// switching over from IPv4-only -- and treating it as an error failed the
	// whole pool build before the caller could reach the code that assigns one.
	var IPv6Cidr *net.IPNet
	if e.IPStack.EnableIPv6() {
		IPv6Cidr, err = e.metadataSvc.GetENIIPv6Prefix(ctx, eniMac)
		if isMetadataNotFound(err) {
			log.Infof("Eni %s has no ipv6 prefix yet; it will be assigned one", eniMac)
			IPv6Cidr, err = nil, nil
		}
		if err != nil {
			return nil, nil, err
		}
	}
	return append([]net.IP{primaryIP}, privateIPv4s...), IPv6Cidr, nil
}

func (e *ZecAPIImpl) AllocIPv4Addresses(eniID, eniMac string, v4Cnt int) ([]net.IP, error) {
	if eniID == "" || eniMac == "" {
		return nil, fmt.Errorf("AllocIPv4Addresses needs both an interface id and its mac, got id %q and mac %q", eniID, eniMac)
	}
	e.privateIPMutex.Lock()
	defer e.privateIPMutex.Unlock()

	var err, v4Err error
	var ipv4s []net.IP
	defer func() {
		if err != nil {
			fmtErr := fmt.Sprintf("AllocIPv4Addresses failed, %v", err)
			_ = tracing.RecordNodeEvent(v1.EventTypeWarning, tracing.EventAllocateResourceFailed, fmtErr)
			log.Warnf("Rollback AllocIPv4Addresses due to %s", fmtErr)
			err = e.deallocIPv4AddressesWithLocked(eniID, eniMac, ipv4s)
			if err != nil {
				fmtErr = fmt.Sprintf("DeallocIPAddress failed while rollback AllocIPv4Addresses, %v", err)
				_ = tracing.RecordNodeEvent(v1.EventTypeWarning, tracing.EventAllocateResourceFailed, fmtErr)
				log.Error(fmtErr)
			}
		}
	}()

	if e.IPStack.EnableIPv4() && v4Cnt > 0 {
		var assignIPResp *zecApi.BatchAssignNetworkInterfaceIpv4Response
		werr := wait.ExponentialBackoff(backoff.BackOff(backoff.APIWriteOps), func() (bool, error) {
			assignIPResp, err = e.ec2Client.AssignPrivateIpAddress(eniID, []string{}, v4Cnt)
			if err == nil {
				return true, nil
			}
			errCodes := &apiErr.OpenApiErrCodeChain{}
			if errCodes.WithPublicErrCodes().WithErrCodes(apiErr.OPERATION_DENIED_SUBNET_TYPE_NOT_SUPPORT_IPV4,
				apiErr.INVALID_NIC_PRIVATE_IP_EXCEED_LIMIT, apiErr.INVALID_SUBNET_PRIVATE_IPV4_IN_USED,
				apiErr.INVALID_IP_BROADCAST_ADDRESS, apiErr.INVALID_SUBNET_PRIVATE_IPV4_IN_USED,
				apiErr.INVALID_IP_OUT_OF_RANGE, apiErr.INVALID_IP_FIRST_ADDRESS,
				apiErr.OPERATION_FAILED_RESOURCE_NOT_FOUND, apiErr.INVALID_IP_NETWORK_ADDRESS,
				// Retrying an exhausted subnet cannot succeed within one call:
				// nothing frees addresses while the backoff runs. Measured at
				// 62-70s burned per attempt before giving up, and before this
				// code was listed the attempts were repeated against the same
				// ENI, because the caller never learned it was full.
				apiErr.OPERATION_DENIED_SUBNET_IP_INSUFFICIENT, apiErr.InsufficientIpInSubnet,
				// A full interface is the same argument as an exhausted subnet:
				// nothing frees an address on this interface while the backoff
				// runs, and the wait is spent holding blockPause, which also
				// stops GC. This is the code the platform actually returns;
				// INVALID_NIC_PRIVATE_IP_EXCEED_LIMIT above is the other spelling,
				// and the forbidAssign check in worker() lists both.
				apiErr.OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT,
				apiErr.INVALID_NIC_STATUS).ErrChainEqual(err) {
				return false, err
			}
			// No error code means the platform never answered, so the request
			// may have been applied. This assign is not idempotent -- an empty
			// ip list means "give me v4Cnt more" -- so retrying would ask for
			// another v4Cnt on top, up to six times. The caller reconciles an
			// ambiguous assign with recoverAfterAmbiguousAssign.
			if apiErr.ErrCode(err) == "" {
				return false, err
			}
			return false, nil
		})
		if err = apiErr.BackoffErrWrapper(werr, err); err != nil {
			// %w, not %s: two readers match on the platform's error code via
			// errors.As -- worker()'s forbidAssign check, and receiveRes(),
			// which refreshes the instance limit and wakes GC. Rendering the
			// error into the message leaves the code visible to
			// strings.Contains but unreachable to errors.As, so both go quiet
			// at once. Pinned by
			// TestAllocIPv4AddressesReturnsAnUnwrappableError.
			return ipv4s, fmt.Errorf("failed to assign private ips on ENI %s, err: %w", eniID, err)
		}

		if assignIPResp != nil && assignIPResp.Response != nil {
			ipl := len(assignIPResp.Response.IpAddresses)
			if ipl < v4Cnt {
				return ipv4s, fmt.Errorf("the total number of ips is %d, less than wanted: %d", ipl, v4Cnt)
			}
			ipv4s, err = pkgUtils.ParseIPs(assignIPResp.Response.IpAddresses[ipl-v4Cnt:])
			if err != nil {
				return ipv4s, err
			}
		}

		// MetaStatusWait, not APIStatusWait: this waits on the node's own
		// metadata, same as the dealloc path. The API budget is 20x longer
		// (~246s) and cannot fit -- the CNI ADD has 118s in total, this runs
		// after the assign's own backoff, and privateIPMutex is held throughout.
		var inErr error
		var metaV4s []net.IP
		v4Err = wait.ExponentialBackoff(backoff.BackOff(backoff.MetaStatusWait), func() (bool, error) {
			metaV4s, inErr = e.metadataSvc.GetENIPrivateIPv4s(context.Background(), eniMac)
			if inErr != nil {
				return false, nil
			}
			if !pkgUtils.NetIPContainAll(metaV4s, ipv4s) {
				return false, nil
			}
			return true, nil
		})
		if v4Err != nil {
			v4Err = fmt.Errorf("%w, metadata err: %v", v4Err, inErr)
		}
	}

	err = k8sErr.NewAggregate([]error{v4Err})
	if err != nil {
		return nil, err
	}
	log.Infof("Successfully assigned IPv4 address on ENI %s, ipv4s: %s", eniID, pkgUtils.ToStringSlice(ipv4s))
	return ipv4s, nil
}

func (e *ZecAPIImpl) DeallocIPv4Addresses(eniID, eniMac string, ipv4s []net.IP) error {
	e.privateIPMutex.Lock()
	defer e.privateIPMutex.Unlock()

	err := e.deallocIPv4AddressesWithLocked(eniID, eniMac, ipv4s)
	return err
}

// deallocIPv4AddressesWithLocked unassigns IPv4 addresses from an ENI. The
// caller must hold privateIPMutex.
//
// IPv4 only, and there is deliberately no IPv6 counterpart: the platform hands
// an interface one IPv6 prefix, whole, and takes back nothing address by
// address. A pod's IPv6 is carved out of that prefix locally, so dropping the
// local reference is the whole of its release.
func (e *ZecAPIImpl) deallocIPv4AddressesWithLocked(eniID, eniMac string, ipv4s []net.IP) (err error) {
	if eniID == "" || eniMac == "" {
		return fmt.Errorf("deallocIPv4Addresses needs both an interface id and its mac, got id %q and mac %q", eniID, eniMac)
	}
	if len(ipv4s) == 0 {
		return nil
	}

	var errs []error
	lg := log.WithFields(zlog.Fields{
		"ENI":   eniID,
		"IPv4s": pkgUtils.ToStringSlice(ipv4s),
	})
	lg.Infof("Deallocating ipaddress")

	defer func() {
		if err != nil {
			fmtErr := fmt.Sprintf("DeallocIPAddresses for eni %s failed, %v", eniID, err)
			_ = tracing.RecordNodeEvent(v1.EventTypeWarning, tracing.EventReleaseResourceFailed, fmtErr)
		} else {
			lg.Infof("UnAssigned ipaddress")
		}
	}()

	// One unassign call covers every address, so this either fully succeeds or
	// fully fails. There is no partially-released state for the caller to
	// distinguish, which is why no "halfway" sentinel is produced here.
	{
		werr := wait.ExponentialBackoff(backoff.BackOff(backoff.APIWriteOps), func() (bool, error) {
			_, err = e.ec2Client.UnAssignPrivateIpAddress(eniID, pkgUtils.ToStringSlice(ipv4s))
			if err == nil {
				return true, nil
			}
			errCodes := &apiErr.OpenApiErrCodeChain{}
			if errCodes.WithPublicErrCodes().WithErrCodes(apiErr.INVALID_NIC_PRIMARY_IP_CANT_DETACH).
				WithErrCodes(apiErr.INVALID_NIC_LB_ASSOCIATED).WithErrCodes(apiErr.INVALID_NIC_STATUS).
				WithErrCodes(apiErr.INVALID_NIC_HAS_PUBLIC_IP_CANT_DETACH).ErrChainEqual(err) {
				return false, err
			}
			errCodes = &apiErr.OpenApiErrCodeChain{}
			if errCodes.WithErrCodes(apiErr.InvalidPrivateIpMalformed, apiErr.INVALID_NIC_NOT_FOUND).ErrChainEqual(err) {
				return true, nil
			}
			lg.Warnf("UnAssignPrivateIpAddress failed: %s", err.Error())
			return false, nil
		})

		if err = apiErr.BackoffErrWrapper(werr, err); err != nil {
			errs = append(errs, err)
		}
	}

	if len(errs) > 0 {
		return k8sErr.NewAggregate(errs)
	}

	var inErr error
	werr := wait.ExponentialBackoff(backoff.BackOff(backoff.MetaStatusWait), func() (bool, error) {
		var metaV4s []net.IP
		metaV4s, inErr = e.metadataSvc.GetENIPrivateIPv4s(context.Background(), eniMac)
		if inErr != nil {
			return false, nil
		}
		if len(ipv4s) > 0 && pkgUtils.NetIPContainAny(metaV4s, ipv4s) {
			inErr = fmt.Errorf("ips %s were expected to be unassigned, but the interface still reports %s", ipv4s, metaV4s)
			return false, nil
		}
		return true, nil
	})

	if werr != nil {
		return inErr
	}
	return nil
}

func (e *ZecAPIImpl) GetOrAssignIPv6Block(eniID, eniMac string) (ipv6Net *net.IPNet, primaryV6 net.IP, err error) {
	if eniID == "" || eniMac == "" {
		return nil, nil, fmt.Errorf("GetOrAssignIPv6Block needs both an interface id and its mac, got id %q and mac %q", eniID, eniMac)
	}
	e.privateIPMutex.Lock()
	defer e.privateIPMutex.Unlock()

	defer func() {
		if err != nil {
			fmtErr := fmt.Sprintf("GetOrAssignIPv6Block failed, %v", err)
			_ = tracing.RecordNodeEvent(v1.EventTypeWarning, tracing.EventAllocateResourceFailed, fmtErr)
		}
	}()

	if !e.IPStack.EnableIPv6() {
		log.Infof("IPv6 is not enabled in IPStack")
		return nil, nil, nil
	}

	// The primary comes back with the prefix, from the same describe: on this
	// platform the interface's primary IPv6 falls inside the prefix, and the
	// callers of this function are the paths that repair a missing one.
	// Discarding it left PrimaryIP.IPv6 nil on exactly those paths, with
	// nothing for the carve to exclude.
	getENIIPv6Block := func(eniID string) (*net.IPNet, net.IP, error) {
		// isCniCreate=false deliberately. The describe already filters
		// server-side by this exact interface id, so the tag filter adds no
		// authorization -- and it excludes the interface that needs this most:
		// the instance's own, which carries neither of the tags isOwnENI
		// requires. With the filter on, the primary answered "none network
		// interfaces found" both before and after the assign, so dual could not
		// be enabled on any node whose primary had no IPv6 prefix yet -- that is,
		// every node upgrading from ipv4 -- and the assign was retried on every
		// restart.
		enis, err := e.getNetworkInterfacesByDescribe(ENIStatusInuse, "", []string{eniID}, false)
		if err != nil {
			return nil, nil, err
		}
		if len(enis) != 1 {
			if len(enis) == 0 {
				return nil, nil, fmt.Errorf("none network interfaces found during NIC query: %s", eniID)
			}
			return nil, nil, fmt.Errorf("multiple network interfaces found during NIC query: %s", eniID)
		}
		if zecCom.ToString(enis[0].Ipv6Cidr) == "" {
			return nil, nil, fmt.Errorf("IPv6 CIDR is empty for ENI: %s", eniID)
		}
		block, err := pkgUtils.ParseCidr(zecCom.ToString(enis[0].Ipv6Cidr))
		if err != nil {
			return nil, nil, err
		}
		return block, net.ParseIP(zecCom.ToString(enis[0].PrimaryIpv6)), nil
	}

	if ipv6Net, primaryV6, err = getENIIPv6Block(eniID); err == nil {
		log.Infof("Found existing IPv6 block on ENI %s: %s, primary %s", eniID, pkgUtils.ToStringIPNet(ipv6Net), primaryV6)
		return ipv6Net, primaryV6, nil
	}

	werr := wait.ExponentialBackoff(backoff.BackOff(backoff.APIWriteOps), func() (bool, error) {
		_, err = e.ec2Client.AssignNetworkInterfaceIpv6(eniID, e.ipv6Billing)
		if err == nil {
			return true, nil
		}
		errCodes := &apiErr.OpenApiErrCodeChain{}
		if errCodes.WithErrCodes(apiErr.OPERATION_DENIED_NIC_EXIST_IPV6).ErrChainEqual(err) {
			log.Warnf("AssignNetworkInterfaceIpv6 %s occur err: %s, ignore", eniID, apiErr.ErrCode(err))
			return true, nil
		}
		if errCodes.WithPublicErrCodes().WithErrCodes(apiErr.INVALID_NIC_STATUS, apiErr.INVALID_SUBNET_IPSTACK_NOT_SUPPORT,
			apiErr.OPERATION_FAILED_RESOURCE_NOT_FOUND, apiErr.OPERATION_DENIED_INTERNET_CHARGE_TYPE_NOT_SUPPORT,
			apiErr.OperationDeniedFlowPackageNotSupportedHourPeriod).ErrChainEqual(err) {
			return false, err
		}
		return false, nil
	})
	if err = apiErr.BackoffErrWrapper(werr, err); err != nil {
		// %w: receiveRes refreshes the instance limit and wakes GC on
		// INVALID_SUBNET_IPSTACK_NOT_SUPPORT, which AssignNetworkInterfaceIpv6
		// is the only source of. It asks by error code (errors.As) and the
		// error reaches it through initENI's epilogue, so a %v anywhere on that
		// path makes the check unreachable.
		return nil, nil, fmt.Errorf("failed to assign ipv6 address on ENI %s, err: %w", eniID, err)
	}

	ipv6Net, primaryV6, err = getENIIPv6Block(eniID)
	if err != nil {
		return nil, nil, err
	}

	log.Infof("Successfully assigned an IPv6 /96 CIDR block on ENI %s: %s, primary %s",
		eniID, pkgUtils.ToStringIPNet(ipv6Net), primaryV6)
	return ipv6Net, primaryV6, nil
}

// GetInstanceLimit returns the instance's vNIC and per-ENI IP quotas.
func (e *ZecAPIImpl) GetInstanceLimit() (*InstanceLimits, error) {
	var resp *zecApi.DescribeInstancesResponse
	var err error
	defer func() {
		if err != nil {
			fmtErr := fmt.Sprintf("get ip/eni quota for instance failed, %v", err)
			_ = tracing.RecordNodeEvent(v1.EventTypeWarning, tracing.EventGetInstanceQuotaFailed, fmtErr)
		}
	}()
	log.Info("Waiting to get the maximum number of ip on an eni")
	werr := wait.ExponentialBackoff(backoff.BackOff(backoff.APIFastRetry), func() (bool, error) {
		resp, err = e.ec2Client.DescribeInstances([]string{e.GetInstanceId()})
		if err != nil {
			return false, nil
		}
		if resp == nil || resp.Response == nil || len(resp.Response.DataSet) == 0 {
			return false, fmt.Errorf("describe instance failed, no result")
		}
		return true, nil
	})

	if err = apiErr.BackoffErrWrapper(werr, err); err != nil {
		log.Errorf("Get instance limits failed: %s", err.Error())
		return nil, err
	}
	instance := resp.Response.DataSet[0]

	attr, err := e.limits.GetLimit(context.Background(),
		zecCom.ToString(instance.InstanceType), zecCom.ToInteger(instance.Cpu))
	if err != nil {
		log.Errorf("Resolve instance networking limits failed: %v", err)
		return nil, err
	}

	limit := &InstanceLimits{InstanceLimitsAttr: attr}

	// Only the quota fields are known here: ENIManaged and ENIUnmanaged are
	// filled in later by the limit manager, and printing their zero values
	// alongside real ones read as "this node has created 0 ENIs" long after it
	// had.
	log.WithFields(zlog.Fields{"InstanceID": e.GetInstanceId(), "RequestID": resp.Response.RequestId}).
		Infof("Instance quotas: %s", attr.String())
	return limit, nil
}

// cleanUpLeakedENIs deletes interfaces this daemon created that are attached to
// nothing and have been untouched for eniLeakedTime -- what a create that
// failed between CreateNetworkInterface and AttachNetworkInterface leaves
// behind.
//
// Run hourly from New.
func (e *ZecAPIImpl) cleanUpLeakedENIs() {
	// Spread the sweep over the next five minutes. Nodes whose daemons started
	// together -- a rolled-out DaemonSet -- otherwise reach this on the same
	// tick and aim one burst of describes at the OpenAPI, on the budget the
	// allocation calls also draw from.
	back := time.Duration(rand.Intn(300)) * time.Second
	time.Sleep(back)

	log.Debug("Checking for leaked ENIs.")
	leakedENIs, err := e.getLeakedENIs()

	if err != nil {
		log.Errorf("Unable to get leaked ENI: %v", err)
		return
	}
	for _, eni := range leakedENIs {
		err := e.deleteENI(eni)
		if err != nil {
			log.Warnf("Failed to clean up leaked ENI %s: %v", eni, err)
		} else {
			log.Infof("Cleaned up leaked ENI %s", eni)
		}
	}
}

func (e *ZecAPIImpl) describeNetworkInterfacesWithPage(pageNumber int, status string, eniIDs []string) (*zecApi.DescribeNetworkInterfacesResponseParams, error) {
	req := zecApi.NewDescribeNetworkInterfacesRequest()
	req.PageNum = zecCom.Integer(pageNumber)
	req.PageSize = zecCom.Integer(maxPageSize)
	req.Status = zecCom.String(status)
	req.NicIds = eniIDs
	req.RegionId = zecCom.String(e.GetRegionId())
	if status == ENIStatusInuse {
		req.InstanceId = zecCom.String(e.GetInstanceId())
	}
	var err error
	var resp *zecApi.DescribeNetworkInterfacesResponse
	werr := wait.ExponentialBackoff(backoff.BackOff(backoff.APIFastRetry), func() (bool, error) {
		resp, err = e.ec2Client.DescribeNetworkInterfaceAttributes(req)
		return err == nil, nil
	})
	// resp stays nil when every attempt failed, so the error is checked before
	// anything reads it. One caller runs on the reconcile goroutine, where
	// HandleCrash re-panics, so a nil dereference here takes the whole daemon
	// down rather than failing that one reconcile.
	if err = apiErr.BackoffErrWrapper(werr, err); err != nil {
		return nil, err
	}
	if resp == nil {
		return nil, fmt.Errorf("describe network interfaces returned no response")
	}
	return resp.Response, nil
}

// getNetworkInterfacesByDescribe pages through DescribeNetworkInterfaces, then
// filters client-side: by NIC type when eniType names one, and down to the
// interfaces this daemon created when isCniCreate.
func (e *ZecAPIImpl) getNetworkInterfacesByDescribe(status string, eniType string, eniIDs []string, isCniCreate bool) ([]*zecApi.NicInfo, error) {
	var result []*zecApi.NicInfo
	pages := 1
	first := true
	for i := 1; i <= pages; i++ {
		resp, err := e.describeNetworkInterfacesWithPage(i, status, eniIDs)
		if err != nil {
			log.Errorf("describeNetworkInterfacesWithPage failed: %s", err.Error())
			return result, err
		}
		if zecCom.ToInteger(resp.TotalCount) == 0 {
			return result, nil
		}
		result = append(result, resp.DataSet...)
		if first {
			pages = zecCom.ToInteger(resp.TotalCount) / maxPageSize
			if zecCom.ToInteger(resp.TotalCount)%maxPageSize != 0 {
				pages += 1
			}
			first = false
			log.Debugf("Pages: %d", pages)
		}
	}
	if strings.EqualFold(eniType, ENITypePrimary) || strings.EqualFold(eniType, ENITypeSecondary) {
		filtered := result[:0]
		for _, item := range result {
			if strings.EqualFold(zecCom.ToString(item.NicType), eniType) {
				filtered = append(filtered, item)
			}
		}
		result = filtered
	}
	if isCniCreate {
		filtered := result[:0]
		for _, item := range result {
			if e.isOwnENI(item) {
				filtered = append(filtered, item)
			}
		}
		result = filtered
	}
	return result, nil
}

// getUnAttachedENIs returns the ENIs this daemon created that are not attached
// to any instance.
func (e *ZecAPIImpl) getUnAttachedENIs() ([]*zecApi.NicInfo, error) {
	return e.getNetworkInterfacesByDescribe(ENIStatusAvailable, ENITypeSecondary, nil, true)
}

func (e *ZecAPIImpl) getLeakedENIs() ([]string, error) {
	enis, err := e.getUnAttachedENIs()
	if err != nil {
		return nil, fmt.Errorf("get unAattached ENIs failed: %s", err.Error())
	}

	var leaked []string
	for _, eni := range enis {
		updateTime, err := time.Parse(time.RFC3339, zecCom.ToString(eni.UpdateTime))
		if err != nil {
			log.Errorf("parse eni create time error: %s", err.Error())
			continue
		}
		if time.Since(updateTime) > eniLeakedTime {
			leaked = append(leaked, zecCom.ToString(eni.NicId))
		}
	}
	return leaked, nil
}

// GetENI builds an ENI carrying the subnet gateway and CIDR the metadata
// service reports for a MAC. It reads no addresses; the caller fills those in
// from the platform's describe.
func (e *ZecAPIImpl) GetENI(eniId, subnetId, mac string) (*zecTypes.ENI, error) {
	ctx := context.Background()
	eniMac, err := net.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("get eni mac failed: %s", err.Error())
	}

	v4Gateway, err := e.metadataSvc.GetENIIPv4Gateway(ctx, mac)
	if err != nil {
		return nil, fmt.Errorf("get subnet id failed: %s", err.Error())
	}

	v4Cidr, err := e.metadataSvc.GetENISubnetCIDR(ctx, mac)
	if err != nil {
		return nil, fmt.Errorf("get subnet cidr failed: %s", err.Error())
	}

	// An IPv4-only subnet has no IPv6 tree in the metadata service:
	// subnet-ipv6-cidr-block answers 404. Reading it regardless makes every ENI
	// fail to initialize on an IPv4 cluster, and the pool surfaces that only as
	// being unable to create a resource -- nowhere near the cause.
	var (
		v6Gateway net.IP
		v6Cidr    *net.IPNet
	)
	if e.IPStack.EnableIPv6() {
		// A 404 on either is tolerated rather than fatal. Both keys describe the
		// interface's subnet rather than the interface, and GetENI runs for
		// every attached interface before the managed filter, so an error here
		// would take down daemon startup and every GC cycle over an interface
		// the CNI does not manage -- including one an operator attached in an
		// IPv4-only subnet.
		v6Gateway, err = e.metadataSvc.GetENIIPv6Gateway(ctx, mac)
		if err != nil {
			if !isMetadataNotFound(err) {
				return nil, fmt.Errorf("get eni ipv6 gateway failed: %s", err.Error())
			}
			log.Infof("Eni %s has no ipv6 gateway in metadata; it cannot serve dual-stack pods", mac)
			v6Gateway = nil
		}

		v6Cidr, err = e.metadataSvc.GetENISubnetIPv6CIDR(ctx, mac)
		if err != nil {
			if !isMetadataNotFound(err) {
				return nil, fmt.Errorf("get subnet ipv6 cidr failed: %s", err.Error())
			}
			log.Infof("Eni %s is in a subnet with no ipv6 cidr; it cannot serve dual-stack pods", mac)
			v6Cidr = nil
		}
	}

	eni := &zecTypes.ENI{
		ID:  eniId,
		Mac: eniMac,
		Subnet: zecTypes.Subnet{
			ID: subnetId,
			Gateway: &zecTypes.IPSet{
				IPv4: v4Gateway,
				IPv6: v6Gateway,
			},
			// Not published in metadata, and not needed: ToPb sends a fixed
			// gateway MAC to the plugin (types/pool.go), and the container
			// reaches the gateway through a permanent neighbor entry rather
			// than by resolving it.
			GatewayMac: nil,
			CIDR: &zecTypes.IPNetSet{
				IPv4: v4Cidr,
				IPv6: v6Cidr,
			},
		},
	}
	return eni, nil
}

func New(apiClient ec2.EC2, ipStack types.IPStack, subnetMgr SubnetManager, instanceMetadata InstanceMetadataGetter,
	ipv6Billing ec2.IPv6Billing, manageUntaggedENI bool) (*ZecAPIImpl, error) {

	impl := &ZecAPIImpl{
		IPStack:                ipStack,
		ipv6Billing:            ipv6Billing,
		manageUntaggedENI:      manageUntaggedENI,
		metadataSvc:            metadata.NewEC2MetadataWrapper(metadata.New()),
		ec2Client:              apiClient,
		InstanceMetadataGetter: instanceMetadata,
		subnetMgr:              subnetMgr,
		limits:                 NewLimitProvider(),
		// Stamped on every ENI this daemon creates and used to recognize them
		// again -- see utils.CniENITags.
		tags: utils.CniENITags(instanceMetadata.GetInstanceId()),
	}

	go wait.Forever(impl.cleanUpLeakedENIs, time.Hour)

	log.Infof("zenlayer api impl created")
	return impl, nil
}
