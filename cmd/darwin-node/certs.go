package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"strings"

	dnconfig "github.com/mlvea/darwin-node/pkg/config"
	"github.com/mlvea/darwin-node/pkg/nodecert"

	"github.com/virtual-kubelet/virtual-kubelet/node/nodeutil"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// nodeSession is the apiserver client after client-certificate bootstrap.
type nodeSession struct {
	Client kubernetes.Interface
	Stop   func()
	Wait   func(context.Context) error
}

var (
	clientsetFromEnv = func(path string) (kubernetes.Interface, error) {
		return nodeutil.ClientsetFromEnv(path)
	}
	connectNodeClient = func(ctx context.Context, nodeName, certDir string, boot *rest.Config) (*nodeSession, error) {
		nc, err := nodecert.StartNodeClient(ctx, nodeName, certDir, boot)
		if err != nil {
			return nil, err
		}
		return &nodeSession{Client: nc.Client, Stop: nc.Rotation.Stop, Wait: nc.Rotation.Wait}, nil
	}
	connectServing = func(ctx context.Context, cfg dnconfig.Config, internalIP string, client kubernetes.Interface) (dnconfig.CertificateProvider, func(), error) {
		var addrs []corev1.NodeAddress
		if ip := strings.TrimSpace(internalIP); ip != "" {
			addrs = append(addrs, corev1.NodeAddress{Type: corev1.NodeInternalIP, Address: ip})
		}
		if name := strings.TrimSpace(cfg.NodeName); name != "" {
			addrs = append(addrs, corev1.NodeAddress{Type: corev1.NodeHostName, Address: name})
		}
		rot, err := nodecert.StartServing(nodecert.ServingOptions{
			Context:   ctx,
			NodeName:  cfg.NodeName,
			CertDir:   cfg.CertDir,
			Addresses: addrs,
			CertFile:  cfg.CertPath,
			KeyFile:   cfg.KeyPath,
			ClientsetFn: func(*tls.Certificate) (kubernetes.Interface, error) {
				return client, nil
			},
		})
		if err != nil {
			return nil, nil, err
		}
		return rot, rot.Stop, nil
	}
)

// rotationWithStandalone reports that certificate rotation is a cluster
// join. --standalone has no apiserver to sign a CSR.
func rotationWithStandalone(standalone bool, cfg dnconfig.Config) error {
	if standalone && (cfg.RotateServerCertificates || cfg.RotateClientCertificates) {
		return fmt.Errorf("certificate rotation joins the cluster and cannot be used with --standalone")
	}
	return nil
}

// clusterClient is the apiserver client for a cluster join.
// With both rotation flags off it loads cfg.KubeConfig once and does not
// create the cert directory. The stop function is always non-nil.
func clusterClient(ctx context.Context, cfg dnconfig.Config, internalIP string) (kubernetes.Interface, dnconfig.CertificateProvider, func(), error) {
	if !cfg.RotateServerCertificates && !cfg.RotateClientCertificates {
		cs, err := clientsetFromEnv(cfg.KubeConfig)
		if err != nil {
			return nil, nil, func() {}, fmt.Errorf("kube client: %w (set KUBECONFIG or pass --standalone)", err)
		}
		return cs, nil, func() {}, nil
	}
	if err := nodecert.PrepareDir(cfg.CertDir); err != nil {
		return nil, nil, func() {}, err
	}
	var stops []func()
	stop := func() {
		for i := len(stops) - 1; i >= 0; i-- {
			if stops[i] != nil {
				stops[i]()
			}
		}
	}
	var client kubernetes.Interface
	if cfg.RotateClientCertificates {
		if strings.TrimSpace(cfg.BootstrapKubeconfig) == "" {
			return nil, nil, stop, fmt.Errorf("client certificate rotation requires --bootstrap-kubeconfig")
		}
		boot, err := clientcmd.BuildConfigFromFlags("", cfg.BootstrapKubeconfig)
		if err != nil {
			return nil, nil, stop, fmt.Errorf("bootstrap kubeconfig: %w", err)
		}
		nc, err := connectNodeClient(ctx, cfg.NodeName, cfg.CertDir, boot)
		if err != nil {
			stop()
			return nil, nil, func() {}, err
		}
		if nc == nil || nc.Client == nil || nc.Wait == nil {
			if nc != nil && nc.Stop != nil {
				nc.Stop()
			}
			return nil, nil, func() {}, fmt.Errorf("node client session is incomplete")
		}
		stops = append(stops, nc.Stop)
		if err := nc.Wait(ctx); err != nil {
			stop()
			return nil, nil, func() {}, err
		}
		client = nc.Client
	} else {
		var err error
		client, err = clientsetFromEnv(cfg.KubeConfig)
		if err != nil {
			stop()
			return nil, nil, func() {}, fmt.Errorf("kube client: %w (set KUBECONFIG or pass --standalone)", err)
		}
	}
	var serving dnconfig.CertificateProvider
	if cfg.RotateServerCertificates {
		rot, stopServing, err := connectServing(ctx, cfg, internalIP, client)
		if err != nil {
			stop()
			return nil, nil, func() {}, err
		}
		stops = append(stops, stopServing)
		serving = rot
		slog.Info("kubelet serving certificate rotation is on; CSRs for kubernetes.io/kubelet-serving are not approved by darwin-node")
	}
	return client, serving, stop, nil
}
