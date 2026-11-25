# Code layout

The packages below are split by concern so two changes can land without
editing the same file. Put a new concern in a new file in the same package.
Do not append it to `pkg/engine/engine.go`, `pkg/runtime/fake/fake.go`,
`pkg/config/config.go`, or `cmd/darwin-node/main.go`.

Those four files stay the entry points: engine state and `New`, the fake
runtime facade, configuration defaults, and process entry plus flag
registration. Exported names stay in the same package when a function moves.

## pkg/engine

| Concern | File |
| --- | --- |
| Pod record, constructor, slot and token hooks | `engine.go` |
| Create, cold start, warm adoption, MAC and agent token | `create.go` |
| Guest connect, fail, and succeed | `connect.go` |
| Close, drain, delete, teardown | `teardown.go` |
| Pod status and conditions | `pod_status.go` |
| Label and annotation updates | `update.go` |
| Exec, logs, SSH fallback | `exec.go` |
| Serial console socket | `console.go` |
| Image resolve and pull policy | `image.go` |
| Init containers | `init_container.go` |
| VM restart after a probe failure | `restart.go` |
| Debug snapshot and pod metrics | `debug.go` |

Host-port reservation and the proxy update already live in `probes.go`.
Disk recovery after a process restart lives in `recover.go`. Pods left on
disk across that restart live in `interrupted.go`. Extend those concerns in
a new file.

## pkg/runtime/fake

| Concern | File |
| --- | --- |
| Runtime facade, test hooks, `New` | `fake.go` |
| In-process machine | `machine.go` |

## pkg/config

| Concern | File |
| --- | --- |
| `Config`, defaults, listen address, graphics | `config.go` |
| Environment parsing (`MergeEnv`, `ApplyEnv`) | `env.go` |
| `Validate` | `validate.go` |

TLS and the kubelet client CA already have their own files (`tls.go`,
`clientca.go`).

## cmd/darwin-node

| Concern | File |
| --- | --- |
| `main` and flag registration | `main.go` |
| Process wiring and the virtual-kubelet run | `run.go` |
| `console` command | `console_cmd.go` |
| `debug-dump` command | `debug_cmd.go` |

Flags are registered in `buildCommand` in `main.go`. Add a new flag area as
a function in a new file and call it from `buildCommand`. Kubelet auth and
shutdown already have their own files.
