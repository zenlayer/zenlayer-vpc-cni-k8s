# 部署串接模式（veth 数据面 + Cilium CNI chaining）

> 本文为中文翻译，以英文版 [deploy-cilium-chained.md](./deploy-cilium-chained.md) 为准。

本文说明如何在 zenlayer-vpc-cni 之上启用 NetworkPolicy：将**社区原版 Cilium** 以 CNI
chaining 的方式串接在本插件之后。这是 `cni.mode=chained`，也是本项目与 Cilium 一起部署的推荐模式。

**本项目不 fork Cilium，也不分发 Cilium 镜像。** Cilium 由您通过其官方 chart 自行安装，
版本由您选择。本项目只做两件事：在 conflist 中追加一个 `cilium-cni` 插件段；在
Cilium agent 就绪之前不放行 conflist。

启用 Cilium 是**可选的**。不启用时，本插件的行为与本文无关：Service 由 kube-proxy 处理，
数据面完全不变。

> 另一种模式是 `cni.mode=delegated`：由 Cilium 创建 Pod 设备（netkit 或 veth），本插件只
> 分配地址，详见 [deploy-cilium-netkit-cn.md](./deploy-cilium-netkit-cn.md)。
> 两种模式的性能对比见 [performance-cn.md](./performance-cn.md)：本模式已达到可获得的性能上限，另一种模式并不更快。

## 目录

