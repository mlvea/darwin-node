// Package nodecert bootstraps and rotates the node's Kubernetes certificates
// with CertificateSigningRequests.
//
// Rotation is opt-in. With the flags off, darwin-node keeps loading its
// kubeconfig once and reloading the kubelet serving files on change.
//
// Serving certificates use signer kubernetes.io/kubelet-serving, subject
// CN=system:node:<nodename> and O=system:nodes, and SANs taken from the
// node's addresses. Client certificates use signer
// kubernetes.io/kube-apiserver-client-kubelet and the same subject, which
// is the identity the Node authorizer and NodeRestriction apply to.
//
// darwin-node never approves or signs a CSR. The bindings under deploy/csr
// only let the bootstrap identity and system:nodes create client CSRs.
// An approver outside this process has to approve them. The apiserver's
// built-in signer can then sign an approved client CSR. Serving CSRs also
// need permission to create kubernetes.io/kubelet-serving, which those
// bindings do not grant.
//
// The implementation uses k8s.io/client-go/util/certificate. It does not
// import k8s.io/kubernetes.
package nodecert
