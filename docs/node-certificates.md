# Node certificates

Certificate rotation is off unless a flag asks for it. With both flags
off, darwin-node loads `--kubeconfig` once and serves the kubelet
certificate from `APISERVER_CERT_LOCATION` and `APISERVER_KEY_LOCATION`.
That path is unchanged. See `docs/kubeconfig-rotation.md` for the
file-reload behaviour.

Rotation uses `k8s.io/client-go/util/certificate`. darwin-node does not
import `k8s.io/kubernetes`. It never signs or approves a
CertificateSigningRequest. An approver outside this process has to do
that.

## Flags

| Flag | Environment | Default |
| --- | --- | --- |
| `--rotate-server-certificates` | `DARWIN_NODE_ROTATE_SERVER_CERTIFICATES` | false |
| `--rotate-client-certificates` | `DARWIN_NODE_ROTATE_CLIENT_CERTIFICATES` | false |
| `--bootstrap-kubeconfig` | `DARWIN_NODE_BOOTSTRAP_KUBECONFIG` | empty |
| `--cert-dir` | `DARWIN_NODE_CERT_DIR` | `~/Library/Application Support/io.darwin-node.node/pki` |

Boolean variables accept `true`, `1`, `false`, and `0`. An explicit flag
wins over the environment. Empty variables leave the current value.
`--rotate-client-certificates` requires `--bootstrap-kubeconfig`. Either
rotation flag requires a cert directory. Neither flag can be combined
with `--standalone`.

The cert directory is created only when a rotation flag is on.

## Serving certificates

`--rotate-server-certificates` starts a certificate manager with signer
`kubernetes.io/kubelet-serving`. The subject is `CN=system:node:<nodename>`,
`O=system:nodes`. The node hostname is a DNS SAN and the host
internal IP is an IP SAN.

The manager writes `kubelet-server-current.pem` in `--cert-dir`. That
name is a symlink to a timestamped PEM that holds the certificate and
the private key, the same layout kubelet uses. The kubelet HTTPS
listener's `GetCertificate` returns the manager's current certificate.
When the apiserver is unreachable at expiry, the manager hides the
expired certificate from `Current` and the listener keeps presenting the
last pair still on disk.

Startup does not wait for the serving CSR to be signed. Until a
certificate exists, handshakes fail. A static pair in
`APISERVER_CERT_LOCATION` / `APISERVER_KEY_LOCATION` is used as the seed
when those files are present. darwin-node does not approve the serving
CSR. The API server also needs a signer or an approver for
`kubernetes.io/kubelet-serving`, and the node identity needs permission
to create that CSR. The examples under `deploy/csr/` do not include
that permission.

## Client certificates

`--rotate-client-certificates` with `--bootstrap-kubeconfig` is the
kubelet bootstrap. The first CSR uses the bootstrap kubeconfig and signer
`kubernetes.io/kube-apiserver-client-kubelet`. The issued certificate is
`CN=system:node:<nodename>`, `O=system:nodes`, with client auth. The
node then calls the API as that user. NodeRestriction and the Node
authorizer can apply. A ServiceAccount kubeconfig cannot.

The process waits until the client certificate exists before it joins,
so the node is not registered with the bootstrap identity. The bindings
under `deploy/csr/` allow the CSR to be created. They do not approve it.
The first client CSR needs an approver. After the node object exists,
kube-controller-manager's CSR approving controller can approve a renewal
whose username is `system:node:<nodename>`. Later
renewals swap the HTTP transport. Requests that have already been sent,
including pod status updates, stay on the transport that started them.
New requests present the new certificate. client-go's cert-rotating
dialer closes live connections when the certificate changes; darwin-node
does not use that dialer for this client.

A denied or failed CSR leaves the previous certificate in place. The
manager backs off (about two seconds, doubling, then a 32 second poll)
and tries again. If the apiserver is down when the certificate reaches
the end of its lifetime, the last certificate is still presented. The
manager's `Current` method hides it once `NotAfter` has passed;
connections keep the retained pair until a new one is signed.

Renewal runs at roughly 70 to 90 percent of the certificate lifetime.

## Files

The cert directory is mode `0700`. Private keys are mode `0600`. A
directory that is already group or world writable is refused and left
as it is. The directory's final path component is opened with
`O_NOFOLLOW`, so a symlink in that position is refused. A symlink inside
the directory must point at a path that stays inside it.
`kubelet-client-current.pem` and `kubelet-server-current.pem` are
symlinks to sibling PEMs.

Parent directories are not walked. On macOS, `/var` and `/tmp` are
symlinks, and refusing every ancestor would reject a normal temp
directory. The operator is responsible for the parents.

## RBAC

`deploy/csr/clusterrolebindings.yaml` binds:

- `system:certificates.k8s.io:certificatesigningrequests:nodeclient` to
  `system:bootstrappers`, for the bootstrap credential
- `system:certificates.k8s.io:certificatesigningrequests:selfnodeclient`
  to `system:nodes`, for renewals as `system:node:<nodename>`

`deploy/csr/chart/` is the same bindings as a Helm chart. Values
`bootstrapGroup` and `nodeGroup` default to those groups. The chart does
not grant an approve verb and does not bind a serving-certificate role.
