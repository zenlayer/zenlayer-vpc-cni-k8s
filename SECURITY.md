# Security Policy

## Supported versions

This project is developed on the `main` branch. Security fixes are applied to
`main` and released in the next chart/image version. Please make sure you can
reproduce an issue against a recent build before reporting it.

## Reporting a vulnerability

**Do not open a public GitHub issue for a security vulnerability.**

Report it privately instead:

- Email **security@zenlayer.com**, with `zenlayer-vpc-cni` in the subject line.
  This is the preferred channel.
- Alternatively, use GitHub's private vulnerability reporting for this
  repository (Security -> Report a vulnerability).

Do not include details of the vulnerability in any public message.

Please include:

- a description of the issue and the impact you believe it has;
- the version (chart version, image tag, or commit) you tested;
- the node kernel version and the Kubernetes version;
- the address family the node was configured for (`zec.ipStack`);
- reproduction steps or a proof of concept;
- any logs, with credentials removed.

### What to expect

| Step | Target |
| --- | --- |
| Acknowledgement that the report was received | 3 working days |
| An initial assessment -- whether it reproduces, and how severe we judge it | 10 working days |
| A fix released, or a written explanation of why it will take longer | 90 days from the report |

Those are targets, not guarantees, and the clock is working days in China
Standard Time. If you have not heard back within the acknowledgement window,
please send a reminder to the same address -- a missed mail is more likely than
a deliberate silence.

We will keep you updated while we investigate, coordinate a disclosure date with
you before publishing, and credit you in the release notes unless you prefer
otherwise. If we conclude a report is not a vulnerability we will say so, with
the reasoning, rather than letting it lapse.

## Scope

In scope: the `zec-cni` plugin, the `zec-ipam` daemon, the Helm chart, the
container image build, and the bootstrap script installed on nodes.

Out of scope: vulnerabilities in Kubernetes itself, in kube-proxy, in the
Zenlayer platform or OpenAPI (report those to Zenlayer support), and issues that
require an attacker to already have root on the node.

## Security-relevant design notes

Useful context when assessing a report:

- The `zec-ipam` DaemonSet runs privileged, with `hostNetwork` and `hostPID`,
  and mounts host paths (`/opt/cni/bin`, `/etc/cni/net.d`, `/var/run/netns`,
  `/var/lib/cni`, `/lib/modules`). It has to, in order to program node
  networking and install the CNI binary.
- Zenlayer OpenAPI credentials are read from files mounted from a Kubernetes
  Secret at `/etc/zec_secret/` (mode `0400`). They are deliberately not stored
  in the chart's values or in the ConfigMap, and the daemon redacts them when
  it logs its configuration.
- The daemon's RBAC is limited to reading nodes, pods and ConfigMaps, patching
  pod annotations, creating pod evictions, and recording events.
- The plugin talks to the daemon over a Unix domain socket at
  `/var/run/zenlayer/cni.socket`; anything able to reach that socket on the
  node can request IP allocation.
- `/healthz` and `/metrics` are served on the node's own network namespace on
  `zec.healthAndDebugPort` (default `11414`). Because the pod is `hostNetwork`,
  restrict access to that port at the node firewall if your nodes are exposed.
  No profiling endpoints are served: this listener has to be reachable from off
  the node for kubelet probes and Prometheus scrapes, and `/debug/pprof` on such
  an address would hand a heap dump, a CPU profile and the process command line
  to anything that can reach it.
- Pod IPs are real, routable VPC addresses. Reachability is governed by the
  security group configured in `zec.securityGroupID` and by your VPC routing —
  this plugin implements no NetworkPolicy.
