// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0

package controlplane

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8stypes "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	zecCom "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/common"
	zecApi "github.com/zenlayer/zenlayercloud-sdk-go/zenlayercloud/zec20250901"

	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/apis/network/v1alpha1"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/factory"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/daemon/types"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/providerid"
	"github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/tracing"
	apiErr "github.com/zenlayer/zenlayer-vpc-cni-k8s/pkg/zapi/errors"
)

const (
	requeueUnverified = 30 * time.Second
	requeueShort      = 15 * time.Second
	// requeueResync re-reads the platform even when nothing in the cluster
	// changed, so an address or interface changed behind the controlplane's
	// back is noticed.
	requeueResync = 5 * time.Minute
	// reverifyPeriod: a verified identity is checked against the platform
	// again this often, so a deleted instance whose Node lingers is noticed.
	reverifyPeriod  = time.Hour
	limitTTL        = 10 * time.Minute
	leakSweepPeriod = time.Hour
	subnetStatAging = 10 * time.Minute
	v6RetryPeriod   = 30 * time.Minute
	untaggedPause   = time.Hour
	// createBackoff spaces creates after a failed one, doubling per failure
	// up to createBackoffMax: a refusal such as an attach the instance never
	// accepts would otherwise churn an interface every pass, indefinitely.
	createBackoff    = time.Minute
	createBackoffMax = 30 * time.Minute
	// allocFailHold keeps a failure reported across a pass that asked for
	// nothing: a daemon whose wait timed out drops its request until the pool
	// retries a couple of minutes later, and that lull is not a recovery.
	allocFailHold = 5 * time.Minute
	// subnetFullHold is how long a subnet the platform called full is
	// skipped. The cached counts refresh only every subnetStatAging and lag
	// behind other nodes' changes, so the refusal is the better evidence until
	// it is this old.
	subnetFullHold = 2 * time.Minute
	// requeueNoSubnet spaces passes whose only trouble is that no configured
	// subnet has room: nothing a node does frees an address in it.
	requeueNoSubnet = time.Minute
	// warnRepeatQuiet is how long a node's Warning reason is not reported
	// again after it was; the condition still shows it throughout.
	warnRepeatQuiet = 10 * time.Minute
	// minPassInterval spaces one node's passes: each spec write starts one,
	// and a node rewriting its spec in a loop must not spend the OpenAPI
	// budget every node shares.
	minPassInterval    = 2 * time.Second
	reasonQuotaFull    = "ENIQuotaReached"
	reasonNoSubnet     = "NoAvailableSubnet"
	reasonAllocError   = "AllocationError"
	reasonNoIPv6Prefix = "IPv6PrefixRefused"
	reasonNoPodENI     = "NoPodInterface"
)

// nodePlatform is what reconciling one node needs from the platform. The
// production implementation is factory.ZecAPIImpl built for the node's instance.
type nodePlatform interface {
	DescribeAttachedNICs() ([]*zecApi.NicInfo, error)
	IsManagedENI(nic *zecApi.NicInfo) bool
	IsOwnENI(nic *zecApi.NicInfo) bool
	GetInstanceLimit() (*factory.InstanceLimits, error)
	AssignIPv4(eniID string, n int) ([]net.IP, error)
	UnassignIPv4(eniID string, ips []net.IP) error
	CreateAndAttachENI(subnetID, securityGroupID string, ipCnt int) (*zecApi.NicInfo, error)
	FreeENI(eniID string) error
	GetOrAssignIPv6Block(eniID, eniMac string) (*net.IPNet, net.IP, error)
}

// subnetPicker chooses the subnet for a new interface.
type subnetPicker interface {
	UpdateSubnetsStatus(options ...factory.UpdateSubnetsStatusOption) error
	GetPodSubnet(subnetID string) *factory.PodSubnet
	SelectSubnet(ipFamily types.IPStack, options ...factory.UpdateSubnetsStatusOption) *factory.PodSubnet
}

// Reconciler keeps every served Node's ZecNode and the node's interfaces in
// line with what the node's daemon asks for.
type Reconciler struct {
	client.Client
	Cfg      *Config
	Lookup   instanceLookup
	Subnets  subnetPicker
	Recorder record.EventRecorder
	// APIReader reads past the cache; gcZecNodes checks with it that a Node
	// is still gone right before deleting its ZecNode. Optional.
	APIReader client.Reader
	// NewNodePlatform builds the platform operations for a verified instance.
	NewNodePlatform func(id *identity) nodePlatform

	mu    sync.Mutex
	nodes map[string]*nodeState
	// fullUntil: subnets the platform last called full, and until when they
	// are skipped -- for every node, so a full subnet costs one refused call
	// per subnetFullHold rather than one per node per pass. Guarded by mu.
	fullUntil map[string]time.Time
	// spent: addresses new interfaces have taken out of each subnet's cached
	// count since it was last refreshed; see reserveAddrs. probing: subnets a
	// create is trying again after a refusal; see claimSubnet. Guarded by mu.
	spent   map[string]*subnetSpend
	probing map[string]bool
}

// subnetSpend is what has been taken out of one cached count. refreshed is
// when that count was read: a newer refresh starts it over.
type subnetSpend struct {
	refreshed time.Time
	used      int
}

// nodeState is what the controlplane remembers about a node between passes.
type nodeState struct {
	// verifiedFor is the providerID and claim the identity was verified
	// against; either changing forces a new verification.
	verifiedFor string
	id          *identity
	api         nodePlatform
	limits      factory.InstanceLimitsAttr
	limitsAt    time.Time
	verifiedAt  time.Time
	// passAt is when the last pass started; see minPassInterval.
	passAt time.Time
	// v6Refused: interfaces the platform refused an IPv6 prefix for, and
	// when. A refusal like a subnet without IPv6 does not change on its own,
	// so it is asked again only after v6RetryPeriod.
	v6Refused map[string]time.Time
	// untaggedAt: when a created interface last came back without this
	// plugin's tags; creating is paused for untaggedPause after it.
	untaggedAt time.Time
	// createFails counts creates failed in a row; createErr is the last one
	// and createAt is when creating may be tried again.
	createFails int
	createErr   error
	createAt    time.Time
	// lastWarn is the reason of the last Warning event, so a failure that
	// persists is reported once, not on every requeue; warnedAt is when each
	// reason was last reported, see warnRepeatQuiet.
	lastWarn string
	warnedAt map[string]time.Time
	// allocFailedAt is when a pass last ended in an allocation error, and
	// allocFailedWant the request it failed to meet; see allocFailHold.
	allocFailedAt   time.Time
	allocFailedWant int
}

// SetupWithManager registers the reconciler.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	r.nodes = map[string]*nodeState{}
	return ctrl.NewControllerManagedBy(mgr).
		Named("zecnode").
		For(&corev1.Node{}, builder.WithPredicates(nodeChanged())).
		// By name, not owner: a ZecNode outlives its Node (see ensureZecNode).
		Watches(&v1alpha1.ZecNode{}, handler.EnqueueRequestsFromMapFunc(
			func(_ context.Context, o client.Object) []reconcile.Request {
				return []reconcile.Request{{NamespacedName: k8stypes.NamespacedName{Name: o.GetName()}}}
			}), builder.WithPredicates(predicate.GenerationChangedPredicate{})).
		WithOptions(controller.Options{MaxConcurrentReconciles: r.Cfg.Workers}).
		Complete(r)
}

