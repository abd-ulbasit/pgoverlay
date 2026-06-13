package api

import (
	"context"
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/abd-ulbasit/pgbranch/internal/registry"
)

// roleRank orders roles by privilege; a higher number satisfies any required
// role at or below it. admin > operator > viewer.
var roleRank = map[string]int{
	registry.RoleViewer:   1,
	registry.RoleOperator: 2,
	registry.RoleAdmin:    3,
}

type ctxKey int

const roleKey ctxKey = iota

// resolveRole maps a presented bearer token to its role. The built-in
// PGBRANCH_TOKEN env value (s.token) is checked first with a constant-time
// compare and is always admin (never stored in the table). Otherwise the token
// is looked up by hash in the registry. Returns "" when no role matches.
func (s *Server) resolveRole(authHeader string) string {
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return ""
	}
	presented := strings.TrimPrefix(authHeader, prefix)
	if s.token != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) == 1 {
		return registry.RoleAdmin
	}
	if s.reg != nil {
		if role, ok := s.reg.LookupAPIToken(presented); ok {
			return role
		}
	}
	return ""
}

// requireRole wraps a handler with bearer auth and a minimum-role check. An
// unresolvable token is 401; a resolved token whose role ranks below min is
// 403. The resolved role is attached to the request context for handlers.
func (s *Server) requireRole(min string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		role := s.resolveRole(r.Header.Get("Authorization"))
		if role == "" {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		if roleRank[role] < roleRank[min] {
			writeError(w, http.StatusForbidden, "role "+role+" lacks the required "+min+" privilege")
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), roleKey, role)))
	}
}
