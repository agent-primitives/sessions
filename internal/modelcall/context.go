// Package modelcall carries host-owned dispatch metadata without changing model requests or
// their durable representation.
package modelcall

import "context"

type exactModelKey struct{}

// WithExactModel marks whether the invocation's model must be sent literally. An explicit false
// masks an outer invocation's selection when a controller starts an unrelated legacy call.
func WithExactModel(ctx context.Context, exact bool) context.Context {
	return context.WithValue(ctx, exactModelKey{}, exact)
}

// ExactModel reports whether adapter fallback must be bypassed for this invocation.
func ExactModel(ctx context.Context) bool {
	exact, _ := ctx.Value(exactModelKey{}).(bool)
	return exact
}
