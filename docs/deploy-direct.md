# Direct mode: every node calls the OpenAPI

English | [简体中文](deploy-direct-cn.md)

The [README](../README.md) describes the centralized deployment (`zec.ipamType=crd`): only the
controlplane holds the OpenAPI credentials, and nodes hold none. This document describes the
other mode, `zec.ipamType=direct`, in which **every node's daemon mounts the credentials and calls
the OpenAPI itself**. This mode has no controlplane and no `ZecNode`.

Except for the differences listed in this document, the datapath, address families, network
policy, Cilium chaining, and uninstall behave as described in the README.

## 1. When to choose direct

| | Centralized (`crd`, README) | Direct (this document) |
| --- | --- | --- |
| OpenAPI credentials | On the controlplane only | Mounted on **every node** |
| Internet egress (to reach the OpenAPI) | Only nodes running the controlplane | **Every node** |
| Kubernetes version | >= 1.30 (needs ValidatingAdmissionPolicy) | >= 1.28.2 |
| Extra components | Controlplane Deployment, `ZecNode` CRD, admission policies | None |
| Warm-pool settings | `controlplane.scaler.*`, applied without restarts | `zec.poolTarget*`, applied by restarting the daemons |
| OpenAPI call volume | Rate-limited centrally by the controlplane | Grows with the node count; each node calls on its own |

Use direct mode only when the cluster cannot run the centralized deployment, typically because
it runs Kubernetes older than 1.30. This mode has two costs: a single compromised node leaks the
account's OpenAPI credentials, and every node must be able to reach the internet.

In-place switching of an existing cluster between the two modes is not currently supported.
Choose the mode at install time.

## 2. Requirements

The requirements are the same as in the README's [Requirements](../README.md#requirements),
with two differences:

