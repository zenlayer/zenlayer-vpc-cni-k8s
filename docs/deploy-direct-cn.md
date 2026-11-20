# direct 模式部署：每个节点直接调用 OpenAPI

[English](deploy-direct.md) | 简体中文

[README](../README-CN.md) 描述的是集中式部署（`zec.ipamType=crd`）：只有 controlplane 持有
OpenAPI 凭证，节点不持有。本文描述另一种模式 `zec.ipamType=direct`：**每个节点的 daemon
各自挂载凭证、直接调用 OpenAPI**，没有 controlplane，也不使用 `ZecNode`。

除本文列出的差异外，datapath、地址族、网络策略、Cilium 串接、卸载等内容与 README 相同。

## 1. 什么时候选 direct

| | 集中式（`crd`，README） | direct（本文） |
| --- | --- | --- |
| OpenAPI 凭证 | 只在 controlplane 上 | **每个节点**都挂载 |
| 公网出向（访问 OpenAPI） | 只有运行 controlplane 的节点需要 | **每个节点**都需要 |
| Kubernetes 版本 | >= 1.30（需要 ValidatingAdmissionPolicy） | >= 1.28.2 |
| 额外组件 | controlplane Deployment、`ZecNode` CRD、准入策略 | 无 |
| 温池参数 | `controlplane.scaler.*`，可热生效 | `zec.poolTarget*`，改动需重启 daemon |
| OpenAPI 调用量 | 由 controlplane 统一限流 | 随节点数增长，各节点各自调用 |

仅当集群无法运行集中式部署时才建议使用 direct 模式，典型情况是 Kubernetes 版本低于 1.30。该模式的
代价是：任何一个节点被攻破，账号的 OpenAPI 凭证即随之泄露；每个节点都必须能够访问公网。

目前不支持在两种模式之间原地切换，请在安装时选定模式。

## 2. 环境要求

