// Copyright 2026 OrgMentem. Licensed under MIT. See LICENSE.
package api

import (
	"context"

	"papio/internal/job"
)

type principalContextKey struct{}

// WithPrincipal attaches the durable acquisition principal to an API call.
func WithPrincipal(ctx context.Context, principal job.Principal) context.Context {
	return context.WithValue(ctx, principalContextKey{}, principal)
}

// PrincipalFrom returns the caller principal, defaulting to the CLI for
// compatibility with untagged RPC callers.
func PrincipalFrom(ctx context.Context) job.Principal {
	principal, _ := ctx.Value(principalContextKey{}).(job.Principal)
	if principal == "" {
		return job.PrincipalCLI
	}
	return principal
}
