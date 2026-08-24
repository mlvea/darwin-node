# darwin-node

This directory **is** the darwin-node repository. It is a standalone
Go module at the repo root. It is **not** a subdirectory of Agoda's
macOS-vz-kubelet.

## Do not use the old nested path

Earlier work lived at:

`…/macOS-vz-kubelet/darwin-node`

That nested layout is gone. This project is now a sibling of any local
Agoda clone, not a folder inside it.

- Repo root: this folder (`go.mod`, `LICENSE`, `NOTICE`, `cmd/`, `pkg/`)
- Go module: `github.com/darwin-node/darwin-node`
- CI: `.github/workflows/ci.yml` at **this** root
- Agoda lineage (separate project, read-only comparison):
  https://github.com/agoda-com/macOS-vz-kubelet
  A local clone may exist as `../macOS-vz-kubelet`. That tree is not
  this repo. Do not edit Agoda files when working here.

Commands run from this directory. Do not `cd darwin-node` first.

```
make test-short
make test
make build
make sign
make licenses
make dist
```

Apache-2.0. Lineage and third-party notices: `NOTICE`,
`THIRD_PARTY_NOTICES`, `docs/credits.md`. Files that still follow Agoda
algorithms or formats say `Derived from Agoda macOS-vz-kubelet
(Apache-2.0)`.