- [1. 前提条件](#1-前提条件)
- [2. 快速开始：从 veth 切到本模式](#2-快速开始从-veth-切到本模式)
- [3. 功能矩阵](#3-功能矩阵)
- [4. 配置参考](#4-配置参考)
- [5. 回退到 veth 模式](#5-回退到-veth-模式)

## 1. 前提条件

| 项 | 要求 | 备注 |
|---|---|---|
| **Cilium 版本** | **v1.15 ~ v1.18** | v1.17.5 已完成代码核对与集群验证。下界仅为上游维护窗口，上界仅表示已核对的范围。`k8sServiceHost=auto`（[§4.2](#42-cilium-values)）需要 v1.16 及以上，v1.15 上须显式设置地址与端口。跨版本前，先执行 `helm show values cilium/cilium --version <ver>`，逐一核对本文用到的每个键，因为 chart 的 values 结构在大版本之间会发生变化 |
| 内核 | ≥ 5.4（RHEL 8 系 ≥ 4.18） | Cilium 自身的最低要求 |
| cgroup v2 | 挂载 | `stat -fc %T /sys/fs/cgroup` 输出 `cgroup2fs`。socket 层负载均衡挂载在 cgroup 上，因此它是 `kubeProxyReplacement` 的硬性前提 |
| bpffs | 挂载于 `/sys/fs/bpf` | **由 Cilium agent 自行挂载**，本项目不接管 |
| `helm` | 在执行命令的机器上可用，≥ 3.14 | **不需要安装在集群节点上**，任何能访问 API server 的机器均可。3.14 起才支持 `--reset-then-reuse-values` |
| 本项目已正常运行 | 是 | 先确保本项目正常运行，再进行串接 |
| kube-proxy | 部署完成后会被下线 | Service 改由 Cilium 的 eBPF 承担。**不要先删除它**：部署过程中仍需要它兜底，回退时也需要将它恢复 |
| 集群协议栈 | 单栈 IPv4，或按 [§4.3](#43-ipv4-单栈与双栈) 部署的双栈 | 双栈集群若遗漏这两个键，Pod 的 IPv4 正常，而 IPv6 会被全部静默丢弃，**不产生任何日志** |

新增的集群组件：1 个 DaemonSet（cilium-agent）、1 个 Deployment（cilium-operator），以及
Cilium 自带的一批 CRD。升级 Cilium 不需要修改本项目的任何配置。

> **cilium-operator 需要部署，但它不在策略执行的关键路径上。** 本方案使用
> `identityAllocationMode=crd`（Cilium 默认），该模式下 CiliumIdentity 与 CiliumEndpoint 由
> agent 直接创建并维护，operator 只负责 identity 垃圾回收。operator 缺失时，新建 Pod 照样获得身份，
> 策略照常生效，唯一的退化是留下孤儿 CiliumIdentity。
>
> 但 operator 仍应保持健康。**注意：它的 toleration 是 `[{"operator":"Exists"}]`，容忍一切 taint**，
> 因此在存在 NotReady / cordoned 节点的集群中，它可能被调度到无法启动的节点上，并一直处于 `Pending`。
> 此外，`node.cilium.io/agent-not-ready` 污点的摘除由 operator 独家负责：operator 不在时，
> 节点即使 agent 已 Ready 也无法调度新 Pod。安装后确认它被调度到健康节点上：
>
> ```bash
> kubectl -n kube-system get pod -l io.cilium/app=operator -o wide
> ```

## 2. 快速开始：从 veth 切到本模式

前提：本项目已正常运行、节点已挂载 cgroup v2、集群为单栈 IPv4（双栈见
[§4.3](#43-ipv4-单栈与双栈)）。各参数取值的原因见 [§4](#4-配置参考)。

```bash
# 0. 前提自检
stat -fc %T /sys/fs/cgroup                      # 必须是 cgroup2fs
kubectl -n kube-system get ds zenlayer-vpc-cni  # 必须已就绪
helm version --short
# 集群是不是双栈 —— 输出非 0 就必须改走 §4.3，否则 Pod 的 IPv6 会被静默丢弃且无任何日志
kubectl get nodes -o jsonpath='{.items[*].status.addresses[*].address}' | tr ' ' '\n' | grep -c ':'

# apiserver 入口 —— 第 1 步的 k8sServiceHost=auto 读的就是这一行。必须有输出且带端口，
# 并且应是负载均衡 / VIP；是某台 master 的 IP 则它成为单点。没有输出（非 kubeadm 集群）、地址是带方括号的 IPv6，或 Cilium 是 v1.15，见 §4.2
kubectl -n kube-public get cm cluster-info -o jsonpath='{.data.kubeconfig}' | grep server:

# 1. 装 Cilium。这一串 --set 是本方案的必填集，少一项就会出问题（逐项解释见 §4.2）
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
# 确认 auto 已解析成真实地址 —— 输出是字面量 auto 或为空，agent 会连不上 apiserver
kubectl -n kube-system get ds cilium -o jsonpath='{.spec.template.spec.containers[0].env[?(@.name=="KUBERNETES_SERVICE_HOST")].value}{"\n"}'

# 2. 打开本项目的串接开关。daemon 会等 Cilium agent 就绪后才落盘新 conflist。
#    用 --reset-then-reuse-values 而不是 --reuse-values：后者会沿用上一个 chart 版本的默认值
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.0 \
  -n kube-system --reset-then-reuse-values \
  --set cilium.enabled=true --set networkPolicy.enabled=true
kubectl -n kube-system rollout restart daemonset/zenlayer-vpc-cni
kubectl -n kube-system rollout status daemonset/zenlayer-vpc-cni --timeout=10m
# release 的 NOTES 列出 daemon 使用的入口（见 §4.2 的 k8sServiceHost 一行）
helm get notes zenlayer-vpc-cni -n kube-system | grep -E '^(APISERVER|WARNING):'

# 3. 逐节点确认 conflist 已追加 cilium-cni 段（不能只查一台，期望 [zec-cni, cilium-cni]；
#    portmap 只在不启用 Cilium 时才串接）
kubectl -n kube-system get pod -l app=zenlayer-vpc-cni -o name | while read -r p; do
  printf '%s: ' "${p#pod/}"
  kubectl -n kube-system exec "${p#pod/}" -c zec-ipam -- \
    grep -o '"type": "[a-z-]*"' /host-etc-net.d/10-zenlayer-cni.conflist | tr '\n' ' '; echo
done

# 4. 确认每个节点都接管了 Service，再下掉 kube-proxy
kubectl -n kube-system get pod -l k8s-app=cilium -o name | while read -r p; do
  kubectl -n kube-system exec "${p#pod/}" -c cilium-agent -- \
    cilium-dbg status | grep -E '^KubeProxyReplacement:|^Routing:'
done                                            # 每个节点都要是 True + Host: BPF
[ -e kube-proxy-backup.yaml ] || kubectl -n kube-system get ds kube-proxy -o yaml > kube-proxy-backup.yaml   # 留作回退；不覆盖更早的备份
kubectl -n kube-system patch ds kube-proxy -p \
  '{"spec":{"template":{"spec":{"nodeSelector":{"kube-proxy-disabled":"true"}}}}}'

# 5. 在每个节点上清掉 kube-proxy 遗留的规则
#    （会连带删掉 kubelet 的 KUBE-FIREWALL / KUBE-KUBELET-CANARY，kubelet 下一轮会自己重建）
iptables-save  | grep -v KUBE | iptables-restore
ip6tables-save | grep -v KUBE | ip6tables-restore

# 6. 滚动重启所有需要被策略保护的工作负载 —— 这一步不能省
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout restart "$t/$n"; done
done

# 7. 验证纳管覆盖率：每个非 hostNetwork 的 Pod 都应有 CiliumEndpoint。
#    要看差集而不是 `get cep` —— 上一轮遗留的孤儿 CEP 会让列表看起来是满的。期望无输出
comm -23 \
  <(kubectl get pod -A --field-selector=status.phase=Running \
      -o go-template='{{range .items}}{{if not .spec.hostNetwork}}{{.metadata.namespace}}/{{.metadata.name}}{{"\n"}}{{end}}{{end}}' | sort) \
  <(kubectl get cep -A \
      -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' | sort)
```

**第 4 步的顺序不能颠倒**：先确认接管，再停用 kube-proxy。反过来会出现一段没有组件处理 Service 的
窗口，期间以 `hostNetwork` 运行的组件（含本项目的 daemon）会因无法连接 API server 而崩溃。

**第 6 步不能省略。** Cilium 的 endpoint 在 CNI ADD 时创建，因此**启用之前已在运行的 Pod 没有任何
CEP**，策略对它们完全不设防。在重启完成之前，不要认为策略已经生效。（回退后再重新安装时，
从未重启过的 Pod 可能从 `/var/run/cilium/state` 中的旧 agent 状态恢复出 CEP，详见 §5 末尾；
因此存在 CEP 也不能证明该 Pod 已重启过。）

## 3. 功能矩阵

| 能力 | 状态 |
|---|---|
| L3/L4 NetworkPolicy、CiliumNetworkPolicy | ✅ |
| L7 policy（HTTP）、FQDN policy（`toFQDNs`） | ✅ 默认可用，见 `l7Proxy`（[§4.2](#42-cilium-values)） |
| Hubble 流级可观测性 | ✅ 需在 Cilium 侧单独开启 |
| Service 由 Cilium 的 eBPF 承担（kube-proxy 被下掉） | ✅ 默认启用 |
| eBPF host-routing | ✅ 默认启用，Pod 流量 +16%~31%（[performance-cn.md](./performance-cn.md)） |
| Pod **出向**带宽限速 | ⚠️ 需单独开启并验证，辅助 ENI 上有缺口 |
| Pod **入向**带宽限速 | ❌ Cilium 明确不支持 |
| IPsec / WireGuard 透明加密 | ❌ 地址不由 Cilium 分配，无法形成加密对端 |
| Ingress / Gateway API | ❌ 依赖已关闭的 Envoy |
| `externalTrafficPolicy: Local`（集群内流量） | ❌ 不生效，这是 `kubeProxyReplacement` 的固有语义 |

**代价**：串接需要在每个 Pod 的 veth 与节点 eth0 上挂载 BPF 程序。若 hostNetwork 流量因此产生开销，
host-routing 无法将其补回；有一次测量结果约为 8%，但这只是单次结果，处于批次间漂移范围内，且在同批对比中未复现
（[performance-cn.md](./performance-cn.md)）。Pod 网络一侧的开销由 eBPF host-routing 补回并反超，前提是不关闭
`kubeProxyReplacement`。

## 4. 配置参考

### 4.1 本项目的两个开关

位于本项目的 `chart/values.yaml`。**两者刻意不合并**：只需要 Hubble 可观测性、不需要策略是真实存在的场景，
合并之后无法表达这种组合。

| 配置项 | 本文档取值 | 说明 |
| --- | --- | --- |
| `cilium.enabled` | `true` | **真正改变链路的开关**：在 conflist 中追加 `cilium-cni` 段，为 daemonset 挂载 agent socket，并让 daemon 等待 agent 就绪后才写入 conflist。 |
| `networkPolicy.enabled` | `true` | 只声明「需要有人执行策略」，本身不提供引擎。**要求 `cilium.enabled` 同时为真**，否则 chart 直接拒绝渲染。若允许这种组合，症状是本该被阻断的流量被放行，且不会有人察觉。 |
| `cni.mode` | `chained` | 本文所述的模式。`delegated` 模式见另一篇文档。 |

> **注意：** 开关名拼错会在渲染阶段直接失败（`values.schema.json` 根层是 `additionalProperties: false`），
> 报错 `additional properties 'celium' not allowed`。

### 4.2 Cilium values

下表列出本方案需要设置的全部 Cilium values，**缺少任何一项都会出问题**。带 ⚠️ 的项在表下单独说明。

| 配置项 | 本文档取值 | 说明 |
| --- | --- | --- |
| `cni.chainingMode` | `generic-veth` | 显式指定。不设置时，cilium-cni 会退化为按 conflist 的 `name` 查找同名 chainer，此时修改网络名会静默停止串接。 |
| `cni.customConf` | `true` | `generic-veth` 的**硬性前提**：Cilium 没有该模式的 conflist 模板，`false` 与它自相矛盾，agent 会卡在 `0/1 Running`。 |
| `cni.exclusive` | `false` | 纵深防御。`customConf=true` 时该键不会写入 configmap，实际不起作用。 |
| `cni.install` | `true` | 本项目需要 `/opt/cni/bin/cilium-cni` 这个二进制，由 Cilium 的 init 容器安装。 |
| `routingMode` | `native` | Pod IP 是 VPC 原生可路由的 ENI 辅助 IP，不需要隧道。 |
| `ipam.mode` | `delegated-plugin` | 告知 Cilium 地址由 CNI 二进制分配，Cilium 不参与。 |
| `extraConfig.local-router-ipv4` | `169.254.32.1` | ⚠️ **不是 helm value**，见下。取值只需避开 `169.254.1.1`（本插件写进容器的默认网关）。 |
| `endpointHealthChecking.enabled` | `false` | `delegated-plugin` 的强制伴随项。 |
| `envoyConfig.enabled` | `false` | 同上。代价是放弃 Ingress / Gateway API。 |
| `endpointRoutes.enabled` | `true` | 同上。不设置会导致 agent CrashLoop；该项不在 `checkIPAMDelegatedPlugin()` 的校验范围内。 |
| `kubeProxyReplacement` | `true` | ⚠️ 与下一项**成对**启用，见下。它还决定 hostPort：启用 Cilium 后 conflist 不再串接 `portmap`，只有它为 `true`（或经 `extraConfig` 设了 `enable-host-port`）时 Cilium 才处理 hostPort。 |
| `bpf.hostLegacyRouting` | `false` | ⚠️ 同上。**单独设置无效**：agent 在 `kubeProxyReplacement` 不为 `true` 时会把它强制改回 `true`，因此这一项不产生任何作用，只会让人误以为已经启用。 |
| `k8sServiceHost` | `auto` | ⚠️ **必须设置**。下线 kube-proxy 后，ClusterIP 只由 Cilium 实现；agent 若仍经 ClusterIP 连接 apiserver，整机重启后它无法启动，ClusterIP 也就无人实现，结果是 agent 与本插件的 daemon 一起 CrashLoop，报错 `dial tcp <ClusterIP>:443: connect: network is unreachable`。`auto` 让 helm 在安装时从 `kube-public/cluster-info` 读取 apiserver 入口（即 kubeadm 的 `controlPlaneEndpoint`），并同时得到端口。需要注意以下三点：**① 该入口应为负载均衡 / VIP**，若为单个 master 的 IP，则它成为单点；**② 需要 Cilium v1.16 及以上，且找不到该 ConfigMap 时的表现因版本而异**：v1.16.0 ~ v1.16.2 渲染直接失败；**v1.16.3 ~ v1.18 helm 不报错**，而是把字面量 `auto` 作为地址写入；v1.19 起又改为渲染失败（`ConfigMap kube-public/cluster-info not found`）；v1.15 根本没有该查找，总是原样写入 `auto`。v1.15 或非 kubeadm 集群改为显式设置 `--set k8sServiceHost=<入口地址> --set k8sServicePort=<端口>`。**入口为 IPv6 字面量（`https://[fd00::10]:6443`）时同样需要显式设置**（`--set k8sServiceHost=fd00::10 --set k8sServicePort=6443`）：Cilium 的查找把第一个 `:` 之前的部分当作主机，得到的是 `[fd00`。本 chart 自身为 `zec.k8sServiceHost` 实现的查找能正确处理这种入口；**③ 只在 install / upgrade 时解析一次**，`helm template` 或离线渲染无法访问集群，表现同 ②；入口变更后需要重新执行 `helm upgrade`。**本插件的 daemon 与 controlplane 需要同一个入口，原因相同**：`cilium.enabled=true` 且 `zec.k8sServiceHost` 留空时，本 chart 在安装 / 升级时从同一个 ConfigMap 读取，结果打印在 release 的 NOTES 里；ConfigMap 不存在时，把 `zec.k8sServiceHost` / `zec.k8sServicePort` 设为该入口。 |
| `enableIPv4Masquerade` | `false` | ⚠️ masquerade 必须全部关闭，见下。 |
| `enableIPv6Masquerade` | `false` | 同上。 |
| `bpf.masquerade` | `false` | 同上。 |
| `extraConfig.enable-session-affinity` | `true` | KPR 开启时由 Cilium 自动启用；显式写出是为了在关闭 KPR 后配置依然正确。若未写出且关闭了 KPR，`sessionAffinity: ClientIP` 会被静默忽略。 |
| `l7Proxy` | 不设置 | 保持 Cilium 默认值 `true`。**关闭它后，已下发的 L7 与 FQDN 策略的失效方式是「放行」而不是报错**：规则中的 L7 部分被忽略，退化为 L4 放行，且没有任何告警。 |
| `operator.replicas` | `2` | 与社区 chart 默认值一致。**不要降到 1**，理由见 §1 的说明框。 |
| `ipv6.enabled` | 仅双栈设 `true` | 见 [§4.3](#43-ipv4-单栈与双栈)。 |
| `extraConfig.local-router-ipv6` | 仅双栈设 | 同上，取值必须避开 `fe80::1`。 |

**⚠️ masquerade 必须全部关闭。** Pod IP 在 VPC 内本身可路由，开启 masquerade 会把 Pod 的源地址改写成节点地址，
VPC 侧看到的来源就不再是 Pod。此外，agent 启动时若发现启用了 **iptables 方式**的 masquerade，
会强制回落到宿主协议栈，`bpf.hostLegacyRouting=false` 随之失效。

**⚠️ `local-router-ipv4` / `local-router-ipv6` 只能通过 `extraConfig` 设置。** 它们不是 helm value：
写成 `--set localRouterIPv4=` 会被**静默丢弃**，随后 agent 因缺少该项而启动失败。

**不要单独设置 `enable-node-port`。** `kubeProxyReplacement=true` 时，agent 会自动派生它，
连同 `enable-external-ips`、`bpf-lb-sock`、`enable-host-port`、`enable-session-affinity`
一起。ConfigMap 是输入而不是生效配置，KPR 相关项一律以 `cilium-dbg config -a` 为准。

**`hostPort` 必须小于 30000。** `kubeProxyReplacement=true` 时，hostPort 由 Cilium 自身的
BPF 负载均衡承担，它会拒绝任何落在 NodePort 范围（30000–32767）内的端口，除一条
`The requested hostPort ... is colliding with the configured NodePort range` 日志外没有任何提示：
Pod 正常启动，`cilium-dbg status --verbose` 仍显示 `HostPort: Enabled`，
只有 `cilium-dbg service list | grep HostPort` 才能看出条目未被创建。

判断 eBPF host-routing 是否真正启用，应使用 `cilium-dbg status`，该字段跨版本稳定：

```bash
kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg status | grep '^Routing:'
# Routing:  Network: Native   Host: BPF      ← 已启用（本文默认档）
# Routing:  Network: Native   Host: Legacy   ← 走宿主协议栈
```

> **注意：** 不要用 `grep ENABLE_HOST_ROUTING /run/cilium/state/globals/node_config.h` 作为判据。
> 该宏在较新版本中已改为运行时配置项，grep 结果恒为 0，会被误读为「未启用」。

### 4.3 IPv4 单栈与双栈

先决定**是否需要 Pod IPv6**：这一点决定了对 Kubernetes 集群本身的要求，安装后再修改代价很大。

| | 单栈（只要 Pod IPv4） | 双栈（Pod 要 IPv4 + IPv6） |
|---|---|---|
| 对 k8s 集群的要求 | 无 | ⚠️ **集群本身必须是双栈**（Node 对象带 IPv6） |
| `ipv6.enabled` | `false`（默认，不用写） | **`true`** |
| `extraConfig.local-router-ipv6` | 不需要 | **需要**，且必须避开 `fe80::1` |
| 本项目的 `zec.ipStack` | `ipv4` | **`dual`** |

单栈的完整安装命令即[快速开始](#2-快速开始从-veth-切到本模式)第 1 步。双栈需在其基础上追加：

```bash
  --set ipv6.enabled=true \
  --set-string extraConfig.local-router-ipv6=fe80::ffff:1
```

此外，必须**先**让每个节点的 Node 对象带上 IPv6。Node 对象的地址来自 kubelet 的 `--node-ip`。在每个节点上执行下面的命令；
若网卡还没有全局 IPv6 且节点上已有 Pod，**先驱逐（drain）该节点**：`networkctl reconfigure`
可能导致 systemd-networkd 删除并非由它自己配置的路由和 ip rule（`ManageForeignRoutes` /
`ManageForeignRoutingPolicyRules` 默认开启），而本插件的每 ENI 路由表和每 Pod 规则正属于这类路由和规则。
此后新启动的 Pod 会重新装上这些路由和规则。

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
systemctl restart kubelet       # 仅当上面打印出了 KUBELET_EXTRA_ARGS 一行
kubectl get node <name> -o jsonpath='{.status.addresses}'
```

实测：重启 kubelet 后，节点约 10 秒即重新变为 `Ready`，已运行的 Pod 不受影响。

- **IPv6 必须取自承载 node IP 的那块网卡。** 多 ENI 节点上有多个全局 IPv6，取错不会立即报错，
  但 Node 对象会声明一个属于另一块网卡的地址，导致控制面与数据面不一致。
- **只需要 Node 对象双栈，不需要 Service 双栈。** apiserver 的 `--service-cluster-ip-range`
  保持单栈即可：它只决定能否创建双栈 Service，与 Pod 的 IPv6 连通性无关。
- **只有第二个方向的失败是静默的**：单栈集群上开启 `ipv6.enabled` 会使 agent fatal 退出，
  日志中写明 Node 对象没有 IPv6；双栈集群上不开启，Pod 能获得 IPv6 地址、配置也完全正确，但每个包都会被 tcx
  程序以 `Unsupported L3 protocol` 丢弃，**不产生任何日志**。
- **双栈集群的新节点必须先有 IPv6，Cilium 才能在其上启动。** 见
  [向双栈集群加入新节点](#向双栈集群加入新节点)。

#### 向双栈集群加入新节点

节点上的 Cilium 从 Node 对象读取节点的 IPv6，而 Node 对象上的地址来自 kubelet 的 `--node-ip`，
该参数只能由节点初始化设置。有两种做法，推荐第一种。

**A. 加入前就有 IPv6（推荐）**。为实例主网卡开启 IPv6（最好在创建实例时），然后在新节点上执行下面的命令。
其中的 reconfigure 步骤只在网卡还没有全局 IPv6 时才执行，即开机之后才开启 IPv6、开机时的 DHCPv6 请求已经结束的情况：

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

预期：Node 注册时即带有两个地址，Cilium 一次启动成功，该节点的 ZecNode 达到
`InstanceVerified=True`（crd 模式），节点变为 `Ready`。

**B. 由 controlplane 开启 IPv6**。以下条件全部满足时才可使用，否则使用 A：

- `zec.ipamType=crd` 且 `zec.ipStack=dual`；
- 节点主网卡所在子网在 `zec.subnetIDs` 中（不在其中的主网卡不服务 Pod，插件也不会为它开启 IPv6）；
- release 的 NOTES 打印的是 `APISERVER:` 一行（可用 `helm get notes zenlayer-vpc-cni -n kube-system`
  重新查看）。本 chart 拿不到入口时会拒绝渲染；若那里显示的是 `WARNING:`，说明 release 由早先的 chart 安装：其 daemon
  会回落到集群内 Service，而在 Cilium 启动之前，该节点上没有组件提供这个 Service，因此 daemon 永远无法注册。

controlplane 由平台运行时，上述关于 `zec.ipStack` 与 Pod 子网的条件指的是平台侧的配置，而不是本 chart 的 values。

不开启 IPv6，直接加入。节点的 daemon 完成注册后，controlplane 校验节点，并自行为主网卡分配 IPv6
前缀；实测在 daemon 启动后约 20 秒内完成。在宿主机获得该地址之前，该节点上的 Cilium 会以
`node <name> does not have an IPv6 address` 反复重启，节点保持 `NotReady`，这是预期行为。
本插件的 daemon 等待 Cilium agent 超过 `cilium.waitTimeout`（默认 5m）后会重启，这同样是预期行为；
若下面的手工步骤耗时更长，应调大该值。

```sh
# 在控制面节点上：等待主网卡自己的 IPv6 出现 —— 随后宿主机拿到的就是这个地址
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

直连 apiserver 的入口见 [§4.2](#42-cilium-values) 中 `k8sServiceHost` 一行。
上文的 `eth0` 指承载节点 IP 的网卡。本插件既不配置宿主机网卡，也不修改 kubelet：这两者属于节点初始化的范畴。

## 5. 回退到 veth 模式

**顺序是固定的，第 ⓪、②、③ 步都不能跳过，也不能与第 ④ 步交换。**

```bash
# ⓪ 先把 kube-proxy 放回来（快速开始第 4 步把它停掉了）
kubectl -n kube-system patch ds kube-proxy --type json \
  -p '[{"op":"remove","path":"/spec/template/spec/nodeSelector/kube-proxy-disabled"}]'
# 若 DaemonSet 已被删除（patch 报 NotFound），改用备份重建，并去掉原样导出带的、create 不接受的字段：
#   sed -E '/^  (resourceVersion|uid|creationTimestamp|generation):/d; /^status:/,$d' \
#     kube-proxy-backup.yaml | kubectl create -f -
kubectl -n kube-system rollout status ds/kube-proxy --timeout=5m
# 确认它真的接管了（在节点上执行）
iptables -t nat -S | grep -c '^-N KUBE-SVC'                                # 期望 > 0
# 此时 https://10.96.0.1:443/healthz 返回 200 说明不了问题：ClusterIP 仍由 Cilium 提供。
# 上面的 KUBE-SVC 计数才是证据。

# ① 关掉两个开关，让 conflist 去掉 cilium-cni 段
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 2.0.0 \
  -n kube-system --reset-then-reuse-values \
  --set cilium.enabled=false --set networkPolicy.enabled=false
kubectl -n kube-system rollout restart daemonset/zenlayer-vpc-cni
kubectl -n kube-system rollout status daemonset/zenlayer-vpc-cni --timeout=600s

# ② 确认【每个】节点上的 conflist 都不再含 cilium-cni
kubectl -n kube-system get pod -l app=zenlayer-vpc-cni -o name | while read -r p; do
  printf '%s: ' "${p#pod/}"
  kubectl -n kube-system exec "${p#pod/}" -c zec-ipam -- \
    grep -c cilium-cni /host-etc-net.d/10-zenlayer-cni.conflist
done                                            # 每个节点都必须是 0

# ③ 滚动重启所有工作负载 —— 必须在卸载 Cilium【之前】做
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout restart "$t/$n"; done
done
# 等每个重启都完成：卸载时不能还有 Pod 停在旧的接线上
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout status "$t/$n" --timeout=10m; done
done
while kubectl get pod -A --no-headers | grep -q Terminating; do sleep 5; done   # 旧 Pod 也要退完

# ④ 再卸载 Cilium。保持 cni.uninstall 为默认的 false（理由见下）
helm uninstall cilium -n kube-system

# ⑤ 清残留 BPF —— 网络设备与 cgroup 两类都要清，见下
```

> **第 ⓪ 步不能跳过。** 本文默认档下，Service 全部由 Cilium 承担，直接卸载会出现一段没有任何组件
> 处理 Service 的窗口。在此期间，从宿主命名空间访问 `10.96.0.1` 会失败，以 `hostNetwork: true`
> 运行的组件（含本项目的 daemon）会因无法连接 API server 而崩溃。

**第 ③ 步为什么必须在卸载之前**：Pod veth 上的 `cil_from_container` / `cil_to_container` 是
**BPF link**，不会随 agent 卸载而消失，`helm uninstall` 也不会清理它们。agent 仍在运行时，会持续维护这些
程序依赖的 LB map，因此这段窗口是安全的；**一旦卸载，map 即被冻结**：这些 Pod 的 ClusterIP
流量会继续经过一份不再更新的负载均衡表，删除某个后端后，它们仍会把相应比例的连接发往已删除的地址，
而 `KUBE-SVC-*` 计数为 0，沿 kube-proxy 方向排查不会有任何发现。

> **不要设置 `cni.uninstall=true`**，直到第 ② 步在所有节点上确认完成。在仍有节点使用串接版
> conflist 时删除 `/opt/cni/bin/cilium-cni`，会导致该节点的 ADD 和 DEL 同时失败：DEL 失败会在
> 本插件执行之前中止链路，**地址与 veth 同时泄漏**，Pod 卡在 `Terminating`。将二进制放回原处后，
> kubelet 会自行完成拆除。
>
> **批量删除 Pod 时需预留时间**：agent 不可达期间，每个 Pod 固定多耗时约 20 秒，且逐个串行处理。

### 清理残留 BPF

残留分两类，**都用 `link detach` 清除**。第二类挂载在 cgroup 上，除重启节点外没有其他办法触及它，
且后果更严重：它基于一份冻结的 LB map 继续在 `connect()` 时翻译 ClusterIP，**影响该节点上的
每一个 Pod**。症状具有迷惑性：卸载前已存在的 Service（如 kube-dns）照常翻译，因此 DNS
可用；而卸载后新建的 Service 完全不可用，因为数据包在 Pod 内就被改写为一个不存在的后端地址。

```bash
# ① 网络设备上的（eth0 / cilium_host / cilium_net / Pod veth）
for i in $(seq 1 60); do
  id=$(bpftool net show | grep cil_ | grep -oE 'link_id [0-9]+' | head -1 | awk '{print $2}')
  [ -z "$id" ] && break; bpftool link detach id "$id"
done

# ② cgroup 上的 socket-LB
for lid in $(bpftool link show | awk '/^[0-9]+: cgroup/{gsub(":","",$1); print $1}'); do
  pid=$(bpftool link show id "$lid" | grep -oE 'prog [0-9]+' | awk '{print $2}')
  bpftool prog show id "$pid" | grep -q 'name cil_' && bpftool link detach id "$lid"
done
```

`cilium_host` 与 `cilium_net` 同样会残留，`link detach` 与 `helm uninstall` 都不会删除它们。
在每个节点上删除：

```bash
ip link del cilium_host
```

两者是一对 veth，删除其中一个，另一个随之消失。

**确认清理干净**（在每个节点上执行，三条命令的结果都必须为 0）：

```bash
bpftool net show                   | grep -c cil_    # 网络设备上的
bpftool cgroup show /sys/fs/cgroup | grep -c cil_    # cgroup 上的 —— 最容易漏
ip -o link show                    | grep -c cilium  # cilium_host / cilium_net
```

> ⚠️ **不要用 `kubectl get cep` 判断 Cilium 是否还在**：其结果具有误导性。CEP 对象在
> `helm uninstall` 之后会原样保留在 API server 中，输出看起来完全正常（`ready`、IPv4/IPv6 齐全），
> 而 Cilium 早已不存在。应使用上面三条命令，或 `kubectl -n kube-system get ds cilium` 判断。
>
> 排查 veth 上的残留**不能用 `tc filter show`**：tcx 挂载点在 tc filter 中不可见，
> 返回空是**假阴性**，应使用 `bpftool net show | grep cil_`。

`/var/run/cilium/state` 同样会在卸载后残留。它不影响本插件，但如果以后还可能再次安装 Cilium，
需要在每个节点上删除它（`rm -rf /var/run/cilium/state`）：重新安装的 agent 会从中恢复 endpoint，
从未重启过的 Pod 会静默地拿回 CEP 和 BPF 程序。

其余残留（Cilium CRD、CEP / CiliumIdentity 对象、`/opt/cni/bin/cilium-cni`、`/run/cilium/` 下的其他内容）
不影响本插件工作，可按需手工清理：

```bash
kubectl delete cep --all -A
kubectl delete ciliumidentity --all
kubectl get crd | grep cilium | awk '{print $1}' | xargs -r kubectl delete crd   # 重装会自动重建
```
