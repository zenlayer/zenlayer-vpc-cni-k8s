# Deploying chained mode (veth datapath + Cilium CNI chaining)

> This document is the authoritative version. A Chinese translation is available at
> [deploy-cilium-chained-cn.md](./deploy-cilium-chained-cn.md).

This document describes how to enable NetworkPolicy on top of zenlayer-vpc-cni by chaining
**upstream community Cilium** after this plugin. This is `cni.mode=chained`, the recommended mode for
deploying this project together with Cilium.

**This project does not fork Cilium and does not distribute Cilium images.** You install Cilium
from its own official chart and choose the version. This project performs only two tasks: it
appends a `cilium-cni` plugin section to the conflist, and it withholds the conflist until the
Cilium agent is ready.

Enabling Cilium is **optional**. When it is not enabled, the behavior of this plugin is unrelated
to this document: Services are handled by kube-proxy, and the datapath is entirely unchanged.

> The other mode is `cni.mode=delegated`, in which Cilium creates the Pod device (netkit or veth)
> and this plugin only supplies addresses. See
> [deploy-cilium-netkit.md](./deploy-cilium-netkit.md). For a comparison of the two modes, see
> [performance.md](./performance.md): this mode already reaches the performance level on offer.

## Contents

- [1. Prerequisites](#1-prerequisites)
- [2. Quick start: switching from veth to this mode](#2-quick-start-switching-from-veth-to-this-mode)
- [3. Capability matrix](#3-capability-matrix)
- [4. Configuration reference](#4-configuration-reference)
- [5. Rolling back to veth mode](#5-rolling-back-to-veth-mode)

## 1. Prerequisites

| Item | Requirement | Notes |
|---|---|---|
| **Cilium version** | **v1.15 to v1.18** | v1.17.5 has been checked against upstream source and verified on a cluster. The lower bound is only the upstream maintenance window; the upper bound only states how far the check went. `k8sServiceHost=auto` ([§4.2](#42-cilium-values)) requires v1.16 or later; on v1.15, set the host and port explicitly. Before moving to another version, run `helm show values cilium/cilium --version <ver>` and re-check every key used in this document, because the chart's values structure changes between major versions |
| Kernel | >= 5.4 (>= 4.18 on RHEL 8 derivatives) | Cilium's own minimum |
| cgroup v2 | mounted | `stat -fc %T /sys/fs/cgroup` prints `cgroup2fs`. The socket-level load balancer attaches to the cgroup, so cgroup v2 is a hard prerequisite of `kubeProxyReplacement` |
| bpffs | mounted at `/sys/fs/bpf` | **The Cilium agent mounts it itself**; this project does not manage it |
| `helm` | available on the machine that runs the commands, >= 3.14 | It does **not** need to be installed on a cluster node; any machine that can reach the API server is sufficient. 3.14 is the first version with `--reset-then-reuse-values` |
| This plugin already running | yes | Get this plugin working first, then enable chaining |
| kube-proxy | removed once deployment completes | Services move to Cilium's eBPF. **Do not delete it outright**: it is the fallback during deployment, and rollback requires it to be restored |
| Cluster IP stack | single-stack IPv4, or dual-stack per [§4.3](#43-ipv4-single-stack-and-dual-stack) | On a dual-stack cluster, omitting those two keys leaves Pod IPv4 working while every Pod IPv6 packet is dropped **with no log line at all** |

Components added to the cluster: one DaemonSet (cilium-agent), one Deployment (cilium-operator),
and Cilium's own set of CRDs. Upgrading Cilium requires no change to this project's configuration.

> **cilium-operator must be deployed, but it is not on the critical path for policy enforcement.**
> This design uses `identityAllocationMode=crd` (Cilium's default). In this mode, the agent
> creates and maintains CiliumIdentity and CiliumEndpoint objects directly, and the operator only
> performs identity garbage collection. Without the operator, new Pods still receive identities
> and policy still takes effect; the only degradation is orphaned CiliumIdentity objects.
>
> The operator should still be kept healthy. **Its toleration is `[{"operator":"Exists"}]`, so it
> tolerates every taint**: on a cluster with NotReady or cordoned nodes, it can be scheduled onto a
> node that never comes up and remain `Pending`. The operator is also exclusively responsible for
> removing the `node.cilium.io/agent-not-ready` taint: without the operator, a node accepts no new
> Pods even after its agent is Ready. After installing, confirm that the operator was scheduled
> onto a healthy node:
>
> ```bash
> kubectl -n kube-system get pod -l io.cilium/app=operator -o wide
> ```

## 2. Quick start: switching from veth to this mode

This section assumes that this plugin is running, cgroup v2 is mounted, and the cluster is
single-stack IPv4 (for dual-stack, see [§4.3](#43-ipv4-single-stack-and-dual-stack)).
[§4](#4-configuration-reference) explains why each parameter has its value.

```bash
# 0. Check the prerequisites
stat -fc %T /sys/fs/cgroup                      # must be cgroup2fs
kubectl -n kube-system get ds zenlayer-vpc-cni  # must be ready
helm version --short
# Is the cluster dual-stack? A non-zero count means you must follow §4.3 instead,
# or Pod IPv6 is dropped silently with no log at all
kubectl get nodes -o jsonpath='{.items[*].status.addresses[*].address}' | tr ' ' '\n' | grep -c ':'

# The apiserver entry point -- step 1's k8sServiceHost=auto reads exactly this line. It must print
# something with a port, and should be a load balancer / VIP; a single master's IP becomes a single
# point of failure. No output (not a kubeadm cluster), a bracketed IPv6 address, or Cilium v1.15: see §4.2
kubectl -n kube-public get cm cluster-info -o jsonpath='{.data.kubeconfig}' | grep server:

# 1. Install Cilium. This --set list is the required set; missing one of them causes problems
#    (each is explained in §4.2)
helm repo add --force-update cilium https://helm.cilium.io/ && helm repo update cilium
helm install cilium cilium/cilium --version 1.17.5 --namespace kube-system \
  --set cni.chainingMode=generic-veth --set cni.customConf=true --set cni.exclusive=false \
  --set cni.install=true --set routingMode=native \
  --set enableIPv4Masquerade=false --set enableIPv6Masquerade=false --set bpf.masquerade=false \
  --set ipam.mode=delegated-plugin --set extraConfig.local-router-ipv4=169.254.32.1 \
  --set-string extraConfig.enable-session-affinity=true \
  --set endpointHealthChecking.enabled=false --set envoyConfig.enabled=false \
  --set endpointRoutes.enabled=true \
  --set kubeProxyReplacement=true --set bpf.hostLegacyRouting=false \
  --set k8sServiceHost=auto \
  --set operator.replicas=2
kubectl -n kube-system rollout status ds/cilium --timeout=10m
# Confirm auto resolved to a real address -- a literal auto, or nothing, means the agent cannot reach the apiserver
kubectl -n kube-system get ds cilium -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="KUBERNETES_SERVICE_HOST")].value}{"\n"}'

# 2. Turn on this project's chaining switches. The daemon waits for the Cilium agent to be ready
#    before writing the new conflist. --reset-then-reuse-values, not --reuse-values: the latter
#    keeps the previous chart version's defaults
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.0 \
  -n kube-system --reset-then-reuse-values \
  --set cilium.enabled=true --set networkPolicy.enabled=true
kubectl -n kube-system rollout restart daemonset/zenlayer-vpc-cni
kubectl -n kube-system rollout status daemonset/zenlayer-vpc-cni --timeout=10m
# The release notes show the entry point the daemon uses (k8sServiceHost row in §4.2)
helm get notes zenlayer-vpc-cni -n kube-system | grep -E '^(APISERVER|WARNING):'

# 3. Confirm on every node that the conflist now carries the cilium-cni section -- do not check
#    just one. Expect [zec-cni, cilium-cni] -- portmap is only chained when Cilium is off
kubectl -n kube-system get pod -l app=zenlayer-vpc-cni -o name | while read -r p; do
  printf '%s: ' "${p#pod/}"
  kubectl -n kube-system exec "${p#pod/}" -c zec-ipam -- \
    grep -o '"type": "[a-z-]*"' /host-etc-net.d/10-zenlayer-cni.conflist | tr '\n' ' '; echo
done

# 4. Confirm every node has taken over Services, and only then remove kube-proxy
kubectl -n kube-system get pod -l k8s-app=cilium -o name | while read -r p; do
  kubectl -n kube-system exec "${p#pod/}" -c cilium-agent -- \
    cilium-dbg status | grep -E '^KubeProxyReplacement:|^Routing:'
done                                            # every node must show True + Host: BPF
[ -e kube-proxy-backup.yaml ] || kubectl -n kube-system get ds kube-proxy -o yaml > kube-proxy-backup.yaml   # keep for rollback; never overwrite an earlier one
kubectl -n kube-system patch ds kube-proxy -p \
  '{"spec":{"template":{"spec":{"nodeSelector":{"kube-proxy-disabled":"true"}}}}}'

# 5. On every node, clear the rules kube-proxy left behind
#    (this also removes kubelet's KUBE-FIREWALL / KUBE-KUBELET-CANARY, which kubelet recreates)
iptables-save  | grep -v KUBE | iptables-restore
ip6tables-save | grep -v KUBE | ip6tables-restore

# 6. Roll every workload that needs policy protection -- this step cannot be skipped
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout restart "$t/$n"; done
done

# 7. Verify coverage: every non-hostNetwork Pod should have a CiliumEndpoint. Compare the
#    difference rather than running `get cep` -- orphaned CEPs from an earlier round make the
#    list look complete. Expect no output
comm -23 \
  <(kubectl get pod -A --field-selector=status.phase=Running \
      -o go-template='{{range .items}}{{if not .spec.hostNetwork}}{{.metadata.namespace}}/{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | sort) \
  <(kubectl get cep -A \
      -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' | sort)
```

**The order in step 4 cannot be reversed**: confirm the takeover first, then stop kube-proxy. The
reverse order leaves a window in which nothing handles Services, and components running with
`hostNetwork` (including this project's daemon) crash because they cannot reach the API server.

**Step 6 cannot be skipped.** Cilium endpoints are created during CNI ADD, so **Pods that were
already running before chaining was enabled have no CEP at all**, and policy does not constrain
them. Until the restart completes, do not assume that policy is in effect. (After a rollback and a
reinstall, a Pod that was never restarted can get its CEP back from the old agent state under
`/var/run/cilium/state`, as described at the end of §5. Therefore, the presence of a CEP does not
prove that the Pod was restarted either.)

## 3. Capability matrix

| Capability | Status |
|---|---|
| L3/L4 NetworkPolicy, CiliumNetworkPolicy | ✅ |
| L7 policy (HTTP), FQDN policy (`toFQDNs`) | ✅ available by default, see `l7Proxy` in [§4.2](#42-cilium-values) |
| Hubble flow observability | ✅ enabled separately on the Cilium side |
| Services carried by Cilium's eBPF (kube-proxy removed) | ✅ on by default |
| eBPF host-routing | ✅ on by default, +16% to +31% on Pod traffic ([performance.md](./performance.md)) |
| Pod **egress** bandwidth limiting | ⚠️ must be enabled and verified separately; there is a gap on secondary ENIs |
| Pod **ingress** bandwidth limiting | ❌ explicitly unsupported by Cilium |
| IPsec / WireGuard transparent encryption | ❌ addresses are not allocated by Cilium, so no encryption peer can be formed |
| Ingress / Gateway API | ❌ depends on Envoy, which is disabled |
| `externalTrafficPolicy: Local` (in-cluster traffic) | ❌ no effect; inherent to `kubeProxyReplacement` |

**The cost**: chaining attaches BPF programs to every Pod veth and to the node's eth0. Any overhead
this places on hostNetwork traffic is not recovered by host-routing. One measurement put that
overhead at about 8%, but it was a single run, fell within run-to-run drift, and was not reproduced
in a same-session comparison ([performance.md](./performance.md)). On the Pod side, eBPF
host-routing more than offsets the overhead, provided that `kubeProxyReplacement` is left on.

## 4. Configuration reference

### 4.1 This project's two switches

Both switches are in this project's `chart/values.yaml`. **They are deliberately kept separate**:
Hubble observability without policy is a real use case, and merging the switches would make that
combination impossible to express.

| Setting | Value here | Notes |
| --- | --- | --- |
| `cilium.enabled` | `true` | **The switch that actually changes the datapath**: appends the `cilium-cni` section to the conflist, mounts the agent socket into the daemonset, and makes the daemon wait for the agent before writing the conflist. |
| `networkPolicy.enabled` | `true` | Declares only that "policy must be enforced by someone"; it brings no engine of its own. **Requires `cilium.enabled` to be true as well**, or the chart refuses to render. If that combination were allowed, traffic that should be blocked would flow instead, and nobody would notice. |
| `cni.mode` | `chained` | The mode this document covers. For `delegated`, see the other document. |

> **Note:** A misspelled switch fails at render time (the root of `values.schema.json` is
> `additionalProperties: false`) with the error `additional properties 'celium' not allowed`.

### 4.2 Cilium values

The following table lists the complete set of Cilium values that this design requires; **omitting
any one of them causes problems**. Items marked ⚠️ are explained below the table.

| Setting | Value here | Notes |
| --- | --- | --- |
| `cni.chainingMode` | `generic-veth` | Set it explicitly. If it is left unset, cilium-cni falls back to looking up a chainer named after the conflist's `name`; renaming the network then silently stops chaining. |
| `cni.customConf` | `true` | A **hard prerequisite** of `generic-veth`: Cilium has no conflist template for that mode, `false` contradicts it, and the agent hangs at `0/1 Running`. |
| `cni.exclusive` | `false` | Defense in depth. With `customConf=true`, this key never reaches the configmap, so it is effectively inert. |
| `cni.install` | `true` | This project needs the `/opt/cni/bin/cilium-cni` binary, which Cilium's init container installs. |
| `routingMode` | `native` | Pod IPs are ENI secondary addresses, natively routable in the VPC; no tunnel is needed. |
| `ipam.mode` | `delegated-plugin` | Tells Cilium that addresses come from the CNI binary and that it must not participate. |
| `extraConfig.local-router-ipv4` | `169.254.32.1` | ⚠️ **Not a helm value**, see below. The value only has to avoid `169.254.1.1`, the default gateway this plugin writes into the container. |
| `endpointHealthChecking.enabled` | `false` | A mandatory companion of `delegated-plugin`. |
| `envoyConfig.enabled` | `false` | Same. The cost is giving up Ingress / Gateway API. |
| `endpointRoutes.enabled` | `true` | Same. Leaving it unset causes the agent to CrashLoop; the setting is outside what `checkIPAMDelegatedPlugin()` validates. |
| `kubeProxyReplacement` | `true` | ⚠️ Enabled **as a pair** with the next row, see below. It also decides hostPort: with Cilium on, `portmap` is not chained, and Cilium serves hostPort only while this is `true` (or `enable-host-port` is set through `extraConfig`). |
| `bpf.hostLegacyRouting` | `false` | ⚠️ Same. **Setting it alone does nothing**: whenever `kubeProxyReplacement` is not `true` the agent resets it to `true`, so the setting only creates the impression that something was enabled. |
| `k8sServiceHost` | `auto` | ⚠️ **Required.** With kube-proxy removed, only Cilium implements ClusterIPs. An agent that still reaches the apiserver through the ClusterIP cannot start after a full node reboot, so nothing implements the ClusterIP either: the agent and this plugin's daemon both CrashLoop with `dial tcp <ClusterIP>:443: connect: network is unreachable`. `auto` makes helm read the apiserver entry point (kubeadm's `controlPlaneEndpoint`), including the port, from `kube-public/cluster-info` at install time. Three caveats apply. **(1) That entry point should be a load balancer / VIP**; a single master's IP makes it a single point of failure. **(2) It requires Cilium v1.16 or later, and the failure mode for a missing ConfigMap differs by version**: v1.16.0 to v1.16.2 fail to render; **from v1.16.3 to v1.18, helm does not fail** and instead writes the literal `auto` as the address; from v1.19, rendering fails again (`ConfigMap kube-public/cluster-info not found`); v1.15 has no lookup at all and always writes `auto`. On v1.15 or a non-kubeadm cluster, set `--set k8sServiceHost=<entry-address> --set k8sServicePort=<port>` instead. **An IPv6-literal entry point (`https://[fd00::10]:6443`) must also be set explicitly** (`--set k8sServiceHost=fd00::10 --set k8sServicePort=6443`): Cilium's lookup takes everything before the first `:` as the host and yields `[fd00`. This chart's own lookup for `zec.k8sServiceHost` handles such an entry point. **(3) It is resolved once, at install / upgrade.** `helm template` and any other offline render cannot access the cluster and therefore behave as described in (2); a changed entry point requires another `helm upgrade`. **This plugin's daemon and controlplane need the same entry point, for the same reason**: with `cilium.enabled=true` and `zec.k8sServiceHost` left empty, this chart reads it from the same ConfigMap at install / upgrade and prints the result in the release notes; where the ConfigMap is missing, set `zec.k8sServiceHost` / `zec.k8sServicePort` to the entry point. |
| `enableIPv4Masquerade` | `false` | ⚠️ Masquerading must be off entirely, see below. |
| `enableIPv6Masquerade` | `false` | Same. |
| `bpf.masquerade` | `false` | Same. |
| `extraConfig.enable-session-affinity` | `true` | Cilium enables it automatically when KPR is on; setting it explicitly keeps the configuration correct if KPR is later turned off. If this setting is absent and KPR is off, `sessionAffinity: ClientIP` is silently ignored. |
| `l7Proxy` | leave unset | Keeps Cilium's default of `true`. **Turning it off makes already-applied L7 and FQDN policies fail open rather than fail loudly**: the L7 part of a rule is ignored, and the rule degrades to allowing the L4 traffic, with no warning. |
| `operator.replicas` | `2` | Same as the community chart's default. **Do not drop to 1**; see the note in §1. |
| `ipv6.enabled` | `true` for dual-stack only | See [§4.3](#43-ipv4-single-stack-and-dual-stack). |
| `extraConfig.local-router-ipv6` | dual-stack only | Same, and the value must avoid `fe80::1`. |

**⚠️ Masquerading must be off entirely.** Pod IPs are routable inside the VPC, so masquerading
rewrites the Pod's source address to the node's address, and the VPC no longer sees the Pod as the
origin. More importantly, an agent that finds **iptables-based** masquerading enabled falls back to
the host stack, which nullifies `bpf.hostLegacyRouting=false`.

**⚠️ `local-router-ipv4` and `local-router-ipv6` can only be set through `extraConfig`.** They are
not helm values: `--set localRouterIPv4=` is **silently discarded**, and the agent then fails to
start because the setting is missing.

**Do not set `enable-node-port` on its own.** With `kubeProxyReplacement=true`, the agent derives
it, along with `enable-external-ips`, `bpf-lb-sock`, `enable-host-port`, and
`enable-session-affinity`. The ConfigMap is the input, not the effective configuration. For any
KPR-related setting, read `cilium-dbg config -a`.

**A `hostPort` must stay below 30000.** With `kubeProxyReplacement=true`, Cilium serves hostPort
from its own BPF load balancer and refuses any port inside the NodePort range (30000-32767). It
logs `The requested hostPort ... is colliding with the configured NodePort range` and reports
nothing else: the Pod starts, `cilium-dbg status --verbose` still reports `HostPort: Enabled`, and
only `cilium-dbg service list | grep HostPort` shows that no entry was created.

To determine whether eBPF host-routing is actually enabled, use `cilium-dbg status`; that field is
stable across versions:

```bash
kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg status | grep '^Routing:'
# Routing:  Network: Native   Host: BPF      <- enabled (the default here)
# Routing:  Network: Native   Host: Legacy   <- going through the host stack
```

> **Note:** Do not use `grep ENABLE_HOST_ROUTING /run/cilium/state/globals/node_config.h` as the
> test. In newer versions, that macro became a runtime option, so the grep result is always 0 and
> reads as "not enabled".

### 4.3 IPv4 single-stack and dual-stack

First decide **whether Pods need IPv6**. This decision determines the requirements on the
Kubernetes cluster itself, and changing it after installation is expensive.

| | Single-stack (Pod IPv4 only) | Dual-stack (Pod IPv4 + IPv6) |
|---|---|---|
| Requirement on the k8s cluster | none | ⚠️ **the cluster itself must be dual-stack** (Node objects carrying IPv6) |
| `ipv6.enabled` | `false` (default, no need to write it) | **`true`** |
| `extraConfig.local-router-ipv6` | not needed | **required**, and must avoid `fe80::1` |
| This project's `zec.ipStack` | `ipv4` | **`dual`** |

The single-stack install command is step 1 of the
[quick start](#2-quick-start-switching-from-veth-to-this-mode). Dual-stack adds the following:

```bash
  --set ipv6.enabled=true \
  --set-string extraConfig.local-router-ipv6=fe80::ffff:1
```

In addition, every node's Node object must carry an IPv6 address **first**. That address comes
from kubelet's `--node-ip`. Run the block below on every node. If a node runs Pods and its
interface has no global IPv6 address yet, **drain the node first**: `networkctl reconfigure` can
make systemd-networkd remove routes and ip rules that it did not configure itself
(`ManageForeignRoutes` / `ManageForeignRoutingPolicyRules` default to yes), and this plugin's
per-ENI route tables and per-Pod rules are such routes and rules. Pods started after the
reconfiguration get their routes and rules again.

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
systemctl restart kubelet       # only if the block printed the KUBELET_EXTRA_ARGS line
kubectl get node <name> -o jsonpath='{.status.addresses}'
```

Measured result: after the kubelet restart, the node is `Ready` again in about 10 seconds, and
running Pods are unaffected.

- **The IPv6 address must come from the interface that carries the node IP.** A node with several
  ENIs has several global IPv6 addresses. Picking the wrong one raises no immediate error, but the
  Node object then advertises an address that belongs to a different interface, and the control
  plane and datapath no longer agree.
- **Only the Node objects need to be dual-stack, not Services.** The apiserver's
  `--service-cluster-ip-range` can stay single-stack: it only determines whether dual-stack
  Services can be created, which is unrelated to Pod IPv6 connectivity.
- **Only the second failure direction is silent.** Enabling `ipv6.enabled` on a single-stack
  cluster makes the agent exit fatally and log that Node objects have no IPv6. Leaving it off on a
  dual-stack cluster gives Pods IPv6 addresses and correct configuration, but the tcx program drops
  every packet as `Unsupported L3 protocol`, **with no log line at all**.
- **A node joining a dual-stack cluster needs its IPv6 before Cilium can start on it.** See
  [Adding a node to a dual-stack cluster](#adding-a-node-to-a-dual-stack-cluster).

#### Adding a node to a dual-stack cluster

Cilium on a node takes the node's IPv6 from its Node object, and the Node object takes it from
kubelet's `--node-ip`, which only node provisioning sets. There are two ways to achieve this; the
first is recommended.

**A. IPv6 before joining (recommended).** Enable IPv6 on the instance's primary interface, ideally
when the instance is created, and then run the following on the new node. The reconfigure step runs
only when the interface has no global IPv6 address yet, i.e., when IPv6 was enabled after boot and
the boot-time DHCPv6 request is over:

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

Expected result: the Node carries both addresses from registration, Cilium starts on the first
attempt, the node's ZecNode reaches `InstanceVerified=True` (crd mode), and the node becomes
`Ready`.

**B. Let the controlplane enable IPv6.** Use this method only when all of the following conditions
hold; otherwise, use A:

- `zec.ipamType=crd` and `zec.ipStack=dual`;
- the node's primary interface is in one of `zec.subnetIDs` (a primary interface outside them
  serves no Pod and gets no IPv6 from the plugin);
- the release notes printed an `APISERVER:` line (read them again with
  `helm get notes zenlayer-vpc-cni -n kube-system`). This chart refuses to render without the entry
  point. A `WARNING:` line there instead comes from an earlier chart: its daemon falls back to the
  in-cluster Service, which nothing serves on that node until Cilium is up, and therefore never
  registers.

When the controlplane is run by the platform, the `zec.ipStack` and pod-subnet conditions above
refer to the platform side's configuration rather than this chart's values.

Join the node without IPv6. The node's daemon registers, and the controlplane verifies the node
and assigns an IPv6 prefix to its primary interface itself; in measurements, this happened within
about 20 seconds of the daemon starting. Until the host has taken that address, Cilium on the node
keeps restarting with `node <name> does not have an IPv6 address`, and the node stays `NotReady`;
this is expected. It is also expected that this plugin's daemon restarts once it has waited longer
than `cilium.waitTimeout` (default 5m) for the Cilium agent. If the manual steps below take longer,
raise that value.

```sh
# On a control-plane host: wait for the interface's own IPv6 -- the address the host then takes
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

For the direct apiserver entry point, see the `k8sServiceHost` row in [§4.2](#42-cilium-values).
In the commands above, `eth0` is the interface that carries the node IP. This plugin configures
neither the host interface nor kubelet; both belong to node provisioning.

## 5. Rolling back to veth mode

**The order is fixed. Steps ⓪, ②, and ③ cannot be skipped, and none of them may be swapped with
step ④.**

```bash
# ⓪ Put kube-proxy back first (step 4 of the quick start stopped it)
kubectl -n kube-system patch ds kube-proxy --type json \
  -p '[{"op":"remove","path":"/spec/template/spec/nodeSelector/kube-proxy-disabled"}]'
# If the DaemonSet has since been deleted (the patch says NotFound), recreate it from the backup
# instead, dropping the fields a raw dump carries that create refuses:
#   sed -E '/^  (resourceVersion|uid|creationTimestamp|generation):/d; /^status:/,$d' \
#     kube-proxy-backup.yaml | kubectl create -f -
kubectl -n kube-system rollout status ds/kube-proxy --timeout=5m
# Confirm it really took over (run on a node)
iptables -t nat -S | grep -c '^-N KUBE-SVC'                                # expect > 0
# A 200 from https://10.96.0.1:443/healthz proves nothing here: Cilium still serves the ClusterIP.
# The KUBE-SVC count above is the evidence.

# ① Turn both switches off so the conflist drops the cilium-cni section
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.0 \
  -n kube-system --reset-then-reuse-values \
  --set cilium.enabled=false --set networkPolicy.enabled=false
kubectl -n kube-system rollout restart daemonset/zenlayer-vpc-cni
kubectl -n kube-system rollout status daemonset/zenlayer-vpc-cni --timeout=600s

# ② Confirm on EVERY node that the conflist no longer contains cilium-cni
kubectl -n kube-system get pod -l app=zenlayer-vpc-cni -o name | while read -r p; do
  printf '%s: ' "${p#pod/}"
  kubectl -n kube-system exec "${p#pod/}" -c zec-ipam -- \
    grep -c cilium-cni /host-etc-net.d/10-zenlayer-cni.conflist
done                                            # must be 0 on every node

# ③ Roll every workload -- this must happen BEFORE uninstalling Cilium
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout restart "$t/$n"; done
done
# Wait for every restart to finish: the uninstall must find no Pod still on the old wiring
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout status "$t/$n" --timeout=10m; done
done
while kubectl get pod -A --no-headers | grep -q Terminating; do sleep 5; done   # old Pods gone too

# ④ Then uninstall Cilium. Leave cni.uninstall at its default of false (reason below)
helm uninstall cilium -n kube-system

# ⑤ Clear the leftover BPF -- both kinds, on network devices and on the cgroup; see below
```

> **Step ⓪ cannot be skipped.** In the default shape of this document, Cilium carries every Service, so
> uninstalling directly leaves a window in which nothing handles Services. During that window,
> reaching `10.96.0.1` from the host namespace fails, and components running with
> `hostNetwork: true` (including this project's daemon) crash because they cannot reach the API
> server.

**Why step ③ must precede the uninstall**: `cil_from_container` and `cil_to_container` on a Pod veth
are **BPF links**. They do not disappear when the agent is removed, and `helm uninstall` does not
clean them up. While the agent is still running, it keeps the LB maps that those programs depend on
up to date, so that window is safe. **Once the agent is uninstalled, the maps freeze**: those Pods
keep sending ClusterIP traffic through a load-balancing table that is never updated again. When a
backend is removed, they still send a proportional share of connections to the deleted address,
while the `KUBE-SVC-*` counters stay at 0 and an investigation along the kube-proxy path finds
nothing.

> **Do not set `cni.uninstall=true`** until step ② has been confirmed on all nodes. Deleting
> `/opt/cni/bin/cilium-cni` while a node still carries the chained conflist breaks both ADD and DEL
> on that node. A failing DEL aborts the chain before this plugin runs, **leaking both the address
> and the veth**, and Pods hang in `Terminating`. Restoring the binary lets kubelet finish the
> teardown.
>
> **Allow time when deleting Pods in bulk**: while the agent is unreachable, every Pod takes about
> 20 extra seconds, and Pods are processed serially.

### Clearing leftover BPF

There are two kinds of leftover BPF, and **both are removed with `link detach`**. The second kind
is attached to the cgroup, where nothing short of rebooting the node can otherwise reach it, and its
consequences are worse: it keeps translating ClusterIPs at `connect()` time against a frozen LB
map, **affecting every Pod on that node**. The symptom is misleading. Services that existed before
the uninstall (such as kube-dns) still translate, so DNS works. Services created afterward are
completely unreachable, because the packet is rewritten inside the Pod to a backend address that
does not exist.

```bash
# ① On network devices (eth0 / cilium_host / cilium_net / Pod veths)
for i in $(seq 1 60); do
  id=$(bpftool net show | grep cil_ | grep -oE 'link_id [0-9]+' | head -1 | awk '{print $2}')
  [ -z "$id" ] && break; bpftool link detach id "$id"
done

# ② On the cgroup (socket LB)
for lid in $(bpftool link show | awk '/^[0-9]+: cgroup/{gsub(":","",$1); print $1}'); do
  pid=$(bpftool link show id "$lid" | grep -oE 'prog [0-9]+' | awk '{print $2}')
  bpftool prog show id "$pid" | grep -q 'name cil_' && bpftool link detach id "$lid"
done
```

`cilium_host` and `cilium_net` also survive the uninstall, and neither `link detach` nor
`helm uninstall` removes them. Delete them on every node:

```bash
ip link del cilium_host
```

They are a veth pair, so removing one removes both.

**Confirming it is clean** (run on every node; all three must be 0):

```bash
bpftool net show                   | grep -c cil_    # on network devices
bpftool cgroup show /sys/fs/cgroup | grep -c cil_    # on the cgroup -- the one most often missed
ip -o link show                    | grep -c cilium  # cilium_host / cilium_net
```

> ⚠️ **Do not use `kubectl get cep` to decide whether Cilium is still present**: its output is
> misleading. CEP objects remain in the API server after `helm uninstall` and look perfectly normal
> (`ready`, both IPv4 and IPv6 present) even though Cilium has long been removed. Use the three
> commands above, or `kubectl -n kube-system get ds cilium`.
>
> Do not use `tc filter show` to look for leftovers on veths either: tcx attachments are not
> visible there, so an empty result is a **false negative**. Use `bpftool net show | grep cil_`.

`/var/run/cilium/state` also survives the uninstall. It does not affect this plugin, but if Cilium
may be installed again, remove it on every node (`rm -rf /var/run/cilium/state`): a reinstalled
agent restores endpoints from it, so a Pod that was never restarted silently gets its CEP and BPF
programs back.

Everything else left behind (Cilium CRDs, CEP / CiliumIdentity objects, `/opt/cni/bin/cilium-cni`,
and the rest of `/run/cilium/`) does not affect this plugin and can be cleaned up manually:

```bash
kubectl delete cep --all -A
kubectl delete ciliumidentity --all
kubectl get crd | grep cilium | awk '{print $1}' | xargs -r kubectl delete crd   # recreated on reinstall
```
