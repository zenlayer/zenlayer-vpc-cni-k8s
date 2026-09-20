# Contributing to Zenlayer VPC CNI for Kubernetes

Thanks for your interest in improving this project. This document describes how
to build, test and submit changes.

## Code of conduct

This project follows the [Contributor Covenant](CODE_OF_CONDUCT.md). Report
unacceptable behavior to security@zenlayer.com.

## Getting started

Prerequisites:

- Go 1.26.8 or newer (see `go.mod`).
- `make`.
- Docker (only for `make image`, and only on an `amd64` host).
- Helm 3 (only for the chart targets).

```sh
git clone https://github.com/zenlayer/zenlayer-vpc-cni-k8s.git
cd zenlayer-vpc-cni-k8s
make bin          # builds output/zec-cni and output/zec-ipam
go test ./...     # on Linux; see below
```

Both binaries are built with `CGO_ENABLED=0 GOOS=linux`; the plugin only runs
on Linux, so cross-compiling from another operating system is expected and fine.

The tests are a different matter. Several packages depend on Linux-only netns
helpers and do not build elsewhere, so `go test ./...` needs a Linux machine or
container. On a non-Linux workstation, `GOOS=linux go vet ./...` type-checks
everything including test files, which catches the compile errors `go build`
misses -- `go build` does not compile `_test.go`.

## Repository layout

| Path | Contents |
| --- | --- |
| `cmd/zenlayer-vpc-cni` | `zec-cni`, the CNI plugin binary kubelet invokes. |
| `cmd/cni-ipam` | `zec-ipam`, the per-node daemon. |
| `pkg/plugin` | CNI-side logic: conflist parsing, the gRPC client, netns setup. |
| `pkg/plugin/datapath` | The datapath: `policy_route.go` (veth pair + per-ENI policy routing). |
| `pkg/daemon` | The daemon: gRPC server, ENI/IP management, pool, reconciliation. |
| `pkg/zapi` | Zenlayer OpenAPI and metadata clients. |
| `pkg/pool` | The warm-IP pool. |
| `pkg/utils/linux` | netlink, tc, sysctl and NIC helpers. |
| `rpc` | Generated protobuf/gRPC stubs — do not edit by hand, except `ipam.proto` and `generate.go`. |
| `chart` | The Helm chart. |
| `images` | Dockerfile for the runtime image. |
| `script/bootstrap` | `install_env.sh`, run by the DaemonSet init container. |
| `docs` | User-facing guides, notably the Cilium chaining deployment walkthrough. |

## Before you open a pull request

Run the same checks CI runs:

```sh
gofmt -l $(find . -name '*.go' -not -path './rpc/*')   # must print nothing
make license-check                                      # SPDX headers present
go mod tidy && git diff --exit-code -- go.mod go.sum
go vet ./...
CGO_ENABLED=1 go test -race -count=1 ./...
golangci-lint run                                       # config in .golangci.yml
make chart-lint
make chart-template
```

`go.mod` ends with a `replace` pointing `github.com/vishvananda/netlink` at a
fork. The datapath depends on behavior only that fork has, so `go get -u` on the
upstream module changes nothing, and dropping the `replace` as an unused
directive swaps out the whole netlink implementation. The packages that would
notice are Linux-only, so the change compiles and the tests pass on macOS.

CI runs the Go steps with `GOOS=linux CGO_ENABLED=0` for the job and overrides
`CGO_ENABLED=1` for the test step, because `-race` needs cgo.

`golangci-lint` must be a v2.x release — `.golangci.yml` uses the v2 schema, and
CI pins v2.12.2. Generated code under `rpc/` is excluded from linting and
formatting checks.

If you change `go.mod` or add dependencies, run `go mod tidy` and commit the
resulting `go.mod`/`go.sum`; CI fails if they are not tidy.

## Changing the chart

`chart/values.yaml` is the user-facing surface. When you add or rename a value:

- update the comment next to it in `values.yaml`,
- update the corresponding template (`chart/templates/`),
- update `chart/values.schema.json` — the root and every section set
  `additionalProperties: false`, so a value absent from the schema is rejected
  at render time,
- update the configuration table in **both** `README.md` and `README-CN.md`,
- verify with `make chart-lint` and `make chart-template`.

Never add credentials to `values.yaml`. The Zenlayer OpenAPI access key lives in
a Secret the operator creates out of band and the daemon reads from
`/etc/zec_secret/`; keep it that way.

## Changing the datapath

There is one datapath — a veth pair plus per-ENI policy routing — and it
manipulates host networking as root. If you touch it:

- state clearly in the PR what you tested and on what kernel;
- make sure teardown is idempotent — the CNI `DEL` may be retried, and it must
  not tear down state that belongs to a different sandbox of the same pod;
