# Performance: measurements and how to read them

> This document is the authoritative version. A Chinese translation is available at
> [performance-cn.md](./performance-cn.md).

This document is the single source for the project's performance data: the measured numbers for
each shape, **where every number comes from**, and **which comparisons hold and which do not**.
The deployment guides cite the conclusions and do not repeat the data.

To determine only which switches to enable, read section 1.

All data comes from the same five-node test cluster, collected 2026-09-15 to 2026-09-16.

## 1. Conclusions

| Conclusion | Evidence |
|---|---|
| **The gain comes from `bpf.hostLegacyRouting=false`**: +16% to +31% on Pod traffic | C to D in §4 -- same session, one key changed |
| **`kubeProxyReplacement=true` on its own buys nothing measurable on Pod-originated traffic** | B and C in §4 nearly coincide. Its value is being the **precondition** for the line above, and taking over NodePort and Service traffic originating in the host namespace |
| **With the two defaults turned off, chaining is a straight loss**: 3% to 10% slower than not chaining at all | §5. The cost of the tc/tcx programs is only repaid once host-routing removes the corresponding host stack work |
| **Chaining also slows hostNetwork traffic by about 8%, and that part is never repaid** | §5. The programs on the host NIC are pure cost for hostNetwork traffic |
| **Delegated mode (Cilium creating netkit devices) adds nothing measurable over the default** | §6. That is "not measurable", not "proven to be zero" -- for the reason and the conditions see §7 |

So use the **default shape** of [deploy-cilium-chained.md](deploy-cilium-chained.md) (shape D):
those two settings are already that document's defaults, no extra code path is involved, and
delegated mode is not needed.

## 2. The shapes measured

| Tag | Shape |
|---|---|
| **A** | This plugin only, no Cilium (veth + per-ENI policy routing, Services forwarded by kube-proxy) |
| **B** | Chained with both defaults turned off: `kubeProxyReplacement` off and `bpf.hostLegacyRouting` unset (equivalent to `true`). This is the shape covered by the "turning KPR off" section of the deployment guide |
| **C** | Chained + `kubeProxyReplacement=true`, but `bpf.hostLegacyRouting=true` |
| **D** | **The default shape of chained mode**: `kubeProxyReplacement=true` + `bpf.hostLegacyRouting=false`, kube-proxy removed |
| **E** | Delegated: `cni.mode=delegated` + `bpf.datapathMode=netkit`, everything else as D (see [deploy-cilium-netkit.md](deploy-cilium-netkit.md)) |

## 3. Method

```bash
netperf -H <target> -p 12865 -t TCP_RR -l 10 -- -r 1,1 -P ,12866
```

- **The primary metric is TCP_RR (request/response rate)**, not throughput. Latency and pps are
  where per-packet datapath cost shows up; throughput on a 25 Gbps link is easily limited by
  something else.
- **`-P ,12866` is not optional.** The TCP_RR control connection uses 12865 but the data
  connection defaults to a random port, which cannot come back through a ClusterIP (you get a row
  of zeros). Pin the data port and add it to the Service.
- Each cell is the **median of 3 runs** (some scenarios in shape E have 10 to 17 -- see the note
  under the table).
- Scenarios: same-node Pod to PodIP, cross-node Pod to PodIP, same-node Pod to ClusterIP,
  cross-node Pod to ClusterIP, plus **cross-node hostNetwork Pod** as an anchor (§4.2 explains how
  to use it).
- Environment: the two nodes under test were 8 vCPU / 16 GiB and 16 vCPU / 32 GiB, 25 Gbps NICs,
  a dual-stack cluster, Ubuntu 26.04 (kernel 7.0), containerd 2.2, Kubernetes v1.34, Cilium
  v1.17.5, load below 0.3 before each run. The test itself uses about 50 Mbps, 0.2% of the link.
- **Absolute figures vary with hardware and kernel**; the percentages here describe only the
  relationship between the shapes.

## 4. Results

TCP_RR, txn/s, median:

| Scenario | A plugin only | B chained, both off | C chained+KPR | **D chained default** | E delegated+netkit |
|---|---|---|---|---|---|
| **hostNetwork cross-node** (anchor) | 15493 | 15440 | 14959 | 16584 | 15156 |
| Same-node Pod to PodIP | 27825 | 25503 | 25630 | **31274** | 32421 |
| Cross-node Pod to PodIP | 14079 | 13386 | 13057 | **15163** | 15236 |
| Same-node Pod to ClusterIP | 27216 | 24961 | 25247 | **33172** | 31902 |
| Cross-node Pod to ClusterIP | 13458 | 13174 | 12945 | **16713** | 15460 |

> **Sessions**: A, C and D were measured back to back in one session; B comes from the previous
> day; E from that evening. This determines which columns may be compared -- see §4.2.
> **Sample counts**: 3 per cell for A through D. For E, the two same-node rows and cross-node Pod
> to PodIP have 10, hostNetwork has 17, and cross-node Pod to ClusterIP has 3.

