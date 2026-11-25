# Security notes

## Host agent

- Runs privileged enough to create VMs. Treat the binary like a kubelet.
- Entitlements: `com.apple.security.virtualization` (required),
  `com.apple.vm.networking` (bridged only), network client/server.
- Codesign every build you run. Unsigned VZ processes fail at VM start.
- Cluster mode will not start a plaintext kubelet HTTP server. Set
  `APISERVER_CERT_LOCATION` and `APISERVER_KEY_LOCATION` (server cert/key).
  `--standalone` does not serve exec/logs and does not need those files.
  The process re-reads that pair on each handshake when the file mtime
  or size changes. A replacement that does not parse is ignored and the
  last good pair stays in use. Replacing the files does not require a
  restart. There is no CSR bootstrap.
- The kubeconfig client credential is loaded once. Rotating it is a
  process restart. See [kubeconfig-rotation.md](kubeconfig-rotation.md).
- Cluster mode will not start without `APISERVER_CA_CERT_LOCATION`.
  That file is the client-certificate CA. The kubelet HTTP server verifies
  any client certificate that is presented (`tls.VerifyClientCertIfGiven`)
  and wraps its handler with TokenReview + SubjectAccessReview
  (`nodeutil.WebhookAuth`). A connection with no client certificate still
  completes the TLS handshake, so a bearer token can be reviewed.
  Anonymous callers receive 401 from the handler. There is no separate
  `--authentication-token-webhook` flag; the client CA is what turns
  webhook auth on. There is no flag that installs anonymous auth.
  `--standalone` is the path that does not serve this API.
- The process watches that file. Replacing it (atomic rename or
  rewrite) updates the handshake's client CA pool and the webhook
  authenticator's verify options without a restart. An empty or
  unparsable replacement is ignored and the last good bundle stays
  loaded. A missing or unparsable file at startup still refuses to
  boot; it does not install `NoAuth`.
- A certificate signed by that CA authenticates as its subject CommonName.
  When a verified client certificate is present, the handler uses that
  identity and does not also consult a bearer token. Bearer TokenReview
  runs for requests that present no client certificate.
- The node identity must be allowed to `create` `tokenreviews`
  (`authentication.k8s.io`) and `subjectaccessreviews`
  (`authorization.k8s.io`). The Node authorizer grants those creates
  to `system:nodes`. The chart's ServiceAccount ClusterRole grants them
  only when `rbac.identity=serviceaccount`. Callers need their own RBAC
  on the node subresource they use (`nodes/log`, `nodes/proxy`,
  `nodes/stats`, or `nodes/metrics`) for this node's name.
- `--listen-address` defaults to `127.0.0.1` (port `KUBELET_PORT`, default
  `10250`). `kubectl logs` and `kubectl exec` from a remote API server
  need this set to the address that server dials, which is the node's
  InternalIP. `0.0.0.0` or an empty value binds every interface. Use that
  only when every interface on the Mac is trusted. `DARWIN_NODE_LISTEN_ADDRESS`
  overrides the flag when it is set.

## API-server auth test

`make test-kubelet-auth` (Linux, `//go:build integration`) downloads
controller-runtime envtest binaries (kube-apiserver and etcd, Kubernetes
1.35) and calls the same `kubeletAuth` handler `runVK` installs, over the
same live client-CA bundle the handshake uses. The kubelet client in that
test is a ServiceAccount allowed only to create TokenReview and
SubjectAccessReview.

Verified when `ClientCA` is set:

- A client certificate signed by that CA is authenticated, and
  SubjectAccessReview allows `get` on `nodes/log` for this node's name.
- A bearer token for a real ServiceAccount is authenticated via
  TokenReview, and SubjectAccessReview allows `get` on `nodes/proxy`
  for this node's name.
- SubjectAccessReview denies that certificate on `nodes/proxy`, denies a
  certificate bound to a different node name, denies a ServiceAccount
  with no binding, and denies the proxy ServiceAccount on `nodes/log`.
- Anonymous requests are rejected with 401. An invalid bearer token is
  rejected with 401. A client certificate signed by a different CA is
  rejected during the TLS handshake.
- A missing or invalid client CA fails closed. Loading the file returns
  an error and does not install `NoAuth`.
- Unit tests replace the CA file and accept a certificate from the new
  bundle, and they keep the previous bundle when the replacement is
  empty or not a certificate.