- check that both pod-to-pod and pod-to-service traffic still work, plus
  node-to-pod (this is the kubelet probe path) and pod egress to the internet;
- if you add a datapath, keep the selection in `getDatePath` refusing unknown
  values rather than defaulting — a node silently running a datapath other than
  the configured one is far harder to diagnose than a pod that will not start.

## Tests

- Unit tests live next to the code as `*_test.go` and must not require root,
  network access or a real cluster.
- Code that must call netlink, tc or the OpenAPI should be behind an interface
  so it can be faked in tests.
- Run `go test ./...` on Linux before pushing.

## Documentation conventions

Four documents are user-facing, and each wraps at its own width. Match the file
you are editing rather than reflowing it:

| File | Wrap at | Counted in |
| --- | --- | --- |
| `README.md`, `CONTRIBUTING.md` | 80 | characters |
| `docs/deploy-cilium-chained.md` | 108 | characters |
| `README-CN.md` | 90 | display columns -- a CJK character is two |
| `docs/deploy-cilium-chained-cn.md` | 92 | display columns |

Tables and headings are exempt: neither can wrap. Everything else does, and a
line that runs well past its file's width shows up as a wide diff for the next
person to touch that paragraph.

Two more that are easy to miss:

- **Every code fence takes a language.** Use `text` for log excerpts, command
  output and error messages -- there is no language, but a bare fence next to
  sixty tagged ones reads as an oversight.
- **Terms are fixed, not synonyms.** `datapath` (not "data plane" -- that phrase
  is reserved for the general control-plane/data-plane sense), `warm pool` for
  the address pool, `ENI` for a network interface, and `GC` for the periodic
  reconcile even though the knob is called `reconcileIntervalSec`. The Chinese
  documents keep these in English, the same way they keep `daemon`, `chart` and
  `conflist`.

## Cutting a release

There is no release automation in this repository: CI builds and tests, and
nothing pushes. The steps below are the whole process, and the order matters
because the binaries take their version from `git describe`.

1. **Add a `## [X.Y.Z] - YYYY-MM-DD` heading** to `CHANGELOG.md` with the
   changes under it. `make version-check` reads the newest version heading, so
   this is what defines the release number -- it matches only headings whose
   bracket starts with a digit.
2. **Update the three other places a version appears**, all to the same number:
   `chart/Chart.yaml`'s `version` and `appVersion`, and `chart/values.yaml`'s
   `image.tag`. The last one is what a cluster actually pulls.
3. **`make version-check`** -- it fails if any of the four disagree. Commit.
4. **Tag the commit**: `git tag vX.Y.Z`. Do this before building: `VERSION` comes
   from `git describe --tags --always --dirty`, so a build from an untagged or
   dirty tree stamps the binaries with a bare SHA or `-dirty` instead of the
   release number, and `zec-cni version` then cannot be traced back to a tag.
5. **`make all`** -- builds `output/zec-cni` and `output/zec-ipam`, copies the
   bootstrap scripts beside them, builds the image as
   `docker.io/zenlayer1/zeccni:vX.Y.Z`, and packages the chart into
   `output/charts/`. Needs an `amd64` host with docker or podman, plus helm.
6. **Publish**, by hand -- there is no `make push`:
   - the image, to the registry `chart/values.yaml`'s `image.repository` names;
   - the packaged chart, to the OCI location the README's install command uses.
7. **Push the tag** once the artifacts are up, so a tag never points at a
   release that was not published.

## Commit messages and pull requests

- Write commits in the imperative mood: "fix ENI leak on failed attach".
- Keep unrelated changes in separate commits.
- In the PR description explain *why* the change is needed, what you tested and
  any upgrade impact (in particular anything requiring a node drain).
- Link the issue the PR resolves.
- Expect review comments; CI must be green before merge.

## Reporting bugs and requesting features

Use the issue templates. For bugs, include the chart values you installed with
(redact IDs and never paste credentials), the address family (`zec.ipStack`),
whether Cilium chaining is enabled, the kernel and Kubernetes versions, and the
relevant logs from
`kubectl -n kube-system logs ds/zenlayer-vpc-cni -c zec-ipam` and
`/var/log/zenlayer/cni.log`.

## Security issues

Do not open a public issue for a vulnerability. See [SECURITY.md](SECURITY.md).

## License

By contributing you agree that your contributions are licensed under the
Apache License 2.0, the same license that covers this repository.

Every hand-written source file carries a two-line SPDX header:

```go
// Copyright 2026 The Zenlayer Authors
// SPDX-License-Identifier: Apache-2.0
```

Run `make license` to add it to new files; CI runs `make license-check` and fails
if any file is missing one. The tool only ever adds a header where none exists,
so it will not touch the year on a file that already has one. Generated protobuf
under `rpc/` keeps whatever its generator emits and is excluded.
