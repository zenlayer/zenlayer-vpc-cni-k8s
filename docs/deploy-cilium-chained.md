# Deploying chained mode (veth datapath + Cilium CNI chaining)

> This document is the authoritative version. A Chinese translation is available at
> [deploy-cilium-chained-cn.md](./deploy-cilium-chained-cn.md).

This document describes how to enable NetworkPolicy on top of zenlayer-vpc-cni by chaining
**upstream community Cilium** after this plugin. This is `cni.mode=chained`, the default mode for
deploying this project together with Cilium.

**This project does not fork Cilium and does not distribute Cilium images.** Cilium is installed by
you from its own official chart, and you choose the version. This project does only two things:
append a `cilium-cni` plugin section to the conflist, and withhold the conflist until the Cilium
agent is ready.

Enabling Cilium is **optional**. When it is not enabled, the behavior of this plugin is unrelated
to this document: Services are handled by kube-proxy and the datapath is entirely unchanged.

> The other mode is `cni.mode=delegated`, where Cilium creates the Pod device (netkit or veth) and
> this plugin only supplies addresses -- see
> [deploy-cilium-netkit.md](./deploy-cilium-netkit.md). For how the two measure up see
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
| **Cilium version** | **v1.15 to v1.18** | v1.17.5 has been checked against upstream source and verified on a cluster. The lower bound is just the upstream maintenance window; the upper bound only states how far the check went. Before crossing versions, run `helm show values cilium/cilium --version <ver>` and re-check every key used here -- the chart's values structure changes between major versions |
| Kernel | >= 5.4 (>= 4.18 on RHEL 8 derivatives) | Cilium's own minimum |
| cgroup v2 | mounted | `stat -fc %T /sys/fs/cgroup` prints `cgroup2fs`. The socket-level load balancer attaches to the cgroup, so this is a hard prerequisite of `kubeProxyReplacement` |
| bpffs | mounted at `/sys/fs/bpf` | **The Cilium agent mounts it itself**; this project does not manage it |
| `helm` | reachable from wherever you run it | It does **not** need to be installed on a cluster node; any machine that can reach the API server will do |
| This plugin already running | yes | Get it working first, then chain |
| kube-proxy | removed once deployment completes | Services move to Cilium's eBPF. **Do not delete it outright** -- it is the fallback during deployment, and rollback needs it back |
| Cluster IP stack | single-stack IPv4, or dual-stack per [§4.3](#43-ipv4-single-stack-and-dual-stack) | On a dual-stack cluster, omitting those two keys leaves Pod IPv4 working while every Pod IPv6 packet is dropped **with no log line at all** |

What this adds to the cluster: one DaemonSet (cilium-agent), one Deployment (cilium-operator), and
Cilium's own set of CRDs. Upgrading Cilium requires no change to this project's configuration.

> **cilium-operator must be deployed, but it is not on the critical path for policy enforcement.**
> This design uses `identityAllocationMode=crd` (Cilium's default), where CiliumIdentity and
> CiliumEndpoint objects are created and maintained by the agent directly and the operator only
> performs identity garbage collection. With the operator gone, new Pods still receive identities
> and policy still takes effect; the only degradation is orphaned CiliumIdentity objects.
>
> It should still be kept healthy, and note that **its toleration is `[{"operator":"Exists"}]`, so
> it tolerates every taint** -- on a cluster with NotReady or cordoned nodes it can be scheduled
> onto a node that never comes up and stay `Pending`. Removal of the
> `node.cilium.io/agent-not-ready` taint is also the operator's exclusive job: without it, a node
> takes no new Pods even once its agent is Ready. After installing, confirm it landed on a healthy
> node:
>
> ```bash
> kubectl -n kube-system get pod -l io.cilium/app=operator -o wide
> ```

## 2. Quick start: switching from veth to this mode

Assumes this plugin is running, cgroup v2 is mounted, and the cluster is single-stack IPv4 (for
dual-stack see [§4.3](#43-ipv4-single-stack-and-dual-stack)). Why each parameter has the value it
has is in [§4](#4-configuration-reference).

```bash
# 0. Check the prerequisites
stat -fc %T /sys/fs/cgroup                      # must be cgroup2fs
kubectl -n kube-system get ds zenlayer-vpc-cni  # must be ready
helm version --short
# Is the cluster dual-stack? A non-zero count means you must follow §4.3 instead,
# or Pod IPv6 is dropped silently with no log at all
kubectl get nodes -o jsonpath='{.items[*].status.addresses[*].address}' | tr ' ' '\n' | grep -c ':'

# 1. Install Cilium. This --set list is the required set; missing one of them causes problems
#    (each is explained in §4.2)
helm repo add cilium https://helm.cilium.io/ && helm repo update cilium
helm install cilium cilium/cilium --version 1.17.5 --namespace kube-system \
  --set cni.chainingMode=generic-veth --set cni.customConf=true --set cni.exclusive=false \
  --set cni.install=true --set routingMode=native \
  --set enableIPv4Masquerade=false --set enableIPv6Masquerade=false --set bpf.masquerade=false \
  --set ipam.mode=delegated-plugin --set extraConfig.local-router-ipv4=169.254.32.1 \
  --set-string extraConfig.enable-session-affinity=true \
  --set endpointHealthChecking.enabled=false --set envoyConfig.enabled=false \
  --set endpointRoutes.enabled=true \
  --set kubeProxyReplacement=true --set bpf.hostLegacyRouting=false \
  --set operator.replicas=2
kubectl -n kube-system rollout status ds/cilium --timeout=10m

# 2. Turn on this project's chaining switches. The daemon waits for the Cilium agent to be ready
#    before writing the new conflist
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 1.0.0 \
  -n kube-system --reuse-values \
  --set cilium.enabled=true --set networkPolicy.enabled=true
kubectl -n kube-system rollout restart daemonset/zenlayer-vpc-cni
kubectl -n kube-system rollout status daemonset/zenlayer-vpc-cni --timeout=10m

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
kubectl -n kube-system get ds kube-proxy -o yaml > kube-proxy-backup.yaml   # keep for rollback
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
      -o jsonpath='{range .items[?(@.spec.hostNetwork!=true)]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' | sort) \
  <(kubectl get cep -A \
      -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' | sort)
```

**The order in step 4 cannot be reversed** -- confirm the takeover first, then stop kube-proxy. The
other way round leaves a window in which nothing handles Services, and components running with
`hostNetwork` (including this project's daemon) crash because they cannot reach the API server.

**Step 6 cannot be skipped.** Cilium endpoints are created during CNI ADD, so **Pods that were
already running before chaining was enabled have no CEP at all** and policy does not constrain
them. Until the restart completes, do not assume policy is in effect.

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

**The cost**: chaining attaches BPF programs to every Pod veth and to the node's eth0, which slows
hostNetwork traffic by about 8% with no way to recover it. On the Pod side eBPF host-routing more
than repays it, provided `kubeProxyReplacement` is left on.

## 4. Configuration reference

### 4.1 This project's two switches

Both live in this project's `chart/values.yaml`. **They are deliberately kept separate** -- wanting
Hubble observability without policy is a real case, and merging them would make that combination
inexpressible.

| Setting | Value here | Notes |
| --- | --- | --- |
| `cilium.enabled` | `true` | **The switch that actually changes the datapath**: appends the `cilium-cni` section to the conflist, mounts the agent socket into the daemonset, and makes the daemon wait for the agent before writing the conflist. |
| `networkPolicy.enabled` | `true` | Declares only that "policy must be enforced by someone"; it brings no engine of its own. **Requires `cilium.enabled` to be true as well**, or the chart refuses to render -- allowing that combination would mean traffic that should be blocked flows instead, and nobody notices. |
| `cni.mode` | `chained` (default) | The mode this document covers. For `delegated`, see the other document. |

> A misspelled switch fails at render time (the root of `values.schema.json` is
> `additionalProperties: false`), reporting `additional properties 'celium' not allowed`.

### 4.2 Cilium values

The table below is the complete set of Cilium values this design requires; **missing one of them
causes problems**. Items marked ⚠️ are explained under the table.

| Setting | Value here | Notes |
| --- | --- | --- |
| `cni.chainingMode` | `generic-veth` | Set it explicitly. Left unset, cilium-cni falls back to looking up a chainer named after the conflist's `name` -- renaming the network then stops chaining silently. |
| `cni.customConf` | `true` | A **hard prerequisite** of `generic-veth`: Cilium has no conflist template for that mode, `false` contradicts it, and the agent hangs at `0/1 Running`. |
| `cni.exclusive` | `false` | Defence in depth. With `customConf=true` this key never reaches the configmap, so it is effectively inert. |
| `cni.install` | `true` | This project needs the `/opt/cni/bin/cilium-cni` binary, installed by Cilium's init container. |
| `routingMode` | `native` | Pod IPs are ENI secondary addresses, natively routable in the VPC; no tunnel is needed. |
| `ipam.mode` | `delegated-plugin` | Tells Cilium that addresses come from the CNI binary and that it must not participate. |
| `extraConfig.local-router-ipv4` | `169.254.32.1` | ⚠️ **Not a helm value**, see below. The value only has to avoid `169.254.1.1`, the default gateway this plugin writes into the container. |
| `endpointHealthChecking.enabled` | `false` | A mandatory companion of `delegated-plugin`. |
| `envoyConfig.enabled` | `false` | Same. The cost is giving up Ingress / Gateway API. |
| `endpointRoutes.enabled` | `true` | Same. Leaving it unset CrashLoops the agent; the setting is outside what `checkIPAMDelegatedPlugin()` validates. |
| `kubeProxyReplacement` | `true` | ⚠️ Enabled **as a pair** with the next row, see below. |
| `bpf.hostLegacyRouting` | `false` | ⚠️ Same. **Setting it alone does nothing**: whenever `kubeProxyReplacement` is not `true` the agent resets it to `true`, so the setting only creates the impression that something was enabled. |
| `enableIPv4Masquerade` | `false` | ⚠️ Masquerading must be off entirely, see below. |
| `enableIPv6Masquerade` | `false` | Same. |
| `bpf.masquerade` | `false` | Same. |
| `extraConfig.enable-session-affinity` | `true` | Cilium enables it automatically when KPR is on; writing it explicitly keeps the configuration correct if KPR is later turned off. Without it and with KPR off, `sessionAffinity: ClientIP` is silently ignored. |
| `l7Proxy` | leave unset | Keeps Cilium's default of `true`. **Turning it off makes already-applied L7 and FQDN policies fail open rather than fail loudly** -- the L7 part of a rule is ignored and degrades to allowing the L4 traffic, with no warning. |
| `operator.replicas` | `2` | Same as the community chart's default. **Do not drop to 1**; see the note in §1. |
| `ipv6.enabled` | `true` for dual-stack only | See [§4.3](#43-ipv4-single-stack-and-dual-stack). |
| `extraConfig.local-router-ipv6` | dual-stack only | Same, and the value must avoid `fe80::1`. |

**⚠️ Masquerading must be off entirely.** Pod IPs are routable inside the VPC, so masquerading
rewrites the Pod's source to the node's address and the VPC no longer sees the Pod as the origin.
More importantly, an agent that finds **iptables-based** masquerading enabled falls back to the
host stack, which disables `bpf.hostLegacyRouting=false`.

**⚠️ `local-router-ipv4` and `local-router-ipv6` are only settable through `extraConfig`.** They are
not helm values -- `--set localRouterIPv4=` is **silently discarded**, after which the agent fails
to start because the setting is missing.

**Do not set `enable-node-port` on its own.** With `kubeProxyReplacement=true` the agent derives it,
along with `enable-external-ips`, `bpf-lb-sock`, `enable-host-port` and `enable-session-affinity`.
The ConfigMap is input, not effective configuration; for anything KPR-related, read
`cilium-dbg config -a`.

**A `hostPort` must stay below 30000.** With `kubeProxyReplacement=true` Cilium serves hostPort
from its own BPF load balancer and refuses any port inside the NodePort range (30000-32767),
logging `The requested hostPort ... is colliding with the configured NodePort range` and nothing
else: the Pod starts, `cilium-dbg status --verbose` still reports `HostPort: Enabled`, and only
`cilium-dbg service list | grep HostPort` shows that no entry was created.

To tell whether eBPF host-routing is actually on, use `cilium-dbg status`; that field is stable
across versions:

```bash
kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg status | grep '^Routing:'
# Routing:  Network: Native   Host: BPF      <- enabled (the default here)
# Routing:  Network: Native   Host: Legacy   <- going through the host stack
```

> Do not use `grep ENABLE_HOST_ROUTING /run/cilium/state/globals/node_config.h` as the test. In
> newer versions that macro became a runtime option, so the grep is always 0 and reads as
> "not enabled".

### 4.3 IPv4 single-stack and dual-stack

Decide **whether Pods need IPv6** first -- it determines what the Kubernetes cluster itself must
be, and changing it after installation is expensive.

| | Single-stack (Pod IPv4 only) | Dual-stack (Pod IPv4 + IPv6) |
|---|---|---|
| Requirement on the k8s cluster | none | ⚠️ **the cluster itself must be dual-stack** (Node objects carrying IPv6) |
| `ipv6.enabled` | `false` (default, no need to write it) | **`true`** |
| `extraConfig.local-router-ipv6` | not needed | **required**, and must avoid `fe80::1` |
| This project's `zec.ipStack` | `ipv4` (default) | **`dual`** |

The single-stack install command is step 1 of the
[quick start](#2-quick-start-switching-from-veth-to-this-mode). Dual-stack adds:

```bash
  --set ipv6.enabled=true \
  --set-string extraConfig.local-router-ipv6=fe80::ffff:1
```

and every node's Node object must carry an IPv6 address **first**. That address comes from
kubelet's `--node-ip`:

```bash
# On every node; <v4> is the node's existing InternalIP and <v6> is a global IPv6
# taken from THE SAME interface that carries that v4
cp -n /etc/default/kubelet /etc/default/kubelet.bak
echo "KUBELET_EXTRA_ARGS=--node-ip=<v4>,<v6>" > /etc/default/kubelet
systemctl restart kubelet
kubectl get node <name> -o jsonpath='{.status.addresses}'
```

- **The IPv6 must come from the interface that carries the node IP.** A node with several ENIs has
  several global IPv6 addresses; picking the wrong one raises no immediate error, but the Node
  object then advertises an address belonging to a different interface and the control plane and
  datapath no longer agree. Measured: the node returns to `Ready` in about 10 seconds and running
  Pods are unaffected.
- **Only the Node objects need to be dual-stack, not Services.** The apiserver's
  `--service-cluster-ip-range` can stay single-stack -- it only decides whether dual-stack Services
  can be created, which is unrelated to Pod IPv6 connectivity.
- **Both failure directions are silent.** Enabling `ipv6.enabled` on a single-stack cluster makes
  the agent exit fatally because Node objects have no IPv6; leaving it off on a dual-stack cluster
  gives Pods IPv6 addresses and correct configuration while every packet is dropped by the tcx
  program as `Unsupported L3 protocol`, **with no log line at all**.

## 5. Rolling back to veth mode

**The order is fixed. Steps ⓪, ② and ③ cannot be skipped, and none of them may be swapped with
step ④.**

```bash
# ⓪ Put kube-proxy back first (step 4 of the quick start stopped it)
kubectl -n kube-system patch ds kube-proxy --type json \
  -p '[{"op":"remove","path":"/spec/template/spec/nodeSelector/kube-proxy-disabled"}]'
kubectl -n kube-system rollout status ds/kube-proxy --timeout=5m
# Confirm it really took over (run on a node)
iptables -t nat -S | grep -c '^-N KUBE-SVC'                                # expect > 0
curl -sk -o /dev/null -w '%{http_code}\n' https://10.96.0.1:443/healthz   # expect 200

# ① Turn both switches off so the conflist drops the cilium-cni section
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 1.0.0 \
  -n kube-system --reuse-values \
  --set cilium.enabled=false --set networkPolicy.enabled=false
kubectl -n kube-system rollout restart daemonset/zenlayer-vpc-cni
kubectl -n kube-system rollout status daemonset/zenlayer-vpc-cni --timeout=600s

# ② Confirm on EVERY node that the conflist no longer contains cilium-cni
for n in <every node>; do ssh root@$n \
  'grep -c cilium-cni /etc/cni/net.d/10-zenlayer-cni.conflist'; done   # must be 0 everywhere

# ③ Roll every workload -- this must happen BEFORE uninstalling Cilium
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout restart "$t/$n"; done
done

# ④ Then uninstall Cilium. Leave cni.uninstall at its default of false (reason below)
helm uninstall cilium -n kube-system

# ⑤ Clear the leftover BPF -- both kinds, on network devices and on the cgroup; see below
```

> **Step ⓪ cannot be skipped.** Under the default shape every Service is carried by Cilium, so
> uninstalling directly leaves a window in which nothing handles Services -- during it, reaching
> `10.96.0.1` from the host namespace fails and components running with `hostNetwork: true`
> (including this project's daemon) crash because they cannot reach the API server.

**Why step ③ must precede the uninstall**: `cil_from_container` and `cil_to_container` on a Pod veth
are **BPF links**. They do not disappear when the agent is removed, and `helm uninstall` does not
clean them. While the agent is still running it keeps the LB maps those programs depend on up to
date, so that window is safe; **once it is uninstalled the maps freeze** -- those Pods keep sending
ClusterIP traffic through a load-balancing table that never updates again, so removing a backend
still leaves them sending a proportional share of connections to the deleted address, while
`KUBE-SVC-*` counters stay at 0 and nothing shows up when investigating along the kube-proxy path.

> **Do not set `cni.uninstall=true`** until step ② has been confirmed on all nodes. Deleting
> `/opt/cni/bin/cilium-cni` while a node still carries the chained conflist kills both ADD and DEL
> on that node -- a failing DEL aborts the chain before this plugin runs, **leaking both the address
> and the veth**, and Pods hang in `Terminating`. Putting the binary back lets kubelet finish the
> teardown.
>
> **Allow time when deleting Pods in bulk**: while the agent is unreachable every Pod costs about
> 20 extra seconds, serially.

### Clearing leftover BPF

There are two kinds, and **both are removed with `link detach`**. The second kind attaches to the
cgroup, where nothing short of rebooting the node can reach it otherwise, and its consequences are
worse: it keeps translating ClusterIPs at `connect()` time against a frozen LB map, **affecting
every Pod on that node**. The symptom is deceptive -- Services that existed before the uninstall
(such as kube-dns) still translate, so DNS works, while Services created afterwards are completely
unreachable because the packet is rewritten inside the Pod to a backend address that does not exist.

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

`cilium_host` and `cilium_net` survive the uninstall as well, and neither `link detach` nor
`helm uninstall` removes them. Delete them on every node:

```bash
ip link del cilium_host
```

They are a veth pair, so removing one takes both.

**Confirming it is clean** (run on every node; all three must be 0):

```bash
bpftool net show                   | grep -c cil_    # on network devices
bpftool cgroup show /sys/fs/cgroup | grep -c cil_    # on the cgroup -- the one most often missed
ip -o link show                    | grep -c cilium  # cilium_host / cilium_net
```

> ⚠️ **Do not use `kubectl get cep` to decide whether Cilium is still there** -- it lies. CEP
> objects remain in the API server after `helm uninstall` and look perfectly normal (`ready`, both
> IPv4 and IPv6 present) while Cilium is long gone. Use the three commands above, or
> `kubectl -n kube-system get ds cilium`.
>
> Do not use `tc filter show` to look for leftovers on veths either -- tcx attachments are
> invisible there, so an empty result is a **false negative**. Use `bpftool net show | grep cil_`.

Everything else left behind (Cilium CRDs, CEP / CiliumIdentity objects, `/opt/cni/bin/cilium-cni`,
`/run/cilium/`) does not affect this plugin and can be cleaned by hand:

```bash
kubectl delete cep --all -A
kubectl delete ciliumidentity --all
kubectl get crd | grep cilium | awk '{print $1}' | xargs -r kubectl delete crd   # recreated on reinstall
```
