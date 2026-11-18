# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added

- The controlplane has its own image, `<image.repository>-controlplane` with
  the same tag (`docker.io/zenlayer1/zeccni-controlplane`): only the static
  `zec-controlplane` binary and CA certificates, running as user 65534. Build
  it with `make image-controlplane`.

- The controlplane accepts a temporary credential from AssumeRole as well as
  an access key: a credential directory holding `AccessKeyID`,
  `AccessKeySecret`, `SecurityToken` and `Expiration` is read as one, and a
  renewal replaced in place is picked up within a minute without a restart.
  The long-term `AccessKeyID` / `AccessKeyPassword` Secret works as before. A
  credential past its expiry and not renewed is logged.

### Changed

- **Breaking (install):** with `cilium.enabled=true` and `zec.k8sServiceHost`
  empty, a render that cannot read the apiserver entry point from
  `kube-public/cluster-info` -- `helm template`, a client-side dry run, a
  GitOps tool, or a cluster without that ConfigMap -- now fails instead of
  falling back to the in-cluster Service, which Cilium in place of kube-proxy
  leaves unserved until it is up. Set `zec.k8sServiceHost` and
  `zec.k8sServicePort` there.
- **Breaking (packaging): the node image no longer carries `zec-controlplane`,
  and `controlplane.deployment` runs the controlplane's own image** by default,
  as user 65534 with a read-only root filesystem; the credential Secret is
  mounted 0440 to its group (`fsGroup`). An offline or mirrored registry needs
  `zeccni-controlplane` as well as `zeccni`. A `controlplane.deployment.image`
  set to the node image no longer works.
- `controlplane.deployment.replicas` defaults to 1 (was 2) and
  `controlplane.deployment.nodeSelector` to `{}` (was the control-plane label):
  the controlplane now prefers control-plane nodes rather than requiring them.
  The old defaults left a second replica Pending and every rollout stuck on a
  cluster with one control-plane node, and a selector set by the user was
  merged with the default label instead of replacing it. Set `nodeSelector`
  to keep the controlplane on particular nodes.

- **Breaking (behavior): pod addresses come only from `zec.subnetIDs`.** To keep
  the old capacity, list the node subnet in `zec.subnetIDs`. A node's primary
  interface used to serve pods whatever its subnet; now it does so only when its
  subnet is one of the pod subnets. Otherwise it takes no new address and gets no
  IPv6 prefix from the plugin, in either mode -- so a node subnet without IPv6 no
  longer stops a dual-stack daemon from starting -- and its share leaves the
  node's capacity: `(ENIQuota - unmanaged - 1) x (IPv4 per interface - 1)`
  instead of `(ENIQuota - unmanaged) x (...)`. On upgrade, addresses pods hold on
  it stay until those pods go, and in both modes the advertised capacity falls
  to the new figure as they go. Addresses on it the daemon has no record of --
  after an upgrade, the warm ones from before it -- are left in place, since
  they may be an operator's; remove them by hand if not needed. **A cluster whose
  node subnets are not among its pod subnets holds fewer pods per node.** The
  primary is reported as ENI state `Excluded` in crd mode and by the Normal node
  event `InterfaceExcluded` in direct mode, both whenever it is excluded.
  The same holds for an interface the plugin created in a subnet later removed
  from `zec.subnetIDs`: it takes no new address, its idle addresses are given
  back, and once its pods are gone it is freed, so a replacement is created in
  a listed subnet.
  A node left with no interface that can serve pods (quota full, or taken by
  unmanaged interfaces) holds none: crd reports ZecNode reason `NoPodInterface`;
  direct advertises a capacity of 0.

- **The controlplane's leaked-interface sweep keeps to its own cluster.** Every
  interface the plugin creates now carries `zenlayer:cni:cluster-id`, the uid of
  the cluster's `kube-system` namespace. The VPC-wide sweep deletes a detached
  interface only when that tag names this cluster or is absent; one tagged with
  another cluster's id is left to that cluster. Absent covers interfaces created
  before this version, which are swept as before, so an upgrade leaves no
  interface behind -- and those a direct-mode daemon creates while it cannot
  read the namespace (it only logs a warning), which the controlplane of any
  crd-mode cluster in the VPC may sweep. Clusters sharing a VPC are kept apart only once all of them
  run this version: an older one still sweeps every plugin interface in the VPC.
  The daemon and the controlplane need `get` on the `kube-system` namespace,
  which the chart grants; the controlplane waits until it can read it.