The single-variable comparison within one session (C to D changes only `bpf.hostLegacyRouting`,
with every other parameter, node and Pod held constant):

| Scenario | C | D | Gain |
|---|---|---|---|
| Same-node Pod to PodIP | 25630 | 31274 | **+22%** |
| Cross-node Pod to PodIP | 13057 | 15163 | **+16%** |
| Same-node Pod to ClusterIP | 25247 | 33172 | **+31%** |
| Cross-node Pod to ClusterIP | 12945 | 16713 | **+29%** |

This is the most tightly controlled group in the whole data set, and the first two conclusions in
section 1 rest on it.

### 4.1 A second, independent measurement

The previous day produced another single-session set covering A, B and D, which serves as a
replication of the table above:

| Scenario | A plain veth | B chained, both off | D chained default |
|---|---|---|---|
| hostNetwork cross-node | 16783 | 15440 | 15030 |
| Same-node Pod to PodIP | 27871 | 25503 | 32355 |
| Cross-node Pod to PodIP | 14421 | 13386 | 15801 |
| Same-node Pod to ClusterIP | 27762 | 24961 | 32925 |
| Cross-node Pod to ClusterIP | 13634 | 13174 | 17542 |

Against A: B is **-3.4% to -10.1%**, D is **+9.6% to +28.7%**. The absolute values differ from the
first set by a few percent, but **the direction and the magnitude agree**.

### 4.2 How to read the two tables

**The anchor is not a constant; it is comparable only across part of the shapes.** A hostNetwork
Pod traverses no CNI-created Pod device, so it should be equal across B, C, D and E, and the
observed spread there (14959 to 16584) is **systematic drift of roughly +-8%** from time of day,
CPU frequency and neighbour noise.

**That does not extend to A.** Installing Cilium attaches tc/tcx programs to the host NIC, and
hostNetwork traffic **does traverse the host NIC** -- the cost on that path is real, not drift.
The 16783 to 15440 step in §4.1 (-8.0%) is exactly that; see §5.

Three rules follow:

1. **Absolute values may only be compared within one session.** A/C/D and the three shapes in §4.1
   each satisfy this, so both the +16% to +31% from C to D and the +9.6% to +28.7% in §4.1 hold. B
   and E come from other sessions, so their differences against the other columns are only
   meaningful above 8%.
2. **The ratio form (Pod divided by the hostNetwork of the same session) removes systematic drift
   but amplifies sampling noise.** The sampling error of numerator and denominator compound, so
   with a thinly sampled denominator the ratio is less stable than the absolute value. Concretely:
   the cross-node ratio for the default shape D was **1.05** in §4.1 (Pod ahead of hostNetwork) and
   **0.914** when re-measured for the main table -- the same shape, 13 percentage points apart.
   **Do not quote this as a result.**
3. **Across A and the other shapes neither form applies directly**, because the anchor itself was
   changed by the shape. There, only the Pod-side absolute values can be compared, drift included.

## 5. What chaining itself costs

Chaining means Cilium attaches tc/tcx programs to every Pod veth and to the host NIC, and every
packet passes through them. **With the defaults `kubeProxyReplacement` and
`bpf.hostLegacyRouting=false` turned off, that cost is purely additive**: the BPF work is done and
none of the host stack work is skipped.

| Shape | Pod traffic against A (plugin only) |
|---|---|
| B chained, both off | **-3% to -10%** |
| D chained default | **+9.6% to +28.7%** |

(The range for B is the union of both sets: -3.4% to -10.1% in §4.1, and -3.8% to -7.9% for C
against A in the main table.)

Once host-routing is on, that BPF cost turns from **additional** into **substitutive** -- the
corresponding host stack work no longer runs, so the cost is recovered and becomes a net gain.

So **with those two turned off, chaining trades 3% to 10% of performance for NetworkPolicy and
Hubble**. The default shape does not carry that trade-off, so do not turn them off without a
separate reason.

**One part is never recovered**: chaining also slows **hostNetwork traffic** by about 8% (16783 to
15440 in §4.1). The programs on the host NIC are pure cost for that traffic, and host-routing
cannot help -- what it removes is the leg between the Pod and the host stack, and hostNetwork
traffic is already in the host stack. This is an inherent cost of chaining.

## 6. Why delegated mode (netkit) shows no gain

The four Pod scenarios differ between E and D by **+3.7% / -3.8% / -0.3% / -7.5%** -- the signs
disagree, and every one of them is smaller than the spread of repeated runs within its own
scenario (4.9% to 11.6% same-node, 17.9% to 19.7% cross-node). Meanwhile the anchor itself moved
by -8.6%, more than any of those deltas.

