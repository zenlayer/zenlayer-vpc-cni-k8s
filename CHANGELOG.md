# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

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
- Capacity is probed from the instance type rather than hardcoded. The primary
  NIC contributes every address except the node's own; each ENI created by the
  plugin contributes all of its addresses, including its own primary.
- Up to 5 subnets per cluster. Subnets are validated at startup against the
  configured VPC and region, and a subnet that does not exist, belongs to
  another VPC, or sits in another region fails the daemon with a message naming
  what it actually belongs to.
- ENIs created by the plugin are tagged with the instance ID. The tag is the
  only ownership credential: an interface carrying another node's tag is never
  claimed, and renaming an interface does not release it. Interfaces attached
  by an operator are left untouched.
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

[1.0.0]: https://github.com/zenlayer/zenlayer-vpc-cni-k8s/releases/tag/v1.0.0