与 README 的[环境要求](../README-CN.md#环境要求)相同，但有两处不同：

- Kubernetes >= 1.28.2（chart 的 `kubeVersion` 约束）。
- **每个**节点都具备**公网出向**能力：节点绑定 EIP，或其所属 VPC 配置 NAT 网关。
  Zenlayer OpenAPI 没有内网端点，而 daemon 在其所在节点上直接调用 OpenAPI，因此无法访问公网的
  节点连一个 Pod 地址也分配不出来。

## 3. 安装

### 3.1 创建凭证 Secret

```sh
kubectl create secret -n kube-system generic zenlayer-cni-access-key \
  --from-literal=AccessKeyID='<access-key-id>' \
  --from-literal=AccessKeyPassword='<access-key-secret>'
```

每个节点的 daemon 都以只读方式将该 Secret 挂载到 `/etc/zec_secret/`，并以文件形式读取这两个键。
若 Secret 使用其他名称，请通过 `--set zec.secretName=<name>` 传入。

### 3.2 安装 chart

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

- 三个 ID 必须通过 `--set-string` 传入，原因见 README 的[安装](../README-CN.md#安装)。
- 双栈安装的 IPv6 计费参数与 README 相同。
- direct 模式不渲染 controlplane 与准入策略。`ZecNode` CRD 位于 `chart/crds/`，Helm 仅在首次安装时
  应用该目录：全新安装会安装该 CRD，从 1.x 升级则不会。无论哪种情况，direct 模式都不使用它。

### 3.3 验证

```sh
kubectl -n kube-system rollout status ds/zenlayer-vpc-cni
kubectl run nettest --image=busybox --restart=Never -- sleep 3600
kubectl wait --for=condition=Ready pod/nettest --timeout=60s
kubectl get pod nettest -o jsonpath='{.status.podIP}{"\n"}'
kubectl delete pod nettest

# 节点上：池状态与健康检查
ssh <节点> 'curl -s 127.0.0.1:11414/metrics | grep -E "^resource_pool_"'
ssh <节点> 'curl -s 127.0.0.1:11414/healthz'
```

daemon 的 `/healthz` 仅在成功访问 OpenAPI 并建立 warm pool 之后才开始响应，因此
DaemonSet 预留了 30 x 10s 的启动窗口。

### 3.4 从 1.x 升级

1.x 的 chart 既没有 `zec.ipamType`，也没有 `controlplane` 段，其 daemon 本身就直接调用
OpenAPI，因此升级后即为 direct 模式（需要 Helm >= 3.14）：

```sh
helm get values zenlayer-vpc-cni -n kube-system --all   # 集群当前实际使用的值
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version <version> \
  --namespace kube-system --reset-then-reuse-values \
  --set zec.ipamType=direct \
  --set zec.ipStack=<ipv4|dual> --set zec.manageUntaggedENI=<true|false> \
  --set cni.mode=<mode> --set cilium.enabled=<bool> --set networkPolicy.enabled=<bool>
```

- **不要**使用 `--reuse-values`：该参数会连同 1.x chart 的默认值一起保留，包括 `image.tag: v1.0.0`。
  其结果是升级**成功**、release 显示新的 chart 版本，但 DaemonSet 运行的仍是 1.x 镜像：节点继续
  运行 1.x，且任何地方都不报错。每次升级后务必核对镜像：
  `kubectl -n kube-system get ds zenlayer-vpc-cni -o jsonpath='{.spec.template.spec.containers[*].image}'`。
- `--reset-then-reuse-values` 会丢弃旧 chart 的默认值，但保留所有显式设置过的值，包括 `image.tag`。
  若 1.x 安装时显式固定了 `image.tag`，还需加上 `--set image.tag=<new-tag>`，否则结果与上一条相同。
- 2.x 的行为开关没有默认值，而 `--reset-then-reuse-values` 只保留显式设置过的值。请按集群当前的
  取值（可通过第一条命令查到）逐个传入；1.x 中 `zec.manageUntaggedENI` 的默认值为 `true`。
- 自 2.0.1 起（见 CHANGELOG 的 `[2.0.1]`），Pod 地址只来自 `zec.subnetIDs`。
  若节点主网卡所在子网不在其中，该主网卡将不再服务新 Pod，节点容量也会相应扣除这块网卡的份额
  （见[容量核对](#5-容量核对)）。如需保持原有容量，请将节点子网也列入 `zec.subnetIDs`。
- 当 `cilium.enabled=true` 且 `zec.k8sServiceHost` 留空时，2.x chart 会在升级时从
  `kube-public/cluster-info` 读取 apiserver 地址。读取到的可能只是某一台 master 的地址。集群中没有该
  ConfigMap（非 kubeadm 搭建）时，以及在任何离线渲染（`helm template`、客户端 dry run、GitOps 工具）下，
  渲染会**直接失败**，并指出需要设置的两个值。Cilium 集群请显式传入
  `zec.k8sServiceHost` / `zec.k8sServicePort`，指向负载均衡或 VIP；详见 README
  [配置参考](../README-CN.md#配置参考)中的 `zec.k8sServiceHost`。
- `image.pullPolicy` 的默认值由 `Always`（1.x）改为 `IfNotPresent`。如果依赖重新拉取同名 tag，
  请传入 `--set image.pullPolicy=Always`。

## 4. 温池与 GC

### 温池参数

| 配置项 | 默认值 | 说明 |
| --- | --- | --- |
| `zec.poolTarget` | `2` | 每个节点 warm（预分配、未使用）IP 的目标数量。 |
| `zec.poolTargetMin` | `1` | 池的总量下限（含已被 Pod 占用的地址），并非仅指空闲部分。必须 <= `poolTarget`。 |
| `zec.poolTargetMax` | `10` | 池的总量上限（含已被 Pod 占用的地址）。**`poolTargetCapProbe` 为 `true`（默认）时该值被忽略**，上限改用实例的真实容量。关闭探测后它将直接成为该节点向调度器上报的容量，需按实例规格调大，且必须 >= `poolTarget`。 |
| `zec.poolTargetCapProbe` | `true` | 探测实例的 ENI/IP 容量；此时 `poolTarget` 与 `poolTargetMax` 可为 0，`poolTarget` 也可大于 `poolTargetMax`。 |
| `zec.reconcileIntervalSec` | `1200` | GC 间隔，单位秒。平台因网卡地址数已满或子网不支持所请求的 IP 栈而拒绝分配地址时，GC 会提前执行。 |

这些值仅在 daemon 启动时读取一次；修改后需经 `helm upgrade` 触发 DaemonSet 滚动更新才会生效。

### GC 期间会暂停整个池

direct 模式下，daemon 的 GC 需要与平台对账，**在整个执行期间（包括其中的 OpenAPI 调用）暂停整个池**，
因此该节点上的 Pod 网络配置会排队等待，而 CNI ADD 的超时时间为 118 秒。daemon 会记录每一轮 GC 的
耗时，并在耗时偏长时输出 warning。如果出现 Pod 偶发无法启动，请首先检查以下日志：

```sh
kubectl -n kube-system logs <daemon-pod> -c zec-ipam | grep -E 'Daemon GC (finished in|took)|Daemon gc failed after'
```

`Daemon GC took ... the pool was paused that whole time` 表示这一轮偏慢，`Daemon gc failed after`
表示失败；耗时短且成功的一轮只输出 `Daemon GC finished in`。

集中式部署不存在这一问题：在该模式下，daemon 的 GC 只与 `ZecNode` 比对，路径上没有 OpenAPI 调用。

## 5. 容量核对

daemon 启动时会打印其解析出的容量。当节点容量与 README [容量规划](../README-CN.md#容量规划)
中的表格不符时，这是最快的核对方式：

```sh
kubectl -n kube-system logs <daemon-pod> -c zec-ipam | grep -E 'Attached eni|InstanceLimit Updated|Capacity'
# Attached eni created by cni: [], adopted: [], primary: [<primary-mac>]
# Attached eni not managed by cni: [<mac-1> <mac-2>]; 2 interface slot(s) held without serving pod addresses (ENIUnmanaged)
# InstanceLimit Updated {ENIQuota: 4, IPv4MaxPerENI: 30, IPv6CidrMaxPerENI: 1, ENIManaged: 0, ENIUnmanaged: 2, Cordon: false, allocatable: 58}
# Capacity 58
```

其中 `ENIUnmanaged` 是本插件不纳管的 ENI 数，`allocatable` 是扣除它们之后的容量。

Pod 地址只来自 `zec.subnetIDs` 中的子网；主网卡仅在其子网被列出时才服务 Pod。未列出时，节点会
收到 `InterfaceExcluded` 事件，向调度器上报的容量会扣除这块网卡的份额
`IPv4MaxPerENI - 1`（减去现有 Pod 仍在其上占用的地址）。`InstanceLimit Updated` 一行中的
`allocatable` **不包含**这一扣减；`Capacity` 一行包含该扣减，并写明扣除的数量，例如
`Capacity 16: 12 of the quota-based 28 sit on interfaces that take no new pod addresses`。

如果实例无法再挂载其他网卡（例如只能挂载 2 块网卡的实例，其主网卡被排除，另一个槽位又被一块不纳管的
网卡占用），节点将**无法承载任何 Pod**。direct 模式没有为此单独设置事件 reason，唯一的迹象是节点
allocatable 中 `zenlayer.com/eni-ip` 为 0，以及前述的 `InterfaceExcluded` 事件。

## 6. 故障排查

**Node 事件**的 reason 与 README [故障排查](../README-CN.md#故障排查)中的相同，但由每个节点的
daemon 各自写入；direct 模式没有 `ZecNode`，因此 `kubectl get zn` 不适用。README 中提到 controlplane 之处，
在 direct 模式下均应理解为**该节点的 daemon**：`OpenApiUnreachable` 表示该节点没有公网出向，
`NodeClockSkew` 表示该节点自身的时钟偏差过大，`OpenApiPermissionDenied` 指出的是该节点 daemon
调用的 API。

direct 模式另有 `InterfaceExcluded` 事件：节点的某块网卡（主网卡，或本插件创建、所在子网后来被移除的网卡）
所在子网不在 `zec.subnetIDs` 中，因此该网卡不再服务新 Pod。其中由插件创建的网卡会在其上的 Pod 全部退出后被释放（见[容量核对](#5-容量核对)）。若实例无法再挂载其他网卡，节点将完全无法承载 Pod，且不会产生
其他事件：`kubectl describe node` 中 `zenlayer.com/eni-ip` 为 0。`InterfaceExcluded` 是 `Normal` 类型的事件，按
`type=Warning` 过滤时不会显示：

```sh
kubectl get events -A --field-selector reason=InterfaceExcluded
```

**常见启动失败**（daemon 在启动时报告并退出），除 README 列出的之外，还包括：

| 报错信息 | 原因 |
| --- | --- |
| `no credentials for the zenlayer api: ...` | Secret 的键不是 `AccessKeyID` / `AccessKeyPassword`。报错信息中会给出 daemon 尝试读取的两个路径，以及指向该 Secret 的 chart value。（Secret 不存在或名称不对时不会进行到这一步：Pod 停留在 `ContainerCreating` 状态并带有 `FailedMount` 事件，Secret 创建后会自行启动。） |
| `securityGroup is not set in the cni configmap ...` / `subnets is empty in the cni configmap ...` | 未设置 `zec.securityGroupID` / `zec.subnetIDs`。 |
| `subnets lists N entries but at most 5 are supported` | 子网超过 5 个。通过 Helm 安装时，chart 的 schema 会先行拒绝；只有手工修改 ConfigMap 时才会由 daemon 报出。 |
| `poolTargetMin (n) must be less than or equal to poolTarget (m) ...` / `poolTarget (n) must be less than or equal to poolTargetMax (m) ...` | 温池边界值不一致；必须满足 `poolTargetMin <= poolTarget`，`zec.poolTargetCapProbe=false` 时还须 `poolTarget <= poolTargetMax`。 |
| `poolTarget (n) and poolTargetMax (m) must both be > 0 unless poolTargetCapProbe is true ...` | `zec.poolTargetCapProbe=false` 时，需将两者均设为正数。 |
| `ipStack dual needs every subnet to carry ipv6, but subnet <id> has no ipv6 cidr` | `zec.ipStack=dual`，但某个子网只有 IPv4 CIDR。 |
| `ipv6InternetChargeType "ByTrafficPackage" requires ipv6PackageSize` | 用 `--set-json` 设置 `zec.ipv6PackageSize`（单位 TB，0.1 的正整数倍）。 |
| `ipv6InternetChargeType "BandwidthCluster" requires ipv6ClusterId` | 设置 `zec.ipv6ClusterId`。 |
| `ipStack "dual" needs a positive ipv6Bandwidth (Mbps)` | `zec.ipv6Bandwidth` 为负数。0 会回落到默认值，且 chart 的 schema 要求 >= 1，因此只有手工修改 ConfigMap 才会出现此错误。 |

## 7. 卸载

与 README 的[卸载](../README-CN.md#卸载)相同。唯一的区别在于由谁回收泄漏的网卡：direct 模式下，每个
daemon 在运行期间每小时检查一次，只删除为**本实例**创建（依据 `zenlayer:cni:created-by` 与
`zenlayer:cni:instance-id` 两个标签）、处于未挂载状态且超过 10 分钟未变动的网卡。direct 模式没有
VPC 范围的清扫，因此 daemon 不复存在后（卸载后，或节点被删除时），本集群中没有任何机制回收其网卡。
（同一 VPC 中若有集中式模式的集群，其清扫会删除不带 `zenlayer:cni:cluster-id` 标签的未挂载插件
网卡，1.x 创建的网卡全部属于此类；但不要依赖这一点。）这些网卡需要手工删除：筛选带 `zenlayer:cni:created-by`
标签、`zenlayer:cni:instance-id` 为该节点实例 ID 的未挂载网卡；存在 `zenlayer:cni:cluster-id` 标签时
也按本集群的值过滤。集群 ID 即 `kube-system` 命名空间的 uid：

```sh
kubectl get ns kube-system -o jsonpath='{.metadata.uid}'
```
