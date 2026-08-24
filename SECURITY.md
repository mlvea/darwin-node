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
  (`APISERVER_CERT_LOCATION` / `APISERVER_KEY_LOCATION`).
- Guest-agent TCP fallback is **off**. vsock is the production path.
  Do not pass `--agent-tcp-fallback` on a bridged network until TLS/PSK
  for that path exists.
- SSH fallback is **off** and never uses password auth.
- `hostPath` is **denied** unless `--allowed-host-paths` is set. `/`
  is never shared.
- Image cache directories are `0700`.

Operational notes: [docs/security.md](docs/security.md).
