package sidecar

import (
	"context"

	"github.com/mlvea/darwin-node/pkg/image"

	corev1 "k8s.io/api/core/v1"
)

// CreateOptions is what the engine knows about a pod and the runtime does
// not. It travels on the Create context so Runtime.Create keeps its
// signature. Docker keeps the last options for a pod and uses them when a
// restart calls Create without them.
type CreateOptions struct {
	AllowedHostPaths []string
	ConfigMaps       map[string]*corev1.ConfigMap
	Secrets          map[string]*corev1.Secret
	Pull             image.RegistryCreds
}

type createOptionsKey struct{}

// WithCreateOptions attaches opts to ctx.
func WithCreateOptions(ctx context.Context, opts CreateOptions) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, createOptionsKey{}, opts)
}

// CreateOptionsFrom reports the options on ctx.
func CreateOptionsFrom(ctx context.Context) (CreateOptions, bool) {
	if ctx == nil {
		return CreateOptions{}, false
	}
	opts, ok := ctx.Value(createOptionsKey{}).(CreateOptions)
	return opts, ok
}