// nodeChanged ignores the Node status heartbeats, which arrive every few
// seconds per node and carry nothing this reconciler reads besides addresses.
func nodeChanged() predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			o, ok1 := e.ObjectOld.(*corev1.Node)
			n, ok2 := e.ObjectNew.(*corev1.Node)
			if !ok1 || !ok2 {
				return true
			}
			return o.Spec.ProviderID != n.Spec.ProviderID ||
				!mapsEqual(o.Labels, n.Labels) ||
				internalIP(o) != internalIP(n)
		},
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if bv, ok := b[k]; !ok || bv != v {
			return false
		}
	}
	return true
}

// warnOnce records a Warning event on the Node unless the same reason was the
// last one reported for it.
func (r *Reconciler) warnOnce(ns *nodeState, node *corev1.Node, reason, msg string) {
	key := reason
	if reason == reasonNoSubnet {
		// The same exhaustion, seen through the cached counts instead of a
		// refused call: passes alternate between the two while it lasts.
		key = tracing.EventInsufficientIpInSubnet
	}
	if ns.lastWarn == key {
		return
	}
	ns.lastWarn = key
	// Two failures can take turns while both last -- a full subnet and a full
	// quota, say -- and each turn would report the other again.
	if at, ok := ns.warnedAt[key]; ok && time.Since(at) < warnRepeatQuiet {
		return
	}
	if ns.warnedAt == nil {
		ns.warnedAt = map[string]time.Time{}
	}
	ns.warnedAt[key] = time.Now()
	r.Recorder.Event(node, corev1.EventTypeWarning, reason, tracing.TruncateMessage(msg))
}

func (r *Reconciler) forget(name string) {
	r.mu.Lock()
	delete(r.nodes, name)
	r.mu.Unlock()
}

func (r *Reconciler) state(name string) *nodeState {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.nodes == nil {
		r.nodes = map[string]*nodeState{}
	}
	st, ok := r.nodes[name]
	if !ok {
		st = &nodeState{}
		r.nodes[name] = st
	}
	return st
}

// Reconcile handles one Node.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	node := &corev1.Node{}
	if err := r.Get(ctx, req.NamespacedName, node); err != nil {
		if apierrors.IsNotFound(err) {
			// The ZecNode stays: it keeps the name's binding to its instance,
			// so a re-registered Node cannot claim another one (gcZecNodes
			// removes it once that no longer matters). No platform call: a
			// deleted Node object does not mean a deleted instance.
			r.forget(req.Name)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}
	if !node.DeletionTimestamp.IsZero() || !r.Cfg.Serves(node.Labels) {
		r.forget(req.Name)
		return ctrl.Result{}, nil
	}

	ns := r.state(node.Name)
	if wait := minPassInterval - time.Since(ns.passAt); wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, nil
	}
	ns.passAt = time.Now()

	zn, err := r.ensureZecNode(ctx, node)
	if err != nil {
		return ctrl.Result{}, err
	}
	status := zn.Status.DeepCopy()
	status.ObservedGeneration = zn.Generation
	status.IPStack = r.Cfg.IPStack

	id, verr := r.verify(ctx, ns, node, zn)
	if verr != nil {
		// A check that could not be made changes nothing: a node verified
		// before stays verified, so its daemon can still restart while the
		// platform is out of reach.
		if c := meta.FindStatusCondition(zn.Status.Conditions, v1alpha1.ConditionInstanceVerified); verr.inconclusive &&
			c != nil && c.Status == metav1.ConditionTrue {
			log.Warnf("Node %s: verification deferred: %v", node.Name, verr)
			return ctrl.Result{RequeueAfter: requeueUnverified}, nil
		}
		r.setCondition(status, zn, v1alpha1.ConditionInstanceVerified, false, verr.reason, verr.msg)
		r.setCondition(status, zn, v1alpha1.ConditionReady, false, verr.reason, verr.msg)
		if verr.warn {
			r.warnOnce(ns, node, verr.reason, verr.msg)
		}
		return ctrl.Result{RequeueAfter: requeueUnverified}, r.writeStatus(ctx, zn, status)
	}
	r.setCondition(status, zn, v1alpha1.ConditionInstanceVerified, true, v1alpha1.ReasonVerified,
		"instance "+id.instanceID)
	status.Node = v1alpha1.NodeIdentity{InstanceID: id.instanceID, ZoneID: id.zoneID}
	sc := r.Cfg.ScalerFor(node.Labels)
	status.Scaler = v1alpha1.Scaler{TargetMin: sc.TargetMin, Target: sc.Target}

	res := r.reconcileAddresses(ns, zn, status)
	if errors.Is(res.err, errInstanceGone) {
		// Verified again on the next pass; the binding stands until that
		// answers, so no other node can take the instance meanwhile.
		ns.verifiedAt = time.Time{}
	}
	r.reportAlloc(ns, node, status, zn, res)
	if err := r.writeStatus(ctx, zn, status); err != nil {
		return ctrl.Result{}, err
	}

	// A verification still owed (providerID not written yet, an instance that
	// showed no primary interface) is retried soon, not at the next resync.
	if res.waitSubnet && !ns.verifiedAt.IsZero() {
		return ctrl.Result{RequeueAfter: requeueNoSubnet}, nil
	}
	if !res.retryAt.IsZero() && !ns.verifiedAt.IsZero() {
		return ctrl.Result{RequeueAfter: min(max(time.Until(res.retryAt), requeueShort), requeueResync)}, nil
	}
	if res.short || ns.verifiedAt.IsZero() || (res.err != nil && !res.settled) {
		return ctrl.Result{RequeueAfter: requeueShort}, nil
	}
	return ctrl.Result{RequeueAfter: requeueResync}, nil
}

// reportAlloc turns a pass's result into conditions and events. A pass that
// asked for nothing within allocFailHold of a failure leaves the failure
// standing, so a waiting node neither reads Satisfied nor re-reports the same
// failure each time its request comes back.
func (r *Reconciler) reportAlloc(ns *nodeState, node *corev1.Node, status *v1alpha1.ZecNodeStatus,
	zn *v1alpha1.ZecNode, res addrResult) {
	switch {
	case res.deferred:
	case res.err != nil:
		ns.allocFailedAt, ns.allocFailedWant = time.Now(), zn.Spec.IPv4.Requested
		r.allocConditions(status, zn, res)
		r.warnOnce(ns, node, res.reason, res.err.Error())
	case res.idle && !ns.allocFailedAt.IsZero() && time.Since(ns.allocFailedAt) < allocFailHold &&
		zn.Spec.IPv4.Requested < ns.allocFailedWant:
		// Idle only because the request dropped since the failure (a daemon
		// whose wait ran out). A request that held and is now met is a
		// recovery: the default branch clears it.
		r.setCondition(status, zn, v1alpha1.ConditionReady, true, "Ready", "")
	default:
		ns.lastWarn, ns.allocFailedAt = "", time.Time{}
		r.allocConditions(status, zn, res)
	}
}