- **An interface is swept after 10 minutes detached, not 5**, longer than the
  roughly 5.5 minutes a create may wait for its interface to attach, so a create
  still in progress is no longer deleted under it.
- The controlplane writes `Node.spec.providerID` as
  `zenlayer:///<regionId>/<instanceId>`, the form
  zenlayer-cloud-controller-manager writes, instead of
  `zenlayer://<instanceId>`. The field can be set only once, so two forms
  would lock out whichever component wrote second. A providerID naming a
  region other than the instance's is refused (`IdentityMismatch`). Nodes
  2.0.0 already wrote in the old form are still read.

### Fixed

- Direct mode: the pool no longer sleeps out its backoff while holding its
  lock, which kept every pod ADD and DEL on the node waiting for it.
- Direct mode: GC's check for a pod's owner now has a timeout, so a slow
  apiserver can no longer hold the pool paused.
- Direct mode: `poolTargetCapProbe=true` with a `poolTarget` above
  `poolTargetMax` is accepted again; `poolTargetMax` is ignored while probing.
- The pool no longer asks for addresses a full node has no room for while it
  holds idle ones it cannot hand out (as on an `Excluded` interface), which
  stalled the next pod for the minute that request waited.
- crd mode: addresses pods still hold on an `Excluded` interface are added back
  to the capacity the daemon advertises, which no longer drops twice for them;
  an interface becoming `Excluded` gives its idle addresses back within about
  a minute, not at the next periodic GC, and the advertised capacity follows
  each address given back there.
- Upgrade advice: use `helm upgrade --reset-then-reuse-values` (Helm 3.14+), not
  `--reuse-values`, which also keeps the previous chart's defaults -- so keys
  added since go missing and an image tag left at its default never moves. This
  supersedes the `--reuse-values` advice in the 2.0.0 notes below. Upgrading
  from 1.0.0 needs the switches passed explicitly with the values 1.0.0 ran
  with; see docs/deploy-direct.md, "Upgrading from 1.x".
- NOTES and values: with `cilium.enabled=true` the Cilium agent enforces
  NetworkPolicy whatever `networkPolicy.enabled` says (that switch only
  declares the requirement); NOTES no longer reports such a cluster as "not
  enforced".
- Docs: Cilium's own `k8sServiceHost=auto` needs Cilium 1.16 or later; on 1.15
  set the address and port explicitly.
- crd mode: a daemon starting while its ZecNode is deleted and recreated (the
  documented way to rebind a node name) no longer waits forever for a
  generation the new object never reaches.
- crd mode: when the subnet the controlplane picks for a new interface turns
  out to be full, it tries the other configured subnets in the same pass
  instead of backing off for minutes.
- A subnet with no address left even for a new interface's own primary is
  now recognized as full (`INVALID_SUBNET_IPV4_INSUFFICIENT`, the code the
  create call returns): the create no longer retries for about two minutes,
  the reason is `InsufficientIpInSubnet` rather than `AllocationError`, and in
  crd mode the controlplane moves on to the next configured subnet.
- crd mode: `IPAllocationFailed` no longer flips back to `Satisfied`, and its
  Warning event is no longer re-sent, each time a node's request lapses
  between the daemon's retries; a failure stands for up to 5 minutes of such
  lulls and clears as soon as an allocation succeeds or the request it failed
  to meet is met.
- crd mode: a daemon waiting for verification reads the node's identity again
  when the controlplane reports `IdentityMismatch`, so fixing a node's
  `--node-ip` no longer needs the daemon restarted.
- crd mode: the `NoPodInterface` message no longer repeats its opening phrase;
  the controlplane logs each address assign, release and interface create.
- Release images are built stripped and optimized (`DEBUG=0`): the node
  image's `zec-cni` and `zec-ipam`, and the new controlplane image. In 2.0.0
  every binary, the controlplane included, kept its symbols and was built
  without optimizations.
