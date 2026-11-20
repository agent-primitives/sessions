// Package replayctx carries reconstruction intent across Harness.Run decorators without adding it
// to the public Start or wire contracts.
package replayctx

import "context"

type reconstructionKey struct{}

// WithReconstruction marks ctx as a reconstruction replay invocation.
func WithReconstruction(ctx context.Context) context.Context {
	return context.WithValue(ctx, reconstructionKey{}, true)
}

// IsReconstruction reports whether ctx belongs to reconstruction replay.
func IsReconstruction(ctx context.Context) bool {
	reconstruction, _ := ctx.Value(reconstructionKey{}).(bool)
	return reconstruction
}
