## What this changes

<!-- A short description of the change and why it is needed. -->

Fixes #

## Type of change

- [ ] Bug fix
- [ ] New feature
- [ ] Refactor / cleanup
- [ ] Helm chart change
- [ ] Documentation
- [ ] CI / build

## How it was tested

<!-- Be specific. For datapath changes, state the datapath, the kernel and the
     Kubernetes version you tested on. -->

- [ ] `go test ./...`
- [ ] `go vet ./...` and `golangci-lint run` are clean
- [ ] `gofmt` clean (generated code under `rpc/` excluded)
- [ ] `make chart-lint` and `make chart-template` pass (if the chart changed)
- [ ] Verified on a live cluster — kernel: ___, k8s: ___

## Upgrade impact

- [ ] No user-visible change
- [ ] Chart values added/renamed/removed (README configuration table updated)
- [ ] Requires a node drain (e.g. a datapath change)
- [ ] Requires new Zenlayer OpenAPI permissions or RBAC rules

## Checklist

- [ ] Commits are focused and have meaningful messages
- [ ] No credentials, access keys or customer identifiers in code, values, tests or logs
- [ ] `go.mod` / `go.sum` are tidy if dependencies changed
- [ ] New source files carry the SPDX header (`make license` adds it)
- [ ] Docs updated (README / CONTRIBUTING) where behavior changed
