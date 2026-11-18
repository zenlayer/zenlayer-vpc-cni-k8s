# 部署 netkit 数据面（Cilium 主 CNI + 本项目提供地址）

> 本文为中文翻译，以英文版 [deploy-cilium-netkit.md](./deploy-cilium-netkit.md) 为准。

本模式下，Pod 设备由 Cilium 创建，本项目仅作为 Cilium 的 IPAM 插件（本侧为 `cni.mode=delegated`）：

```
kubelet → cilium-cni（主 CNI，建设备、配 Pod 内网络）
              └─ ipam: zec-cni（分配地址、装宿主 per-ENI 路由）
```

IP 分配、ENI 生命周期、温池与 GC 均保持不变，仍由本项目负责。

## 目录

- [1. 前提条件](#1-前提条件)
- [2. 快速开始：从 veth 切到本模式](#2-快速开始从-veth-切到本模式)
- [3. 功能矩阵](#3-功能矩阵)
- [4. 配置参考](#4-配置参考)
- [5. 回退到 veth 模式](#5-回退到-veth-模式)

## 1. 前提条件

| 项 | 要求 | 备注 |
|---|---|---|
| **Cilium 版本** | **≥ v1.16** | netkit 的 `bpf.datapathMode` 从 1.16 起提供；本文基于 v1.17.5 编写 |
| 内核 | **≥ 6.7** | netkit 设备本身的最低要求，高于串接模式要求的 5.4 |
| cgroup v2 | 挂载 | `stat -fc %T /sys/fs/cgroup` 输出 `cgroup2fs`；这是 `kubeProxyReplacement` 的硬前提 |
| `kubeProxyReplacement` | **必须为 true** | netkit 与 eBPF host-routing 都以它为前提 |
| 本项目已正常运行 | 是 | 先以 `cni.mode=chained` 运行正常，再切换 |
| `helm` | 在执行命令的机器上可用 | 不必安装在集群节点上 |
| 集群协议栈 | 单栈 IPv4，或按 [§4.6](#46-ipv4-单栈与双栈) 部署的双栈 | 双栈集群遗漏这两个键时，Pod 的 IPv6 会被静默丢弃 |

**设备类型在 Pod 创建时固定。** 切换 `bpf.datapathMode` 只改变新建 Pod 获得的设备；已在运行的
Pod 保留创建时的设备，直到被重建。因此在工作负载完成滚动之前，同一节点上两种设备并存。
该设置作用于整个集群，无法按节点灰度。

## 2. 快速开始：从 veth 切到本模式

每个参数取值的原因见 [§4](#4-配置参考)。

```bash
# 0. 前提自检
stat -fc %T /sys/fs/cgroup                      # 必须是 cgroup2fs
uname -r                                        # netkit 需要 >= 6.7
kubectl -n kube-system get ds zenlayer-vpc-cni  # 必须已就绪
helm version --short
# 集群是不是双栈 —— 输出非 0 时第 1 步要加 §4.6 的两个键
kubectl get nodes -o jsonpath='{.items[*].status.addresses[*].address}' | tr ' ' '\n' | grep -c ':'

# apiserver 入口 —— 第 1 步的 k8sServiceHost=auto 读的就是这一行。必须有输出且带端口，
# 并且应是负载均衡 / VIP；是某台 master 的 IP 则它成为单点。没有输出（非 kubeadm 集群）见 §4.3 的 `k8sServiceHost` 一行
kubectl -n kube-public get cm cluster-info -o jsonpath='{.data.kubeconfig}' | grep server:

# 1. 装 Cilium（若已按串接模式装过，改用本节末尾的 helm upgrade）
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
# 确认 auto 已解析成真实地址 —— 输出是字面量 auto 或为空，agent 会连不上 apiserver
kubectl -n kube-system get ds cilium -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="KUBERNETES_SERVICE_HOST")].value}{"\n"}'

# 2. 把本项目切到 delegated。daemon 会等 Cilium agent 就绪后才落盘新 conflist。
#    networkPolicy.enabled 按需（§4.1）。用 --reset-then-reuse-values（Helm ≥ 3.14）：
#    --reuse-values 会连旧 chart 版本的默认值一起沿用
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.0 \
  -n kube-system --reset-then-reuse-values \
  --set cni.mode=delegated --set cilium.enabled=true --set networkPolicy.enabled=true
kubectl -n kube-system rollout restart daemonset/zenlayer-vpc-cni
kubectl -n kube-system rollout status daemonset/zenlayer-vpc-cni --timeout=10m
# 必须打印出 daemon 连接的 apiserver 入口（见 §4.1 的 zec.k8sServiceHost）；cilium.enabled 时本 chart
# 拿不到入口会直接拒绝渲染，所以输出为空说明 DaemonSet 来自更早的 chart
kubectl -n kube-system get ds zenlayer-vpc-cni -o jsonpath='{..env[?(@.name=="KUBERNETES_SERVICE_HOST")].value}{"\n"}'

# 3. 确认每个节点都接管了 Service，再下掉 kube-proxy
kubectl -n kube-system get pod -l k8s-app=cilium -o name | while read -r p; do
  kubectl -n kube-system exec "${p#pod/}" -c cilium-agent -- \
    cilium-dbg status | grep -E '^KubeProxyReplacement:|^Routing:|^Device Mode:'
done                                            # 每个节点都要是 True + Host: BPF + netkit
# 保留最早的一份：若串接模式已经下线过 kube-proxy，要恢复的是那时的备份
[ -e kube-proxy-backup.yaml ] || kubectl -n kube-system get ds kube-proxy -o yaml > kube-proxy-backup.yaml
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

# 6. 清掉切换前遗留的 Cilium endpoint（见下方警告）。先等第 5 步重启的每个工作负载完成：
#    rollout restart 返回时新 Pod 还没有建出来
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout status "$t/$n" --timeout=10m; done
done
while kubectl get pod -A --no-headers | grep -q Terminating; do sleep 5; done   # 旧 Pod 也要退完
# 然后逐节点只列出宿主设备（eni*）已经不存在的 endpoint
kubectl -n kube-system get pod -l k8s-app=cilium -o name | while read -r p; do
  echo "== ${p#pod/}"
  kubectl -n kube-system exec "${p#pod/}" -c cilium-agent -- sh -c '
    cilium-dbg endpoint list -o jsonpath="{range [*]}{.id} {.status.networking.interface-name}{\"\n\"}{end}" |
    while read -r id ifn; do
      case "$ifn" in eni*) ip link show "$ifn" >/dev/null 2>&1 || echo "$id $ifn";; esac
    done'
done
# 在列出它的那个节点上逐个删除：
#   kubectl -n kube-system exec <cilium-pod> -c cilium-agent -- cilium-dbg endpoint disconnect <id>

# 7. 确认 conflist 已是 delegated 形态：主插件 cilium-cni，ipam 指向 zec-cni
kubectl -n kube-system get pod -l app=zenlayer-vpc-cni -o name | while read -r p; do
  printf '%s: ' "${p#pod/}"
  kubectl -n kube-system exec "${p#pod/}" -c zec-ipam -- \
    grep -o '"type": "[a-z-]*"' /host-etc-net.d/10-zenlayer-cni.conflist | tr '\n' ' '; echo
done
```

若 Cilium 已按串接模式装过，第 1 步换成：

```bash
helm upgrade cilium cilium/cilium --version 1.17.5 -n kube-system --reset-then-reuse-values \
  --set bpf.datapathMode=netkit \
  --set ipam.installUplinkRoutesForDelegatedIPAM=false \
  --set cni.chainingMode=null \
  --set k8sServiceHost=auto
kubectl -n kube-system rollout restart ds/cilium
kubectl -n kube-system rollout status ds/cilium --timeout=10m
# 必须没有输出：串接模式确实已清掉
kubectl -n kube-system get cm cilium-config -o jsonpath='{.data.cni-chaining-mode}{"\n"}'
```

> **⚠️ 第 6 步不能省。** 切换可能留下指向旧设备的 Cilium endpoint。旧 Pod 经新 conflist 正常拆除时，
> 会带走自己的 endpoint；未经这条路径拆除的旧 Pod 则会留下 endpoint。这类 endpoint 的宿主设备是
> 本项目创建的 `eni*` veth（已经不存在），而不是 netkit 的 `lxc*`。以下两种后果都不会报错：
>
> - 新 Pod 被分配到陈旧 endpoint 仍占用的地址时，`cilium-cni` 返回
>   `400 IP <addr> is already in use`，而 CNI ADD 没有降级路径。
> - 更隐蔽的情况是：地址被回收后分配给了**另一个节点**上的 Pod，而留有陈旧 endpoint 的节点
>   仍把它当作本地地址，于是发往该 Pod 的流量在这个节点被截住。
>
> 根因是地址的真实状态存在两份且从不对账：本项目的 IPAM 池认为该地址已释放，而 Cilium 的
> endpoint 管理器认为它仍被占用。
>
> **不要只按名字前缀删除。** 设备仍然存在的 `eni*` endpoint 属于仍在运行的 Pod：要么是滚动尚未
> 完成的 Pod，要么是第 5 步根本不会重启的 Pod（Job、裸 Pod 与静态 Pod）。本模式下 Pod 的流量
> 由 Cilium 的 endpoint 承载，删除它等于让该 Pod 断网。这些 Pod 需要手动重建，之后再执行一遍
> 列表命令。`eni` 是本插件宿主侧 veth 的默认前缀（`veth_prefix`）；若已修改，替换为实际配置的
> 前缀。检查在 agent 容器内执行，该容器与宿主机共享网络命名空间，镜像中带有 `sh` 与 `ip`。
> 删除命令是 `cilium-dbg endpoint disconnect <id>`；`cilium-dbg` 没有 `endpoint delete` 命令。

## 3. 功能矩阵

| | 串接模式（推荐） | **netkit 模式（本文）** |
|---|---|---|
| Pod 设备 | 本项目创建 veth 对 | **Cilium 创建**（netkit 或 veth） |
| Pod 地址 | 本项目分配 | 本项目分配（不变） |
| ENI 生命周期、温池、GC | 本项目 | 本项目（不变） |
| 宿主 per-ENI 策略路由 | 本项目 | 本项目（不变） |
| NetworkPolicy / Hubble | Cilium | Cilium |
| Service 转发 | Cilium（KPR 开时） | Cilium（KPR 必须开） |
| `hostPort` | ✅ | ✅ 但端口必须落在 NodePort 范围之外，见 §4.5 |
| conflist 里的 `ExtraRoutes` | ✅ 写进容器 | ❌ **无出口**，见 §4.2 |
| conflist 里的 `veth_prefix` | 可设置且生效 | ❌ 设置无意义 |
| 多网卡 Pod | ✅ | ❓ **未验证**；请仅使用单接口 |
| 生产就绪 | ✅ | ❌ **未验证** |

## 4. 配置参考

### 4.1 本项目的 values

| 配置项 | 取值 | 说明 |
| --- | --- | --- |
| `cni.mode` | `delegated` | 渲染 delegated 版 conflist：主插件是 `cilium-cni`，本项目落在它的 `ipam` 块里。要求 `cilium.enabled=true`，否则 chart `fail`。 |
| `cilium.enabled` | `true` | daemon 会**阻塞等待** `/var/run/cilium/cilium.sock` 之后才落盘新 conflist。Cilium 的 ADD 没有降级路径，没有 agent 时每个 Pod 都会失败。 |
| `networkPolicy.enabled` | 按需 | 仅「声明策略必须被执行」，且要求 `cilium.enabled=true` 同时为真。 |
| `zec.dataPath` | 不设 | 该键描述的是本项目自己创建的设备，而本模式下本项目不创建设备；schema 也只允许 `veth`。 |
| `zec.k8sServiceHost` / `zec.k8sServicePort` | 通常留空 | daemon 与 controlplane 用来替代集群内 Service 的 apiserver 入口。`cilium.enabled=true` 且留空时，在 install / upgrade 时从 `kube-public/cluster-info` 读取。在 `helm template` 等离线渲染下，或该 ConfigMap 不存在时，chart 拒绝渲染，直到设置了地址。端口需要与地址一起设置：只设置地址也能渲染，但端口会使用集群内 Service 的 443。其重要性见 [§4.3](#43-cilium-values) 的 `k8sServiceHost` 一行。 |

### 4.2 conflist 里的键

`ipam` 块里有两个由模板固定的行为键（第三个键 `debug` 跟随 `zec.logLevel`）：

| 键 | 取值 | 说明 |
| --- | --- | --- |
| `zec_mode` | `ipam-only` | 选择 IPAM-only 路径。**未知取值会被拒绝**，而不会回落到完整插件。拼错的值如果被忽略，节点会运行与配置不符的数据面。 |
| `install_host_routes` | `true` | 使 IPAM-only 路径安装该 Pod 的两条 ip rule 以及该 ENI 的默认路由。两条 rule 为：`from <podIP>` 查该 ENI 的表（prio 2048）；`to <podIP> lookup main`（prio 512，避免同节点发往该 Pod 的流量被前一条规则带走）。关闭该键后，位于辅助网卡上的 Pod 会从实例自身网卡发出，源地址属于另一个端口的 MAC，会被网络丢弃。缺省值为 `true`。 |

本项目不创建设备，以下描述 Pod 内网络的键随之失效：

| 键 | 本模式下 |
| --- | --- |
| `ExtraRoutes` | **不支持**。delegated IPAM 的返回值里 Routes 会被主 CNI 丢弃，需求只能改用 Cilium 侧的配置表达。 |
| `DefaultRoute` | 由 Cilium 决定，指向它自己的 `cilium_host`。 |
| `veth_prefix` | 无意义。本项目不创建设备，因此没有可命名的 host veth。 |
| `eniip_virtual_type` | 无意义，conflist 里不渲染。 |
| `mtu` | 由 Cilium 的 `MTU` / `RouteMTU` 决定。**两侧要一致**，否则出现 MTU 黑洞。 |

### 4.3 Cilium values

下表是本方案需要设置的全部 Cilium values。与[串接模式](deploy-cilium-chained-cn.md)的差异用 **★** 标出。

| 配置项 | 本文档取值 | 说明 |
| --- | --- | --- |
| ★ `bpf.datapathMode` | `netkit` | Pod 设备使用 netkit 而不是 veth。`netkit-l2` 对应 L2 模式，选择方法见 [§4.4](#44-netkit-l3-与-l2-的选择)。 |
| ★ `ipam.installUplinkRoutesForDelegatedIPAM` | `false` | **必须关闭**。这是 Cilium 自己安装宿主路由的路径，在本平台不可用：它要求上行网卡带 `IFF_SLAVE` 标志，而本平台的 ENI 不带该标志；此外，它在多 ENI 节点上只识别一张路由表，会导致第一块之后的每块 ENI 共用表号。宿主路由改由本项目按「每 ENI 一张表，表号 = 1000 + ifindex」安装。 |
| ★ `cni.chainingMode` | **不设** | 本模式不是串接，Cilium 是主 CNI。从串接模式残留的值会被静默接受（agent 照常运行），但此时配置所声明的模式与集群实际所处的模式不符。按 §2 第 1 步的变体将其清除，并确认 `cilium-config` 中的 `cni-chaining-mode` 为空。**注意：** `--reuse-values --set cni.chainingMode=null` **无法清除**，`--reset-then-reuse-values` 才可以。 |
| `cni.customConf` | `true` | conflist 由本项目写入，Cilium 不要自行渲染。 |
| `cni.exclusive` | `false` | 纵深防御。 |
| `cni.install` | `true` | 需要 `/opt/cni/bin/cilium-cni` 这个二进制。 |
| `routingMode` | `native` | Pod IP 是 VPC 原生可路由的 ENI 辅助 IP，不需要隧道。 |
| `ipam.mode` | `delegated-plugin` | 告诉 Cilium：地址由 CNI 二进制分配，Cilium 不要参与，也不要让 operator 划分 PodCIDR。 |
| `extraConfig.local-router-ipv4` | `169.254.32.1` | ⚠️ **不是 helm value**，写成 `--set localRouterIPv4=` 会被静默丢弃，随后 agent 启动失败。取值只需避开本项目给 Pod 的网关 `169.254.1.1`。 |
| `endpointRoutes.enabled` | `true` | 与 `local-router-ipv4` **必须同时开启**，否则 agent `fatal`。它还提供 main 表中到每个 Pod 的路由：本模式下没有 host veth，该路由正是由它提供，本项目不再安装；否则同一地址会有两条指向不同设备的路由。 |
| `endpointHealthChecking.enabled` | `false` | `delegated-plugin` 的强制伴随项。 |
| `envoyConfig.enabled` | `false` | 同上。代价是放弃 Ingress / Gateway API。 |
| `kubeProxyReplacement` | `true` | ⚠️ 与下一项**成对**启用，也是 netkit 的前提。 |
| `bpf.hostLegacyRouting` | `false` | ⚠️ 同上。单独设是无效的：agent 在 `kubeProxyReplacement` 不为 `true` 时会把它强制改回 `true`。 |
| `k8sServiceHost` | `auto` | ⚠️ **必须设置**。下线 kube-proxy 后，ClusterIP 只由 Cilium 实现。agent 若仍经 ClusterIP 访问 apiserver，节点整机重启后 agent 无法启动，ClusterIP 因此也无人实现，结果是 agent 与本插件的 daemon 一起 CrashLoop，报 `dial tcp <ClusterIP>:443: connect: network is unreachable`。`auto` 让 helm 在安装时从 `kube-public/cluster-info` 读出 apiserver 入口（即 kubeadm 的 `controlPlaneEndpoint`），并同时得出端口。需要注意以下三点：**① 该入口应是负载均衡 / VIP**，是单个 master 的 IP 则它成为单点；**② 找不到该 ConfigMap 时的表现因 Cilium 版本而异**：v1.16.0 ~ v1.16.2 渲染直接失败；**v1.16.3 ~ v1.18 helm 不报错**，而是把字面量 `auto` 当地址写进去；v1.19 起又改为渲染失败（`ConfigMap kube-public/cluster-info not found`）。非 kubeadm 集群改为显式 `--set k8sServiceHost=<入口地址> --set k8sServicePort=<端口>`。**入口是 IPv6 字面量（`https://[fd00::10]:6443`）时同样要显式设置**（`--set k8sServiceHost=fd00::10 --set k8sServicePort=6443`）：Cilium 的查找把第一个 `:` 之前的部分当主机，得到的是 `[fd00`。本 chart 自己为 `zec.k8sServiceHost` 做的查找能正确处理这种入口；**③ 只在 install / upgrade 时解析一次**，`helm template` 或离线渲染拿不到集群，表现同 ②；入口变更后需要重新执行 `helm upgrade`。**本插件的 daemon 与 controlplane 需要同一个入口，原因相同**：`cilium.enabled=true` 且 `zec.k8sServiceHost` 留空时，本 chart 在安装 / 升级时从同一个 ConfigMap 读取，结果打印在 release 的 NOTES 里；ConfigMap 不存在时，把 `zec.k8sServiceHost` / `zec.k8sServicePort` 设为该入口。 |
| `enableIPv4Masquerade` | `false` | ⚠️ masquerade 必须全关，见下。 |
| `enableIPv6Masquerade` | `false` | 同上。 |
| `bpf.masquerade` | `false` | 同上。 |
| `extraConfig.enable-session-affinity` | `true` | KPR 开启时 agent 会自动派生它，省略与否不影响 `sessionAffinity: ClientIP` 生效；显式写出可使配置在 KPR 被关闭后依然正确。 |
| `operator.replicas` | `2` | `node.cilium.io/agent-not-ready` 污点的摘除由 operator 独家负责，单副本没有冗余。 |

**⚠️ masquerade 必须全关。** Pod IP 在 VPC 内本身可路由，开启 masquerade 会把源地址改写成节点地址。
更重要的是：agent 启动时若发现启用了 **iptables 方式**的 masquerade，会把
`bpf.hostLegacyRouting` 强制改回 `true`，eBPF host-routing 随之失效。本模式的性能正是建立在
eBPF host-routing 之上，而不是建立在 netkit 设备本身。

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

**需要 L2 的具体场景，是会读取网卡 MAC 的工作负载。** KubeVirt 定义虚拟机时会把这个 MAC 交给
libvirt；L3 下该地址为空，virt-handler 直接失败并报
`XML error: unable to parse mac address ''`。其他会读取 MAC、或在 Pod 内做 ARP / DHCP 的负载，
会遇到同样的问题。

本文其余内容对两种模式完全相同，唯一的差别是一个取值：

```bash
  --set bpf.datapathMode=netkit-l2
```

- **L3 与 L2 之间的切换作用于整个集群，且必须重建 Pod**，与从 veth 切换过来相同（§1）。
  设备模式在 Pod 创建时即已固定。
- **凡属本插件职责的部分，两种模式等价**：per-ENI 规则与路由、地址分配与回收、多 ENI 出向、
  新挂 ENI 上的首个 Pod、NetworkPolicy 的执行、Service 的全部形态。两者的行为差异仅在于邻居解析：
  L3 下设备带 `NOARP`，两侧邻居表始终为空；L2 下宿主到 Pod 走 ARP/NDP 并留下动态 `REACHABLE`
  表项，Pod 对网关的请求由 Cilium 的 BPF 程序就地应答。
- **Cilium ≥ 1.20** 另有一个 `auto` 取值：探测内核，在 netkit 不可用时**静默回落到 veth**。
  本模式下不要使用它：静默回落会让集群运行在与配置不符的数据面上。

### 4.5 `hostPort` 不能落在 NodePort 范围内

`kubeProxyReplacement=true` 时，hostPort 由 Cilium 自己的 BPF 负载均衡承担，因此本模式的 conflist
不再串接 `portmap`。但 agent **会拒绝任何落在 NodePort 范围内的 hostPort**（默认
30000–32767），并输出以下日志：

```
The requested hostPort 31883 is colliding with the configured NodePort range [30000, 32767]. Ignoring.
```

**除此之外没有任何提示。** Pod 正常启动，`cilium-dbg status --verbose` 照样显示
`HostPort: Enabled`，只有 `cilium-dbg service list` 才能看出根本没有创建条目。本模式下没有其他实现 hostPort 的插件，因此被拒的端口在任何方向上都无人应答。

实测端口落在范围外（18081）时，hostPort 在五个方向上均可访问：Pod 所属节点、其他节点上的 Pod、
其他节点的 host netns、VPC 内的非集群主机，以及集群外。此时 `cilium-dbg service list` 中会有
四条 HostPort 条目：IPv4 与 IPv6 各两条（节点地址与通配）。

**因此，请将 hostPort 选在 30000 以下**，并验证其确实生效：

```bash
kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg service list | grep HostPort
```

输出为空即说明端口被拒绝，无论 `status --verbose` 显示什么。

### 4.6 IPv4 单栈与双栈

协议栈由参数决定，两者的差别仅在于下面两个键：

| 集群协议栈 | 需要追加的 Cilium values |
|---|---|
| **IPv4 单栈** | 无（上面的快速开始即为单栈配置） |
| **双栈** | `--set ipv6.enabled=true`<br>`--set extraConfig.local-router-ipv6=fe80::ffff:1` |

- 集群本身必须是双栈（Node 对象带 IPv6），否则添加这两个键没有意义。
- `local-router-ipv6` 的取值必须避开 `fe80::1`，该地址是本项目给 Pod 的 IPv6 网关。
- 在双栈集群上**遗漏**这两个键，Pod 的 IPv6 会被静默丢弃，且没有任何错误日志。
- **双栈集群的新节点需要先具备 IPv6，Cilium 才能在其上启动。** 见
  [向双栈集群加入新节点](#向双栈集群加入新节点)。

#### 向双栈集群加入新节点

节点上的 Cilium 从 Node 对象读取节点的 IPv6，而 Node 对象上的地址来自 kubelet 的 `--node-ip`，
该参数只能由节点初始化设置。有两种做法，推荐第一种。

**A. 加入前就有 IPv6（推荐）**。给实例主网卡开启 IPv6（最好在创建实例时开启），然后在新节点上执行下面的命令。
其中的 reconfigure 步骤仅在网卡尚无全局 IPv6 时执行，即开机之后才开启 IPv6、且开机时的 DHCPv6 请求已经结束的情况：

```sh
IFACE=eth0   # 承载节点 IP 的网卡
# 仅当它还没有全局 IPv6 时执行（开机后才开启 IPv6，或做法 B）。示例为 systemd-networkd；
# 用其他网络管理工具时，改为续租该网卡的 DHCPv6。
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
kubeadm join ...   # 仅当上面打印出了 KUBELET_EXTRA_ARGS 一行；
                   # 由 kubeadm token create --print-join-command 生成
```

预期：Node 注册时就带两个地址，Cilium 一次启动成功，该节点的 ZecNode 达到
`InstanceVerified=True`（仅 `zec.ipamType=crd`），节点变为 `Ready`。

**B. 由 controlplane 开启 IPv6**。仅在以下条件全部满足时可用，否则使用 A：

- `zec.ipamType=crd` 且 `zec.ipStack=dual`。
- 节点主网卡所在子网在 `zec.subnetIDs` 中；不在其中的主网卡不服务 Pod，插件也不会为它开启 IPv6。
- release 的 NOTES 中打印的是 `APISERVER:` 一行（可用 `helm get notes zenlayer-vpc-cni -n kube-system`
  重新查看）。本 chart 拿不到入口时会拒绝渲染；若该处是 `WARNING`，说明是由早先的 chart 安装的：其 daemon
  会回落到集群内 Service，而在 Cilium 启动之前该节点上没有任何组件提供该 Service，因此 daemon 永远无法注册。

controlplane 由平台运行时，上面关于 `zec.ipStack` 与 Pod 子网的条件，指的是平台侧的配置，而不是本 chart 的 values。

不开启 IPv6，直接加入。节点的 daemon 完成注册后，controlplane 校验节点，并自行为主网卡分配 IPv6
前缀（实测在 daemon 启动后约 20 秒内完成）。在宿主机获得该地址之前，该节点上的 Cilium 会以
`node <name> does not have an IPv6 address` 反复重启，节点保持 `NotReady`，这是预期行为。
本插件的 daemon 等待 Cilium agent 超过 `cilium.waitTimeout`（默认 5m）后会重启，这同样是预期行为；
若下面的手工步骤耗时更长，请调大该值。

```sh
# 在控制面节点上：等待前缀出现
kubectl get zn <node> -o jsonpath='{.status.enis[?(@.primary==true)].ipv6.primary}{"\n"}'

# 出现后，在新节点上执行（实测 `networkctl renew` 拿不到新地址）
IFACE=eth0   # 承载节点 IP 的网卡
# 仅当它还没有全局 IPv6 时执行（开机后才开启 IPv6，或做法 B）。示例为 systemd-networkd；
# 用其他网络管理工具时，改为续租该网卡的 DHCPv6。
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
systemctl restart kubelet       # 仅当上面打印出了 KUBELET_EXTRA_ARGS 一行

# 在控制面节点上：重启该节点的 Cilium，不必等它的退避
kubectl -n kube-system delete pod -l k8s-app=cilium --field-selector spec.nodeName=<node>
```

直连 apiserver 的入口见 [§4.1](#41-本项目的-values) 中 `zec.k8sServiceHost` 一行。
上文的 `eth0` 指承载节点 IP 的网卡。本插件不配置宿主机网卡，也不修改 kubelet：二者属于节点初始化的范畴。

## 5. 回退到 veth 模式

```bash
# ① 先把本项目切回 chained
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.0 \
  -n kube-system --reset-then-reuse-values --set cni.mode=chained
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

# ④ 清掉 netkit 设备留下的 endpoint —— 与 §2 第 6 步方向相反。
#   先等 ③ 重启的每个工作负载完成
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout status "$t/$n" --timeout=10m; done
done
while kubectl get pod -A --no-headers | grep -q Terminating; do sleep 5; done   # 旧 Pod 也要退完
# 然后只列出 netkit 设备（lxc<十六进制>）已经不存在的 endpoint（lxc_health 不在其中）
kubectl -n kube-system get pod -l k8s-app=cilium -o name | while read -r p; do
  echo "== ${p#pod/}"
  kubectl -n kube-system exec "${p#pod/}" -c cilium-agent -- sh -c '
    cilium-dbg endpoint list -o jsonpath="{range [*]}{.id} {.status.networking.interface-name}{\"\n\"}{end}" |
    while read -r id ifn; do
      case "$ifn" in lxc[0-9a-f]*) ip link show "$ifn" >/dev/null 2>&1 || echo "$id $ifn";; esac
    done'
done
# 在列出它的那个节点上逐个删除：
#   kubectl -n kube-system exec <cilium-pod> -c cilium-agent -- cilium-dbg endpoint disconnect <id>
```

第 ④ 步不能省略，原因见 §2 下方的警告：netkit 的 endpoint 可能比其 Pod 存活更久。反方向同样需要注意：
设备仍然存在的 `lxc*` endpoint 属于仍在运行的 Pod（③ 不会重启的 Job、裸 Pod 或静态 Pod），
应重建该 Pod，而不是删除其 endpoint。

**这样回退后 `kubeProxyReplacement=true` 保持不变，kube-proxy 仍处于下线状态。** 这也是串接模式的
默认形态（Service 继续由 Cilium 的 eBPF 承载，`k8sServiceHost` 也按该形态的要求保持设置），
因此 ② 不改动这些值。恢复 kube-proxy 属于彻底移除 Cilium 的步骤：见串接模式回退的第 ⓪ 步
（若 DaemonSet 已被删除，用第 3 步备份的 `kube-proxy-backup.yaml` 恢复）。

这样回到的是**串接模式的 veth**。若要继续回到不安装 Cilium 的纯 veth，以及清理卸载后的残留
（cgroup 上的 socket-LB 程序、孤儿 CiliumEndpoint），见
[deploy-cilium-chained-cn.md](deploy-cilium-chained-cn.md) 的回退一节。
