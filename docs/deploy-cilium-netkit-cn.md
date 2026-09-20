# 部署 netkit 数据面（Cilium 主 CNI + 本项目提供地址）

> 本文为中文翻译，以英文版 [deploy-cilium-netkit.md](./deploy-cilium-netkit.md) 为准。

本模式把 Pod 设备的创建权交给 Cilium，本项目退为它的 IPAM 插件（本侧为 `cni.mode=delegated`）：

```
kubelet → cilium-cni（主 CNI，建设备、配 Pod 内网络）
              └─ ipam: zec-cni（分配地址、装宿主 per-ENI 路由）
```

IP 分配、ENI 生命周期、温池与 GC 全部不变，仍由本项目负责。

## 目录

- [1. 前提条件](#1-前提条件)
- [2. 快速开始：从 veth 切到本模式](#2-快速开始从-veth-切到本模式)
- [3. 功能矩阵](#3-功能矩阵)
- [4. 配置参考](#4-配置参考)
- [5. 回退到 veth 模式](#5-回退到-veth-模式)

## 1. 前提条件

| 项 | 要求 | 备注 |
|---|---|---|
| **Cilium 版本** | **≥ v1.16** | netkit 的 `bpf.datapathMode` 从 1.16 起提供；本文按 v1.17.5 写 |
| 内核 | **≥ 6.7** | netkit 设备本身的最低要求，高于串接模式的 5.4 |
| cgroup v2 | 挂载 | `stat -fc %T /sys/fs/cgroup` 输出 `cgroup2fs`，`kubeProxyReplacement` 的硬前提 |
| `kubeProxyReplacement` | **必须为 true** | netkit 与 eBPF host-routing 都以它为前提 |
| 本项目已正常运行 | 是 | 先以 `cni.mode=chained` 跑通，再切 |
| `helm` | 能访问执行机 | 不必装在集群节点上 |
| 集群协议栈 | 单栈 IPv4，或按 [§4.6](#46-ipv4-单栈与双栈) 部署的双栈 | 双栈漏了那两个键会让 Pod 的 IPv6 被静默丢弃 |

**设备类型在 Pod 创建时固定。** 切换 `bpf.datapathMode` 只改变新建 Pod 拿到的设备，已在运行的
Pod 保留创建时的设备直到被重建，因此在工作负载滚完之前，同一节点上两种设备并存。
该设置是整集群的，无法按节点灰度。

## 2. 快速开始：从 veth 切到本模式

每个参数为什么是这个值，见 [§4](#4-配置参考)。

```bash
# 0. 前提自检
stat -fc %T /sys/fs/cgroup                      # 必须是 cgroup2fs
uname -r                                        # netkit 需要 >= 6.7
kubectl -n kube-system get ds zenlayer-vpc-cni  # 必须已就绪
helm version --short
# 集群是不是双栈 —— 输出非 0 时第 1 步要加 §4.6 的两个键
kubectl get nodes -o jsonpath='{.items[*].status.addresses[*].address}' | tr ' ' '\n' | grep -c ':'

# 1. 装 Cilium（若已按串接模式装过，改用本节末尾的 helm upgrade）
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

# 2. 把本项目切到 delegated。daemon 会等 Cilium agent 就绪后才落盘新 conflist
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 1.0.0 \
  -n kube-system --reuse-values \
  --set cni.mode=delegated --set cilium.enabled=true --set networkPolicy.enabled=true
kubectl -n kube-system rollout restart daemonset/zenlayer-vpc-cni
kubectl -n kube-system rollout status daemonset/zenlayer-vpc-cni --timeout=10m

# 3. 确认每个节点都接管了 Service，再下掉 kube-proxy
kubectl -n kube-system get pod -l k8s-app=cilium -o name | while read -r p; do
  kubectl -n kube-system exec "${p#pod/}" -c cilium-agent -- \
    cilium-dbg status | grep -E '^KubeProxyReplacement:|^Routing:|^Device Mode:'
done                                            # 每个节点都要是 True + Host: BPF + netkit
kubectl -n kube-system get ds kube-proxy -o yaml > kube-proxy-backup.yaml
kubectl -n kube-system patch ds kube-proxy -p \
  '{"spec":{"template":{"spec":{"nodeSelector":{"kube-proxy-disabled":"true"}}}}}'

# 4. 在每个节点上清掉 kube-proxy 遗留的规则
iptables-save  | grep -v KUBE | iptables-restore
ip6tables-save | grep -v KUBE | ip6tables-restore

# 5. 滚动重启所有工作负载 —— 不能省，旧 Pod 还挂在本项目建的 veth 上
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout restart "$t/$n"; done
done

# 6. 清掉切换前遗留的 Cilium endpoint（见下方警告）
kubectl -n kube-system get pod -l k8s-app=cilium -o name | while read -r p; do
  kubectl -n kube-system exec "${p#pod/}" -c cilium-agent -- \
    cilium-dbg endpoint list | grep -E 'eni[0-9a-f]+'
done                                            # 列出的逐个 cilium-dbg endpoint delete <id>

# 7. 确认 conflist 已是 delegated 形态：主插件 cilium-cni，ipam 指向 zec-cni
kubectl -n kube-system get pod -l app=zenlayer-vpc-cni -o name | while read -r p; do
  printf '%s: ' "${p#pod/}"
  kubectl -n kube-system exec "${p#pod/}" -c zec-ipam -- \
    grep -o '"type": "[a-z-]*"' /host-etc-net.d/10-zenlayer-cni.conflist | tr '\n' ' '; echo
done
```

若 Cilium 已按串接模式装过，第 1 步换成：

```bash
helm upgrade cilium cilium/cilium --version 1.17.5 -n kube-system --reuse-values \
  --set bpf.datapathMode=netkit \
  --set installUplinkRoutesForDelegatedIPAM=false \
  --set cni.chainingMode=null
kubectl -n kube-system rollout restart ds/cilium
kubectl -n kube-system rollout status ds/cilium --timeout=10m
```

> **⚠️ 第 6 步不能省。** 切换会留下指向旧设备的 Cilium endpoint：宿主设备是本项目建的
> `eni*` veth（已经不存在），而不是 netkit 的 `lxc*`。两种后果都不报错 ——
> 新 Pod 被分到旧 endpoint 占着的地址时 `cilium-cni` 返回 `400 IP <addr> is already in use`，
> 而 CNI ADD 没有降级路径；更隐蔽的是地址被回收后发给了**另一个节点**的 Pod，
> 而留着陈旧 endpoint 的节点仍把它当本地地址，于是流量在这个节点被截住。
> 根因是地址的真相有两份且不对账：本项目的 IPAM 池认为已释放，Cilium 的 endpoint 管理器
> 认为仍被占用。

## 3. 功能矩阵

| | 串接模式（默认） | **netkit 模式（本文）** |
|---|---|---|
| Pod 设备 | 本项目创建 veth 对 | **Cilium 创建**（netkit 或 veth） |
| Pod 地址 | 本项目分配 | 本项目分配（不变） |
| ENI 生命周期、温池、GC | 本项目 | 本项目（不变） |
| 宿主 per-ENI 策略路由 | 本项目 | 本项目（不变） |
| NetworkPolicy / Hubble | Cilium | Cilium |
| Service 转发 | Cilium（KPR 开时） | Cilium（KPR 必须开） |
| `hostPort` | ✅ | ✅ 但端口必须落在 NodePort 范围之外，见 §4.5 |
| conflist 里的 `ExtraRoutes` | ✅ 写进容器 | ❌ **无出口**，见 §4.2 |
| conflist 里的 `veth_prefix` | 可设，生效 | ❌ 设了也无意义 |
| 多网卡 Pod | ✅ | ❓ **未验证**，只用单接口 |
| 生产就绪 | ✅ | ❌ **未验证** |

## 4. 配置参考

### 4.1 本项目的 values

| 配置项 | 取值 | 说明 |
| --- | --- | --- |
| `cni.mode` | `delegated` | 渲染 delegated 版 conflist：主插件是 `cilium-cni`，本项目落在它的 `ipam` 块里。要求 `cilium.enabled=true`，否则 chart `fail`。 |
| `cilium.enabled` | `true` | daemon 会**阻塞等待** `/var/run/cilium/cilium.sock` 之后才落盘新 conflist。Cilium 的 ADD 没有降级路径，agent 不在就每个 Pod 都失败。 |
| `networkPolicy.enabled` | 按需 | 只是「声明策略必须被执行」，要求 `cilium.enabled=true` 同时为真。 |
| `zec.dataPath` | 不设 | 描述的是本项目自己建的设备，本模式下不建，schema 也只允许 `veth`。 |

### 4.2 conflist 里的键

`ipam` 块里有两个由模板固定写死的键：

| 键 | 取值 | 说明 |
| --- | --- | --- |
| `zec_mode` | `ipam-only` | 选择 IPAM-only 路径。**未知取值会被拒绝**而不是回落到完整插件 —— 一个拼错的值如果被忽略，节点会跑着与配置不符的数据面。 |
| `install_host_routes` | `true` | 让 IPAM-only 路径装这个 Pod 的 per-ENI ip rule（`from <podIP>` prio 2048）与该 ENI 的默认路由。关掉它，落在辅助网卡上的 Pod 会从实例自身网卡出去、源地址属于另一个端口的 MAC，被网络丢弃。缺省即为 `true`。 |

本项目不创建设备，以下描述 Pod 内网络的键随之失效：

| 键 | 本模式下 |
| --- | --- |
| `ExtraRoutes` | **不支持**。delegated IPAM 的返回值里 Routes 会被主 CNI 丢弃，需求只能改用 Cilium 侧的配置表达。 |
| `DefaultRoute` | 由 Cilium 决定，指向它自己的 `cilium_host`。 |
| `veth_prefix` | 无意义。本项目不建设备，也就没有 host veth 可命名。 |
| `eniip_virtual_type` | 无意义，conflist 里不渲染。 |
| `mtu` | 由 Cilium 的 `MTU` / `RouteMTU` 决定。**两侧要一致**，否则出现 MTU 黑洞。 |

### 4.3 Cilium values

下表是本方案要设置的全部 Cilium values。与[串接模式](deploy-cilium-chained-cn.md)的差异用 **★** 标出。

| 配置项 | 本文档取值 | 说明 |
| --- | --- | --- |
| ★ `bpf.datapathMode` | `netkit` | Pod 设备用 netkit 而不是 veth。`netkit-l2` 选的是 L2 模式，怎么选见 [§4.4](#44-netkit-l3-与-l2-的选择)。 |
| ★ `installUplinkRoutesForDelegatedIPAM` | `false` | **必须关**。这是 Cilium 自己装宿主路由的路径，在本平台不可用：它要求上行网卡带 `IFF_SLAVE` 标志，而本平台的 ENI 没有；且它对多 ENI 节点只认一张路由表，会让第二块之后的 ENI 共用表号。宿主路由改由本项目按「每 ENI 一张表，表号 = 1000 + ifindex」装配。 |
| ★ `cni.chainingMode` | **不设** | 本模式不是串接，Cilium 是主 CNI。设了会让它去找 chainer 并失败。 |
| `cni.customConf` | `true` | conflist 由本项目写，Cilium 不要自己渲染。 |
| `cni.exclusive` | `false` | 纵深防御。 |
| `cni.install` | `true` | 需要 `/opt/cni/bin/cilium-cni` 这个二进制。 |
| `routingMode` | `native` | Pod IP 是 VPC 原生可路由的 ENI 辅助 IP，不需要隧道。 |
| `ipam.mode` | `delegated-plugin` | 告诉 Cilium 地址由 CNI 二进制分配、它不要参与，也不要让 operator 划 PodCIDR。 |
| `extraConfig.local-router-ipv4` | `169.254.32.1` | ⚠️ **不是 helm value**，写成 `--set localRouterIPv4=` 会被静默丢弃然后 agent 启动失败。取值只需避开本项目给 Pod 的网关 `169.254.1.1`。 |
| `endpointRoutes.enabled` | `true` | 与 `local-router-ipv4` **必须同开**，否则 agent `fatal`。它还提供 main 表里到每个 Pod 的路由 —— 本模式下没有 host veth，那条路由正是由它提供的，本项目不再装，否则同一地址会有两条指向不同设备的路由。 |
| `endpointHealthChecking.enabled` | `false` | `delegated-plugin` 的强制伴随项。 |
| `envoyConfig.enabled` | `false` | 同上。代价是放弃 Ingress / Gateway API。 |
| `kubeProxyReplacement` | `true` | ⚠️ 与下一项**成对**启用，也是 netkit 的前提。 |
| `bpf.hostLegacyRouting` | `false` | ⚠️ 同上。单独设是无效的：agent 在 `kubeProxyReplacement` 不为 `true` 时会把它强制改回 `true`。 |
| `enableIPv4Masquerade` | `false` | ⚠️ masquerade 必须全关，见下。 |
| `enableIPv6Masquerade` | `false` | 同上。 |
| `bpf.masquerade` | `false` | 同上。 |
| `extraConfig.enable-session-affinity` | `true` | KPR 开启时 agent 会自动派生它，省略与否不影响 `sessionAffinity: ClientIP` 生效；显式写出可使配置在 KPR 被关闭后依然正确。 |
| `operator.replicas` | `2` | `node.cilium.io/agent-not-ready` 污点的摘除由 operator 独家负责，单副本没有冗余。 |

**⚠️ masquerade 必须全关。** Pod IP 在 VPC 内本身可路由，开着会把源地址改写成节点地址。
更要紧的是：agent 启动时若发现启用了 **iptables 方式**的 masquerade，会把
`bpf.hostLegacyRouting` 强制改回 `true`，eBPF host-routing 随之失效 —— 本模式的性能正建立
在它之上，而不是建立在 netkit 设备本身。

**不要单独设 `enable-node-port`。** `kubeProxyReplacement=true` 时 agent 会自动派生它，
连同 `enable-external-ips`、`bpf-lb-sock`、`enable-host-port`、`enable-session-affinity`
一起。ConfigMap 是输入而不是生效配置，KPR 相关项一律以 `cilium-dbg config -a` 为准。

Service 与策略的语义与串接模式的默认档完全一致，包括 `externalTrafficPolicy: Local`
在集群内不生效这一条。

### 4.4 netkit L3 与 L2 的选择

`bpf.datapathMode` 决定 Pod 设备以哪种模式创建：

| | **L3（`netkit`）** | **L2（`netkit-l2`）** |
|---|---|---|
| Pod 网卡的 MAC 地址 | 无，全 0 | 有 |
| ARP 处理 | 不需要 | 需要 |
| 性能 | 上游推荐值 | 与 L3 的差别在 TCP_RR 上不可分辨（[performance-cn.md](performance-cn.md) §6.1） |
| 什么时候用 | 其余全部情况 | 工作负载需要 Pod 内的以太层语义 |
| 本文的验证状态 | ✅ | ✅ 与 L3 等价 |

**需要 L2 的具体场景，是会去读网卡 MAC 的工作负载。** KubeVirt 定义虚拟机时会把这个 MAC 交给
libvirt，L3 下它是空的，virt-handler 直接失败并报
`XML error: unable to parse mac address ''`。其余会读 MAC、或在 Pod 内做 ARP / DHCP 的负载，
撞的是同一堵墙。

本文其余内容两种模式完全相同，差别只有一个取值：

```bash
  --set bpf.datapathMode=netkit-l2
```

- **L3 与 L2 之间切换是整集群的，且必须重建 Pod**，与从 veth 切过来一样（§1）。
  设备模式在 Pod 创建时就固定了。
- **凡属本插件职责的部分，两种模式等价**：per-ENI 规则与路由、地址分配与回收、多 ENI 出向、
  新挂 ENI 上的首个 Pod、NetworkPolicy 的执行、Service 的全部形态。两者的行为差异只在邻居解析：
  L3 下设备带 `NOARP`、两侧邻居表恒为空；L2 下宿主到 Pod 走 ARP/NDP 并留下动态 `REACHABLE`
  表项，Pod 对网关的请求由 Cilium 的 BPF 程序就地应答。
- **Cilium ≥ 1.20** 另有一个 `auto`：探测内核，netkit 不可用时**静默回落到 veth**。
  本模式下不要用它 —— 静默回落会让集群跑在一个与配置不符的数据面上。

### 4.5 `hostPort` 不能落在 NodePort 范围内

`kubeProxyReplacement=true` 时，hostPort 由 Cilium 自己的 BPF 负载均衡承担，本模式的 conflist
因此不再串接 `portmap`。但 agent **会拒绝任何落在 NodePort 范围内的 hostPort**（默认
30000–32767），并打一条日志：

```
The requested hostPort 31883 is colliding with the configured NodePort range [30000, 32767]. Ignoring.
```

**除此之外没有任何提示。** Pod 正常启动，`cilium-dbg status --verbose` 照样显示
`HostPort: Enabled`，只有 `cilium-dbg service list` 才能看出根本没有建条目。本模式下没有第二个实现 hostPort 的插件，被拒的端口因此在任何方向上都无人应答。

端口落在范围外时，hostPort 在五个方向上均可访问：Pod 所属节点、其他节点上的 Pod、
其他节点的 host netns、VPC 内的非集群主机、以及集群外。此时 `cilium-dbg service list` 中会有
四条 HostPort 条目 —— IPv4 与 IPv6 各两条（节点地址与通配）。

**所以请把 hostPort 选在 30000 以下**，并验证它确实生效：

```bash
kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg service list | grep HostPort
```

输出为空就说明端口被拒了，无论 `status --verbose` 怎么说。

### 4.6 IPv4 单栈与双栈

协议栈由参数决定，两者的差别只有下面这两个键：

| 集群协议栈 | 需要追加的 Cilium values |
|---|---|
| **IPv4 单栈** | 无（上面的快速开始即为单栈配置） |
| **双栈** | `--set ipv6.enabled=true`<br>`--set extraConfig.local-router-ipv6=fe80::ffff:1` |

- 集群本身必须是双栈（Node 对象带 IPv6），否则加了这两个键也没有意义。
- `local-router-ipv6` 的取值必须避开 `fe80::1` —— 那是本项目给 Pod 的 IPv6 网关。
- 在双栈集群上**漏掉**这两个键，Pod 的 IPv6 会被静默丢弃且无任何错误日志。

## 5. 回退到 veth 模式

```bash
# ① 先把本项目切回 chained
helm upgrade zenlayer-vpc-cni <chart> -n kube-system --reuse-values --set cni.mode=chained
kubectl -n kube-system rollout restart daemonset/zenlayer-vpc-cni

# ② Cilium 换回串接参数
helm upgrade cilium cilium/cilium --version 1.17.5 -n kube-system --reuse-values \
  --set cni.chainingMode=generic-veth --set bpf.datapathMode=veth
kubectl -n kube-system rollout restart ds/cilium

# ③ 滚动重启所有工作负载 —— 必须做，否则 netkit 设备会一直留着
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout restart "$t/$n"; done
done
```

这样回到的是**串接模式的 veth**。若要继续回到不装 Cilium 的纯 veth，以及卸载后的残留清理
（cgroup 上的 socket-LB 程序、孤儿 CiliumEndpoint），见
[deploy-cilium-chained-cn.md](deploy-cilium-chained-cn.md) 的回退一节。
