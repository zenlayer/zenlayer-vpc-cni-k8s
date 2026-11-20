# Deploying the netkit datapath (Cilium as the primary CNI, addresses from this plugin)

> This document is the authoritative version. A Chinese translation is available at
> [deploy-cilium-netkit-cn.md](./deploy-cilium-netkit-cn.md).

In this mode, Cilium creates the Pod device, and this plugin acts only as Cilium's IPAM plugin
(`cni.mode=delegated` on this side):

```
kubelet -> cilium-cni (primary CNI: creates the device, configures networking inside the Pod)
              └─ ipam: zec-cni (allocates addresses, installs host per-ENI routing)
```

IP allocation, the ENI lifecycle, the warm pool, and GC are unchanged and remain the responsibility
of this project.

## Contents

- [1. Prerequisites](#1-prerequisites)
- [2. Quick start: switching from veth to this mode](#2-quick-start-switching-from-veth-to-this-mode)
- [3. Capability matrix](#3-capability-matrix)
- [4. Configuration reference](#4-configuration-reference)
- [5. Rolling back to veth mode](#5-rolling-back-to-veth-mode)

## 1. Prerequisites

| Item | Requirement | Notes |
|---|---|---|
| **Cilium version** | **>= v1.16** | `bpf.datapathMode` with netkit is available from 1.16. This document is written against v1.17.5 |
| Kernel | **>= 6.8** | Cilium documents netkit as available from kernel 6.8; higher than the 5.4 required by chained mode |
| cgroup v2 | mounted | `stat -fc %T /sys/fs/cgroup` prints `cgroup2fs`. A hard prerequisite of `kubeProxyReplacement` |
| `kubeProxyReplacement` | **must be true** | Both netkit and eBPF host-routing depend on it |
| This plugin already running | yes | Get it working with `cni.mode=chained` first, then switch |
| `helm` | available on the machine that runs the commands | Does not need to be installed on a cluster node |
| Cluster IP stack | single-stack IPv4, or dual-stack per [§4.6](#46-ipv4-single-stack-and-dual-stack) | On a dual-stack cluster, omitting those two keys causes Pod IPv6 to be dropped silently |

**The device type is fixed when a Pod is created.** Switching `bpf.datapathMode` changes the device
that new Pods receive. Pods that are already running keep the device they were created with until
they are recreated, so a node carries both device types until the workloads are rolled. The setting
is cluster-wide; it cannot be rolled out per node.

## 2. Quick start: switching from veth to this mode

The reason for each parameter value is given in [§4](#4-configuration-reference).

```bash
# 0. Check the prerequisites
stat -fc %T /sys/fs/cgroup                      # must be cgroup2fs
uname -r                                        # netkit needs >= 6.8
kubectl -n kube-system get ds zenlayer-vpc-cni  # must be ready
helm version --short
# Is the cluster dual-stack? A non-zero count means step 1 needs the two keys from §4.6
kubectl get nodes -o jsonpath='{.items[*].status.addresses[*].address}' | tr ' ' '\n' | grep -c ':'

# The apiserver entry point -- step 1's k8sServiceHost=auto reads exactly this line. It must print
# something with a port, and should be a load balancer / VIP; a single master's IP becomes a single
# point of failure. No output (not a kubeadm cluster): see the `k8sServiceHost` row in §4.3
kubectl -n kube-public get cm cluster-info -o jsonpath='{.data.kubeconfig}' | grep server:

# 1. Install Cilium (if it is already installed for chained mode, use the helm upgrade at the
#    end of this section instead)
helm repo add --force-update cilium https://helm.cilium.io/ && helm repo update cilium
helm install cilium cilium/cilium --version 1.17.5 --namespace kube-system \
  --set cni.customConf=true --set cni.exclusive=false --set cni.install=true \
  --set routingMode=native \
  --set bpf.datapathMode=netkit \
  --set ipam.installUplinkRoutesForDelegatedIPAM=false \
  --set enableIPv4Masquerade=false --set enableIPv6Masquerade=false --set bpf.masquerade=false \
  --set ipam.mode=delegated-plugin --set extraConfig.local-router-ipv4=169.254.32.1 \
  --set endpointHealthChecking.enabled=false --set envoyConfig.enabled=false \
  --set endpointRoutes.enabled=true \
  --set kubeProxyReplacement=true --set bpf.hostLegacyRouting=false \
  --set k8sServiceHost=auto \
  --set-string extraConfig.enable-session-affinity=true \
  --set operator.replicas=2
kubectl -n kube-system rollout status ds/cilium --timeout=10m
# Confirm auto resolved to a real address -- a literal auto, or nothing, means the agent cannot reach the apiserver
kubectl -n kube-system get ds cilium -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="KUBERNETES_SERVICE_HOST")].value}{"\n"}'

# 2. Switch this project to delegated. The daemon waits for the Cilium agent to be ready before
#    writing the new conflist. networkPolicy.enabled is as needed (§4.1). --reset-then-reuse-values
#    (Helm >= 3.14): --reuse-values would also keep the old chart version's defaults
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.1 \
  -n kube-system --reset-then-reuse-values \
  --set cni.mode=delegated --set cilium.enabled=true --set networkPolicy.enabled=true
kubectl -n kube-system rollout restart daemonset/zenlayer-vpc-cni
kubectl -n kube-system rollout status daemonset/zenlayer-vpc-cni --timeout=10m
# Must print the apiserver entry point the daemon connects to (§4.1, zec.k8sServiceHost); with
# cilium.enabled this chart refuses to render without one, so empty means the DaemonSet is from an older chart
kubectl -n kube-system get ds zenlayer-vpc-cni -o jsonpath='{..env[?(@.name=="KUBERNETES_SERVICE_HOST")].value}{"\n"}'

# 3. Confirm every node has taken over Services, and only then remove kube-proxy
kubectl -n kube-system get pod -l k8s-app=cilium -o name | while read -r p; do
  kubectl -n kube-system exec "${p#pod/}" -c cilium-agent -- \
    cilium-dbg status | grep -E '^KubeProxyReplacement:|^Routing:|^Device Mode:'
done                                            # every node must show True + Host: BPF + netkit
# Keep the first backup: if chained mode already disabled kube-proxy, its backup is the one to restore
[ -e kube-proxy-backup.yaml ] || kubectl -n kube-system get ds kube-proxy -o yaml > kube-proxy-backup.yaml
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

# 6. Clear the Cilium endpoints left behind by the switch (see the warning below). First wait for
#    every workload restarted in step 5: rollout restart returns before the new Pods exist
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout status "$t/$n" --timeout=10m; done
done
while kubectl get pod -A --no-headers | grep -q Terminating; do sleep 5; done   # old Pods gone too
# Then list, per node, only the endpoints whose eni* host device no longer exists
kubectl -n kube-system get pod -l k8s-app=cilium -o name | while read -r p; do
  echo "== ${p#pod/}"
  kubectl -n kube-system exec "${p#pod/}" -c cilium-agent -- sh -c '
    cilium-dbg endpoint list -o jsonpath="{range [*]}{.id} {.status.networking.interface-name}{\"\n\"}{end}" |
    while read -r id ifn; do
      case "$ifn" in eni*) ip link show "$ifn" >/dev/null 2>&1 || echo "$id $ifn";; esac
    done'
done
# Remove each listed id on the node it was listed under:
#   kubectl -n kube-system exec <cilium-pod> -c cilium-agent -- cilium-dbg endpoint disconnect <id>

# 7. Confirm the conflist is in delegated form: cilium-cni on top, ipam naming zec-cni
kubectl -n kube-system get pod -l app=zenlayer-vpc-cni -o name | while read -r p; do
  printf '%s: ' "${p#pod/}"
  kubectl -n kube-system exec "${p#pod/}" -c zec-ipam -- \
    grep -o '"type": "[a-z-]*"' /host-etc-net.d/10-zenlayer-cni.conflist | tr '\n' ' '; echo
done
```

If Cilium was already installed for chained mode, replace step 1 with:

```bash
helm upgrade cilium cilium/cilium --version 1.17.5 -n kube-system --reset-then-reuse-values \
  --set bpf.datapathMode=netkit \
  --set ipam.installUplinkRoutesForDelegatedIPAM=false \
  --set cni.chainingMode=null \
  --set k8sServiceHost=auto
kubectl -n kube-system rollout restart ds/cilium
kubectl -n kube-system rollout status ds/cilium --timeout=10m
# Must print nothing: the chaining mode is really gone
kubectl -n kube-system get cm cilium-config -o jsonpath='{.data.cni-chaining-mode}{"\n"}'
```

> **⚠️ Step 6 cannot be skipped.** The switch can leave Cilium endpoints that point at the old
> devices. An old Pod that is torn down through the new conflist normally takes its endpoint with it;
> an old Pod that is not torn down that way leaves its endpoint behind. The host device of such an
> endpoint is an `eni*` veth that this project created (and that no longer exists) rather than a
> netkit `lxc*` device. Neither of the following consequences raises an error:
>
> - When a new Pod is given an address that a stale endpoint still holds, `cilium-cni` returns
>   `400 IP <addr> is already in use`, and CNI ADD has no fallback path.
> - In the subtler case, an address is reclaimed and handed to a Pod on **another node** while the
>   node with the stale endpoint still treats it as local, so traffic for that Pod is intercepted on
>   that node.
>
> The root cause is that the truth about an address exists in two places that are never reconciled:
> this project's IPAM pool considers the address released, while Cilium's endpoint manager considers
> it in use.
>
> **Do not delete by name prefix alone.** An `eni*` endpoint whose device still exists belongs to a
> live Pod: either a Pod whose rollout has not finished, or a Pod that step 5 does not restart at all
> (Jobs, bare Pods, and static Pods). In this mode, Cilium's endpoint is what carries a Pod's
> traffic, so deleting it cuts that Pod off. Recreate those Pods manually, then run the listing
> again. `eni` is this plugin's default host veth prefix (`veth_prefix`); if it was changed,
> substitute the configured prefix. The check runs inside the agent container, which shares the host
> network namespace and ships `sh` and `ip`. The removal command is
> `cilium-dbg endpoint disconnect <id>`; `cilium-dbg` has no `endpoint delete` command.

## 3. Capability matrix

| | Chained mode (recommended) | **netkit mode (this document)** |
|---|---|---|
| Pod device | created by this plugin (veth pair) | **created by Cilium** (netkit or veth) |
| Pod addresses | this plugin | this plugin (unchanged) |
| ENI lifecycle, warm pool, GC | this plugin | this plugin (unchanged) |
| Host per-ENI policy routing | this plugin | this plugin (unchanged) |
| NetworkPolicy / Hubble | Cilium | Cilium |
| Service forwarding | Cilium (with KPR on) | Cilium (KPR is mandatory) |
| `hostPort` | ✅ | ✅ but the port must sit outside the NodePort range, see §4.5 |
| `ExtraRoutes` in the conflist | ✅ written into the container | ❌ **no way out**, see §4.2 |
| `veth_prefix` in the conflist | can be set and takes effect | ❌ meaningless |
| Pods with several interfaces | ✅ | ❓ **unverified**; use a single interface |
| Production ready | ✅ | ❌ **unverified** |

## 4. Configuration reference

### 4.1 This project's values

| Setting | Value here | Notes |
| --- | --- | --- |
| `cni.mode` | `delegated` | Renders the delegated conflist: `cilium-cni` is the primary plugin and this project sits in its `ipam` block. Requires `cilium.enabled=true`; otherwise the chart `fail`s. |
| `cilium.enabled` | `true` | The daemon **blocks** waiting for `/var/run/cilium/cilium.sock` before writing the new conflist. Cilium's ADD has no fallback path, so if the agent is not present, every Pod fails. |
| `networkPolicy.enabled` | as needed | Declares only that "policy must be enforced". Also requires `cilium.enabled=true`. |
| `zec.dataPath` | leave unset | This key describes a device that this project builds itself, and this project builds no device in this mode. The schema also allows only `veth`. |
| `zec.k8sServiceHost` / `zec.k8sServicePort` | usually empty | The apiserver entry point that the daemon and controlplane use instead of the in-cluster Service. If it is empty and `cilium.enabled=true`, it is read from `kube-public/cluster-info` at install / upgrade. Under `helm template` or another offline render, or where that ConfigMap is missing, the chart refuses to render until the host is set. Set the port together with the host: a host set alone still renders, and the in-cluster Service port 443 is then used. For why this matters, see the `k8sServiceHost` row in [§4.3](#43-cilium-values). |

### 4.2 Keys in the conflist

Two behavioral keys in the `ipam` block are fixed by the template (the third key, `debug`, follows `zec.logLevel`):

| Key | Value | Notes |
| --- | --- | --- |
| `zec_mode` | `ipam-only` | Selects the IPAM-only path. **An unrecognized value is refused** rather than falling back to the full plugin. If a typo were ignored, the node would run a datapath that its configuration does not describe. |
| `install_host_routes` | `true` | Makes the IPAM-only path install this Pod's two ip rules and that ENI's default route. The two rules are `from <podIP>` to the ENI's table at priority 2048, and `to <podIP> lookup main` at priority 512, so that same-node traffic to the Pod is not diverted by the first rule. With this key off, a Pod on a secondary ENI leaves through the instance's own interface with a source address that belongs to another port's MAC, and the network drops it. The default is `true`. |

This project creates no device, so the keys that describe networking inside the Pod no longer apply:

| Key | In this mode |
| --- | --- |
| `ExtraRoutes` | **Unsupported**. Routes in a delegated IPAM result are discarded by the primary CNI; the requirement must be expressed on the Cilium side instead. |
| `DefaultRoute` | Decided by Cilium, pointing at its own `cilium_host`. |
| `veth_prefix` | Meaningless. This project builds no device, so there is no host veth to name. |
| `eniip_virtual_type` | Meaningless; not rendered into the conflist. |
| `mtu` | Decided by Cilium's `MTU` / `RouteMTU`. **The two sides must agree**; otherwise, an MTU black hole results. |

### 4.3 Cilium values

The following table lists the complete set of Cilium values that this design requires. Differences
from [chained mode](deploy-cilium-chained.md) are marked **★**.

| Setting | Value here | Notes |
| --- | --- | --- |
| ★ `bpf.datapathMode` | `netkit` | Pod devices are netkit rather than veth. `netkit-l2` selects L2 mode instead; for how to choose, see [§4.4](#44-choosing-between-netkit-l3-and-l2). |
| ★ `ipam.installUplinkRoutesForDelegatedIPAM` | `false` | **Must be off.** This is Cilium's own path for installing host routes, and it does not work on this platform. It requires the uplink interface to carry the `IFF_SLAVE` flag, which this platform's ENIs do not carry. It also recognizes only one routing table per node, so every ENI after the first would share a table ID. This project installs host routing instead, following "one table per ENI, table ID = 1000 + ifindex". |
| ★ `cni.chainingMode` | **leave unset** | This is not chaining; Cilium is the primary CNI. A value left over from chained mode is accepted silently (the agent keeps running), but the configuration then names a mode that the cluster is not in. Clear it as the step 1 variant in §2 does, and check that `cni-chaining-mode` in `cilium-config` is empty. **Note:** `--reuse-values --set cni.chainingMode=null` does **not** clear it; `--reset-then-reuse-values` does. |
| `cni.customConf` | `true` | The conflist is written by this project; Cilium must not render its own. |
| `cni.exclusive` | `false` | Defense in depth. |
| `cni.install` | `true` | The `/opt/cni/bin/cilium-cni` binary is required. |
| `routingMode` | `native` | Pod IPs are ENI secondary addresses, natively routable in the VPC; no tunnel is needed. |
| `ipam.mode` | `delegated-plugin` | Tells Cilium that addresses come from the CNI binary, that it must not participate, and that the operator must not hand out PodCIDRs. |
| `extraConfig.local-router-ipv4` | `169.254.32.1` | ⚠️ **Not a helm value**; `--set localRouterIPv4=` is silently discarded, and the agent then fails to start. The value only needs to avoid `169.254.1.1`, which is this plugin's gateway for Pods. |
| `endpointRoutes.enabled` | `true` | **Must be on together with** `local-router-ipv4`; otherwise, the agent goes `fatal`. It also supplies the main-table route to each Pod. There is no host veth in this mode, so that route comes from here, and this project no longer installs it; otherwise, one address would have two routes pointing at different devices. |
| `endpointHealthChecking.enabled` | `false` | A mandatory companion of `delegated-plugin`. |
| `envoyConfig.enabled` | `false` | Same. The cost is giving up Ingress / Gateway API. |
| `kubeProxyReplacement` | `true` | ⚠️ Enabled **as a pair** with the next row, and a prerequisite of netkit. |
| `bpf.hostLegacyRouting` | `false` | ⚠️ Same. Setting it alone does nothing: whenever `kubeProxyReplacement` is not `true` the agent resets it to `true`. |
| `k8sServiceHost` | `auto` | ⚠️ **Required.** With kube-proxy removed, only Cilium implements ClusterIPs. An agent that still reaches the apiserver through the ClusterIP cannot start after a full node reboot, so nothing implements the ClusterIP either. As a result, the agent and this plugin's daemon both CrashLoop with `dial tcp <ClusterIP>:443: connect: network is unreachable`. `auto` makes helm read the apiserver entry point (kubeadm's `controlPlaneEndpoint`) from `kube-public/cluster-info` at install time, including the port. Three caveats: **(1) that entry point should be a load balancer / VIP**; a single master's IP makes it a single point of failure. **(2) A missing ConfigMap fails differently by Cilium version**: v1.16.0 to v1.16.2 fail to render; **from v1.16.3 to v1.18 helm does not fail**, and instead writes the literal `auto` as the address; from v1.19 rendering fails again (`ConfigMap kube-public/cluster-info not found`). On a non-kubeadm cluster set `--set k8sServiceHost=<entry-address> --set k8sServicePort=<port>` instead. **An IPv6-literal entry point (`https://[fd00::10]:6443`) must be set explicitly as well** (`--set k8sServiceHost=fd00::10 --set k8sServicePort=6443`): Cilium's lookup takes everything before the first `:` as the host and yields `[fd00`. This chart's own lookup for `zec.k8sServiceHost` handles such an entry point. **(3) It is resolved once, at install / upgrade**; `helm template` or any offline render cannot see the cluster and so behaves as in (2), and a changed entry point needs another `helm upgrade`. **This plugin's daemon and controlplane need the same entry point, for the same reason**: with `cilium.enabled=true` and `zec.k8sServiceHost` left empty, this chart reads it from the same ConfigMap at install / upgrade and prints the result in the release notes; where the ConfigMap is missing, set `zec.k8sServiceHost` / `zec.k8sServicePort` to the entry point. |
| `enableIPv4Masquerade` | `false` | ⚠️ Masquerading must be off entirely, see below. |
| `enableIPv6Masquerade` | `false` | Same. |
| `bpf.masquerade` | `false` | Same. |
| `extraConfig.enable-session-affinity` | `true` | The agent derives it when KPR is on, so omitting it does not stop `sessionAffinity: ClientIP` from working; writing it explicitly keeps the configuration correct if KPR is later turned off. |
| `operator.replicas` | `2` | Removal of the `node.cilium.io/agent-not-ready` taint is the operator's exclusive job, and one replica has no redundancy. |

**⚠️ Masquerading must be off entirely.** Pod IPs are routable inside the VPC, so masquerading
rewrites the source to the node's address. More importantly, an agent that finds **iptables-based**
masquerading enabled resets `bpf.hostLegacyRouting` to `true`, which disables eBPF host-routing.
This mode's performance rests on eBPF host-routing, not on the netkit device itself.

**Do not set `enable-node-port` on its own.** With `kubeProxyReplacement=true`, the agent derives it,
along with `enable-external-ips`, `bpf-lb-sock`, `enable-host-port`, and `enable-session-affinity`.
The ConfigMap is input, not the effective configuration. For any KPR-related setting, read
`cilium-dbg config -a`.

Service and policy semantics are identical to the default shape of chained mode, including
`externalTrafficPolicy: Local` having no effect on in-cluster traffic.

### 4.4 Choosing between netkit L3 and L2

`bpf.datapathMode` selects the mode in which the Pod device is created:

| | **L3 (`netkit`)** | **L2 (`netkit-l2`)** |
|---|---|---|
| MAC address on the Pod interface | none, all zeroes | present |
| ARP handling | not needed | needed |
| Performance | upstream's recommendation | the difference from L3 is not resolvable in TCP_RR ([performance.md](performance.md) §6.1) |
| Use it when | everything else | a workload needs Ethernet-layer semantics inside the Pod |
| Verified here | ✅ | ✅ equivalent to L3 |

**The concrete case for L2 is a workload that reads the interface's MAC.** KubeVirt passes the MAC
to libvirt when defining a VM. Under L3, that address is empty, and virt-handler fails with
`XML error: unable to parse mac address ''`. Any other workload that reads the MAC, or that does ARP
or DHCP inside the Pod, hits the same limitation.

Everything else in this document is the same for both modes. The only change is one value:

```bash
  --set bpf.datapathMode=netkit-l2
```

- **Switching between L3 and L2 is cluster-wide and requires the Pods to be recreated**, in the same
  way as the switch from veth (§1). The device mode is fixed when a Pod is created.
- **The two modes are equivalent across everything this plugin owns**: per-ENI rules and routes,
  address allocation and reclaim, egress across multiple ENIs, the first Pod on a freshly attached
  ENI, NetworkPolicy enforcement, and every form of Service. Their behavior differs only in
  neighbor resolution. Under L3, the device carries `NOARP` and both neighbor tables stay empty.
  Under L2, host-to-Pod traffic uses ARP/NDP with dynamic `REACHABLE` entries, and the Pod's
  requests for its gateway are answered in place by Cilium's BPF program.
- On **Cilium >= 1.20**, there is also `auto`, which probes the kernel and **silently falls back to
  veth** when netkit is unavailable. Do not use it here: in this mode, a silent fallback would leave
  the cluster on a datapath that the configuration does not describe.

### 4.5 A `hostPort` must not fall inside the NodePort range

With `kubeProxyReplacement=true`, Cilium serves hostPort natively from its own BPF load balancer, so
the conflist chains no `portmap` plugin in this mode. However, the agent **refuses any hostPort that
falls inside the configured NodePort range** (30000-32767 by default) and logs:

```
The requested hostPort 31883 is colliding with the configured NodePort range [30000, 32767]. Ignoring.
```

**Nothing else reports it.** The Pod starts normally, `cilium-dbg status --verbose` still prints
`HostPort: Enabled`, and only `cilium-dbg service list` shows that no HostPort entry was created.
Nothing else implements hostPort in this mode, so a rejected port is not answered from any direction.

Measured with a port outside the range (18081), a hostPort works from **every** direction: the Pod's
own node, a Pod on another node, another node's host namespace, a non-cluster VM inside the VPC, and
outside the cluster. `cilium-dbg service list` then shows four HostPort entries: IPv4 and IPv6, each
for the node address and the wildcard.

**Therefore, choose a hostPort below 30000**, and verify that it took effect:

```bash
kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg service list | grep HostPort
```

An empty result means that the port was rejected, regardless of what `status --verbose` reports.

### 4.6 IPv4 single-stack and dual-stack

The IP stack is selected by parameters. The only difference between the two configurations is the
following keys:

| Cluster IP stack | Cilium values to add |
|---|---|
| **Single-stack IPv4** | none (the quick start above is the single-stack configuration) |
| **Dual-stack** | `--set ipv6.enabled=true`<br>`--set extraConfig.local-router-ipv6=fe80::ffff:1` |

- The cluster itself must be dual-stack (Node objects carrying IPv6); otherwise, adding these keys
  serves no purpose.
- `local-router-ipv6` must avoid `fe80::1`, which is this plugin's IPv6 gateway for Pods.
- On a dual-stack cluster, **omitting** these keys causes Pod IPv6 to be dropped silently, with no
  error log at all.
- **A node joining a dual-stack cluster needs its IPv6 before Cilium can start on it.** See
  [Adding a node to a dual-stack cluster](#adding-a-node-to-a-dual-stack-cluster).

#### Adding a node to a dual-stack cluster

Cilium on a node takes the node's IPv6 address from its Node object, and the Node object takes it
from kubelet's `--node-ip`, which only node provisioning sets. There are two ways to achieve this;
the first is recommended.

**A. IPv6 before joining (recommended).** Enable IPv6 on the instance's primary interface, ideally
when the instance is created, then run the following on the new node. Its reconfigure step runs only
when the interface has no global IPv6 yet, that is, when IPv6 was enabled after boot and the
boot-time DHCPv6 request is over:

```sh
IFACE=eth0   # the interface that carries the node IP
# Only if it has no global IPv6 yet (IPv6 enabled after boot, or flow B). systemd-networkd shown;
# with another network manager, renew the interface's DHCPv6 lease instead.
ip -6 -o addr show "$IFACE" scope global | grep -q . || { networkctl reconfigure "$IFACE"; sleep 10; }
V4=$(ip -4 -o addr show "$IFACE" scope global | awk '{print $4}' | cut -d/ -f1 | head -1)
V6=$(ip -6 -o addr show "$IFACE" scope global | awk '{print $4}' | cut -d/ -f1 | head -1)
F=/etc/default/kubelet; [ -e /etc/sysconfig/kubelet ] && F=/etc/sysconfig/kubelet
if [ -n "$V4" ] && [ -n "$V6" ]; then
  [ -e "$F" ] && cp -n "$F" "$F.bak"
  grep -qs '^KUBELET_EXTRA_ARGS=' "$F" || echo 'KUBELET_EXTRA_ARGS=' >> "$F"
  sed -i -E -e "/^KUBELET_EXTRA_ARGS=/ s/[[:space:]]*--node-ip[= ][^[:space:]\"']*//g" \
            -e "/^KUBELET_EXTRA_ARGS=/ s/^(KUBELET_EXTRA_ARGS=[\"']?)/\1--node-ip=$V4,$V6 /" "$F"
  grep '^KUBELET_EXTRA_ARGS=' "$F"
else
  echo "no IPv4 or IPv6 on $IFACE yet (v4=$V4 v6=$V6); stop here"
fi
kubeadm join ...   # only if the block printed the KUBELET_EXTRA_ARGS line;
                   # as printed by: kubeadm token create --print-join-command
```

Expected: the Node carries both addresses from registration, Cilium starts on the first attempt, the
node's ZecNode reaches `InstanceVerified=True` (`zec.ipamType=crd` only) and the node turns `Ready`.

**B. Let the controlplane enable IPv6.** Use this only when all of the following hold; otherwise,
use A:

- `zec.ipamType=crd` and `zec.ipStack=dual`.
- The node's primary interface is in one of `zec.subnetIDs`. A primary interface outside them serves
  no Pod and gets no IPv6 from the plugin.
- The release notes printed an `APISERVER:` line (view them again with
  `helm get notes zenlayer-vpc-cni -n kube-system`). This chart refuses to render without the entry
  point. No such line means the release is still on 1.0.0, whose daemon uses the in-cluster
  Service; nothing serves that Service on the node until Cilium is up, so that daemon never
  registers. Upgrade it first ([deploy-direct.md](./deploy-direct.md) §3.4).

With a platform-run controlplane, the `zec.ipStack` and pod-subnet conditions above refer to the
platform side's configuration rather than this chart's values.

Join without IPv6. The node's daemon registers, and the controlplane verifies the node and assigns
its primary interface an IPv6 prefix itself (measured: within about 20 seconds of the daemon
starting). Until the host has taken that address, Cilium on the node keeps restarting with
`node <name> does not have an IPv6 address`, and the node stays `NotReady`; this is expected. It is
also expected that this plugin's daemon restarts once it has waited longer than `cilium.waitTimeout`
(default 5m) for the Cilium agent; raise that value if the manual steps below take longer.

```sh
# On a control-plane host: wait for the prefix
kubectl get zn <node> -o jsonpath='{.status.enis[?(@.primary==true)].ipv6.primary}{"\n"}'

# On the new node, once it is there (`networkctl renew` did not take the new address when measured)
IFACE=eth0   # the interface that carries the node IP
# Only if it has no global IPv6 yet (IPv6 enabled after boot, or flow B). systemd-networkd shown;
# with another network manager, renew the interface's DHCPv6 lease instead.
ip -6 -o addr show "$IFACE" scope global | grep -q . || { networkctl reconfigure "$IFACE"; sleep 10; }
V4=$(ip -4 -o addr show "$IFACE" scope global | awk '{print $4}' | cut -d/ -f1 | head -1)
V6=$(ip -6 -o addr show "$IFACE" scope global | awk '{print $4}' | cut -d/ -f1 | head -1)
F=/etc/default/kubelet; [ -e /etc/sysconfig/kubelet ] && F=/etc/sysconfig/kubelet
if [ -n "$V4" ] && [ -n "$V6" ]; then
  [ -e "$F" ] && cp -n "$F" "$F.bak"
  grep -qs '^KUBELET_EXTRA_ARGS=' "$F" || echo 'KUBELET_EXTRA_ARGS=' >> "$F"
  sed -i -E -e "/^KUBELET_EXTRA_ARGS=/ s/[[:space:]]*--node-ip[= ][^[:space:]\"']*//g" \
            -e "/^KUBELET_EXTRA_ARGS=/ s/^(KUBELET_EXTRA_ARGS=[\"']?)/\1--node-ip=$V4,$V6 /" "$F"
  grep '^KUBELET_EXTRA_ARGS=' "$F"
else
  echo "no IPv4 or IPv6 on $IFACE yet (v4=$V4 v6=$V6); stop here"
fi
systemctl restart kubelet       # only if the block printed the KUBELET_EXTRA_ARGS line

# On a control-plane host: restart Cilium on that node rather than wait out its back-off
kubectl -n kube-system delete pod -l k8s-app=cilium --field-selector spec.nodeName=<node>
```

For the direct apiserver entry point, see the `zec.k8sServiceHost` row in [§4.1](#41-this-projects-values).
`eth0` above is the interface that carries the node IP. This plugin configures neither the host
interface nor kubelet; both belong to node provisioning.

## 5. Rolling back to veth mode

```bash
# ① Switch this project back to chained first
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.1 \
  -n kube-system --reset-then-reuse-values --set cni.mode=chained
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

# ④ Clear the endpoints the netkit devices left behind -- the reverse of step 6 in §2.
#   First wait for every workload restarted in ③
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout status "$t/$n" --timeout=10m; done
done
while kubectl get pod -A --no-headers | grep -q Terminating; do sleep 5; done   # old Pods gone too
# Then list only the endpoints whose netkit lxc<hex> device no longer exists (lxc_health is excluded)
kubectl -n kube-system get pod -l k8s-app=cilium -o name | while read -r p; do
  echo "== ${p#pod/}"
  kubectl -n kube-system exec "${p#pod/}" -c cilium-agent -- sh -c '
    cilium-dbg endpoint list -o jsonpath="{range [*]}{.id} {.status.networking.interface-name}{\"\n\"}{end}" |
    while read -r id ifn; do
      case "$ifn" in lxc[0-9a-f]*) ip link show "$ifn" >/dev/null 2>&1 || echo "$id $ifn";; esac
    done'
done
# Remove each listed id on the node it was listed under:
#   kubectl -n kube-system exec <cilium-pod> -c cilium-agent -- cilium-dbg endpoint disconnect <id>
```

Step ④ cannot be skipped, for the reason given in the warning under §2: a netkit endpoint can
outlive its Pod. The same caution applies in reverse: an `lxc*` endpoint whose device still exists
belongs to a live Pod (a Job, bare Pod, or static Pod that ③ does not restart). Recreate that Pod
rather than delete its endpoint.

**This rollback keeps `kubeProxyReplacement=true` and kube-proxy removed.** That is also chained
mode's default shape (Services stay on Cilium's eBPF, and `k8sServiceHost` stays set as that shape
requires), so ② leaves those values as they are. Bringing kube-proxy back is part of leaving Cilium
altogether: see step ⓪ of the chained rollback (`kube-proxy-backup.yaml` from step 3 restores the
DaemonSet if it has since been deleted).

This rollback returns the cluster to **chained mode on veth**. To continue back to plain veth
without Cilium, and to clean up what an uninstall leaves behind (socket LB programs on the cgroup,
orphaned CiliumEndpoints), see the rollback section of
[deploy-cilium-chained.md](deploy-cilium-chained.md).
