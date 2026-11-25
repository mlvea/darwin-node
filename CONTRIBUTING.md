# Contributing

Issues and focused pull requests are welcome.

## Workflow

Everything lands through a pull request against `main`. Do not push to
`main`, and do not force-push a shared branch. When `main` moves, update
the branch with a merge commit. CI has to be green on the PR's head
commit (`test` on macOS, `test (linux)`, `kubelet auth envtest`, and
`lint`) before a maintainer merges. A hosted macOS job cancelled with
"not acquired by runner" is a missing runner, not a red test: push an
empty commit or re-run the job so the head is actually green.

This project is **alpha**: much of the code was produced with coding
agents and still needs review and hardware testing. Treat every PR as
an audit, not a rubber stamp, especially `pkg/engine`, `pkg/guest`,
`pkg/runtime/vz`, and anything that touches TLS or images.

## Requirements

- Apple Silicon Mac for Virtualization.framework tests
- Go **1.25+** (see `go.mod`). `make` prepends `$HOME/.local/go/bin` if
  you keep a local toolchain there.

## Commands

```bash
make test-short   # no hardware
make test         # unit tests, including race on Darwin
make vet
make build
make sign         # ad-hoc codesign with Virtualization entitlements
make licenses     # regenerate THIRD_PARTY_NOTICES from go.mod
make dist         # binaries + LICENSE + NOTICE + THIRD_PARTY_NOTICES
```

If you add or bump a Go dependency, run `make licenses` and commit
`THIRD_PARTY_NOTICES`. Do not ship binaries without those three license
files.

Do not commit `bin/`, `dist/`, `*.img`, `*.ipsw`, certs, or `debug-snapshot.json`.

## Tests

Prefer tests that call shipped functions (engine, guest protocol, image
pack/verify, capacity). The fake runtime (`--runtime=fake`) covers
lifecycle without a VM. Hardware boot, IPSW restore, and live ORAS
pulls of multi-GB images are not required for unit PRs.

## Docs

User-facing behavior lives in `README.md` and `docs/`. Architecture
decisions live in `docs/adr/`. Known limits in `docs/limitations.md`.
Keep those three in agreement when you change a default.
