package api

import (
	"crypto/subtle"
	"net/http"
)

// auth enforces Authorization: Bearer <token> with a constant-time compare.
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want := "Bearer " + s.token
		got := r.Header.Get("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			writeError(w, http.StatusUnauthorized, "missing or invalid bearer token")
			return
		}
		next.ServeHTTP(w, r)
	})
}