- Kubernetes >= 1.28.2 (the chart's `kubeVersion` constraint).
- **Every** node has **internet egress**, through either an EIP on the node or a NAT gateway for
  its VPC. The Zenlayer OpenAPI has no internal endpoint, and the daemon calls it from the node
  it runs on. A node that cannot reach the internet therefore cannot allocate a single pod
  address.

## 3. Install

### 3.1 Create the credential Secret

```sh
kubectl create secret -n kube-system generic zenlayer-cni-access-key \
  --from-literal=AccessKeyID='<access-key-id>' \
  --from-literal=AccessKeyPassword='<access-key-secret>'
```

The daemon on every node mounts the Secret read-only at `/etc/zec_secret/` and reads the two
keys as files. If the Secret has a different name, pass `--set zec.secretName=<name>`.

### 3.2 Install the chart

```sh
helm install zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.1 \
  --namespace kube-system \
  --set-string zec.vpcID=<vpc-id> \
  --set-string 'zec.subnetIDs={<subnet-id-1>,<subnet-id-2>}' \
  --set-string zec.securityGroupID=<security-group-id> \
  --set zec.ipamType=direct \
  --set zec.ipStack=ipv4 --set zec.manageUntaggedENI=false \
  --set cni.mode=chained --set cilium.enabled=false \
  --set networkPolicy.enabled=false
```

- The three IDs must be passed with `--set-string`. The README's
  [Install](../README.md#installation) section explains why.
- A dual-stack install takes the same IPv6 billing values as described in the README.
- Direct mode renders neither the controlplane nor the admission policies. The `ZecNode` CRD
  is located in `chart/crds/`, which Helm applies only on a first install: a fresh install
  receives the CRD, and an upgrade from 1.x does not. In either case, no component in direct
  mode uses it.

### 3.3 Verify

```sh
kubectl -n kube-system rollout status ds/zenlayer-vpc-cni
kubectl run nettest --image=busybox --restart=Never -- sleep 3600
kubectl wait --for=condition=Ready pod/nettest --timeout=60s
kubectl get pod nettest -o jsonpath='{.status.podIP}{"\n"}'
kubectl delete pod nettest

# On a node: pool state and health
ssh <node> 'curl -s 127.0.0.1:11414/metrics | grep -E "^resource_pool_"'
ssh <node> 'curl -s 127.0.0.1:11414/healthz'
```

The daemon's `/healthz` endpoint starts responding only after the daemon has reached the OpenAPI
and built its warm pool. For this reason, the DaemonSet allows a 30 x 10s startup window.

### 3.4 Upgrading from 1.x

A 1.x chart has neither `zec.ipamType` nor a `controlplane` block, and its daemons already call
the OpenAPI themselves. A 1.x release therefore upgrades to direct mode (requires Helm >= 3.14):

```sh
helm get values zenlayer-vpc-cni -n kube-system --all   # what the cluster runs now
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version <version> \
  --namespace kube-system --reset-then-reuse-values \
  --set zec.ipamType=direct \
  --set zec.ipStack=<ipv4|dual> --set zec.manageUntaggedENI=<true|false> \
  --set cni.mode=<mode> --set cilium.enabled=<bool> --set networkPolicy.enabled=<bool>
```

- Do **not** use `--reuse-values`. That flag also keeps the 1.x chart's defaults, including
  `image.tag: v1.0.0`. The upgrade then **succeeds** and the release reports the new chart
  version, but the DaemonSet still runs the 1.x image: nodes continue to run 1.x, and no error
  is reported anywhere. Check the image after every upgrade:
  `kubectl -n kube-system get ds zenlayer-vpc-cni -o jsonpath='{.spec.template.spec.containers[*].image}'`.
- `--reset-then-reuse-values` discards the old chart's defaults but keeps every value that was
  set explicitly, including `image.tag`. If 1.x was installed with a pinned `image.tag`, also
  pass `--set image.tag=<new-tag>`; otherwise, the result is the same as above.
- The behavior switches have no default in 2.x, and `--reset-then-reuse-values` keeps only the
  values that were set explicitly. Pass each switch with the value the cluster currently uses
  (the first command shows it). In 1.x, `zec.manageUntaggedENI` defaulted to `true`.
- Starting with 2.0.1 (see `[2.0.1]` in the CHANGELOG), pod addresses
  come only from `zec.subnetIDs`. If a node's primary interface is in a subnet that is not
  listed there, that interface stops serving new pods, and the node's capacity drops by that
  interface's share (see [Checking capacity](#5-checking-capacity)). To keep the previous
  capacity, list the node subnets in `zec.subnetIDs`.
- With `cilium.enabled=true` and `zec.k8sServiceHost` empty, the 2.x chart reads the apiserver
  address from `kube-public/cluster-info` at upgrade time. That address may belong to a single
  master. On a cluster without that ConfigMap (one not set up by kubeadm), and in any offline
  render (`helm template`, a client-side dry run, a GitOps tool), the render **fails** and the
  error names the two values to set. On Cilium clusters, pass `zec.k8sServiceHost` /
  `zec.k8sServicePort` explicitly, pointing at a load balancer or VIP. For details, see
  `zec.k8sServiceHost` in the README's [Configuration reference](../README.md#configuration-reference).
- The default `image.pullPolicy` changed from `Always` (1.x) to `IfNotPresent`. If you relied on
  re-pulling a moved tag, pass `--set image.pullPolicy=Always`.

## 4. Warm pool and GC

### Warm-pool settings

| Value | Default | Description |
| --- | --- | --- |
| `zec.poolTarget` | `2` | Target number of warm (pre-allocated, unused) IPs per node. |
| `zec.poolTargetMin` | `1` | Lower bound on the pool as a whole (addresses in use by pods included), not on the idle part. Must be <= `poolTarget`. |
| `zec.poolTargetMax` | `10` | Upper bound on the pool as a whole (addresses in use by pods included). **Ignored while `poolTargetCapProbe` is `true` (the default)**; the instance's real capacity is the bound instead. With probing off it becomes the capacity the node reports to the scheduler, so raise it to match the instance type, and it must be >= `poolTarget`. |
| `zec.poolTargetCapProbe` | `true` | Probe the instance's ENI/IP capacity; `poolTarget` and `poolTargetMax` may then be 0, and `poolTarget` may exceed `poolTargetMax`. |
| `zec.reconcileIntervalSec` | `1200` | GC interval in seconds. GC also runs early when the platform refuses an address because an interface has reached its address limit or the subnet does not support the requested IP stack. |

The daemon reads these values once at startup. A change takes effect when `helm upgrade` rolls
the DaemonSet.

### GC pauses the whole pool

In direct mode, the daemon's GC reconciles against the platform and **pauses the whole pool for
its entire run, including the OpenAPI calls**. Pod network setup on that node therefore queues
behind the GC, and CNI ADD times out at 118 seconds. The daemon logs the duration of each GC
run and logs a warning when a run takes long. If pods occasionally fail to start, check these
log lines first:

```sh
kubectl -n kube-system logs <daemon-pod> -c zec-ipam | grep -E 'Daemon GC (finished in|took)|Daemon gc failed after'
```

`Daemon GC took ... the pool was paused that whole time` marks a slow run, and
`Daemon gc failed after` marks a failed run. A fast, successful run logs `Daemon GC finished in`.

The centralized deployment does not have this behavior: in that mode, the daemon's GC compares
only against the `ZecNode`, and no OpenAPI call is on the path.

## 5. Checking capacity

The daemon logs the capacity it resolved at startup. When a node's capacity does not match the
table in the README's [Capacity planning](../README.md#capacity-planning), checking this log is
the quickest way to investigate:

```sh
kubectl -n kube-system logs <daemon-pod> -c zec-ipam | grep -E 'Attached eni|InstanceLimit Updated|Capacity'
# Attached eni created by cni: [], adopted: [], primary: [<primary-mac>]
# Attached eni not managed by cni: [<mac-1> <mac-2>]; 2 interface slot(s) held without serving pod addresses (ENIUnmanaged)
# InstanceLimit Updated {ENIQuota: 4, IPv4MaxPerENI: 30, IPv6CidrMaxPerENI: 1, ENIManaged: 0, ENIUnmanaged: 2, Cordon: false, allocatable: 58}
# Capacity 58
```

`ENIUnmanaged` is the number of interfaces this plugin does not manage, and `allocatable` is the
capacity once they are subtracted.

Pod addresses come only from the subnets in `zec.subnetIDs`. The primary interface serves pods
only when its subnet is listed. When the subnet is not listed, the node receives an
`InterfaceExcluded` event, and the capacity reported to the scheduler drops by that interface's
share, `IPv4MaxPerENI - 1` (less the addresses that existing pods still hold on it). The
`allocatable` value in the `InstanceLimit Updated` line does **not** include that deduction. The
`Capacity` line does include it and states the amount, e.g., `Capacity 16: 12 of the quota-based 28 sit on interfaces that take no
new pod addresses`.

If the instance cannot take any other interface (for example, a 2-interface instance whose
primary interface is excluded and whose other slot holds an unmanaged interface), the node
holds **no** pods. Direct mode has no dedicated event reason for this condition. The only sign
is a capacity of 0 for `zenlayer.com/eni-ip` in the node's allocatable resources, alongside the
`InterfaceExcluded` event.

## 6. Troubleshooting

**Node events** use the same reasons as in the README's
[Troubleshooting](../README.md#troubleshooting), but each node's daemon writes its own events.
Direct mode has no `ZecNode`, so `kubectl get zn` does not apply. Where the README refers to the
controlplane, read it as **that node's daemon**: `OpenApiUnreachable` means that node has no
internet egress, `NodeClockSkew` means that node's own clock is off, and
`OpenApiPermissionDenied` names an API that the node's daemon calls.

Direct mode adds the `InterfaceExcluded` reason. It means that an interface of the node (its
primary interface, or an interface the plugin created in a subnet that has since been removed)
is in a subnet not listed in `zec.subnetIDs`, so the interface serves no new pods. An interface
the plugin created is freed once its pods are gone. See
[Checking capacity](#5-checking-capacity). On an instance that cannot take any other interface,
the node then holds no pods at all, and no further event is emitted: `zenlayer.com/eni-ip`
shows 0 in `kubectl describe node`. `InterfaceExcluded` is a `Normal` event, so a
`type=Warning` filter hides it:

```sh
kubectl get events -A --field-selector reason=InterfaceExcluded
```

**Common startup failures** (the daemon reports these at startup and then exits), in addition
to those listed in the README:

| Error | Cause |
| --- | --- |
| `no credentials for the zenlayer api: ...` | The Secret's keys are not `AccessKeyID` / `AccessKeyPassword`. The message names the two paths the daemon tried and the chart value that points at the Secret. (A missing or misnamed Secret does not reach this point: the pod stays in `ContainerCreating` with a `FailedMount` event, and starts once the Secret exists.) |
| `securityGroup is not set in the cni configmap ...` / `subnets is empty in the cni configmap ...` | `zec.securityGroupID` / `zec.subnetIDs` is not set. |
| `subnets lists N entries but at most 5 are supported` | More than 5 subnets are configured. When installing through Helm, the chart's schema rejects this first; the daemon reports it only for a hand-edited ConfigMap. |
| `poolTargetMin (n) must be less than or equal to poolTarget (m) ...` / `poolTarget (n) must be less than or equal to poolTargetMax (m) ...` | The warm-pool bounds are inconsistent. `poolTargetMin <= poolTarget` must hold; with `zec.poolTargetCapProbe=false`, `poolTarget <= poolTargetMax` must also hold. |
| `poolTarget (n) and poolTargetMax (m) must both be > 0 unless poolTargetCapProbe is true ...` | With `zec.poolTargetCapProbe=false`, set both to positive values. |
| `ipStack dual needs every subnet to carry ipv6, but subnet <id> has no ipv6 cidr` | `zec.ipStack=dual`, but a subnet has only an IPv4 CIDR. |
| `ipv6InternetChargeType "ByTrafficPackage" requires ipv6PackageSize` | Set `zec.ipv6PackageSize` with `--set-json` (TB, a positive multiple of 0.1). |
| `ipv6InternetChargeType "BandwidthCluster" requires ipv6ClusterId` | Set `zec.ipv6ClusterId`. |
| `ipStack "dual" needs a positive ipv6Bandwidth (Mbps)` | `zec.ipv6Bandwidth` is negative. Zero falls back to the default, and the chart's schema requires >= 1, so only a hand-edited ConfigMap can produce this error. |

## 7. Uninstall

Uninstall as described in the README's [Uninstall](../README.md#uninstalling). The only
difference is which component reclaims leaked interfaces. In direct mode, each daemon, while it
runs, checks once an hour and deletes only the interfaces that were created for **its own
instance** (identified by the `zenlayer:cni:created-by` and `zenlayer:cni:instance-id` tags) and
that have been detached and untouched for more than 10 minutes. Direct mode has no VPC-wide
sweep. Once a daemon is gone (after uninstall, or when a node is deleted), nothing in this
cluster reclaims its interfaces. (A centralized-mode cluster in the same VPC does sweep detached
plugin interfaces that carry no `zenlayer:cni:cluster-id` tag, which includes every interface
created by 1.x; do not rely on it.) Delete these interfaces manually: select the detached
interfaces that carry the `zenlayer:cni:created-by` tag and that node's
`zenlayer:cni:instance-id`, and, where the `zenlayer:cni:cluster-id` tag is present, the
cluster's `zenlayer:cni:cluster-id`. The cluster ID is the UID of the `kube-system` namespace:

```sh
kubectl get ns kube-system -o jsonpath='{.metadata.uid}'
```