- crd mode: a subnet the platform calls full is skipped for 2 minutes by every
  node, not retried by each node every 15 seconds; after that one interface
  create at a time tries it again (assigns on interfaces already in it resume
  on every node). New interfaces, for all nodes together, ask for no more
  addresses than the subnet's cached count leaves beyond their own primaries,
  and a subnet with none left is not chosen -- such a create only made an
  interface to delete it again. With every configured subnet full, a node
  looks again after a minute. A node's Warning is not repeated within 10
  minutes when two failures take turns (a full subnet and a full quota, say).
  A subnet that refuses a multi-address request but takes single addresses
  is filled one address at a time on the interfaces already there before a
  new one is made. A node whose creates are paused or backing off is looked
  at again when that ends, not every 15 seconds, and a node that is satisfied
  forgets its earlier failed creates. A failed pass is reported under the
  failure that stopped it (a full quota, a refused attach), not under a
  full interface or subnet it worked around on the way.
- crd mode: `AttachENIFailed` is reported only when the instance refused to
  take a new interface, decided by the step that failed rather than by an
  error code containing `ATTACH`; a full interface
  (`OPERATION_DENIED_NIC_ATTACH_LAN_IP_EXCEED_LIMIT`) is no longer reported as
  one. The reason's message says whether the refused interface was freed.
  Platform events raised by code shared with the daemon, which name no node,
  are logged by the controlplane instead of being dropped.
- crd mode: a quota refusal (`QuotaExceeded.*`, `LimitExceeded.*`) is reported
  as `VpcResourceQuotaExceeded`, as in direct mode, rather than
  `AllocationError`; a failure to refresh the subnets' counts is reported as
  that failure rather than as `NoAvailableSubnet`; `ipStack: ipv6` is refused
  as not implemented yet rather than as an unknown value.
- The chart no longer requires `controlplane.regionID` when `zec.ipamType` is
  `direct` and `controlplane.deployment.enabled` is left on.
- crd mode: a node whose instance is in another region of the VPC than
  `controlplane.regionID` is refused (`IdentityMismatch`) instead of being
  verified and then failing every interface create. A subnet that refuses a
  batch of addresses is asked once more for a single one before it counts as
  full; a create that leaves its interface behind keeps its addresses counted
  against the subnet. The controlplane Deployment no longer tolerates eviction
  from a node that stops answering (it leaves after 60 s), so the single
  default replica moves to a live node, nor a cordoned node, so a drain moves
  it; it tolerates only the taints of a node still coming up and of
  control-plane nodes.
- Both modes: a new interface's create is no longer repeated when the
  platform's answer was lost (a repeat could make a second interface), and a
  create whose rollback also fails says the interface was left behind. crd
  mode: an empty interface is kept when an unanswered assign cut the fill
  short, and with `ipamType: crd` the daemon's refusal of `poolCapRatio` and
  `subnetStat*` in `eni_conf` says they have no effect instead of sending them
  to the controlplane, which does not take them.
- The capacity advertised to the scheduler follows pods that hold an address
  without requesting `zenlayer.com/eni-ip` as they start and go, and in direct
  mode each address given back and each GC, instead of up to 5 minutes later.
- Logs and events: releasing an address the pool does not hold (a pod on an
  interface the node no longer manages) is logged at info, not as an error;
  direct mode logs the capacity it advertises whenever it changes, including
  what interfaces taking no new addresses take off the quota-based figure;
  event messages are cut to 512 characters, so a gateway's HTML error page no
  longer lands whole in `kubectl describe`. The `NodeClockSkew` description
  now says the clock is behind; a clock running ahead is not reported.
- Direct mode: an interface outside the pod subnets is freed once empty even
  when no interface on the node reports any address. One that drops out of a
  single describe no longer has its pods' addresses taken back when it returns.
- The pool no longer gives back, at once, addresses just created for
  Allocates waiting on them, and a capacity that shrinks and grows back no
  longer lets more creates run than there is room for.
- The chart refuses a controlplane scaler with `targetMin` above `target`
  instead of installing a controlplane that will not start.
- crd mode: addresses held on an interface refused an IPv6 prefix are added
  back to the advertised capacity, as on an `Excluded` one, and addresses the
  node will never take (retired ones) are taken out of it; the device plugin
  is told whenever that figure changes, not only when the controlplane's
  capacity does.
- Docs: Helm 3.14 or later is required; the offline `helm template` preview
  passes `--kube-version`; L7 and FQDN policy work under chaining with
  Cilium's default `l7Proxy=true` (the README said otherwise); the netkit
  guide's Cilium value is `ipam.installUplinkRoutesForDelegatedIPAM`; the
  chaining guide's coverage check now lists Pods (the old filter matched
  none); a missing credentials Secret shows as `FailedMount`, not a `/healthz`
  message.