// allocConditions sets IPAllocationFailed and Ready from a pass's result.
// Ready is about the node -- verified, its interfaces known -- so a failure to
// allocate, often cluster-wide (a bad credential, OpenAPI out of reach), shows
// in IPAllocationFailed and leaves Ready alone once the interfaces are known.
func (r *Reconciler) allocConditions(status *v1alpha1.ZecNodeStatus, zn *v1alpha1.ZecNode, res addrResult) {
	if res.err == nil {
		r.setCondition(status, zn, v1alpha1.ConditionIPAllocationFailed, false, "Satisfied", "")
		r.setCondition(status, zn, v1alpha1.ConditionReady, true, "Ready", "")
		return
	}
	r.setCondition(status, zn, v1alpha1.ConditionIPAllocationFailed, true, res.reason, res.err.Error())
	// No primary interface means the instance is gone or stopped: the list
	// carried over from an earlier pass is stale, and that is the node's own.
	if len(status.ENIs) > 0 && !errors.Is(res.err, errInstanceGone) {
		r.setCondition(status, zn, v1alpha1.ConditionReady, true, "Ready", "")
	} else {
		r.setCondition(status, zn, v1alpha1.ConditionReady, false, res.reason, res.err.Error())
	}
}

// ensureZecNode returns the node's ZecNode, creating it with an empty spec.
//
// It carries no owner reference to the Node: deleting the Node -- which the
// node itself may do -- must not delete the record of which instance the name
// was verified as. One made by an older controlplane loses its reference here.
func (r *Reconciler) ensureZecNode(ctx context.Context, node *corev1.Node) (*v1alpha1.ZecNode, error) {
	zn := &v1alpha1.ZecNode{}
	err := r.Get(ctx, client.ObjectKey{Name: node.Name}, zn)
	if err == nil {
		if len(zn.OwnerReferences) > 0 {
			patch := client.RawPatch(k8stypes.MergePatchType, []byte(`{"metadata":{"ownerReferences":null}}`))
			if err := r.Patch(ctx, zn, patch); err != nil {
				return nil, fmt.Errorf("drop the owner reference of ZecNode %s: %w", node.Name, err)
			}
		}
		return zn, nil
	}
	if !apierrors.IsNotFound(err) {
		return nil, err
	}
	zn = &v1alpha1.ZecNode{ObjectMeta: metav1.ObjectMeta{Name: node.Name}}
	if err := r.Create(ctx, zn); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return zn, r.Get(ctx, client.ObjectKey{Name: node.Name}, zn)
		}
		return nil, fmt.Errorf("create ZecNode %s: %w", node.Name, err)
	}
	// The object Create filled in, not a cache read: the informer has not seen
	// it yet.
	return zn, nil
}

// verify returns the node's identity. It is verified again when the
// providerID, the daemon's claim or the InternalIP changes, and at least every
// reverifyPeriod; providerID is written when it was found by address. Binding
// is exclusive: see bindLocked.
func (r *Reconciler) verify(ctx context.Context, ns *nodeState, node *corev1.Node, zn *v1alpha1.ZecNode) (*identity, *verifyError) {
	c := zn.Spec.Identity
	key := strings.Join([]string{node.Spec.ProviderID, c.InstanceID, c.PrimaryMAC, c.PrimaryIP, internalIP(node)}, "|")
	if ns.id != nil && ns.verifiedFor == key && time.Since(ns.verifiedAt) < reverifyPeriod {
		return ns.id, nil
	}
	id, verr := verifyIdentity(r.Lookup, r.Cfg, node, c)
	if verr == nil {
		verr = r.keepsName(zn, id)
	}
	if verr == nil {
		verr = r.bind(ctx, ns, node, id, key)
	}
	if verr != nil {
		// Only a conclusive answer ends a binding. A platform that did not
		// answer keeps it, and the next pass asks again.
		if verr.inconclusive && ns.id != nil && ns.verifiedFor == key {
			log.Warnf("Node %s: re-verification deferred, keeping instance %s: %v", node.Name, ns.id.instanceID, verr)
			return ns.id, nil
		}
		r.unbind(ns)
		return nil, verr
	}
	if id.fromLookup {
		if err := r.writeProviderID(ctx, node, id.regionID, id.instanceID); err != nil {
			// The identity is verified; only the record of it failed (often
			// an optimistic-lock conflict). Verify and write again next pass.
			log.Warnf("Node %s: write providerID: %v; retrying", node.Name, err)
			ns.verifiedAt = time.Time{}
		}
		// The Node update re-queues this node; the next pass verifies by
		// providerID, which is the form every later pass will see.
	}
	return id, nil
}

// keepsName refuses a node name verified before as another instance that
// still exists: a node that deletes and re-registers its Node object must not
// come back as some other machine. Once that instance is gone, the name is
// free -- a rebuilt machine reusing a hostname needs no one's help.
func (r *Reconciler) keepsName(zn *v1alpha1.ZecNode, id *identity) *verifyError {
	prev := zn.Status.Node.InstanceID
	if prev == "" || prev == id.instanceID {
		return nil
	}
	exists, verr := instanceExists(r.Lookup, prev)
	if verr != nil {
		return verr
	}
	if exists {
		return failVerify(v1alpha1.ReasonBoundToOtherInstance, true,
			"node name %s was verified as instance %s, which still exists, and now claims instance %s; "+
				"if the name really moved to another machine, clear the record: kubectl delete zecnode %s",
			zn.Name, prev, id.instanceID, zn.Name)
	}
	log.Infof("Node %s: instance %s it was bound to is gone; binding the name to %s", zn.Name, prev, id.instanceID)
	return nil
}

