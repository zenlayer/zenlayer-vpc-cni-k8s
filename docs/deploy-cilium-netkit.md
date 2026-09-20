# Deploying the netkit datapath (Cilium as the primary CNI, addresses from this plugin)

> This document is the authoritative version. A Chinese translation is available at
> [deploy-cilium-netkit-cn.md](./deploy-cilium-netkit-cn.md).

This mode hands creation of the Pod device to Cilium and reduces this plugin to its IPAM plugin
(`cni.mode=delegated` on this side):

```
kubelet -> cilium-cni (primary CNI: creates the device, configures networking inside the Pod)
              └─ ipam: zec-cni (allocates addresses, installs host per-ENI routing)
```

IP allocation, ENI lifecycle, the warm pool and GC are unchanged and remain this project's job.

## Contents

- [1. Prerequisites](#1-prerequisites)
- [2. Quick start: switching from veth to this mode](#2-quick-start-switching-from-veth-to-this-mode)
- [3. Capability matrix](#3-capability-matrix)
- [4. Configuration reference](#4-configuration-reference)
- [5. Rolling back to veth mode](#5-rolling-back-to-veth-mode)

## 1. Prerequisites

| Item | Requirement | Notes |
|---|---|---|
| **Cilium version** | **>= v1.16** | `bpf.datapathMode` with netkit is available from 1.16; this document is written against v1.17.5 |
| Kernel | **>= 6.7** | The minimum for the netkit device itself, higher than chained mode's 5.4 |
| cgroup v2 | mounted | `stat -fc %T /sys/fs/cgroup` prints `cgroup2fs`; a hard prerequisite of `kubeProxyReplacement` |
| `kubeProxyReplacement` | **must be true** | Both netkit and eBPF host-routing depend on it |
| This plugin already running | yes | Get it working with `cni.mode=chained` first, then switch |
| `helm` | reachable from wherever you run it | Does not need to be installed on a cluster node |
| Cluster IP stack | single-stack IPv4, or dual-stack per [§4.6](#46-ipv4-single-stack-and-dual-stack) | Omitting those two keys on a dual-stack cluster leaves Pod IPv6 dropped silently |

**The device type is fixed when a Pod is created.** Switching `bpf.datapathMode` changes what new
Pods get; Pods already running keep the device they were created with until they are recreated, so
a node carries both kinds until the workloads are rolled. The setting is cluster-wide -- there is
no per-node rollout.

## 2. Quick start: switching from veth to this mode

Why each parameter has the value it has is in [§4](#4-configuration-reference).

```bash
# 0. Check the prerequisites
stat -fc %T /sys/fs/cgroup                      # must be cgroup2fs
uname -r                                        # netkit needs >= 6.7
kubectl -n kube-system get ds zenlayer-vpc-cni  # must be ready
helm version --short
# Is the cluster dual-stack? A non-zero count means step 1 needs the two keys from §4.6
kubectl get nodes -o jsonpath='{.items[*].status.addresses[*].address}' | tr ' ' '\n' | grep -c ':'

# 1. Install Cilium (if it is already installed for chained mode, use the helm upgrade at the
#    end of this section instead)
helm repo add cilium https://helm.cilium.io/ && helm repo update cilium
helm install cilium cilium/cilium --version 1.17.5 --namespace kube-system \
  --set cni.customConf=true --set cni.exclusive=false --set cni.install=true \
  --set routingMode=native \
  --set bpf.datapathMode=netkit \
  --set installUplinkRoutesForDelegatedIPAM=false \
  --set enableIPv4Masquerade=false --set enableIPv6Masquerade=false --set bpf.masquerade=false \
  --set ipam.mode=delegated-plugin --set extraConfig.local-router-ipv4=169.254.32.1 \
  --set endpointHealthChecking.enabled=false --set envoyConfig.enabled=false \
  --set endpointRoutes.enabled=true \
  --set kubeProxyReplacement=true --set bpf.hostLegacyRouting=false \
  --set-string extraConfig.enable-session-affinity=true \
  --set operator.replicas=2
kubectl -n kube-system rollout status ds/cilium --timeout=10m

# 2. Switch this project to delegated. The daemon waits for the Cilium agent to be ready before
#    writing the new conflist
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 1.0.0 \
  -n kube-system --reuse-values \
  --set cni.mode=delegated --set cilium.enabled=true --set networkPolicy.enabled=true
kubectl -n kube-system rollout restart daemonset/zenlayer-vpc-cni
kubectl -n kube-system rollout status daemonset/zenlayer-vpc-cni --timeout=10m

# 3. Confirm every node has taken over Services, and only then remove kube-proxy
kubectl -n kube-system get pod -l k8s-app=cilium -o name | while read -r p; do
  kubectl -n kube-system exec "${p#pod/}" -c cilium-agent -- \
    cilium-dbg status | grep -E '^KubeProxyReplacement:|^Routing:|^Device Mode:'
done                                            # every node must show True + Host: BPF + netkit
kubectl -n kube-system get ds kube-proxy -o yaml > kube-proxy-backup.yaml
kubectl -n kube-system patch ds kube-proxy -p \
  '{"spec":{"template":{"spec":{"nodeSelector":{"kube-proxy-disabled":"true"}}}}}'

# 4. On every node, clear the rules kube-proxy left behind
iptables-save  | grep -v KUBE | iptables-restore
ip6tables-save | grep -v KUBE | ip6tables-restore

# 5. Roll every workload -- this cannot be skipped; old Pods are still on veths this project made
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout restart "$t/$n"; done
done

# 6. Clear the Cilium endpoints left behind by the switch (see the warning below)
kubectl -n kube-system get pod -l k8s-app=cilium -o name | while read -r p; do
  kubectl -n kube-system exec "${p#pod/}" -c cilium-agent -- \
    cilium-dbg endpoint list | grep -E 'eni[0-9a-f]+'
done                                            # delete each one with cilium-dbg endpoint delete <id>

# 7. Confirm the conflist is in delegated form: cilium-cni on top, ipam naming zec-cni
kubectl -n kube-system get pod -l app=zenlayer-vpc-cni -o name | while read -r p; do
  printf '%s: ' "${p#pod/}"
  kubectl -n kube-system exec "${p#pod/}" -c zec-ipam -- \
    grep -o '"type": "[a-z-]*"' /host-etc-net.d/10-zenlayer-cni.conflist | tr '\n' ' '; echo
done
```

If Cilium was already installed for chained mode, replace step 1 with:

```bash
helm upgrade cilium cilium/cilium --version 1.17.5 -n kube-system --reuse-values \
  --set bpf.datapathMode=netkit \
  --set installUplinkRoutesForDelegatedIPAM=false \
  --set cni.chainingMode=null
kubectl -n kube-system rollout restart ds/cilium
kubectl -n kube-system rollout status ds/cilium --timeout=10m
```

> **⚠️ Step 6 cannot be skipped.** The switch leaves Cilium endpoints pointing at the old devices:
> the host device is an `eni*` veth this project created (and which no longer exists) rather than a
> netkit `lxc*`. Neither consequence raises an error -- when a new Pod is given an address a stale
> endpoint still holds, `cilium-cni` returns `400 IP <addr> is already in use` and CNI ADD has no
> fallback path; the subtler case is an address that is reclaimed and handed to a Pod on **another
> node** while the node with the stale endpoint still treats it as local, so traffic for that Pod is
> intercepted there. The root cause is that the truth about an address exists in two places that are
> never reconciled: this project's IPAM pool considers it released, Cilium's endpoint manager
> considers it in use.

## 3. Capability matrix

| | Chained mode (default) | **netkit mode (this document)** |
|---|---|---|
| Pod device | created by this plugin (veth pair) | **created by Cilium** (netkit or veth) |
| Pod addresses | this plugin | this plugin (unchanged) |
| ENI lifecycle, warm pool, GC | this plugin | this plugin (unchanged) |
| Host per-ENI policy routing | this plugin | this plugin (unchanged) |
| NetworkPolicy / Hubble | Cilium | Cilium |
| Service forwarding | Cilium (with KPR on) | Cilium (KPR is mandatory) |
| `hostPort` | ✅ | ✅ but the port must sit outside the NodePort range, see §4.5 |
| `ExtraRoutes` in the conflist | ✅ written into the container | ❌ **no way out**, see §4.2 |
| `veth_prefix` in the conflist | settable, takes effect | ❌ meaningless |
| Pods with several interfaces | ✅ | ❓ **unverified**; use a single interface |
| Production ready | ✅ | ❌ **unverified** |

## 4. Configuration reference

### 4.1 This project's values

| Setting | Value here | Notes |
| --- | --- | --- |
| `cni.mode` | `delegated` | Renders the delegated conflist: `cilium-cni` is the primary plugin and this project sits in its `ipam` block. Requires `cilium.enabled=true`, or the chart `fail`s. |
| `cilium.enabled` | `true` | The daemon **blocks** waiting for `/var/run/cilium/cilium.sock` before writing the new conflist. Cilium's ADD has no fallback path, so without the agent every Pod fails. |
| `networkPolicy.enabled` | as needed | Declares only that "policy must be enforced"; requires `cilium.enabled=true` as well. |
| `zec.dataPath` | leave unset | It describes a device this project builds itself, which it does not do here; the schema only allows `veth` anyway. |

### 4.2 Keys in the conflist

Two keys in the `ipam` block are fixed by the template:

| Key | Value | Notes |
| --- | --- | --- |
| `zec_mode` | `ipam-only` | Selects the IPAM-only path. **An unrecognised value is refused** rather than falling back to the full plugin -- a typo that was ignored would leave the node running a datapath its configuration does not describe. |
| `install_host_routes` | `true` | Makes the IPAM-only path install this Pod's per-ENI ip rule (`from <podIP>` at priority 2048) and that ENI's default route. With it off, a Pod on a secondary ENI leaves through the instance's own interface with a source address belonging to another port's MAC, and the network drops it. The default is `true`. |

This project creates no device, so the keys that describe networking inside the Pod stop applying:

| Key | In this mode |
| --- | --- |
| `ExtraRoutes` | **Unsupported**. Routes in a delegated IPAM result are discarded by the primary CNI; the requirement has to be expressed on the Cilium side instead. |
| `DefaultRoute` | Decided by Cilium, pointing at its own `cilium_host`. |
| `veth_prefix` | Meaningless. This project builds no device, so there is no host veth to name. |
| `eniip_virtual_type` | Meaningless; not rendered into the conflist. |
| `mtu` | Decided by Cilium's `MTU` / `RouteMTU`. **The two sides must agree**, or you get an MTU black hole. |

### 4.3 Cilium values

The table below is the complete set of Cilium values this design requires. Differences from
[chained mode](deploy-cilium-chained.md) are marked **★**.

| Setting | Value here | Notes |
| --- | --- | --- |
| ★ `bpf.datapathMode` | `netkit` | Pod devices are netkit rather than veth. `netkit-l2` selects L2 mode instead; which to use is [§4.4](#44-choosing-between-netkit-l3-and-l2). |
| ★ `installUplinkRoutesForDelegatedIPAM` | `false` | **Must be off.** This is Cilium's own path for installing host routes, and it does not work on this platform: it requires the uplink interface to carry the `IFF_SLAVE` flag, which this platform's ENIs do not, and it recognises only one routing table per node, so every ENI after the first would share a table ID. Host routing is installed by this project instead, following "one table per ENI, table ID = 1000 + ifindex". |
| ★ `cni.chainingMode` | **leave unset** | This is not chaining; Cilium is the primary CNI. Setting it makes Cilium look for a chainer and fail. |
| `cni.customConf` | `true` | The conflist is written by this project; Cilium must not render its own. |
| `cni.exclusive` | `false` | Defence in depth. |
| `cni.install` | `true` | The `/opt/cni/bin/cilium-cni` binary is needed. |
| `routingMode` | `native` | Pod IPs are ENI secondary addresses, natively routable in the VPC; no tunnel is needed. |
| `ipam.mode` | `delegated-plugin` | Tells Cilium that addresses come from the CNI binary, that it must not participate, and that the operator must not hand out PodCIDRs. |
| `extraConfig.local-router-ipv4` | `169.254.32.1` | ⚠️ **Not a helm value**; `--set localRouterIPv4=` is silently discarded and the agent then fails to start. The value only has to avoid `169.254.1.1`, this plugin's gateway for Pods. |
| `endpointRoutes.enabled` | `true` | **Must be on together with** `local-router-ipv4`, or the agent goes `fatal`. It also supplies the main-table route to each Pod -- there is no host veth in this mode, so that route comes from here, and this project no longer installs it; otherwise one address would have two routes pointing at different devices. |
| `endpointHealthChecking.enabled` | `false` | A mandatory companion of `delegated-plugin`. |
| `envoyConfig.enabled` | `false` | Same. The cost is giving up Ingress / Gateway API. |
| `kubeProxyReplacement` | `true` | ⚠️ Enabled **as a pair** with the next row, and a prerequisite of netkit. |
| `bpf.hostLegacyRouting` | `false` | ⚠️ Same. Setting it alone does nothing: whenever `kubeProxyReplacement` is not `true` the agent resets it to `true`. |
| `enableIPv4Masquerade` | `false` | ⚠️ Masquerading must be off entirely, see below. |
| `enableIPv6Masquerade` | `false` | Same. |
| `bpf.masquerade` | `false` | Same. |
| `extraConfig.enable-session-affinity` | `true` | The agent derives it when KPR is on, so omitting it does not stop `sessionAffinity: ClientIP` from working; writing it explicitly keeps the configuration correct if KPR is later turned off. |
| `operator.replicas` | `2` | Removal of the `node.cilium.io/agent-not-ready` taint is the operator's exclusive job, and one replica has no redundancy. |

**⚠️ Masquerading must be off entirely.** Pod IPs are routable inside the VPC, so masquerading
rewrites the source to the node's address. More importantly, an agent that finds **iptables-based**
masquerading enabled resets `bpf.hostLegacyRouting` to `true`, which disables eBPF host-routing --
and this mode's performance rests on that, not on the netkit device itself.

**Do not set `enable-node-port` on its own.** With `kubeProxyReplacement=true` the agent derives it,
along with `enable-external-ips`, `bpf-lb-sock`, `enable-host-port` and `enable-session-affinity`.
The ConfigMap is input, not effective configuration; for anything KPR-related, read
`cilium-dbg config -a`.

Service and policy semantics are identical to the default shape of chained mode, including
`externalTrafficPolicy: Local` having no effect on in-cluster traffic.

### 4.4 Choosing between netkit L3 and L2

`bpf.datapathMode` selects which one the Pod device is created in:

| | **L3 (`netkit`)** | **L2 (`netkit-l2`)** |
|---|---|---|
| MAC address on the Pod interface | none, all zeroes | present |
| ARP handling | not needed | needed |
| Performance | upstream's recommendation | the difference from L3 is not resolvable in TCP_RR ([performance.md](performance.md) §6.1) |
| Use it when | everything else | a workload needs Ethernet-layer semantics inside the Pod |
| Verified here | ✅ | ✅ equivalent to L3 |

**The concrete case for L2 is a workload that reads the interface's MAC.** KubeVirt passes it to
libvirt when defining a VM; under L3 that address is empty and virt-handler fails with
`XML error: unable to parse mac address ''`. Anything else that reads the MAC, or that does ARP or
DHCP inside the Pod, hits the same wall.

Everything else in this document is the same for both. The only change is one value:

```bash
  --set bpf.datapathMode=netkit-l2
```

- **Switching between L3 and L2 is cluster-wide and needs the Pods recreated**, exactly like the
  switch from veth (§1). The device mode is fixed when a Pod is created.
- **The two modes are equivalent across everything this plugin owns**: per-ENI rules and routes,
  address allocation and reclaim, egress across multiple ENIs, the first Pod on a freshly attached
  ENI, NetworkPolicy enforcement, and every form of Service. Their behaviour differs only in
  neighbour resolution: under L3 the device carries `NOARP` and both neighbour tables stay empty,
  while under L2 host-to-Pod uses ARP/NDP with dynamic `REACHABLE` entries and the Pod's requests
  for its gateway are answered in place by Cilium's BPF program.
- On **Cilium >= 1.20** there is also `auto`, which probes the kernel and **silently falls back to
  veth** when netkit is unavailable. Do not use it here: in this mode a silent fallback would leave
  the cluster on a datapath the configuration does not describe.

### 4.5 A `hostPort` must not fall inside the NodePort range

With `kubeProxyReplacement=true` Cilium serves hostPort natively from its own BPF load balancer, so
the conflist chains no `portmap` plugin in this mode. But the agent **refuses any hostPort that
falls inside the configured NodePort range** (30000-32767 by default), logging:

```
The requested hostPort 31883 is colliding with the configured NodePort range [30000, 32767]. Ignoring.
```

**Nothing else reports it.** The Pod starts normally, `cilium-dbg status --verbose` still prints
`HostPort: Enabled`, and only `cilium-dbg service list` shows that no HostPort entry was created.
Nothing else implements hostPort in this mode, so a rejected port answers from nowhere at all.

Measured with a port outside the range (18081), a hostPort works from **every** direction: the Pod's
own node, a Pod on another node, another node's host namespace, a non-cluster VM inside the VPC, and
from outside the cluster. `cilium-dbg service list` then shows four HostPort entries -- IPv4 and
IPv6, node address and wildcard.

**So pick a hostPort below 30000**, and verify it took effect:

```bash
kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg service list | grep HostPort
```

An empty result means the port was rejected, whatever `status --verbose` says.

### 4.6 IPv4 single-stack and dual-stack

The IP stack is selected by parameters; the only difference between the two is these keys:

| Cluster IP stack | Cilium values to add |
|---|---|
| **Single-stack IPv4** | none (the quick start above is the single-stack configuration) |
| **Dual-stack** | `--set ipv6.enabled=true`<br>`--set extraConfig.local-router-ipv6=fe80::ffff:1` |

- The cluster itself must be dual-stack (Node objects carrying IPv6), or adding these keys is
  pointless.
- `local-router-ipv6` must avoid `fe80::1` -- that is this plugin's IPv6 gateway for Pods.
- **Omitting** these keys on a dual-stack cluster leaves Pod IPv6 dropped silently, with no error
  log at all.

## 5. Rolling back to veth mode

```bash
# ① Switch this project back to chained first
helm upgrade zenlayer-vpc-cni <chart> -n kube-system --reuse-values --set cni.mode=chained
kubectl -n kube-system rollout restart daemonset/zenlayer-vpc-cni

# ② Move Cilium back to the chained parameters
helm upgrade cilium cilium/cilium --version 1.17.5 -n kube-system --reuse-values \
  --set cni.chainingMode=generic-veth --set bpf.datapathMode=veth
kubectl -n kube-system rollout restart ds/cilium

# ③ Roll every workload -- required, or the netkit devices stay in place
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout restart "$t/$n"; done
done
```

This returns you to **chained mode on veth**. To continue back to plain veth without Cilium, and
for cleaning up what an uninstall leaves behind (socket LB programs on the cgroup, orphaned
CiliumEndpoints), see the rollback section of
[deploy-cilium-chained.md](deploy-cilium-chained.md).