**This is "not measurable", not "proven to be zero"**. A cross-session comparison has no resolving
power at this magnitude.

### 6.1 netkit L3 against netkit L2

The two netkit modes were compared the only way this magnitude can be compared: **within one
session, changing `bpf.datapathMode` and nothing else**, with the Pods recreated in between and the
hostNetwork anchor collected on both sides.

| Scenario | L3 (`netkit`) | L2 (`netkit-l2`) | Delta | Spread within a shape |
|---|---|---|---|---|
| **hostNetwork cross-node** (anchor) | 15598 | 15787 | +1.2% | 13% / 22% |
| Same-node Pod to PodIP (n=15) | 31531 | 31855 | +1.0% | 10.0% / 10.3% |
| Cross-node Pod to PodIP (n=10) | 16255 | 16452 | +1.2% | — |

**All three rows moved by the same +1%, including the anchor**, which no CNI setting can affect.
That is the cleanest statement the data supports: the Pod-side difference between L3 and L2 is
session noise, and every delta is an order of magnitude below the spread of repeated runs.

The expected direction is that L2 costs slightly more -- it carries an Ethernet header and resolves
neighbours, where L3 does neither. Converted to per-transaction time the same-node figures are
31.71 us for L3 and 31.39 us for L2, a difference of 0.32 us that falls on the wrong side of the
prediction, which is what noise looks like.

Confirmation that the two shapes were really in effect: the Pod interface MAC was
`00:00:00:00:00:00` under L3 and `3e:b5:a2:c8:45:8f` under L2.


## 7. What netkit removes, and how much it is worth

What a netkit device does is **move the Pod's BPF program into the device's transmit routine
itself**: traffic leaving the node is classified in the sender's process context and forwarded
straight to the physical device, without entering the per-CPU backlog queue and without switching
to softirq context (kernel commit `netkit, bpf: Add bpf programmable net device`, v6.7). Layer 3
mode additionally removes the Ethernet header and neighbour resolution on the Pod side.

What matters is **which segment that saves**. Upstream states eBPF host-routing as netkit's
precondition (`Documentation/operations/performance/tuning.rst`); the two are consecutive segments
of one path, not two independent wins:

- **eBPF host-routing** bypasses iptables and the upper host stack -- the large part, which is the
  +16% to +31% measured in §4;
- **netkit** only zeroes out the network namespace switch that is left.

The default shape (D) already has host-routing on, so D to E can only be that remainder.
Converting the measurements into per-transaction time shows its size:

| Scenario | D | E | Delta | Share |
|---|---|---|---|---|
| Same-node Pod to PodIP | 31.97 us | 30.84 us | 1.13 us | 3.5% |
| Cross-node Pod to PodIP | 65.95 us | 65.63 us | 0.32 us | 0.5% |

One TCP_RR transaction crosses four namespace boundaries (in and out at both ends), so the
same-node 1.13 us works out to about **0.28 us per crossing** -- the right order of magnitude for
one backlog enqueue plus one softirq wakeup, so both the sign and the size match what the
mechanism predicts. The problem is that it is 3.5% of the transaction while repeated runs of that
same scenario spread by 4.9%: **below the resolution of the measurement**.

Cross-node is less favourable still: the same 1.13 us inside a 66 us transaction is 1.7%, against
a spread near 20%, and the wire and the NIC take almost all of that time.

**The condition under which this gain grows is CPU and softirq saturation** (high pps, many
parallel streams), where the per-packet cost saved turns into tail latency and CPU headroom. The
measurement here is a single-stream, one-byte latency test on idle nodes -- precisely the shape in
which it cannot show. If your workload saturates a node's softirq processing, measure it yourself;
otherwise pick a shape by §1.

## 8. Reproducing this

```bash
# 1) Start the bench Pods: one same-node pair, one cross-node pair, one hostNetwork pair.
#    The tool image needs netperf, and the Service must expose both 12865 and 12866.
# 2) Per shape:
for i in $(seq 10); do
  kubectl exec bench-cli -- netperf -H <target-ip> -p 12865 -t TCP_RR -l 10 -- -r 1,1 -P ,12866 \
    | awk 'NR==7{print $6}'
done
```

Things to get right:

- **Put the shapes you want to compare into one session**; do not compare across days.
- **Collect the anchor for every shape**, or there is no way to tell a real difference from drift.
- **Three runs cannot resolve differences under 5%.** At this environment's spread, the same-node
  scenarios need roughly n >= 20.
- **Recreate the Pods after changing shape.** The device type is fixed when a Pod is created;
  existing Pods do not follow.

## 9. Not covered here

Throughput (parallel streams), CPU cost (softirq% and total host CPU), IPv6, NodePort, and Service
traffic originating in the host namespace. The last two are the paths `kubeProxyReplacement`
uniquely owns, so the "nothing measurable" conclusion above holds only within **Pod-originated
traffic**.
