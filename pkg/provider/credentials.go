package provider

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/darwin-node/darwin-node/pkg/engine"
	"github.com/darwin-node/darwin-node/pkg/image"
	"github.com/darwin-node/darwin-node/pkg/volume"

	authv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
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
	seenToken := map[string]struct{}{}

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
				if s.ServiceAccountToken != nil {
					tok, err := mintServiceAccountToken(ctx, core, ns, pod.Spec.ServiceAccountName, v.Name, s.ServiceAccountToken)
					if err != nil {
						return engine.Credentials{}, err
					}
					key := tok.Volume + "\x00" + tok.Path
					if _, dup := seenToken[key]; dup {
						return engine.Credentials{}, fmt.Errorf("projected volume %s path %s is duplicated", tok.Volume, tok.Path)
					}
					seenToken[key] = struct{}{}
					creds.Tokens = append(creds.Tokens, tok)
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

// mintServiceAccountToken asks the API server for one projected token.
// A transport error or an empty token fails the pod; we do not write a blank file.
func mintServiceAccountToken(ctx context.Context, core typedcorev1.CoreV1Interface, ns, serviceAccount, volumeName string, proj *corev1.ServiceAccountTokenProjection) (volume.ProjectedToken, error) {
	if proj == nil {
		return volume.ProjectedToken{}, fmt.Errorf("nil service account projection")
	}
	path := proj.Path
	if path == "" {
		path = "token"
	}
	sa := serviceAccount
	if sa == "" {
		sa = "default"
	}
	tr := &authv1.TokenRequest{Spec: authv1.TokenRequestSpec{
		ExpirationSeconds: proj.ExpirationSeconds,
	}}
	if proj.Audience != "" {
		tr.Spec.Audiences = []string{proj.Audience}
	}
	res, err := core.ServiceAccounts(ns).CreateToken(ctx, sa, tr, metav1.CreateOptions{})
	if err != nil {
		return volume.ProjectedToken{}, fmt.Errorf("serviceaccount %s token %s/%s: %w", sa, volumeName, path, err)
	}
	if res == nil || strings.TrimSpace(res.Status.Token) == "" {
		return volume.ProjectedToken{}, fmt.Errorf("serviceaccount %s token %s/%s: empty token", sa, volumeName, path)
	}
	var exp time.Time
	if !res.Status.ExpirationTimestamp.IsZero() {
		exp = res.Status.ExpirationTimestamp.Time
	}
	return volume.ProjectedToken{
		Volume:    volumeName,
		Path:      path,
		Token:     res.Status.Token,
		ExpiresAt: exp,
	}, nil
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