## [2.0.0] - 2026-11-09

### Added

- `zec.ipamType=crd`: nodes hold no OpenAPI credentials. A new controlplane
  (`zec-controlplane`, image binary of the same name) is the only component
  that calls the OpenAPI; each node states how many addresses it wants and
  which it gives back in a cluster-scoped `ZecNode` object
  (`zecnodes.network.zenlayer.com`, `kubectl get zn`), and the controlplane
  reports the node's interfaces, addresses and capacity in its status. The
  controlplane verifies each node's instance before acting for it and, with
  `controlplane.identity.providerID=auto`, fills an empty
  `Node.spec.providerID` (`zenlayer://<instanceId>`) once verified. An
  admission policy keyed on the node name in each daemon's service account
  token keeps a node to its own `ZecNode`, so crd mode needs Kubernetes 1.30
  or later. Deleting a Node object never detaches or deletes interfaces.
  `controlplane.deployment.enabled` runs the controlplane in the cluster for
  clusters the platform does not manage.

- Warning events on the Node for failures that stop pods from getting
  addresses: `CredentialServiceAbnormal` (the OpenAPI rejected the credential,
  or none is configured), `OpenApiPermissionDenied` (the credential is valid
  but lacks a permission), `NodeClockSkew` (requests rejected as expired -- fix
  the node's time sync, a new credential will not help), `OpenApiUnreachable`
  (the OpenAPI endpoint cannot be reached, or a proxy or gateway in front of
  it answered instead), `AttachENIFailed`, and
  `DaemonStartFailed` for any other startup failure once the daemon can reach
  the API server. A startup failure is written before the daemon exits, so it
  is visible while the pod is in `CrashLoopBackOff`. See Troubleshooting in the
  README.

### Changed

- **Breaking (chart):** `zec.ipamType` (`direct` or `crd`) is a new behavior
  switch with no default; pass `--set zec.ipamType=direct` to keep the current
  behavior. The daemon likewise refuses to start when `eni_conf` leaves out
  `ipamType`, which matters only for a hand-written configuration such as a
  per-node ConfigMap selected with the `k8s.zenlayer.com/vpc-cni-config` label.

- **Breaking (chart):** the behavior switches `zec.ipStack`,
  `zec.manageUntaggedENI`, `cni.mode`, `cilium.enabled` and
  `networkPolicy.enabled` no longer have defaults and must be passed on install
  and on every upgrade, so a default changing in a later chart can never flip
  one on an existing cluster. `helm upgrade` without them fails schema
  validation; `helm upgrade --reuse-values` carries the previous values
  forward. The 1.0.0 defaults were `ipv4`, `true`, `chained`, `false`, `false`.
  The daemon likewise refuses to start when `eni_conf` leaves out `ipStack` or
  `manageUntaggedENI`, which matters only for a hand-written configuration,
  such as a per-node ConfigMap selected with the
  `k8s.zenlayer.com/vpc-cni-config` label: the chart always writes both.
- Turning `zec.manageUntaggedENI` from `true` to `false` on a node with adopted
  interfaces leaves their pods running, but the addresses on those interfaces
  are no longer managed: they are not returned to the subnet when the pods go
  away, and the node advertises one interface's worth of addresses less while
  those pods still count against it. Move the pods off first, or release the
  addresses by hand afterwards.
- `image.pullPolicy` now defaults to `IfNotPresent`. Set it to `Always` when
  re-pushing the same tag. `helm upgrade --reuse-values` keeps `Always`.

### Fixed

- Release notes for 1.0.0, corrected in place:
  - Every ENI, the primary NIC included, contributes one address fewer than
    its per-ENI limit, because each keeps its own primary address. The entry
    said the ENIs the plugin creates contribute all of theirs.
  - The entry said interfaces attached by an operator are left untouched. With
    1.0.0's default `manageUntaggedENI=true`, those in a configured pod subnet
    were adopted; the sentence is removed.

## [1.0.0] - 2026-08-28

First public release. Pod addresses are secondary IPs on Zenlayer VPC elastic
network interfaces, wired with veth pairs and per-ENI policy routing. Nodes run
either IPv4-only or dual-stack, selected per node; NetworkPolicy is available by
chaining upstream Cilium.

### Added

#### IPAM — ENI multi-IP

- Pod addresses are secondary IPs on Zenlayer VPC elastic network interfaces.
  The daemon attaches ENIs on demand, assigns secondary IPs from the configured
  subnets, and returns both to the platform when they are no longer needed.
- A per-node warm pool keeps a configurable number of addresses ready so that
  pod creation does not wait on an OpenAPI round trip. Tunable through
  `zec.poolTarget`, `zec.poolTargetMin`, `zec.poolTargetMax` and
  `zec.poolTargetCapProbe`.
- Capacity is probed from the instance type rather than hardcoded. Every ENI,
  the primary NIC included, contributes every address except its own primary.
- Up to 5 subnets per cluster. Subnets are validated at startup against the
  configured VPC and region, and a subnet that does not exist, belongs to
  another VPC, or sits in another region fails the daemon with a message naming
  what it actually belongs to.
- ENIs created by the plugin are tagged with the instance ID. The tag is the
  only ownership credential: an interface carrying another node's tag is never
  claimed, and renaming an interface does not release it.
- Interfaces that were created but never attached are deleted on the spot; any
  that still escape are removed by an hourly sweep once they have been idle for
  more than five minutes.
- Pod-to-address records are persisted locally, so a daemon restart rebuilds
  the pool from the platform without reassigning addresses that containers are
  still using. If that store is lost while pods keep running, the daemon takes
  the API server's word for which addresses are held and marks them in use
  rather than handing them out again; deleting such a pod still releases its
  address and removes the routing rules installed for it.
- Secondary network interfaces attached to a node outside this plugin are
  adopted when they sit in one of the configured pod subnets and carry no
  `zenlayer:cni:no-manage=true` tag: their addresses join the pod address pool.
  An adopted interface is detached **and deleted** when the pool shrinks, and
  under `dual` the daemon enables IPv6 on it, which is billable on a
  Public-IPv6 subnet. `zec.manageUntaggedENI=false` opts a node out entirely.

#### Address families — IPv4-only and dual-stack

- `zec.ipStack` selects the address families a node serves: `ipv4` (the default,
  and what an empty value means) or `dual`. `ipv6` parses but is refused with a
  message that says it is not implemented rather than that it is misspelled,
  because IPv6-only would need a per-ENI IPv6 budget that the platform does not
  publish, a Service CIDR path that still demands an IPv4 answer, and the IPv4
  address a new ENI is born with to be stripped.
- Under `dual` a pod receives one IPv4 and one IPv6 address as a single
  indivisible unit. A resource that covers only one family is never handed to a
  pod; it is marked and reclaimed instead. This is what keeps a pod from
  starting with half an address set after a partial platform failure.
- The platform routes one `/96` prefix per interface, and addresses are carved
  from it locally — there is no per-address IPv6 API. The interface's own
  primary IPv6 falls inside that prefix and is excluded from the carve, from
  two independent sources; when those sources disagree, both candidates are
  withheld rather than one being guessed.
- IPv6 never constrains node capacity. A `/96` holds far more addresses than any
  interface can serve, so `zenlayer.com/eni-ip` and the warm pool are still
  sized on IPv4 alone, and a node reports the same capacity in both modes.
- Under `dual` every configured subnet must carry both an IPv4 and an IPv6 CIDR.
  The daemon refuses to start otherwise, naming the subnet and the family it is
  missing. Checking at startup rather than at allocation time is deliberate: a
  per-allocation warning is indistinguishable from a slow platform, and the
  symptom would be a pool that never fills with every pod stuck in
  `ContainerCreating`.
- Dual-stack Service CIDRs are read as a comma-separated pair. A missing IPv4
  half is still fatal; a missing IPv6 half is logged and tolerated.
- Public IPv6 billing is configuration, not a constant. The platform refuses to
  enable IPv6 on an interface in a subnet whose `ipv6Type` is `Public` without a
  charge type and a bandwidth cap, and it is this plugin that enables IPv6 on
  every interface it creates, so `zec.ipv6InternetChargeType` and
  `zec.ipv6Bandwidth` decide what the cluster is billed for. `ByTrafficPackage`
  also requires `zec.ipv6PackageSize` and `BandwidthCluster` also requires
  `zec.ipv6ClusterId`; both are refused at render time by the chart and again at
  startup by the daemon, rather than surfacing later as a `MISSING_PARAMETER`
  from inside warm-pool fill.

#### Datapath — veth with per-ENI policy routing

- Each pod gets a veth pair. The host end is named deterministically from the
  pod identity, and its MAC is set explicitly rather than left to the kernel.
- Egress is pinned to the ENI that owns the pod's address through two rules per
  pod (priority 512 for return traffic, priority 2048 for pod-sourced traffic),
  with one routing table per ENI numbered `1000 + master ifindex`.
- Pods reach the host stack directly, so kube-proxy sees both directions of
  Service traffic without additional configuration.
- The container side carries a `/32` address and a default route through
  `169.254.1.1` with a permanent neighbor entry, which keeps pod egress
  independent of ARP on the pod network. Under `dual` it also carries a `/128`
  address and a default route through the link-local `fe80::1`, again with a
  permanent neighbor entry, so neither family depends on address resolution on
  the pod network. Each family gets its own pair of policy rules.
- Every interface the plugin creates configures its own primary address rather
  than borrowing the node's. A fabric that checks source addresses drops an ARP
  or neighbor solicitation whose sender belongs to a different port, which
  leaves the gateway unresolved and every pod on that interface black-holed.

#### NetworkPolicy — optional Cilium chaining

- `cilium.enabled=true` chains upstream Cilium after this plugin using its
  `generic-veth` chaining mode. Cilium is neither forked, patched, nor vendored,
  and the chart does not install it.
- `networkPolicy.enabled=true` declares that policy must be enforced. With
  provider `cilium` it requires `cilium.enabled=true`, and the chart refuses to
  render otherwise rather than accepting policy objects that nothing enforces.
- When the conflist chains Cilium, the daemon holds the conflist back until the
  Cilium agent answers on its socket, because `cilium-cni` has no fallback path
  on ADD.
- The veth pair and the per-ENI policy routing are unchanged by chaining.
  See [docs/deploy-cilium-chained.md](./docs/deploy-cilium-chained.md) for the required Cilium
  values, verification steps, and the switching procedure in both directions.

#### Operations

- Helm chart with a JSON schema that rejects invalid configuration at install
  time: required identifiers, subnet count, enumerated values, warm pool
  ordering, and unknown keys -- a misspelled key is refused rather than read as
  a default.
- Credentials are read from a Secret mounted read-only at `/etc/zec_secret/`.
  The chart never renders them, so they do not reach `values.yaml`, git, or the
  Helm release, and they are redacted in daemon logs.
- The extended resource `zenlayer.com/eni-ip` reports assignable addresses so
  that pods declaring it are scheduled against real capacity.
- Prometheus metrics on the daemon's debug port: `resource_pool_total`,
  `resource_pool_available`, `resource_pool_target`, `resource_pool_target_min`,
  `resource_pool_max_cap`, plus `zec_api_req_count`, `zec_api_latency_ms` and
  `zec_api_error_count` for the platform API.
- A `/healthz` endpoint that only starts answering once the daemon has reached
  the OpenAPI and built its pool. It reports the daemon, not the node: the
  conflist is written afterwards, so with Cilium chaining `/healthz` answers
  `ok` while the daemon is still waiting for the Cilium agent and the node
  cannot start pods. Both probes are wired to it, so watch restart counts and
  the `Still waiting for the cilium agent` log line as well.
- Kubernetes events on the pod for per-pod allocation outcomes and on the node
  for conditions that affect the whole node, such as a subnet with no addresses
  left.
- Per-node configuration override through a node label, for changing pool
  parameters on a single node without touching the cluster-wide ConfigMap.
- `zec.logLevel` sets the level for both binaries from one value. It renders
  `ZENLAYER_CNI_LOGLEVEL` onto the DaemonSet for the daemon and, for `debug`,
  the conflist `debug` flag -- the only channel the plugin has, since kubelet
  execs it from the host and it never sees the pod's environment.

- The daemon logs what it resolved for the node on every quota refresh:
  `InstanceLimit Updated {ENIQuota, IPv4MaxPerENI, IPv6CidrMaxPerENI,
  ENIManaged, ENIUnmanaged, Cordon, allocatable}`, alongside the MAC addresses
  of the interfaces it created, adopted, and left alone. `allocatable` is the
  daemon's own capacity figure; the device plugin advertises it minus the pods
  that do not request `zenlayer.com/eni-ip`, so the two differ by that count.

### Known limitations

- IPv6-only is not implemented. `zec.ipStack` accepts `ipv4` and `dual`.
- Under `dual`, IPv6 allocation state is maintained by the daemon alone —
  addresses are carved from each interface's prefix rather than requested
  individually, so the platform publishes no per-address IPv6 view to reconcile
  against. IPv4 is additionally reconciled against the platform on every
  garbage collection pass.
- One datapath. `zec.dataPath` accepts `veth`; any other value is rejected at
  install time and at daemon startup.
- An interface attached outside this plugin and sitting outside the configured
  pod subnets is never adopted. It still holds one of the instance's interface
  slots, so the node's capacity is one interface's worth of addresses lower
  than the instance size alone would suggest.
- An interface detached from a node out of band is reconciled on the next
  garbage collection pass: its addresses leave the pool and the interface is
  dropped. Pods that held those addresses lose connectivity and have to be
  recreated; nothing restores them in place.
- One network mode. `zec.networkMode` accepts `eni_multi_ip`.
- `linux/amd64` only. The DaemonSet has a hard node affinity on
  `kubernetes.io/os=linux` and `kubernetes.io/arch=amd64`.
- No configuration is applied without a restart. The ConfigMap is mounted with
  `subPath` and the conflist is written at startup, so changing configuration
  requires recreating the daemon pods.
- **Dual-stack with Cilium chaining requires a dual-stack Kubernetes cluster.**
  Cilium must be deployed with `ipv6.enabled=true`, and that in turn requires
  every Node object to carry an IPv6 address — a cluster-level prerequisite no
  Cilium value can supply. Neither half can be skipped: without
  `ipv6.enabled=true` the agent's per-veth BPF drops every IPv6 packet as an
  unsupported L3 protocol, and it does so silently — the agent reports healthy,
  every endpoint lists the correct IPv6 address, and nothing is logged above
  info; with it on a single-stack cluster the agent refuses to start. This
  plugin's own dual-stack support does not depend on the cluster being
  dual-stack; the requirement is introduced only by layering chaining on top.
  See [docs/deploy-cilium-chained.md](docs/deploy-cilium-chained.md) §5.5 and §5.6.
- With Cilium chaining, L7 policy (HTTP/DNS/FQDN), transparent encryption
  (IPsec/WireGuard), and Cilium Ingress and Gateway API are not available.
  L7 and FQDN policies fail open: the API server accepts the object, the agent
  never loads it, and the traffic it should deny is allowed. A policy using only
  `toFQDNs` is even reported as `Valid=True`. Express isolation with native
  NetworkPolicy or a purely L3/L4 CiliumNetworkPolicy; see
  [docs/deploy-cilium-chained.md](docs/deploy-cilium-chained.md) §2.
- With Cilium chaining, ClusterIP traffic **from pods** is load-balanced by
  Cilium's tc-BPF even with `kubeProxyReplacement=false`; the `KUBE-SVC-*`
  chains stay at zero packets while `cilium-dbg service list` shows the
  frontend. Traffic originated by the node itself still goes through
  kube-proxy, so the two paths differ. This is inherent to chaining mode.
  Troubleshoot Services with `cilium-dbg service list` rather than iptables.
- The extended resource `zenlayer.com/eni-ip` is a lagging figure, refreshed
  about every 5 minutes and computed as capacity minus the pods that do not
  request it. It is a floor rather than an overstatement, so the scheduler is
  not misled, but it is not live capacity -- read the `resource_pool_*` metrics
  for that.
- Uninstalling Cilium requires the documented order, and rolling the workloads
  first is not optional. The tc/tcx BPF programs on a pod's veth are BPF links
  that `helm uninstall` does not remove, and once the agent is gone their
  load-balancing maps freeze: pods created while chaining was enabled keep
  resolving ClusterIPs through a table that no longer tracks backend changes,
  which drops traffic silently. See
  [docs/deploy-cilium-chained.md](docs/deploy-cilium-chained.md) §10.
- Pod egress bandwidth limiting is not implemented by this plugin. The conflist
  does not declare the `bandwidth` capability, so kubelet does not pass the
  bandwidth annotations to it.

[Unreleased]: https://github.com/zenlayer/zenlayer-vpc-cni-k8s/compare/v2.0.0...HEAD
[2.0.0]: https://github.com/zenlayer/zenlayer-vpc-cni-k8s/compare/v1.0.0...v2.0.0
[1.0.0]: https://github.com/zenlayer/zenlayer-vpc-cni-k8s/releases/tag/v1.0.0
