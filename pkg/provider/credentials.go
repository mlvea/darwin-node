package provider

import (
	"context"
	"fmt"

	"github.com/darwin-node/darwin-node/pkg/engine"
	"github.com/darwin-node/darwin-node/pkg/image"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
)

// CredentialResolver loads Secrets, ConfigMaps, SA tokens, and pull creds for a pod.
type CredentialResolver interface {
	Resolve(ctx context.Context, pod *corev1.Pod) (engine.Credentials, error)
}

// ResolverFunc adapts a function to CredentialResolver.
type ResolverFunc func(ctx context.Context, pod *corev1.Pod) (engine.Credentials, error)

func (f ResolverFunc) Resolve(ctx context.Context, pod *corev1.Pod) (engine.Credentials, error) {
	return f(ctx, pod)
}

// KubeResolver uses the Kubernetes API.
type KubeResolver struct {
	Client kubernetes.Interface
}

func (r KubeResolver) Resolve(ctx context.Context, pod *corev1.Pod) (engine.Credentials, error) {
	if r.Client == nil || pod == nil {
		return engine.Credentials{}, nil
	}
	creds := engine.Credentials{
		ConfigMaps: map[string]*corev1.ConfigMap{},
		Secrets:    map[string]*corev1.Secret{},
	}
	ns := pod.Namespace
	core := r.Client.CoreV1()

	addCM := func(name string, optional bool) error {
		if name == "" || creds.ConfigMaps[name] != nil {
			return nil
		}
		cm, err := core.ConfigMaps(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if optional {
				return nil
			}
			return fmt.Errorf("configmap %s: %w", name, err)
		}
		creds.ConfigMaps[name] = cm
		return nil
	}
	addSec := func(name string, optional bool) error {
		if name == "" || creds.Secrets[name] != nil {
			return nil
		}
		sec, err := core.Secrets(ns).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			if optional {
				return nil
			}
			return fmt.Errorf("secret %s: %w", name, err)
		}
		creds.Secrets[name] = sec
		return nil
	}

	for _, v := range pod.Spec.Volumes {
		switch {
		case v.ConfigMap != nil:
			opt := v.ConfigMap.Optional != nil && *v.ConfigMap.Optional
			if err := addCM(v.ConfigMap.Name, opt); err != nil {
				return engine.Credentials{}, err
			}
		case v.Secret != nil:
			opt := v.Secret.Optional != nil && *v.Secret.Optional
			if err := addSec(v.Secret.SecretName, opt); err != nil {
				return engine.Credentials{}, err
			}
		case v.Projected != nil:
			for _, s := range v.Projected.Sources {
				if s.ConfigMap != nil {
					opt := s.ConfigMap.Optional != nil && *s.ConfigMap.Optional
					if err := addCM(s.ConfigMap.Name, opt); err != nil {
						return engine.Credentials{}, err
					}
				}
				if s.Secret != nil {
					opt := s.Secret.Optional != nil && *s.Secret.Optional
					if err := addSec(s.Secret.Name, opt); err != nil {
						return engine.Credentials{}, err
					}
				}
				if s.ServiceAccountToken != nil && creds.ServiceToken == "" {
					sa := pod.Spec.ServiceAccountName
					if sa == "" {
						sa = "default"
					}
					tr := &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{
						ExpirationSeconds: s.ServiceAccountToken.ExpirationSeconds,
					}}
					if s.ServiceAccountToken.Audience != "" {
						tr.Spec.Audiences = []string{s.ServiceAccountToken.Audience}
					}
					if res, err := core.ServiceAccounts(ns).CreateToken(ctx, sa, tr, metav1.CreateOptions{}); err == nil {
						creds.ServiceToken = res.Status.Token
					}
				}
			}
		}
	}
	for _, ref := range pod.Spec.ImagePullSecrets {
		sec, err := core.Secrets(ns).Get(ctx, ref.Name, metav1.GetOptions{})
		if err != nil {
			return engine.Credentials{}, fmt.Errorf("imagePullSecret %s: %w", ref.Name, err)
		}
		parsed, err := image.ParsePullSecret(sec)
		if err != nil {
			return engine.Credentials{}, err
		}
		for _, c := range parsed {
			creds.Pull = c
			break
		}
	}
	return creds, nil
}

func deleteGrace(pod *corev1.Pod) int64 {
	if pod == nil {
		return 30
	}
	if pod.DeletionGracePeriodSeconds != nil {
		return *pod.DeletionGracePeriodSeconds
	}
	if pod.Spec.TerminationGracePeriodSeconds != nil {
		return *pod.Spec.TerminationGracePeriodSeconds
	}
	return 30
}
