# Security

Treat `darwin-node` like a kubelet: it is privileged, it creates VMs,
and it serves exec/logs.

This tree is **alpha**. The implementation is largely agent-written and
still needs review and hardware soak. Assume bugs, including in
fail-closed paths. Prefer `--runtime=fake` and isolated hosts until
that work lands. The software is provided AS IS, without warranty;
see [LICENSE](LICENSE).

## Report a vulnerability

Please **do not** open a public issue for exploitable bugs. Email the
maintainers listed in the repository, or open a private security
advisory if this project is on GitHub.

## Defaults that matter

- Cluster mode **will not start** without a kubelet TLS cert/key
  (`APISERVER_CERT_LOCATION` / `APISERVER_KEY_LOCATION`) and a client CA
  (`APISERVER_CA_CERT_LOCATION`). The kubelet HTTP server defaults to
  `127.0.0.1:10250`.
- Authenticate the process as `system:node:<nodename>` in group
  `system:nodes` so the Node authorizer and NodeRestriction apply.
  The chart default does not grant cluster-wide Secret, ConfigMap, or
  pod delete. See [docs/security.md](docs/security.md).
- The guest agent is reached only over vsock. The plaintext TCP
  fallback is gone: `--agent-tcp-fallback`, `--listen`, and
  `DARWIN_NODE_AGENT_TCP_FALLBACK` do nothing. The pod token is not
  sent on a TCP socket. Over the shared NAT the other VM could read
  or stall that connection.
- Treat the guest as hostile root. Workload exit codes are clamped to
  0..255 before they are stored (a guest can still report 0). Guest
  text written into PodStatus or events is truncated to 1024 printable
  UTF-8 characters. Control frames are capped at 64 KiB; a larger
  frame, or JSON nested deeper than 32, closes the session. Stream
  and materialize frames keep the 16 MiB cap.
- Guest-reported CPU and memory are untrusted telemetry for
  `kubectl top`. They are not a scheduling input. An agent that does
  not advertise the `metrics` capability is not asked, and the metrics
  API falls back to the VM's configured size.
- SSH fallback is **off** and never uses password auth.
- `hostPath` is **denied** unless `--allowed-host-paths` is set. `/`
  is never shared.
- Image cache directories are `0700`.

Operational notes: [docs/security.md](docs/security.md).