// bind records node as the one Node of id.instanceID, refusing if another
// Node already is: by this controlplane's verification, or -- after a restart,
// before that Node's own pass -- by its ZecNode status. A providerID alone
// does not count, since any node can set its own; counting it would let one
// node unbind another by copying its providerID. Check and record happen under
// one lock, so two passes cannot both bind the same instance.
func (r *Reconciler) bind(ctx context.Context, ns *nodeState, node *corev1.Node, id *identity, key string) *verifyError {
	zns := &v1alpha1.ZecNodeList{}
	nodes := &corev1.NodeList{}
	err := r.List(ctx, zns)
	if err == nil {
		err = r.List(ctx, nodes)
	}
	if err != nil {
		e := failVerify(v1alpha1.ReasonInstanceAmbiguous, false, "list ZecNodes and Nodes: %v", err)
		e.inconclusive = true
		return e
	}
	alive := map[string]bool{}
	for i := range nodes.Items {
		alive[nodes.Items[i].Name] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for name, st := range r.nodes {
		if name != node.Name && st.id != nil && st.id.instanceID == id.instanceID {
			return failVerify(v1alpha1.ReasonInstanceAmbiguous, true,
				"instance %s is already bound to node %s", id.instanceID, name)
		}
	}
	for i := range zns.Items {
		o := &zns.Items[i]
		// A ZecNode whose Node is gone keeps its own name's binding, but
		// reserves the instance for nobody: the machine may have been renamed.
		if o.Name == node.Name || o.Status.Node.InstanceID != id.instanceID || !alive[o.Name] {
			continue
		}
		if st, known := r.nodes[o.Name]; known && st.id != nil {
			continue // judged by its in-memory binding above
		}
		if c := meta.FindStatusCondition(o.Status.Conditions, v1alpha1.ConditionInstanceVerified); c != nil && c.Status == metav1.ConditionTrue {
			return failVerify(v1alpha1.ReasonInstanceAmbiguous, true,
				"instance %s is already bound to node %s", id.instanceID, o.Name)
		}
	}
	ns.id, ns.verifiedFor, ns.verifiedAt = id, key, time.Now()
	ns.api = r.NewNodePlatform(id)
	ns.limitsAt = time.Time{}
	return nil
}

func (r *Reconciler) unbind(ns *nodeState) {
	r.mu.Lock()
	ns.id, ns.api, ns.verifiedFor = nil, nil, ""
	r.mu.Unlock()
}

// gcZecNodes deletes the ZecNodes whose Node is gone once their binding no
// longer protects anything: never verified, the instance is gone, or the
// instance has since been verified under another, live name (a renamed
// machine). A ZecNode left by kubectl delete node, its instance still there,
// is kept for the Node to come back to. Leader only, no Node in the way.
func (r *Reconciler) gcZecNodes(ctx context.Context) {
	zns := &v1alpha1.ZecNodeList{}
	nodes := &corev1.NodeList{}
	if err := r.List(ctx, zns); err != nil {
		log.Warnf("List ZecNodes for cleanup: %v", err)
		return
	}
	if err := r.List(ctx, nodes); err != nil {
		log.Warnf("List Nodes for ZecNode cleanup: %v", err)
		return
	}
	alive := map[string]bool{}
	for i := range nodes.Items {
		alive[nodes.Items[i].Name] = true
	}
	verifiedLive := map[string]bool{}
	for i := range zns.Items {
		o := &zns.Items[i]
		if c := meta.FindStatusCondition(o.Status.Conditions, v1alpha1.ConditionInstanceVerified); alive[o.Name] &&
			c != nil && c.Status == metav1.ConditionTrue && o.Status.Node.InstanceID != "" {
			verifiedLive[o.Status.Node.InstanceID] = true
		}
	}
	for i := range zns.Items {
		o := &zns.Items[i]
		if alive[o.Name] {
			continue
		}
		inst := o.Status.Node.InstanceID
		why := ""
		switch {
		case inst == "":
			why = "never verified"
		case verifiedLive[inst]:
			why = fmt.Sprintf("instance %s now serves another node", inst)
		default:
			exists, verr := instanceExists(r.Lookup, inst)
			if verr != nil {
				log.Warnf("ZecNode %s: %v; kept for now", o.Name, verr)
				continue
			}
			if exists {
				continue
			}
			why = fmt.Sprintf("instance %s is gone", inst)
		}
		// A Node that re-registers under the same name reuses this very
		// object (ensureZecNode), and the Node list above is a cache read
		// taken before the platform calls: look again, uncached, right before
		// deleting. The UID precondition covers the other case, a ZecNode
		// deleted and recreated meanwhile.
		if r.APIReader != nil {
			if err := r.APIReader.Get(ctx, client.ObjectKey{Name: o.Name}, &corev1.Node{}); err == nil {
				continue
			} else if !apierrors.IsNotFound(err) {
				log.Warnf("ZecNode %s: recheck its Node: %v; kept for now", o.Name, err)
				continue
			}
		}
		if err := r.Delete(ctx, o, client.Preconditions{UID: &o.UID}); err != nil && !apierrors.IsNotFound(err) {
			log.Warnf("Delete ZecNode %s (%s): %v", o.Name, why, err)
			continue
		}
		log.Infof("Deleted ZecNode %s: its Node is gone and %s", o.Name, why)
	}
}

// writeProviderID sets an empty providerID, in the cloud controller manager's
// form (see package providerid). The apiserver refuses any change
// once it is set; the optimistic lock makes a concurrent write by someone else
// fail here instead of being overwritten.
//
// A Node just registered is still being updated by its kubelet, so a conflict
// is normal then: the write is retried against a fresh read.
func (r *Reconciler) writeProviderID(ctx context.Context, node *corev1.Node, regionID, instanceID string) error {
	// Written once and for good: never without the region the format needs.
	if regionID == "" {
		return fmt.Errorf("the platform reports no region for instance %s", instanceID)
	}
	cur := node
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if cur == nil {
			cur = &corev1.Node{}
			// The live object: the cache may not have the kubelet's write yet.
			var rd client.Reader = r.Client
			if r.APIReader != nil {
				rd = r.APIReader
			}
			if err := rd.Get(ctx, client.ObjectKeyFromObject(node), cur); err != nil {
				return err
			}
			if cur.Spec.ProviderID != "" {
				return nil // someone else set it; the next pass checks it
			}
		}
		patched := cur.DeepCopy()
		patched.Spec.ProviderID = providerid.Format(regionID, instanceID)
		err := r.Patch(ctx, patched, client.MergeFromWithOptions(cur, client.MergeFromWithOptimisticLock{}))
		cur = nil
		return err
	})
}

func (r *Reconciler) setCondition(st *v1alpha1.ZecNodeStatus, zn *v1alpha1.ZecNode, typ string, ok bool, reason, msg string) {
	s := metav1.ConditionFalse
	if ok {
		s = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&st.Conditions, metav1.Condition{
		Type: typ, Status: s, Reason: reason, Message: msg, ObservedGeneration: zn.Generation,
	})
}

// writeStatus replaces the whole status. On a conflict it re-reads the object
// and writes the same status again: status is computed from the platform and
// the spec, and the spec part (observedGeneration) is the one thing a newer
// object could change -- the next pass, which that change queues, fixes it.
func (r *Reconciler) writeStatus(ctx context.Context, zn *v1alpha1.ZecNode, st *v1alpha1.ZecNodeStatus) error {
	// First with the object in hand: right after ensureZecNode created it the
	// informer may not have it yet, so a cache read would fail NotFound.
	cur := zn.DeepCopy()
	first := true
	return retry.RetryOnConflict(retry.DefaultRetry, func() error {
		if !first {
			cur = &v1alpha1.ZecNode{}
			if err := r.Get(ctx, client.ObjectKeyFromObject(zn), cur); err != nil {
				return err
			}
		}
		first = false
		cur.Status = *st.DeepCopy()
		return r.Status().Update(ctx, cur)
	})
}

// addrResult is the outcome of one address pass.
type addrResult struct {
	err    error
	reason string
	// short: the node holds fewer addresses than it asked for and nothing
	// failed outright (e.g. a new interface is being created); look again soon.
	short bool
	// deferred: the fill waits for the daemon to settle a release; the outcome
	// of the fill is unknown, so conditions are left as they were.
	deferred bool
	// settled: err is a refusal that will not change soon (a subnet without
	// IPv6); it is reported, but the next look waits for the regular resync.
	settled bool
	// idle: the node asked for no more than it holds, so the pass allocated
	// nothing and proves nothing about whether allocating works.
	idle bool
	// waitSubnet: the only failure is that no configured subnet has room;
	// look again after requeueNoSubnet rather than requeueShort.
	waitSubnet bool
	// retryAt: the only failure is a create pause or backoff ending then;
	// the next look waits for it (up to requeueResync).
	retryAt time.Time
}

