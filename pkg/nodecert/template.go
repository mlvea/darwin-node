package nodecert

import (
	"crypto/x509"
	"crypto/x509/pkix"
	"net"

	corev1 "k8s.io/api/core/v1"
)

const (
	// ServerPairPrefix is the kubelet serving file prefix. The current
	// pair is <cert-dir>/kubelet-server-current.pem.
	ServerPairPrefix = "kubelet-server"
	// ClientPairPrefix is the kubelet client file prefix. The current
	// pair is <cert-dir>/kubelet-client-current.pem.
	ClientPairPrefix = "kubelet-client"

	nodeUserPrefix = "system:node:"
	nodeGroup      = "system:nodes"
)

// ServerTemplate is the CSR subject and SANs for the kubelet serving
// certificate. Hostname addresses become DNS SANs. Internal and external
// IPs become IP SANs. Empty and unparsable addresses are skipped.
func ServerTemplate(nodeName string, addrs []corev1.NodeAddress) *x509.CertificateRequest {
	req := nodeSubject(nodeName)
	for _, addr := range addrs {
		switch addr.Type {
		case corev1.NodeHostName:
			if addr.Address != "" {
				req.DNSNames = append(req.DNSNames, addr.Address)
			}
		case corev1.NodeInternalIP, corev1.NodeExternalIP:
			if ip := net.ParseIP(addr.Address); ip != nil {
				req.IPAddresses = append(req.IPAddresses, ip)
			}
		}
	}
	return req
}

// ClientTemplate is the CSR subject for the node client certificate.
// Client certificates do not carry serving SANs.
func ClientTemplate(nodeName string) *x509.CertificateRequest {
	return nodeSubject(nodeName)
}

func nodeSubject(nodeName string) *x509.CertificateRequest {
	return &x509.CertificateRequest{
		Subject: pkix.Name{
			CommonName:   nodeUserPrefix + nodeName,
			Organization: []string{nodeGroup},
		},
	}
}
