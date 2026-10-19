package tailscale

import (
	"context"

	"github.com/metacubex/http"
)

type authenticatedKey struct{}

// MarkControllerAuth is installed by the patched Clash API router inside the
// authenticated route group. It records whether that listener requires a
// secret, so secret-bearing routes can refuse unauthenticated controllers.
func MarkControllerAuth(secretRequired bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), authenticatedKey{}, secretRequired)))
		})
	}
}

// ControllerAuthenticated reports whether the request passed secret checks.
func ControllerAuthenticated(ctx context.Context) bool {
	authenticated, _ := ctx.Value(authenticatedKey{}).(bool)
	return authenticated
}
