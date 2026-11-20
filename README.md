# Zenlayer VPC CNI for Kubernetes

English | [简体中文](README-CN.md)

A CNI plugin that gives Kubernetes Pods native Zenlayer VPC addresses. Pod IPs
are secondary IPs on Zenlayer elastic network interfaces (ENIs) attached to the
node, so the VPC itself routes Pod traffic. There is no overlay and no
encapsulation, and Pod addresses are reachable from anywhere in the VPC.

Status: the plugin supports the `eni_multi_ip` network mode on `linux/amd64`
nodes. A node serves either IPv4 only or dual-stack; see
[Address families](#address-families).

**This document describes the centralized deployment (`zec.ipamType=crd`)**: only
the controlplane holds the Zenlayer OpenAPI credentials and calls the OpenAPI;
nodes hold none, and each states how many addresses it needs in its own
`ZecNode` object. For direct mode (`zec.ipamType=direct`), where every node calls
the OpenAPI itself, see [docs/deploy-direct.md](docs/deploy-direct.md).
`zec.ipamType` has no default and must be passed at install time.

## Feature support

| Capability | Status |
| --- | --- |
| IPAM mode | `eni_multi_ip`: a Pod IP is a secondary IP on a VPC elastic network interface |
| Datapath | veth pair + per-ENI policy routing. Under `delegated`, Cilium provides the Pod device |
| Address families | IPv4 or dual-stack. IPv6-only is not implemented |
| Node platform | `linux/amd64` only |
| Deployment mode | Centralized (`zec.ipamType=crd`, this document): only the controlplane holds the OpenAPI credentials. Direct mode, with credentials on every node, is in [docs/deploy-direct.md](docs/deploy-direct.md) |
| Kubernetes | Centralized deployment >= 1.30 (requires ValidatingAdmissionPolicy); direct mode >= 1.28.2 (the chart's `kubeVersion` constraint) |
| Service handling | kube-proxy, or a replacement that you install yourself. This plugin provides none |
| NetworkPolicy | Not enforced by default. Available by chaining upstream Cilium; see [Network policy](#network-policy) |
| Cilium (chaining) | Recommended: v1.15 to v1.18 (`k8sServiceHost=auto` in Cilium's own chart requires v1.16 or later; see [docs/deploy-cilium-chained.md](docs/deploy-cilium-chained.md)). Checked against the v1.17.5 source and verified on a v1.17.5 cluster |
| SNAT / iptables rules | None. Pods that need public IPv4 egress require a NAT gateway on the VPC side |
| Pod bandwidth limiting | Not implemented. Egress shaping is available through Cilium's bandwidth manager |
| `hostPort` | Supported by whichever component owns the datapath. Without Cilium, the conflist chains the upstream `portmap` plugin (the binary ships in the image). With `cilium.enabled=true`, Cilium's own BPF load balancer serves hostPort and `portmap` is not chained at all. This works only while Cilium runs with `kubeProxyReplacement=true` (the recommended setting) or has `enable-host-port` set; otherwise nothing serves hostPort. In that case, **keep the port below 30000**: Cilium refuses any hostPort inside the NodePort range and only logs a warning (see [docs/deploy-cilium-netkit.md](docs/deploy-cilium-netkit.md) §4.5). Pod IPs are routable within the VPC in any case, so reaching a Pod by its own IP or through a Service is usually the better fit. A hostPort pins the Pod to one node and collides with any other Pod on that node that requests the same port |
| Pod subnets per VPC | At most 5 |

## Contents

- [Feature support](#feature-support)
- [Quick Start](#quick-start)
- [Architecture](#architecture)
- [Datapath](#datapath)
- [Address families](#address-families)
- [Network policy](#network-policy)
- [Requirements](#requirements)
- [Installation](#installation)
- [Capacity planning](#capacity-planning)
- [Configuration reference](#configuration-reference)
- [Uninstalling](#uninstalling)
- [Building from source](#building-from-source)
- [Further documentation](#further-documentation)
- [Troubleshooting](#troubleshooting)
- [Contributing](#contributing)
- [Security](#security)
- [Changelog](#changelog)
- [License](#license)

## Quick Start

The following three steps apply to an existing cluster of Zenlayer VPC instances
(Kubernetes >= 1.30). See [Installation](#installation) for what each step does
and [Requirements](#requirements) for the prerequisites.

```sh
# 1. Credentials. Not templated by the chart, so they never reach values.yaml,
#    git, or the Helm release. Only the controlplane mounts them; nodes do not.
kubectl create secret -n kube-system generic zenlayer-cni-access-key \
  --from-literal=AccessKeyID='<access-key-id>' \
  --from-literal=AccessKeyPassword='<access-key-secret>'

# 2. Install. Every --set below is required; rendering fails without them.
helm install zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.1 \
  --namespace kube-system \
  --set zec.ipamType=crd \
  --set controlplane.deployment.enabled=true \
  --set controlplane.regionID=<region-id> \
  --set-string zec.vpcID=<vpc-id> \
  --set-string 'zec.subnetIDs={<subnet-id-1>,<subnet-id-2>}' \
  --set-string zec.securityGroupID=<security-group-id> \
  --set zec.ipStack=ipv4 --set zec.manageUntaggedENI=false \
  --set cni.mode=chained --set cilium.enabled=false \
  --set networkPolicy.enabled=false

# 3. Wait for the controlplane and the DaemonSet and for every node's ZecNode to
#    be Ready, then check a pod actually gets a VPC address.
kubectl -n kube-system rollout status deploy/zenlayer-vpc-cni-controlplane
kubectl -n kube-system rollout status ds/zenlayer-vpc-cni --timeout=10m
kubectl get zn
kubectl run nettest --image=busybox --restart=Never -- sleep 3600
kubectl wait --for=condition=Ready pod/nettest --timeout=60s
kubectl get pod nettest -o jsonpath='{.status.podIP}{"\n"}'
kubectl delete pod nettest
```

The Pod IP should be an address from one of the subnets that you passed in.

Notable defaults: a warm pool of 2 IPs per node (`controlplane.scaler`), and one
controlplane replica, placed on a control-plane node if one exists
(`controlplane.deployment.nodeSelector` makes that placement a requirement).
The node that runs the controlplane needs internet egress.
The component that builds the Pod device depends on `cni.mode`; see
[Datapath](#datapath).

## Architecture

Three binaries are built from this repository:

| Binary | Source | Role |
| --- | --- | --- |
| `zec-cni` | `cmd/zenlayer-vpc-cni` | The CNI plugin kubelet execs on Pod sandbox setup/teardown. It performs no IPAM of its own: it dials the node daemon over gRPC and then wires up the Pod netns. |
| `zec-ipam` | `cmd/cni-ipam` | The per-node daemon (a DaemonSet, `hostNetwork: true`). It maintains the warm pool, serves the gRPC API, and writes how many addresses its node needs, and which it gives back, into its own `ZecNode`. It holds no credentials and never calls the OpenAPI. |
| `zec-controlplane` | `cmd/zec-controlplane` | The only component holding the OpenAPI credentials (a Deployment, `hostNetwork: true`, leader-elected through a Lease). It verifies each node's instance, creates interfaces and assigns and releases secondary IPs to meet each `ZecNode`'s demand, and writes the interfaces, addresses and capacity back into the `ZecNode`'s status. |

`ZecNode` (`zecnodes.network.zenlayer.com`, short name `zn`) is a cluster-scoped
object, one per Node. Only that node's daemon writes its spec, and only the
controlplane writes its status. An admission policy keyed on the node name in
the daemon's service account token restricts each node to its own object.
`kubectl get zn` shows the address state of every node.

`zec-cni` and the daemon communicate over gRPC on a Unix domain socket at
`/var/run/zenlayer/cni.socket`. Everything the plugin needs (the allocated IP,
the ENI it belongs to, the gateway, and the service CIDR) is returned in the
`AllocIP` response.

The daemon also:

- reads its configuration from the `zenlayer-cni-config` ConfigMap in
  `kube-system` (key `eni_conf`), and reads each interface's gateway and CIDR
  from the instance metadata service (link-local; no credentials are needed);
- persists resource state in BoltDB under `/var/lib/cni`;
- registers a kubelet device plugin that advertises the node's assignable
  address count as the extended resource `zenlayer.com/eni-ip`
  (`disableDevicePlugin` in `eni_conf` turns it off; the chart does not
  template this key, see [Configuration reference](#configuration-reference)).
  **This figure constrains scheduling only for workloads that themselves
  request `zenlayer.com/eni-ip`**; see [Capacity planning](#capacity-planning);
- serves `/healthz` and `/metrics` (Prometheus) on
  `healthAndDebugPort` (default `11414`) in the node's own network namespace.

An init container (`script/bootstrap/install_env.sh`) installs `zec-cni` and the
upstream `loopback` plugin onto the host. The init container does not install
the conflist. The daemon writes it once it is serving, so that kubelet does not
call the plugin before the daemon can answer. The daemon starts serving only
after the controlplane has verified the node's instance (`InstanceVerified=True`
on its `ZecNode`) and the warm pool is assembled.

Every `reconcileIntervalSec`, and sooner (within about a minute) when the
`ZecNode` status stops listing an address or interface the daemon holds, or
reports an interface as `Excluded`, the daemon runs a **GC** pass: it reconciles each pool against the
resources that Pods actually hold and releases what has leaked. This mechanism
appears under three names: the setting is `reconcileIntervalSec`, the logs say
`Daemon GC`, and the rest of this document says GC. The first pass after
startup is skipped, and repeated runs are debounced to one per minute.

A record whose Pod no longer exists is not reclaimed immediately. The first
pass that detects it marks it. The record is dropped, together with the routing
rules that name its address, once an hour has passed. The address therefore
returns to the pool **one hour to roughly one and a half hours** after the
daemon first detected it: one hour, then up to one GC interval plus its 20%
jitter (the GC interval is `reconcileIntervalSec`, 20 minutes by default). This timer
is held in memory and restarts with the daemon, so a daemon that restarts more
often than that never reaches the reclaim step. The effect is on capacity, not
on connectivity.

The daemon's GC compares against the `ZecNode` only; its path contains no
OpenAPI call. Reconciling against the platform (adopting addresses already on
the instance and sweeping orphaned interfaces no longer attached to any
instance) is the controlplane's job. The controlplane re-reconciles every node
every 5 minutes, and sweeps orphans when a leader takes over and then about
once an hour.

IP allocation uses a warm pool: the daemon keeps `controlplane.scaler.target`
free IPs ready, so Pod creation usually does not have to wait for a round trip
through the controlplane. `controlplane.scaler.targetMin` bounds the pool as a
whole (including addresses already held by Pods) rather than its idle part. The
upper bound is the node's capacity (`status.capacity.allocatable` on its
`ZecNode`); there is no separate setting for it. The controlplane writes both
values into every node's `ZecNode`, and a change takes effect without
restarting the daemons; see [Configuration reference](#configuration-reference).

While the controlplane is unavailable, existing Pods are unaffected, and new
Pods start as long as the pool has idle addresses. Growing the pool waits, and
addresses to be given back queue in the `ZecNode`'s `spec.ipv4.releasing`: they
are not returned yet, but they are not leaked either.

This plugin does *not* handle Service traffic. It contains no service load
balancer and no NetworkPolicy implementation; kube-proxy (or a replacement that
you install yourself) handles Services.

## Datapath

Which component builds the Pod device follows `cni.mode`:

- **`chained` (recommended)**: this plugin creates a veth pair
  (`pkg/plugin/datapath/policy_route.go`). The Pod reaches the host stack
  directly, so kube-proxy sees both directions of service traffic without
  additional handling. This is also the device model that Cilium's
  `generic-veth` CNI chaining expects.
- **`delegated`**: Cilium is the primary CNI and creates the Pod device (a
  netkit device or a veth, according to its own `bpf.datapathMode`). In this
  mode, this plugin builds no device at all. See
  [docs/deploy-cilium-netkit.md](docs/deploy-cilium-netkit.md).

**The per-ENI policy routing is identical in both modes and is always installed
by this plugin.** Each ENI gets its own route table (`1000 + ifindex`) that holds
the ENI's default route, and a `from <pod-ip> lookup <eni-table>` rule steers
the Pod's egress traffic into that table.

`zec.dataPath` renders into the conflist as `eniip_virtual_type` and accepts
only `veth`. It describes a device that this plugin builds itself, so it is not
rendered under `cni.mode=delegated`.

**This plugin performs no address translation (SNAT) and installs no iptables
rules of its own.** Pod addresses are secondary IPs on an ENI and are directly
routable inside the VPC, so east-west traffic is sent and received with the
Pod's own address and is not translated. **Reaching the public IPv4 internet
from a Pod therefore depends on a NAT gateway on the VPC side.** A NAT gateway
must be configured for the subnet that the Pod's address comes from; otherwise,
Pods have no public IPv4 egress. Public IPv6 follows a different path: the
platform assigns a public address to the interface directly; see
[Address families](#address-families).

**This plugin does not implement Pod bandwidth limiting.** The conflist does
not declare the standard CNI `bandwidth` runtime capability, and declaring it
would be false. Nothing in this plugin consumes `runtimeConfig.bandwidth`, so
kubelet would translate the bandwidth annotations and pass them to a plugin that
ignores them. The Pod would run unthrottled while the conflist claimed
otherwise.

Egress rate limiting is available through Cilium instead, if Cilium is enabled
(see [Network policy](#network-policy)). Cilium's bandwidth manager reads the
`kubernetes.io/egress-bandwidth` annotation from the Pod and paces traffic with
EDT. Enable it with `bandwidthManager.enabled=true` in Cilium's own values.
Consider the following before relying on it:

- **Egress only.** Cilium's documentation states that
  `kubernetes.io/ingress-bandwidth` is not supported, is not recommended, and is
  ignored.
- **Pods on a secondary ENI may not be limited.** Cilium selects the devices to
  shape by looking for the default route or the node's `InternalIP` /
  `ExternalIP`, which describe the primary ENI. A Pod whose IP is on a
  secondary ENI egresses through that ENI's own route table, which has neither.
  Check what is actually in effect with
  `cilium-dbg status | grep BandwidthManager`, which prints the device list.
- **It is driven by Pod annotations, not by the CNI chain, and needs no CRD.**
  The settings are annotations only (`kubernetes.io/egress-bandwidth`, and
  `bandwidth.cilium.io/priority` for priority banding). Cilium's Pod watcher
  acts on annotation changes, so editing the annotation on a running Pod takes
  effect without recreating the Pod.

This chart provides no bandwidth switch of its own.
`bandwidthManager.enabled` belongs to Cilium's release, which this chart can
neither read nor set. A switch in this chart could therefore only claim that the
feature is on, without being able to verify it.

See [docs/deploy-cilium-chained.md](./docs/deploy-cilium-chained.md) for the verification
steps and the remaining upstream limitations.

## Address families

`zec.ipStack` selects the address families a node serves:

| Value | Meaning |
| --- | --- |
| `ipv4` | Pods receive one IPv4 address. |
| `dual` | Pods receive one IPv4 and one IPv6 address. |
| `ipv6` | Not implemented. |

Consider the following four properties of `dual` before enabling it.

**Every configured subnet must carry both an IPv4 and an IPv6 CIDR.** Otherwise,
the controlplane stops at `waiting for the pod subnets` and does not start
serving (its `/healthz` returns 503); its log names the subnet and the missing
family. The check deliberately runs at startup rather than at allocation time:
a per-allocation warning is indistinguishable from a slow platform, and the
symptom would be a warm pool that never fills, with every Pod stuck in
`ContainerCreating` and a single warning line as the only clue.

**An address pair is indivisible.** A Pod receives its IPv4 and IPv6 addresses
as one unit. A resource that covers only one family is never handed to a Pod;
it is marked and reclaimed instead. This prevents a Pod from starting with half
an address set after a partial platform failure.

**IPv6 never constrains node capacity.** The platform routes one `/96` prefix
per interface, and the daemon on the node carves addresses out of it locally;
there is no per-address IPv6 API. A `/96` holds far more addresses than any
interface can serve, so `zenlayer.com/eni-ip` and the warm pool are sized on
IPv4 alone, and a node reports the same capacity in both modes. See
[Capacity planning](#capacity-planning).

**Public IPv6 is billed, and the charge type must be specified.** The platform
refuses to enable IPv6 on an interface in a subnet whose `ipv6Type` is `Public`
unless a charge type and a bandwidth cap are supplied, and this plugin is the
component that enables IPv6 on every interface it creates.
`zec.ipv6InternetChargeType` and `zec.ipv6Bandwidth` therefore determine what
the cluster is billed for. They are ignored on a subnet with private IPv6.
`ByTrafficPackage` additionally requires `zec.ipv6PackageSize`, and
`BandwidthCluster` additionally requires `zec.ipv6ClusterId`. Both are checked
by the chart at render time and again by the controlplane at startup. See
[Configuration reference](#configuration-reference).

The daemon maintains IPv6 allocation state itself (in its local record store
and from the Pod addresses reported by the API server), because addresses are
carved from the interface's prefix rather than requested individually. The
controlplane additionally reconciles IPv4 state against the platform.

Dual-stack in this plugin does **not** require the Kubernetes cluster itself to
be dual-stack. Layering Cilium chaining on top does; see
[Network policy](#network-policy) and
[docs/deploy-cilium-chained.md](docs/deploy-cilium-chained.md) §4.3.

## Network policy

NetworkPolicy is **not enforced by default**. This project ships no policy
engine, so the API server accepts policy objects and they are then ignored: a
rule that should block traffic silently does not.

`cilium.enabled=true` chains **upstream Cilium** after this plugin, using
Cilium's `generic-veth` chaining mode. `networkPolicy.enabled=true` declares
that policy must be enforced. With provider `cilium`, the second setting
requires the first; otherwise, the chart refuses to render rather than accept
policy objects that nothing will act on. The two settings are separate because
policy is not the only reason to deploy Cilium: Hubble observability needs the
agent and no policy. Under `cni.mode=chained`, the veth pair and the per-ENI
policy routing are unchanged, and Cilium adds endpoint registration and tc eBPF
policy enforcement on the host-side veth. Under `cni.mode=delegated`, Cilium
creates the device itself and enforces policy on it, while the per-ENI routing
remains the responsibility of this plugin. Cilium does not take over IPAM or
routing: the chainer hardcodes `ExternalIpam: true` and `RequireRouting: false`.

This project does not fork, patch, or vendor Cilium, and the chart does not
install it; deploy Cilium from its own upstream chart. Supported range:
**v1.15 - v1.18**, code-checked against v1.17.5. `k8sServiceHost=auto` in
Cilium's own chart requires v1.16 or later (see
[docs/deploy-cilium-chained.md](docs/deploy-cilium-chained.md)).

Consider the following five points before enabling it:

- **`cni.customConf=true` is mandatory; set `cni.exclusive=false` alongside it.**
  The first is a hard prerequisite of `generic-veth`. Cilium **has no conflist
  template for that mode**, so `customConf=false` contradicts it, and **Cilium
  itself rejects the combination**: the agent remains at `0/1 Running` and never
  becomes Ready (logging `invalid CNI chaining mode: generic-veth`), while
  **Pod networking and NetworkPolicy keep working**. A wrong setting does not
  degrade silently, lose policy, or drop Pods; it produces one permanent,
  visible failure indicator. The second setting is redundant in this mode (with
  `customConf=true`, the key never reaches the ConfigMap at all), but it takes
  effect again under a chaining mode that Cilium can render, and setting it has
  no cost.
- **After changing anything under `cni.*` or `extraConfig.*`, restart the Cilium
  agent explicitly.** These keys change only the ConfigMap and do not alter the
  Pod template, so `helm upgrade` does not roll the DaemonSet, and
  `kubectl rollout status ds/cilium` **returns success immediately, which is a
  false positive**. Use `kubectl -n kube-system rollout restart daemonset/cilium`.
- **L7 (HTTP) and FQDN (`toFQDNs`) policies work with Cilium's default
  `l7Proxy=true`; transparent encryption (IPsec/WireGuard) and Cilium
  Ingress/Gateway API are not available** in this mode. Do not change
  `l7Proxy`: **with it off, L7 and FQDN policies fail open**. The API server
  accepts the object, the agent never enforces its L7 part, and the traffic that
  it should deny is allowed; a policy that uses only `toFQDNs` is even reported
  as `Valid=True`. See the `l7Proxy` row in
  [docs/deploy-cilium-chained.md](docs/deploy-cilium-chained.md) §4.2.

- **A typo in the switch name is rejected rather than silently read as "off".**
  `values.schema.json` sets `additionalProperties: false` at the root and in
  every section, so `--set celium.enabled=true` (one letter wrong) fails at
  render time and names the offending key:
  `additional properties 'celium' not allowed`.

- **`zec.ipStack=dual` with chaining requires a dual-stack Kubernetes cluster.**
  Cilium must then be deployed with `ipv6.enabled=true`, which in turn requires
  every Node object to carry an IPv6 address. This is a cluster-level
  prerequisite that no Cilium value can supply. Neither half can be skipped.
  Without `ipv6.enabled=true`, the agent's per-veth BPF drops every IPv6 packet
  as an unsupported L3 protocol, and does so silently: the agent reports
  healthy, every endpoint lists the correct IPv6 address, and nothing is logged
  above info. With it enabled on a single-stack cluster, the agent refuses to
  start. This plugin's own dual-stack support does not depend on the cluster
  being dual-stack; only layering chaining on top introduces the requirement.
  See [docs/deploy-cilium-chained.md](docs/deploy-cilium-chained.md) §4.3.

Full instructions, the required values with the reason for each, verification
steps, and **the switching procedure in both directions** (plain veth <-> chained)
are in [docs/deploy-cilium-chained.md](./docs/deploy-cilium-chained.md).

Read that switching section before uninstalling Cilium: **the order is fixed, and
the rolling restart of workloads is not optional.** The tc/tcx BPF programs on a
Pod's veth are BPF links that outlive the agent, and `helm uninstall` does not
remove them. While the agent runs, it keeps their load-balancing maps current.
Once the agent is gone, **those maps freeze**, and Pods created during chaining
keep resolving ClusterIPs through a table that is never updated again. After any
subsequent change to a Service's backends, those Pods send traffic to endpoints
that no longer exist. The connections are dropped while the `KUBE-SVC-*`
counters stay at zero, so inspecting kube-proxy reveals nothing.

## Requirements

- Kubernetes >= 1.30. The centralized deployment restricts each node to its own
  `ZecNode` with a ValidatingAdmissionPolicy, and the chart refuses to render on
  a cluster without that API. (Direct mode requires only >= 1.28.2; see
  [docs/deploy-direct.md](docs/deploy-direct.md).)
- Linux nodes on `amd64`. The DaemonSet has a hard node affinity on
  `kubernetes.io/os=linux` and `kubernetes.io/arch=amd64`, and the image build
  refuses any other `GOARCH`.
- Nodes running as Zenlayer VPC instances, with a VPC, at most 5 subnets in
  that VPC for Pod IPs, and a security group for the ENIs that the controlplane
  creates.
- Zenlayer OpenAPI static credentials (access key ID + password) with
  permission to manage ENIs and their secondary IPs. Only the controlplane
  mounts them.
- **Public** outbound internet access from **the nodes that run the
  controlplane**, through an EIP bound to the node or a NAT gateway on its VPC.
  The Zenlayer OpenAPI is a public endpoint with no internal endpoint, and only
  the controlplane calls it. The controlplane prefers control-plane nodes; to
  keep it on the nodes that have egress, set
  `controlplane.deployment.nodeSelector`. The other nodes need no internet
  egress. This requirement is distinct from the Pod egress requirement below,
  which only limits what Pods can reach.
- Helm 3.14 or newer. The commands in this document use `oci://` (3.8),
  `--set-json` (3.10) and, for upgrades, `--reset-then-reuse-values` (3.14). In
  addition, before 3.14, `helm template` checks the chart's `kubeVersion`
  against a built-in version older than this chart accepts.
- Linux kernel >= 6.8 on every node **when** `cni.mode=delegated` runs Cilium with
  `bpf.datapathMode=netkit` or `netkit-l2`: Cilium documents netkit as available
  from kernel 6.8. The `chained` mode adds no kernel requirement of its own.
- kube-proxy, or another component that provides Service handling. Under
  `cni.mode=delegated`, that component is Cilium, and its `kubeProxyReplacement`
  is mandatory in that mode.
- If Pods need public IPv4 egress: a NAT gateway configured for the subnet that
  the Pod addresses come from. This plugin performs no SNAT; see [Datapath](#datapath).

## Installation

### 1. Create the credentials Secret

The chart does **not** template the credentials, so they never appear in
`values.yaml`, in git, or in the Helm release. Create the Secret separately in
`kube-system`:

```sh
kubectl create secret -n kube-system generic zenlayer-cni-access-key \
  --from-literal=AccessKeyID='<access-key-id>' \
  --from-literal=AccessKeyPassword='<access-key-secret>'
```

**Only the controlplane mounts it** (read-only). The controlplane reads the two
keys, `AccessKeyID` and `AccessKeyPassword`, as files and re-reads them every 60
seconds, so rotating the credentials requires no restart.

The controlplane also accepts a temporary (STS) credential in the same
directory: the keys `AccessKeyID`, `AccessKeySecret`, `SecurityToken` and
`Expiration` (an RFC 3339 time). The presence of a `SecurityToken` key selects
this form, and `AccessKeyPassword` is then ignored. Whatever renews the
credential must rewrite these files before `Expiration`; the controlplane picks
up the renewal on its next read. A credential that has expired without being
renewed is still used, and an error is logged. The daemons on the
nodes mount no credentials at all. If the Secret has a different name, pass
`--set zec.secretName=<name>`.

### 2. Install the chart

```sh
helm install zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.1 \
  --namespace kube-system \
  --set zec.ipamType=crd \
  --set controlplane.deployment.enabled=true \
  --set controlplane.regionID=<region-id> \
  --set-string zec.vpcID=<vpc-id> \
  --set-string 'zec.subnetIDs={<subnet-id-1>,<subnet-id-2>}' \
  --set-string zec.securityGroupID=<security-group-id> \
  --set zec.ipStack=ipv4 --set zec.manageUntaggedENI=false \
  --set cni.mode=chained --set cilium.enabled=false \
  --set networkPolicy.enabled=false
```

> **Use `--set-string` for the IDs.** `--set` infers the value's type, and an
> all-digit ID becomes an int64 that the schema rejects with
> `got number, want string`. Zenlayer IDs are all digits, so this problem occurs
> on the very first install.

- `zec.ipamType=crd` selects the centralized deployment, and
  `controlplane.deployment.enabled=true` makes the chart run the controlplane in
  this cluster. `controlplane.regionID` (the region that contains `zec.vpcID`),
  `zec.vpcID`, `zec.subnetIDs` and `zec.securityGroupID` are then required;
  rendering fails without them. They configure the controlplane and never reach
  the nodes.
- When the platform runs the controlplane outside the cluster, pass
  `--set controlplane.platformUser=<user>` instead. This is the user as which
  the platform's controlplane authenticates to this cluster; the chart grants it
  the permissions it needs. Omit `controlplane.deployment.enabled`, the Secret
  from step 1, and the VPC, subnet, security group and region above: the
  platform configures those. Set exactly one of the two options; otherwise, the
  chart refuses to render.
- The behavior switches `zec.ipamType`, `zec.ipStack`, `zec.manageUntaggedENI`,
  `cni.mode`, `cilium.enabled` and `networkPolicy.enabled` are also required.
  They have no default, so a changed default in a later chart version can never
  flip one of them on an existing cluster. Pass them again on every
  `helm upgrade`, or use `--reset-then-reuse-values` (Helm 3.14 or later), which
  carries them forward while taking the defaults of new keys and `image.tag`
  from the new chart. `--reuse-values` also keeps the old chart's defaults, so
  new keys are missing and the image is never upgraded. A values file kept in
  version control also works. (With `platformUser`, `zec.manageUntaggedENI` is
  configured on the platform side and is not passed.) A release installed with
  1.0.0 set these switches through that chart's defaults, so no reuse flag
  carries the right values forward: to upgrade from 1.0.0, follow
  [docs/deploy-direct.md](docs/deploy-direct.md) §3.4.
- The `ZecNode` CRD is in the chart's `crds/` directory, which Helm installs
  on the **first install only** and never upgrades. Before upgrading the chart,
  apply the new CRD manually:
  `helm pull oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.1 --untar`,
  then `kubectl apply --server-side -f zenlayer-vpc-cni/crds/zecnodes.yaml`.
- The controlplane runs from its own image, `<image.repository>-controlplane`,
  with the same tag; the node image does not contain it. A registry mirror or an
  offline install requires both images.

A dual-stack install also requires the IPv6 billing values, because the
controlplane enables public IPv6 on every interface it creates.
`zec.ipv6InternetChargeType` defaults to `ByTrafficPackage`, which requires a
package size. A dual-stack install therefore replaces `zec.ipStack=ipv4` in the
command above and adds either

```sh
  --set zec.ipStack=dual \
  --set zec.ipv6InternetChargeType=ByBandwidth --set zec.ipv6Bandwidth=1
```

or, for a traffic package, `--set-json zec.ipv6PackageSize=0.1` alongside the
default charge type. See [Address families](#address-families) for what each
option bills.

To preview the rendered manifests first, run the following. An offline render
must declare the cluster's version and the admission policy API; otherwise, the
chart assumes a cluster older than 1.30 and refuses to render.

```sh
helm template zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.1 \
  --kube-version 1.30.0 \
  --api-versions admissionregistration.k8s.io/v1/ValidatingAdmissionPolicy \
  --set zec.ipamType=crd \
  --set controlplane.deployment.enabled=true \
  --set controlplane.regionID=<region-id> \
  --set-string zec.vpcID=<vpc-id> \
  --set-string 'zec.subnetIDs={<subnet-id>}' \
  --set-string zec.securityGroupID=<security-group-id> \
  --set zec.ipStack=ipv4 --set zec.manageUntaggedENI=false \
  --set cni.mode=chained --set cilium.enabled=false \
  --set networkPolicy.enabled=false
```

### 3. Verify

```sh
# 1. the controlplane and every daemon pod ready, no restarts
kubectl -n kube-system rollout status deploy/zenlayer-vpc-cni-controlplane
kubectl -n kube-system rollout status ds/zenlayer-vpc-cni --timeout=10m
kubectl -n kube-system get pod -l 'app in (zenlayer-vpc-cni,zenlayer-vpc-cni-controlplane)' -o wide

# 2. every node's ZecNode: READY True, ALLOCATION Satisfied, ALLOCATABLE the node's capacity
kubectl get zn

# 3. a new pod gets an address from the VPC subnet (not the kubeadm podCIDR)
kubectl run nettest --image=busybox --restart=Never -- sleep 3600
kubectl wait --for=condition=Ready pod/nettest --timeout=60s
kubectl get pod nettest -o jsonpath='{.status.podIP}{"\n"}'
kubectl delete pod nettest

# 4. on a node: the conflist landed with mode 0600, the binary carries version info
ssh <node> 'ls -l /etc/cni/net.d/10-zenlayer-cni.conflist; /opt/cni/bin/zec-cni version'

# 5. on a node: pool state
ssh <node> 'curl -s 127.0.0.1:11414/metrics | grep -E "^resource_pool_"'
ssh <node> 'curl -s 127.0.0.1:11414/healthz'
```

Note the following four points:

- **`/healthz` and `/metrics` are served on every node address, not only on
  loopback.** The daemon runs with hostNetwork and binds `:11414` so that
  Prometheus can scrape it from outside the node; kubelet's probes go to
  `127.0.0.1`. The examples above use ssh by convention, not because the port is
  restricted. The image has no `curl`, but it does have `wget`, so
  `kubectl exec` reaches the same endpoints without leaving the cluster:

  ```sh
  POD=$(kubectl -n kube-system get pod -l app=zenlayer-vpc-cni \
          --field-selector spec.nodeName=<node> -o jsonpath='{.items[0].metadata.name}')
  kubectl -n kube-system exec $POD -c zec-ipam -- \
    wget -qO- http://127.0.0.1:11414/metrics | grep -E '^resource_pool_'
  ```

  That listener serves only `/healthz` and `/metrics`; it deliberately exposes
  no pprof.
- **`zec-cni version` should print a non-empty version, commit, and build time.**
  `CNI Version: unknown` or `Git Commit ID: DEFAULT` means that git could not
  provide the information during the build, so the image carries no provenance.
  Empty fields mean that the link-time flags were not passed at all, which is
  the result of a plain `go build`.
- The daemon's `/healthz` starts answering only after the controlplane has
  verified the node and the warm pool is set up. For this reason, the DaemonSet
  allows a generous startup window (360 x 10s): a new node waits for the
  controlplane to verify its instance and, if needed, create interfaces.
  `/healthz` reports the state of the daemon, not of the node. The conflist is
  written after that point, so with Cilium chaining the daemon's `/healthz`
  answers `ok` while the daemon is still waiting for the Cilium agent and the
  node cannot start Pods. The DaemonSet's probes are wired to `/healthz`, so a
  node in that state stays Ready; also monitor the restart count and the
  `Still waiting for the cilium agent` log line. See
  [docs/deploy-cilium-chained.md](docs/deploy-cilium-chained.md) §4.1.
- The controlplane's own `/healthz` is on `:11416`. Until the controlplane is
  ready, it answers 503 and states what it is waiting for (see
  [Troubleshooting](#troubleshooting)).

## Capacity planning

The number of Pod addresses a node can hand out follows from its instance size:

```text
capacity = (ENI quota - ENIs attached outside this plugin - (1 if the primary ENI is excluded, else 0)) x (addresses per ENI - 1)
```

The table below assumes the primary ENI's subnet is listed in `zec.subnetIDs`.

| Size | ENI quota | Addresses per ENI | Assignable |
|---|---|---|---|
| 8 vCPU | 2 | 15 | **28** |
| 16 vCPU | 4 | 30 | **116** |

Every ENI reserves one address for itself (both the instance's own ENI and the
ENIs that this plugin creates), so each ENI contributes one address fewer than
its per-ENI limit.

The instance's primary ENI serves Pods only when its subnet is listed in
`zec.subnetIDs`. Otherwise, it receives no new Pod address and no IPv6 prefix,
and its share (addresses per ENI - 1) is the excluded-primary term in the
formula; it is **not** counted in `eniUnmanaged`. In crd mode, its state in
`kubectl describe zn` is `Excluded`; in direct mode, the daemon instead records
a **Normal** `InterfaceExcluded` event on the Node. The same applies to an ENI
that this plugin created in a subnet later removed from `zec.subnetIDs`; such an
ENI is also freed once its Pods are gone. Addresses that Pods already hold on
such an interface are released as those Pods terminate, and in both modes the
capacity decreases gradually with them, because only the unused room on the
interface is deducted. Idle addresses that the daemon holds there are given
back at its next GC (in crd mode, within about a minute). An address on the
instance's own primary interface that the daemon has no record of is left in
place, because it may belong to an operator. After an upgrade, this includes
warm addresses from before the upgrade; remove them manually if they are not
needed. If no other interface can serve Pods (the ENI quota is full, or is taken
by unmanaged ENIs or by interfaces that the platform refused an IPv6 prefix),
the node holds no Pods at all; in crd mode, `ALLOCATION` then shows
`NoPodInterface` (see [Troubleshooting](#troubleshooting)).

Secondary ENIs attached to the node by other means are left alone unless
`zec.manageUntaggedENI=true`. In that case, they are **adopted** if they are in
one of the configured Pod subnets: their addresses join the pool and count
towards the figures above.

> **Warning:** **An adopted ENI is detached _and deleted_ when the pool
> shrinks**, and under `zec.ipStack=dual` the controlplane enables IPv6 on it,
> which is a billable operation on a subnet whose `ipv6Type` is `Public`. With
> adoption turned on, exclude an ENI attached for another purpose (storage,
> management, monitoring) by tagging it `zenlayer:cni:no-manage=true`.

A secondary ENI that is **not** adopted (because adoption is off, the interface
is tagged, or it is outside the configured Pod subnets) is left completely
untouched: no new Pod receives an address from it, and nothing detaches it. It
still occupies one of the instance's ENI slots, however, so the node's capacity
is lower than the table predicts by one ENI's worth of addresses. `eniUnmanaged`
below counts these ENIs.

The controlplane writes the values it resolved into every node's `ZecNode`.
This is the quickest way to check a node whose capacity differs from what the
table predicts:

```sh
kubectl get zn <node> -o jsonpath='{.status.capacity}{"\n"}'
# {"allocatable":58,"eniQuota":4,"eniUnmanaged":2,"ipv4PerENI":30}
```

`eniUnmanaged` is the number of ENIs that this plugin does not manage.
`allocatable` is the capacity remaining after those ENIs and after every
interface that takes no new address: an `Excluded` interface (the primary, or an
interface that this plugin created in a subnet no longer in `zec.subnetIDs`) or,
under `dual`, an interface that was refused an IPv6 prefix. `allocatable` is the
ALLOCATABLE column of `kubectl get zn`. The node advertises slightly less: the
device plugin subtracts the Pods that do not request `zenlayer.com/eni-ip`.

**These figures are IPv4 addresses, and they do not change under
`zec.ipStack=dual`.** The IPv6 half of a Pod's address pair is carved locally
from the ENI's `/96` prefix, which holds far more addresses than any ENI can
serve, so IPv6 never becomes the binding constraint. A node reports the same
`zenlayer.com/eni-ip` capacity in both modes.

> **The real ceiling is usually kubelet's `maxPods` (default 110), not this
> number.** On a 16 vCPU node, the plugin offers 116 addresses, while kubelet
> schedules only 110 Pods, minus the hostNetwork system Pods. Plan for the lower
> of the two; using the full figure also requires raising `maxPods`.

The device plugin advertises the assignable count as the extended resource
`zenlayer.com/eni-ip`: capacity minus the Pods that hold an address without
requesting the resource. The count is recalculated when such a Pod starts or
terminates, when an address is given back, and when the instance limit changes,
with a periodic recount as a fallback. Because those Pods are subtracted, the
figure is below the node's actual capacity by their number. It is a lower bound
and never an overstatement; read the node's `resource_pool_*` metrics for the
live pool state.

**The figure constrains only Pods that request the resource.** The scheduler
filters on the resources that a Pod explicitly names in `resources.requests`,
and this plugin does not inject that request. A Pod with an empty `resources`
is therefore placed on a node whose `zenlayer.com/eni-ip` is already `0`, and
then remains in `ContainerCreating` with no address until CNI ADD times out and
kubelet retries.

To make the scheduler place Pods by address capacity, do one of the following:

- declare `resources.requests: {zenlayer.com/eni-ip: "1"}` (with a matching
  `limits`) on the workloads themselves; or
- set kubelet's `--max-pods` to the instance's assignable address count, using
  the formula above, to bound Pods per node instead.

One node-side fallback exists. For a Pod annotated
`k8s.zenlayer.com/pod-evict-policy: allow`, the daemon compares the node's
recorded Pod count against its capacity before allocating. Once the count has
reached that capacity, the daemon evicts the Pod so that it is rescheduled
elsewhere, rather than leaving it stuck. Pods without the annotation are
unaffected.

## Configuration reference

All values below are defined in `chart/values.yaml`. Where a `zec.` value is
rendered depends on which component reads it, and the prefix alone does not
indicate the split:

| Rendered into | Values |
| --- | --- |
| The `zenlayer-vpc-cni-controlplane` ConfigMap (read by the controlplane) | `zec.vpcID`, `zec.subnetIDs`, `zec.securityGroupID`, `zec.ipStack`, `zec.manageUntaggedENI`, `zec.ipv6*`, `controlplane.regionID`, `controlplane.identity`, `controlplane.scaler`, `controlplane.nodeGroups` |
| The `eni_conf` key of the `zenlayer-cni-config` ConfigMap (read by the daemon) | The rest of `zec.`: the values the daemon itself uses (such as `zec.ipStack`, `zec.serviceCidr`, `zec.poolMonitorIntervalSec`) |
| The CNI conflist | `zec.dataPath` |
| Both | `zec.logLevel` |
| The DaemonSet itself, not any ConfigMap | `zec.k8sServiceHost`, `zec.k8sServicePort`, `zec.logLevel` (as `ZENLAYER_CNI_LOGLEVEL`) and `zec.healthAndDebugPort` (as the probe ports) |
| The controlplane Deployment itself | `zec.secretName` (as the mounted Secret), `controlplane.deployment.*`, `zec.k8sServiceHost` / `zec.k8sServicePort` |

A per-node configuration override replaces `eni_conf` only, so it can change
neither what the controlplane reads nor the "DaemonSet itself" row; see below.

| Value | Default | Description |
| --- | --- | --- |
| `image.repository` | `docker.io/zenlayer1/zeccni` | Image for the init container and `zec-ipam`. The controlplane's image is this name with `-controlplane` appended, with the same tag. |
| `image.tag` | `v2.0.1` | Image tag of both images. |
| `image.pullPolicy` | `IfNotPresent` | Image pull policy. |
| `zec.vpcID` | `""` | **Required** with `controlplane.deployment.enabled` (and in direct mode); unused with `controlplane.platformUser`. Zenlayer VPC ID. All subnets must belong to this VPC. |
| `zec.subnetIDs` | `[]` | **Required** under the same conditions as `zec.vpcID`. The only subnets that Pod IPs come from; interfaces that the plugin creates are placed in them. A node's primary interface serves Pods only when its subnet is listed. Otherwise, the node holds fewer Pods by that interface's share, and a node where no other interface can serve Pods holds none (in crd mode, `NoPodInterface`). On upgrade, existing nodes whose primary subnet is not listed lose that share as the Pods holding addresses there terminate, and those addresses are released with them; see [Capacity planning](#capacity-planning) for idle addresses. At most 5, all in the same VPC. |
| `zec.securityGroupID` | `""` | **Required** under the same conditions as `zec.vpcID`. Security group applied to the ENIs that this plugin creates. |
| `zec.ipamType` | **required** | Which component calls the Zenlayer OpenAPI. `crd` (the centralized deployment described in this document): only the controlplane; nodes hold no credentials, and each node states what it needs in its `ZecNode` object (`kubectl get zn`); requires Kubernetes 1.30 or later. `direct`: every node's daemon, with the credentials mounted on every node; see [docs/deploy-direct.md](docs/deploy-direct.md). |
| `controlplane.deployment.enabled` | `false` | Run the controlplane in this cluster (a Deployment, `hostNetwork: true`). Set exactly one of this value and `controlplane.platformUser`. |
| `controlplane.platformUser` | `""` | When the platform runs the controlplane outside the cluster: the user as which that controlplane authenticates to this cluster; the chart grants it the permissions it needs. The platform then supplies the controlplane's configuration, and `controlplane.regionID`, `scaler`, `nodeGroups`, `identity` and `deployment.*` are unused. |
| `controlplane.regionID` | `""` | Region of the Pod subnets and of every node. The controlplane looks up interfaces and subnets in this region, and refuses a node whose instance is in another region of the same VPC (`IdentityMismatch`). Required with `deployment.enabled`. |
| `controlplane.scaler.target` | `2` | Target number of warm (pre-allocated, unused) IPs per node. Must be >= 1. |
| `controlplane.scaler.targetMin` | `1` | Lower bound on the pool as a whole, including addresses held by Pods, not on the idle part alone. The upper bound is the node's capacity; there is no setting for it. |
| `controlplane.nodeGroups` | `[]` | Warm-pool sizing per node label. The first group whose `selector` matches takes effect, e.g., `[{selector: {pool: large}, scaler: {targetMin: 10, target: 20}}]`. |
| `controlplane.identity.providerID` | `auto` | `auto`: when a Node has no `spec.providerID`, the controlplane finds its instance by the Node's InternalIP, checks it against the instance ID that the daemon reads from DMI, and writes `zenlayer:///<regionId>/<instanceId>`, the form that zenlayer-cloud-controller-manager writes. It is written only once, because the API server never allows it to change. `manual`: never write it; serve only Nodes whose providerID is already set in that form, or in the older `zenlayer://<instanceId>` form. |
| `controlplane.deployment.image` | `""` | Empty: `<image.repository>-controlplane:<image.tag>`. The controlplane runs as an unprivileged user (65534) and reads the Secret through its group (mode 0440, `fsGroup`), so an image set here must also run as that user. |
| `controlplane.deployment.nodeSelector` | `{}` | Where the controlplane must run. Empty: anywhere, preferring nodes labeled `node-role.kubernetes.io/control-plane`, because the controlplane mounts the credentials and a worker runs tenant workloads. The node it runs on needs internet egress. |
| `controlplane.deployment.replicas` | `1` | Controlplane replicas. They are leader-elected, so only one works at a time. A second replica takes over sooner, and keeps working when a rollout brings a configuration that never becomes ready (with one replica, the old Pod is removed first). The health port is a host port, so at most one replica runs per node: more replicas require as many nodes on which the controlplane may run. |
| `controlplane.deployment.workers` | `4` | Number of nodes that the controlplane reconciles concurrently. |
| `controlplane.deployment.resources` | `requests: {cpu: 50m, memory: 64Mi}` | The controlplane container's resources. |
| `zec.ipStack` | **required** | Address families assigned to Pods: `ipv4` or `dual`. `dual` requires every subnet to carry both an IPv4 and an IPv6 CIDR. IPv6-only is not implemented. See [Address families](#address-families). |
| `zec.ipv6InternetChargeType` | `ByTrafficPackage` | How the platform bills public IPv6 on an interface: `ByTrafficPackage`, `ByBandwidth` or `BandwidthCluster`. Used only when `ipStack` is `dual`; ignored on a subnet with private IPv6. |
| `zec.ipv6Bandwidth` | `10000` | Public IPv6 bandwidth cap in Mbps per interface. Used only when `ipStack` is `dual`. Must be >= 1. |
| `zec.ipv6PackageSize` | `0` | Traffic package size in TB. Required (positive, a multiple of 0.1) when `ipv6InternetChargeType` is `ByTrafficPackage`. Use `--set-json`, because `--set` keeps fractions as strings. |
| `zec.ipv6ClusterId` | `""` | Shared bandwidth package that the interface joins. Required when `ipv6InternetChargeType` is `BandwidthCluster`. |
| `zec.networkMode` | `eni_multi_ip` | Network mode. Only `eni_multi_ip` is supported. |
| `zec.dataPath` | `veth` | Pod datapath, rendered as `eniip_virtual_type`. `veth` is the only accepted value. The key is not rendered under `cni.mode=delegated`, where this plugin builds no device; see [Datapath](#datapath). |
| `zec.serviceCidr` | `""` | Service CIDR(s), comma-separated. **Leave it empty only on a kubeadm cluster**: an empty value makes the daemon read the service subnet from `kube-system/kubeadm-config`, and a failed lookup prevents the daemon from starting. No part of the datapath acts on the value (service traffic already traverses the host stack), but the daemon still requires an IPv4 service CIDR from one of the two sources. On a dual-stack cluster, a missing IPv6 entry is only logged. |
| `zec.secretName` | `zenlayer-cni-access-key` | Name of the pre-created Secret that holds the OpenAPI credentials. Mounted on the controlplane only (on every node in direct mode). |
| `zec.poolTarget`, `zec.poolTargetMin`, `zec.poolTargetMax`, `zec.poolTargetCapProbe` | see `values.yaml` | **Direct mode only**; see [docs/deploy-direct.md](docs/deploy-direct.md). In the centralized deployment, `controlplane.scaler` sizes the pool. |
| `zec.manageUntaggedENI` | **required** | Adopt secondary interfaces attached to the node that this plugin did not create. `false` leaves them untouched; with `true`, tag an interface `zenlayer:cni:no-manage=true` to leave it untouched. See [Capacity planning](#capacity-planning). |
| `zec.reconcileIntervalSec` | `1200` | The daemon's GC interval, in seconds. See [Architecture](#architecture). |
| `zec.poolMonitorIntervalSec` | `120` | Pool monitor interval, in seconds. |
| `zec.healthAndDebugPort` | `11414` | Port for `/healthz` and `/metrics`. The Pod uses `hostNetwork`, so this port is occupied on the node. |
| `zec.logLevel` | `""` | Log level for both binaries (`trace｜debug｜info｜warn｜error｜fatal｜panic`); empty keeps the built-in default (`info`). It sets `ZENLAYER_CNI_LOGLEVEL` on the DaemonSet. `debug` or `trace` additionally sets `"debug": true` in the conflist (in the `ipam` block when `zec-cni` runs as Cilium's delegated IPAM, because the top-level keys of that entry belong to Cilium). This is the only channel available to `zec-cni`: kubelet execs it from the host, so it never sees the Pod's environment. |
| `zec.k8sServiceHost` | `""` | Sets `KUBERNETES_SERVICE_HOST` on the daemon and the controlplane. Empty with `cilium.enabled=true`: read from `kube-public/cluster-info` at install/upgrade time (the port is taken from its URL, default 443). Reason: where Cilium serves Service traffic in place of kube-proxy, as this project's Cilium documents configure it, the in-cluster API server Service answers only once Cilium is up, and on a dual-stack node Cilium waits for the IPv6 address that this daemon's registration provides. Where the value cannot be read (under `helm template`, a client-side dry run or a GitOps tool, or where that ConfigMap is missing because the cluster is not a kubeadm cluster), the chart **refuses to render** until this value is set, rather than fall back to the in-cluster Service; set `zec.k8sServicePort` together with it (see the next row). The value is whatever `cluster-info` holds, which may be a single master's address, and a changed entry point requires another `helm upgrade`. Empty otherwise: the in-cluster Service. Setting it explicitly to a load balancer or VIP is preferred. |
| `zec.k8sServicePort` | `""` | Sets `KUBERNETES_SERVICE_PORT`; read together with the host whenever the host is read. Set it whenever you set the host. If left empty, the in-cluster Service port 443 that kubelet injects is used, and a direct API server entry point (typically 6443) usually does not listen on that port. |
| `cni.mode` | **required** | Which plugin creates the Pod's network device. `chained`: this plugin creates a veth pair, and Cilium, if enabled, is chained after it. `delegated`: Cilium creates the device, and this plugin only supplies addresses and installs host per-ENI routing; requires `cilium.enabled=true`. This mode is **verified end to end on a test cluster but not in production**, and is **not a reason to expect higher performance** (see [docs/performance.md](docs/performance.md)); steps are in [docs/deploy-cilium-netkit.md](docs/deploy-cilium-netkit.md). |
| `cilium.enabled` | **required** | Chain the upstream `cilium-cni`. With `cni.mode=chained`, it runs **after** this plugin (`generic-veth` chaining); with `cni.mode=delegated`, it is the primary plugin and this plugin is placed in its `ipam` block. Does not install Cilium. See [Network policy](#network-policy). |
| `cilium.waitTimeout` | `""` | How long the daemon waits for the Cilium agent socket before restarting. Go duration; empty keeps the 5m default. |
| `networkPolicy.enabled` | **required** | Declare that NetworkPolicy must be enforced; enabling it requires `cilium.enabled=true`. It enforces nothing itself: with `cilium.enabled=true`, the Cilium agent enforces policy regardless of this setting, and with Cilium off, nothing enforces policy. |
| `networkPolicy.provider` | `cilium` | Engine expected to enforce policy. Only `cilium` is accepted. |
| `resources.requests.cpu` | `250m` | CPU request for `zec-ipam`. |
| `resources.requests.memory` | `256Mi` | Memory request for `zec-ipam`. |
| `resources.limits` | unset | Accepted and rendered onto the container, but unset by default. The daemon is on every Pod's CNI ADD path, so a CPU limit throttles Pod startup node-wide, and a memory limit turns a spike into an OOMKill that takes IPAM down with it. |

The daemon's configuration schema (`pkg/daemon/config/config.go`) accepts
several additional fields that the chart does not template, including
`disableDevicePlugin`, `extraRoutes`, `poolCapRatio`, `poolGCPeriodSec`,
`subnetStatAgingSec` and `subnetStatUpdateIntervalSec`. Set them by editing the
`eni_conf` JSON in the ConfigMap directly. With `ipamType: crd`, the daemon
refuses to start if `poolCapRatio`, `subnetStat*` or `rateLimit` is present in
`eni_conf`. The first two have no counterpart in that mode (the controlplane
sizes pools by `controlplane.scaler` and refreshes subnet counts itself), and
the controlplane has its own `rateLimit`, shaped
`{readQPS, readBurst, writeQPS, writeBurst}`.

In direct mode, `rateLimit` is parsed but has no consumer: setting it changes
nothing, and no warning indicates this. Do not use it to bound OpenAPI call
rates.

A node can also be pointed at a per-node ConfigMap with the
`k8s.zenlayer.com/vpc-cni-config` node label, whose value is
`<namespace>.<name>`. Note the following five points, which are easy to get
wrong:

- **The namespace must be the daemon's own.** A reference to another namespace
  is refused, and the daemon does not start. The label is on the Node, and a
  kubelet may set labels on its own Node with any prefix outside
  `kubernetes.io`; this prefix is outside it. Without the restriction, a
  compromised node could point its own daemon at a configuration that it wrote,
  and `openAPIDomain` in `eni_conf` is the host to which the daemon sends its
  credentials.

- **It covers `eni_conf` and nothing else.** The daemon reads exactly that one
  key from the ConfigMap that the label names, and fails to start if the key is
  absent. Everything in the "DaemonSet itself" row above (the log level
  environment variable, the probe ports, the mounted Secret) comes from the
  DaemonSet and cannot be changed per node this way.
- **It replaces rather than merges.** The named `eni_conf` is used in its
  entirety; nothing is inherited from the cluster-wide one. An omitted key is
  not "kept at the cluster value"; it is unset, and the daemon applies its own
  default or refuses to start.
- **The CNI conflist still comes from the cluster-wide ConfigMap.** Therefore,
  `zec.dataPath` cannot be overridden per node either.
- **In the centralized deployment, it may contain no platform setting.** The
  VPC, subnets, security group, `manageUntaggedENI` and IPv6 billing belong to
  the controlplane, and the daemon refuses to start if they are present in its
  `eni_conf`. Use `controlplane.nodeGroups` to size node groups differently.

### Applying a configuration change

**The daemon's configuration is not applied live.** Every value in `eni_conf`
is read once at startup, so a change takes effect on a node only when its Pod
restarts. Reading the configuration once and requiring a restart is a
deliberate choice: a value that can change at runtime would have to be safe to
change at every point in the daemon's execution.

**The only exception is warm-pool sizing.** The controlplane reads
`controlplane.scaler` and `controlplane.nodeGroups` and passes them to every
node through its `ZecNode`; the daemon adjusts its pool on receipt, without
restarting. The controlplane's Deployment also carries a `checksum/config`
annotation, so a change triggers a rollout of it; existing Pods are unaffected
in the meantime.

`helm upgrade` handles this correctly without further action. The DaemonSet
carries a `checksum/config` annotation over the rendered ConfigMap, so changing
any value changes the Pod template and triggers a rolling update.

Editing the ConfigMap manually does **not** restart anything. Afterwards, run:

```sh
kubectl -n kube-system rollout restart ds/zenlayer-vpc-cni
```

Run the same command after a **failed** `helm upgrade`. Helm applies objects
one at a time, so an upgrade that fails partway may already have rolled the
DaemonSet without having written the ConfigMap; the new Pods then start with the
old values. A later successful upgrade does not correct this: the Pod template
is unchanged by then, so nothing rolls a second time, and those nodes remain on
the old configuration indefinitely.

The CNI conflist on each node is written once when the daemon starts and is not
re-read afterwards, so the same rule applies to `dataPath` and the other
conflist fields.

## Uninstalling

`helm uninstall` removes the Kubernetes objects that the chart rendered. **The
`ZecNode` CRD and every `ZecNode` object remain** (Helm never deletes a CRD from
`crds/`); delete them manually once they are no longer needed:

```sh
kubectl delete crd zecnodes.network.zenlayer.com   # ZecNode has no finalizer; deleting it makes no platform call
```

It also deliberately leaves the following node-local state in place:

| Left on every node | |
| --- | --- |
| `/etc/cni/net.d/10-zenlayer-cni.conflist` | the CNI configuration |
| `/opt/cni/bin/zec-cni` | the CNI binary |
| `/var/lib/cni/zenlayer/` | the address records |
| `/var/run/zenlayer/`, `/var/log/zenlayer/` | the log directory; the socket itself is removed with the Pod |
| `/etc/sysctl.d/99-zenlayer-vpc-cni.conf` | the `rp_filter` drop-in |

**The conflist is the significant item.** While it is present with no daemon
behind it, kubelet still assumes that this CNI owns the node's Pod networking,
and new Pods remain in `ContainerCreating` rather than falling back to another
plugin.

```sh
rm -f /etc/cni/net.d/10-zenlayer-cni.conflist
```

**Remove it only if another CNI's conflist remains in `/etc/cni/net.d/`.** On a
node where this is the only plugin, removing it leaves kubelet with no network
plugin at all: the node becomes `NotReady` with `cni plugin not initialized`,
Pods still cannot start, and after five minutes the
`node.kubernetes.io/not-ready:NoExecute` taint begins evicting running Pods.
On such a node, leaving the file in place is the less harmful failure; drain
the node instead.

ENIs that the CNI created remain attached to their instances. This trade-off is
deliberate: detaching an ENI is a slow API call, and a slow API call does not
belong in a Pod's termination grace period. Tying teardown to it would require
either a grace period long enough to penalize every normal restart, or a
detach that is killed halfway.

While a crd-mode controlplane runs, it sweeps (when a leader takes over, and
then about once an hour) the VPC's interfaces that meet all of the following
conditions: they are no longer attached to an instance, have been untouched for
over 10 minutes, carry `zenlayer:cni:created-by=zenlayer-vpc-cni`, and have a
`zenlayer:cni:cluster-id` tag that is either this cluster's or absent. It never
touches an interface that is still attached, nor one tagged with another
cluster's id; once the controlplane is gone, nothing sweeps them. Direct-mode
daemons sweep only their own instance's interfaces, never the VPC. The cluster
id is the UID of the `kube-system` namespace. An interface with no cluster id
(created before this version, by a cluster on an older version, or by a
direct-mode daemon that could not read `kube-system`) is swept by the
controlplane of any crd-mode cluster in the VPC. Clusters that share a VPC are
therefore isolated from each other only once all of them run this version.
**Record the cluster id before uninstalling or deleting the cluster**
(`kubectl get ns kube-system -o jsonpath='{.metadata.uid}'`), **then check the
account for detached ENIs carrying both `zenlayer:cni:created-by=zenlayer-vpc-cni`
and `zenlayer:cni:cluster-id=<that UID>`.** Filtering by `created-by` alone
also matches other clusters' interfaces in a shared VPC. Interfaces created
before this version carry no cluster id; find them by `created-by` together
with `zenlayer:cni:instance-id` equal to the instance ID of one of the
cluster's nodes.

**Secondary addresses are also left behind, and the interface tags do not
locate them.** Whatever the warm pool held at uninstall time remains assigned
to its interface, and an address carries no tag of its own; only the interface
does. Addresses left on a node's **primary** ENI are therefore invisible to a
tag-based search. Measured on a five-node cluster: 10 addresses remained after
`helm uninstall`, exactly the warm-pool target (2 by default) per node, and 5 of
them were on primary ENIs with no tag at all. Reinstalling the chart on the same
nodes adopts them again (those on a primary ENI only when its subnet is listed
in `zec.subnetIDs`). If the nodes are being retired, list the secondary
addresses of each instance's interfaces and release them explicitly. Save the
output of `kubectl get zn -o yaml` before uninstalling: it lists every node's
interfaces and addresses, which makes that check straightforward.

**None of this manual cleanup is needed when the instance itself is released,
provided that the platform's "release associated resources" option remains
enabled** (in the OpenAPI, this is `ReleaseInstances`'s `DependResource`,
default `true`). Releasing an instance also deletes the ENIs attached to it and
the addresses on them, including those that this plugin created.

Conversely, **releasing a node with that option turned off** (releasing the
instance only and keeping its associated resources) leaves this plugin's ENIs
and addresses behind. While a crd-mode controlplane runs, its VPC sweep removes
the detached plugin interfaces as described above; what actually remains is the
secondary addresses on the released instance's primary ENI, which nothing
reclaims. Without such a controlplane, clean up the interfaces by the
`zenlayer:cni:created-by` **and** `zenlayer:cni:cluster-id` tags (or
`zenlayer:cni:instance-id` for untagged interfaces), as described above.

## Building from source

Requirements: Go 1.26.8 (per `go.mod`) and, for the image, Docker on an `amd64`
host.

| Target | What it does |
| --- | --- |
| `make help` | Print the resolved build variables and the available targets. |
| `make bin` | `go mod tidy`, then build `zec-cni`, `zec-ipam` and `zec-controlplane` into `output/` (`CGO_ENABLED=0 GOOS=linux`). |
| `make zec-cni` / `make zec-ipam` / `make zec-controlplane` | Build a single binary. |
| `make pkg` | `bin` plus the bootstrap scripts copied into `output/`. |
| `make image` | Build the node image (`zec-cni`, `zec-ipam`) with `images/Dockerfile`. amd64 only. |
| `make image-controlplane` | Build the controlplane's image, `<IMAGE_NAME>-controlplane`, with `images/controlplane.Dockerfile`. amd64 only. |
| `make chart-lint` | `helm lint` the chart with the required values filled in. |
| `make chart-template` | Render the chart with the required values filled in. |
| `make chart` | Lint, then `helm package` into `output/charts/`. |
| `make all` | `pkg` + `image` + `image-controlplane` + `chart`. |
| `make tidy` | `go mod tidy`. |
| `make clean` | `go clean`, remove `output/`, remove both built images. |

Relevant variables: `OUTPUT` (default `output`), `IMAGE_NAME`
(`docker.io/zenlayer1/zeccni`), `IMAGE_TAG`, `CHART_DIR` (`chart`),
`CHART_OUTPUT` (`output/charts`), `CHART_VERSION` / `CHART_APP_VERSION` (read
from `Chart.yaml`), `ENGINE` (`docker`), `HELM` (`helm`), `GOPROXY`, and
`DEBUG` (`1` by default, which disables optimizations and keeps symbols; set
`DEBUG=0` for a stripped release build).

Run the tests with:

```sh
go test ./...
```

Several packages depend on Linux-only netns helpers and do not build on other
operating systems, so run the tests on Linux. CI does so, with
`GOOS=linux CGO_ENABLED=0` for the job and `CGO_ENABLED=1 go test -race` for the
test step. On a non-Linux workstation, `GOOS=linux go vet ./...` type-checks
everything, including test files.

## Troubleshooting

**Logs.**

- `zec-cni` writes to `/var/log/zenlayer/cni.log` on the node (rotated by
  lumberjack: 256 MB, 2 backups, 7 days). Kubelet also surfaces CNI errors in
  the Pod's events.
- `zec-ipam` logs to stderr: `kubectl -n kube-system logs ds/zenlayer-vpc-cni -c zec-ipam`.
- The controlplane: `kubectl -n kube-system logs deploy/zenlayer-vpc-cni-controlplane`
  selects one replica; with several replicas, only the one holding the Lease is
  active. The Lease holder's name starts with the name of the node it runs on:
  `kubectl -n kube-system get lease zenlayer-vpc-cni-controlplane -o jsonpath='{.spec.holderIdentity}'`.
- `zec.logLevel` sets the level for both binaries at install time. It renders
  `ZENLAYER_CNI_LOGLEVEL` onto the DaemonSet and, for `debug` and `trace`, the
  conflist `debug` flag that `zec-cni` reads. `trace` is the more verbose of the
  two, so it does not turn that flag off again.
- The log level and destination can also be overridden with the
  `ZENLAYER_CNI_LOGLEVEL` (`trace|debug|info|warn|error|fatal|panic`) and
  `ZENLAYER_CNI_LOCATION` (a file path, or `stdout`) environment variables.
  `zec-ipam` also accepts `-log-level`, which overrides `ZENLAYER_CNI_LOGLEVEL`
  when given.
- Setting `"debug": true` in the conflist raises `zec-cni` to debug level for
  that invocation.
- `ZENLAYER_CNI_SDK_HTTP_DUMP=true`, set on the component that calls the OpenAPI
  (the controlplane, in the centralized deployment), additionally dumps every
  OpenAPI request and response body. It is off by default and is **not** implied
  by `ZENLAYER_CNI_LOGLEVEL=debug`, because the dump is all-or-nothing: it
  includes the `Authorization` header, so it writes the account's AccessKeyID
  into the log in plaintext on every call. Use it on a disposable cluster,
  and treat the logs it produces as credentials.

**Health and metrics.** On any node running the DaemonSet:

```sh
curl 127.0.0.1:11414/healthz
curl 127.0.0.1:11414/metrics
```

The daemon serves no profiling endpoints. This port is reachable from outside
the node (Prometheus scrapes it there), and `/debug/pprof` on it would expose a
heap dump, a CPU profile and the process command line to anything that can
reach the node. Use a build made for profiling instead.

**Start with `kubectl get zn`.** It shows one row per node. When Pods get no
address, the reason is nearly always visible here:

```sh
kubectl get zn
# NAME   INSTANCE   REQUESTED   ALLOCATABLE   READY   REASON   ALLOCATION   AGE
kubectl describe zn <node>     # every condition, interface and address
```

| Column / condition | Meaning |
| --- | --- |
| `READY` / `REASON` | The node itself only: `True` once its instance is verified and its interface list is known. An allocation failure does **not** change it. |
| `ALLOCATION` | The reason of `IPAllocationFailed`. `Satisfied` means that the demand is met; any other value is the failure. Possible values are the controlplane's own `ENIQuotaReached`, `NoAvailableSubnet`, `IPv6PrefixRefused` and `NoPodInterface` (crd mode only: the primary interface is excluded because its subnet is not a Pod subnet, and no other interface can serve Pods, because the ENI quota is full or is taken by unmanaged ENIs or by interfaces that the platform refused an IPv6 prefix; list the node's subnet in `zec.subnetIDs`), or a reason named like the Node events below (`InsufficientIpInSubnet`, `VpcResourceQuotaExceeded`, `CredentialServiceAbnormal`, `OpenApiUnreachable` and so on). Any other platform refusal appears as `AllocationError`, with the platform's code in the message. |
| `InstanceVerified` condition | The node's binding to its instance. While it is `False`, the daemon does not start serving; the reason states the cause: `IdentityClaimMissing`, `ProviderIDMissing`, `ProviderIDUnparseable`, `IdentityMismatch`, `InstanceNotFound`, `InstanceAmbiguous`, `BoundToOtherInstance` (see [Security](#operating-zecipamtypecrd-safely)). When the platform cannot be queried, the reason is that of the platform event (`CredentialServiceAbnormal`, `OpenApiUnreachable`, `OpenApiFlowLimit`, ...) or `AllocationError` with the platform's code in the message; a condition that is already `True` remains `True` until the platform answers. |
| A node has no `ZecNode` | The controlplane is not running, or has not reached that node yet. |

**The controlplane.** A missing or misnamed Secret (`zec.secretName`) keeps the
controlplane Pod in `ContainerCreating`: `kubectl -n kube-system describe pod -l
app=zenlayer-vpc-cni-controlplane` shows a `FailedMount` event, and kubelet
retries the mount automatically once the Secret exists. Once running, the
controlplane does not exit because of problems with the credentials' content,
the VPC or the subnets. Instead, it stops at that step, its `/healthz`
(`:11416`) answers 503 and states what it is waiting for, and the Deployment
remains unready. A wrong security group is not checked at startup: it appears as
a failed create in a node's `ALLOCATION` column once a node needs a new
interface.

```sh
kubectl -n kube-system get deploy zenlayer-vpc-cni-controlplane
kubectl -n kube-system logs deploy/zenlayer-vpc-cni-controlplane
```

| `/healthz` answer | Cause |
| --- | --- |
| `waiting for OpenAPI credentials` | The Secret is mounted but its keys cannot be read: they are not `AccessKeyID` / `AccessKeyPassword`, or one is empty. For a temporary credential (a `SecurityToken` key is present): `AccessKeySecret`, `SecurityToken` or `Expiration` is missing or empty, or `Expiration` is not an RFC 3339 time. |
| `waiting for the cluster id` | The `kube-system` namespace, whose UID tags the interfaces that this cluster creates, cannot be read: RBAC is missing or the API server is unreachable. |
| `waiting for the pod subnets` | The subnet lookup failed: the OpenAPI rejected the credential or cannot be reached, a subnet does not exist or is not in `zec.vpcID`, or `zec.ipStack=dual` is set and a subnet has no IPv6 CIDR. The log states the exact cause. |
| `waiting for the ZecNode CRD` | The CRD is not installed. Install it with `kubectl apply --server-side -f zenlayer-vpc-cni/crds/zecnodes.yaml` from the chart (`helm pull oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.1 --untar`). |
| `credential invalid` / `openapi unreachable` / `clock skew: request signatures rejected` | A cluster-wide failure while running: the credential was rejected, the OpenAPI cannot be reached, or the clock on the controlplane's node is too far off. Also shown in every `ZecNode`'s `ALLOCATION` column and in Node events. |

Invalid IPv6 billing values (such as `ByTrafficPackage` without
`ipv6PackageSize`) make the controlplane exit at startup, with the error in its
log.

**Common daemon startup failures** (reported by the daemon, which then exits):

| Message | Cause |
| --- | --- |
| `<field> is set in the cni configmap, but with ipamType "crd" it belongs to the controlplane's configuration` (or `... it has no effect and no controlplane counterpart`) | A manually written or per-node `eni_conf` contains a setting that belongs to the controlplane (VPC, subnets, security group, credentials, pool sizing and so on) or that crd mode does not use. Remove it. |
| `error retrieving service cidr: configmaps "kubeadm-config" not found` | The cluster was not built with kubeadm, and `zec.serviceCidr` was left empty. Set it explicitly. |
| `ipamType is not set in the cni configmap` / `ipStack is not set in the cni configmap` | A manually written `eni_conf` omits the switch. Both are required and have no default; set them explicitly. |
| `unsupported ipStack <value> in configMap` | `zec.ipStack` is neither `ipv4` nor `dual`. |
| `ipStack "ipv6" (IPv6-only) is not implemented yet` | IPv6-only is not supported. Use `ipv4` or `dual`. |
| `unsupported daemon mode` | Only `eni_multi_ip` is supported. |

Startup failures specific to direct mode are in
[docs/deploy-direct.md](docs/deploy-direct.md) §6.

**Node events.** Failures that prevent Pods from getting addresses are
recorded as Warning events on the Node, so they can be read without the Pod
log. The controlplane writes the OpenAPI-related events (every reason below
except `DaemonStartFailed` and `InterfaceExcluded`, and the `InstanceVerified`
failure reasons above), once per reason. The daemon writes the other two:
`DaemonStartFailed` (Warning), visible while the daemon is in
`CrashLoopBackOff`, and `InterfaceExcluded`, which is **Normal**.
`kubectl describe node <node>` also lists them. To filter them:

```sh
kubectl get events -n default --field-selector involvedObject.kind=Node,involvedObject.name=<node>,type=Warning
```

| Reason | Cause |
| --- | --- |
| `CredentialServiceAbnormal` | The OpenAPI rejected the credential, or none was configured. Check the Secret. |
| `OpenApiPermissionDenied` | The credential is valid but lacks permission for an API that the controlplane calls; the message names the API. Grant the permission; a new key does not help. |
| `NodeClockSkew` | Requests were rejected as expired: the caller's clock (on the controlplane's node) is behind the platform's. A clock running ahead is not detected this way. Fix time synchronization first; a new credential does not help. |
| `OpenApiUnreachable` | The OpenAPI endpoint could not be reached, or something in front of it (a proxy, a gateway, a block) answered instead. The OpenAPI has no internal endpoint, so the nodes running the controlplane need outbound internet access. |
| `AttachENIFailed` | A newly created ENI could not be attached to the instance. The message contains the attach error and whether the ENI was freed. |
| `DaemonStartFailed` | A daemon startup failure, such as an invalid configuration or a timeout while waiting for the Cilium agent; the message is the error with which the daemon exited. A failure before the daemon reaches the API server produces no event. |
| `VpcResourceQuotaExceeded` / `InsufficientIpInSubnet` / `OpenApiFlowLimit` | Account quota reached / Pod subnet out of addresses / OpenAPI throttling. |
| `InterfaceExcluded` | Direct mode: an interface's subnet (the primary's, or one since removed) is not in `zec.subnetIDs`, so the interface serves no new Pod; see [Capacity planning](#capacity-planning). Recorded regardless of capacity; the node usually keeps its normal capacity from secondary interfaces. This is a **Normal** event, so the `type=Warning` filter above hides it. In crd mode, the same situation appears as the interface's `Excluded` state in `kubectl describe zn`, and a node left with no Pod capacity appears as `NoPodInterface`. |

**Pods stuck in `ContainerCreating`.** Check that `/opt/cni/bin/zec-cni` exists
on the node (the init container installs it), then check that
`/etc/cni/net.d/10-zenlayer-cni.conflist` and `/var/run/zenlayer/cni.socket`
exist, and read `/var/log/zenlayer/cni.log`.

The order matters because the init container does **not** install the conflist.
The daemon writes it once it is serving, so a missing conflist points to the
daemon, not to the init container, which will have exited successfully with
nothing missing. With Cilium chaining enabled, the daemon additionally waits for
`/var/run/cilium/cilium.sock` before publishing the conflist, so in that case an
absent conflist means that the Cilium agent is not up. The daemon also waits for
the controlplane to verify the node (`InstanceVerified=True`) before it serves,
so check `kubectl get zn` when the conflist is missing. See
[Architecture](#architecture).

## Further documentation

- [Direct mode](./docs/deploy-direct.md): deploying with credentials on every
  node, each node calling the OpenAPI itself. Covers when to choose it, the
  install steps, and how its warm pool, GC and troubleshooting differ from the
  centralized deployment.
- [Deploying with Cilium chaining](./docs/deploy-cilium-chained.md): the
  recommended mode, `cni.mode=chained`. Covers prerequisites, the steps to switch
  over from veth, every Cilium value that must be set and why, and how to roll
  back. Read it before enabling network policy.
- [Delegated IPAM mode](./docs/deploy-cilium-netkit.md): the full walkthrough
  for `cni.mode=delegated`, where Cilium creates the Pod device (netkit or veth)
  and this plugin only supplies addresses and host per-ENI routing.
- [Performance: measurements and how to read them](./docs/performance.md):
  measured numbers for each configuration shape, which switches provide an
  actual benefit, and why cross-session comparisons are not valid. Read it
  before choosing a shape.
- [CHANGELOG.md](./CHANGELOG.md): release notes, including the known limitations
  of the current release.
- [CONTRIBUTING.md](./CONTRIBUTING.md): build, test and lint commands, and the
  checks applied to a pull request.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the build, test and review process.
Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md).

## Security

Report vulnerabilities privately; see [SECURITY.md](SECURITY.md).

### Operating `zec.ipamType=crd` safely

With `crd`, nodes hold no OpenAPI credentials, and each node may change only
its own `ZecNode`. The cluster operator remains responsible for the following
two points:

- **Delete bootstrap tokens once nodes have joined.** A valid bootstrap token
  can obtain a kubelet certificate for *any* node name (Kubernetes approves
  those requests without checking the name), so whoever holds one can register
  a node or impersonate an existing one. Create a token immediately before
  joining nodes (`kubeadm token create --print-join-command`, valid 24 hours),
  delete it as soon as the nodes have joined (`kubeadm token delete <id>`),
  check for leftover tokens with `kubeadm token list`, and never create one with
  `--ttl 0`.
- **A node name remains bound to the instance it was verified as.** If a Node is
  deleted and registers again claiming a different instance while the first
  instance still exists, the controlplane refuses it (`InstanceVerified=False`,
  reason `BoundToOtherInstance`). Reusing a hostname after the old machine is
  deleted requires no action. Moving a name to a new machine while the old one
  is still running requires clearing the record first:
  `kubectl delete zecnode <node-name>`.

## Changelog

Release notes for each version are in [CHANGELOG.md](./CHANGELOG.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