// nicView is an attached interface as this pass sees it.
type nicView struct {
	nic       *zecApi.NicInfo
	primary   bool
	own       bool
	secondary []string
	// excluded: in a subnet outside cfg.Subnets. Pod addresses come only from
	// the configured pod subnets; an own one drains and is then freed.
	excluded bool
}

func (v *nicView) id() string     { return zecCom.ToString(v.nic.NicId) }
func (v *nicView) mac() string    { return zecCom.ToString(v.nic.MacAddress) }
func (v *nicView) subnet() string { return zecCom.ToString(v.nic.SubnetId) }

// reconcileAddresses brings the node's addresses to what its spec asks for and
// fills status.capacity and status.enis from what the platform then reports.
//
// The order matters: release first, so a full interface can take new
// addresses; fill existing interfaces before creating one; and free an empty
// interface only after filling, so one about to be reused is not torn down.
func (r *Reconciler) reconcileAddresses(ns *nodeState, zn *v1alpha1.ZecNode, st *v1alpha1.ZecNodeStatus) addrResult {
	api := ns.api
	if err := r.refreshLimits(ns); err != nil {
		return addrResult{err: err, reason: reasonFor(err)}
	}
	views, unmanaged, err := r.describe(api)
	if err != nil {
		return addrResult{err: err, reason: reasonFor(err)}
	}
	limits := factory.InstanceLimits{InstanceLimitsAttr: ns.limits}
	limits.ENIUnmanaged = unmanaged
	perENI := limits.IPv4MaxPerENI
	allocatable := limits.AllocatableIPs()

	// errs fail the pass; soft are refusals another interface or subnet may
	// get past, reported only if the node still ends up short; slow are
	// refusals that will not change soon.
	var errs, soft, slow []error
	releasing := map[string]bool{}
	for _, ip := range zn.Spec.IPv4.Releasing {
		releasing[ip] = true
	}

	// 1. Release what the daemon gave up. One address the platform will not
	// release (a public IP or load balancer on it) must not hold back the rest
	// of the interface's, so a refused batch is retried one by one.
	for _, v := range views {
		var drop []net.IP
		for _, ip := range v.secondary {
			if releasing[ip] {
				drop = append(drop, net.ParseIP(ip))
			}
		}
		if len(drop) == 0 {
			continue
		}
		gone := map[string]bool{}
		err := api.UnassignIPv4(v.id(), drop)
		switch {
		case err == nil:
			for _, ip := range drop {
				gone[ip.String()] = true
			}
		case len(drop) > 1 && refusedPerAddress(err):
			for _, ip := range drop {
				if err := api.UnassignIPv4(v.id(), []net.IP{ip}); err != nil {
					errs = append(errs, fmt.Errorf("release %s from %s: %w", ip, v.id(), err))
					continue
				}
				gone[ip.String()] = true
			}
		default:
			errs = append(errs, fmt.Errorf("release %v from %s: %w", drop, v.id(), err))
		}
		if len(gone) > 0 {
			log.Infof("Node %s: released %d address(es) from %s", zn.Name, len(gone), v.id())
		}
		v.secondary = slices.DeleteFunc(v.secondary, func(ip string) bool { return gone[ip] })
	}

	// 2. IPv6: every managed interface needs its /96 before its addresses can
	// be paired. One that cannot get it serves no pod on a dual node: it is
	// neither filled nor counted, and its share leaves allocatable.
	unusable := map[string]bool{}
	if types.IPStack(r.Cfg.IPStack).EnableIPv6() {
		for _, v := range views {
			if v.excluded {
				continue // serves no pod, so needs no prefix
			}
			if zecCom.ToString(v.nic.Ipv6Cidr) != "" {
				delete(ns.v6Refused, v.id())
				continue
			}
			if at, ok := ns.v6Refused[v.id()]; ok && time.Since(at) < v6RetryPeriod {
				unusable[v.id()] = true
				slow = append(slow, fmt.Errorf("%w: interface %s (asked again after %s)", errNoIPv6Prefix, v.id(), v6RetryPeriod))
				continue
			}
			if _, _, err := api.GetOrAssignIPv6Block(v.id(), v.mac()); err != nil {
				unusable[v.id()] = true
				if v6RefusalLasts(err) {
					if ns.v6Refused == nil {
						ns.v6Refused = map[string]time.Time{}
					}
					ns.v6Refused[v.id()] = time.Now()
					slow = append(slow, fmt.Errorf("%w: interface %s: %w", errNoIPv6Prefix, v.id(), err))
				} else {
					errs = append(errs, fmt.Errorf("assign ipv6 prefix to %s: %w", v.id(), err))
				}
			}
		}
	}
	// An excluded interface is neither filled nor counted, like one refused a
	// prefix; what pods still hold on it drains as they go.
	excluded := false
	for _, v := range views {
		if v.excluded {
			unusable[v.id()], excluded = true, true
		}
	}
	allocatable = max(0, allocatable-len(unusable)*max(0, perENI-1))
	if excluded && allocatable == 0 {
		slow = append(slow, fmt.Errorf("%w (outside the pod subnets, or refused an IPv6 prefix) "+
			"and the instance can take no other interface", errNoPodENI))
	}
	st.Capacity = v1alpha1.Capacity{
		ENIQuota: limits.ENIQuota, IPv4PerENI: perENI, ENIUnmanaged: unmanaged, Allocatable: allocatable,
	}

	// 3. Fill up to what the node asked for, never beyond what it can hold.
	// An address whose release failed is still on the interface but no longer
	// the node's: it does not count toward what the node holds.
	held := 0
	onPlatform := map[string]bool{}
	for _, v := range views {
		for _, ip := range v.secondary {
			onPlatform[ip] = true
			if !unusable[v.id()] && !releasing[ip] {
				held++
			}
		}
	}
	want := min(zn.Spec.IPv4.Requested, allocatable)
	need := want - held
	idle := need <= 0
	short := false
	// While the spec still lists an address the platform no longer has -- the
	// daemon has not settled the release -- fill nothing: the platform may
	// hand that address straight back, and it must be seen gone first.
	unsettled := false
	for ip := range releasing {
		if !onPlatform[ip] {
			unsettled = true
			break
		}
	}
	deferred := unsettled && need > 0
	if deferred {
		need, short = 0, true
	}
	exhausted := map[string]bool{}
	// busy: subnets another node's create holds the probe of; single: subnets
	// that refused a batch, to be asked once more for one address.
	busy, single := map[string]bool{}, map[string]bool{}
	ambiguous := false
	// blockedUntil: creating waits for a pause or backoff that ends then.
	var blockedUntil time.Time
	if need > 0 {
		for _, v := range views {
			if need <= 0 {
				break
			}
			if unusable[v.id()] || exhausted[v.subnet()] || r.heldFull(v.subnet()) {
				continue
			}
			room := max(0, perENI-1-len(v.secondary))
			if room == 0 {
				continue
			}
			n := min(room, need)
			got, err := api.AssignIPv4(v.id(), n)
			if err != nil && subnetFull(err) && n > 1 {
				// A batch the subnet cannot cover fails whole; the subnet is
				// full only if a single address is refused too.
				n = 1
				got, err = api.AssignIPv4(v.id(), n)
			}
			if err == nil {
				r.clearExpiredFull(v.subnet())
			}
			if err != nil {
				e := fmt.Errorf("assign %d address(es) on %s: %w", n, v.id(), err)
				switch {
				case interfaceFull(err):
					soft = append(soft, e) // try the next interface
					continue
				case subnetFull(err):
					exhausted[v.subnet()] = true
					r.markFull(v.subnet())
					soft = append(soft, e) // try an interface in another subnet
					continue
				}
				// Anything else -- no answer, a timeout, a flow limit -- could
				// also hit a new interface, and an unanswered assign may have
				// landed. Re-read on the next pass.
				errs = append(errs, e)
				ambiguous = true
				break
			}
			log.Infof("Node %s: assigned %v on %s", zn.Name, got, v.id())
			need -= len(got)
			room -= len(got)
			// After a refused batch, keep taking single addresses here while
			// the subnet gives them: making new interfaces for what this one
			// still has room for spends quota and addresses on primaries.
			for n == 1 && need > 0 && room > 0 {
				more, err := api.AssignIPv4(v.id(), 1)
				if err != nil {
					switch {
					case subnetFull(err):
						exhausted[v.subnet()] = true
						r.markFull(v.subnet())
					case !interfaceFull(err):
						// As above: an unanswered assign may have landed.
						errs = append(errs, fmt.Errorf("assign 1 address on %s: %w", v.id(), err))
						ambiguous = true
					}
					break
				}
				log.Infof("Node %s: assigned %v on %s", zn.Name, more, v.id())
				need -= len(more)
				room -= len(more)
			}
			if ambiguous {
				break
			}
		}
	}
	if need > 0 && !ambiguous {
		created := 0
		for _, v := range views {
			if !v.primary {
				created++
			}
		}
		for need > 0 && created < limits.ENIAvailable() {
			if !ns.untaggedAt.IsZero() && time.Since(ns.untaggedAt) < untaggedPause {
				errs = append(errs, fmt.Errorf("%w; creating paused until %s", errUntagged,
					ns.untaggedAt.Add(untaggedPause).Format(time.RFC3339)))
				blockedUntil = ns.untaggedAt.Add(untaggedPause)
				break
			}
			if ns.createFails > 0 && time.Now().Before(ns.createAt) {
				errs = append(errs, fmt.Errorf("%w; %d failure(s) in a row, next try at %s",
					ns.createErr, ns.createFails, ns.createAt.Format(time.RFC3339)))
				blockedUntil = ns.createAt
				break
			}
			subnetID, cached, refreshed, perr := r.pickSubnet(exhausted)
			if perr != nil {
				errs = append(errs, perr)
				break
			}
			if subnetID == "" {
				if len(busy) > 0 {
					// Another node's create holds the only subnet with room:
					// not exhaustion, so look again soon and report nothing.
					short = true
					break
				}
				errs = append(errs, errNoSubnet)
				break
			}
			ok, claimed := r.claimSubnet(subnetID)
			if !ok {
				exhausted[subnetID] = true // another node's create is trying it
				busy[subnetID] = true
				continue
			}
			release := func() {
				if claimed {
					r.releaseProbe(subnetID)
				}
			}
			n := min(need, perENI-1)
			if single[subnetID] {
				n = 1
			}
			if cached > 0 {
				// Take the addresses out of what the cached count leaves,
				// shared by every node's pass (reserveAddrs): a fill the subnet
				// cannot cover fails whole and deletes the interface; a smaller
				// one is topped up on a later pass.
				if n = r.reserveAddrs(subnetID, cached, refreshed, n); n == 0 {
					release()
					exhausted[subnetID] = true
					continue
				}
			}
			nic, err := api.CreateAndAttachENI(subnetID, r.Cfg.SecurityGroup, n+1)
			release()
			if err != nil && cached > 0 && leftNothing(err) {
				r.unreserveAddrs(subnetID, refreshed, n)
			}
			if err != nil && subnetFull(err) {
				soft = append(soft, fmt.Errorf("create interface in subnet %s: %w", subnetID, err))
				if n > 1 {
					// The batch failed whole; try once more with the smallest.
					single[subnetID] = true
					continue
				}
				// Only that subnet is out: try another, without backing off.
				exhausted[subnetID] = true
				r.markFull(subnetID)
				continue
			}
			if err == nil {
				r.clearFull(subnetID)
			}
			if err != nil {
				ns.createErr = fmt.Errorf("create interface in subnet %s: %w", subnetID, err)
				ns.createAt = time.Now().Add(min(createBackoff<<min(ns.createFails, 10), createBackoffMax))
				ns.createFails++
				errs = append(errs, ns.createErr)
				break
			}
			ns.createFails, ns.createErr = 0, nil
			if nic != nil && !api.IsOwnENI(nic) {
				// Created without this plugin's tags (a tag grant missing?): it
				// would never be counted or freed. Undo it and pause creating,
				// so a missing grant does not churn an interface every pass.
				ns.untaggedAt = time.Now()
				id := zecCom.ToString(nic.NicId)
				if err := api.FreeENI(id); err != nil {
					errs = append(errs, fmt.Errorf("free untagged interface %s (left attached; remove it by hand): %w", id, err))
				} else if cached > 0 {
					r.unreserveAddrs(subnetID, refreshed, n)
				}
				errs = append(errs, fmt.Errorf("%w: interface %s", errUntagged, id))
				break
			}
			if nic != nil {
				log.Infof("Node %s: created interface %s in subnet %s for %d address(es)",
					zn.Name, zecCom.ToString(nic.NicId), subnetID, n)
			}
			created++
			need -= n
			short = true
			if single[subnetID] {
				// The subnet takes one address at a time: the next pass fills
				// this interface (step 3) rather than another being made.
				break
			}
		}
		if need > 0 && created >= limits.ENIAvailable() && len(errs) == 0 {
			errs = append(errs, fmt.Errorf("%w: %d more address(es) wanted, all %d interface(s) in use",
				errQuotaFull, need, limits.ENIAvailable()+1))
		}
	}
	// Only "no subnet has room": nothing this node does frees an address.
	waitSubnet := len(errs) == 1 && errors.Is(errs[0], errNoSubnet)
	// The reason names what stopped the pass, not a refusal it worked around.
	var cause error
	if len(errs) > 0 {
		cause = errs[0]
	} else if need > 0 && len(soft) > 0 {
		cause = soft[0]
	}
	if need > 0 {
		errs = append(soft, errs...)
	}

	// 4. Re-read, then free own interfaces that ended up empty.
	views, _, err = r.describe(api)
	if err != nil {
		return addrResult{err: err, reason: reasonFor(err)}
	}
	for _, v := range views {
		// Not while the fill waits or was cut short by an unanswered
		// assign: the next pass may need the interface.
		if deferred || ambiguous || v.primary || !v.own || len(v.secondary) > 0 {
			continue
		}
		if err := api.FreeENI(v.id()); err != nil {
			errs = append(errs, fmt.Errorf("free empty interface %s: %w", v.id(), err))
			continue
		}
		v.nic = nil
	}

	st.ENIs = enisStatus(views)
	if len(errs) > 0 {
		if cause == nil {
			cause = errs[0] // only step 4's frees failed
		}
		res := addrResult{err: errors.Join(errs...), reason: reasonFor(cause),
			waitSubnet: waitSubnet && len(errs) == len(soft)+1}
		if !blockedUntil.IsZero() && len(errs) == len(soft)+1 {
			res.retryAt = blockedUntil // nothing to do before then
		}
		return res
	}
	if need <= 0 {
		// Satisfied: an old streak of failed creates no longer counts.
		ns.createFails, ns.createErr = 0, nil
	}
	if len(slow) > 0 {
		return addrResult{err: errors.Join(slow...), reason: reasonFor(slow[0]), settled: true, short: short}
	}
	return addrResult{short: short, deferred: deferred, idle: idle}
}

