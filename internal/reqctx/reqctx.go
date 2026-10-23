// Package reqctx menyimpan identitas request di context (owner: A).
// Dipisah dari httpx supaya authz bisa memakainya tanpa import cycle
// (httpx → authz → httpx).
package reqctx

import "context"

type Principal struct {
	UserID          int64
	Username        string
	IsPlatformAdmin bool
}

type key int

const (
	principalKey key = iota
	csrfKey
)

func WithPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, principalKey, p)
}

func CurrentPrincipal(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(principalKey).(Principal)
	return p, ok
}

func WithCSRFToken(ctx context.Context, tok string) context.Context {
	return context.WithValue(ctx, csrfKey, tok)
}

// CSRFToken mengembalikan token untuk dirender ke <input type="hidden" name="csrf_token">.
func CSRFToken(ctx context.Context) string {
	tok, _ := ctx.Value(csrfKey).(string)
	return tok
}
