# Security notes

## Host agent

- Runs privileged enough to create VMs. Treat the binary like a kubelet.
- Entitlements: `com.apple.security.virtualization` (required),
  `com.apple.vm.networking` (bridged only), network client/server.
- Codesign every build you run. Unsigned VZ processes fail at VM start.
- Cluster mode will not start a plaintext kubelet HTTP server. Set
  `APISERVER_CERT_LOCATION` and `APISERVER_KEY_LOCATION` (server cert/key).
  `--standalone` does not serve exec/logs and does not need those files.
- Set `APISERVER_CA_CERT_LOCATION` so the kubelet HTTP server requires and
  verifies client certificates (`tls.RequireAndVerifyClientCert`). Without a
  client CA, the socket is TLS-encrypted but callers are anonymous.
- Bind with `--listen-address` (default: all interfaces) so the kubelet HTTP
  port is not exposed on untrusted networks.
- There is no `--authentication-token-webhook` flag. TokenReview /
  SubjectAccessReview webhook auth is not implemented; production authn is
  mTLS via the client CA.
- Do not disable client verification except on a laptop.

## Guest agent

- launchd, root inside the *guest*, not on the host.
- vsock is the preferred channel: not routed, not visible to other VMs
  (each VM's vsock is isolated; Apple sets guest CID independently).
- TCP fallback: per-pod 32-byte token in a 0600 file on the control share.
  The token is generated on the host at CreatePod and never logged.
- Exec argv comes from the Kubernetes API (kubectl exec / probes). Same
  trust model as kubelet. There is no extra in-guest authorization.

## hostPath

- Default deny. Empty `--allowed-host-paths` /
  `DARWIN_NODE_ALLOWED_HOST_PATHS` (comma-separated prefixes) rejects every
  `hostPath` volume at admission (`ValidatePod`) and again in
  `volume.Materialize`.
- Allowed prefixes are compared after `filepath.Abs` and `EvalSymlinks`. A
  path must equal a prefix or be a child (`prefix/` ...). Sibling names such
  as `/Volumes/Workevil` do not match `/Volumes/Work`.
- The host root `/` is never shared into a guest, even if `/` is on the
  allowlist.
- `HostPathType` is enforced: Directory, File, Socket, CharDevice,
  BlockDevice, plus DirectoryOrCreate / FileOrCreate (those two may create).

## Secrets and ConfigMaps

- Materialized on the host under `cache/pods/<uid>/volumes/<name>` with
  0700 directories, 0600 secret files.
- Copied into the guest by the agent (`mode=copy`) so the automount folder
  is not the long-term home of secret bytes.
- Deleted recursively on pod teardown *after* `machine.Stop` returns, so
  the guest cannot write through virtio-fs while backing files are removed.
- Crash recovery (`Engine.Recover` / `RecoverCache`, run from `engine.New`)
  sweeps `cache/pods/<uid>` directories older than 24h that are not in the
  slot table. APFS snapshots taken while secret files still exist on disk
  may retain those bytes until the snapshot is dropped.

## Images

- Pull authenticates with dockerconfigjson from the pod's pull secrets.
- Uncompressed sha256 is verified before a VM is started from cache.
- We do not run untrusted host code from the image; we boot it as a VM.

## SSH fallback

- Off by default. Opt in with `DARWIN_NODE_ENABLE_SSH_FALLBACK=true` and
  `DARWIN_NODE_SSH_USER`. Without both, exec returns `guest agent not connected`.
- Key-based auth only (`DARWIN_NODE_SSH_PRIVATE_KEY_PATH` or
  `DARWIN_NODE_SSH_PRIVATE_KEY_BASE64`). Password authentication has been
  removed: `VZ_SSH_PASSWORD` / `DARWIN_NODE_SSH_PASSWORD` fail config
  validation (`password auth removed`).
- Fail-closed host-key verification: set `DARWIN_NODE_SSH_KNOWN_HOSTS` or pin
  `DARWIN_NODE_SSH_HOST_KEY` (OpenSSH authorized_keys line). Missing both
  refuses the connection and does not dial. Image `config.json` may record
  `guestSSHHostKey` for operators to pin.
- Exec argv is strictly single-quote escaped so `;`, `$()`, backticks, and
  quotes stay literal arguments rather than a remote shell.
- Prefer baking the guest agent into images. SSH is last-resort exec only.

## What we will not do

- Accept SSH password authentication (including the former Agoda
  `VZ_SSH_PASSWORD=master` plist pattern).
- Catch and retry `VirtualMachineLimitExceeded` (would hide a quota bug).
- Mount `/` of the host into a guest.