// interfaceFull reports an assign refused because that one interface has no
// room, which another interface may still have.
func interfaceFull(err error) bool {
	c := apiErr.ErrCode(err)
	return c == apiErr.OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT || c == apiErr.INVALID_NIC_PRIVATE_IP_EXCEED_LIMIT
}

// pickSubnet chooses the subnet for a new interface: the one with the most
// free addresses, unless this pass found it full -- its count is refreshed only
// every subnetStatAging, so a subnet other nodes just filled can still lead;
// then any other configured subnet this pass has not found full. None when
// the subnets' own counts show no room at all.
//
// A subnet the platform called full within subnetFullHold counts as found
// full, and so does one whose cached count leaves no address beyond the new
// interface's own primary: the create would succeed, the fill fail, and the
// interface be deleted again. It also reports the chosen subnet's cached free
// count and when it was read, or 0 when the count is not to be trusted, so the
// create can be sized to it (reserveAddrs). A refresh that fails is returned,
// not reported as "no subnet has room".
func (r *Reconciler) pickSubnet(exhausted map[string]bool) (string, int, time.Time, error) {
	if err := r.Subnets.UpdateSubnetsStatus(factory.WithAging(subnetStatAging)); err != nil {
		return "", 0, time.Time{}, fmt.Errorf("refresh subnet counts: %w", err)
	}
	s := r.Subnets.SelectSubnet(types.IPStack(r.Cfg.IPStack), factory.WithAging(subnetStatAging))
	if s == nil {
		return "", 0, time.Time{}, nil
	}
	skip := func(id string) bool { return exhausted[id] || r.heldFull(id) }
	if !skip(s.SubnetId) {
		// The most free addresses of all; fewer than two means no subnet has
		// room for an interface and an address by the cached counts.
		if free := s.GetAvailableIPv4AddressCount(); free >= 2 {
			return s.SubnetId, free, s.GetLastUpdate(), nil
		}
		return "", 0, time.Time{}, nil
	}
	// The leader is found full or spent: the best of the others by their own
	// cached counts, with the same "room beyond a primary" rule.
	best, bestFree, bestAt := "", 1, time.Time{}
	for _, id := range r.Cfg.Subnets {
		if id == s.SubnetId || skip(id) {
			continue
		}
		ps := r.Subnets.GetPodSubnet(id)
		if ps == nil || !ps.Available(types.IPStack(r.Cfg.IPStack)) {
			continue
		}
		if free := ps.GetAvailableIPv4AddressCount(); free > bestFree {
			best, bestFree, bestAt = id, free, ps.GetLastUpdate()
		}
	}
	if best == "" {
		return "", 0, time.Time{}, nil
	}
	return best, bestFree, bestAt, nil
}

