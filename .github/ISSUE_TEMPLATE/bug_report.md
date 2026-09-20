---
name: Bug report
about: Report a problem with the CNI plugin, the daemon or the Helm chart
title: "[bug] "
labels: bug
assignees: ''
---

**Not for security vulnerabilities.** Report those privately -- see
[SECURITY.md](../../SECURITY.md). A vulnerability filed here is public the moment
you submit it.

<!--
Redact VPC/subnet/security-group IDs if you consider them sensitive, and never
paste access keys.
-->

## What happened

<!-- A clear description of the problem. -->

## What you expected to happen

## How to reproduce

1.
2.
3.

## Environment

- Chart version / image tag:
- Commit (`zec-ipam version` or `zec-cni version` output, if available):
- Kubernetes version (`kubectl version`):
- Node OS and kernel (`uname -a`):
- Installed with (redacted `helm install` command or values):

```yaml
# relevant part of your values.yaml
```

## Logs

<details>
<summary>zec-ipam (<code>kubectl -n kube-system logs ds/zenlayer-vpc-cni -c zec-ipam</code>)</summary>

```
paste here
```

</details>

<details>
<summary>zec-cni (<code>/var/log/zenlayer/cni.log</code> on the affected node)</summary>

```
paste here
```

</details>

<details>
<summary>Pod / node state (<code>kubectl describe pod</code>, <code>ip a</code>, <code>ip rule</code>, <code>ip route show table all</code>)</summary>

```
paste here
```

</details>

## Additional context

<!-- Anything else: how often it happens, whether it started after an upgrade,
     whether a node drain / restart works around it. -->
