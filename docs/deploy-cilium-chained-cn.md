# 部署串接模式（veth 数据面 + Cilium CNI chaining）

> 本文为中文翻译，以英文版 [deploy-cilium-chained.md](./deploy-cilium-chained.md) 为准。

本文说明如何在 zenlayer-vpc-cni 之上启用 NetworkPolicy：把**社区原版 Cilium** 以 CNI
chaining 的方式串在本插件之后。这是 `cni.mode=chained`，也是本项目与 Cilium 一起部署的默认模式。

**本项目不 fork Cilium，也不分发 Cilium 镜像。** Cilium 由你从它自己的官方 chart 安装，
版本由你选择。本项目只做两件事：在 conflist 里追加一个 `cilium-cni` 插件段，以及在
Cilium agent 就绪之前不放行 conflist。

启用 Cilium 是**可选的**。不启用时本插件的行为与本文无关，Service 由 kube-proxy 处理，
数据面完全不变。

> 另一种模式是 `cni.mode=delegated`：由 Cilium 创建 Pod 设备（netkit 或 veth），本插件只
> 分配地址，见 [deploy-cilium-netkit-cn.md](./deploy-cilium-netkit-cn.md)。
> 两种模式的性能对比见 [performance-cn.md](./performance-cn.md)：本模式已达到可用的性能水平。

## 目录

