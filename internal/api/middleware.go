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

// envTokenActor is the audit name recorded for mutations made with the built-in
// PGBRANCH_TOKEN env value, which carries admin but has no stored token name.
const envTokenActor = "root"

// resolveActor maps a presented bearer token to the actor behind it: the
// resolved role plus the identity recorded in the audit log. The built-in
// PGBRANCH_TOKEN env value (s.token) is checked first with a constant-time
// compare and is always admin under the "root" sentinel (it is never stored in
// the table, so it has no name). Otherwise the token is looked up by hash and
// the actor carries its stored name. Returns the zero Actor when no role
// matches.
func (s *Server) resolveActor(authHeader string) registry.Actor {
	const prefix = "Bearer "
	if !strings.HasPrefix(authHeader, prefix) {
		return registry.Actor{}
	}
	presented := strings.TrimPrefix(authHeader, prefix)
	if s.token != "" && subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) == 1 {
		return registry.Actor{Name: envTokenActor, Role: registry.RoleAdmin}
	}
	if s.reg != nil {
		if name, role, ok := s.reg.LookupAPITokenActor(presented); ok {
			return registry.Actor{Name: name, Role: role}
		}
	}
	return registry.Actor{}
}

// resolveRole maps a presented bearer token to its role only ("" when no role
// matches). It is a thin wrapper over resolveActor kept for callers (and tests)
// that need just the role.
func (s *Server) resolveRole(authHeader string) string {
	return s.resolveActor(authHeader).Role
}

// requireRole wraps a handler with bearer auth and a minimum-role check. An
// unresolvable token is 401; a resolved token whose role ranks below min is
// 403. The resolved role is attached to the request context for handlers.
func (s *Server) requireRole(min string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor := s.resolveActor(r.Header.Get("Authorization"))
		if actor.Role == "" {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		if roleRank[actor.Role] < roleRank[min] {
			writeError(w, http.StatusForbidden, "role "+actor.Role+" lacks the required "+min+" privilege")
			return
		}
		// Stash both the bare role (handlers' existing roleKey contract) and the
		// full actor (registry.WithActor) so engine/registry mutations downstream
		// can record WHO performed them in the audit log.
		ctx := context.WithValue(r.Context(), roleKey, actor.Role)
		ctx = registry.WithActor(ctx, actor)
		next(w, r.WithContext(ctx))
	}
}
