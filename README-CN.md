# Zenlayer VPC CNI for Kubernetes

[English](README.md) | 简体中文

一个为 Kubernetes Pod 提供 Zenlayer VPC 原生地址的 CNI 插件。Pod IP 是挂载在节点上的
Zenlayer 弹性网卡（ENI）的辅助 IP，因此 Pod 流量由 VPC 自身路由：没有 overlay，没有封装，
并且 Pod 地址在 VPC 内的任意位置均可达。

当前状态：插件支持 `linux/amd64` 节点上的 `eni_multi_ip` 网络模式。每个节点仅提供 IPv4
或提供双栈，参见[地址族](#地址族)。

**本文描述的是集中式部署（`zec.ipamType=crd`）**：只有 controlplane 持有 Zenlayer OpenAPI 凭证并
调用 OpenAPI；节点不持有凭证，各自在自身的 `ZecNode` 对象中声明所需的地址数量。
如需每个节点直接调用 OpenAPI 的 direct 模式（`zec.ipamType=direct`），参见
[docs/deploy-direct-cn.md](docs/deploy-direct-cn.md)。`zec.ipamType` 没有默认值，安装时必须显式传入。

## 能力矩阵

| 能力 | 状态 |
| --- | --- |
| IPAM 模式 | `eni_multi_ip`：Pod IP 是 VPC 弹性网卡上的一个辅助 IP |
| datapath | veth pair + per-ENI 策略路由。`delegated` 模式下 Pod 设备由 Cilium 创建 |
| 地址族 | IPv4 或双栈。IPv6 单栈未实现 |
| 节点平台 | 仅 `linux/amd64` |
| 部署模式 | 集中式（`zec.ipamType=crd`，本文）：只有 controlplane 持有 OpenAPI 凭证。direct 模式（每个节点各自持有凭证）见 [docs/deploy-direct-cn.md](docs/deploy-direct-cn.md) |
| Kubernetes | 集中式部署 >= 1.30（需要 ValidatingAdmissionPolicy）；direct 模式 >= 1.28.2（chart 的 `kubeVersion` 约束） |
| Service 转发 | 由 kube-proxy 或用户自行部署的替代组件提供。本插件不提供 |
| NetworkPolicy | 默认不执行。可通过串接上游 Cilium 获得，参见[网络策略](#网络策略) |
| Cilium（串接） | 推荐 v1.15 ~ v1.18（Cilium 自身 chart 的 `k8sServiceHost=auto` 需要 v1.16 及以上，见 [docs/deploy-cilium-chained-cn.md](docs/deploy-cilium-chained-cn.md)）；按 v1.17.5 源码核对，并在 v1.17.5 集群上验证 |
| SNAT / iptables 规则 | 无。Pod 需要公网 IPv4 出向时，需在 VPC 侧配置 NAT 网关 |
| Pod 带宽限速 | 未实现。出向整形可通过 Cilium 的 bandwidth manager 获得 |
| `hostPort` | 支持，由持有数据面的那一方实现。不启用 Cilium 时 conflist 串接上游 `portmap` 插件（二进制随镜像分发）；`cilium.enabled=true` 时由 Cilium 自身的 BPF 负载均衡承担，**conflist 不再串接 `portmap`**。这仅在 Cilium 以 `kubeProxyReplacement=true`（推荐值）运行、或设置了 `enable-host-port` 时成立，否则没有任何组件处理 hostPort。此时**端口须选在 30000 以下**：Cilium 会拒绝任何落在 NodePort 范围内的 hostPort，除一条 warning 外没有任何提示，参见 [docs/deploy-cilium-netkit-cn.md](docs/deploy-cilium-netkit-cn.md) §4.5。由于 Pod IP 本身在 VPC 内可路由，多数场景下直接使用 Pod IP 或 Service 更为合适：hostPort 会将 Pod 固定在特定节点，并在同一节点上产生端口冲突 |
| 每个 VPC 的 Pod 子网数 | 最多 5 个 |

## 目录

- [能力矩阵](#能力矩阵)
- [快速开始](#快速开始)
- [架构](#架构)
- [datapath](#datapath)
- [地址族](#地址族)
- [网络策略](#网络策略)
- [环境要求](#环境要求)
- [安装](#安装)
- [容量规划](#容量规划)
- [配置参考](#配置参考)
- [卸载](#卸载)
- [从源码构建](#从源码构建)
- [延伸文档](#延伸文档)
- [故障排查](#故障排查)
- [贡献](#贡献)
- [安全](#安全)
- [变更记录](#变更记录)
- [许可证](#许可证)

## 快速开始

在已有的 Zenlayer VPC 实例集群（Kubernetes >= 1.30）上，只需三步。每一步的具体作用参见
[安装](#安装)，前置条件参见[环境要求](#环境要求)。

```sh
# 1. 凭证。刻意不由 chart 渲染，因此它们永远不会进入 values.yaml、git 或 Helm release。
#    只有 controlplane 挂载它，节点上没有。
kubectl create secret -n kube-system generic zenlayer-cni-access-key \
  --from-literal=AccessKeyID='<access-key-id>' \
  --from-literal=AccessKeyPassword='<access-key-secret>'

# 2. 安装。下面每个 --set 都是必填的，缺少任意一个都会导致渲染失败。
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

# 3. 等待 controlplane 与 DaemonSet 就绪、每个节点的 ZecNode 为 Ready，然后确认 Pod 确实拿到了 VPC 地址。
kubectl -n kube-system rollout status deploy/zenlayer-vpc-cni-controlplane
kubectl -n kube-system rollout status ds/zenlayer-vpc-cni --timeout=10m
kubectl get zn
kubectl run nettest --image=busybox --restart=Never -- sleep 3600
kubectl wait --for=condition=Ready pod/nettest --timeout=60s
kubectl get pod nettest -o jsonpath='{.status.podIP}{"\n"}'
kubectl delete pod nettest
```

Pod IP 应为所传入的某个子网中的地址。

以下默认值值得留意：每个节点 2 个 IP 的 warm pool（`controlplane.scaler`）；controlplane 一个副本，
存在控制面节点时优先调度到控制面节点上（设置 `controlplane.deployment.nodeSelector` 后变为强制），
其所在节点需要公网出向。
由哪个组件创建 Pod 设备取决于 `cni.mode`，参见[datapath](#datapath)。

## 架构

本仓库会构建出三个二进制：

| 二进制 | 源码目录 | 职责 |
| --- | --- | --- |
| `zec-cni` | `cmd/zenlayer-vpc-cni` | kubelet 在 Pod sandbox 创建/销毁时执行的 CNI 插件。它自身不做任何 IPAM：通过 gRPC 连接节点上的 daemon，然后配置 Pod 的 netns。 |
| `zec-ipam` | `cmd/cni-ipam` | 每节点一个的 daemon（以 DaemonSet 运行，`hostNetwork: true`）。它维护 warm pool、提供 gRPC API，并把本节点需要多少地址、要归还哪些地址写进自己的 `ZecNode`。它不持有凭证，也不调用 OpenAPI。 |
| `zec-controlplane` | `cmd/zec-controlplane` | 唯一持有 OpenAPI 凭证的组件（Deployment，`hostNetwork: true`，Lease 选主）。它核验每个节点对应的实例，按 `ZecNode` 里的需求建网卡、分配和回收辅助 IP，并把网卡、地址与容量写回 `ZecNode` 的 status。 |

`ZecNode`（`zecnodes.network.zenlayer.com`，简写 `zn`）是集群级对象，每个 Node 对应一个：spec 只由该
节点的 daemon 写入，status 只由 controlplane 写入。准入策略依据 daemon 的 ServiceAccount token 中的节点名，
限定每个节点只能修改自身对应的对象。`kubectl get zn` 是查看各节点地址状态的入口。

`zec-cni` 与 daemon 通过 `/var/run/zenlayer/cni.socket` 上的 Unix domain socket（gRPC）通信。插件所需的全部信息
（分配到的 IP、该 IP 所属的 ENI、网关、service CIDR）均在 `AllocIP` 的响应中返回。

daemon 还会：

- 从 `kube-system` 命名空间下的 `zenlayer-cni-config` ConfigMap（键为 `eni_conf`）读取自身配置；
  网卡的网关、CIDR 等从实例元数据（link-local，无需凭证）读取；
- 将资源状态持久化到 `/var/lib/cni` 下的 BoltDB 中；
- 注册一个 kubelet device plugin，把每节点的可分配地址数上报为扩展资源 `zenlayer.com/eni-ip`
  （由 `eni_conf` 中的 `disableDevicePlugin` 关闭，chart 不渲染该字段，参见
  [配置参考](#配置参考)）。
  **该上报值只对在 `resources.requests` 中显式声明了 `zenlayer.com/eni-ip` 的工作负载起调度约束**，
  详见[容量规划](#容量规划)；
- 在节点自身的网络命名空间中，于 `healthAndDebugPort`（默认 `11414`）上提供 `/healthz`、
  `/metrics`（Prometheus）。

一个 init 容器（`script/bootstrap/install_env.sh`）负责把 `zec-cni` 与上游的 `loopback`
插件安装到宿主机上。conflist 不由 init 容器安装：该文件由 daemon 在开始服务之后写入，
因此 kubelet 不会在 daemon 能够应答之前调用插件。daemon 须等待 controlplane 核验本节点的实例
（`ZecNode` 的 `InstanceVerified=True`）并组装好 warm pool 之后，才开始服务。

daemon 每隔 `reconcileIntervalSec` 执行一次 **GC**：将每个 warm pool 与 Pod 实际持有的资源进行比对，
释放已经泄漏的部分。当 `ZecNode` 的 status 不再列出 daemon 持有的某个地址或网卡，或将某块网卡
报告为 `Excluded` 时，GC 会在约一分钟内提前执行。该操作有三个
名称，此处统一说明：配置项称为 `reconcileIntervalSec`，日志中称为 `Daemon GC`，本文档一律称为 GC。
启动后的首轮会被跳过，
重复触发经防抖后每分钟至多执行一次。

Pod 已消失的记录**不会在发现时立即回收**：第一轮只打标，满一小时后才连同指向该地址的策略路由
规则一并删除，因此地址会在 daemon 首次发现之后的 **1 小时 ~ 约 1 个半小时**内回池（满 1 小时后，最多再等待一个 GC 周期，即 `reconcileIntervalSec`（默认 20 分钟），再加上 20% 的抖动）。该计时
为内存态，随 daemon 重启重新开始，因此重启频率高于该时长的 daemon 不会执行到回收步骤。
受影响的是容量，而非连通性。

daemon 的 GC 只与 `ZecNode` 比对，路径上没有 OpenAPI 调用；与平台的对账（收养平台上已有的地址、
清扫已脱离实例的孤儿网卡）由 controlplane 执行。controlplane 每 5 分钟执行一次全量对账；孤儿网卡清扫
在 leader 上任时执行一次，之后约每小时执行一次。

IP 分配遵循 warm pool 策略：daemon 保持 `controlplane.scaler.target` 个空闲 IP 处于就绪状态，
因此创建 Pod 通常无需等待一次 controlplane 往返。`controlplane.scaler.targetMin` 是池的下限，
约束的是池的**总量**（含已被 Pod 占用的地址），而非空闲部分。池的上限是该节点的容量
（`ZecNode` 的 `status.capacity.allocatable`），没有单独的上限配置。controlplane 将这两个值写入
每个节点的 `ZecNode`，修改后无需重启 daemon；参见[配置参考](#配置参考)。

controlplane 不可用时，已有 Pod 不受影响；只要池中还有空闲地址，新 Pod 即可启动。扩容会等待，
待归还的地址积压在 `ZecNode` 的 `spec.ipv4.releasing` 中，暂不归还，但不会泄漏。

Service 流量**不**由本插件处理。本插件不包含 service 负载均衡器，也不包含 NetworkPolicy 实现；
Service 由 kube-proxy（或用户自行安装的替代组件）处理。

## datapath

由哪个组件创建 Pod 设备，取决于 `cni.mode`：

- **`chained`（推荐）**：由本插件创建 veth pair（`pkg/plugin/datapath/policy_route.go`）。
  Pod 直接抵达宿主机协议栈，因此 kube-proxy 无需额外配合就能看到 service 流量的**两个方向**，
  这也正是 Cilium `generic-veth` CNI chaining 所要求的设备形态。
- **`delegated`**：Cilium 作为主 CNI 创建 Pod 设备，根据其自身的 `bpf.datapathMode`
  取值，可能是 netkit 设备，也可能是 veth。本插件在该模式下不创建任何设备，
  参见 [docs/deploy-cilium-netkit-cn.md](docs/deploy-cilium-netkit-cn.md)。

**per-ENI 策略路由在两种模式下完全相同，且始终由本插件安装。** 每个 ENI 拥有自己的路由表
（`1000 + ifindex`），其中保存该 ENI 的默认路由，并由一条 `from <pod-ip> lookup <eni-table>`
规则把 Pod 的出向流量导入该表。

`zec.dataPath` 渲染进 conflist 的 `eniip_virtual_type` 字段，**只接受 `veth`**。
它描述的是本插件自己创建的设备，因此在 `cni.mode=delegated` 下不渲染。

**本插件不做地址伪装（SNAT），也不安装任何 iptables 规则。** Pod 地址是 ENI 的辅助 IP，
在 VPC 内可直接路由，东西向流量以 Pod 自身地址收发，不经过任何转换。因此
**Pod 访问公网 IPv4 依赖 VPC 侧的 NAT 网关**：需要为 Pod 地址所属的子网配置 NAT 网关，
否则 Pod 没有公网 IPv4 出口。公网 IPv6 采用另一条路径：由平台直接为网卡分配公网地址，
参见[地址族](#地址族)。

**本插件不实现 Pod 带宽限速。** conflist 不声明标准 CNI `bandwidth` runtime capability：
本插件不消费 `runtimeConfig.bandwidth`，声明该 capability 只会使 kubelet 将带宽注解交给一个不予处理的
插件，导致 Pod 实际不受限，而 conflist 却宣称支持。

出向限速可以由 Cilium 提供（需启用，参见[网络策略](#网络策略)）：其 bandwidth manager 从
Pod 的 `kubernetes.io/egress-bandwidth` 注解取值，使用 EDT 进行整形，通过 Cilium 自身 values 中的
`bandwidthManager.enabled=true` 开启。依赖该功能之前，需要了解以下三点：

- **仅支持出向。** Cilium 官方文档写明 `kubernetes.io/ingress-bandwidth`
  不支持、不推荐、**会被忽略**。
- **IP 位于辅助 ENI 上的 Pod 可能不受限。** Cilium 依据「持有默认路由，或被赋予节点
  `InternalIP` / `ExternalIP`」来选择安装 `fq` 的设备，这描述的是主 ENI；而 IP 位于辅助
  ENI 上的 Pod 使用该 ENI 自身的路由表，两个条件均不满足。可使用
  `cilium-dbg status | grep BandwidthManager` 查看实际生效的设备列表。
- **由 Pod 注解驱动，与 CNI 链无关，且不需要任何 CRD。** Cilium 的 bandwidth manager 从不读取
  `runtimeConfig`，因此在 conflist 中重新声明该 capability 既不是其前提，也对其没有帮助；
  也不存在带宽相关的 CRD，只有注解（`kubernetes.io/egress-bandwidth`，以及用于优先级分级的
  `bandwidth.cilium.io/priority`）。另一个好处是：Cilium 的 pod watcher 会响应注解变化，
  因此修改运行中 Pod 的注解会**立即生效，无需重建**。

本 chart **不提供**自身的带宽开关：`bandwidthManager.enabled` 属于 Cilium 的 release，
本 chart 既无法读取也无法修改；此处的开关只能声明功能已启用，无法校验其是否实际生效。

验证步骤与其余上游限制参见 [docs/deploy-cilium-chained-cn.md](./docs/deploy-cilium-chained-cn.md)。

## 地址族

`zec.ipStack` 决定一个节点提供哪些地址族：

| 取值 | 含义 |
| --- | --- |
| `ipv4` | Pod 获得一个 IPv4 地址。 |
| `dual` | Pod 获得一个 IPv4 地址和一个 IPv6 地址。 |
| `ipv6` | 尚未实现。 |

启用 `dual` 之前，需要了解以下四项性质。

**每个配置的子网都必须同时具备 IPv4 和 IPv6 CIDR。** 任一子网缺少某个地址族时，controlplane 会停留在
`waiting for the pod subnets` 状态、不开始服务（`/healthz` 返回 503），并在日志中指明哪个子网缺少哪个地址族。**该检查刻意放在启动时而非分配时**：如果分配时只发出
一条 warning，将无法与「平台变慢」区分，症状会是 warm pool 永远无法填满、所有 Pod 卡在
`ContainerCreating`，而唯一的线索只有那一行 warning。

**一对地址不可拆分。** Pod 的 IPv4 与 IPv6 地址作为一个整体交付；只覆盖单个地址族的资源永远不会
交给 Pod，而是被打标并回收。这保证了平台部分失败之后，Pod 不会带着半套地址启动。

**IPv6 不构成节点容量约束。** 平台为每块 ENI 下发一个 `/96` 前缀，节点上的 daemon 在本地从中切出
具体地址，不存在按地址申请的 IPv6 接口。一个 `/96` 能容纳的地址数远超任何 ENI 的服务
能力，因此
`zenlayer.com/eni-ip` 与 warm pool 仍然只按 IPv4 计算容量，两种模式下节点上报的容量相同。
参见[容量规划](#容量规划)。

**公网 IPv6 是计费项，必须声明计费方式。** 当子网的 `ipv6Type` 为 `Public` 时，如果未提供计费类型和带宽上限，平台
将拒绝为网卡启用 IPv6，而为自建网卡启用 IPv6 的正是本插件。
因此 `zec.ipv6InternetChargeType` 与 `zec.ipv6Bandwidth` 决定了集群的计费方式。在私网 IPv6
的子网上，这两项会被忽略。`ByTrafficPackage` 另需 `zec.ipv6PackageSize`，`BandwidthCluster`
另需 `zec.ipv6ClusterId`；两者都由 chart 在渲染时、controlplane 在启动时各校验一次。
参见[配置参考](#配置参考)。

由于 IPv6 地址是从网卡前缀中本地切分的，而非逐个向平台申请，IPv6 的分配状态由 daemon 自身
维护，依据为本地记录与 API Server 上报的 Pod 地址。IPv4 则额外由 controlplane 与平台对账。

本插件的双栈支持**不要求** Kubernetes 集群本身为双栈；在此基础上叠加 Cilium 串接才会引入该项
要求，参见[网络策略](#网络策略)与
[docs/deploy-cilium-chained-cn.md](docs/deploy-cilium-chained-cn.md) §4.3。

## 网络策略

**默认不执行 NetworkPolicy。** 本项目不自带策略引擎，因此 NetworkPolicy 对象会被 apiserver
正常接收，随后被忽略：本应阻断流量的规则只会静默地不生效，不会有任何组件报错。

`cilium.enabled=true` 通过 Cilium 的 `generic-veth` chaining 模式，将**社区原版 Cilium**
串接在本插件之后；`networkPolicy.enabled=true` 则声明「策略必须被执行」。provider 为 `cilium`
时后者要求前者，否则 chart **直接拒绝渲染**，而不是接受一批无人执行的策略对象。两个开关相互
独立：只需要 Hubble 可观测性而不需要策略执行时，仅开启 `cilium.enabled` 即可。

在 `cni.mode=chained` 下，veth pair 与 per-ENI 策略路由**完全不变**，Cilium 只增加 endpoint
注册和宿主侧 veth 上的 tc eBPF 策略执行；在 `cni.mode=delegated` 下，设备由 Cilium 自身创建，
策略也在该设备上执行，而 per-ENI 路由仍由本插件负责。Cilium 不接管 IPAM，
也不接管路由：chainer 硬编码了 `ExternalIpam: true` 与 `RequireRouting: false`。

本项目**不 fork、不打补丁、不 vendor** Cilium，chart 也不安装 Cilium：请使用 Cilium 自身的官方
chart 部署。支持范围为 **v1.15 ~ v1.18**，已按 v1.17.5 进行代码核对；Cilium 自身 chart 的 `k8sServiceHost=auto`
需要 v1.16 及以上（参见 [docs/deploy-cilium-chained-cn.md](docs/deploy-cilium-chained-cn.md)）。

启用前必须了解以下五点：

- **`cni.customConf=true` 是强制项，建议同时设置 `cni.exclusive=false`。** 前者是
  `generic-veth` 的硬性前提：Cilium **没有 `generic-veth` 的 conflist 模板**，因此
  `customConf=false` 与其自相矛盾，**Cilium 自身会阻止该配置**：agent 会停留在 `0/1 Running`、
  永不 Ready（日志持续输出 `invalid CNI chaining mode: generic-veth`），而 **Pod 网络与
  NetworkPolicy 照常工作**。配置错误不会导致静默降级、丢失策略或 Pod 中断，只会使 agent 永久显示为未就绪。
  后者在本模式下是冗余的（`customConf=true` 时该键根本不会进入 configmap），但换用 Cilium 能够
  渲染的串接模式时会重新生效，同时设置没有任何代价。
- **修改 `cni.*` 或 `extraConfig.*` 之后必须显式重启 cilium agent。** 这些键只写入 ConfigMap，
  不修改 Pod template，`helm upgrade` 不会滚动更新 DaemonSet，而 `kubectl rollout status ds/cilium`
  会**立即返回成功（假绿）**。请使用 `kubectl -n kube-system rollout restart daemonset/cilium`。
- **L7（HTTP）与 FQDN（`toFQDNs`）策略在 Cilium 默认的 `l7Proxy=true` 下可用；透明加密
  （IPsec / WireGuard）与 Cilium Ingress / Gateway API 在该模式下不可用。** 不要关闭 `l7Proxy`：
  **关闭后，L7 与 FQDN 策略的失效方式是「放行」**：API Server 接受策略对象，agent 不执行其 L7
  部分，本应被拒绝的流量全部通过；仅使用 `toFQDNs` 的策略甚至会被标记为 `Valid=True`。详见
  [docs/deploy-cilium-chained-cn.md](docs/deploy-cilium-chained-cn.md) §4.2 的 `l7Proxy` 行。

- **开关名拼写错误会被拒绝，而不会静默地处于未开启状态。** `values.schema.json` 在根层与各段都设置了
  `additionalProperties: false`，因此 `--set celium.enabled=true`（将 `cilium` 拼错一个
  字母）会在渲染阶段失败，并指出出错的键：`additional properties 'celium' not allowed`。

- **`zec.ipStack=dual` 叠加串接时，要求 Kubernetes 集群本身为双栈。** 此时 Cilium 必须以
  `ipv6.enabled=true` 部署，而这又要求每个 Node 对象都带有 IPv6 地址。这是集群级前提，
  任何 Cilium value 都无法替代。两部分缺一不可：不开启 `ipv6.enabled=true` 时，agent 的 per-veth BPF
  会将所有 IPv6 报文当作不支持的 L3 协议丢弃，而且是**静默**丢弃（agent 报告健康、每个
  endpoint 都列出正确的 IPv6 地址、info 及以上级别没有任何日志）；在单栈集群上开启它，则 agent
  拒绝启动。本插件自身的双栈支持不依赖集群为双栈，该要求只由叠加串接引入。详见
  [docs/deploy-cilium-chained-cn.md](docs/deploy-cilium-chained-cn.md) §4.3。

完整步骤、每一项必填 values 的理由、验证方法，以及**两个方向的模式切换流程**（纯 veth ⇄ 串接）
参见 [docs/deploy-cilium-chained-cn.md](./docs/deploy-cilium-chained-cn.md)。

**卸载 Cilium 之前，请先阅读该文档中的模式切换流程一节：顺序是固定的，滚动重启工作负载是必需步骤。** Pod veth 上的
tc/tcx BPF 程序是 BPF link，**不会随 agent 卸载而消失，`helm uninstall` 也不会清理**。agent 运行期间，
会持续维护这些程序所依赖的负载均衡 map；**一旦卸载，map 即被冻结**：串接期间创建的 Pod 会继续
使用一份永不更新的表来解析 ClusterIP。此后该 Service 的后端一旦变动，这些 Pod 发往已下线后端的
连接即被丢弃，且 `KUBE-SVC-*` 计数保持为 0，沿 kube-proxy 方向排查不会有任何发现。

## 环境要求

- Kubernetes >= 1.30。集中式部署使用 ValidatingAdmissionPolicy 将每个节点限定在自身的 `ZecNode` 上，
  集群不提供该 API 时 chart 拒绝渲染。（direct 模式只需 >= 1.28.2，参见
  [docs/deploy-direct-cn.md](docs/deploy-direct-cn.md)。）
- `amd64` 架构的 Linux 节点。DaemonSet 对 `kubernetes.io/os=linux` 和
  `kubernetes.io/arch=amd64` 设置了硬性 node affinity，镜像构建也会拒绝其他 `GOARCH`。
- 节点为 Zenlayer VPC 实例，具备一个 VPC、该 VPC 中最多 5 个用于分配 Pod IP 的子网，
  以及一个供 controlplane 创建 ENI 时使用的安全组。
- Zenlayer OpenAPI 静态凭证（access key ID + password），并具有管理 ENI 及其辅助 IP 的权限。
  凭证只挂载给 controlplane。
- **运行 controlplane 的节点**具备**公网出向**能力：节点绑定 EIP，或其所属 VPC 配置 NAT 网关。
  Zenlayer OpenAPI 是公网端点，没有内网端点，且只有 controlplane 调用它。controlplane 优先调度到
  控制面节点；如需将其固定在具备出向能力的节点上，请设置 `controlplane.deployment.nodeSelector`。
  其余节点不需要公网出向。
  这与下文的 Pod 出向要求不同：后者只限制 Pod 能访问的目标。
- Helm 3.14 及以上。本文的命令用到 `oci://`（3.8）、`--set-json`（3.10），升级用到
  `--reset-then-reuse-values`（3.14）；此外，3.14 之前的 `helm template` 会使用内置的、低于本 chart 要求的
  Kubernetes 版本来核对 `kubeVersion`。
- 当 `cni.mode=delegated` 且 Cilium 使用 `bpf.datapathMode=netkit` 或 `netkit-l2` 时，
  每个节点的 Linux 内核须 >= 6.8：Cilium 文档规定 netkit 从内核 6.8 起可用。
  `chained` 模式不额外引入内核要求。
- kube-proxy，或其他提供 Service 处理能力的组件。在 `cni.mode=delegated` 下该组件即为
  Cilium，其 `kubeProxyReplacement` 在该模式下是必选项。
- 若 Pod 需要访问公网 IPv4：为 Pod 地址所属的子网配置 NAT 网关。本插件不做 SNAT，
  参见[datapath](#datapath)。

## 安装

### 1. 创建凭证 Secret

凭证刻意**不**由 chart 渲染，因此它们永远不会落到 `values.yaml`、git 或 Helm release 中。
请在 `kube-system` 命名空间中单独创建该 Secret：

```sh
kubectl create secret -n kube-system generic zenlayer-cni-access-key \
  --from-literal=AccessKeyID='<access-key-id>' \
  --from-literal=AccessKeyPassword='<access-key-secret>'
```

**只有 controlplane 挂载该 Secret**（只读，按文件读取 `AccessKeyID`、`AccessKeyPassword` 两个键，
每 60 秒重新读取一次，轮换凭证无需重启）；节点上的 daemon 不挂载任何凭证。如果 Secret 使用了
其他名称，请通过 `--set zec.secretName=<name>` 传入。

controlplane 也接受放在同一目录中的临时（STS）凭证：键为 `AccessKeyID`、`AccessKeySecret`、
`SecurityToken` 与 `Expiration`（RFC 3339 格式的时间）。存在 `SecurityToken` 键即按此形式读取，
此时忽略 `AccessKeyPassword`。负责续期的一方须在 `Expiration` 之前改写这些文件，controlplane
在下一次读取时生效。已过期且未续期的凭证仍会继续使用，并输出错误日志。

### 2. 安装 chart

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

> **三个 ID 必须使用 `--set-string`。** `--set` 会推断类型，纯数字的 ID 会被解析为
> int64，并被 schema 以 `got number, want string` 拒绝。Zenlayer 的 ID 全部为纯数字，
> 因此首次安装即会遇到该问题。

- `zec.ipamType=crd` 选择集中式部署；`controlplane.deployment.enabled=true` 使 chart 在本集群内
  运行 controlplane，此时 `controlplane.regionID`（`zec.vpcID` 所在的地域）、`zec.vpcID`、
  `zec.subnetIDs`、`zec.securityGroupID` 为必填项，缺少任一项都会导致渲染失败。这些值用于配置
  controlplane，不会下发到节点。
- controlplane 由平台托管、运行在集群之外时，改为传入 `--set controlplane.platformUser=<user>`
  （平台 controlplane 访问本集群时用于认证的用户名，chart 为其授权），不传入
  `controlplane.deployment.enabled`，也不需要第 1 步的 Secret 以及上文的 VPC / 子网 / 安全组 / 地域：
  这些由平台侧配置。两者必须且只能设置一个，否则 chart 拒绝渲染。
- 行为开关 `zec.ipamType`、`zec.ipStack`、`zec.manageUntaggedENI`、`cni.mode`、`cilium.enabled`、
  `networkPolicy.enabled` 同样为必填项：它们没有默认值，因此今后 chart 修改默认值时，
  不会翻转已有集群的行为。每次 `helm upgrade` 都需要重新传入这些值，或者使用
  `--reset-then-reuse-values`（Helm ≥ 3.14），或者使用纳入版本管理的 values 文件。不要使用
  `--reuse-values`：它会连同上一个 chart 的默认值一起沿用，跨 chart 版本升级时新增的 key 会缺失，
  `image.tag` 也会停留在旧版本。（使用 `platformUser` 时，`zec.manageUntaggedENI` 由平台侧配置，无需传入。）
  用 1.0.0 安装的 release，这些开关取的是当时 chart 的默认值，任何 reuse 参数都无法正确带过来：
  从 1.0.0 升级请按 [docs/deploy-direct-cn.md](docs/deploy-direct-cn.md) §3.4 操作。
- `ZecNode` CRD 位于 chart 的 `crds/` 目录，helm 只在**首次安装**时安装它，升级时不会更新。
  升级 chart 前，请先手动应用新版 CRD：
  `helm pull oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.1 --untar`，
  然后执行 `kubectl apply --server-side -f zenlayer-vpc-cni/crds/zecnodes.yaml`。
- controlplane 使用独立的镜像 `<image.repository>-controlplane`（tag 相同），节点镜像中不包含它。
  使用镜像仓库代理或离线安装时，需要准备这两个镜像。

双栈安装还需要提供 IPv6 计费参数：controlplane 会为其创建的每块网卡启用公网 IPv6，而
`zec.ipv6InternetChargeType` 的默认值 `ByTrafficPackage` 要求同时提供流量包大小。
因此双栈安装需要替换上述命令中的 `zec.ipStack=ipv4`，并加上

```sh
  --set zec.ipStack=dual \
  --set zec.ipv6InternetChargeType=ByBandwidth --set zec.ipv6Bandwidth=1
```

或者保持默认计费方式并追加 `--set-json zec.ipv6PackageSize=0.1`。
两者分别按什么计费，参见[地址族](#地址族)。

如需先预览渲染出的 manifest（离线渲染时需要声明集群版本和准入策略 API，
否则 chart 会认为集群低于 1.30 而拒绝渲染）：

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

### 3. 验证

```sh
# ① controlplane 与 DaemonSet 全部就绪，且没有重启
kubectl -n kube-system rollout status deploy/zenlayer-vpc-cni-controlplane
kubectl -n kube-system rollout status ds/zenlayer-vpc-cni --timeout=10m
kubectl -n kube-system get pod -l 'app in (zenlayer-vpc-cni,zenlayer-vpc-cni-controlplane)' -o wide

# ② 每个节点的 ZecNode：READY 为 True，ALLOCATION 为 Satisfied，ALLOCATABLE 是该节点的容量
kubectl get zn

# ③ 新建 Pod 拿到的是 VPC 子网里的地址（不是 kubeadm 的 podCIDR）
kubectl run nettest --image=busybox --restart=Never -- sleep 3600
kubectl wait --for=condition=Ready pod/nettest --timeout=60s
kubectl get pod nettest -o jsonpath='{.status.podIP}{"\n"}'
kubectl delete pod nettest

# ④ 节点上：conflist 落盘且权限 0600、二进制带版本信息
ssh <节点> 'ls -l /etc/cni/net.d/10-zenlayer-cni.conflist; /opt/cni/bin/zec-cni version'

# ⑤ 节点上：池状态
ssh <节点> 'curl -s 127.0.0.1:11414/metrics | grep -E "^resource_pool_"'
ssh <节点> 'curl -s 127.0.0.1:11414/healthz'
```

需要注意以下四点：

- **`/healthz` 与 `/metrics` 监听在节点的每一个地址上，而不只是回环地址。** daemon 使用
  hostNetwork，监听 `:11414`，以便 Prometheus 从节点之外抓取；kubelet 探针访问的是 `127.0.0.1`。
  上面的示例使用 ssh 只是习惯，并非因为端口只对节点开放：镜像中确实没有 `curl`，但**有 `wget`**，
  因此通过 `kubectl exec` 也能访问同样的端点，无需登录节点：

  ```sh
  POD=$(kubectl -n kube-system get pod -l app=zenlayer-vpc-cni \
          --field-selector spec.nodeName=<节点> -o jsonpath='{.items[0].metadata.name}')
  kubectl -n kube-system exec $POD -c zec-ipam -- \
    wget -qO- http://127.0.0.1:11414/metrics | grep -E '^resource_pool_'
  ```

  该监听上只有 `/healthz` 与 `/metrics`，刻意不提供 pprof。
- **`zec-cni version` 应打印出非空的版本 / commit / build time。**
  `CNI Version: unknown` 或 `Git Commit ID: DEFAULT` 表示构建时 git 无法提供该信息，
  该镜像不具备可追溯性。字段为空则说明链接期参数根本未传入，直接执行 `go build` 即会得到这一结果。
- daemon 的 `/healthz` 只有在 controlplane 核验过本节点、并建立好 warm pool 之后才开始响应，
  这也是 DaemonSet 预留了较宽松的启动窗口（360 x 10s）的原因：新节点需要等待 controlplane 核验实例，
  必要时还需创建网卡。**但它反映的是 daemon 的状态，而非节点的状态**：conflist 在此之后才写入磁盘，因此在 Cilium
  串接模式下，当 daemon 仍在等待 Cilium agent、该节点无法接收任何 Pod 时，daemon 的 `/healthz`
  依然返回 `ok`。DaemonSet 的探针都只访问 `/healthz`，因此在这种状态下节点会一直处于 Ready，
  **监控还需关注重启计数与日志中的 `Still waiting for the cilium agent`**。详见
  [docs/deploy-cilium-chained-cn.md](docs/deploy-cilium-chained-cn.md) §4.1。
- controlplane 自身的 `/healthz` 位于 `:11416`，未就绪时返回 503 并说明正在等待的条件（参见[故障排查](#故障排查)）。

## 容量规划

单节点可分配的 Pod 地址数由实例规格决定：

```text
容量 = (ENI 配额 - 本插件之外挂载的 ENI 数 - 主网卡被排除时为 1，否则为 0) x (每 ENI 地址数 - 1)
```

下表按主网卡所在子网已列在 `zec.subnetIDs` 中的情况计算。

| 规格 | ENI 配额 | 每 ENI 地址数 | 本插件可分配 |
|---|---|---|---|
| 8 vCPU | 2 | 15 | **28** |
| 16 vCPU | 4 | 30 | **116** |

每块 ENI 都需要为自身保留一个地址：实例自带的 ENI 与本插件自建的 ENI 相同，因此各自贡献
「每 ENI 地址数减一」。

以其他方式挂载到节点上的辅助 ENI 不会被纳管，除非设置 `zec.manageUntaggedENI=true`：此时
**位于已配置 Pod 子网内**的这类 ENI 会被**纳管**，其地址加入 warm pool，并计入上表数值。

⚠️ **被纳管的 ENI 在池收缩时会被卸载并删除**；在 `zec.ipStack=dual` 下，controlplane 还会
为其启用 IPv6，在 `ipv6Type` 为 `Public` 的子网上这是一次产生费用的操作。开启纳管时，
若某块 ENI 挂载用于其他用途（存储、管理、监控），请为其添加 `zenlayer:cni:no-manage=true`
标签将其排除。

**未被纳管的 ENI**（无论是因为纳管未开启、添加了标签，还是本身就位于已配置 Pod 子网之外）
会被完全保留：新 Pod 不会从其上分配到地址，它也不会被卸载。但它仍占用实例的一个 ENI 槽位，
因此该节点的容量会比上表少「一块 ENI 的地址数」，即下文 `eniUnmanaged` 所计的数量。

**节点主网卡**同理：只有其子网列在 `zec.subnetIDs` 中时才服务 Pod。否则它不再补充新地址，
Pod 正在使用的地址随 Pod 退出而释放，空闲地址被归还；它对应的那一份（每 ENI 地址数 − 1）即公式中
「主网卡被排除」那一项，但**不计入** `eniUnmanaged`，插件也不为其分配 IPv6 前缀。
crd 模式下 `ZecNode` 中该网卡的状态为 `Excluded`；direct 模式下 daemon 改为在 Node 上记录一条
**Normal** 事件 `InterfaceExcluded`。两种模式下，容量都随该网卡上的 Pod 退出而逐步下降（只扣除其尚未使用的部分）。
本插件创建、但所在子网后来被移出 `zec.subnetIDs` 的 ENI 按同样方式处理，并在其上 Pod 全部退出后被释放。
daemon 持有的空闲地址在下一次 GC 时归还（crd 模式下约一分钟内）。实例主网卡上 daemon 没有记录的地址不会被改动，
因为它可能是运维人员手动添加的；升级后，这也包括升级前遗留的 warm 地址，如不需要请手动删除。
若没有其他网卡能够服务 Pod（ENI 配额已满，或已被不纳管的 ENI、被平台拒绝分配 IPv6 前缀的网卡占满），
该节点将无法放置任何 Pod，crd 模式下 `ALLOCATION` 为 `NoPodInterface`。

controlplane 将其计算出的各项数值写入每个节点的 `ZecNode`。当某个节点的容量与上表不符时，
这是最快的核对方式（主网卡是否被排除，请查看 `kubectl describe zn <节点>` 中该网卡的状态）：

```sh
kubectl get zn <节点> -o jsonpath='{.status.capacity}{"\n"}'
# {"allocatable":58,"eniQuota":4,"eniUnmanaged":2,"ipv4PerENI":30}
```

其中 `eniUnmanaged` 是本插件不纳管的 ENI 数，`allocatable` 是扣除这些 ENI 以及所有不再补充新地址的网卡（`Excluded` 的网卡，即主网卡或本插件在已移出 `zec.subnetIDs` 的子网中创建的网卡；以及双栈下被拒绝分配 IPv6 前缀的网卡）之后的容量
（`kubectl get zn` 的 ALLOCATABLE 列）。节点上报值会略低：device plugin 还会再减去未声明
`zenlayer.com/eni-ip` 的存量 Pod。

**上表均为 IPv4 地址数，`zec.ipStack=dual` 不会改变这些数值。** Pod 地址对中的 IPv6 部分由
daemon 从 ENI 的 `/96` 前缀中本地切出，而一个 `/96` 能容纳的地址数远超任何 ENI 的服务能力，
因此 IPv6 永远不会成为约束项。两种模式下节点上报的 `zenlayer.com/eni-ip` 容量相同。

> **但真正的上限往往不是该数值，而是 kubelet 的 `maxPods`（默认 110）。** 在 16 vCPU 节点上，
> 本插件能提供 116 个地址，而 kubelet 只会调度 110 个 Pod（还需减去 hostNetwork 的系统
> Pod）。规划时取两者中的较小值；如需用满，需同时调大 `maxPods`。

device plugin 将可分配数上报为扩展资源 `zenlayer.com/eni-ip`，其值为容量减去「占用地址却未声明
该资源」的 Pod 数。这类 Pod 启动或退出、地址被归还、实例上限变化时都会立即重新计算，另有周期性重新计算作为兜底。
由于减去了这些 Pod，上报值比节点实际容量低出相应的个数（是下界，不会虚报）；排查时如需查看
池的实时状态，请读取节点上的 `resource_pool_*` 指标。

**该上报值只约束显式声明了该资源的 Pod。** 调度器的资源筛选只检查 Pod 在
`resources.requests` 中实际声明的资源，本插件也不会自动注入该声明。因此，一个
`resources` 为空的普通 Pod，即使节点的 `zenlayer.com/eni-ip` 已经为 `0`，仍会被调度到该节点，
随后因无法获得地址而卡在 `ContainerCreating`，直到 CNI ADD 超时，并由 kubelet 反复重试。

如需让调度器真正按 IP 容量分配，有以下两种做法：

- 为工作负载显式声明 `resources.requests: {zenlayer.com/eni-ip: "1"}`（`limits` 设置相同的值）；
- 或者按上述公式将 kubelet 的 `--max-pods` 设置为该实例的可分配地址数，
  通过 Pod 数上限间接约束。

另有一项节点侧的兜底机制：Pod 带有 `k8s.zenlayer.com/pod-evict-policy: allow` 注解时，
daemon 在分配地址前会先比较本节点已记录的 Pod 数与容量上限，达到上限即驱逐该 Pod，
使其重新调度到其他节点，而不是一直卡住。未带该注解的 Pod 不受此逻辑影响。

## 配置参考

以下所有配置项都位于 `chart/values.yaml`。一个 `zec.` 值最终渲染到何处，取决于由哪个组件读取它，
这一点无法从前缀上看出：

| 渲染到 | 配置项 |
| --- | --- |
| `zenlayer-vpc-cni-controlplane` ConfigMap（controlplane 读） | `zec.vpcID`、`zec.subnetIDs`、`zec.securityGroupID`、`zec.ipStack`、`zec.manageUntaggedENI`、`zec.ipv6*`、`controlplane.regionID`、`controlplane.identity`、`controlplane.scaler`、`controlplane.nodeGroups` |
| `zenlayer-cni-config` ConfigMap 的 `eni_conf` 键（daemon 读） | 其余 `zec.` 值，即 daemon 自身使用的值（如 `zec.ipStack`、`zec.serviceCidr`、`zec.poolMonitorIntervalSec`） |
| CNI conflist | `zec.dataPath` |
| 两处都渲染 | `zec.logLevel` |
| **DaemonSet 本身，不进任何 ConfigMap** | `zec.k8sServiceHost`、`zec.k8sServicePort`、`zec.logLevel`（作为 `ZENLAYER_CNI_LOGLEVEL`）、`zec.healthAndDebugPort`（作为探针端口） |
| **controlplane Deployment 本身** | `zec.secretName`（作为挂载的 Secret）、`controlplane.deployment.*`、`zec.k8sServiceHost` / `zec.k8sServicePort` |

「按节点覆盖配置」的机制**只替换 `eni_conf`**，因此 controlplane 读取的值和「DaemonSet 本身」
那一行中的值都无法通过该机制修改，参见下文。

| 配置项 | 默认值 | 说明 |
| --- | --- | --- |
| `image.repository` | `docker.io/zenlayer1/zeccni` | init 容器与 `zec-ipam` 的镜像。controlplane 的镜像是这个名字加 `-controlplane`，tag 相同。 |
| `image.tag` | `v2.0.1` | 镜像 tag，两个镜像共用。 |
| `image.pullPolicy` | `IfNotPresent` | 镜像拉取策略。 |
| `zec.vpcID` | `""` | `controlplane.deployment.enabled` 时**必填**（direct 模式同样必填）；使用 `controlplane.platformUser` 时不生效。Zenlayer VPC ID。所有子网都必须属于它。 |
| `zec.subnetIDs` | `[]` | 必填条件同 `zec.vpcID`。Pod IP 只来自这些子网，插件新建的网卡也创建在这些子网中。节点主网卡所在子网在其中时，主网卡也参与分配；否则不参与，节点可放置的 Pod 数会减少主网卡对应的那一份（参见[容量规划](#容量规划)）。从旧版本升级时，主网卡子网不在列表中的存量节点会减少这一份容量，随主网卡上的 Pod 退出而逐步生效（地址随 Pod 一起释放；空闲地址的处理参见[容量规划](#容量规划)）。没有其他网卡能够服务 Pod 时，节点将无法放置任何 Pod（crd 模式下 `ALLOCATION` 为 `NoPodInterface`）。最多 5 个，且须位于同一个 VPC 中。 |
| `zec.securityGroupID` | `""` | 必填条件同 `zec.vpcID`。应用到本插件所创建 ENI 上的安全组。 |
| `zec.ipamType` | **必填** | 由哪个组件调用 Zenlayer OpenAPI。`crd`（本文描述的集中式部署）：只有 controlplane 调用；节点不持有凭证，各自在自身的 `ZecNode` 对象中声明需求（`kubectl get zn`）；需要 Kubernetes 1.30 及以上。`direct`：每个节点的 daemon 自行调用，凭证挂载到每个节点，参见 [docs/deploy-direct-cn.md](docs/deploy-direct-cn.md)。 |
| `controlplane.deployment.enabled` | `false` | 在本集群内运行 controlplane（Deployment，`hostNetwork: true`）。与 `controlplane.platformUser` 必须且只能设置一个。 |
| `controlplane.platformUser` | `""` | controlplane 由平台托管、运行在集群之外时，其访问本集群时用于认证的用户名；chart 为其授权。设置后 controlplane 的配置由平台提供，`controlplane.regionID`、`scaler`、`nodeGroups`、`identity` 与 `deployment.*` 均不生效。 |
| `controlplane.regionID` | `""` | Pod 子网与所有节点所在的地域；controlplane 在该地域查询网卡和子网，并拒绝实例位于同一 VPC 其他地域的节点（`IdentityMismatch`）。`deployment.enabled` 时必填。 |
| `controlplane.scaler.target` | `2` | 每个节点 warm（预分配、未使用）IP 的目标数量。必须 >= 1。 |
| `controlplane.scaler.targetMin` | `1` | 池的总量下限（含已被 Pod 占用的地址），并非仅指空闲部分。池的上限是节点容量，没有单独的配置。 |
| `controlplane.nodeGroups` | `[]` | 按节点标签分组设置温池大小，第一个 `selector` 匹配的组生效，例如 `[{selector: {pool: large}, scaler: {targetMin: 10, target: 20}}]`。 |
| `controlplane.identity.providerID` | `auto` | `auto`：Node 没有 `spec.providerID` 时，controlplane 按 Node 的 InternalIP 找到实例，并与 daemon 从 DMI 读取的实例号核对后，补写 `zenlayer:///<regionId>/<instanceId>`，与 zenlayer-cloud-controller-manager 写入的格式相同（只写一次，API server 不允许再修改）。`manual`：从不写入，只服务已按该格式（或旧格式 `zenlayer://<instanceId>`）设置了 providerID 的节点。 |
| `controlplane.deployment.image` | `""` | 留空即为 `<image.repository>-controlplane:<image.tag>`。controlplane 以非特权用户（65534）运行，通过所属组读取 Secret（权限 0440，`fsGroup`），因此此处指定的镜像也必须以该用户运行。 |
| `controlplane.deployment.nodeSelector` | `{}` | controlplane 必须运行在哪些节点上。留空：任意节点，优先选择带 `node-role.kubernetes.io/control-plane` 标签的节点，因为 controlplane 挂载凭证，而 worker 上运行的是租户负载。controlplane 所在的节点需要公网出向。 |
| `controlplane.deployment.replicas` | `1` | controlplane 副本数，通过 Lease 选主，同一时刻只有一个副本在工作。多一个副本可以更快接管；滚动升级到一个始终无法启动的配置时，另一个副本仍在工作（只有一个副本时，旧 Pod 会先被删除）。health 端口是 host 端口，每个节点最多运行一个副本：副本数不能超过可运行它的节点数。 |
| `controlplane.deployment.workers` | `4` | controlplane 同时调谐的节点数。 |
| `controlplane.deployment.resources` | `requests: {cpu: 50m, memory: 64Mi}` | controlplane 容器的资源配置。 |
| `zec.ipStack` | **必填** | Pod 获得的地址族：`ipv4` 或 `dual`。`dual` 要求每个子网都同时具备 IPv4 与 IPv6 CIDR。IPv6 单栈尚未实现。参见[地址族](#地址族)。 |
| `zec.ipv6InternetChargeType` | `ByTrafficPackage` | 平台对网卡公网 IPv6 的计费方式：`ByTrafficPackage`、`ByBandwidth` 或 `BandwidthCluster`。仅在 `ipStack` 为 `dual` 时使用；私网 IPv6 的子网上会被忽略。 |
| `zec.ipv6Bandwidth` | `10000` | 每块网卡的公网 IPv6 带宽上限，单位 Mbps，仅在 `ipStack` 为 `dual` 时使用。必须 >= 1。 |
| `zec.ipv6PackageSize` | `0` | 流量包大小，单位 TB。`ipv6InternetChargeType` 为 `ByTrafficPackage` 时必填，取值为 0.1 的正整数倍。请使用 `--set-json` 传入：`--set` 会将小数当作字符串。 |
| `zec.ipv6ClusterId` | `""` | 网卡需要加入的共享带宽包。`ipv6InternetChargeType` 为 `BandwidthCluster` 时必填。 |
| `zec.networkMode` | `eni_multi_ip` | 网络模式。仅支持 `eni_multi_ip`。 |
| `zec.dataPath` | `veth` | Pod datapath，渲染为 `eniip_virtual_type`。**只接受 `veth`**；在 `cni.mode=delegated` 下不渲染该键，因为本插件在该模式下不创建设备，参见[datapath](#datapath)。 |
| `zec.serviceCidr` | `""` | Service CIDR，多个值以逗号分隔。**只有 kubeadm 创建的集群才可以留空**：留空时 daemon 会读取 `kube-system/kubeadm-config`，读取失败则无法启动。datapath 确实不消费该值（service 流量本来就经过宿主机协议栈），但 daemon 仍要求从两个来源之一获得 IPv4 的 service CIDR。双栈集群下缺少 IPv6 那一项只会记录一条 Info 日志。 |
| `zec.secretName` | `zenlayer-cni-access-key` | 预先创建的、保存 OpenAPI 凭证的 Secret 名称。只挂载给 controlplane（direct 模式下挂载给每个节点）。 |
| `zec.poolTarget`、`zec.poolTargetMin`、`zec.poolTargetMax`、`zec.poolTargetCapProbe` | 见 `values.yaml` | **仅 direct 模式使用**，参见 [docs/deploy-direct-cn.md](docs/deploy-direct-cn.md)。集中式部署的温池大小由 `controlplane.scaler` 决定。 |
| `zec.manageUntaggedENI` | **必填** | 是否纳管本插件之外挂载到节点上的辅助网卡。`false` 不纳管；设为 `true` 时，可为网卡添加 `zenlayer:cni:no-manage=true` 标签将其排除。参见[容量规划](#容量规划)。 |
| `zec.reconcileIntervalSec` | `1200` | daemon 的 GC 间隔，单位秒。参见[架构](#架构)。 |
| `zec.poolMonitorIntervalSec` | `120` | 池监控间隔，单位秒。 |
| `zec.healthAndDebugPort` | `11414` | `/healthz` 与 `/metrics` 使用的端口。Pod 为 `hostNetwork`，因此该端口会占用节点端口。 |
| `zec.logLevel` | `""` | 两个二进制共用的日志级别（`trace｜debug｜info｜warn｜error｜fatal｜panic`），留空则保持内置默认值 `info`。它渲染为 DaemonSet 上的 `ZENLAYER_CNI_LOGLEVEL`；取 `debug` 或 `trace` 时还会在 conflist 中写入 `"debug": true`（`zec-cni` 作为 Cilium 的委托 IPAM 运行时写在 `ipam` 块中，因为该条目的顶层键属于 Cilium）。这是 `zec-cni` 唯一的通道，因为 kubelet 从宿主机 exec 它，它无法看到 Pod 的环境变量。 |
| `zec.k8sServiceHost` | `""` | 设置 daemon 与 controlplane 的 `KUBERNETES_SERVICE_HOST`。`cilium.enabled=true` 时留空：安装 / 升级时从 `kube-public/cluster-info` 读取（端口取自其中的 URL，缺省为 443）。原因：Cilium 代替 kube-proxy 处理 Service 流量时（本项目的 Cilium 文档即按此配置），集群内 Service 要等 Cilium 启动后才可用，而双栈节点上 Cilium 又要等待本插件注册后带来的 IPv6。读取失败时（`helm template`、客户端 dry-run、GitOps 工具等离线渲染，或该 ConfigMap 不存在，即非 kubeadm 集群），chart **拒绝渲染**，直到显式设置该值，而不会退回使用集群内 Service；同时需要设置 `zec.k8sServicePort`（参见下一行）。读取到的就是 cluster-info 中的值，可能只是某一台 master 的地址；应填写负载均衡或 VIP 地址。入口变化后需要重新执行 `helm upgrade`。其余情况留空：使用集群内 Service。 |
| `zec.k8sServicePort` | `""` | 设置 `KUBERNETES_SERVICE_PORT`；地址自动读取时随之一并读取。显式设置 host 时需要同时设置该值，否则使用的是 kubelet 注入的集群内 Service 端口 `443`，而直连的 apiserver 入口（通常是 6443）一般不监听该端口。 |
| `cni.mode` | **必填** | 由哪个组件创建 Pod 的网络设备。`chained`：本插件创建 veth 对，Cilium（若启用）串接在其后。`delegated`：Cilium 创建设备，本插件只分配地址并安装宿主 per-ENI 路由，要求 `cilium.enabled=true`；**已在测试集群端到端验证，尚未经过生产验证**，且**不要出于性能考虑选择该模式**（参见 [docs/performance-cn.md](docs/performance-cn.md)），步骤参见 [docs/deploy-cilium-netkit-cn.md](docs/deploy-cilium-netkit-cn.md)。 |
| `cilium.enabled` | **必填** | 串接上游 `cilium-cni`。`cni.mode=chained` 时它排在本插件**之后**（`generic-veth` chaining）；`cni.mode=delegated` 时它是主插件，本插件位于其 `ipam` 块中。**不会**安装 Cilium，参见[网络策略](#网络策略)。 |
| `cilium.waitTimeout` | `""` | daemon 等待 Cilium agent socket 的时长，超时则重启。使用 Go duration 语法，留空则为默认值 5m。 |
| `networkPolicy.enabled` | **必填** | 声明 NetworkPolicy 必须被执行；开启时要求 `cilium.enabled=true`。它本身不执行任何策略：`cilium.enabled=true` 时，无论此处如何设置，Cilium agent 都会执行策略；Cilium 关闭时则无组件执行。 |
| `networkPolicy.provider` | `cilium` | 期望的策略引擎，只接受 `cilium`。 |
| `resources.requests.cpu` | `250m` | `zec-ipam` 的 CPU request。 |
| `resources.requests.memory` | `256Mi` | `zec-ipam` 的内存 request。 |
| `resources.limits` | 未设置 | 同样被接受并直接渲染到容器上，但默认不设置：daemon 位于每个 Pod 的 CNI ADD 路径上，CPU limit 会拖慢整个节点的 Pod 启动，内存 limit 则会将一次瞬时尖峰变成 OOMKill，导致 IPAM 一并中断。 |

daemon 的配置 schema（`pkg/daemon/config/config.go`）还接受一些 chart 未渲染的字段，
其中包括 `disableDevicePlugin`、`extraRoutes`、`poolCapRatio`、`poolGCPeriodSec`、
`subnetStatAgingSec` 和 `subnetStatUpdateIntervalSec`。
如需设置，请直接编辑 ConfigMap 中的 `eni_conf` JSON。`ipamType: crd` 下，`eni_conf` 中出现
`poolCapRatio`、`subnetStat*` 或 `rateLimit` 会使 daemon 拒绝启动：前两者在 controlplane 中没有对应项
（池大小由 `controlplane.scaler` 决定，子网计数由 controlplane 自行刷新），`rateLimit` 则由 controlplane 自身的
同名配置承担，格式为 `{readQPS, readBurst, writeQPS, writeBurst}`。

direct 模式下 `rateLimit` 会被解析，但**没有任何消费者**：设置它不会产生任何效果，也不会产生任何告警。
不要使用它来限制 OpenAPI 的调用频率。

也可以通过 `k8s.zenlayer.com/vpc-cni-config` 节点标签，将某个节点指向一个专属的 ConfigMap，
标签值格式为 `<namespace>.<name>`。以下五点容易出错：

- **命名空间必须是 daemon 自身所在的命名空间**，指定其他命名空间会被拒绝，daemon 无法启动。标签添加在 Node 上，
  而 kubelet 可以为自身的 Node 添加任意非 `kubernetes.io` 前缀的标签，该标签的前缀恰好不是
  `kubernetes.io`。如果没有该限制，一台被攻破的节点就能让自身的 daemon 读取一份由该节点自行写入的
  配置，而 `eni_conf` 中的 `openAPIDomain` 决定了 daemon 携带凭据访问哪台主机。

- **它只覆盖 `eni_conf`，不覆盖其他任何内容。** daemon 只从标签指定的 ConfigMap 中读取这一个键，
  该键不存在时 daemon 无法启动。上表中「DaemonSet 本身」那一行的内容（日志级别环境变量、探针端口、
  挂载的 Secret）来自 DaemonSet，无法通过这种方式按节点修改。
- **是整体替换，而非合并。** 指定的 `eni_conf` 会被整份采用，不继承集群级配置的任何内容。
  未填写的键**不会**沿用集群级的值，而是处于未设置状态：daemon 会使用自身的默认值，或者直接拒绝启动。
- **CNI conflist 仍然使用集群级的那一份。** 因此 `zec.dataPath` 也无法按节点覆盖。
- **集中式部署下，它不能携带任何平台配置。** VPC、子网、安全组、`manageUntaggedENI`、IPv6 计费
  等由 controlplane 管理，写入节点的 `eni_conf` 会使 daemon 拒绝启动；如需按节点组区分温池大小，请使用
  `controlplane.nodeGroups`。

### 配置变更如何生效

**daemon 的配置不支持热生效。** `eni_conf` 中的值都在 daemon 启动时读取一次，修改后只有在该节点的
pod 重启后才会生效。「只读取一次、通过重启生效」是刻意的设计：一个可以在运行中变化的配置项，必须在
daemon 可能处于的每一个状态下都能被安全地修改。

**唯一的例外是温池大小。** `controlplane.scaler` / `controlplane.nodeGroups` 由 controlplane
读取，经 `ZecNode` 下发到各节点，daemon 收到后直接调整池水位，无需重启。controlplane 自身的
Deployment 同样带有 `checksum/config` 注解，修改会触发其滚动重启，期间已有 Pod 不受影响。

`helm upgrade` 会自动处理。DaemonSet 上带有针对渲染后 ConfigMap 的 `checksum/config`
注解，任何配置变化都会改变 pod 模板，从而触发滚动更新。

**手动编辑 ConfigMap 不会触发任何重启**，需要手动执行以下步骤：

```sh
kubectl -n kube-system rollout restart ds/zenlayer-vpc-cni
```

`helm upgrade` **失败之后也需要执行这一步**。helm 逐个应用对象，中途失败的升级
可能已经完成了 DaemonSet 的滚动更新，却尚未写入 ConfigMap，导致新启动的 pod 读取到旧值。
之后即使有一次成功的 upgrade 也无法修复：此时 pod 模板已没有变化，不会再次滚动更新，
这些节点将长期停留在旧配置上。

节点上的 CNI conflist 同样只在 daemon 启动时写入一次，之后不再重新读取，因此 `dataPath`
等 conflist 字段也适用同一规则。

## 卸载

`helm uninstall` 只删除 chart 渲染的 Kubernetes 对象。**`ZecNode` CRD 和所有 `ZecNode` 对象会
保留**（helm 从不删除 `crds/` 中的 CRD），确定不再使用时请手动删除：

```sh
kubectl delete crd zecnodes.network.zenlayer.com   # ZecNode 没有 finalizer，删除不触发任何平台调用
```

节点本地的状态同样会**有意保留**：

| 每个节点上的残留 | |
| --- | --- |
| `/etc/cni/net.d/10-zenlayer-cni.conflist` | CNI 配置 |
| `/opt/cni/bin/zec-cni` | CNI 二进制 |
| `/var/lib/cni/zenlayer/` | 地址记录 |
| `/var/run/zenlayer/`、`/var/log/zenlayer/` | 日志目录；socket 随 pod 一起消失 |
| `/etc/sysctl.d/99-zenlayer-vpc-cni.conf` | `rp_filter` 配置片段 |

**其中真正关键的是 conflist。** 只要它仍然存在而后端 daemon 已不存在，kubelet 仍会认为
本插件负责该节点的 Pod 网络，新建 Pod 会一直卡在 `ContainerCreating`，而不会回退到
其他 CNI。

```sh
rm -f /etc/cni/net.d/10-zenlayer-cni.conflist
```

**只有当 `/etc/cni/net.d/` 下还有另一个 CNI 的 conflist 时，才可以删除该文件。** 如果本插件是
该节点上唯一的 CNI，删除之后 kubelet 将完全没有网络插件：节点会变为 `NotReady`
并报告 `cni plugin not initialized`，Pod 同样无法启动，而且 5 分钟后
`node.kubernetes.io/not-ready:NoExecute` 污点会开始驱逐已在运行的负载。
在这种节点上保留该文件反而是更好的失败方式，应当对该节点执行 drain。

CNI 创建的 ENI 会继续挂载在实例上。这一取舍是刻意的：detach ENI 是一次很慢的 API 调用，而 pod 的
终止宽限期不适合执行慢速 API 调用。将两者绑定意味着：要么将宽限期延长到影响每一次正常重启，
要么 detach 执行到一半时被终止。

crd 模式的 controlplane 存活期间（leader 上任时执行一次，之后约每小时一次）会删除本 VPC 内满足以下全部条件的网卡：未挂载；带有 `zenlayer:cni:created-by=zenlayer-vpc-cni`；
`zenlayer:cni:cluster-id` 等于本集群 id（`kube-system` 命名空间的 UID）或没有该标签；
10 分钟以上没有变动。带有其他集群 id 的网卡不会被改动，仍挂载在实例上的网卡也不会被改动。**没有集群标签的网卡**
（本版本之前创建的、旧版本集群创建的，或 direct 模式下 daemon 无法读取 `kube-system` 时创建的）
会被同一 VPC 中任何一个 crd 模式集群的 controlplane 清理。多个集群共用 VPC 时，需要全部升级到本版本才能避免互相误删。
controlplane 一旦不存在，就没有任何机制执行该清理；direct 模式的 daemon 只清理本实例的网卡，不清扫 VPC。**卸载或删除集群前，请先记下集群 UID**
（`kubectl get ns kube-system -o jsonpath='{.metadata.uid}'`），之后自行检查账号内同时带有
`zenlayer:cni:created-by=zenlayer-vpc-cni` 和 `zenlayer:cni:cluster-id=<该 UID>` 标签的 ENI；
只按 `created-by` 过滤会误删共用该 VPC 的其他集群的网卡。本版本之前创建的网卡没有集群标签，
需要按 `created-by` 加上 `zenlayer:cni:instance-id` 等于本集群某个节点的实例 ID 进行筛选。

**辅助地址同样会残留，而且无法通过网卡上的标签查到。** 卸载时 warm pool 中持有的
地址会继续绑定在所属网卡上，而**地址本身不带任何标签，只有网卡带有标签**。因此位于节点**主 ENI**
上的残留地址无法通过标签搜索到。在一个五节点集群上，`helm uninstall` 后共残留
**10 个**地址，恰好是每节点温池目标数（默认 2 个），其中 **5 个**位于主 ENI 上、
**无任何标签**。如果在同一批节点上重新安装 chart，这些地址会被重新收养；其中主网卡上的地址仅当主网卡所在子网列在 `zec.subnetIDs` 中时才会被收养。如果这些
节点需要下线，请逐个实例列出网卡的辅助地址并显式释放。卸载前，请先保存一份
`kubectl get zn -o yaml`，其中包含每个节点的网卡和地址，可在清理时用于核对。

**但如果实例本身需要释放，则无需执行上述手动清理，前提是保持平台的「同步释放关联资源」
为开启状态**（OpenAPI 中为 `ReleaseInstances` 的 `DependResource`，默认 `true`）。实例释放
时会一并删除挂载在其上的弹性网卡及其辅助地址，包括本插件创建的网卡和地址。

反之，**释放节点时若关闭该选项**（仅释放实例本身、保留关联资源），本插件创建的网卡与
地址都会残留。crd 模式的 controlplane 存活时，其 VPC 清扫会按上文所述删除已脱离的插件网卡；
真正残留且没有任何机制回收的，是该实例主网卡上的辅助地址。没有这样的 controlplane 时，需要按
`zenlayer:cni:created-by` 与 `zenlayer:cni:cluster-id=<集群 UID>` 两个标签手动清理网卡
（没有集群标签的网卡按 `zenlayer:cni:instance-id` 筛选）。

## 从源码构建

需要 Go 1.26.8（依据 `go.mod`），构建镜像还需要在 `amd64` 宿主机上安装 Docker。

| 目标 | 作用 |
| --- | --- |
| `make help` | 打印解析后的构建变量和可用的 make 目标。 |
| `make bin` | 执行 `go mod tidy`，然后将 `zec-cni`、`zec-ipam`、`zec-controlplane` 构建到 `output/`（`CGO_ENABLED=0 GOOS=linux`）。 |
| `make zec-cni` / `make zec-ipam` / `make zec-controlplane` | 只构建其中一个二进制。 |
| `make pkg` | 在 `bin` 的基础上，将 bootstrap 脚本一并复制到 `output/`。 |
| `make image` | 使用 `images/Dockerfile` 构建节点镜像（`zec-cni`、`zec-ipam`）。仅支持 amd64。 |
| `make image-controlplane` | 使用 `images/controlplane.Dockerfile` 构建 controlplane 镜像 `<IMAGE_NAME>-controlplane`。仅支持 amd64。 |
| `make chart-lint` | 填入必填值后对 chart 执行 `helm lint`。 |
| `make chart-template` | 填入必填值后渲染 chart。 |
| `make chart` | 先 lint，再 `helm package` 到 `output/charts/`。 |
| `make all` | `pkg` + `image` + `image-controlplane` + `chart`。 |
| `make tidy` | `go mod tidy`。 |
| `make clean` | 执行 `go clean`，删除 `output/`，并删除构建出的两个镜像。 |

常用变量：`OUTPUT`（默认 `output`）、`IMAGE_NAME`（`docker.io/zenlayer1/zeccni`）、
`IMAGE_TAG`、`CHART_DIR`（`chart`）、`CHART_OUTPUT`（`output/charts`）、
`CHART_VERSION` / `CHART_APP_VERSION`（从 `Chart.yaml` 读取）、`ENGINE`（`docker`）、
`HELM`（`helm`）、`GOPROXY`，以及 `DEBUG`（默认为 `1`，会关闭优化并保留符号；
设为 `DEBUG=0` 可构建剥离符号的发布版本）。

运行测试：

```sh
go test ./...
```

其中若干包依赖 Linux 专有的 netns 辅助函数，在其他操作系统上无法编译，因此测试需要在 Linux
上运行。CI 即按此方式运行：整个 job 设置 `GOOS=linux CGO_ENABLED=0`，测试步骤单独使用
`CGO_ENABLED=1 go test -race`。在非 Linux 工作机上，`GOOS=linux go vet ./...` 可以对包括
测试文件在内的全部代码进行类型检查。

## 故障排查

**日志。**

- `zec-cni` 在节点上写入 `/var/log/zenlayer/cni.log`（由 lumberjack 轮转：256 MB、2 个备份、
  保留 7 天）。kubelet 也会在 Pod 的 event 中暴露 CNI 错误。
- `zec-ipam` 日志输出到 stderr：`kubectl -n kube-system logs ds/zenlayer-vpc-cni -c zec-ipam`。
- controlplane 日志：`kubectl -n kube-system logs deploy/zenlayer-vpc-cni-controlplane`
  只取其中一个副本的日志；多副本时只有持有 Lease 的副本在工作。Lease 持有者的前缀是其所在的节点名：
  `kubectl -n kube-system get lease zenlayer-vpc-cni-controlplane -o jsonpath='{.spec.holderIdentity}'`。
- `zec.logLevel` 在安装时统一设置两个二进制的日志级别：它渲染 DaemonSet 上的
  `ZENLAYER_CNI_LOGLEVEL`，取 `debug` **或 `trace`** 时还会写入 `zec-cni` 读取的 conflist
  `debug` 标志：`trace` 比 `debug` 更详细，因此它不会关闭该标志。
- 日志级别和输出位置也可以通过环境变量 `ZENLAYER_CNI_LOGLEVEL`
  （`trace|debug|info|warn|error|fatal|panic`）和 `ZENLAYER_CNI_LOCATION`
  （文件路径，或 `stdout`）覆盖。`zec-ipam` 还支持 `-log-level` 参数，且优先级高于
  `ZENLAYER_CNI_LOGLEVEL`。
- 在 conflist 中设置 `"debug": true` 会将本次调用的 `zec-cni` 日志级别提升到 debug。
- 在调用 OpenAPI 的组件（集中式部署下为 controlplane）上设置 `ZENLAYER_CNI_SDK_HTTP_DUMP=true`，
  会额外打印每次 OpenAPI 调用的完整请求与响应。该选项默认关闭，
  且**不会**因为 `ZENLAYER_CNI_LOGLEVEL=debug` 而开启：该 dump 是全量的，包含
  `Authorization` 头，即每次调用都会将账号的 AccessKeyID 以明文写入日志。仅在可丢弃的
  集群上开启，并将其产生的日志视为凭据对待。

**健康检查与指标。** 在任意运行该 DaemonSet 的节点上：

```sh
curl 127.0.0.1:11414/healthz
curl 127.0.0.1:11414/metrics
```

daemon 不提供任何 profiling 端点。该端口需要能从节点外访问（供 Prometheus 抓取），而 `/debug/pprof` 会将堆快照、CPU profile 和进程命令行
暴露给任何能访问该节点的一方。需要 profile 时，请单独构建带 pprof 的版本。

**首先查看 `kubectl get zn`。** 每个节点一行，Pod 无法获得地址时，几乎总能在此处看到原因：

```sh
kubectl get zn
# NAME   INSTANCE   REQUESTED   ALLOCATABLE   READY   REASON   ALLOCATION   AGE
kubectl describe zn <节点>     # 完整的 conditions、网卡与地址
```

| 列 / condition | 含义 |
| --- | --- |
| `READY` / `REASON` | 只反映节点本身的状态：实例已核验、网卡清单已知即为 `True`。分配失败**不会**改变它。 |
| `ALLOCATION` | `IPAllocationFailed` 的 reason。`Satisfied` 表示需求已满足；否则为失败原因：controlplane 自行判定的 `ENIQuotaReached`（实例网卡数已满）、`NoAvailableSubnet`（没有可用的 Pod 子网）、`IPv6PrefixRefused`（平台拒绝为网卡分配 IPv6 前缀）、`NoPodInterface`（仅 crd 模式：主网卡因所在子网不是 Pod 子网而被排除，且没有其他网卡能够服务 Pod，即 ENI 配额已满，或已被不纳管的 ENI、被平台拒绝分配 IPv6 前缀的网卡占满；处理方法是将节点所在子网加入 `zec.subnetIDs`），或来自 OpenAPI 错误、与下表 Node 事件同名的原因（如 `InsufficientIpInSubnet`、`VpcResourceQuotaExceeded`、`CredentialServiceAbnormal`、`OpenApiUnreachable`）。其余平台拒绝显示为 `AllocationError`，message 中带有平台错误码。 |
| `InstanceVerified` condition | 节点与实例的绑定。`False` 时 daemon 不会开始服务，reason 说明原因：`IdentityClaimMissing`、`ProviderIDMissing`、`ProviderIDUnparseable`、`IdentityMismatch`、`InstanceNotFound`、`InstanceAmbiguous`、`BoundToOtherInstance`（参见[安全](#安全地运行-zecipamtypecrd)）。无法查询平台时，reason 为对应的平台事件原因（`CredentialServiceAbnormal`、`OpenApiUnreachable`、`OpenApiFlowLimit` 等），或 `AllocationError`（message 中带有平台错误码）；已经为 `True` 的保持不变，直到平台给出答复。 |
| 某个节点没有 `ZecNode` | controlplane 未在运行，或尚未处理到该节点。 |

**controlplane。** Secret（`zec.secretName`）不存在或名称不正确时，controlplane 的 Pod 停留在
`ContainerCreating`：通过 `kubectl -n kube-system describe pod -l app=zenlayer-vpc-cni-controlplane`
可以看到 `FailedMount` 事件，Secret 创建后 kubelet 会自行重试挂载。controlplane 运行之后，凭证内容、VPC、
子网的问题不会使其退出，而是使其停留在某一步，`/healthz`（`:11416`）返回 503 并说明正在等待的条件，Deployment
保持未就绪。安全组在启动时不做检查：节点需要新网卡时，创建网卡失败会显示在该节点的 `ALLOCATION` 列中。

```sh
kubectl -n kube-system get deploy zenlayer-vpc-cni-controlplane
kubectl -n kube-system logs deploy/zenlayer-vpc-cni-controlplane
```

| `/healthz` 返回 | 原因 |
| --- | --- |
| `waiting for OpenAPI credentials` | Secret 已挂载，但无法读取其中的键：键不是 `AccessKeyID` / `AccessKeyPassword`，或值为空。对临时凭证（存在 `SecurityToken` 键）：`AccessKeySecret`、`SecurityToken` 或 `Expiration` 缺失或为空，或 `Expiration` 不是 RFC 3339 格式。 |
| `waiting for the pod subnets` | 查询子网失败：OpenAPI 拒绝了凭证或无法访问、子网不存在或不属于 `zec.vpcID`，或 `zec.ipStack=dual` 而某个子网没有 IPv6 CIDR。日志中给出具体原因。 |
| `waiting for the cluster id` | 无法读取 `kube-system` 命名空间（使用其 UID 作为集群 id）：RBAC 缺失或 apiserver 不可达。 |
| `waiting for the ZecNode CRD` | CRD 未安装。请从 chart 中安装：`helm pull oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.1 --untar`，然后执行 `kubectl apply --server-side -f zenlayer-vpc-cni/crds/zecnodes.yaml`。 |
| `credential invalid` / `openapi unreachable` / `clock skew: request signatures rejected` | 运行中出现的全局故障：凭证被拒、无法访问 OpenAPI、controlplane 所在节点时钟偏差过大。同时记录在各节点 `ZecNode` 的 `ALLOCATION` 列和 Node 事件中。 |

IPv6 计费参数不合法（如 `ByTrafficPackage` 缺少 `ipv6PackageSize`）时，controlplane 启动后立即退出，
错误记录在其日志中。

**daemon 常见启动失败**（daemon 在启动时报告并退出）：

| 报错信息 | 原因 |
| --- | --- |
| `<字段> is set in the cni configmap, but with ipamType "crd" it belongs to the controlplane's configuration`（或 `... it has no effect and no controlplane counterpart`） | 手写或按节点覆盖的 `eni_conf` 中带有属于 controlplane 的配置（VPC、子网、安全组、凭证、温池大小等），或 crd 模式不使用的配置，请将其删除。 |
| `error retrieving service cidr: configmaps "kubeadm-config" not found` | 集群不是由 kubeadm 创建的，而 `zec.serviceCidr` 又留空。请显式设置该值。 |
| `ipamType is not set in the cni configmap` / `ipStack is not set in the cni configmap` | 手写的 `eni_conf` 缺少该开关。两者均为必填项、没有默认值，需要显式设置。 |
| `unsupported ipStack <取值> in configMap` | `zec.ipStack` 既不是 `ipv4` 也不是 `dual`。 |
| `ipStack "ipv6" (IPv6-only) is not implemented yet` | IPv6 单栈是被刻意拒绝的，并非拼写错误。请使用 `dual` 或 `ipv4`。 |
| `unsupported daemon mode` | 仅支持 `eni_multi_ip`。 |

direct 模式特有的启动失败参见 [docs/deploy-direct-cn.md](docs/deploy-direct-cn.md) §6。

**Node 事件。** 导致 Pod 无法获得地址的故障会以 Warning 事件记录在 Node 上，无需查看 Pod 日志即可查到：
OpenAPI 相关的事件（下表中除 `DaemonStartFailed` 与 `InterfaceExcluded` 外的各项，以及上文
`InstanceVerified` 的失败原因）由 controlplane 写入，同一原因只记录一次；另外两项由 daemon 写入：
`DaemonStartFailed`（Warning），daemon 处于 `CrashLoopBackOff` 时同样可见；`InterfaceExcluded`
是 **Normal** 事件。
`kubectl describe node <node>` 也会列出这些事件；需要过滤时：

```sh
kubectl get events -n default --field-selector involvedObject.kind=Node,involvedObject.name=<node>,type=Warning
```

| Reason | 原因 |
| --- | --- |
| `CredentialServiceAbnormal` | OpenAPI 拒绝了凭证，或未配置凭证。请检查 Secret。 |
| `OpenApiPermissionDenied` | 凭证有效，但缺少 controlplane 所调用的某个 API 的权限，message 中带有该 API。补充授予权限即可，更换密钥无济于事。 |
| `NodeClockSkew` | 请求因过期被拒：调用方（controlplane 所在节点）的时钟落后于平台。时钟超前不会以这种方式被发现。请先修复时间同步，更换凭证无济于事。 |
| `OpenApiUnreachable` | 无法连通 OpenAPI 端点，或由端点前面的代理、网关、拦截方代为应答。OpenAPI 没有内网端点，运行 controlplane 的节点必须具备公网出向。 |
| `AttachENIFailed` | 新建的 ENI 挂载到实例失败，message 中带有 attach 的错误以及该 ENI 是否已释放。 |
| `DaemonStartFailed` | daemon 启动失败，如配置错误、等待 Cilium agent 超时；message 即 daemon 退出时的错误。daemon 连接到 API server 之前的失败不会留下事件。 |
| `VpcResourceQuotaExceeded` / `InsufficientIpInSubnet` / `OpenApiFlowLimit` | 账号配额已满 / Pod 子网地址耗尽 / OpenAPI 限流。 |
| `InterfaceExcluded` | direct 模式：某块网卡（主网卡，或所在子网后来被移除的网卡）的子网不在 `zec.subnetIDs` 中，它不服务新 Pod，参见[容量规划](#容量规划)。无论是否影响容量都会记录：节点通常仍依靠辅助网卡保有正常容量。这是 **Normal** 事件，上文带 `type=Warning` 的过滤无法看到它。crd 模式下，同一情形表现为 `kubectl describe zn` 中该网卡的 `Excluded` 状态；节点因此无法放置任何 Pod 时为 `NoPodInterface`。 |

**Pod 卡在 `ContainerCreating`。** 首先确认节点上存在 `/opt/cni/bin/zec-cni`（由 init 容器
安装），再检查 `/etc/cni/net.d/10-zenlayer-cni.conflist` 与 `/var/run/zenlayer/cni.socket`
是否存在，然后查看 `/var/log/zenlayer/cni.log`。

该顺序有其原因：**conflist 不是由 init 容器安装的**，而是由 daemon 在开始服务之后写入，因此
conflist 缺失指向 daemon，而不是 init 容器（后者会成功退出，且不缺少任何内容）。启用 Cilium
串接时，daemon 还会先阻塞等待 `/var/run/cilium/cilium.sock`，此时 conflist 缺失意味着
Cilium agent 未启动。daemon 也要等待 controlplane 核验本节点（`InstanceVerified=True`）后
才开始服务，因此 conflist 缺失时同样需要查看 `kubectl get zn`。参见[架构](#架构)。

## 延伸文档

- [direct 模式部署](./docs/deploy-direct-cn.md)：每个节点各自持有凭证、直接调用 OpenAPI 的部署方式，
  包括适用场景、安装步骤，以及在温池、GC、故障排查方面与集中式部署的差异。
- [与 Cilium 串接部署](./docs/deploy-cilium-chained-cn.md)：推荐模式 `cni.mode=chained`，
  包括前提条件、从 veth 切换过来的完整步骤、每一个必填的 Cilium value 及其理由，以及回退流程。
  启用网络策略前请先阅读此文档。
- [delegated IPAM 模式](./docs/deploy-cilium-netkit-cn.md)：`cni.mode=delegated` 的完整步骤，
  由 Cilium 创建 Pod 设备（netkit 或 veth），本插件只分配地址并安装宿主 per-ENI 路由。
- [性能对比与测量方法](./docs/performance-cn.md)：各形态的测量数据、哪些开关真正有收益，
  以及跨会话对比为什么不可信。选择形态前请阅读此文档。
- [CHANGELOG.md](./CHANGELOG.md)：各版本的变更记录，包含当前版本的已知限制。
- [CONTRIBUTING.md](./CONTRIBUTING.md)：构建、测试、lint 命令，以及 PR 的检查项。

## 贡献

构建、测试与评审流程参见 [CONTRIBUTING.md](CONTRIBUTING.md)。参与本项目须遵守
[行为准则](CODE_OF_CONDUCT.md)。

## 安全

请以私下方式报告漏洞，参见 [SECURITY.md](SECURITY.md)。

### 安全地运行 `zec.ipamType=crd`

`crd` 模式下节点不持有 OpenAPI 凭证，每个节点也只能修改自身的 `ZecNode`。以下两项仍需由集群运维人员负责：

- **节点加入后立即删除 bootstrap token。** 有效的 bootstrap token 可以为**任意**节点名申请 kubelet 证书
  （Kubernetes 批准这类申请时不检查名称），持有者能够注册新节点，也能冒充已有节点。请在添加节点前再生成 token
  （`kubeadm token create --print-join-command`，有效期 24 小时），节点添加完成后立即删除（`kubeadm token delete <id>`），
  并使用 `kubeadm token list` 检查是否有残留；不要使用 `--ttl 0` 生成永久 token。
- **节点名与其核验过的实例绑定。** 若 Node 被删除后重新注册，却声称是另一台实例，而原实例仍然存在，
  controlplane 会拒绝（`InstanceVerified=False`，原因为 `BoundToOtherInstance`）。原机器删除后复用主机名无需任何操作；
  原机器仍在运行时将该名称转给新机器，需要先清除记录：`kubectl delete zecnode <节点名>`。

## 变更记录

各版本的发布说明参见 [CHANGELOG.md](./CHANGELOG.md)。

## 许可证

Apache License 2.0。参见 [LICENSE](LICENSE)。