- [1. 前提条件](#1-前提条件)
- [2. 快速开始：从 veth 切到本模式](#2-快速开始从-veth-切到本模式)
- [3. 功能矩阵](#3-功能矩阵)
- [4. 配置参考](#4-配置参考)
- [5. 回退到 veth 模式](#5-回退到-veth-模式)

## 1. 前提条件

| 项 | 要求 | 备注 |
|---|---|---|
| **Cilium 版本** | **v1.15 ~ v1.18** | v1.17.5 做过代码核对与集群验证。下界只是上游维护窗口，上界只表示已核对范围 —— 跨版本前先 `helm show values cilium/cilium --version <ver>` 对一遍本文用到的每个键，chart 的 values 结构在大版本间会变 |
| 内核 | ≥ 5.4（RHEL 8 系 ≥ 4.18） | Cilium 自身的最低要求 |
| cgroup v2 | 挂载 | `stat -fc %T /sys/fs/cgroup` 为 `cgroup2fs`。socket 层负载均衡挂在 cgroup 上，是 `kubeProxyReplacement` 的硬前提 |
| bpffs | 挂在 `/sys/fs/bpf` | **Cilium agent 会自己挂**，本项目不接管 |
| `helm` | 能访问执行机 | **不需要装在集群节点上**，任何能连到 API server 的机器即可 |
| 本项目已正常运行 | 是 | 先跑通再串接 |
| kube-proxy | 部署完成后会被下掉 | Service 改由 Cilium 的 eBPF 承担。**不要先删掉** —— 部署过程中还要靠它兜底，回退时也要放回来 |
| 集群协议栈 | 单栈 IPv4，或按 [§4.3](#43-ipv4-单栈与双栈) 部署的双栈 | 双栈集群若漏了那两个键，Pod 的 IPv4 正常而 IPv6 被全部静默丢弃，**不产生任何日志** |

新增的集群组件：1 个 DaemonSet（cilium-agent）、1 个 Deployment（cilium-operator）、
Cilium 自带的一批 CRD。升级 Cilium 不需要改本项目的任何配置。

> **cilium-operator 要部署，但它不在策略执行的关键路径上。** 本方案使用
> `identityAllocationMode=crd`（Cilium 默认），该模式下 CiliumIdentity 与 CiliumEndpoint 由
> agent 直接创建并维护，operator 只负责 identity 垃圾回收。它缺失时新建 Pod 照样拿到身份、
> 策略照常生效，唯一退化是留下孤儿 CiliumIdentity。
>
> 但它仍应保持健康，且**注意它的 toleration 是 `[{"operator":"Exists"}]`，容忍一切 taint** ——
> 在有节点 NotReady / cordoned 的集群里可能被调度到起不来的节点上然后一直 `Pending`。
> 另外 `node.cilium.io/agent-not-ready` 污点的摘除由 operator 独家负责，它不在，
> 节点即使 agent 已 Ready 也排不上新 Pod。安装后确认它落在健康节点上：
>
> ```bash
> kubectl -n kube-system get pod -l io.cilium/app=operator -o wide
> ```

## 2. 快速开始：从 veth 切到本模式

前提：本项目已正常运行、节点挂了 cgroup v2、集群是单栈 IPv4（双栈见
[§4.3](#43-ipv4-单栈与双栈)）。每个参数为什么是这个值见 [§4](#4-配置参考)。

```bash
# 0. 前提自检
stat -fc %T /sys/fs/cgroup                      # 必须是 cgroup2fs
kubectl -n kube-system get ds zenlayer-vpc-cni  # 必须已就绪
helm version --short
# 集群是不是双栈 —— 输出非 0 就必须改走 §4.3，否则 Pod 的 IPv6 会被静默丢弃且无任何日志
kubectl get nodes -o jsonpath='{.items[*].status.addresses[*].address}' | tr ' ' '\n' | grep -c ':'

# 1. 装 Cilium。这一串 --set 是本方案的必填集，少一项就会出问题（逐项解释见 §4.2）
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

# 2. 打开本项目的串接开关。daemon 会等 Cilium agent 就绪后才落盘新 conflist
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 1.0.0 \
  -n kube-system --reuse-values \
  --set cilium.enabled=true --set networkPolicy.enabled=true
kubectl -n kube-system rollout restart daemonset/zenlayer-vpc-cni
kubectl -n kube-system rollout status daemonset/zenlayer-vpc-cni --timeout=10m

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
kubectl -n kube-system get ds kube-proxy -o yaml > kube-proxy-backup.yaml   # 留作回退
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
      -o jsonpath='{range .items[?(@.spec.hostNetwork!=true)]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' | sort) \
  <(kubectl get cep -A \
      -o jsonpath='{range .items[*]}{.metadata.namespace}/{.metadata.name}{"\n"}{end}' | sort)
```

**第 4 步的顺序不能反** —— 先确认接管、再停 kube-proxy。反过来会有一段没人处理 Service 的
窗口，期间以 `hostNetwork` 运行的组件（含本项目的 daemon）会因连不上 API server 而崩溃。

**第 6 步不能省。** Cilium 的 endpoint 是在 CNI ADD 时创建的，**启用之前就在跑的 Pod 一条
CEP 都不会有**，策略对它们完全不设防。在重启完成之前，不要认为策略已经生效。

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

**代价**：串接要在每个 Pod 的 veth 与节点 eth0 上挂 BPF 程序，hostNetwork 流量因此慢约 8%
且无法补偿。Pod 网络这一侧由 eBPF host-routing 补回并反超，前提是不要关掉
`kubeProxyReplacement`。

## 4. 配置参考

### 4.1 本项目的两个开关

位于本项目 `chart/values.yaml`。**两者故意不合并** —— 只要 Hubble 可观测性、不要策略是真实场景，
合并之后这个组合就没法表达。

| 配置项 | 本文档取值 | 说明 |
| --- | --- | --- |
| `cilium.enabled` | `true` | **真正改变链路的开关**：conflist 追加 `cilium-cni` 段、daemonset 挂上 agent socket、daemon 开始等待 agent 就绪才落盘 conflist。 |
| `networkPolicy.enabled` | `true` | 只声明「需要有人执行策略」，不会自己带来引擎。**要求 `cilium.enabled` 同时为真**，否则 chart 直接拒绝渲染 —— 若允许这种组合，症状是本该被阻断的流量通了，没人会发现。 |
| `cni.mode` | `chained`（默认） | 本文的模式。改成 `delegated` 见另一篇。 |

> 开关名拼错会在渲染阶段直接失败（`values.schema.json` 根层是 `additionalProperties: false`），
> 报 `additional properties 'celium' not allowed`。

### 4.2 Cilium values

下表是本方案要设置的全部 Cilium values，**少一项就会出问题**。带 ⚠️ 的项在表下单独说明。

| 配置项 | 本文档取值 | 说明 |
| --- | --- | --- |
| `cni.chainingMode` | `generic-veth` | 显式指定。不设时 cilium-cni 会退化为按 conflist 的 `name` 去查同名 chainer —— 改个网络名就静默停止串接。 |
| `cni.customConf` | `true` | `generic-veth` 的**硬性前提**：Cilium 没有该模式的 conflist 模板，`false` 与它自相矛盾，agent 会卡在 `0/1 Running`。 |
| `cni.exclusive` | `false` | 纵深防御。`customConf=true` 时这个键不进 configmap，实际是惰性的。 |
| `cni.install` | `true` | 本项目需要 `/opt/cni/bin/cilium-cni` 这个二进制，由 Cilium 的 init 容器装。 |
| `routingMode` | `native` | Pod IP 是 VPC 原生可路由的 ENI 辅助 IP，不需要隧道。 |
| `ipam.mode` | `delegated-plugin` | 告诉 Cilium 地址由 CNI 二进制分配、它不要参与。 |
| `extraConfig.local-router-ipv4` | `169.254.32.1` | ⚠️ **不是 helm value**，见下。取值只需避开 `169.254.1.1`（本插件写进容器的默认网关）。 |
| `endpointHealthChecking.enabled` | `false` | `delegated-plugin` 的强制伴随项。 |
| `envoyConfig.enabled` | `false` | 同上。代价是放弃 Ingress / Gateway API。 |
| `endpointRoutes.enabled` | `true` | 同上。不设会让 agent CrashLoop；该项不在 `checkIPAMDelegatedPlugin()` 的校验范围内。 |
| `kubeProxyReplacement` | `true` | ⚠️ 与下一项**成对**启用，见下。 |
| `bpf.hostLegacyRouting` | `false` | ⚠️ 同上。**单独设是无效的**：agent 在 `kubeProxyReplacement` 不为 `true` 时会把它强制改回 `true`，于是这一项什么都不做，只会让人误以为已经启用。 |
| `enableIPv4Masquerade` | `false` | ⚠️ masquerade 必须全关，见下。 |
| `enableIPv6Masquerade` | `false` | 同上。 |
| `bpf.masquerade` | `false` | 同上。 |
| `extraConfig.enable-session-affinity` | `true` | KPR 开启时由 Cilium 自动启用；显式写出是为了关掉 KPR 后配置依然正确。不写而又关掉 KPR，`sessionAffinity: ClientIP` 会被静默忽略。 |
| `l7Proxy` | 不设置 | 保持 Cilium 默认 `true`。**关掉它，已下发的 L7 与 FQDN 策略的失效方式是「放行」而不是报错** —— 规则里的 L7 部分被忽略，退化成 L4 放行，没有任何告警。 |
| `operator.replicas` | `2` | 与社区 chart 默认一致。**不要降到 1**，理由见 §1 的说明框。 |
| `ipv6.enabled` | 仅双栈设 `true` | 见 [§4.3](#43-ipv4-单栈与双栈)。 |
| `extraConfig.local-router-ipv6` | 仅双栈设 | 同上，取值必须避开 `fe80::1`。 |

**⚠️ masquerade 必须全关。** Pod IP 在 VPC 内本身可路由，开着会把 Pod 的源地址改写成节点地址，
VPC 侧看到的来源就不再是 Pod。而且 agent 启动时若发现启用了 **iptables 方式**的 masquerade，
会强制回落到宿主协议栈，`bpf.hostLegacyRouting=false` 随之失效。

**⚠️ `local-router-ipv4` / `local-router-ipv6` 只能走 `extraConfig`。** 它们不是 helm value ——
写成 `--set localRouterIPv4=` 会被**静默丢弃**，然后 agent 因为缺这一项而启动失败。

**不要单独设 `enable-node-port`。** `kubeProxyReplacement=true` 时 agent 会自动派生它，
连同 `enable-external-ips`、`bpf-lb-sock`、`enable-host-port`、`enable-session-affinity`
一起。ConfigMap 是输入而不是生效配置，KPR 相关项一律以 `cilium-dbg config -a` 为准。

**`hostPort` 必须选在 30000 以下。** `kubeProxyReplacement=true` 时 hostPort 由 Cilium 自己的
BPF 负载均衡承担，它会拒绝任何落在 NodePort 范围（30000–32767）内的端口，除一条
`The requested hostPort ... is colliding with the configured NodePort range` 日志外没有任何提示：
Pod 正常启动，`cilium-dbg status --verbose` 照样显示 `HostPort: Enabled`，
只有 `cilium-dbg service list | grep HostPort` 才能看出没有建条目。

判断 eBPF host-routing 是否真的启用，用 `cilium-dbg status`，该字段跨版本稳定：

```bash
kubectl -n kube-system exec ds/cilium -c cilium-agent -- cilium-dbg status | grep '^Routing:'
# Routing:  Network: Native   Host: BPF      ← 已启用（本文默认档）
# Routing:  Network: Native   Host: Legacy   ← 走宿主协议栈
```

> 不要用 `grep ENABLE_HOST_ROUTING /run/cilium/state/globals/node_config.h` 作判据。
> 该宏在较新版本中已改为运行时配置项，grep 恒为 0，会被误读成「未启用」。

### 4.3 IPv4 单栈与双栈

先决定**要不要 Pod IPv6** —— 这一条决定了对 Kubernetes 集群本身的要求，装完再改代价很大。

| | 单栈（只要 Pod IPv4） | 双栈（Pod 要 IPv4 + IPv6） |
|---|---|---|
| 对 k8s 集群的要求 | 无 | ⚠️ **集群本身必须是双栈**（Node 对象带 IPv6） |
| `ipv6.enabled` | `false`（默认，不用写） | **`true`** |
| `extraConfig.local-router-ipv6` | 不需要 | **需要**，且必须避开 `fe80::1` |
| 本项目的 `zec.ipStack` | `ipv4`（默认） | **`dual`** |

单栈的完整安装命令就是[快速开始](#2-快速开始从-veth-切到本模式)第 1 步。双栈在它之上追加：

```bash
  --set ipv6.enabled=true \
  --set-string extraConfig.local-router-ipv6=fe80::ffff:1
```

并且必须**先**让每个节点的 Node 对象带上 IPv6。Node 对象的地址来自 kubelet 的 `--node-ip`：

```bash
# 在每个节点上执行；<v4> 是该节点现有的 InternalIP，
# <v6> 取【承载该 v4 的同一块网卡】上的全局 IPv6
cp -n /etc/default/kubelet /etc/default/kubelet.bak
echo "KUBELET_EXTRA_ARGS=--node-ip=<v4>,<v6>" > /etc/default/kubelet
systemctl restart kubelet
kubectl get node <name> -o jsonpath='{.status.addresses}'
```

- **IPv6 必须取自承载 node IP 的那块网卡。** 多 ENI 节点上有多个全局 IPv6，取错不会立刻报错，
  但 Node 对象会声明一个属于另一块网卡的地址，控制面与数据面就此对不上。
  节点约 10 秒重新 `Ready`，已运行的 Pod 不受影响。
- **只需要 Node 对象双栈，不需要 Service 双栈。** apiserver 的 `--service-cluster-ip-range`
  保持单栈即可 —— 那只决定能否创建双栈 Service，与 Pod 的 IPv6 连通性无关。
- **两个方向的失败都没有明确提示**：单栈集群上开 `ipv6.enabled` 会让 agent 因 Node 对象没有
  IPv6 而 fatal 退出；双栈集群上不开，Pod 拿得到 IPv6 地址、配置也全对，但每个包都被 tcx
  程序按 `Unsupported L3 protocol` 丢弃，**不产生任何日志**。

## 5. 回退到 veth 模式

**顺序是固定的，第 ⓪、②、③ 步都不能跳过，也不能与第 ④ 步交换。**

```bash
# ⓪ 先把 kube-proxy 放回来（快速开始第 4 步把它停掉了）
kubectl -n kube-system patch ds kube-proxy --type json \
  -p '[{"op":"remove","path":"/spec/template/spec/nodeSelector/kube-proxy-disabled"}]'
kubectl -n kube-system rollout status ds/kube-proxy --timeout=5m
# 确认它真的接管了（在节点上执行）
iptables -t nat -S | grep -c '^-N KUBE-SVC'                                # 期望 > 0
curl -sk -o /dev/null -w '%{http_code}\n' https://10.96.0.1:443/healthz   # 期望 200

# ① 关掉两个开关，让 conflist 去掉 cilium-cni 段
helm upgrade zenlayer-vpc-cni \
  oci://registry-1.docker.io/zenlayer1/zenlayer-vpc-cni --version 1.0.0 \
  -n kube-system --reuse-values \
  --set cilium.enabled=false --set networkPolicy.enabled=false
kubectl -n kube-system rollout restart daemonset/zenlayer-vpc-cni
kubectl -n kube-system rollout status daemonset/zenlayer-vpc-cni --timeout=600s

# ② 确认【每个】节点上的 conflist 都不再含 cilium-cni
for n in <每个节点>; do ssh root@$n \
  'grep -c cilium-cni /etc/cni/net.d/10-zenlayer-cni.conflist'; done   # 每台都必须是 0

# ③ 滚动重启所有工作负载 —— 必须在卸载 Cilium【之前】做
for t in deployment statefulset daemonset; do
  kubectl get "$t" -A -o custom-columns=NS:.metadata.namespace,N:.metadata.name --no-headers \
  | while read -r ns n; do kubectl -n "$ns" rollout restart "$t/$n"; done
done

# ④ 再卸载 Cilium。保持 cni.uninstall 为默认的 false（理由见下）
helm uninstall cilium -n kube-system

# ⑤ 清残留 BPF —— 网络设备与 cgroup 两类都要清，见下
```

> **第 ⓪ 步不能跳过。** 默认档下 Service 全部由 Cilium 承担，直接卸载会出现一段没有任何组件
> 处理 Service 的窗口 —— 期间宿主命名空间访问 `10.96.0.1` 会失败，以 `hostNetwork: true`
> 运行的组件（含本项目的 daemon）会因为连不上 API server 而崩溃。

**第 ③ 步为什么必须在卸载之前**：Pod veth 上的 `cil_from_container` / `cil_to_container` 是
**BPF link**，不随 agent 卸载消失，`helm uninstall` 也不清理。agent 还在跑时它会持续维护这些
程序依赖的 LB map，所以这个窗口是安全的；**一旦卸载，map 就冻结了** —— 那些 Pod 的 ClusterIP
流量会继续走一份永不更新的负载均衡表，删掉某个后端它们仍会把相应比例的连接发往已删除的地址，
而 `KUBE-SVC-*` 计数为 0，沿 kube-proxy 方向排查不会有任何发现。

> **不要设 `cni.uninstall=true`**，直到第 ② 步在所有节点确认完成。在还有节点挂着串接版
> conflist 时删掉 `/opt/cni/bin/cilium-cni`，会同时打死该节点的 ADD 和 DEL —— DEL 失败会在
> 本插件之前中止链路，**地址与 veth 双泄漏**，Pod 卡在 `Terminating`。把二进制放回原处后
> kubelet 会自行补完拆除。
>
> **批量删 Pod 时留出时间**：agent 不可达期间每个 Pod 固定多付约 20 秒，且逐个串行。

### 清理残留 BPF

残留分两类，**都用 `link detach`**。第二类挂在 cgroup 上，重启节点之外没有别的办法够得着它，
而且后果更重：它配着一份冻结的 LB map 继续在 `connect()` 时翻译 ClusterIP，**影响该节点上的
每一个 Pod**。症状很有迷惑性 —— 卸载前就存在的 Service（如 kube-dns）照常翻译，所以 DNS 是
通的，而卸载后新建的 Service 完全不可用，包在 Pod 内就被改写成一个不存在的后端地址。

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

`cilium_host` 与 `cilium_net` 同样会留下来，`link detach` 与 `helm uninstall` 都不删它们。
在每个节点上删除：

```bash
ip link del cilium_host
```

两者是一对 veth，删掉一个另一个随之消失。

**确认卸干净**（在每个节点上执行，三条都必须为 0）：

```bash
bpftool net show                   | grep -c cil_    # 网络设备上的
bpftool cgroup show /sys/fs/cgroup | grep -c cil_    # cgroup 上的 —— 最容易漏
ip -o link show                    | grep -c cilium  # cilium_host / cilium_net
```

> ⚠️ **不要用 `kubectl get cep` 判断 Cilium 是否还在** —— 它会骗人。CEP 对象在
> `helm uninstall` 之后会原样留在 API server 里，输出看起来完全正常（`ready`、IPv4/IPv6 齐全），
> 而 Cilium 早已不存在。判断用上面那三条，或 `kubectl -n kube-system get ds cilium`。
>
> 排查 veth 上的残留**不能用 `tc filter show`** —— tcx 挂点在 tc filter 里看不见，
> 返回空是**假阴性**，要用 `bpftool net show | grep cil_`。

其余残留（Cilium CRD、CEP / CiliumIdentity 对象、`/opt/cni/bin/cilium-cni`、`/run/cilium/`）
不影响本插件工作，按需手工清：

```bash
kubectl delete cep --all -A
kubectl delete ciliumidentity --all
kubectl get crd | grep cilium | awk '{print $1}' | xargs -r kubectl delete crd   # 重装会自动重建
```
