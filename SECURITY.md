# Security

Treat `darwin-node` like a kubelet: it is privileged, it creates VMs,
and it serves exec/logs.

This tree is **alpha**. The implementation is largely agent-written and
still needs review and hardware soak. Assume bugs, including in
fail-closed paths. Prefer `--runtime=fake` and isolated hosts until
that work lands. The software is provided AS IS, without warranty;
see [LICENSE](LICENSE).

## Report a vulnerability

Please **do not** open a public issue for exploitable bugs. Report them
through GitHub private vulnerability reporting, which is the only
channel: on https://github.com/mlvea/darwin-node open the **Security**
tab and choose **Report a vulnerability**.

## Defaults that matter

- Cluster mode **will not start** without a kubelet TLS cert/key
  (`APISERVER_CERT_LOCATION` / `APISERVER_KEY_LOCATION`) and a client CA
  (`APISERVER_CA_CERT_LOCATION`). The kubelet HTTP server defaults to
  `127.0.0.1:10250`.
- Authenticate the process as `system:node:<nodename>` in group
  `system:nodes` so the Node authorizer and NodeRestriction apply.
  The chart default does not grant cluster-wide Secret, ConfigMap, or
  pod delete. See [docs/security.md](docs/security.md).
- `--rotate-client-certificates` (default off, with
  `DARWIN_NODE_ROTATE_CLIENT_CERTIFICATES`) is the opt-in that makes
  the process `system:node:<nodename>` in `system:nodes`, so
  NodeRestriction and the Node authorizer can apply. The ServiceAccount
  kubeconfig remains the default: with the rotation flags off, the
  process still loads `--kubeconfig` once.
  `--rotate-server-certificates`, `--bootstrap-kubeconfig`, and
  `--cert-dir` are the other opt-in knobs
  (`DARWIN_NODE_ROTATE_SERVER_CERTIFICATES`,
  `DARWIN_NODE_BOOTSTRAP_KUBECONFIG`, `DARWIN_NODE_CERT_DIR`).
  darwin-node does not approve CertificateSigningRequests. An approver
  outside the process has to sign them. See
  [docs/node-certificates.md](docs/node-certificates.md).
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
  is never shared. A symlink is resolved again when the path is handed
  to Docker, and a target outside the allowlist is rejected.
- Sidecar probes dial the container's own address. An empty host is not
  the node's loopback, and probe events do not include response bodies.
- Sidecars run on a per-pod Docker network with `no-new-privileges`.
  `allowPrivilegeEscalation: true`, `runAsNonRoot` without an explicit
  non-zero `runAsUser`, and `docker.sock` are rejected on that path
  before a VM slot is taken. `CreatePod` rejects `privileged`, added
  capabilities, `hostNetwork`, `hostPID`, `hostIPC`, a `seccompProfile`
  other than unset or `RuntimeDefault`, `appArmorProfile`,
  `seLinuxOptions`, and `procMount: Unmasked` as `Failed` with reason
  `UnsupportedPodField` and a Warning event, before a slot is taken.
  `volumeDevices` and volume sources other than `emptyDir`, `secret`,
  `configMap`, `downwardAPI`, allowlisted `hostPath`, and `projected`
  (`serviceAccountToken`, `secret`, `configMap`, `downwardAPI`) are
  `UnsupportedVolume`. `restartPolicy` `Never`, `OnFailure`, and
  `Always` are admitted. `Always` restarts the guest process inside the
  same VM and still uses one slot. `UpdatePod` does not run the check
  again. Legacy seccomp and AppArmor annotations are not read. See
  [docs/admission.md](docs/admission.md). The optional cluster policy
  is `deploy/admission/validatingadmissionpolicy.yaml`
  (`failurePolicy: Fail`), applied on its own or by Helm when
  `admission.podProfilePolicy.enabled` is true. It has not been applied
  to a live API server.
- Image cache directories are `0700`.
- Explicit flags win over `DARWIN_NODE_*` environment variables. The
  stock launchd plist sets `--listen-address`. That flag is the kubelet
  bind address. `DARWIN_NODE_LISTEN_ADDRESS` applies only when the flag
  is omitted. A value the process cannot parse is an error at startup,
  before `--help`.
- Named caches are read-only unless `--cache-writers`
  (`DARWIN_NODE_CACHE_WRITERS`, default empty) names the pod's namespace
  or `namespace/serviceaccount` and the pod sets
  `cache-write.darwin.node/<name>: "true"`. The store is replaced only
  after exit 0, by cloning to a temporary directory and renaming it
  into place. A write annotation outside the list emits `CacheReadOnly`
  and the share stays read-only. A graceful delete of a pod that is
  still running does not replace the cache. See
  [docs/cache-volumes.md](docs/cache-volumes.md). Confirming that a
  booted guest cannot write through the read-only virtio-fs mount is
  hardware-pending.
- `kubectl attach` returns an error and does not start a shell. Use
  `kubectl exec` or `kubectl logs -f`.
- hostPort listeners and spliced connections close when the pod is
  deleted or fails. There is no flag for that lifecycle.
- A warm VM adopted by a pod gets a fresh agent token. The pod's own
  pull credentials are checked again before that adoption when a
  committed image cache exists. See [docs/security.md](docs/security.md).
- `--vm-supervisor` defaults to `off`. With it on, the run directory
  (mode `0700`) and a same-uid peer check on the slot socket are the
  local trust boundary. The supervisor does not parse the guest
  protocol. The adapter calls `supervisor.Adopt` on first attach and
  on `Recover`. Engine startup calls that `Recover`: a live supervisor
  matching `cache/pods/<uid>` is reattached as `Running` (slot held,
  disk and agent token kept, guest redialed, no new VM). A dead
  supervisor keeps the node-restart settlement. A supervisor lost at
  runtime is reason and message `supervisor lost`, with no replacement
  boot. Kubelet TLS and the guest handshake are unchanged while the
  flag is off. See [docs/vm-supervisor.md](docs/vm-supervisor.md) and
  [docs/vm-supervisor-recovery.md](docs/vm-supervisor-recovery.md).
- The node always publishes `darwin.node/os=macos:NoSchedule`, including
  with `--disable-taint`. The label stays `kubernetes.io/os=darwin`.
  Container 0 is admitted by repository name. A custom name needs
  `darwin.node/disk-image: "true"`. The pull still requires a darwin-node
  or Agoda disk layer, or a layer titled `disk.img`. See
  [docs/admission.md](docs/admission.md).

Operational notes: [docs/security.md](docs/security.md).