`go test ./...` does not run this test. Without `KUBEBUILDER_ASSETS` or
`DARWIN_NODE_ENVTEST_DOWNLOAD=1`, `go test -tags=integration` runs the
fail-closed CA cases and skips the live API server. The Linux CI job
`kubelet auth envtest` sets the download flag and fails if the binaries
cannot be fetched.

Not verified here: `make test-hardware`, and a 24 hour `make soak IMAGE=...` result. The short fake-runtime soak is `make soak`.
A nil client-CA bundle still installs `nodeutil.NoAuth` inside
`kubeletAuth`. Cluster startup never passes that nil: `RequireKubeletClientCA`
returns first. The unit tests in `cmd/darwin-node/kubelet_auth_test.go`
are the callers that pass a nil bundle.

## Node identity

The process should authenticate to the API server as a node, not as a
ServiceAccount. The username is `system:node:<nodename>` and the group is
`system:nodes`. `<nodename>` is the `--nodename` flag. A client certificate
uses that name as the subject CommonName and `system:nodes` as the
Organization. The API server's Node authorizer and NodeRestriction
admission plugin are on by default. Together they allow this process to:

- create its Node and patch that Node's status
- list and watch pods with `spec.nodeName` equal to this node
- update status of pods bound to this node, and delete those pods
- get Secrets and ConfigMaps referenced by those pods
- create `serviceaccounts/token` for the service account of those pods
  (the projected token Kubernetes adds to pods by default)
- create and update its lease in `kube-node-lease`
- create TokenReview and SubjectAccessReview objects for the kubelet
  webhook authenticator

NodeRestriction does not apply to a ServiceAccount. The chart's default
`rbac.identity` is `node`, and that mode renders no Role and no
ClusterRoleBinding.

`rbac.identity=serviceaccount` is the fallback when a node client
certificate cannot be issued. It binds the chart ServiceAccount.
`rbac.namespaces` must list every namespace whose pods this Mac may run.
Secret get, ConfigMap get, pod delete, pod status update, and
`serviceaccounts/token` create are a Role in each of those namespaces.
They are not cluster-wide. Pod list/watch stays on the ClusterRole because
virtual-kubelet's informer lists at cluster scope with a `spec.nodeName`
field selector, which a Role cannot authorize. The lease Role is only in
`kube-node-lease`, with get, create, update, and patch.

Virtual-kubelet also starts cluster-wide Secret and ConfigMap informers.
Neither chart identity grants that list, and the Node authorizer denies
it. The informer cache stays empty. `env` `secretKeyRef`,
`configMapKeyRef`, and `envFrom` are resolved from that cache, so those
references do not resolve. Put those values in a volume. Volume secrets,
image pull secrets, and projected service account tokens are read by name
(`KubeResolver`). The node identity may get objects a pod on this node
references, and may create that pod's service account token. The
ServiceAccount identity may get Secrets and ConfigMaps, and may create
`serviceaccounts/token`, in `rbac.namespaces` only.

## Guest agent

- launchd, root inside the *guest*, not on the host. The host treats
  that process as hostile: it can send any JSON it wants.
- vsock is the only agent transport. It is not routed and not visible
  to other VMs (each VM's vsock is isolated; Apple sets guest CID
  independently). There is no TCP listener and no TCP dial. The pod
  token stays in the 0600 file on the control share and is sent only
  on the vsock handshake. It is generated at CreatePod and never logged.
  A pod that adopts a warm VM gets a new token, written with a temp file
  and rename, before the pod dials. The warm-boot token is not reused.
  The supplied guest watches that file about every 2s. The fake runtime
  used in tests is told the new token directly. A dial retry covers the
  watch interval on a real guest.
- Control frames are capped at 64 KiB. Stream and materialize frames
  may be up to 16 MiB. A frame over its cap, or JSON nested deeper
  than 32, closes the session. Unknown JSON fields are ignored.
- Handshake carries the host's protocol range and capability list.
  The guest replies with the selected protocol, its version, and its
  own list. The session uses the intersection. An older agent that
  omits the list keeps working; the intersection is empty and optional
  features stay off.
- Workload exit codes are stored in 0..255. Below 0 becomes 0, above
  255 becomes 255. A guest can already report success. Guest text
  copied into PodStatus or an event is limited to 1024 printable
  UTF-8 characters.