// markFull records that the platform called subnet id full just now.
func (r *Reconciler) markFull(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fullUntil == nil {
		r.fullUntil = map[string]time.Time{}
	}
	r.fullUntil[id] = time.Now().Add(subnetFullHold)
}

// heldFull reports whether subnet id was called full within subnetFullHold.
func (r *Reconciler) heldFull(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return time.Now().Before(r.fullUntil[id])
}

// claimSubnet lets one create at a time try a subnet that was called full
// before, so passes running in parallel for other nodes do not all repeat the
// refused call; releaseProbe ends the claim whatever the create's outcome. A
// subnet never called full is not limited. The claim is kept apart from the
// hold: only a refusal makes the subnet count as full for assigns too.
func (r *Reconciler) claimSubnet(id string) (ok, claimed bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, refused := r.fullUntil[id]; !refused {
		return true, false
	}
	if r.probing[id] {
		return false, false
	}
	if r.probing == nil {
		r.probing = map[string]bool{}
	}
	r.probing[id] = true
	return true, true
}

// releaseProbe ends a claimSubnet claim; only the pass that took it (claimed)
// may call it, or it would drop another pass's claim.
func (r *Reconciler) releaseProbe(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.probing, id)
}

// reserveAddrs takes the addresses for a new interface -- want, plus its own
// primary -- out of subnet id's cached free count, read at refreshed, and
// returns how many it may ask for: fewer than want when that is all the count
// leaves, 0 when it leaves none beyond a primary. The count is refreshed only
// every subnetStatAging, and every node's pass draws on the same one, so
// without this each would spend it again: in the same pass, in the next, and
// in parallel for other nodes. A newer refresh starts the spending over, even
// one that reads the same number.
func (r *Reconciler) reserveAddrs(id string, cached int, refreshed time.Time, want int) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.spent == nil {
		r.spent = map[string]*subnetSpend{}
	}
	sp := r.spent[id]
	if sp == nil || !sp.refreshed.Equal(refreshed) {
		sp = &subnetSpend{refreshed: refreshed}
		r.spent[id] = sp
	}
	free := cached - sp.used
	if free < 2 {
		return 0
	}
	n := min(want, free-1)
	sp.used += n + 1
	return n
}

// unreserveAddrs gives back what reserveAddrs took for a create that left no
// interface behind.
func (r *Reconciler) unreserveAddrs(id string, refreshed time.Time, n int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if sp := r.spent[id]; sp != nil && sp.refreshed.Equal(refreshed) {
		sp.used = max(0, sp.used-(n+1))
	}
}

// clearExpiredFull forgets a refusal whose hold is over once an address was
// assigned in the subnet: the subnet has room again, so creates in it need
// not be taken one at a time any more.
func (r *Reconciler) clearExpiredFull(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if until, ok := r.fullUntil[id]; ok && !time.Now().Before(until) {
		delete(r.fullUntil, id)
	}
}

// leftNothing reports a failed create the platform answered definitely and
// that left no interface behind: only then are its reserved addresses really
// still free. A lost answer may have created one; a failed cleanup left one.
func leftNothing(err error) bool {
	if errors.Is(err, factory.ErrENILeftBehind) {
		return false
	}
	if errors.Is(err, factory.ErrAttachENI) {
		return true // the attach path freed the interface it made
	}
	return !factory.OutcomeUnknown(err)
}

// clearFull forgets that subnet id was called full: an interface was just
// created in it.
func (r *Reconciler) clearFull(id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.fullUntil, id)
}

