# Zenlayer VPC CNI for Kubernetes

[English](README.md) | 简体中文

一个为 Kubernetes Pod 提供 Zenlayer VPC 原生地址的 CNI 插件。Pod IP 是挂载在节点上的
Zenlayer 弹性网卡（ENI）的辅助 IP，因此 Pod 流量由 VPC 自身路由 —— 没有 overlay，没有封装，
并且 Pod 地址在 VPC 内的任意位置都可达。

当前状态：插件支持 `linux/amd64` 节点上的 `eni_multi_ip` 网络模式。每个节点要么只提供 IPv4，
要么提供双栈，参见[地址族](#地址族)。

## 能力矩阵

| 能力 | 状态 |
| --- | --- |
| IPAM 模式 | `eni_multi_ip` —— Pod IP 是 VPC 弹性网卡上的一个辅助 IP |
| datapath | veth pair + per-ENI 策略路由。`delegated` 模式下 Pod 设备由 Cilium 创建 |
| 地址族 | IPv4 或双栈。IPv6 单栈未实现 |
| 节点平台 | 仅 `linux/amd64` |
| Kubernetes | >= 1.28.2（chart 的 `kubeVersion` 约束） |
| Service 转发 | 由 kube-proxy 或你自己部署的替代品提供。本插件不提供 |
| NetworkPolicy | 默认不执行。可通过串接上游 Cilium 获得 —— 见[网络策略](#网络策略) |
| Cilium（串接） | 推荐 v1.15 ~ v1.18；按 v1.17.5 源码核对，并在 v1.17.5 集群上验证 |
| SNAT / iptables 规则 | 无。Pod 需要公网 IPv4 出向时，需在 VPC 侧配置 NAT 网关 |
| Pod 带宽限速 | 未实现。出向整形可通过 Cilium 的 bandwidth manager 获得 |
| `hostPort` | 支持，由持有数据面的那一方实现。不启用 Cilium 时 conflist 串接上游 `portmap` 插件（二进制随镜像分发）；`cilium.enabled=true` 时由 Cilium 自己的 BPF 负载均衡承担，**conflist 不再串接 `portmap`**。此时**端口要选在 30000 以下**：Cilium 会拒绝任何落在 NodePort 范围内的 hostPort，除一条 warning 外没有任何提示，见 [docs/deploy-cilium-netkit-cn.md](docs/deploy-cilium-netkit-cn.md) §4.5。不过 Pod IP 本身在 VPC 内可路由，多数场景直接用 Pod IP 或 Service 更合适 —— hostPort 会把 Pod 钉在特定节点，并在同节点上产生端口冲突 |
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

在一个已有的 Zenlayer VPC 实例集群上，只需三步。每一步的具体作用见[安装](#安装)，
前置条件见[环境要求](#环境要求)。

```sh
# 1. 凭证。刻意不由 chart 渲染，因此它们永远不会进入 values.yaml、git 或 Helm release。
kubectl create secret -n kube-system generic zenlayer-cni-access-key \
  --from-literal=AccessKeyID='<access-key-id>' \
  --from-literal=AccessKeyPassword='<access-key-secret>'

# 2. 安装。这三个 --set 值是必填的，缺少任意一个都会导致渲染失败。
helm install zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 1.0.0 \
  --namespace kube-system \
  --set-string zec.vpcID=<vpc-id> \
  --set-string 'zec.subnetIDs={<subnet-id-1>,<subnet-id-2>}' \
  --set-string zec.securityGroupID=<security-group-id>

# 3. 等待 DaemonSet 就绪，然后确认 Pod 确实拿到了 VPC 地址。
kubectl -n kube-system rollout status ds/zenlayer-vpc-cni
kubectl run nettest --image=busybox --restart=Never -- sleep 3600
kubectl wait --for=condition=Ready pod/nettest --timeout=60s
kubectl get pod nettest -o jsonpath='{.status.podIP}{"\n"}'
kubectl delete pod nettest
```

Pod IP 应当是你传入的某个子网中的地址。

有几个默认值值得留意：每个节点 2 个 IP 的 warm pool，以及从 `/etc/zec_secret/` 读取凭证。
datapath 只有一种 —— 参见[datapath](#datapath)。

## 架构

本仓库会构建出两个二进制：

| 二进制 | 源码目录 | 职责 |
| --- | --- | --- |
| `zec-cni` | `cmd/zenlayer-vpc-cni` | kubelet 在 Pod sandbox 创建/销毁时执行的 CNI 插件。它自身不做任何 IPAM：通过 gRPC 连接节点上的 daemon，然后配置 Pod 的 netns。 |
| `zec-ipam` | `cmd/cni-ipam` | 每节点一个的 daemon（以 DaemonSet 运行，`hostNetwork: true`）。它调用 Zenlayer OpenAPI，挂载 ENI，分配和释放辅助 IP，维护 warm pool，并提供 gRPC API。 |

两者通过 `/var/run/zenlayer/cni.socket` 上的 Unix domain socket（gRPC）通信。插件所需的一切
—— 分配到的 IP、该 IP 所属的 ENI、网关、service CIDR —— 都在 `AllocIP` 的响应中返回。

daemon 还会：

- 从 `kube-system` 命名空间下的 `zenlayer-cni-config` ConfigMap（键为 `eni_conf`）读取自身配置，
  并从 `/etc/zec_secret/` 下的文件读取 OpenAPI 凭证；
- 将资源状态持久化到 `/var/lib/cni` 下的 BoltDB 中；
- 注册一个 kubelet device plugin，把每节点的可分配地址数上报为扩展资源 `zenlayer.com/eni-ip`
  （由 `eni_conf` 中的 `disableDevicePlugin` 关闭，chart 并不渲染该字段 —— 参见
  [配置参考](#配置参考)）。
  **该上报值只对在 `resources.requests` 中显式声明了 `zenlayer.com/eni-ip` 的工作负载起调度约束**，
  详见[容量规划](#容量规划)；
- 在节点自身的网络命名空间中，于 `healthAndDebugPort`（默认 `11414`）上提供 `/healthz`、
  `/metrics`（Prometheus）。

一个 init 容器（`script/bootstrap/install_env.sh`）负责把 `zec-cni` 与上游的 `loopback`
插件安装到宿主机上。conflist 不由 init 容器安装 —— 该文件由 daemon 在开始服务之后写入，
因此 kubelet 不会在 daemon 能够应答之前调用插件。

每隔 `reconcileIntervalSec`（以及分配 IP 遇到配额错误时按需触发），daemon 会执行一次
**GC**：把每个 warm pool 与 Pod 实际持有的资源做比对，释放已经泄漏的部分。这件事有三个
名字，这里统一一下：配置项叫 `reconcileIntervalSec`，日志里叫 `Daemon GC`，本文档一律称 GC
。启动后的首轮会被跳过，
重复触发被防抖到每分钟至多一次。

Pod 已经消失的记录**不会一发现就回收**：第一轮只打标，满一小时后才连同指向该地址的策略路由
规则一起删掉，因此地址会在「daemon 第一次注意到」之后的 **1 小时 ~ 1 小时 20 分**回池。该计时
为内存态，随 daemon 重启重新开始，所以重启频率高于这个时长的 daemon 不会走到回收那一步 ——
影响的是容量，而非连通性。

GC **在整个执行期间暂停整个池**，包括其中的 OpenAPI 调用，因此该节点上的 Pod 网络配置会排队
等待，而 CNI ADD 的超时是 118 秒。daemon 会记录每一轮 GC 的耗时，偏长时打 warning —— 如果
出现 Pod 偶发起不来，先看 `Daemon GC finished in` 这条日志。

IP 分配遵循 warm pool 策略：daemon 会保持 `poolTarget` 个空闲 IP 处于就绪状态，因此创建 Pod
通常无需等待一次 OpenAPI 往返。`poolTargetMin` 是池的下限，`poolTargetMax` 是上限 —— 两者约束的
都是池的**总量**（含已被 Pod 占用的地址），而非空闲部分。启用 `poolTargetCapProbe`（默认）后，
daemon 会探测实例真实的 ENI/IP 容量并以此为上限，`poolTargetMax` 不再生效；参见
[配置参考](#配置参考)。

Service 流量**不**由本插件处理。这里没有 service 负载均衡器，也没有 NetworkPolicy 实现 ——
Service 由 kube-proxy（或你自行安装的替代组件）处理。

## datapath

由哪个组件创建 Pod 设备，取决于 `cni.mode`：

- **`chained`（默认）**：由本插件创建 veth pair（`pkg/plugin/datapath/policy_route.go`）。
  Pod 直接抵达宿主机协议栈，因此 kube-proxy 无需额外配合就能看到 service 流量的**两个方向**，
  这也正是 Cilium `generic-veth` CNI chaining 所要求的设备形态。
- **`delegated`**：Cilium 作为主 CNI 创建 Pod 设备 —— 按它自己的 `bpf.datapathMode`
  取值，可能是 netkit 设备，也可能是 veth。本插件在该模式下不创建任何设备，
  见 [docs/deploy-cilium-netkit-cn.md](docs/deploy-cilium-netkit-cn.md)。

**per-ENI 策略路由在两种模式下完全相同，且始终由本插件安装。** 每个 ENI 拥有自己的路由表
（`1000 + ifindex`），其中保存该 ENI 的默认路由，并由一条 `from <pod-ip> lookup <eni-table>`
规则把 Pod 的出向流量导入该表。

`zec.dataPath` 渲染进 conflist 的 `eniip_virtual_type` 字段，**只接受 `veth`**。
它描述的是本插件自己创建的设备，因此在 `cni.mode=delegated` 下不渲染。

**本插件不做地址伪装（SNAT），也不安装任何 iptables 规则。** Pod 地址是 ENI 的辅助 IP，
在 VPC 内可直接路由，东西向流量以 Pod 自身地址收发，不经过任何转换。因此
**Pod 访问公网 IPv4 依赖 VPC 侧的 NAT 网关** —— 需要为 Pod 地址所属的子网配置 NAT 网关，
否则 Pod 没有公网 IPv4 出口。公网 IPv6 走的是另一条路径：由平台直接为网卡分配公网地址，
见[地址族](#地址族)。

**本插件不实现 Pod 带宽限速。** conflist 不声明标准 CNI `bandwidth` runtime capability：
本插件不消费 `runtimeConfig.bandwidth`，声明它只会让 kubelet 把带宽注解交给一个不予处理的
插件，Pod 实际不受限而 conflist 却宣称支持。

出向限速可以由 Cilium 提供（需启用，见[网络策略](#网络策略)）：它的 bandwidth manager 从
Pod 的 `kubernetes.io/egress-bandwidth` 注解取值，用 EDT 做整形，由 Cilium 自己 values 里的
`bandwidthManager.enabled=true` 打开。依赖它之前要知道三件事：

- **只有出向。** Cilium 官方文档写明 `kubernetes.io/ingress-bandwidth`
  不支持、不推荐、**会被忽略**。
- **IP 在辅助 ENI 上的 Pod 可能不受限。** Cilium 按「持有默认路由，或被赋予节点
  `InternalIP` / `ExternalIP`」来挑选要装 `fq` 的设备，那描述的是主 ENI；而 IP 落在辅助
  ENI 上的 Pod 走的是该 ENI 自己的路由表，两个条件都不满足。用
  `cilium-dbg status | grep BandwidthManager` 看实际生效的设备列表。
- **由 Pod 注解驱动，与 CNI 链无关，且不需要任何 CRD。** Cilium 的 bandwidth manager 从不读
  `runtimeConfig`，所以在 conflist 里重新声明这个 capability 既不是它的前提、也帮不上它；
  也没有带宽相关的 CRD，只有注解（`kubernetes.io/egress-bandwidth`，以及做优先级分级的
  `bandwidth.cilium.io/priority`）。附带一个好处：Cilium 的 pod watcher 会响应注解变化，
  所以改运行中 Pod 的注解**立即生效，不需要重建**。

本 chart **不提供**自己的带宽开关：`bandwidthManager.enabled` 属于 Cilium 的 release，
本 chart 既读不到也改不了，此处的开关只能声明功能已启用，无法校验其实际生效。

验证步骤与其余上游限制见 [docs/deploy-cilium-chained-cn.md](./docs/deploy-cilium-chained-cn.md)。

## 地址族

`zec.ipStack` 决定一个节点提供哪些地址族：

| 取值 | 含义 |
| --- | --- |
| `ipv4` | 默认值，留空等同于该值。Pod 获得一个 IPv4 地址。 |
| `dual` | Pod 获得一个 IPv4 地址和一个 IPv6 地址。 |
| `ipv6` | 尚未实现。 |

启用 `dual` 之前，有四项性质需要了解。

**每个配置的子网都必须同时具备 IPv4 和 IPv6 CIDR。** 任一子网缺少某个族，daemon 就拒绝
启动，并指明是哪个子网缺哪个族。**这个检查放在启动时而不是分配时，是刻意的**：分配时只发
一条 warning 的话，和「平台变慢」无法区分，症状会是 warm pool 永远填不满、所有 Pod 卡在
`ContainerCreating`，而唯一的线索只有那一行 warning。

**一对地址不可拆分。** Pod 的 IPv4 与 IPv6 地址作为一个整体交付；只覆盖单个族的资源永远不会
交给 Pod，而是被打标并回收。这正是「平台部分失败之后，Pod 不会带着半套地址启动」的保证。

**IPv6 不构成节点容量约束。** 平台为每块 ENI 下发一个 `/96` 前缀，daemon 在本地从中切出
具体地址 —— 不存在按地址申请的 IPv6 接口。一个 `/96` 能容纳的地址数远超任何 ENI 的服务
能力，因此
`zenlayer.com/eni-ip` 与 warm pool 仍然只按 IPv4 计算容量，两种模式下节点上报的容量相同。
参见[容量规划](#容量规划)。

**公网 IPv6 是计费项，daemon 必须声明计费方式。** 当子网的 `ipv6Type` 为 `Public` 时，平台
拒绝在未提供计费类型和带宽上限的情况下为网卡启用 IPv6，而为自建网卡启用 IPv6 的正是本插件。
因此 `zec.ipv6InternetChargeType` 与 `zec.ipv6Bandwidth` 决定了集群的计费方式。在私网 IPv6
的子网上它们会被忽略。`ByTrafficPackage` 另需 `zec.ipv6PackageSize`，`BandwidthCluster`
另需 `zec.ipv6ClusterId`；两者都由 chart 在渲染时、daemon 在启动时各校验一次。
参见[配置参考](#配置参考)。

由于 IPv6 地址是从网卡前缀中本地切分的，而非逐个向平台申请，IPv6 的分配状态由 daemon 自身
维护 —— 依据本地记录与 API Server 上报的 Pod 地址。IPv4 则在每一轮 GC 中额外与平台对账。

本插件的双栈支持**不要求** Kubernetes 集群本身是双栈；在其之上叠加 Cilium 串接才会引入这项
要求，参见[网络策略](#网络策略)与
[docs/deploy-cilium-chained-cn.md](docs/deploy-cilium-chained-cn.md) §4.3。

## 网络策略

**默认不执行 NetworkPolicy。** 本项目不自带策略引擎，因此 NetworkPolicy 对象会被 apiserver
正常接收然后被忽略 —— 一条本该阻断流量的规则只是静默地不生效，不会有任何组件报错。

`cilium.enabled=true` 会通过 Cilium 的 `generic-veth` chaining 模式，把**社区原版 Cilium**
串在本插件之后；`networkPolicy.enabled=true` 则声明「策略必须被执行」—— provider 为 `cilium`
时后者要求前者，否则 chart **直接拒绝渲染**，而不是接受一批无人执行的策略对象。两个开关相互
独立：只需要 Hubble 可观测性而不需要策略执行时，只开 `cilium.enabled` 即可。

在 `cni.mode=chained` 下，veth pair 与 per-ENI 策略路由**完全不变**，Cilium 只增加 endpoint
注册和宿主侧 veth 上的 tc eBPF 策略执行；在 `cni.mode=delegated` 下，设备由 Cilium 自己创建、
策略也在该设备上执行，而 per-ENI 路由仍归本插件。它不接管 IPAM、
也不接管路由 —— chainer 硬编码了 `ExternalIpam: true` 与 `RequireRouting: false`。

本项目**不 fork、不打补丁、不 vendor** Cilium，chart 也不安装它：请从 Cilium 自己的官方
chart 部署。支持范围 **v1.15 ~ v1.18**，已按 v1.17.5 做代码核对。

启用前有四件事必须知道：

- **`cni.customConf=true` 是强制的，`cni.exclusive=false` 建议一并设。** 前者是
  `generic-veth` 的硬性前提：Cilium **没有 `generic-veth` 的 conflist 模板**，所以
  `customConf=false` 与它自相矛盾，**Cilium 自己会挡住** —— agent 会卡在 `0/1 Running`
  永不 Ready（日志刷 `invalid CNI chaining mode: generic-veth`），而 **Pod 网络与
  NetworkPolicy 照常工作**。配错不会静默降级、不会丢策略、不会掉 Pod，只会亮一盏永久红灯。
  后者在本模式下是冗余的（`customConf=true` 时该键根本不会进入 configmap），但换用 Cilium 能
  渲染的串接模式时会重新生效，同时设的代价为零。
- **改 `cni.*` 或 `extraConfig.*` 之后必须显式重启 cilium agent。** 这些键只落 ConfigMap、
  不改 Pod template，`helm upgrade` 不会滚动 DaemonSet，而 `kubectl rollout status ds/cilium`
  会**立即返回成功（假绿）**。用 `kubectl -n kube-system rollout restart daemonset/cilium`。
- **L7 策略（HTTP/DNS/FQDN）、透明加密（IPsec / WireGuard）、Cilium Ingress / Gateway API
  在这个模式下都拿不到。** 如果上 Cilium 的目的是 L7 策略，这个方案不满足。更要紧的是 **L7 与
  FQDN 策略的失效方式是「放行」**：API Server 接受策略对象，节点上的 Cilium 从不装载它，本该
  被拒绝的流量全部通过；只用 `toFQDNs` 的策略甚至会被标记为 `Valid=True`。隔离需求请用原生
  NetworkPolicy 或纯 L3/L4 的 CiliumNetworkPolicy 表达，详见
  [docs/deploy-cilium-chained-cn.md](docs/deploy-cilium-chained-cn.md) §4.2 的 `l7Proxy` 行。

- **开关名拼错会被拒绝，不会静默地「没开」。** `values.schema.json` 在根层与各段都设了
  `additionalProperties: false`，所以 `--set celium.enabled=true`（把 `cilium` 拼错一个
  字母）会在渲染阶段失败并指出是哪个键：`additional properties 'celium' not allowed`。

- **`zec.ipStack=dual` 叠加串接时，要求 Kubernetes 集群本身是双栈。** 此时 Cilium 必须以
  `ipv6.enabled=true` 部署，而这又要求每个 Node 对象都带 IPv6 地址 —— 这是集群级前提，
  任何 Cilium value 都替代不了。两半都不能省：不开 `ipv6.enabled=true`，agent 的每-veth BPF
  会把所有 IPv6 报文当作不支持的 L3 协议丢弃，而且是**静默**丢弃 —— agent 报告健康、每个
  endpoint 都列出正确的 IPv6 地址、info 以上级别没有任何日志；在单栈集群上开启它，则 agent
  拒绝启动。本插件自身的双栈支持不依赖集群是双栈，这项要求只由叠加串接引入。详见
  [docs/deploy-cilium-chained-cn.md](docs/deploy-cilium-chained-cn.md) §4.3。

完整步骤、每一项必填 values 的理由、验证方法，以及**两个方向的模式切换流程**（纯 veth ⇄ 串接）
见 [docs/deploy-cilium-chained-cn.md](./docs/deploy-cilium-chained-cn.md)。

**卸载 Cilium 之前请先读那一节：顺序是固定的，滚动重启工作负载不是可选项。** Pod veth 上的
tc/tcx BPF 程序是 BPF link，**不随 agent 卸载消失，`helm uninstall` 也不清理**。agent 还在跑时
它会持续维护这些程序依赖的负载均衡 map；**一旦卸载，map 就冻结了** —— 串接期创建的 Pod 会继续
用一份永不更新的表来解析 ClusterIP。此后该 Service 的后端一旦变动，这些 Pod 发往已下线后端的
连接即被丢弃，且 `KUBE-SVC-*` 计数保持为 0，沿 kube-proxy 方向排查不会有任何发现。

## 环境要求

- Kubernetes >= 1.28.2（chart 的 `kubeVersion` 约束）。
- `amd64` 架构的 Linux 节点。DaemonSet 对 `kubernetes.io/os=linux` 和
  `kubernetes.io/arch=amd64` 设置了硬性 node affinity，镜像构建也会拒绝其他 `GOARCH`。
- 节点为 Zenlayer VPC 实例，具备一个 VPC、该 VPC 中最多 5 个用于分配 Pod IP 的子网，
  以及一个供 daemon 创建 ENI 时使用的安全组。
- Zenlayer OpenAPI 静态凭证（access key ID + password），并具有管理 ENI 及其辅助 IP 的权限。
- **每个**节点都具备**公网出向**能力 —— 节点绑定 EIP，或其所属 VPC 配置 NAT 网关。
  Zenlayer OpenAPI 是公网端点，而 daemon 在它所在的节点上直接调用它，所以访问不了公网的
  节点一个 Pod 地址也分配不出来。这与下面那条 Pod 出向不是一回事：后者只限制 Pod 能访问什么。
- 使用 Helm 3.8 及以上版本安装。3.8 是 `oci://` 仓库支持正式可用的版本，本文档的安装命令都从
  Docker Hub 上的 OCI 仓库取 chart。若只用本地 chart 目录安装，3.1 即可 —— 这是 chart 的 NOTES
  用到的 `lookup` 函数所要求的下限。
- 每个节点的 Linux 内核 >= 6.7，**当** `cni.mode=delegated` 且 Cilium 使用
  `bpf.datapathMode=netkit` 或 `netkit-l2` 时：低于该版本没有 netkit 设备。
  默认的 `chained` 模式不额外引入内核要求。
- kube-proxy，或其他提供 Service 处理能力的组件。在 `cni.mode=delegated` 下该组件即为
  Cilium，其 `kubeProxyReplacement` 在该模式下是必选项。
- 若 Pod 需要访问公网 IPv4：为 Pod 地址所属的子网配置 NAT 网关。本插件不做 SNAT，
  见[datapath](#datapath)。

## 安装

### 1. 创建凭证 Secret

凭证刻意**不**由 chart 渲染，因此它们永远不会落到 `values.yaml`、git 或 Helm release 中。
请在 `kube-system` 命名空间中单独创建该 Secret：

```sh
kubectl create secret -n kube-system generic zenlayer-cni-access-key \
  --from-literal=AccessKeyID='<access-key-id>' \
  --from-literal=AccessKeyPassword='<access-key-secret>'
```

daemon 会以只读方式将其挂载到 `/etc/zec_secret/`，并按文件读取这两个键
（`AccessKeyID`、`AccessKeyPassword`）。如果你给 Secret 取了别的名字，
请通过 `--set zec.secretName=<name>` 传入。

### 2. 安装 chart

```sh
helm install zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 1.0.0 \
  --namespace kube-system \
  --set-string zec.vpcID=<vpc-id> \
  --set-string 'zec.subnetIDs={<subnet-id-1>,<subnet-id-2>}' \
  --set-string zec.securityGroupID=<security-group-id>
```

> **三个 ID 必须用 `--set-string`。** `--set` 会推断类型，纯数字的 ID 会被解析成
> int64，schema 以 `got number, want string` 拒绝。Zenlayer 的 ID 全是纯数字，
> 因此首次安装即会遇到该问题。

`zec.vpcID`、`zec.subnetIDs` 和 `zec.securityGroupID` 为必填项 —— 缺少它们渲染会失败。

双栈安装还需要给出 IPv6 计费参数：daemon 会为它创建的每块网卡启用公网 IPv6，而
`zec.ipv6InternetChargeType` 的默认值 `ByTrafficPackage` 要求同时给出流量包大小。
因此双栈安装要带上

```sh
  --set-string zec.ipStack=dual \
  --set-string zec.ipv6InternetChargeType=ByBandwidth --set zec.ipv6Bandwidth=1
```

或者保持默认计费方式并追加 `--set-json zec.ipv6PackageSize=0.1`。
两者分别计费什么见[地址族](#地址族)。

如果想先预览渲染出的 manifest：

```sh
helm template zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 1.0.0 \
  --set-string zec.vpcID=<vpc-id> \
  --set-string 'zec.subnetIDs={<subnet-id>}' \
  --set-string zec.securityGroupID=<security-group-id>
```

### 3. 验证

```sh
# ① DaemonSet 全部就绪，且没有重启
kubectl -n kube-system rollout status ds/zenlayer-vpc-cni
kubectl -n kube-system get pod -l app=zenlayer-vpc-cni -o wide

# ② 新建 Pod 拿到的是 VPC 子网里的地址（不是 kubeadm 的 podCIDR）
kubectl run nettest --image=busybox --restart=Never -- sleep 3600
kubectl get pod nettest -o jsonpath='{.status.podIP}{"\n"}'
kubectl delete pod nettest

# ③ 节点上：conflist 落盘且权限 0600、二进制带版本信息
ssh <节点> 'ls -l /etc/cni/net.d/10-zenlayer-cni.conflist; /opt/cni/bin/zec-cni version'

# ④ 节点上：池状态
ssh <节点> 'curl -s 127.0.0.1:11414/metrics | grep -E "^resource_pool_"'
ssh <节点> 'curl -s 127.0.0.1:11414/healthz'
```

三点需要注意：

- **`/healthz` 与 `/metrics` 开在节点的每一个地址上，不只是回环。** daemon 是
  hostNetwork，监听 `:11414` —— 这是必须的：kubelet 探针和 Prometheus 抓取都来自节点之外。
  上面的例子走 ssh 只是习惯，不是因为端口只对节点开放：镜像里确实没有 `curl`，但**有 `wget`**，
  所以 `kubectl exec` 也能取到同样的端点，不必登上节点：

  ```sh
  POD=$(kubectl -n kube-system get pod -l app=zenlayer-vpc-cni \
          --field-selector spec.nodeName=<节点> -o jsonpath='{.items[0].metadata.name}')
  kubectl -n kube-system exec $POD -c zec-ipam -- \
    wget -qO- http://127.0.0.1:11414/metrics | grep -E '^resource_pool_'
  ```

  这个监听上只有 `/healthz` 与 `/metrics`，刻意没有 pprof。
- **`zec-cni version` 应打印出非空的版本 / commit / build time。**
  `CNI Version: unknown` 或 `Git Commit ID: DEFAULT` 表示构建时 git 无法给出答案，
  该镜像不具备可追溯性。字段为空则说明链接期参数根本没传 —— 直接 `go build` 就是这个结果。
- daemon 的 `/healthz` 只有在成功访问 OpenAPI 并建立好 warm pool 之后才会开始响应，
  这也是 DaemonSet 预留了较宽松的启动窗口（30 x 10s）的原因。**但它反映的是 daemon，不是
  节点** —— conflist 是在这之后才落盘的，所以在 Cilium 串接模式下，daemon 仍在等待 Cilium
  agent、该节点一个 Pod 都收不了的时候，`/healthz` 依然返回 `ok`。两个探针都只打 `/healthz`，
  因此这种状态下节点会一直是 Ready，**监控还要看重启计数与日志里的
  `Still waiting for the cilium agent`**。详见
  [docs/deploy-cilium-chained-cn.md](docs/deploy-cilium-chained-cn.md) §4.1。

## 容量规划

单节点可分配的 Pod 地址数由实例规格决定：

```text
容量 = (ENI 配额 - 本插件之外挂载的 ENI 数) x (每 ENI 地址数 - 1)
```

| 规格 | ENI 配额 | 每 ENI 地址数 | 本插件可分配 |
|---|---|---|---|
| 8 vCPU | 2 | 15 | **28** |
| 16 vCPU | 4 | 30 | **116** |

每块 ENI 都要为自己保留一个地址 —— 实例自带的 ENI 与本插件自建的一样，因此各自贡献
「每 ENI 地址数减一」。

以其他方式挂载到节点上的辅助 ENI，**若位于已配置的 Pod 子网内**，会被**纳管**：其地址加入
warm pool，并计入上表数值。

⚠️ **被纳管的 ENI 在池收缩时会被卸载并删除**；在 `zec.ipStack=dual` 下，daemon 还会
为它启用 IPv6 —— 在 `ipv6Type` 为 `Public` 的子网上这是一次产生费用的操作。若某块 ENI 挂载
用于其他用途（存储、管理、监控），请给它打上 `zenlayer:cni:no-manage=true` 标签将其排除；
也可以设置 `zec.manageUntaggedENI=false` 让整个节点不纳管。

**未被纳管的 ENI** —— 无论是被上述两种方式排除，还是本身就位于已配置 Pod 子网之外 ——
会被完全保留：其地址永不被使用，也不会被卸载。但它仍占用实例的一个 ENI 槽位，因此该节点的
容量会比上表少「一块 ENI 的地址数」—— 这就是下面启动日志里 `interface slot(s)` 计的那个数。

daemon 启动时会打印它解析到的各项数值。当某个节点的容量与上表不符时，这是最快的核对方式：

```sh
kubectl -n kube-system logs <daemon-pod> -c zec-ipam | grep -E 'Attached eni|InstanceLimit Updated'
# Attached eni created by cni: [], adopted: [], primary: [<primary-mac>]
# Attached eni not managed by cni: [<mac-1> <mac-2>]; 2 interface slot(s) held without serving pod addresses (ENIUnmanaged)
# InstanceLimit Updated {ENIQuota: 4, IPv4MaxPerENI: 30, IPv6CidrMaxPerENI: 1, ENIManaged: 0, ENIUnmanaged: 2, Cordon: false, allocatable: 58}
```

其中 `ENIUnmanaged` 是本插件不纳管的 ENI 数，`allocatable` 是扣除它们之后的容量。节点上报
值会略低：device plugin 还要再减去不声明 `zenlayer.com/eni-ip` 的存量 Pod。

**上表都是 IPv4 地址数，`zec.ipStack=dual` 不会改变它们。** Pod 地址对中的 IPv6 那一半是
daemon 从 ENI 的 `/96` 前缀里本地切出来的，而一个 `/96` 能容纳的地址数远超任何 ENI 的服务能力，
因此 IPv6 永远不会成为约束项。两种模式下节点上报的 `zenlayer.com/eni-ip` 容量相同。

> **但真正的上限往往不是这个数，而是 kubelet 的 `maxPods`（默认 110）。** 16 vCPU 节点上
> 本插件能给出 116 个地址，而 kubelet 只肯调度 110 个 Pod（还要减去 hostNetwork 的系统
> Pod）。规划时取两者的较小值；想用满则需同时调大 `maxPods`。

device plugin 会把可分配数上报成扩展资源 `zenlayer.com/eni-ip`。**这个上报值是滞后的**（约 5 分钟
刷新一次，且等于「容量 − 不声明该资源的存量 Pod 数」），排查时想看实时值请读节点上的
`resource_pool_*` 指标。节点繁忙时该上报值可能明显低于实际容量，节点空载后回升。它是下界、
不会虚报，但**不能当作实时容量读取**。

**该上报值只约束显式声明了该资源的 Pod。** 调度器的资源筛选只检查 Pod 在
`resources.requests` 里实际声明过的资源，本插件也不会自动注入这项声明。因此一个
`resources` 为空的普通 Pod，即便节点的 `zenlayer.com/eni-ip` 已经是 `0`，仍会被调度过去，
随后因拿不到地址而卡在 `ContainerCreating`，直到 CNI ADD 超时并由 kubelet 反复重试。

想让调度器真正按 IP 容量分配，有两种做法：

- 给工作负载显式声明 `resources.requests: {zenlayer.com/eni-ip: "1"}`（配合 `limits` 同值）；
- 或者按[容量规划](#容量规划)的公式把 kubelet 的 `--max-pods` 设成该实例的可分配地址数，
  由 Pod 数上限间接约束。

另有一条节点侧的兜底：Pod 带上 `k8s.zenlayer.com/pod-evict-policy: allow` 注解后，
daemon 在分配地址前会先比较本节点已记录的 Pod 数与容量上限，达到上限就驱逐该 Pod，
使其重新调度到别处，而不是让它一直卡住。不带该注解的 Pod 不受此逻辑影响。

## 配置参考

下面所有配置项都位于 `chart/values.yaml`。一个 `zec.` 值最终落到哪里，取决于谁读它 ——
这一点从前缀上看不出来：

| 渲染到 | 配置项 |
| --- | --- |
| `zenlayer-cni-config` ConfigMap 的 `eni_conf` 键 | `zec.` 下的大部分 —— daemon 从配置文件里读的那些 |
| CNI conflist | `zec.dataPath` |
| 两处都渲染 | `zec.logLevel` |
| **DaemonSet 本身，不进任何 ConfigMap** | `zec.k8sServiceHost`、`zec.k8sServicePort`、`zec.logLevel`（作为 `ZENLAYER_CNI_LOGLEVEL`）、`zec.healthAndDebugPort`（作为探针端口）、`zec.secretName`（作为挂载的 Secret） |

最后一行在「按节点覆盖配置」时很关键：那个机制**只替换 `eni_conf`**，所以这一行里的值
无法用它来改 —— 见下文。

| 配置项 | 默认值 | 说明 |
| --- | --- | --- |
| `image.repository` | `docker.io/zenlayer1/zeccni` | init 容器与 `zec-ipam` 共用的镜像。 |
| `image.tag` | `v1.0.0` | 镜像 tag。 |
| `image.pullPolicy` | `Always` | 镜像拉取策略。 |
| `zec.vpcID` | `""` | **必填。** Zenlayer VPC ID。所有子网都必须属于它。 |
| `zec.subnetIDs` | `[]` | **必填。** Pod IP 的来源子网。最多 5 个，且都在同一个 VPC 中。 |
| `zec.securityGroupID` | `""` | **必填。** 应用到 daemon 所创建 ENI 上的安全组。 |
| `zec.ipStack` | `ipv4` | Pod 获得的地址族：`ipv4` 或 `dual`。`dual` 要求每个子网都同时具备 IPv4 与 IPv6 CIDR。IPv6 单栈尚未实现。参见[地址族](#地址族)。 |
| `zec.ipv6InternetChargeType` | `ByTrafficPackage` | 平台对网卡公网 IPv6 的计费方式：`ByTrafficPackage`、`ByBandwidth` 或 `BandwidthCluster`。仅在 `ipStack` 为 `dual` 时使用；私网 IPv6 的子网上会被忽略。 |
| `zec.ipv6Bandwidth` | `10000` | 每块网卡的公网 IPv6 带宽上限，单位 Mbps，仅在 `ipStack` 为 `dual` 时使用。必须 >= 1。 |
| `zec.ipv6PackageSize` | `0` | 流量包大小，单位 TB。`ipv6InternetChargeType` 为 `ByTrafficPackage` 时必填，取值为 0.1 的正整数倍。请用 `--set-json` 传入：`--set` 会把小数当作字符串。 |
| `zec.ipv6ClusterId` | `""` | 网卡要加入的共享带宽包。`ipv6InternetChargeType` 为 `BandwidthCluster` 时必填。 |
| `zec.networkMode` | `eni_multi_ip` | 网络模式。仅支持 `eni_multi_ip`。 |
| `zec.dataPath` | `veth` | Pod datapath，渲染为 `eniip_virtual_type`。**只接受 `veth`**；在 `cni.mode=delegated` 下不渲染该键，因为本插件在该模式下不创建设备 —— 参见[datapath](#datapath)。 |
| `zec.serviceCidr` | `""` | Service CIDR，多个用逗号分隔。**只有 kubeadm 建的集群才可以留空**：留空时 daemon 会去读 `kube-system/kubeadm-config`，读不到就起不来。datapath 确实不消费这个值 —— service 流量本来就经过宿主机协议栈 —— 但 daemon 仍要求从两个来源之一拿到 IPv4 的 service CIDR。双栈集群下缺 IPv6 那一项只会记一条 Info。 |
| `zec.secretName` | `zenlayer-cni-access-key` | 预先创建的、保存 OpenAPI 凭证的 Secret 名称。 |
| `zec.poolTarget` | `2` | 每个节点 warm（预分配、未使用）IP 的目标数量。 |
| `zec.poolTargetMin` | `1` | 池的总量下限（含已被 Pod 占用的地址），并非仅指空闲部分。默认值下 `poolTarget` 更严，本项通常不起作用；只有 in-use 较多时两者才会分叉。必须 <= `poolTarget`。 |
| `zec.poolTargetMax` | `10` | 池的总量上限（含已被 Pod 占用的地址），并非仅指空闲部分。**`poolTargetCapProbe` 为 `true`（默认）时该值被忽略**，上限改用实例的真实容量。关闭探测后它将直接成为该节点向调度器上报的容量，需按实例规格调大 —— 默认值只是最小规格实例的容量（两块网卡 x 每块 6 个地址，各扣除主 IP）。必须 >= `poolTarget`。 |
| `zec.poolTargetCapProbe` | `true` | 探测实例的 ENI/IP 容量；会放宽 `poolTarget*` 系列参数的下限约束。 |
| `zec.manageUntaggedENI` | `true` | 是否纳管本插件之外挂载到节点上的辅助网卡。设为 `false`，或给网卡打 `zenlayer:cni:no-manage=true` 标签，即可将其排除。参见[容量规划](#容量规划)。 |
| `zec.reconcileIntervalSec` | `1200` | GC 间隔，单位秒。参见[架构](#架构)中的说明 —— GC 执行期间会暂停整个池。 |
| `zec.poolMonitorIntervalSec` | `120` | 池监控间隔，单位秒。 |
| `zec.healthAndDebugPort` | `11414` | `/healthz` 与 `/metrics` 使用的端口。Pod 为 `hostNetwork`，因此该端口会占用节点端口。 |
| `zec.logLevel` | `""` | 两个二进制共用的日志级别（`trace｜debug｜info｜warn｜error｜fatal｜panic`），留空保持内置默认值 `info`。它渲染 DaemonSet 上的 `ZENLAYER_CNI_LOGLEVEL`；取 `debug` 或 `trace` 时还会在 conflist 中写入 `"debug": true` —— 这是 `zec-cni` 唯一的通道，因为 kubelet 从宿主机 exec 它，它看不到 Pod 的环境变量。 |
| `zec.k8sServiceHost` | `""` | 设置 `KUBERNETES_SERVICE_HOST`。当 hostNetwork 的 daemon 无法解析集群内 apiserver Service 时使用（例如 kube-proxy 尚未就绪）。 |
| `zec.k8sServicePort` | `""` | 设置 `KUBERNETES_SERVICE_PORT`。 |
| `cni.mode` | `chained` | 由谁创建 Pod 的网络设备。`chained`：本插件创建 veth 对，Cilium（若启用）串在其后。`delegated`：Cilium 创建设备，本插件只分配地址并装宿主 per-ENI 路由，要求 `cilium.enabled=true`；**已在测试集群端到端验证，尚未生产验证**，且**不要为性能选它**（见 [docs/performance-cn.md](docs/performance-cn.md)），步骤见 [docs/deploy-cilium-netkit-cn.md](docs/deploy-cilium-netkit-cn.md)。 |
| `cilium.enabled` | `false` | 串接上游 `cilium-cni`。`cni.mode=chained` 时它排在本插件**之后**（`generic-veth` chaining）；`cni.mode=delegated` 时它是主插件、本插件落在它的 `ipam` 块里。**不会**安装 Cilium，见[网络策略](#网络策略)。 |
| `cilium.waitTimeout` | `""` | daemon 等待 Cilium agent socket 的时长，超时则重启。Go duration 语法，留空为默认 5m。 |
| `networkPolicy.enabled` | `false` | 声明 NetworkPolicy 必须被执行。关闭时策略对象存在但无人执行；打开则要求 `cilium.enabled=true`。 |
| `networkPolicy.provider` | `cilium` | 期望的策略引擎，只接受 `cilium`。 |
| `resources.requests.cpu` | `250m` | `zec-ipam` 的 CPU request。 |
| `resources.requests.memory` | `256Mi` | `zec-ipam` 的内存 request。 |
| `resources.limits` | 未设置 | 同样被接受并直接渲染到容器上，但默认不设：daemon 处在每个 Pod 的 CNI ADD 路径上，CPU limit 会拖慢整个节点的 Pod 启动，内存 limit 则会把一次瞬时尖峰变成 OOMKill，把 IPAM 一起带走。 |

daemon 的配置 schema（`pkg/daemon/config/config.go`）还接受一些 chart 未渲染的字段，
其中包括 `disableDevicePlugin`、`extraRoutes`、`poolCapRatio`、`poolGCPeriodSec`、
`subnetStatAgingSec` 和 `subnetStatUpdateIntervalSec`。
如需设置，请直接编辑 ConfigMap 中的 `eni_conf` JSON。

`rateLimit` 会被解析但**没有任何消费者**：设置它不会有任何效果，也不会有任何告警。
不要用它来限制 OpenAPI 的调用频率。

也可以通过 `k8s.zenlayer.com/vpc-cni-config` 节点标签，把某个节点指向一个专属的 ConfigMap，
标签值格式为 `<namespace>.<name>`。有四件事容易搞错：

- **命名空间必须是 daemon 自己所在的那个**，写别的会被拒绝、daemon 起不来。标签打在 Node 上，
  而 kubelet 可以给自己的 Node 打任意非 `kubernetes.io` 前缀的标签 —— 这个前缀正是非
  `kubernetes.io` 的。不加这个限制，一台被攻破的节点就能让自己的 daemon 去读一份它自己写的
  配置，而 `eni_conf` 里的 `openAPIDomain` 决定了 daemon 带着凭据去访问哪台主机。

- **它只覆盖 `eni_conf`,别的都不覆盖。** daemon 只从标签指定的那个 ConfigMap 里读这一个键，
  键不存在就起不来。上面表格「DaemonSet 本身」那一行里的东西 —— 日志级别环境变量、探针端口、
  挂载的 Secret —— 来自 DaemonSet，无法用这种方式按节点改。
- **是整体替换，不是合并。** 指定的 `eni_conf` 会被整份采用，集群级那份的内容一点都不继承。
  你没写的键**不会**「沿用集群级的值」，而是没有设置 —— daemon 会套用自己的默认值，或者直接拒绝启动。
- **CNI conflist 仍然走集群级那一份。** 所以 `zec.dataPath` 也无法按节点覆盖。

### 配置变更如何生效

**没有任何配置项支持热生效。** 所有值都在 daemon 启动时读取一次，改动只有等该节点的
pod 重启才会生效。「只读一次、靠重启生效」是刻意的选择：一个可以中途变化的配置项，必须在
daemon 可能处于的每一个状态下都能安全改变 —— 包括正在挂载 ENI 的中途。

`helm upgrade` 会自动处理。DaemonSet 上带有针对渲染后 ConfigMap 的 `checksum/config`
注解，任何配置变化都会改变 pod 模板，从而触发滚动更新。

**手工编辑 ConfigMap 不会触发任何重启**，需要自己补一步：

```sh
kubectl -n kube-system rollout restart ds/zenlayer-vpc-cni
```

`helm upgrade` **失败之后也要补这一步**。helm 是逐个对象应用的，中途失败的升级
可能已经滚完了 DaemonSet 却还没写 ConfigMap —— 新起来的 pod 读到的是旧值。
之后即使有一次成功的 upgrade 也救不回来：那时 pod 模板已经没有变化，不会再滚一次，
这些节点就会长期停在旧配置上。

节点上的 CNI conflist 同样只在 daemon 启动时写入一次、之后不再重读，所以 `dataPath`
等 conflist 字段也适用同一条规则。

## 卸载

`helm uninstall` 只删除 Kubernetes 对象，节点本地的状态会**有意保留**：

| 每个节点上的残留 | |
| --- | --- |
| `/etc/cni/net.d/10-zenlayer-cni.conflist` | CNI 配置 |
| `/opt/cni/bin/zec-cni` | CNI 二进制 |
| `/var/lib/cni/zenlayer/` | 地址记录 |
| `/var/run/zenlayer/`、`/var/log/zenlayer/` | 日志目录；socket 随 pod 一起消失 |
| `/etc/sysctl.d/99-zenlayer-vpc-cni.conf` | `rp_filter` 配置片段 |

**其中真正要紧的是 conflist。** 只要它还在而后端 daemon 已经没了，kubelet 仍会认为
本插件负责该节点的 Pod 网络，新建 Pod 会一直卡在 `ContainerCreating`，而不会回退到
其它 CNI。

```sh
rm -f /etc/cni/net.d/10-zenlayer-cni.conflist
```

**只有当 `/etc/cni/net.d/` 下还有另一个 CNI 的 conflist 时才可以删。** 如果本插件是
该节点上唯一的 CNI，删掉之后 kubelet 就完全没有网络插件了：节点会变成 `NotReady`
并报 `cni plugin not initialized`，Pod 照样起不来，而且 5 分钟后
`node.kubernetes.io/not-ready:NoExecute` 污点会开始驱逐已在运行的负载。
这种节点上把文件留着反而是更好的失败姿态 —— 应该 drain 它。

CNI 创建的 ENI 会继续挂在实例上。daemon 存活期间会每小时回收自己泄漏的 ENI，
依据是 `zenlayer:cni:created-by` 与 `zenlayer:cni:instance-id` 两个标签；daemon 一旦
不在，就没有任何机制会做这件事。**删除集群后，请自行检查账号内是否还有带这两个标签的
ENI。** 这个取舍是刻意的：detach ENI 是一次很慢的 API 调用，而 pod 的终止宽限期不是放
慢速 API 调用的地方 —— 绑在一起意味着要么把宽限期拉长到影响每一次正常重启，要么 detach
执行到一半被杀掉。

**辅助地址同样会残留，而且上面那两个标签查不到它们。** daemon 消失时 warm pool 里持有的
地址会继续绑在所属网卡上，而**地址本身不带任何标签，只有网卡带**。所以落在节点**主 ENI**
上的残留地址，用标签是搜不出来的。在五节点集群上，`helm uninstall` 后共残留
**10 个**地址，恰好是每节点 `poolTarget`（2）个，其中 **5 个**落在主 ENI 上、
**无任何标签**。如果在同一批节点上重装 chart，GC 会重新纳管它们；如果这些
节点要下线，请逐个实例列出网卡的辅助地址并显式释放。

**但如果实例本身要释放，上面这些手工清理都不必做 —— 前提是保持平台的「同步释放关联资源」
为开启状态**（OpenAPI 中是 `ReleaseInstances` 的 `DependResource`，默认 `true`）。实例释放
时会一并删除挂在其上的弹性网卡及其辅助地址，包括本插件创建的那些。

反过来说：**释放节点时若关闭该选项**（仅释放实例本身、保留关联资源），本插件创建的网卡与
地址都会残留，且没有任何机制会回收它们，需按上面两个标签手工清理。

## 从源码构建

需要 Go 1.26.8（依据 `go.mod`），构建镜像还需要在 `amd64` 宿主机上安装 Docker。

| 目标 | 作用 |
| --- | --- |
| `make help` | 打印解析后的构建变量和可用的 make 目标。 |
| `make bin` | 执行 `go mod tidy`，然后把两个二进制构建到 `output/`（`CGO_ENABLED=0 GOOS=linux`）。 |
| `make zec-cni` / `make zec-ipam` | 只构建其中一个二进制。 |
| `make pkg` | 在 `bin` 的基础上，把 bootstrap 脚本一并拷贝到 `output/`。 |
| `make image` | 使用 `images/Dockerfile` 构建容器镜像。仅支持 amd64。 |
| `make chart-lint` | 填入必填值后对 chart 执行 `helm lint`。 |
| `make chart-template` | 填入必填值后渲染 chart。 |
| `make chart` | 先 lint，再 `helm package` 到 `output/charts/`。 |
| `make all` | `pkg` + `image` + `chart`。 |
| `make tidy` | `go mod tidy`。 |
| `make clean` | 执行 `go clean`，删除 `output/`，并删除构建出的镜像。 |

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
上运行 —— CI 即如此，整个 job 设 `GOOS=linux CGO_ENABLED=0`，测试步单独用
`CGO_ENABLED=1 go test -race`。在非 Linux 工作机上，`GOOS=linux go vet ./...` 可以对包括
测试文件在内的全部代码做类型检查。

## 故障排查

**日志。**

- `zec-cni` 在节点上写入 `/var/log/zenlayer/cni.log`（由 lumberjack 轮转：256 MB、2 个备份、
  保留 7 天）。kubelet 也会在 Pod 的 event 中暴露 CNI 错误。
- `zec-ipam` 日志输出到 stderr：`kubectl -n kube-system logs ds/zenlayer-vpc-cni -c zec-ipam`。
- `zec.logLevel` 在安装时统一设置两个二进制的日志级别：它渲染 DaemonSet 上的
  `ZENLAYER_CNI_LOGLEVEL`，取 `debug` **或 `trace`** 时还会写入 `zec-cni` 读取的 conflist
  `debug` 标志 —— `trace` 比 `debug` 更详细，所以它不会把这个标志关掉。
- 日志级别和输出位置也可以通过环境变量 `ZENLAYER_CNI_LOGLEVEL`
  （`trace|debug|info|warn|error|fatal|panic`）和 `ZENLAYER_CNI_LOCATION`
  （文件路径，或 `stdout`）覆盖。`zec-ipam` 还支持 `-log-level` 参数，且优先级高于
  `ZENLAYER_CNI_LOGLEVEL`。
- 在 conflist 中设置 `"debug": true` 会把本次调用的 `zec-cni` 日志级别提升到 debug。
- `ZENLAYER_CNI_SDK_HTTP_DUMP=true` 会额外打印每次 OpenAPI 调用的完整请求与响应。默认关闭，
  且**不会**因为 `ZENLAYER_CNI_LOGLEVEL=debug` 而打开 —— 这个 dump 是全量的，包含
  `Authorization` 头，也就是每次调用都把账号的 AccessKeyID 明文写进日志。只在可丢弃的
  集群上开，并且把它产生的日志当作凭据对待。

**健康检查与指标。** 在任意运行了该 DaemonSet 的节点上：

```sh
curl 127.0.0.1:11414/healthz
curl 127.0.0.1:11414/metrics
```

daemon 不提供任何 profiling 端点。该端口需要能被节点外访问（kubelet 探针、
Prometheus 抓取），而 `/debug/pprof` 会把堆快照、CPU profile 和进程命令行
交给任何能访问到该节点的一方。需要 profile 时请单独构建带 pprof 的版本。

**常见启动失败**（均由 daemon 在启动时报告）：

| 报错信息 | 原因 |
| --- | --- |
| `no credentials for the zenlayer api: ...` | Secret 不存在、名字不对，或者它的键不是 `AccessKeyID` / `AccessKeyPassword`。报错信息里会给出它尝试读的两个路径，以及指向该 Secret 的 chart value。 |
| `securityGroup is not set in the cni configmap ...` / `subnets is empty in the cni configmap ...` | 未设置 `zec.securityGroupID` / `zec.subnetIDs`。 |
| `subnets lists N entries but at most 5 are supported` | `zec.subnetIDs` 中的条目超过 5 个。 |
| `PoolTargetMin (n) must be less than or equal to PoolTarget (m)` | warm pool 的边界值不一致；必须满足 `poolTargetMin <= poolTarget <= poolTargetMax`。 |
| `error retrieving service cidr: configmaps "kubeadm-config" not found` | 集群不是 kubeadm 建的，而 `zec.serviceCidr` 又留空了。显式设置它。 |
| `unsupported ipStack <取值> in configMap` | `zec.ipStack` 既不是 `ipv4` 也不是 `dual`。 |
| `ipStack "ipv6" (IPv6-only) is not implemented yet` | IPv6 单栈是被刻意拒绝的，不是拼写错误。请用 `dual` 或 `ipv4`。 |
| `ipStack dual needs every subnet to carry ipv6, but subnet <id> has no ipv6 cidr` | `zec.ipStack=dual`，但某个子网只有 IPv4 CIDR。给该子网配置 IPv6 CIDR，或把 `zec.ipStack` 改回 `ipv4`。 |
| `ipv6InternetChargeType "ByTrafficPackage" requires ipv6PackageSize` | 用 `--set-json` 设置 `zec.ipv6PackageSize`（单位 TB，0.1 的正整数倍）。若账号处于按小时计费周期，请改用 `ipv6InternetChargeType: ByBandwidth`。 |
| `ipv6InternetChargeType "BandwidthCluster" requires ipv6ClusterId` | 设置 `zec.ipv6ClusterId` 为网卡要加入的共享带宽包。 |
| `ipStack "dual" needs a positive ipv6Bandwidth (Mbps)` | `zec.ipv6Bandwidth` 为 0 或负数。平台要求公网 IPv6 必须有带宽上限。 |
| `unsupported daemon mode` | 仅支持 `eni_multi_ip`。 |

**Pod 卡在 `ContainerCreating`。** 先确认节点上有 `/opt/cni/bin/zec-cni`（由 init 容器
安装），再看 `/etc/cni/net.d/10-zenlayer-cni.conflist` 与 `/var/run/zenlayer/cni.socket`
是否存在，然后查 `/var/log/zenlayer/cni.log`。

顺序有讲究：**conflist 不是 init 容器装的** —— 它由 daemon 在开始服务之后写入，所以
conflist 缺失指向 daemon，而不是 init 容器（后者会成功退出、什么都不缺）。启用 Cilium
串接时，daemon 还会先阻塞等 `/var/run/cilium/cilium.sock`，此时 conflist 缺失意味着
Cilium agent 没起来。参见[架构](#架构)。

## 延伸文档

- [与 Cilium 串接部署](./docs/deploy-cilium-chained-cn.md) —— 默认模式 `cni.mode=chained`：
  前提条件、从 veth 切过来的完整步骤、每一个必填的 Cilium value 及其理由，以及回退流程。
  启用网络策略前请先读这篇。
- [delegated IPAM 模式](./docs/deploy-cilium-netkit-cn.md) —— `cni.mode=delegated` 的完整步骤：
  由 Cilium 创建 Pod 设备（netkit 或 veth），本插件只分配地址并装宿主 per-ENI 路由。
- [性能对比与测量方法](./docs/performance-cn.md) —— 各形态的测量数据、哪些开关真正有收益，
  以及跨会话对比为什么不可信。选形态前读这篇。
- [CHANGELOG.md](./CHANGELOG.md) —— 各版本的变更记录，含当前版本的已知限制。
- [CONTRIBUTING.md](./CONTRIBUTING.md) —— 构建、测试、lint 命令，以及 PR 的检查项。

## 贡献

构建、测试与评审流程参见 [CONTRIBUTING.md](CONTRIBUTING.md)。参与本项目须遵守
[行为准则](CODE_OF_CONDUCT.md)。

## 安全

请以私下方式报告漏洞 —— 参见 [SECURITY.md](SECURITY.md)。

## 变更记录

各版本的发布说明见 [CHANGELOG.md](./CHANGELOG.md)。

## 许可证

Apache License 2.0。参见 [LICENSE](LICENSE)。