- CPU and memory figures from the guest are untrusted telemetry for
  `kubectl top`. Scheduling uses the host probe and the pod's
  requests, not these numbers. An agent without the `metrics`
  capability does not supply them; the metrics API then reports the
  configured VM size.
- `kubectl exec` returns the command's exit code. A non-zero status
  is that code, not a transport error.
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
- Docker binds of ConfigMap, Secret, projected, and downwardAPI volumes are
  read-only, including when the pod's volumeMount is not. emptyDir and
  hostPath still follow the mount's readOnly flag.
- Each rewrite writes the bytes with `O_NOFOLLOW` into a new directory and
  publishes them by renaming a `..data` symlink, the same shape as kubelet's
  AtomicWriter. A symlink or hard link left at the visible name is replaced,
  not opened. `..` names are reserved for that directory.
- Copied into the guest by the agent (`mode=copy`) so the automount folder
  is not the long-term home of secret bytes. The copy skips `..` names and
  follows the visible symlinks, so the guest gets regular files.
- Deleted recursively on pod teardown *after* `machine.Stop` returns, so
  the guest cannot write through virtio-fs while backing files are removed.
- Crash recovery (`Engine.Recover`, run from `engine.New`) deletes
  `cache/pods/<uid>` directories that are not in the slot table, including
  ones only a minute old. A crash kills the VMs, so those directories are
  secrets and overlays, not live machines, and they are not rebuilt into
  the slot table. The pods are not started again from the deleted disks:
  `Never` is marked `Failed` (`NodeRestarted`); `Always` and `OnFailure`
  follow restart semantics. Drain the node before each upgrade. `cache/warm` leftovers from the previous process are
  removed at the same time. A directory whose uid already holds a slot is
  left alone. The committed cache store is not rewritten from the orphan.
  APFS snapshots taken while secret files still exist on disk may retain
  those bytes until the snapshot is dropped.

## Images

- Cluster mode rejects a host directory as the pod image. A cache path or
  another pod's overlay cannot be named as `image`. `--standalone` still
  boots a local bake. A symlink in an image directory (the directory
  itself, `disk.img`, or `aux.img`) is rejected, and clonefile does not
  follow a symlink.
- Pull authenticates with dockerconfigjson from the pod's pull secrets.
  The entry is chosen by registry host, using the kubelet docker-config
  rules (host and port, path prefix, `*` in a host label, `/v1/` and
  `/v2/` meaning the host, `index.docker.io/v1/` for Docker Hub). The
  `auth` field (`base64(username:password)`) is decoded. A secret for one
  registry is not sent to another, including a delta image's base.
- A cached image is used only after the registry accepts this pod's
  credentials. Anonymous success is a public image. `imagePullPolicy:
  Always` pulls again. `IfNotPresent` and `Never` still do the manifest
  check, so a registry that is down refuses the cache.
- Adopting a warm VM for a private image uses that same check, with the
  pod's own pull credentials. A committed pull cache that this pod cannot
  authorize is not adopted; the pod cold-boots. A warm image with no
  committed cache (a local bake the fake runtime never pulled) is not
  sent to the registry. An authorization failure does not fail the pod
  by itself.
- The cache directory name is the sha256 of the full reference. Older
  names (`:` rewritten to `--`, `/` to `_`) are deleted on startup and
  are not renamed onto the new key. The next start pulls again.
- Uncompressed sha256 is verified before a VM is started from cache.
- We do not run untrusted host code from the image; we boot it as a VM.

## Projected service account tokens

- Each `serviceAccountToken` projection is minted with its own audience
  and expiration. Two paths do not share one token.
- `TokenRequest` errors and empty tokens fail pod admission. The node
  does not create the token file in that case.
- While the pod is running, the watch loop refreshes at half the
  remaining lifetime, republishes the host file (mode 0600) through the
  same atomic write, and asks the guest agent to copy it again.
- A failed refresh leaves the previous file in place. Once the recorded
  expiration has passed, the pod fails and its VM slot is released.

## emptyDir size

`sizeLimit` is the sum of regular-file sizes under the host directory,
checked on the probe interval. The walk does not follow symlinks. An
absolute symlink is a guest path and is not an error. A dangling symlink
is not an error. A file that disappears during the walk is not an error.
A relative symlink that lexically leaves the directory fails the pod, and
the pod message does not include the host cache path. Usage over the limit
fails the pod. This is a host-side check between ticks, not an APFS quota.

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
