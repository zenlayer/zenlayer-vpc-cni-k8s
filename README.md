# Zenlayer VPC CNI for Kubernetes

English | [简体中文](README-CN.md)

A CNI plugin that gives Kubernetes Pods native Zenlayer VPC addresses. Pod IPs
are secondary IPs on Zenlayer elastic network interfaces (ENIs) attached to the
node, so Pod traffic is routed by the VPC itself — no overlay, no encapsulation,
and Pod addresses are reachable from anywhere in the VPC.

Status: the plugin supports the `eni_multi_ip` network mode on `linux/amd64`
nodes. A node serves either IPv4 only or dual-stack; see
[Address families](#address-families).

## Feature support

| Capability | Status |
| --- | --- |
| IPAM mode | `eni_multi_ip` -- a Pod IP is a secondary IP on a VPC elastic network interface |
| Datapath | veth pair + per-ENI policy routing. Under `delegated` the Pod device is Cilium's |
| Address families | IPv4 or dual-stack. IPv6-only is not implemented |
| Node platform | `linux/amd64` only |
| Kubernetes | >= 1.28.2 (the chart's `kubeVersion` constraint) |
| Service handling | kube-proxy, or a replacement you install yourself. This plugin provides none |
| NetworkPolicy | Not enforced by default. Available by chaining upstream Cilium -- see [Network policy](#network-policy) |
| Cilium (chaining) | Recommended v1.15 -- v1.18; checked against the v1.17.5 source and verified on a v1.17.5 cluster |
| SNAT / iptables rules | None. Pods needing public IPv4 egress require a NAT gateway on the VPC side |
| Pod bandwidth limiting | Not implemented. Egress shaping is available through Cilium's bandwidth manager |
| `hostPort` | Supported, by whichever component owns the datapath. Without Cilium the conflist chains the upstream `portmap` plugin (the binary ships in the image); with `cilium.enabled=true` Cilium's own BPF load balancer serves hostPort and `portmap` is not chained at all. In that case **keep the port below 30000** -- Cilium refuses any hostPort inside the NodePort range, logging a warning and nothing else, see [docs/deploy-cilium-netkit.md](docs/deploy-cilium-netkit.md) §4.5. Pod IPs are routable within the VPC anyway, so reaching a Pod by its own IP or through a Service is usually the better fit -- a hostPort pins the Pod to one node and collides with any other Pod wanting the same port there |
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

Three steps on an existing cluster of Zenlayer VPC instances. See
[Installation](#installation) for what each one does and
[Requirements](#requirements) for the prerequisites.

```sh
# 1. Credentials. Not templated by the chart, so they never reach values.yaml,
#    git, or the Helm release.
kubectl create secret -n kube-system generic zenlayer-cni-access-key \
  --from-literal=AccessKeyID='<access-key-id>' \
  --from-literal=AccessKeyPassword='<access-key-secret>'

# 2. Install. The three --set values are required; rendering fails without them.
helm install zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 1.0.0 \
  --namespace kube-system \
  --set-string zec.vpcID=<vpc-id> \
  --set-string 'zec.subnetIDs={<subnet-id-1>,<subnet-id-2>}' \
  --set-string zec.securityGroupID=<security-group-id>

# 3. Wait for the DaemonSet, then check a pod actually gets a VPC address.
kubectl -n kube-system rollout status ds/zenlayer-vpc-cni
kubectl run nettest --image=busybox --restart=Never -- sleep 3600
kubectl wait --for=condition=Ready pod/nettest --timeout=60s
kubectl get pod nettest -o jsonpath='{.status.podIP}{"\n"}'
kubectl delete pod nettest
```

The Pod IP should be an address from one of the subnets you passed in.

Defaults worth knowing: a warm pool of 2 IPs per node and credentials read from
`/etc/zec_secret/`. Which component builds the Pod device follows `cni.mode` — see
[Datapath](#datapath).

## Architecture

Two binaries are built from this repository:

| Binary | Source | Role |
| --- | --- | --- |
| `zec-cni` | `cmd/zenlayer-vpc-cni` | The CNI plugin kubelet execs on Pod sandbox setup/teardown. It performs no IPAM of its own: it dials the node daemon over gRPC and then wires up the Pod netns. |
| `zec-ipam` | `cmd/cni-ipam` | The per-node daemon (a DaemonSet, `hostNetwork: true`). It talks to the Zenlayer OpenAPI, attaches ENIs, allocates and frees secondary IPs, maintains the warm pool, and serves the gRPC API. |

The two communicate over a Unix domain socket at `/var/run/zenlayer/cni.socket`
(gRPC). Everything the plugin needs — the allocated IP, the ENI it belongs to,
the gateway, the service CIDR — comes back in the `AllocIP` response.

The daemon also:

- reads its configuration from the `zenlayer-cni-config` ConfigMap in
  `kube-system` (key `eni_conf`), and the OpenAPI credentials from files under
  `/etc/zec_secret/`;
- persists resource state in BoltDB under `/var/lib/cni`;
- registers a kubelet device plugin that advertises the node's assignable
  address count as the extended resource `zenlayer.com/eni-ip`
  (`disableDevicePlugin` in `eni_conf` turns it off, which the chart does not
  template -- see [Configuration reference](#configuration-reference)). **That
  figure only constrains scheduling for workloads that request
  `zenlayer.com/eni-ip` themselves**; see [Capacity planning](#capacity-planning);
- serves `/healthz` and `/metrics` (Prometheus) on
  `healthAndDebugPort` (default `11414`) on the node's own network namespace.

An init container (`script/bootstrap/install_env.sh`) installs `zec-cni` and the
upstream `loopback` plugin onto the host. The conflist is not installed by the
init container: the daemon writes it once it is serving, so kubelet does not
call the plugin before the daemon can answer.

Every `reconcileIntervalSec` (and on demand, when an allocation hits a quota
error) the daemon runs a **GC** pass: it reconciles each pool against the
resources Pods actually hold and releases what has leaked. One thing goes by
three names, so: the knob is `reconcileIntervalSec`, the logs say `Daemon GC`,
and the rest of this document says GC. The
first pass after startup is skipped, and repeat runs are debounced to one a
minute.

A record whose Pod is gone is not reclaimed on sight. The first pass that
notices marks it, and it is dropped -- together with the routing rules naming
its address -- once an hour has passed, so the address returns to the pool
**one hour to one hour twenty** after the daemon first noticed. This timer is
held in memory and restarts with the daemon, so a daemon restarting more often
than that does not reach the reclaim step; the effect is on capacity, not on
connectivity.

GC **pauses the pool for its whole duration**, including the OpenAPI calls it
makes, so Pod setup on that node waits behind it. CNI ADD gives up after 118
seconds. The daemon logs the duration of every pass and warns when one runs
long — watch for `Daemon GC finished in` if Pods intermittently fail to start.

IP allocation follows a warm pool: the daemon keeps `poolTarget` free IPs ready,
so Pod creation usually does not have to wait for an OpenAPI round trip.
`poolTargetMin` and `poolTargetMax` bound the pool as a whole -- addresses
already held by Pods included -- rather than the idle part of it. With
`poolTargetCapProbe` enabled, the default, the daemon probes the instance's real
ENI/IP capacity and uses that as the upper bound instead, leaving
`poolTargetMax` with no effect; see [Configuration
reference](#configuration-reference).

Service traffic is *not* handled here. There is no service load balancer and no
NetworkPolicy implementation in this plugin — kube-proxy (or a replacement you
install yourself) handles services.

## Datapath

Which component builds the Pod device follows `cni.mode`:

- **`chained` (the default)**: this plugin creates a veth pair
  (`pkg/plugin/datapath/policy_route.go`). The Pod reaches the host stack
  directly, so kube-proxy sees both directions of service traffic without help,
  and it is the device model Cilium's `generic-veth` CNI chaining expects.
- **`delegated`**: Cilium is the primary CNI and creates the Pod device -- a
  netkit device, or a veth, according to its own `bpf.datapathMode`. This plugin
  builds no device at all there. See
  [docs/deploy-cilium-netkit.md](docs/deploy-cilium-netkit.md).

**The per-ENI policy routing is identical in both modes and is always installed
by this plugin.** Each ENI gets its own route table (`1000 + ifindex`) holding
that ENI's default route, and a `from <pod-ip> lookup <eni-table>` rule steers
the Pod's egress into it.

`zec.dataPath` renders into the conflist as `eniip_virtual_type` and accepts
only `veth`. It describes a device this plugin builds itself, so it is not
rendered under `cni.mode=delegated`.

**This plugin performs no address translation (SNAT) and installs no iptables
rules of its own.** Pod addresses are secondary IPs on an ENI and are directly
routable inside the VPC, so east-west traffic is sent and received with the
Pod's own address and passes through no translation. **Reaching the public IPv4
internet from a Pod therefore depends on a NAT gateway on the VPC side** -- one
must be configured for the subnet the Pod's address comes from, otherwise Pods
have no public IPv4 egress. Public IPv6 follows a different path: the platform
assigns a public address to the interface directly, see
[Address families](#address-families).

**Pod bandwidth limiting is not implemented by this plugin.** The conflist does
not declare the standard CNI `bandwidth` runtime capability, and declaring it
would be a lie: nothing here consumes `runtimeConfig.bandwidth`, so kubelet
would translate the bandwidth annotations, hand them to a plugin that ignores
them, and the Pod would run unthrottled while the conflist claimed otherwise.

Egress rate limiting is available through Cilium instead, if it is enabled (see
[Network policy](#network-policy)): its bandwidth manager reads the
`kubernetes.io/egress-bandwidth` annotation off the Pod and paces with EDT. Turn
it on with `bandwidthManager.enabled=true` in Cilium's own values. Three things
to know before relying on it:

- **Egress only.** Cilium's documentation states that
  `kubernetes.io/ingress-bandwidth` is not supported, is not recommended, and is
  ignored.
- **Pods on a secondary ENI may not be limited.** Cilium picks the devices to
  shape on by looking for the default route or the node's `InternalIP` /
  `ExternalIP`, which describes the primary ENI. A Pod whose IP lives on a
  secondary ENI egresses through that ENI's own route table, which has neither.
  Check what is actually in effect with
  `cilium-dbg status | grep BandwidthManager`, which prints the device list.
- **It is driven by Pod annotations, not by the CNI chain, and needs no CRD.**
  The settings are annotations only (`kubernetes.io/egress-bandwidth`, and
  `bandwidth.cilium.io/priority` for priority banding). Cilium's Pod watcher
  acts on annotation changes, so editing the annotation on a running Pod takes
  effect without recreating it.

This chart provides no bandwidth switch of its own.
`bandwidthManager.enabled` lives in Cilium's release, which this chart can
neither read nor set, so a switch here could only claim the feature is on
without being able to check that it is.

See [docs/deploy-cilium-chained.md](./docs/deploy-cilium-chained.md) for the verification
steps and the remaining upstream limitations.

## Address families

`zec.ipStack` selects the address families a node serves:

| Value | Meaning |
| --- | --- |
| `ipv4` | Default, and what an empty value means. Pods receive one IPv4 address. |
| `dual` | Pods receive one IPv4 and one IPv6 address. |
| `ipv6` | Not implemented. |

Four properties of `dual` are worth knowing before enabling it.

**Every configured subnet must carry both an IPv4 and an IPv6 CIDR.** The
daemon refuses to start otherwise, naming the subnet and the family it is
missing. The check runs at startup rather than at allocation time on purpose: a
per-allocation warning is indistinguishable from a slow platform, and the
symptom would be a warm pool that never fills with every Pod stuck in
`ContainerCreating` and a single warning line as the only clue.

**An address pair is indivisible.** A Pod receives its IPv4 and its IPv6
address as one unit; a resource that covers only one family is never handed to
a Pod, but marked and reclaimed. This is what keeps a Pod from starting with
half an address set after a partial platform failure.

**IPv6 never constrains node capacity.** The platform routes one `/96` prefix
per interface and the daemon carves addresses out of it locally -- there is no
per-address IPv6 API. A `/96` holds far more addresses than any interface can
serve, so `zenlayer.com/eni-ip` and the warm pool are sized on IPv4 alone and a
node reports the same capacity in both modes. See
[Capacity planning](#capacity-planning).

**Public IPv6 is billed, and the daemon must state how.** The platform refuses
to enable IPv6 on an interface in a subnet whose `ipv6Type` is `Public` unless a
charge type and a bandwidth cap are supplied, and it is this plugin that enables
IPv6 on every interface it creates. `zec.ipv6InternetChargeType` and
`zec.ipv6Bandwidth` therefore decide what the cluster is billed for. They are
ignored on a subnet with private IPv6. `ByTrafficPackage` additionally requires
`zec.ipv6PackageSize` and `BandwidthCluster` additionally requires
`zec.ipv6ClusterId`; both are checked at render time by the chart and again at
startup by the daemon. See [Configuration reference](#configuration-reference).

IPv6 allocation state is maintained by the daemon itself -- in its local record
store and from the Pod addresses reported by the API server -- because addresses
are carved from the interface's prefix rather than requested individually. IPv4
state is additionally reconciled against the platform on every GC
collection pass.

Dual-stack on this plugin does **not** require the Kubernetes cluster itself to
be dual-stack. Layering Cilium chaining on top does -- see
[Network policy](#network-policy) and
[docs/deploy-cilium-chained.md](docs/deploy-cilium-chained.md) §4.3.

## Network policy

NetworkPolicy is **not enforced by default**. This project ships no policy
engine, so policy objects are accepted by the apiserver and then ignored -- a
rule that should block traffic silently does not.

`cilium.enabled=true` chains **upstream Cilium** after this plugin, using
Cilium's `generic-veth` chaining mode, and `networkPolicy.enabled=true` declares
that policy must be enforced -- with provider `cilium` the second requires the
first, and the chart refuses to render otherwise rather than accepting policy
objects nothing will act on. The two are separate because policy is not the only
reason to want Cilium: Hubble observability needs the agent and no policy. Under
`cni.mode=chained` the veth pair and the per-ENI policy routing are unchanged and
Cilium adds endpoint registration and tc eBPF policy enforcement on the
host-side veth; under `cni.mode=delegated` Cilium creates the device itself and
enforces policy on it, while the per-ENI routing remains this plugin's. It does not
take over IPAM or routing -- the chainer hardcodes `ExternalIpam: true` and
`RequireRouting: false`.

Cilium is not forked, patched, or vendored here, and the chart does not install
it: deploy it from its own upstream chart. Supported range **v1.15 - v1.18**,
code-checked against v1.17.5.

Four things are worth knowing before enabling it:

- **`cni.customConf=true` is mandatory; set `cni.exclusive=false` alongside it.**
  The first is a hard prerequisite of `generic-veth`: Cilium **has no conflist
  template for that mode**, so `customConf=false` contradicts it and **Cilium
  rejects the combination itself**: the agent sits at `0/1 Running`
  and never becomes Ready (logging `invalid CNI chaining mode: generic-veth`)
  while **Pod networking and NetworkPolicy keep working**. Getting it wrong does
  not degrade silently, lose policy, or drop Pods; it lights one permanent red
  lamp. The second is redundant in this mode (with `customConf=true` the key
  never reaches the configmap at all), but becomes live again under a chaining
  mode Cilium can render, and costs nothing to set.
- **After changing anything under `cni.*` or `extraConfig.*`, restart the cilium
  agent explicitly.** Those keys only land in the ConfigMap and do not alter the
  Pod template, so `helm upgrade` does not roll the DaemonSet -- and
  `kubectl rollout status ds/cilium` **returns success immediately, a false
  green**. Use `kubectl -n kube-system rollout restart daemonset/cilium`.
- **L7 policy (HTTP/DNS/FQDN), transparent encryption (IPsec/WireGuard), and
  Cilium Ingress/Gateway API are not available** in this mode, so enabling
  Cilium for L7 policy will not work. What matters more is that **L7 and FQDN
  policies fail open**: the API server accepts the object, the Cilium agent on
  the node never loads it, and the traffic it should deny is all allowed; a
  policy using only `toFQDNs` is even reported as `Valid=True`. Express
  isolation with native NetworkPolicy or a purely L3/L4 CiliumNetworkPolicy --
  see the `l7Proxy` row in [docs/deploy-cilium-chained.md](docs/deploy-cilium-chained.md) §4.2.

- **A typo in the switch name is rejected rather than silently read as "off".**
  `values.schema.json` sets `additionalProperties: false` at the root and in
  every section, so `--set celium.enabled=true` (one letter wrong) fails at
  render time and names the offending key:
  `additional properties 'celium' not allowed`.

- **`zec.ipStack=dual` with chaining requires a dual-stack Kubernetes cluster.**
  Cilium must then be deployed with `ipv6.enabled=true`, which in turn requires
  every Node object to carry an IPv6 address -- a cluster-level prerequisite no
  Cilium value can supply. Neither half can be skipped: without
  `ipv6.enabled=true` the agent's per-veth BPF drops every IPv6 packet as an
  unsupported L3 protocol, and it does so silently -- the agent reports healthy,
  every endpoint lists the correct IPv6 address, and nothing is logged above
  info; with it on a single-stack cluster the agent refuses to start. This
  plugin's own dual-stack support does not depend on the cluster being
  dual-stack; the requirement is introduced only by layering chaining on top.
  See [docs/deploy-cilium-chained.md](docs/deploy-cilium-chained.md) §4.3.

Full instructions, the required values with the reason for each, verification
steps, and **the switching procedure in both directions** (plain veth <-> chained)
are in [docs/deploy-cilium-chained.md](./docs/deploy-cilium-chained.md).

Read that switching section before uninstalling Cilium: **the order is fixed and
the rolling restart of workloads is not optional.** The tc/tcx BPF programs on a
Pod's veth are BPF links that outlive the agent and are not removed by
`helm uninstall`. While the agent runs it keeps their load-balancing maps
current, but once it is gone **those maps freeze**, and Pods created during
chaining keep resolving ClusterIPs through a table that is never updated again.
Any subsequent change to a Service's backends leaves those Pods sending traffic
to endpoints that no longer exist, and the connections are dropped with the
`KUBE-SVC-*` counters at zero, so looking at kube-proxy finds nothing.

## Requirements

- Kubernetes >= 1.28.2 (the chart's `kubeVersion` constraint).
- Linux nodes on `amd64`. The DaemonSet has a hard node affinity on
  `kubernetes.io/os=linux` and `kubernetes.io/arch=amd64`, and the image build
  refuses any other `GOARCH`.
- Nodes running as Zenlayer VPC instances, with a VPC, at most 5 subnets in
  that VPC for Pod IPs, and a security group for the ENIs the daemon creates.
- Zenlayer OpenAPI static credentials (access key ID + password) with
  permission to manage ENIs and their secondary IPs.
- **Public** outbound internet access from **every** node -- an EIP bound to the
  node, or a NAT gateway on its VPC. The Zenlayer OpenAPI is a public endpoint
  and the daemon calls it from the node it runs on, so a node that cannot reach
  the public internet cannot allocate a single Pod address. This is not the same
  as the Pod egress below, which only limits what Pods can reach.
- Helm 3.8 or newer to install. 3.8 is where `oci://` repository support became
  generally available, and every install command here pulls the chart from the OCI
  repository on Docker Hub. Installing from a local chart directory needs only 3.1,
  the floor set by the `lookup` function the chart's NOTES use.
- Linux kernel >= 6.7 on every node **when** `cni.mode=delegated` runs Cilium with
  `bpf.datapathMode=netkit` or `netkit-l2`: netkit devices do not exist below it.
  The default `chained` mode adds no kernel requirement of its own.
- kube-proxy, or another component providing Service handling. Under
  `cni.mode=delegated` that component is Cilium, whose `kubeProxyReplacement`
  is mandatory there.
- If Pods need public IPv4 egress: a NAT gateway configured for the subnet the
  Pod addresses come from. This plugin performs no SNAT; see [Datapath](#datapath).

## Installation

### 1. Create the credentials Secret

The credentials are **not** templated by the chart, so they never land in
`values.yaml`, in git, or in the Helm release. Create the Secret separately, in
`kube-system`:

```sh
kubectl create secret -n kube-system generic zenlayer-cni-access-key \
  --from-literal=AccessKeyID='<access-key-id>' \
  --from-literal=AccessKeyPassword='<access-key-secret>'
```

The daemon mounts it read-only at `/etc/zec_secret/` and reads the two keys as
files (`AccessKeyID`, `AccessKeyPassword`). If you name the Secret something
else, pass `--set zec.secretName=<name>`.

### 2. Install the chart

```sh
helm install zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 1.0.0 \
  --namespace kube-system \
  --set-string zec.vpcID=<vpc-id> \
  --set-string 'zec.subnetIDs={<subnet-id-1>,<subnet-id-2>}' \
  --set-string zec.securityGroupID=<security-group-id>
```

> **Use `--set-string` for the IDs.** `--set` types the value, and an all-digit
> ID becomes an int64 that the schema rejects with
> `got number, want string`. Zenlayer IDs are all digits, so this bites on the
> very first install.

`zec.vpcID`, `zec.subnetIDs` and `zec.securityGroupID` are required — rendering
fails without them.

A dual-stack install needs the IPv6 billing values as well, since the daemon
enables public IPv6 on every interface it creates. `zec.ipv6InternetChargeType`
defaults to `ByTrafficPackage`, which requires a package size, so a dual-stack
install carries either

```sh
  --set-string zec.ipStack=dual \
  --set-string zec.ipv6InternetChargeType=ByBandwidth --set zec.ipv6Bandwidth=1
```

or, for a traffic package, `--set-json zec.ipv6PackageSize=0.1` alongside the
default charge type. See [Address families](#address-families) for what each one
bills.

To preview the rendered manifests first:

```sh
helm template zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 1.0.0 \
  --set-string zec.vpcID=<vpc-id> \
  --set-string 'zec.subnetIDs={<subnet-id>}' \
  --set-string zec.securityGroupID=<security-group-id>
```

### 3. Verify

```sh
# 1. every pod ready, no restarts
kubectl -n kube-system rollout status ds/zenlayer-vpc-cni
kubectl -n kube-system get pod -l app=zenlayer-vpc-cni -o wide

# 2. a new pod gets an address from the VPC subnet (not the kubeadm podCIDR)
kubectl run nettest --image=busybox --restart=Never -- sleep 3600
kubectl get pod nettest -o jsonpath='{.status.podIP}{"\n"}'
kubectl delete pod nettest

# 3. on a node: the conflist landed with mode 0600, the binary carries version info
ssh <node> 'ls -l /etc/cni/net.d/10-zenlayer-cni.conflist; /opt/cni/bin/zec-cni version'

# 4. on a node: pool state
ssh <node> 'curl -s 127.0.0.1:11414/metrics | grep -E "^resource_pool_"'
ssh <node> 'curl -s 127.0.0.1:11414/healthz'
```

Three things to note:

- **`/healthz` and `/metrics` are served on every node address, not just
  loopback.** The daemon runs with hostNetwork and binds `:11414`, which it has
  to: kubelet probes the endpoint and Prometheus scrapes it from off the node.
  The examples above go in over ssh out of habit, not because the port is
  restricted: the image has no `curl`, but it does have `wget`, so
  `kubectl exec` reaches the same endpoints without leaving the cluster:

  ```sh
  POD=$(kubectl -n kube-system get pod -l app=zenlayer-vpc-cni \
          --field-selector spec.nodeName=<node> -o jsonpath='{.items[0].metadata.name}')
  kubectl -n kube-system exec $POD -c zec-ipam -- \
    wget -qO- http://127.0.0.1:11414/metrics | grep -E '^resource_pool_'
  ```

  That listener carries `/healthz` and `/metrics` and nothing else; there is
  deliberately no pprof on it.
- **`zec-cni version` should print a non-empty version / commit / build time.**
  `CNI Version: unknown` or `Git Commit ID: DEFAULT` means git could not answer
  during the build, so the image carries no provenance. Empty fields mean the
  link-time flags were not passed at all, which is what a plain `go build`
  produces.
- The daemon's `/healthz` only starts answering after it has reached the OpenAPI
  and set up the warm pool, which is why the DaemonSet allows a generous startup
  window (30 x 10s). It reports the daemon, not the node: the conflist is
  written after that point, so with Cilium chaining `/healthz` answers `ok`
  while the daemon is still waiting for the Cilium agent and the node cannot
  start Pods. Both probes are wired to `/healthz`, so a node in that state
  stays Ready -- watch the restart count and the `Still waiting for the cilium
  agent` log line too. See [docs/deploy-cilium-chained.md](docs/deploy-cilium-chained.md) §4.1.

## Capacity planning

The number of Pod addresses a node can hand out follows from its instance size:

```text
capacity = (ENI quota - ENIs attached outside this plugin) x (addresses per ENI - 1)
```

| Size | ENI quota | Addresses per ENI | Assignable |
|---|---|---|---|
| 8 vCPU | 2 | 15 | **28** |
| 16 vCPU | 4 | 30 | **116** |

Every ENI reserves one address for itself -- the instance's own ENI and the ones
this plugin creates alike -- so each contributes one fewer than its per-ENI
limit.

Secondary ENIs attached to the node by other means are **adopted** when
they sit in one of the configured Pod subnets: their addresses join the pool and
count towards the figures above.

⚠️ **An adopted ENI is detached _and deleted_ when the pool shrinks**, and
under `zec.ipStack=dual` the daemon will enable IPv6 on it, which is a billable
operation on a subnet whose `ipv6Type` is `Public`. Keep an ENI attached for
another purpose -- storage, management, monitoring -- out of this by tagging
it `zenlayer:cni:no-manage=true`, or set `zec.manageUntaggedENI=false` to opt the
whole node out.

An ENI that is **not** adopted -- excluded by either of those, or sitting
outside the configured Pod subnets -- is left completely alone: its addresses
are never used, and nothing detaches it. It does still occupy one of the
instance's ENI slots, so the node's capacity is lower than the table predicts by
one ENI's worth of addresses. That is the `interface slot(s)` the startup log
below counts.

The daemon logs the values it resolved at startup, which is the quickest way to
check a node whose capacity is not what the table predicts:

```sh
kubectl -n kube-system logs <daemon-pod> -c zec-ipam | grep -E 'Attached eni|InstanceLimit Updated'
# Attached eni created by cni: [], adopted: [], primary: [<primary-mac>]
# Attached eni not managed by cni: [<mac-1> <mac-2>]; 2 interface slot(s) held without serving pod addresses (ENIUnmanaged)
# InstanceLimit Updated {ENIQuota: 4, IPv4MaxPerENI: 30, IPv6CidrMaxPerENI: 1, ENIManaged: 0, ENIUnmanaged: 2, Cordon: false, allocatable: 58}
```

`ENIUnmanaged` is the count of ENIs this plugin does not manage, and
`allocatable` is the capacity they leave. The node advertises slightly less: the
device plugin subtracts the Pods that do not request `zenlayer.com/eni-ip`.

**These figures are IPv4 addresses, and they do not change under
`zec.ipStack=dual`.** The IPv6 half of a Pod's address pair is carved locally
from the ENI's `/96` prefix, which holds far more addresses than any ENI can
serve, so IPv6 never becomes the binding constraint. A node
reports the same `zenlayer.com/eni-ip` capacity in both modes.

> **The real ceiling is usually kubelet's `maxPods` (default 110), not this
> number.** On a 16 vCPU node the plugin offers 116 addresses while kubelet will
> only schedule 110 Pods, minus the hostNetwork system Pods. Plan for the lower
> of the two; using the full figure requires raising `maxPods` as well.

The device plugin advertises the assignable count as the extended resource
`zenlayer.com/eni-ip`. **That figure lags** (refreshed roughly every 5 minutes,
and computed as capacity minus the Pods that do not request the resource), so
read the node's `resource_pool_*` metrics when you need the live value. On a
busy node the advertised figure can sit well below the node's actual capacity
and returns to it once the node is idle. It is a floor, never an overstatement,
but it should not be read as live capacity.

**The figure only constrains Pods that request the resource.** The scheduler
filters on the resources a Pod actually names in `resources.requests`, and this
plugin does not inject that request. A Pod with an empty `resources` is
therefore placed on a node whose `zenlayer.com/eni-ip` is already `0`, and then
sits in `ContainerCreating` with no address until CNI ADD times out and kubelet
retries.

To have the scheduler place Pods by address capacity, either:

- declare `resources.requests: {zenlayer.com/eni-ip: "1"}` (with a matching
  `limits`) on the workloads themselves; or
- set kubelet's `--max-pods` to the instance's assignable address count using
  the formula in [Capacity planning](#capacity-planning), bounding Pods per node
  instead.

One node-side fallback exists: for a Pod annotated
`k8s.zenlayer.com/pod-evict-policy: allow`, the daemon compares the node's
recorded Pod count against its capacity before allocating, and evicts the Pod --
so that it reschedules elsewhere -- once the count has reached that capacity,
rather than leaving it stuck. Pods without the annotation are unaffected.

## Configuration reference

All values below live under `chart/values.yaml`. Where a `zec.` value ends up
depends on which component reads it, and the split is not obvious from the
prefix:

| Rendered into | Values |
| --- | --- |
| The `eni_conf` key of the `zenlayer-cni-config` ConfigMap | most of `zec.` -- everything the daemon reads from its config file |
| The CNI conflist | `zec.dataPath` |
| Both | `zec.logLevel` |
| The DaemonSet itself, not any ConfigMap | `zec.k8sServiceHost`, `zec.k8sServicePort`, `zec.logLevel` (as `ZENLAYER_CNI_LOGLEVEL`), `zec.healthAndDebugPort` (as the probe ports) and `zec.secretName` (as the mounted Secret) |

The last row matters when overriding configuration per node: that mechanism
replaces `eni_conf` only, so a value from this row cannot be changed that way --
see below.

| Value | Default | Description |
| --- | --- | --- |
| `image.repository` | `docker.io/zenlayer1/zeccni` | Image for both the init container and `zec-ipam`. |
| `image.tag` | `v1.0.0` | Image tag. |
| `image.pullPolicy` | `Always` | Image pull policy. |
| `zec.vpcID` | `""` | **Required.** Zenlayer VPC ID. All subnets must belong to it. |
| `zec.subnetIDs` | `[]` | **Required.** Subnets Pod IPs are taken from. At most 5, all in the same VPC. |
| `zec.securityGroupID` | `""` | **Required.** Security group applied to the ENIs the daemon creates. |
| `zec.ipStack` | `ipv4` | Address families given to Pods: `ipv4` or `dual`. `dual` requires every subnet to carry both an IPv4 and an IPv6 CIDR. IPv6-only is not implemented. See [Address families](#address-families). |
| `zec.ipv6InternetChargeType` | `ByTrafficPackage` | How the platform bills public IPv6 on an interface. `ByTrafficPackage`, `ByBandwidth` or `BandwidthCluster`. Used only when `ipStack` is `dual`; ignored on a subnet with private IPv6. |
| `zec.ipv6Bandwidth` | `10000` | Public IPv6 cap in Mbps per interface, used only when `ipStack` is `dual`. Must be >= 1. |
| `zec.ipv6PackageSize` | `0` | Traffic package size in TB. Required (positive, a multiple of 0.1) when `ipv6InternetChargeType` is `ByTrafficPackage`. Use `--set-json`: `--set` keeps fractions as strings. |
| `zec.ipv6ClusterId` | `""` | Shared bandwidth package the interface joins. Required when `ipv6InternetChargeType` is `BandwidthCluster`. |
| `zec.networkMode` | `eni_multi_ip` | Network mode. Only `eni_multi_ip` is supported. |
| `zec.dataPath` | `veth` | Pod datapath, rendered as `eniip_virtual_type`. `veth` is the only accepted value, and the key is not rendered under `cni.mode=delegated`, where this plugin builds no device — see [Datapath](#datapath). |
| `zec.serviceCidr` | `""` | Service CIDR(s), comma-separated. **Leave it empty only on a kubeadm cluster**: an empty value makes the daemon read the service subnet from `kube-system/kubeadm-config`, and a failed lookup keeps the daemon from starting. Nothing in the datapath acts on the value -- service traffic already traverses the host stack -- but the daemon still requires an IPv4 service CIDR from one of the two sources. On a dual-stack cluster a missing IPv6 entry is only logged. |
| `zec.secretName` | `zenlayer-cni-access-key` | Name of the pre-created Secret holding the OpenAPI credentials. |
| `zec.poolTarget` | `2` | Target number of warm (pre-allocated, unused) IPs per node. |
| `zec.poolTargetMin` | `1` | Lower bound on the pool as a whole, addresses held by Pods included -- not on the idle part alone. At the defaults `poolTarget` is the tighter of the two and this one rarely binds; they only diverge once many addresses are in use. Must be <= `poolTarget`. |
| `zec.poolTargetMax` | `10` | Upper bound on the pool as a whole, addresses held by Pods included -- not on the idle part alone. **Ignored while `poolTargetCapProbe` is `true` (the default)**, which takes the bound from the instance's real capacity instead. With probing off it becomes the capacity this node advertises to the scheduler, so raise it to match the instance: the default is only what the smallest one holds (two interfaces x six addresses, less one primary each). Must be >= `poolTarget`. |
| `zec.poolTargetCapProbe` | `true` | Probe the instance's ENI/IP capacity; relaxes the lower bounds on the `poolTarget*` knobs. |
| `zec.manageUntaggedENI` | `true` | Adopt secondary interfaces attached to the node that this plugin did not create. Set to `false`, or tag an interface `zenlayer:cni:no-manage=true`, to leave it alone. See [Capacity planning](#capacity-planning). |
| `zec.reconcileIntervalSec` | `1200` | GC interval, seconds. See the note below — GC pauses the pool while it runs. |
| `zec.poolMonitorIntervalSec` | `120` | Pool monitor interval, seconds. |
| `zec.healthAndDebugPort` | `11414` | Port for `/healthz` and `/metrics`. The Pod is `hostNetwork`, so this port is taken on the node. |
| `zec.logLevel` | `""` | Log level for both binaries (`trace｜debug｜info｜warn｜error｜fatal｜panic`); empty keeps the built-in default (`info`). It sets `ZENLAYER_CNI_LOGLEVEL` on the DaemonSet, and `debug` or `trace` additionally sets `"debug": true` in the conflist, which is the only channel `zec-cni` has -- kubelet execs it from the host, so it never sees the Pod's environment. |
| `zec.k8sServiceHost` | `""` | Sets `KUBERNETES_SERVICE_HOST`. Use when the hostNetwork daemon cannot resolve the in-cluster apiserver service (e.g. before kube-proxy is up). |
| `zec.k8sServicePort` | `""` | Sets `KUBERNETES_SERVICE_PORT`. |
| `cni.mode` | `chained` | Which plugin creates the pod's network device. `chained`: this plugin creates a veth pair and Cilium, if enabled, is chained after it. `delegated`: Cilium creates the device and this plugin only supplies addresses and installs host per-ENI routing; requires `cilium.enabled=true`. It is **verified end to end on a test cluster but not in production**, and **not a reason to expect more performance** (see [docs/performance.md](docs/performance.md)); steps in [docs/deploy-cilium-netkit.md](docs/deploy-cilium-netkit.md). |
| `cilium.enabled` | `false` | Chain the upstream `cilium-cni`. With `cni.mode=chained` it runs **after** this plugin (`generic-veth` chaining); with `cni.mode=delegated` it is the primary plugin and this one sits in its `ipam` block. Does not install Cilium. See [Network policy](#network-policy). |
| `cilium.waitTimeout` | `""` | How long the daemon waits for the Cilium agent socket before restarting. Go duration; empty keeps the 5m default. |
| `networkPolicy.enabled` | `false` | Require NetworkPolicy to be enforced. Off means policy objects exist and nothing acts on them. On requires `cilium.enabled=true`. |
| `networkPolicy.provider` | `cilium` | Engine expected to enforce policy. Only `cilium` is accepted. |
| `resources.requests.cpu` | `250m` | CPU request for `zec-ipam`. |
| `resources.requests.memory` | `256Mi` | Memory request for `zec-ipam`. |
| `resources.limits` | unset | Accepted and rendered onto the container, but unset by default -- the daemon is on every Pod's CNI ADD path, so a cpu limit throttles Pod startup node-wide and a memory limit turns a spike into an OOMKill that takes IPAM with it. |

The daemon's config schema (`pkg/daemon/config/config.go`) accepts a few more
fields that the chart does not template, among them `disableDevicePlugin`,
`extraRoutes`, `poolCapRatio`, `poolGCPeriodSec`, `subnetStatAgingSec` and
`subnetStatUpdateIntervalSec`. Set them by editing the `eni_conf` JSON in the
ConfigMap directly.

`rateLimit` is parsed but has no consumer: setting it changes nothing, and
nothing warns that it did not. Do not use it to bound OpenAPI call rates.

A node may also be pointed at a per-node ConfigMap with the
`k8s.zenlayer.com/vpc-cni-config` node label, whose value is
`<namespace>.<name>`. Four things about it are easy to get wrong:

- **The namespace has to be the daemon's own**; naming another one is refused and
  the daemon does not start. The label is on the Node, and a kubelet may set
  labels on its own Node with any prefix outside `kubernetes.io` -- this prefix
  is outside it. Without the restriction a node that had been taken over could
  point its own daemon at a config it wrote, and `openAPIDomain` in `eni_conf` is
  the host the daemon sends its credentials to.

- **It covers `eni_conf` and nothing else.** The daemon reads exactly that one
  key from the ConfigMap the label names, and fails to start if the key is
  absent. Everything in the "DaemonSet itself" row above -- the log level
  environment variable, the probe ports, the mounted Secret -- comes from the
  DaemonSet and cannot be changed per node this way.
- **It replaces, it does not merge.** The named `eni_conf` is used whole; nothing
  is inherited from the cluster-wide one. A key you leave out is not "kept at the
  cluster value", it is unset, and the daemon applies its own default or refuses
  to start.
- **The CNI conflist still comes from the cluster-wide ConfigMap.** So
  `zec.dataPath` is not overridable per node either.

### Applying a configuration change

**No setting takes effect while the daemon is running.** Every value is read
once at startup, so a change reaches a node only when its Pod restarts. Reading
config once and requiring a restart is the deliberate choice: a value that can
change mid-flight would have to be safe to change at every point the daemon
might be at, including halfway through attaching an ENI.

`helm upgrade` does the right thing on its own. The DaemonSet carries a
`checksum/config` annotation over the rendered ConfigMap, so changing any value
changes the Pod template and triggers a rolling update.

Editing the ConfigMap by hand does **not** restart anything. Follow it with:

```sh
kubectl -n kube-system rollout restart ds/zenlayer-vpc-cni
```

Run the same command after a **failed** `helm upgrade`. Helm applies the
objects one at a time, so an upgrade that fails partway can already have
rolled the DaemonSet without having written the ConfigMap -- the new Pods then
come up reading the old values. A later successful upgrade does not fix it:
the Pod template is by then unchanged, so nothing rolls a second time and those
nodes stay on the old configuration indefinitely.

The CNI conflist on each node is written once when the daemon starts and is not
re-read afterwards, so the same rule covers `dataPath` and the other conflist
fields.

## Uninstalling

`helm uninstall` removes the Kubernetes objects. It deliberately leaves the
node-local state behind:

| Left on every node | |
| --- | --- |
| `/etc/cni/net.d/10-zenlayer-cni.conflist` | the CNI configuration |
| `/opt/cni/bin/zec-cni` | the CNI binary |
| `/var/lib/cni/zenlayer/` | the address records |
| `/var/run/zenlayer/`, `/var/log/zenlayer/` | the log directory; the socket itself goes with the Pod |
| `/etc/sysctl.d/99-zenlayer-vpc-cni.conf` | the `rp_filter` drop-in |

**The conflist is the one that matters.** While it is present with no daemon
behind it, kubelet still believes this CNI owns the node's Pod networking, and
new Pods stay in `ContainerCreating` rather than falling back to another
plugin.

```sh
rm -f /etc/cni/net.d/10-zenlayer-cni.conflist
```

**Only remove it if another CNI's conflist remains in `/etc/cni/net.d/`.** On a
node where this is the only plugin, taking it away leaves kubelet with no
network plugin at all: the node goes `NotReady` with `cni plugin not
initialized`, Pods still cannot start, and after five minutes the
`node.kubernetes.io/not-ready:NoExecute` taint begins evicting what is running.
Leaving the file in place is the better failure on such a node -- drain it
instead.

ENIs the CNI created stay attached to their instances. While the daemon runs it
reclaims its own leaked ENIs hourly, identifying them by the
`zenlayer:cni:created-by` and `zenlayer:cni:instance-id` tags; once it is gone
nothing does. **After deleting a cluster, check the account for ENIs carrying
those tags.** The trade-off is deliberate: detaching an ENI is a slow API call,
and a Pod's termination grace period is not where a slow API call belongs --
tying teardown to it would mean either a grace period long enough to hurt every
normal restart, or a detach that gets killed halfway.

**Secondary addresses are left behind too, and the two tags above will not find
them.** Whatever the warm pool was holding when the daemon went away stays
assigned to its interface, and an address carries no tag of its own -- only the
interface does. Addresses left on a node's **primary** ENI are therefore
invisible to a tag-based search. Measured on a five-node cluster: 10 addresses
remained after `helm uninstall`, exactly `poolTarget` (2) per node, and 5 of
them sat on primary ENIs with no tag at all. GC adopts them again if the
chart is reinstalled on the same nodes; if the nodes are being retired, list the
secondary addresses of each instance's interfaces and release them explicitly.

**None of that manual cleanup is needed when the instance itself is released --
provided the platform's "release associated resources" option stays on** (in the
OpenAPI it is `ReleaseInstances`'s `DependResource`, default `true`). Releasing
an instance also deletes the ENIs attached to it and the addresses on them,
including the ones this plugin created.

The converse: **releasing a node with that option turned off** (release the
instance only, keep its associated resources) leaves this plugin's ENIs and
addresses behind, with nothing to reclaim them. Clean them up by the two tags
above.

## Building from source

Go 1.26.8 (per `go.mod`) and, for the image, Docker on an `amd64` host.

| Target | What it does |
| --- | --- |
| `make help` | Print the resolved build variables and the available targets. |
| `make bin` | `go mod tidy`, then build both binaries into `output/` (`CGO_ENABLED=0 GOOS=linux`). |
| `make zec-cni` / `make zec-ipam` | Build a single binary. |
| `make pkg` | `bin` plus the bootstrap scripts copied into `output/`. |
| `make image` | Build the container image with `images/Dockerfile`. amd64 only. |
| `make chart-lint` | `helm lint` the chart with the required values filled in. |
| `make chart-template` | Render the chart with the required values filled in. |
| `make chart` | Lint, then `helm package` into `output/charts/`. |
| `make all` | `pkg` + `image` + `chart`. |
| `make tidy` | `go mod tidy`. |
| `make clean` | `go clean`, remove `output/`, remove the built image. |

Useful variables: `OUTPUT` (default `output`), `IMAGE_NAME`
(`docker.io/zenlayer1/zeccni`), `IMAGE_TAG`, `CHART_DIR` (`chart`),
`CHART_OUTPUT` (`output/charts`), `CHART_VERSION` / `CHART_APP_VERSION` (read
from `Chart.yaml`), `ENGINE` (`docker`), `HELM` (`helm`), `GOPROXY`, and
`DEBUG` (`1` by default, which disables optimizations and keeps symbols; set
`DEBUG=0` for a stripped release build).

Run the tests with:

```sh
go test ./...
```

Several packages depend on Linux-only netns helpers and will not build on other
operating systems, so run the tests on Linux -- CI does, with
`GOOS=linux CGO_ENABLED=0` for the job and `CGO_ENABLED=1 go test -race` for the
test step. On a non-Linux workstation, `GOOS=linux go vet ./...` type-checks
everything, test files included.

## Troubleshooting

**Logs.**

- `zec-cni` writes to `/var/log/zenlayer/cni.log` on the node (rotated by
  lumberjack: 256 MB, 2 backups, 7 days). Kubelet also surfaces CNI errors in
  the Pod's events.
- `zec-ipam` logs to stderr: `kubectl -n kube-system logs ds/zenlayer-vpc-cni -c zec-ipam`.
- `zec.logLevel` sets the level for both binaries at install time. It renders
  `ZENLAYER_CNI_LOGLEVEL` onto the DaemonSet and, for `debug` and `trace`, the
  conflist `debug` flag `zec-cni` reads -- `trace` is the more verbose of the
  two, so it does not turn that flag back off.
- Log level and destination can also be overridden with the `ZENLAYER_CNI_LOGLEVEL`
  (`trace|debug|info|warn|error|fatal|panic`) and `ZENLAYER_CNI_LOCATION`
  (a file path, or `stdout`) environment variables. `zec-ipam` also takes
  `-log-level`, which overrides `ZENLAYER_CNI_LOGLEVEL` when given.
- Setting `"debug": true` in the conflist raises `zec-cni` to debug level for
  that invocation.
- `ZENLAYER_CNI_SDK_HTTP_DUMP=true` additionally dumps every OpenAPI request and
  response body. It is off by default and is **not** implied by
  `ZENLAYER_CNI_LOGLEVEL=debug`, because the dump is all-or-nothing: it includes
  the `Authorization` header, so it writes the account's AccessKeyID into the
  log in plaintext on every call. Use it on a throwaway cluster, and treat the
  logs it produces as credentials.

**Health and metrics.** On any node running the DaemonSet:

```sh
curl 127.0.0.1:11414/healthz
curl 127.0.0.1:11414/metrics
```

The daemon serves no profiling endpoints. This port is reachable from off the
node -- kubelet probes it and Prometheus scrapes it -- and `/debug/pprof` there
would hand a heap dump, a CPU profile and the process command line to anything
that can reach the node. Profile a build made for it instead.

**Common startup failures** (all reported by the daemon at startup):

| Message | Cause |
| --- | --- |
| `no credentials for the zenlayer api: ...` | The Secret is missing, misnamed, or its keys are not `AccessKeyID` / `AccessKeyPassword`. The message names both paths it tried and the chart value that points at the Secret. |
| `securityGroup is not set in the cni configmap ...` / `subnets is empty in the cni configmap ...` | `zec.securityGroupID` / `zec.subnetIDs` were not set. |
| `subnets lists N entries but at most 5 are supported` | More than 5 entries in `zec.subnetIDs`. |
| `PoolTargetMin (n) must be less than or equal to PoolTarget (m)` | Warm-pool bounds are inconsistent; the ordering `poolTargetMin <= poolTarget <= poolTargetMax` must hold. |
| `error retrieving service cidr: configmaps "kubeadm-config" not found` | The cluster was not built with kubeadm and `zec.serviceCidr` was left empty. Set it explicitly. |
| `unsupported ipStack <value> in configMap` | `zec.ipStack` is neither `ipv4` nor `dual`. |
| `ipStack "ipv6" (IPv6-only) is not implemented yet` | IPv6-only is not supported. Use `ipv4` or `dual`. |
| `ipStack dual needs every subnet to carry ipv6, but subnet <id> has no ipv6 cidr` | `zec.ipStack=dual` with a subnet that has only an IPv4 CIDR. Give the subnet an IPv6 CIDR, or set `zec.ipStack=ipv4`. |
| `ipv6InternetChargeType "ByTrafficPackage" requires ipv6PackageSize` | Set `zec.ipv6PackageSize` (TB, a positive multiple of 0.1) with `--set-json`. On an account with an hourly billing period, use `ipv6InternetChargeType: ByBandwidth` instead. |
| `ipv6InternetChargeType "BandwidthCluster" requires ipv6ClusterId` | Set `zec.ipv6ClusterId` to the shared bandwidth package the interface joins. |
| `ipStack "dual" needs a positive ipv6Bandwidth (Mbps)` | `zec.ipv6Bandwidth` is 0 or negative. The platform requires a cap for public IPv6. |
| `unsupported daemon mode` | Only `eni_multi_ip` is supported. |

**Pods stuck in `ContainerCreating`.** Check that `/opt/cni/bin/zec-cni` exists
on the node -- the init container installs it -- and then that
`/etc/cni/net.d/10-zenlayer-cni.conflist` and `/var/run/zenlayer/cni.socket`
exist, and read `/var/log/zenlayer/cni.log`.

The order matters, because the conflist is **not** installed by the init
container: the daemon writes it once it is serving, so a missing conflist points
at the daemon, not at the init container -- which will have exited successfully
with nothing missing. With Cilium chaining enabled the daemon additionally waits
for `/var/run/cilium/cilium.sock` before publishing it, so an absent conflist
there means the Cilium agent is not up. See [Architecture](#architecture).

## Further documentation

- [Deploying with Cilium chaining](./docs/deploy-cilium-chained.md) -- the default mode
  `cni.mode=chained`: prerequisites, the steps to switch over from veth, every Cilium
  value that must be set and why, and how to roll back. Read it before enabling network
  policy.
- [Delegated IPAM mode](./docs/deploy-cilium-netkit.md) -- the full walkthrough for
  `cni.mode=delegated`, where Cilium creates the Pod device (netkit or veth) and this
  plugin only supplies addresses and host per-ENI routing.
- [Performance: measurements and how to read them](./docs/performance.md) -- measured
  numbers for each shape, which switches actually buy something, and why cross-session
  comparisons do not hold. Read it before picking a shape.
- [CHANGELOG.md](./CHANGELOG.md) -- release notes, including the known limitations
  of the current release.
- [CONTRIBUTING.md](./CONTRIBUTING.md) -- build, test and lint commands, and what
  a pull request is checked against.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) for the build, test and review process.
Participation is governed by the [Code of Conduct](CODE_OF_CONDUCT.md).

## Security

Please report vulnerabilities privately — see [SECURITY.md](SECURITY.md).

## Changelog

Release notes for each version are in [CHANGELOG.md](./CHANGELOG.md).

## License

Apache License 2.0. See [LICENSE](LICENSE).