// subnetFull reports a create or assign refused because the subnet has no free
// address; an interface in another subnet may still take one. Every spelling
// the platform has used is matched, as everywhere else exhaustion is checked.
func subnetFull(err error) bool {
	switch apiErr.ErrCode(err) {
	case apiErr.OPERATION_DENIED_SUBNET_IP_INSUFFICIENT, apiErr.INVALID_SUBNET_IPV4_INSUFFICIENT,
		apiErr.InsufficientIpInSubnet:
		return true
	}
	return false
}

// refusedPerAddress reports an unassign refused because of one address in it
// (something else is bound to that address), not because of the call.
func refusedPerAddress(err error) bool {
	switch apiErr.ErrCode(err) {
	case apiErr.INVALID_NIC_HAS_PUBLIC_IP_CANT_DETACH, apiErr.INVALID_NIC_LB_ASSOCIATED, apiErr.INVALID_NIC_PRIMARY_IP_CANT_DETACH:
		return true
	}
	return false
}

// v6RefusalLasts reports an IPv6 prefix refusal that asking again soon will
// not change: the subnet or the billing setup does not allow one.
func v6RefusalLasts(err error) bool {
	switch apiErr.ErrCode(err) {
	case apiErr.INVALID_SUBNET_IPSTACK_NOT_SUPPORT, apiErr.OPERATION_DENIED_INTERNET_CHARGE_TYPE_NOT_SUPPORT,
		apiErr.OperationDeniedFlowPackageNotSupportedHourPeriod:
		return true
	}
	return false
}

var (
	// errInstanceGone: describe shows no primary interface, so the instance is
	// gone or stopped; the identity is verified again before anything else.
	errInstanceGone = errors.New("the instance reports no primary interface")
	errNoSubnet     = errors.New("no configured subnet can take another interface")
	errUntagged     = errors.New("a created interface did not carry this plugin's tags; check that the credential may tag on create")
	errQuotaFull    = errors.New("the instance has no room for another interface")
	errNoIPv6Prefix = errors.New("the platform refused an ipv6 prefix; the interface serves no pod on a dual-stack node")
	errNoPodENI     = errors.New("no interface can serve pods")
)

func reasonFor(err error) string {
	switch {
	case errors.Is(err, errNoSubnet):
		return reasonNoSubnet
	case errors.Is(err, errQuotaFull):
		return reasonQuotaFull
	case errors.Is(err, errInstanceGone):
		return v1alpha1.ReasonInstanceNotFound
	case errors.Is(err, errNoIPv6Prefix):
		return reasonNoIPv6Prefix
	case errors.Is(err, errNoPodENI):
		return reasonNoPodENI
	}
	if r := apiErr.OpenAPIErrEventReason(err); r != "" {
		return r
	}
	// A VPC or account quota (QuotaExceeded.*, LimitExceeded.*): the same
	// reason the daemon raises for it in direct mode.
	if c := apiErr.ErrCode(err); strings.HasPrefix(c, "QuotaExceeded") || strings.HasPrefix(c, "LimitExceeded") {
		return tracing.EventVpcResourceQuotaExceeded
	}
	// The interface was created but the instance did not take it: name that,
	// not a generic failure, since the fix is on the platform side. Decided by
	// the step that failed, not by the error code -- a full interface's code
	// (OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT) also contains ATTACH.
	if errors.Is(err, factory.ErrAttachENI) {
		return tracing.EventAttachENIFailed
	}
	return reasonAllocError
}

func (r *Reconciler) refreshLimits(ns *nodeState) error {
	if !ns.limitsAt.IsZero() && time.Since(ns.limitsAt) < limitTTL {
		return nil
	}
	l, err := ns.api.GetInstanceLimit()
	if err != nil {
		return fmt.Errorf("instance limits: %w", err)
	}
	ns.limits, ns.limitsAt = l.InstanceLimitsAttr, time.Now()
	return nil
}

// describe returns the managed interfaces -- the primary included, whose
// secondary addresses serve pods too -- and how many attached interfaces are
// not managed.
func (r *Reconciler) describe(api nodePlatform) ([]*nicView, int, error) {
	nics, err := api.DescribeAttachedNICs()
	if err != nil {
		return nil, 0, fmt.Errorf("describe interfaces: %w", err)
	}
	var views []*nicView
	managedSecondary := 0
	for _, nic := range nics {
		primary := strings.EqualFold(zecCom.ToString(nic.NicType), nicTypePrimary)
		if !primary && !api.IsManagedENI(nic) {
			continue
		}
		if !primary {
			managedSecondary++
		}
		views = append(views, &nicView{nic: nic, primary: primary, own: api.IsOwnENI(nic), secondary: secondaryIPv4s(nic),
			excluded: !slices.Contains(r.Cfg.Subnets, zecCom.ToString(nic.SubnetId))})
	}
	if !slices.ContainsFunc(views, func(v *nicView) bool { return v.primary }) {
		return nil, 0, errInstanceGone
	}
	unmanaged := max(0, len(nics)-managedSecondary-1)
	// Primary first, then by id: a stable order keeps the fill from spreading
	// addresses differently on every pass.
	sort.SliceStable(views, func(i, j int) bool {
		if views[i].primary != views[j].primary {
			return views[i].primary
		}
		return views[i].id() < views[j].id()
	})
	return views, unmanaged, nil
}

// secondaryIPv4s is every IPv4 on the interface but its own primary.
func secondaryIPv4s(nic *zecApi.NicInfo) []string {
	primary := zecCom.ToString(nic.PrimaryIpv4)
	set := map[string]bool{}
	for _, ip := range nic.SecondaryIpv4s {
		set[ip] = true
	}
	for _, ip := range nic.PrivateIpList {
		if p := net.ParseIP(ip); p != nil && p.To4() != nil {
			set[ip] = true
		}
	}
	delete(set, primary)
	out := make([]string, 0, len(set))
	for ip := range set {
		out = append(out, ip)
	}
	sort.Strings(out)
	return out
}

func enisStatus(views []*nicView) []v1alpha1.ENIStatus {
	var out []v1alpha1.ENIStatus
	for _, v := range views {
		if v.nic == nil {
			continue
		}
		e := v1alpha1.ENIStatus{
			ID:          v.id(),
			MAC:         strings.ToLower(v.mac()),
			Primary:     v.primary,
			SubnetID:    zecCom.ToString(v.nic.SubnetId),
			State:       v1alpha1.ENIStateReady,
			PrimaryIPv4: zecCom.ToString(v.nic.PrimaryIpv4),
		}
		if v.excluded {
			e.State = v1alpha1.ENIStateExcluded
		}
		for _, ip := range v.secondary {
			e.IPv4 = append(e.IPv4, v1alpha1.IPAddr{IP: ip, State: v1alpha1.IPStateReady})
		}
		if prefix := zecCom.ToString(v.nic.Ipv6Cidr); prefix != "" {
			e.IPv6 = &v1alpha1.IPv6Prefix{Prefix: prefix, Primary: zecCom.ToString(v.nic.PrimaryIpv6)}
		}
		out = append(out, e)
	}
	return out
}
